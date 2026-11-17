package upload

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// t0 is the start of the injected clock.
var t0 = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

const testSHA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type notes struct {
	mu    sync.Mutex
	texts []string
	ch    chan string
}

func (n *notes) Notify(_ context.Context, text string) error {
	n.mu.Lock()
	n.texts = append(n.texts, text)
	ch := n.ch
	n.mu.Unlock()
	if ch != nil {
		ch <- text
	}
	return nil
}

func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.texts...)
}

// env is one test's database, clock, owner and data directory.
type env struct {
	t       *testing.T
	st      *store.Store
	dataDir string
	clock   *clock
	notes   *notes
	n       int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &env{t: t, st: st, dataDir: dir, clock: &clock{now: t0}, notes: &notes{}}
}

func uploadConfig(targets ...config.UploadTarget) config.Upload {
	return config.Upload{
		Targets:         targets,
		MaxAttempts:     3,
		RetryBackoff:    time.Minute,
		MaxRetryBackoff: 90 * time.Second,
		PollInterval:    time.Minute,
		AttemptTimeout:  time.Hour,
	}
}

func target(name string, dailyCap int, spacing time.Duration) config.UploadTarget {
	return config.UploadTarget{Name: name, DailyCap: dailyCap, MinSpacing: spacing}
}

