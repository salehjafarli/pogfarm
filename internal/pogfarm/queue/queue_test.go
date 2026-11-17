package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// t0 is where every test's clock starts. It has a fraction below the
// millisecond so the tests see that times are cut the way the store cuts
// them.
var t0 = time.Date(2026, 11, 17, 14, 30, 0, 123456789, time.UTC)

// t0ms is t0 as it is stored.
var t0ms = t0.Truncate(time.Millisecond)

// clock is a clock the test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: t0} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fixture is a queue on a fresh database that holds one streamer and one
// VOD.
type fixture struct {
	t     *testing.T
	store *store.Store
	clock *clock
	queue *Queue
	vod   store.VOD
	// nextStart keeps the windows of the candidates apart.
	nextStart float64
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	f := &fixture{t: t, store: st, clock: newClock()}
	if opts.Now == nil {
		opts.Now = f.clock.Now
	}
	if f.queue, err = New(st, opts); err != nil {
		t.Fatal(err)
	}

	streamer, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform:       api.PlatformTwitch,
		PlatformUserID: "1001",
		Login:          "somestreamer",
		DisplayName:    "SomeStreamer",
		Enabled:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.vod, err = store.InsertVOD(ctx, st.Writer(), store.VOD{
		Platform:        api.PlatformTwitch,
		PlatformVODID:   "2301234567",
		StreamerID:      streamer.ID,
		Title:           "a stream",
		DurationSeconds: 100000,
		StartedAt:       t0.Add(-30 * time.Hour),
		EndedAt:         t0.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// candidate stores a new candidate with a window of its own.
func (f *fixture) candidate() store.Candidate {
	f.t.Helper()
	f.nextStart += 100
	c, err := store.InsertCandidate(context.Background(), f.store.Writer(), store.Candidate{
		Platform:     api.PlatformTwitch,
		VODID:        f.vod.ID,
		StartSeconds: f.nextStart + 0.5,
		EndSeconds:   f.nextStart + 45,
		Score:        1,
		Signal:       store.SignalChatSpike,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// enqueue stores a new candidate and queues its job.
func (f *fixture) enqueue() store.Job {
	f.t.Helper()
	job, created, err := f.queue.Enqueue(context.Background(), f.candidate().ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if !created {
		f.t.Fatal("Enqueue of a new candidate: created = false")
	}
	return job
}

// claim claims a job that must be there and returns it with its row id.
func (f *fixture) claim(clipper string) (api.Job, int64) {
	f.t.Helper()
	job, err := f.queue.Claim(context.Background(), clipper)
	if err != nil {
		f.t.Fatalf("Claim(%q): %v", clipper, err)
	}
	id, err := ParseJobID(job.ID)
	if err != nil {
		f.t.Fatalf("Claim(%q) returned id %q: %v", clipper, job.ID, err)
	}
	return job, id
}

// job reads a job row.
func (f *fixture) job(id int64) store.Job {
	f.t.Helper()
	job, err := store.GetJob(context.Background(), f.store.Reader(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return job
}

// setStatus forces a job into a status that only later parts of the queue
// produce, with its lease cleared.
func (f *fixture) setStatus(id int64, status store.JobStatus) {
	f.t.Helper()
	_, err := f.store.Writer().ExecContext(context.Background(),
		`UPDATE jobs SET status = ?, lease_expires_at = NULL WHERE id = ?`, status, id)
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestNew(t *testing.T) {
	f := newFixture(t, Options{})

	tests := []struct {
		name          string
		opts          Options
		wantErr       string
		wantLease     time.Duration
		wantHeartbeat time.Duration
		wantAttempts  int
	}{
		{name: "defaults", wantLease: 2 * time.Minute, wantHeartbeat: 30 * time.Second, wantAttempts: 3},
		{
			name:      "heartbeat is a quarter of the lease",
			opts:      Options{LeaseDuration: 10 * time.Minute, MaxAttempts: 5},
			wantLease: 10 * time.Minute, wantHeartbeat: 150 * time.Second, wantAttempts: 5,
		},
		{
			name:      "derived heartbeat is cut to whole seconds",
			opts:      Options{LeaseDuration: 90 * time.Second},
			wantLease: 90 * time.Second, wantHeartbeat: 22 * time.Second, wantAttempts: 3,
		},
		{
			name:      "shortest lease",
			opts:      Options{LeaseDuration: MinLeaseDuration},
			wantLease: 4 * time.Second, wantHeartbeat: time.Second, wantAttempts: 3,
		},
		{
			name:      "explicit heartbeat, half the lease",
			opts:      Options{LeaseDuration: time.Minute, HeartbeatInterval: 30 * time.Second, MaxAttempts: 1},
			wantLease: time.Minute, wantHeartbeat: 30 * time.Second, wantAttempts: 1,
		},
		{name: "lease too short", opts: Options{LeaseDuration: 3 * time.Second}, wantErr: "lease duration"},
		{name: "negative lease", opts: Options{LeaseDuration: -time.Minute}, wantErr: "lease duration"},
		{
			name:    "heartbeat above half the lease",
			opts:    Options{LeaseDuration: time.Minute, HeartbeatInterval: 31 * time.Second},
			wantErr: "heartbeat interval",
		},
		{name: "heartbeat with a fraction", opts: Options{HeartbeatInterval: 1500 * time.Millisecond}, wantErr: "heartbeat interval"},
		{name: "heartbeat below a second", opts: Options{HeartbeatInterval: 500 * time.Millisecond}, wantErr: "heartbeat interval"},
		{name: "negative heartbeat", opts: Options{HeartbeatInterval: -time.Second}, wantErr: "heartbeat interval"},
		{name: "negative attempts", opts: Options{MaxAttempts: -1}, wantErr: "max attempts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := New(f.store, tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("New: err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if q.LeaseDuration() != tt.wantLease || q.HeartbeatInterval() != tt.wantHeartbeat || q.MaxAttempts() != tt.wantAttempts {
				t.Errorf("lease %s, heartbeat %s, attempts %d; want %s, %s, %d",
					q.LeaseDuration(), q.HeartbeatInterval(), q.MaxAttempts(),
					tt.wantLease, tt.wantHeartbeat, tt.wantAttempts)
			}
		})
	}

	t.Run("nil store", func(t *testing.T) {
		if _, err := New(nil, Options{}); err == nil {
			t.Error("New(nil): no error")
		}
	})
}

func TestParseJobID(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"42", 42, true},
		{"9223372036854775807", 9223372036854775807, true},
		{"", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"042", 0, false},
		{" 42", 0, false},
		{"42x", 0, false},
		{"4.2", 0, false},
		{"9223372036854775808", 0, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.in), func(t *testing.T) {
			got, err := ParseJobID(tt.in)
			if tt.ok {
				if err != nil || got != tt.want {
					t.Fatalf("ParseJobID = %d, %v; want %d", got, err, tt.want)
				}
				if JobID(got) != tt.in {
					t.Errorf("JobID(%d) = %q, want %q", got, JobID(got), tt.in)
				}
				return
			}
			if !errors.Is(err, ErrUnknownJob) {
				t.Errorf("ParseJobID: err = %v, want ErrUnknownJob", err)
			}
		})
	}
}

func TestEnqueue(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	cand := f.candidate()

	job, created, err := f.queue.Enqueue(ctx, cand.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("first Enqueue: created = false")
	}
	want := store.Job{
		ID:          job.ID,
		Platform:    api.PlatformTwitch,
		CandidateID: cand.ID,
		Status:      store.JobQueued,
		CreatedAt:   t0ms,
		UpdatedAt:   t0ms,
	}
	if job != want {
		t.Errorf("Enqueue returned\n %+v, want\n %+v", job, want)
	}
	if got := f.job(job.ID); got != want {
		t.Errorf("stored job\n %+v, want\n %+v", got, want)
	}

	stored, err := store.GetCandidate(ctx, f.store.Reader(), cand.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.CandidateQueued || !stored.UpdatedAt.Equal(t0ms) {
		t.Errorf("candidate: status %q, updated %s; want %q, %s",
			stored.Status, stored.UpdatedAt, store.CandidateQueued, t0ms)
	}

	t.Run("a second enqueue returns the same job", func(t *testing.T) {
		f.clock.Advance(time.Hour)
		again, created, err := f.queue.Enqueue(ctx, cand.ID)
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Error("created = true")
		}
		if again != want {
			t.Errorf("got\n %+v, want\n %+v", again, want)
		}
	})

	t.Run("a second enqueue leaves a claimed job alone", func(t *testing.T) {
		_, id := f.claim("node-a")
		before := f.job(id)
		again, created, err := f.queue.Enqueue(ctx, cand.ID)
		if err != nil {
			t.Fatal(err)
		}
		if created || again != before {
			t.Errorf("created = %v, job\n %+v, want the leased job\n %+v", created, again, before)
		}
	})

	t.Run("unknown candidate", func(t *testing.T) {
		_, _, err := f.queue.Enqueue(ctx, 99999)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want store.ErrNotFound", err)
		}
	})

	t.Run("EnqueueTx is undone with its transaction", func(t *testing.T) {
		other := f.candidate()
		boom := errors.New("boom")
		err := f.store.WriteTx(ctx, func(tx *sql.Tx) error {
			if _, created, err := f.queue.EnqueueTx(ctx, tx, other.ID); err != nil || !created {
				t.Errorf("EnqueueTx: created = %v, err = %v", created, err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("WriteTx: %v", err)
		}
		if _, err := store.GetJobByCandidate(ctx, f.store.Reader(), other.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("job after rollback: err = %v, want store.ErrNotFound", err)
		}
		stored, err := store.GetCandidate(ctx, f.store.Reader(), other.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != store.CandidateNew {
			t.Errorf("candidate status after rollback = %q, want %q", stored.Status, store.CandidateNew)
		}
	})
}

func TestClaimReturnsFullPayload(t *testing.T) {
	f := newFixture(t, Options{})
	queued := f.enqueue()
	cand, err := store.GetCandidate(context.Background(), f.store.Reader(), queued.CandidateID)
	if err != nil {
		t.Fatal(err)
	}

	f.clock.Advance(10 * time.Second)
	claimedAt := t0ms.Add(10 * time.Second)
	got, id := f.claim("node-a")

	want := api.Job{
		ID:                       JobID(queued.ID),
		Attempt:                  1,
		Platform:                 api.PlatformTwitch,
		VODID:                    "2301234567",
		StartSeconds:             cand.StartSeconds,
		EndSeconds:               cand.EndSeconds,
		Streamer:                 api.Streamer{Login: "somestreamer", DisplayName: "SomeStreamer"},
		LeaseExpiresAt:           claimedAt.Add(2 * time.Minute),
		HeartbeatIntervalSeconds: 30,
	}
	if got != want {
		t.Errorf("Claim returned\n %+v, want\n %+v", got, want)
	}

	// Whatever the contract's job carries, now or later, must be filled in.
	requireNoZeroField(t, reflect.ValueOf(got), "Job")

	wantRow := store.Job{
		ID:              queued.ID,
		Platform:        api.PlatformTwitch,
		CandidateID:     cand.ID,
		Status:          store.JobLeased,
		Attempts:        1,
		Clipper:         "node-a",
		LeaseExpiresAt:  claimedAt.Add(2 * time.Minute),
		LastHeartbeatAt: claimedAt,
		CreatedAt:       t0ms,
		UpdatedAt:       claimedAt,
	}
	if row := f.job(id); row != wantRow {
		t.Errorf("stored job\n %+v, want\n %+v", row, wantRow)
	}
}

// requireNoZeroField fails for every field of v, at any depth, that holds
// its zero value.
func requireNoZeroField(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	if v.Kind() == reflect.Struct && v.Type() != reflect.TypeFor[time.Time]() {
		for i := range v.NumField() {
			requireNoZeroField(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
		return
	}
	if v.IsZero() {
		t.Errorf("%s is not set", path)
	}
}

func TestClaimUsesConfiguredLease(t *testing.T) {
	f := newFixture(t, Options{LeaseDuration: 10 * time.Minute, HeartbeatInterval: 45 * time.Second})
	f.enqueue()
	job, id := f.claim("node-a")
	if want := t0ms.Add(10 * time.Minute); !job.LeaseExpiresAt.Equal(want) || !f.job(id).LeaseExpiresAt.Equal(want) {
		t.Errorf("lease expires %s (stored %s), want %s", job.LeaseExpiresAt, f.job(id).LeaseExpiresAt, want)
	}
	if job.HeartbeatIntervalSeconds != 45 || job.HeartbeatInterval() != 45*time.Second {
		t.Errorf("heartbeat interval = %d s, want 45", job.HeartbeatIntervalSeconds)
	}
}

func TestClaimOldestFirst(t *testing.T) {
	f := newFixture(t, Options{})

	// Candidates are stored first and queued in another order, so the order
	// of the queue is not the order of the candidate ids.
	cands := []store.Candidate{f.candidate(), f.candidate(), f.candidate(), f.candidate(), f.candidate()}
	var want []string
	enqueue := func(i int) {
		t.Helper()
		job, _, err := f.queue.Enqueue(context.Background(), cands[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, JobID(job.ID))
	}
	enqueue(3)
	f.clock.Advance(time.Second)
	enqueue(0)
	f.clock.Advance(time.Second)
	// Two jobs queued in the same millisecond go out in the order queued.
	enqueue(4)
	enqueue(1)
	f.clock.Advance(time.Second)
	enqueue(2)

	var got []string
	for range want {
		job, _ := f.claim("node-a")
		got = append(got, job.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claimed in order %v, want %v", got, want)
	}
}

func TestClaimPrefersOlderJobOverLowerID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	late := f.enqueue()
	early := f.enqueue()
	// Make the job with the higher id the older one.
	if _, err := f.store.Writer().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
		store.FormatTime(t0.Add(-time.Hour)), early.ID); err != nil {
		t.Fatal(err)
	}
	if _, id := f.claim("node-a"); id != early.ID {
		t.Errorf("claimed job %d first, want the older job %d", id, early.ID)
	}
	if _, id := f.claim("node-a"); id != late.ID {
		t.Errorf("claimed job %d second, want %d", id, late.ID)
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})

	if job, err := f.queue.Claim(ctx, "node-a"); !errors.Is(err, ErrNoJob) || job != (api.Job{}) {
		t.Fatalf("Claim on a new queue = %+v, %v; want no job and ErrNoJob", job, err)
	}

	// Jobs that are not queued are not handed out: one leased, one failed,
	// one done.
	for range 3 {
		f.enqueue()
	}
	_, leased := f.claim("node-a")
	_, failed := f.claim("node-a")
	if _, err := f.queue.Fail(ctx, failed, "node-a", "VOD was deleted", false); err != nil {
		t.Fatal(err)
	}
	_, done := f.claim("node-a")
	f.setStatus(done, store.JobDone)

	if job, err := f.queue.Claim(ctx, "node-b"); !errors.Is(err, ErrNoJob) || job != (api.Job{}) {
		t.Fatalf("Claim with nothing queued = %+v, %v; want no job and ErrNoJob", job, err)
	}
	if row := f.job(leased); row.Status != store.JobLeased || row.Clipper != "node-a" || row.Attempts != 1 {
		t.Errorf("leased job changed: %+v", row)
	}
}

func TestClaimWithoutClipperName(t *testing.T) {
	f := newFixture(t, Options{})
	queued := f.enqueue()
	if _, err := f.queue.Claim(context.Background(), ""); !errors.Is(err, ErrNoClipper) {
		t.Fatalf("Claim(\"\"): err = %v, want ErrNoClipper", err)
	}
	if row := f.job(queued.ID); row != queued {
		t.Errorf("job changed:\n %+v, want\n %+v", row, queued)
	}
}

func TestClaimConcurrent(t *testing.T) {
	const (
		jobs     = 120
		claimers = 24
	)
	ctx := context.Background()
	f := newFixture(t, Options{})
	want := make(map[string]bool, jobs)
	for range jobs {
		want[JobID(f.enqueue().ID)] = true
	}

	// Every claimer keeps claiming until the queue is empty.
	type claimed struct {
		job     api.Job
		clipper string
	}
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make(chan claimed, jobs+claimers)
		errs    = make(chan error, claimers)
	)
	for i := range claimers {
		clipper := fmt.Sprintf("node-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				job, err := f.queue.Claim(ctx, clipper)
				if errors.Is(err, ErrNoJob) {
					return
				}
				if err != nil {
					errs <- fmt.Errorf("%s: %w", clipper, err)
					return
				}
				results <- claimed{job, clipper}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	holder := make(map[string]string, jobs)
	for c := range results {
		if !want[c.job.ID] {
			t.Errorf("job %s was handed out but never queued", c.job.ID)
		}
		if other, dup := holder[c.job.ID]; dup {
			t.Errorf("job %s was handed out twice: to %s and to %s", c.job.ID, other, c.clipper)
		}
		holder[c.job.ID] = c.clipper
		if c.job.Attempt != 1 {
			t.Errorf("job %s: attempt = %d, want 1", c.job.ID, c.job.Attempt)
		}
	}
	if len(holder) != jobs {
		t.Errorf("%d distinct jobs handed out, want %d", len(holder), jobs)
	}

	// The rows agree with what the claimers were told.
	for id, clipper := range holder {
		rowID, err := ParseJobID(id)
		if err != nil {
			t.Fatal(err)
		}
		if row := f.job(rowID); row.Status != store.JobLeased || row.Clipper != clipper || row.Attempts != 1 {
			t.Errorf("job %s: status %q, clipper %q, attempts %d; want leased to %s once",
				id, row.Status, row.Clipper, row.Attempts, clipper)
		}
	}
}

func TestFail(t *testing.T) {
	const reason = "download failed: connection reset"

	tests := []struct {
		name        string
		maxAttempts int
		// claims is how many times the job is handed out to node-a before
		// the call under test; between them it is failed as retryable.
		claims int
		// status, when set, is forced onto the job before the call.
		status    store.JobStatus
		clipper   string
		retryable bool

		wantErr    error
		wantStatus store.JobStatus
	}{
		{name: "retryable, attempts remain", maxAttempts: 3, claims: 1, clipper: "node-a", retryable: true, wantStatus: store.JobQueued},
		{name: "retryable, one attempt left", maxAttempts: 3, claims: 2, clipper: "node-a", retryable: true, wantStatus: store.JobQueued},
		{name: "retryable, last attempt used", maxAttempts: 3, claims: 3, clipper: "node-a", retryable: true, wantStatus: store.JobFailed},
		{name: "retryable, single attempt allowed", maxAttempts: 1, claims: 1, clipper: "node-a", retryable: true, wantStatus: store.JobFailed},
		{name: "not retryable, attempts remain", maxAttempts: 3, claims: 1, clipper: "node-a", retryable: false, wantStatus: store.JobFailed},
		{name: "not retryable, last attempt used", maxAttempts: 2, claims: 2, clipper: "node-a", retryable: false, wantStatus: store.JobFailed},

		{name: "another clipper holds the lease", maxAttempts: 3, claims: 1, clipper: "node-b", retryable: true, wantErr: ErrLeaseLost},
		{name: "another clipper, not retryable", maxAttempts: 3, claims: 1, clipper: "node-b", retryable: false, wantErr: ErrLeaseLost},
		{name: "clipper name differs in case", maxAttempts: 3, claims: 1, clipper: "Node-A", retryable: true, wantErr: ErrLeaseLost},
		{name: "job was never claimed", maxAttempts: 3, claims: 0, clipper: "node-a", retryable: true, wantErr: ErrLeaseLost},
		{name: "job is queued again", maxAttempts: 3, claims: 1, status: store.JobQueued, clipper: "node-a", retryable: true, wantErr: ErrLeaseLost},
		{name: "job already failed", maxAttempts: 3, claims: 1, status: store.JobFailed, clipper: "node-a", retryable: true, wantErr: ErrLeaseLost},
		{name: "job already has a result", maxAttempts: 3, claims: 1, status: store.JobDone, clipper: "node-a", retryable: true, wantErr: ErrAlreadyCompleted},
		{name: "result came from another clipper", maxAttempts: 3, claims: 1, status: store.JobDone, clipper: "node-b", retryable: false, wantErr: ErrAlreadyCompleted},
		{name: "empty clipper name", maxAttempts: 3, claims: 1, clipper: "", retryable: true, wantErr: ErrNoClipper},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, Options{MaxAttempts: tt.maxAttempts})
			id := f.enqueue().ID
			for i := range tt.claims {
				if i > 0 {
					if _, err := f.queue.Fail(ctx, id, "node-a", "earlier failure", true); err != nil {
						t.Fatal(err)
					}
				}
				f.clock.Advance(time.Minute)
				f.claim("node-a")
			}
			if tt.status != "" {
				f.setStatus(id, tt.status)
			}
			before := f.job(id)

			f.clock.Advance(7 * time.Second)
			got, err := f.queue.Fail(ctx, id, tt.clipper, reason, tt.retryable)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Fail: err = %v, want %v", err, tt.wantErr)
				}
				if got != (store.Job{}) {
					t.Errorf("Fail returned a job with its error: %+v", got)
				}
				if after := f.job(id); after != before {
					t.Errorf("a rejected report changed the job:\n %+v, was\n %+v", after, before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			// Only the status, the lease, the error and the update time
			// change; the holder stays on the row as who held it last.
			want := before
			want.Status = tt.wantStatus
			want.LeaseExpiresAt = time.Time{}
			want.LastError = reason
			want.UpdatedAt = f.clock.Now().Truncate(time.Millisecond)
			if got != want {
				t.Errorf("Fail returned\n %+v, want\n %+v", got, want)
			}
			if after := f.job(id); after != want {
				t.Errorf("stored job\n %+v, want\n %+v", after, want)
			}
			if want.Attempts != tt.claims || want.Clipper != "node-a" {
				t.Errorf("attempts %d, clipper %q; want %d, node-a", want.Attempts, want.Clipper, tt.claims)
			}

			// A requeued job can be claimed again; a failed one cannot.
			next, err := f.queue.Claim(ctx, "node-b")
			if tt.wantStatus == store.JobQueued {
				if err != nil || next.ID != JobID(id) || next.Attempt != tt.claims+1 {
					t.Errorf("Claim after requeue = %+v, %v; want job %d with attempt %d", next, err, id, tt.claims+1)
				}
			} else if !errors.Is(err, ErrNoJob) {
				t.Errorf("Claim after the job failed = %+v, %v; want ErrNoJob", next, err)
			}
		})
	}
}

func TestFailUnknownJob(t *testing.T) {
	f := newFixture(t, Options{})
	f.enqueue()
	if _, err := f.queue.Fail(context.Background(), 99999, "node-a", "whatever", true); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("Fail: err = %v, want ErrUnknownJob", err)
	}
}

func TestFailByFormerHolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.enqueue()

	// node-a gives the job back and node-b takes it.
	_, id := f.claim("node-a")
	if _, err := f.queue.Fail(ctx, id, "node-a", "shutting down", true); err != nil {
		t.Fatal(err)
	}
	f.claim("node-b")
	before := f.job(id)

	// A late, repeated report from node-a must not touch node-b's lease.
	if _, err := f.queue.Fail(ctx, id, "node-a", "shutting down", false); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Fail by the former holder: err = %v, want ErrLeaseLost", err)
	}
	if after := f.job(id); after != before {
		t.Errorf("job changed:\n %+v, was\n %+v", after, before)
	}

	// node-b still holds it and can report on it.
	got, err := f.queue.Fail(ctx, id, "node-b", "VOD was deleted", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobFailed || got.LastError != "VOD was deleted" || got.Clipper != "node-b" {
		t.Errorf("job after node-b's report: %+v", got)
	}
}

func TestFailAfterLeaseRanOut(t *testing.T) {
	// Until the reaper takes an expired lease back, the job was given to
	// nobody else and its holder's report counts.
	f := newFixture(t, Options{})
	f.enqueue()
	_, id := f.claim("node-a")
	f.clock.Advance(time.Hour)
	got, err := f.queue.Fail(context.Background(), id, "node-a", "disk full", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobQueued {
		t.Errorf("status = %q, want %q", got.Status, store.JobQueued)
	}
}

func TestAttemptCounting(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{MaxAttempts: 3})
	id := f.enqueue().ID
	if got := f.job(id).Attempts; got != 0 {
		t.Fatalf("attempts of a job never handed out = %d, want 0", got)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		clipper := fmt.Sprintf("node-%d", attempt)
		f.clock.Advance(time.Minute)
		job, claimed := f.claim(clipper)
		if claimed != id || job.Attempt != attempt {
			t.Fatalf("claim %d: job %s with attempt %d, want job %d with attempt %d",
				attempt, job.ID, job.Attempt, id, attempt)
		}
		if got := f.job(id).Attempts; got != attempt {
			t.Fatalf("claim %d: stored attempts = %d", attempt, got)
		}

		reason := fmt.Sprintf("failure %d", attempt)
		got, err := f.queue.Fail(ctx, id, clipper, reason, true)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := store.JobQueued
		if attempt == 3 {
			wantStatus = store.JobFailed
		}
		// Failing does not count as an attempt, and the latest reason wins.
		if got.Status != wantStatus || got.Attempts != attempt || got.LastError != reason {
			t.Fatalf("after failure %d: status %q, attempts %d, error %q; want %q, %d, %q",
				attempt, got.Status, got.Attempts, got.LastError, wantStatus, attempt, reason)
		}
	}

	if _, err := f.queue.Claim(ctx, "node-4"); !errors.Is(err, ErrNoJob) {
		t.Errorf("Claim after the attempt limit: err = %v, want ErrNoJob", err)
	}
}

func TestRequeuedJobKeepsItsPlace(t *testing.T) {
	// A job that goes back to the queue is still the oldest, so it is the
	// next one handed out.
	f := newFixture(t, Options{})
	first := f.enqueue()
	f.clock.Advance(time.Second)
	second := f.enqueue()

	_, id := f.claim("node-a")
	if id != first.ID {
		t.Fatalf("claimed job %d, want %d", id, first.ID)
	}
	if _, err := f.queue.Fail(context.Background(), id, "node-a", "network error", true); err != nil {
		t.Fatal(err)
	}
	if job, id := f.claim("node-b"); id != first.ID || job.Attempt != 2 {
		t.Errorf("claimed job %d with attempt %d, want job %d with attempt 2", id, job.Attempt, first.ID)
	}
	if _, id := f.claim("node-b"); id != second.ID {
		t.Errorf("claimed job %d, want %d", id, second.ID)
	}
}
