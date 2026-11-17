package queue

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// reap runs one reaper pass that must succeed.
func (f *fixture) reap() ReapResult {
	f.t.Helper()
	res, err := f.queue.Reap(context.Background())
	if err != nil {
		f.t.Fatalf("Reap: %v", err)
	}
	return res
}

func TestReapRequeuesExpiredLease(t *testing.T) {
	f := newFixture(t, Options{LeaseDuration: time.Minute})
	f.enqueue()
	_, id := f.claim("node-a")
	before := f.job(id)

	// One millisecond before the lease runs out nothing happens.
	f.clock.Advance(time.Minute - time.Millisecond)
	if res := f.reap(); len(res.Requeued)+len(res.Failed) != 0 {
		t.Fatalf("Reap before expiry = %+v", res)
	}
	if after := f.job(id); !reflect.DeepEqual(after, before) {
		t.Fatalf("job changed before expiry:\nbefore %+v\nafter  %+v", before, after)
	}

	f.clock.Advance(time.Millisecond)
	res := f.reap()
	if !reflect.DeepEqual(res, ReapResult{Requeued: []int64{id}}) {
		t.Fatalf("Reap = %+v, want job %d queued again", res, id)
	}
	job := f.job(id)
	if job.Status != store.JobQueued || !job.LeaseExpiresAt.IsZero() || job.Attempts != 1 {
		t.Errorf("status %q, lease %s, attempts %d; want queued, no lease, 1 attempt",
			job.Status, job.LeaseExpiresAt, job.Attempts)
	}
	if job.Clipper != "node-a" || !job.LastHeartbeatAt.Equal(t0ms) {
		t.Errorf("clipper %q, last heartbeat %s; want the last holder node-a at %s",
			job.Clipper, job.LastHeartbeatAt, t0ms)
	}
	for _, want := range []string{"lease expired", `"node-a"`, store.FormatTime(t0ms)} {
		if !strings.Contains(job.LastError, want) {
			t.Errorf("last error %q does not contain %q", job.LastError, want)
		}
	}

	// The job is handed out again, as its second attempt.
	got, again := f.claim("node-b")
	if again != id || got.Attempt != 2 {
		t.Errorf("claim after reaping: job %d attempt %d, want job %d attempt 2", again, got.Attempt, id)
	}
}

func TestMissedHeartbeatsLoseTheJob(t *testing.T) {
	f := newFixture(t, Options{LeaseDuration: time.Minute})
	f.enqueue()
	_, id := f.claim("node-a")
	interval := f.queue.HeartbeatInterval()

	// As long as heartbeats come, the reaper leaves the job alone, long
	// after the first lease would have run out.
	for range 20 {
		f.clock.Advance(interval)
		if _, err := f.queue.Heartbeat(context.Background(), id, "node-a"); err != nil {
			t.Fatal(err)
		}
		if res := f.reap(); len(res.Requeued)+len(res.Failed) != 0 {
			t.Fatalf("Reap while heartbeats come = %+v", res)
		}
	}

	// The clipper goes quiet: missed heartbeats alone do nothing until the
	// lease, counted from the last heartbeat, has run out.
	for range 3 {
		f.clock.Advance(interval)
		if res := f.reap(); len(res.Requeued)+len(res.Failed) != 0 {
			t.Fatalf("Reap within the lease = %+v", res)
		}
	}
	f.clock.Advance(time.Minute - 3*interval)
	if res := f.reap(); !reflect.DeepEqual(res.Requeued, []int64{id}) {
		t.Fatalf("Reap after the lease ran out = %+v, want job %d queued again", res, id)
	}
	if job := f.job(id); job.Status != store.JobQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}
}

func TestReapFailsAtAttemptLimit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{MaxAttempts: 3})
	id := f.enqueue().ID

	for attempt := 1; attempt <= 3; attempt++ {
		f.claim("node-a")
		f.clock.Advance(DefaultLeaseDuration)
		res := f.reap()
		job := f.job(id)
		if attempt < 3 {
			if !reflect.DeepEqual(res, ReapResult{Requeued: []int64{id}}) || job.Status != store.JobQueued {
				t.Fatalf("attempt %d: Reap = %+v, status %q; want queued again", attempt, res, job.Status)
			}
			continue
		}
		if !reflect.DeepEqual(res, ReapResult{Failed: []int64{id}}) || job.Status != store.JobFailed {
			t.Fatalf("last attempt: Reap = %+v, status %q; want failed", res, job.Status)
		}
		if job.Attempts != 3 || !job.LeaseExpiresAt.IsZero() || !strings.Contains(job.LastError, "lease expired") {
			t.Errorf("failed job: attempts %d, lease %s, last error %q", job.Attempts, job.LeaseExpiresAt, job.LastError)
		}
	}

	// A failed job is not handed out again, and later passes leave it be.
	if _, err := f.queue.Claim(ctx, "node-b"); !errors.Is(err, ErrNoJob) {
		t.Errorf("Claim after the last attempt: err = %v, want ErrNoJob", err)
	}
	f.clock.Advance(time.Hour)
	if res := f.reap(); len(res.Requeued)+len(res.Failed) != 0 {
		t.Errorf("Reap of a failed job = %+v", res)
	}
}