func (e *env) loop(cfg config.Upload, targets ...Target) *Loop {
	e.t.Helper()
	l, err := New(Options{
		Store: e.st, DataDir: e.dataDir, Config: cfg, Targets: targets,
		Notifier: e.notes, Now: e.clock.Now,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// clip inserts a clip with the given review status and one pending upload
// row per target, and returns the clip id.
func (e *env) clip(review store.ReviewStatus, targets ...string) int64 {
	e.t.Helper()
	ctx := context.Background()
	w := e.st.Writer()
	e.n++
	n := e.n
	s, err := store.InsertStreamer(ctx, w, store.Streamer{
		Platform: api.PlatformTwitch, PlatformUserID: fmt.Sprintf("u%d", n),
		Login: fmt.Sprintf("login%d", n), DisplayName: fmt.Sprintf("Streamer%d", n), Enabled: true,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := store.InsertVOD(ctx, w, store.VOD{
		Platform: s.Platform, PlatformVODID: fmt.Sprintf("v%d", n), StreamerID: s.ID,
		DurationSeconds: 3600, StartedAt: t0.Add(-24 * time.Hour), EndedAt: t0.Add(-23 * time.Hour),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := store.InsertCandidate(ctx, w, store.Candidate{
		Platform: v.Platform, VODID: v.ID, StartSeconds: 10, EndSeconds: 40, Score: 5, Signal: store.SignalChatSpike,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := store.InsertJob(ctx, w, store.Job{Platform: c.Platform, CandidateID: c.ID, Status: store.JobDone})
	if err != nil {
		e.t.Fatal(err)
	}
	clip := store.Clip{
		Platform: j.Platform, JobID: j.ID, FilePath: fmt.Sprintf("clips/2026-03/%d.mp4", j.ID),
		SHA256: testSHA, SizeBytes: 1, DurationSeconds: 30, Width: 1080, Height: 1920,
		TranscriptJSON: `{}`, ReviewStatus: review,
	}
	if review != store.ReviewPending {
		clip.ReviewedAt = t0.Add(-time.Hour)
	}
	clip, err = store.InsertClip(ctx, w, clip)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, name := range targets {
		if _, err := store.InsertUpload(ctx, w, store.Upload{ClipID: clip.ID, TargetPlatform: name}); err != nil {
			e.t.Fatal(err)
		}
	}
	return clip.ID
}

func (e *env) upload(clipID int64, target string) store.Upload {
	e.t.Helper()
	u, err := store.GetUploadByTarget(context.Background(), e.st.Reader(), clipID, target)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) attention(uploadID int64) (at, reason string) {
	e.t.Helper()
	var a *string
	err := e.st.Reader().QueryRow(`SELECT attention_at, attention_reason FROM uploads WHERE id = ?`, uploadID).Scan(&a, &reason)
	if err != nil {
		e.t.Fatal(err)
	}
	if a != nil {
		at = *a
	}
	return at, reason
}

func (e *env) pass(l *Loop) {
	e.t.Helper()
	if err := l.Pass(context.Background()); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func TestApprovedClipUploadedOnce(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake)
	id := e.clip(store.ReviewApproved, "fake")

	e.pass(l)
	e.clock.Add(time.Hour)
	e.pass(l)

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("Upload called %d times, want 1", len(calls))
	}
	p := calls[0]
	wantPath := filepath.Join(e.dataDir, "clips", "2026-03", fmt.Sprintf("%d.mp4", id))
	if p.ClipID != id || p.FilePath != wantPath || p.Title != "Streamer1 2026-03-03" || p.Description != "" || p.Tags != nil {
		t.Errorf("post = %+v, want clip %d at %s titled %q", p, id, wantPath, "Streamer1 2026-03-03")
	}
	u := e.upload(id, "fake")
	posted := fake.Posts()[id]
	if u.Status != store.UploadDone || u.ExternalPostID != posted.ExternalID || !u.PostedAt.Equal(t0) || u.Attempts != 1 {
		t.Errorf("row = %+v, want done at %s with %s after 1 attempt", u, t0, posted.ExternalID)
	}
	texts := e.notes.all()
	if len(texts) != 1 || !strings.Contains(texts[0], posted.URL) {
		t.Errorf("notes = %q, want one with %s", texts, posted.URL)
	}
}

func TestUnapprovedClipsNeverUploaded(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake)
	pending := e.clip(store.ReviewPending, "fake")
	rejected := e.clip(store.ReviewRejected, "fake")

	e.pass(l)
	e.pass(l)

	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("Upload called %d times, want 0", n)
	}
	for _, id := range []int64{pending, rejected} {
		if u := e.upload(id, "fake"); u.Status != store.UploadPending || u.Attempts != 0 {
			t.Errorf("clip %d: row = %+v, want untouched", id, u)
		}
	}
}

// crash runs a pass whose upload posts the clip and then hangs, and stops
// the pass as pogfarm stopping would: the row is left in uploading.
func crash(t *testing.T, e *env, l *Loop, fake *Fake, clipID int64) {
	t.Helper()
	posted := fake.HangAfterPost()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Pass(ctx) }()
	<-posted
	cancel()
	<-done
	if u := e.upload(clipID, fake.Name()); u.Status != store.UploadUploading {
		t.Fatalf("after the crash the row is %s, want uploading", u.Status)
	}
	if _, ok := fake.Posts()[clipID]; !ok {
		t.Fatal("the fake did not post before the crash")
	}
}

func TestCrashWithoutCheckerHoldsForOwner(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	cfg := uploadConfig(target("fake", 10, 0))
	id := e.clip(store.ReviewApproved, "fake")
	crash(t, e, e.loop(cfg, fake), fake, id)

	// Restart: a new loop finds the row in uploading.
	l := e.loop(cfg, fake)
	e.pass(l)
	e.clock.Add(24 * time.Hour)
	e.pass(l)

	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("Upload called %d times, want 1", n)
	}
	u := e.upload(id, "fake")
	at, reason := e.attention(u.ID)
	if u.Status != store.UploadUploading || at != store.FormatTime(t0) || reason == "" {
		t.Fatalf("row = %+v attention %q %q, want uploading and held since %s", u, at, reason, t0)
	}
	texts := e.notes.all()
	if len(texts) != 1 || !strings.Contains(texts[0], "may or may not be posted") {
		t.Fatalf("notes = %q, want one asking for a look", texts)
	}

	// The owner checked and found the post.
	ctx := context.Background()
	if err := ResolveAttention(ctx, e.st, u.ID, true, "abc", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if u := e.upload(id, "fake"); u.Status != store.UploadDone || u.ExternalPostID != "abc" {
		t.Errorf("after resolving: %+v", u)
	}
	if err := ResolveAttention(ctx, e.st, u.ID, true, "abc", e.clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("resolving twice: %v, want ErrNotFound", err)
	}
}

func TestResolveAttentionNotPostedUploadsAgain(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake)
	id := e.clip(store.ReviewApproved, "fake")
	u := e.upload(id, "fake")
	// A crash before the target was reached.
	if _, err := e.st.Writer().Exec(`UPDATE uploads SET status = 'uploading', attempts = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	e.pass(l)
	if len(fake.Calls()) != 0 {
		t.Fatal("uploaded a row left in uploading")
	}
	if err := ResolveAttention(context.Background(), e.st, u.ID, false, "", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.pass(l)
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("Upload called %d times, want 1", n)
	}
	if u := e.upload(id, "fake"); u.Status != store.UploadDone {
		t.Errorf("row = %+v, want done", u)
	}
}

func TestCrashWithCheckerFindsThePost(t *testing.T) {
	e := newEnv(t)
	fake := NewCheckingFake("fake")
	cfg := uploadConfig(target("fake", 10, 0))
	id := e.clip(store.ReviewApproved, "fake")
	crash(t, e, e.loop(cfg, fake), fake.Fake, id)

	l := e.loop(cfg, fake)
	e.pass(l)
	e.pass(l)

	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("Upload called %d times, want 1", n)
	}
	posted := fake.Posts()[id]
	u := e.upload(id, "fake")
	if u.Status != store.UploadDone || u.ExternalPostID != posted.ExternalID {
		t.Errorf("row = %+v, want done with %s", u, posted.ExternalID)
	}
	if texts := e.notes.all(); len(texts) != 1 || !strings.Contains(texts[0], posted.URL) {
		t.Errorf("notes = %q, want one with %s", texts, posted.URL)
	}
}

func TestCrashWithCheckerNotPostedRetries(t *testing.T) {
	e := newEnv(t)
	fake := NewCheckingFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake)
	id := e.clip(store.ReviewApproved, "fake")
	u := e.upload(id, "fake")
	if _, err := e.st.Writer().Exec(`UPDATE uploads SET status = 'uploading', attempts = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	// The first lookup fails: the row stays as it is.
	fake.FailNextFind(errors.New("platform down"))
	if err := l.Pass(context.Background()); err == nil {
		t.Fatal("a failed lookup was not reported")
	}
	if u := e.upload(id, "fake"); u.Status != store.UploadUploading {
		t.Fatalf("after a failed lookup the row is %s", u.Status)
	}

	e.pass(l) // the lookup says not posted: back to pending, with backoff
	u = e.upload(id, "fake")
	if u.Status != store.UploadPending || !u.NextAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("row = %+v, want pending until %s", u, t0.Add(time.Minute))
	}
	e.clock.Add(time.Minute)
	e.pass(l)
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("Upload called %d times, want 1", n)
	}
	if u := e.upload(id, "fake"); u.Status != store.UploadDone || u.Attempts != 2 {
		t.Errorf("row = %+v, want done after 2 attempts", u)
	}
}

func TestTimeoutIsNotRetriedBlindly(t *testing.T) {
	for _, checking := range []bool{false, true} {
		t.Run(fmt.Sprintf("checker=%v", checking), func(t *testing.T) {
			e := newEnv(t)
			cfg := uploadConfig(target("fake", 10, 0))
			cfg.AttemptTimeout = time.Millisecond
			var tgt Target
			fake := NewFake("fake")
			tgt = fake
			if checking {
				cf := NewCheckingFake("fake")
				fake, tgt = cf.Fake, cf
			}
			l := e.loop(cfg, tgt)
			id := e.clip(store.ReviewApproved, "fake")
			fake.HangAfterPost()
			e.pass(l)
			e.pass(l)
			if n := len(fake.Calls()); n != 1 {
				t.Fatalf("Upload called %d times, want 1", n)
			}
			u := e.upload(id, "fake")
			at, _ := e.attention(u.ID)
			if checking && u.Status != store.UploadDone {
				t.Errorf("row = %+v, want done", u)
			}
			if !checking && (u.Status != store.UploadUploading || at == "") {
				t.Errorf("row = %+v attention %q, want held", u, at)
			}
		})
	}
}

func TestUnconfirmedErrorIsChecked(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake)
	id := e.clip(store.ReviewApproved, "fake")
	fake.FailNext(Unconfirmed(errors.New("connection reset after sending")))
	e.pass(l)
	e.pass(l)
	u := e.upload(id, "fake")
	if at, _ := e.attention(u.ID); u.Status != store.UploadUploading || at == "" || len(fake.Calls()) != 1 {
		t.Errorf("row = %+v attention %q after %d calls, want held after 1", u, at, len(fake.Calls()))
	}
}

func TestTransientErrorsRetryWithBackoffThenFail(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake) // 3 attempts, 1m doubling up to 90s
	id := e.clip(store.ReviewApproved, "fake")
	fake.FailNext(errors.New("503"), errors.New("503"), errors.New("503"))

	steps := []struct {
		advance time.Duration
		calls   int
		status  store.UploadStatus
		next    time.Duration // from t0; 0: unset
	}{
		{0, 1, store.UploadPending, time.Minute},
		{59 * time.Second, 1, store.UploadPending, time.Minute}, // not due yet
		{time.Second, 2, store.UploadPending, time.Minute + 90*time.Second},
		{89 * time.Second, 2, store.UploadPending, time.Minute + 90*time.Second},
		{time.Second, 3, store.UploadFailed, 0},
		{24 * time.Hour, 3, store.UploadFailed, 0},
	}
	for i, s := range steps {
		e.clock.Add(s.advance)
		e.pass(l)
		u := e.upload(id, "fake")
		var want time.Time
		if s.next != 0 {
			want = t0.Add(s.next)
		}
		if n := len(fake.Calls()); n != s.calls || u.Status != s.status || !u.NextAttemptAt.Equal(want) {
			t.Fatalf("step %d: %d calls, row %+v; want %d calls, %s, next %s", i, n, u, s.calls, s.status, want)
		}
	}
	u := e.upload(id, "fake")
	if u.Attempts != 3 || !strings.Contains(u.LastError, "503") {
		t.Errorf("row = %+v", u)
	}
	if texts := e.notes.all(); len(texts) != 1 || !strings.Contains(texts[0], "given up") {
		t.Errorf("notes = %q, want one saying it is given up", texts)
	}
}

func TestPermanentErrorFailsAtOnce(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 10, 0)), fake)
	id := e.clip(store.ReviewApproved, "fake")
	fake.FailNext(Permanent(errors.New("video rejected")))
	e.pass(l)
	e.clock.Add(time.Hour)
	e.pass(l)
	u := e.upload(id, "fake")
	if u.Status != store.UploadFailed || u.Attempts != 1 || u.LastError != "video rejected" || len(fake.Calls()) != 1 {
		t.Errorf("row = %+v after %d calls, want failed after 1", u, len(fake.Calls()))
	}
	if texts := e.notes.all(); len(texts) != 1 || !strings.Contains(texts[0], "video rejected") {
		t.Errorf("notes = %q", texts)
	}
}

