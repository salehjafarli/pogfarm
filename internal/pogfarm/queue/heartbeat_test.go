package queue

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

func TestHeartbeatExtendsLease(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{LeaseDuration: time.Minute})
	f.enqueue()
	_, id := f.claim("node-a")

	for i := 1; i <= 3; i++ {
		f.clock.Advance(15 * time.Second)
		got, err := f.queue.Heartbeat(ctx, id, "node-a")
		if err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		now := t0ms.Add(time.Duration(i) * 15 * time.Second)
		if want := now.Add(time.Minute); !got.Equal(want) {
			t.Errorf("heartbeat %d: lease expires %s, want %s", i, got, want)
		}
		job := f.job(id)
		if !job.LeaseExpiresAt.Equal(got) {
			t.Errorf("heartbeat %d: stored lease %s, returned %s", i, job.LeaseExpiresAt, got)
		}
		if !job.LastHeartbeatAt.Equal(now) || !job.UpdatedAt.Equal(now) {
			t.Errorf("heartbeat %d: last heartbeat %s, updated %s; want both %s",
				i, job.LastHeartbeatAt, job.UpdatedAt, now)
		}
		if job.Status != store.JobLeased || job.Clipper != "node-a" || job.Attempts != 1 {
			t.Errorf("heartbeat %d: status %q, clipper %q, attempts %d; want leased to node-a, attempt 1",
				i, job.Status, job.Clipper, job.Attempts)
		}
	}
}

func TestHeartbeatAfterLeaseRanOut(t *testing.T) {
	// Like Fail: until the reaper has taken the job back, nobody else was
	// given it, so its holder may still renew the lease.
	f := newFixture(t, Options{})
	f.enqueue()
	_, id := f.claim("node-a")
	f.clock.Advance(time.Hour)
	got, err := f.queue.Heartbeat(context.Background(), id, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if want := t0ms.Add(time.Hour + DefaultLeaseDuration); !got.Equal(want) {
		t.Errorf("lease expires %s, want %s", got, want)
	}
	res, err := f.queue.Reap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requeued)+len(res.Failed) != 0 {
		t.Errorf("Reap after the renewal took back %+v", res)
	}
}

func TestHeartbeatRejected(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})

	f.enqueue()
	_, held := f.claim("node-a")
	f.enqueue()
	_, done := f.claim("node-a")
	f.setStatus(done, store.JobDone)
	f.enqueue()
	_, failed := f.claim("node-a")
	if _, err := f.queue.Fail(ctx, failed, "node-a", "VOD deleted", false); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue().ID

	tests := []struct {
		name    string
		id      int64
		clipper string
		want    error
	}{
		{"another clipper's job", held, "node-b", ErrLeaseLost},
		{"queued job", queued, "node-a", ErrLeaseLost},
		{"done job", done, "node-a", ErrAlreadyCompleted},
		{"failed job", failed, "node-a", ErrLeaseLost},
		{"unknown job", 9999, "node-a", ErrUnknownJob},
		{"no clipper name", held, "", ErrNoClipper},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var before store.Job
			if tt.id != 9999 {
				before = f.job(tt.id)
			}
			f.clock.Advance(time.Second)
			got, err := f.queue.Heartbeat(ctx, tt.id, tt.clipper)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Heartbeat: err = %v, want %v", err, tt.want)
			}
			if !got.IsZero() {
				t.Errorf("Heartbeat returned expiry %s with an error", got)
			}
			if tt.id == 9999 {
				return
			}
			if after := f.job(tt.id); !reflect.DeepEqual(after, before) {
				t.Errorf("job changed:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

func TestHeartbeatByStaleHolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.enqueue()
	_, id := f.claim("node-a")

	// node-a goes quiet; the reaper takes the job back.
	f.clock.Advance(DefaultLeaseDuration)
	if res, err := f.queue.Reap(ctx); err != nil || !reflect.DeepEqual(res.Requeued, []int64{id}) {
		t.Fatalf("Reap = %+v, %v; want job %d queued again", res, err, id)
	}
	if _, err := f.queue.Heartbeat(ctx, id, "node-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("heartbeat on the queued job: err = %v, want ErrLeaseLost", err)
	}

	// node-b gets it; node-a's late heartbeats must not take it back.
	f.clock.Advance(time.Second)
	_, again := f.claim("node-b")
	if again != id {
		t.Fatalf("node-b claimed job %d, want %d", again, id)
	}
	before := f.job(id)
	f.clock.Advance(time.Second)
	if _, err := f.queue.Heartbeat(ctx, id, "node-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("heartbeat by the former holder: err = %v, want ErrLeaseLost", err)
	}
	if after := f.job(id); !reflect.DeepEqual(after, before) {
		t.Errorf("job changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, err := f.queue.Heartbeat(ctx, id, "node-b"); err != nil {
		t.Errorf("heartbeat by the new holder: %v", err)
	}
}

func TestHeartbeatConcurrentSameJob(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.enqueue()
	_, id := f.claim("node-a")

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			if _, err := f.queue.Heartbeat(ctx, id, "node-a"); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent heartbeat: %v", err)
	}
	if job := f.job(id); job.Status != store.JobLeased || job.Clipper != "node-a" {
		t.Errorf("job is %q with %q, want leased to node-a", job.Status, job.Clipper)
	}
}

// TestHeartbeatsRaceReaper runs holders' heartbeats against reaper passes
// while the clock jumps past leases. Which heartbeats win is up to the
// scheduler; what must hold is that every heartbeat either renews the lease
// or reports it lost, that a lost lease stays lost, and that every job ends
// in a state the schema allows.
func TestHeartbeatsRaceReaper(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{MaxAttempts: 1})
	const jobs = 6
	ids := make([]int64, jobs)
	for i := range jobs {
		f.enqueue()
		_, ids[i] = f.claim(fmt.Sprintf("node-%d", i))
	}

	stop := make(chan struct{})
	var holders, reaper sync.WaitGroup
	errs := make(chan error, 100)
	for i, id := range ids {
		clipper := fmt.Sprintf("node-%d", i)
		holders.Go(func() {
			lost := false
			for range 30 {
				_, err := f.queue.Heartbeat(ctx, id, clipper)
				switch {
				case err == nil && lost:
					errs <- fmt.Errorf("%s: heartbeat on job %d succeeded after the lease was lost", clipper, id)
					return
				case err == nil:
				case errors.Is(err, ErrLeaseLost):
					lost = true
				default:
					errs <- fmt.Errorf("%s: heartbeat on job %d: %w", clipper, id, err)
					return
				}
			}
		})
	}
	reaper.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.clock.Advance(DefaultLeaseDuration / 3)
			if _, err := f.queue.Reap(ctx); err != nil {
				errs <- fmt.Errorf("Reap: %w", err)
				return
			}
		}
	})
	holders.Wait()
	close(stop)
	reaper.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Once everyone is quiet, every lease runs out and every job, at its
	// only attempt, is failed.
	f.clock.Advance(time.Hour)
	if _, err := f.queue.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if job := f.job(id); job.Status != store.JobFailed || !job.LeaseExpiresAt.IsZero() {
			t.Errorf("job %d: status %q, lease %s; want failed without a lease", id, job.Status, job.LeaseExpiresAt)
		}
	}
}