func TestReapTouchesOnlyExpiredLeases(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{LeaseDuration: time.Minute})

	// Leases taken ten seconds apart: they expire at t0+60s, +70s, +80s, +90s.
	leased := make([]int64, 4)
	for i := range leased {
		f.enqueue()
		_, leased[i] = f.claim("node-a")
		f.clock.Advance(10 * time.Second)
	}
	// Jobs in every other state, which have no lease to expire.
	f.enqueue()
	_, done := f.claim("node-b")
	f.setStatus(done, store.JobDone)
	f.enqueue()
	_, failed := f.claim("node-b")
	if _, err := f.queue.Fail(ctx, failed, "node-b", "VOD deleted", false); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue().ID
	untouched := []int64{leased[2], leased[3], queued, done, failed}
	before := map[int64]store.Job{}
	for _, id := range untouched {
		before[id] = f.job(id)
	}

	// Now t0+40s; at t0+75s the first two leases have run out.
	f.clock.Advance(35 * time.Second)
	if res := f.reap(); !reflect.DeepEqual(res, ReapResult{Requeued: leased[:2]}) {
		t.Fatalf("Reap = %+v, want %v queued again", res, leased[:2])
	}
	for _, id := range untouched {
		if after := f.job(id); !reflect.DeepEqual(after, before[id]) {
			t.Errorf("job %d changed:\nbefore %+v\nafter  %+v", id, before[id], after)
		}
	}
}

func TestReapSkipsLeaseRenewedSinceFound(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.enqueue()
	_, id := f.claim("node-a")
	f.clock.Advance(DefaultLeaseDuration)

	// The pass finds the job expired, then a heartbeat arrives before it is
	// taken back.
	ids, err := f.queue.expiredJobs(ctx)
	if err != nil || !reflect.DeepEqual(ids, []int64{id}) {
		t.Fatalf("expiredJobs = %v, %v; want [%d]", ids, err, id)
	}
	if _, err := f.queue.Heartbeat(ctx, id, "node-a"); err != nil {
		t.Fatal(err)
	}
	before := f.job(id)
	if _, reaped, err := f.queue.reapJob(ctx, id); err != nil || reaped {
		t.Fatalf("reapJob = reaped %t, %v; want the renewed lease left alone", reaped, err)
	}
	if after := f.job(id); !reflect.DeepEqual(after, before) {
		t.Errorf("job changed:\nbefore %+v\nafter  %+v", before, after)
	}

	// Likewise when the holder failed it in between.
	f.clock.Advance(DefaultLeaseDuration)
	if _, err := f.queue.Fail(ctx, id, "node-a", "disk full", true); err != nil {
		t.Fatal(err)
	}
	if _, reaped, err := f.queue.reapJob(ctx, id); err != nil || reaped {
		t.Fatalf("reapJob after Fail = reaped %t, %v; want nothing done", reaped, err)
	}
}

func TestReapWithCancelledContext(t *testing.T) {
	f := newFixture(t, Options{})
	f.enqueue()
	_, id := f.claim("node-a")
	f.clock.Advance(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.queue.Reap(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Reap: err = %v, want context.Canceled", err)
	}
	if job := f.job(id); job.Status != store.JobLeased {
		t.Errorf("status = %q, want still leased", job.Status)
	}
}

// syncBuffer is a log destination the reaper goroutine and the test can
// share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitDone fails the test when done is not closed in time. The timeout only
// keeps a broken loop from hanging the test run; a working loop returns at
// once.
func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatalf("%s did not return after cancel", what)
	}
}

func TestRunReaperLoop(t *testing.T) {
	f := newFixture(t, Options{MaxAttempts: 2})
	id := f.enqueue().ID
	f.claim("node-a")
	f.clock.Advance(DefaultLeaseDuration)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.queue.runReaper(ctx, tick, logger)
	}()

	// The tick is unbuffered: once the loop takes it, the pass before it
	// is over. The first pass runs at the start, without a tick.
	tick <- time.Time{}
	if job := f.job(id); job.Status != store.JobQueued {
		t.Fatalf("after the first pass: status %q, want queued", job.Status)
	}

	f.claim("node-b")
	tick <- time.Time{}
	if job := f.job(id); job.Status != store.JobLeased {
		t.Fatalf("pass before expiry: status %q, want still leased", job.Status)
	}
	f.clock.Advance(DefaultLeaseDuration)
	tick <- time.Time{} // runs the pass that takes the job back
	tick <- time.Time{} // waits for it
	if job := f.job(id); job.Status != store.JobFailed {
		t.Fatalf("after the last attempt expired: status %q, want failed", job.Status)
	}

	cancel()
	waitDone(t, done, "runReaper")
	out := logs.String()
	for _, want := range []string{"job queued again", "job failed after its last attempt"} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not mention %q:\n%s", want, out)
		}
	}
}

func TestRunReaperStopsOnCancel(t *testing.T) {
	f := newFixture(t, Options{})

	t.Run("while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var err error
		go func() {
			defer close(done)
			err = f.queue.RunReaper(ctx, time.Hour, nil)
		}()
		cancel()
		waitDone(t, done, "RunReaper")
		if err != nil {
			t.Errorf("RunReaper = %v, want nil after cancel", err)
		}
	})

	t.Run("already cancelled", func(t *testing.T) {
		f.enqueue()
		_, id := f.claim("node-a")
		f.clock.Advance(time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.queue.RunReaper(ctx, time.Hour, nil); err != nil {
			t.Errorf("RunReaper = %v, want nil", err)
		}
		if job := f.job(id); job.Status != store.JobLeased {
			t.Errorf("status = %q: a cancelled reaper took the job back", job.Status)
		}
	})

	t.Run("bad interval", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Second} {
			if err := f.queue.RunReaper(context.Background(), d, nil); err == nil {
				t.Errorf("RunReaper with interval %s: no error", d)
			}
		}
	})
}