func TestDailyCapAndSpacing(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 2, time.Hour)), fake)
	for range 4 {
		e.clip(store.ReviewApproved, "fake")
	}
	steps := []struct {
		advance time.Duration
		posts   int
	}{
		{0, 1},
		{0, 1},                // spacing
		{59 * time.Minute, 1}, // spacing
		{time.Minute, 2},      // t0+1h
		{2 * time.Hour, 2},    // cap: 2 in the last 24h
		{20 * time.Hour, 2},   // t0+23h: still 2
		{time.Hour, 3},        // t0+24h: the first post left the window
		{30 * time.Minute, 3}, // spacing, and the post at t0+1h is still in
		{30 * time.Minute, 4}, // t0+25h
	}
	for i, s := range steps {
		e.clock.Add(s.advance)
		e.pass(l)
		if n := len(fake.Calls()); n != s.posts {
			t.Fatalf("step %d (%s after t0): %d posts, want %d", i, e.clock.Now().Sub(t0), n, s.posts)
		}
	}
}

func TestFailingTargetDoesNotBlockAnother(t *testing.T) {
	e := newEnv(t)
	bad, good := NewFake("bad"), NewFake("good")
	l := e.loop(uploadConfig(target("bad", 10, 0), target("good", 10, 0)), bad, good)
	a := e.clip(store.ReviewApproved, "bad", "good")
	b := e.clip(store.ReviewApproved, "bad", "good")
	bad.FailNext(Permanent(errors.New("no")), Permanent(errors.New("no")))

	e.pass(l)
	e.pass(l)
	for _, id := range []int64{a, b} {
		if u := e.upload(id, "good"); u.Status != store.UploadDone {
			t.Errorf("clip %d on good: %+v", id, u)
		}
		if u := e.upload(id, "bad"); u.Status != store.UploadFailed {
			t.Errorf("clip %d on bad: %+v", id, u)
		}
	}
}

func TestStuckTargetDoesNotBlockAnother(t *testing.T) {
	e := newEnv(t)
	stuck, good := NewFake("stuck"), NewFake("good")
	l := e.loop(uploadConfig(target("stuck", 10, 0), target("good", 10, 0)), stuck, good)
	id := e.clip(store.ReviewApproved, "stuck", "good")
	entered, release := stuck.Hold()

	var wg sync.WaitGroup
	reports := make(chan string, 4)
	l.dispatch(context.Background(), &wg, func(ts *targetState, _ bool, err error) {
		if err != nil {
			t.Errorf("%s: %v", ts.name, err)
		}
		reports <- ts.name
	})
	<-entered
	if got := <-reports; got != "good" {
		t.Fatalf("first turn to end: %s, want good", got)
	}
	if u := e.upload(id, "good"); u.Status != store.UploadDone {
		t.Errorf("good: %+v", u)
	}
	// A second pass while stuck is busy leaves it alone.
	e.pass(l)
	if n := len(stuck.Calls()); n != 1 {
		t.Errorf("stuck called %d times", n)
	}
	release()
	wg.Wait()
	if u := e.upload(id, "stuck"); u.Status != store.UploadDone {
		t.Errorf("stuck: %+v", u)
	}
}

func TestConcurrentPassesPostOnce(t *testing.T) {
	e := newEnv(t)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 100, 0)), fake)
	var ids []int64
	for range 5 {
		ids = append(ids, e.clip(store.ReviewApproved, "fake"))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 6 {
				if err := l.Pass(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	e.pass(l)
	e.pass(l)
	e.pass(l)
	e.pass(l)
	e.pass(l)

	seen := map[int64]int{}
	for _, p := range fake.Calls() {
		seen[p.ClipID]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("clip %d uploaded %d times", id, seen[id])
		}
		if u := e.upload(id, "fake"); u.Status != store.UploadDone {
			t.Errorf("clip %d: %+v", id, u)
		}
	}
}

func TestRunDrainsBacklogAndStops(t *testing.T) {
	e := newEnv(t)
	e.notes.ch = make(chan string, 8)
	fake := NewFake("fake")
	l := e.loop(uploadConfig(target("fake", 100, 0)), fake)
	for range 3 {
		e.clip(store.ReviewApproved, "fake")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// No ticks: only the start and the wake-ups after work.
		l.run(ctx, nil)
		close(done)
	}()
	for range 3 {
		<-e.notes.ch
	}
	cancel()
	<-done
	if n := len(fake.Calls()); n != 3 {
		t.Errorf("%d uploads, want 3", n)
	}
}

func TestNewChecksTargets(t *testing.T) {
	e := newEnv(t)
	opts := Options{Store: e.st, DataDir: e.dataDir, Config: uploadConfig(target("youtube", 1, 0))}
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "no implementation") {
		t.Errorf("enabled target without implementation: %v", err)
	}
	off := false
	opts.Config.Targets[0].Enabled = &off
	if _, err := New(opts); err != nil {
		t.Errorf("disabled target without implementation: %v", err)
	}
	opts.Targets = []Target{NewFake("a"), NewFake("a")}
	if _, err := New(opts); err == nil {
		t.Error("two targets with one name accepted")
	}
}

func TestAttentionOnlyWhileUploading(t *testing.T) {
	e := newEnv(t)
	id := e.clip(store.ReviewApproved, "fake")
	u := e.upload(id, "fake")
	_, err := e.st.Writer().Exec(`UPDATE uploads SET attention_at = ? WHERE id = ?`, store.FormatTime(t0), u.ID)
	if err == nil {
		t.Error("attention_at set on a pending row")
	}
}

func TestErrorClasses(t *testing.T) {
	base := errors.New("x")
	if Permanent(nil) != nil || Unconfirmed(nil) != nil {
		t.Error("nil not kept")
	}
	p := fmt.Errorf("wrapped: %w", Permanent(base))
	if !IsPermanent(p) || IsUnconfirmed(p) || !errors.Is(p, base) {
		t.Error("Permanent")
	}
	u := Unconfirmed(base)
	if !IsUnconfirmed(u) || IsPermanent(u) || !errors.Is(u, base) || IsPermanent(base) {
		t.Error("Unconfirmed")
	}
}
