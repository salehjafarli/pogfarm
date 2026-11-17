package intake

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

const (
	rejectedRetention = 24 * time.Hour
	postedRetention   = 7 * 24 * time.Hour
	orphanGrace       = time.Hour
)

// cleanupFixture is a fixture with a cleaner whose clock the test moves.
type cleanupFixture struct {
	*fixture
	cleaner *Cleaner
	mu      sync.Mutex
	now     time.Time
	logs    *syncBuffer
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()
	cf := &cleanupFixture{fixture: newFixture(t, 1<<20), now: t0, logs: &syncBuffer{}}
	c, err := cf.intake.NewCleaner(CleanupOptions{
		RejectedRetention: rejectedRetention,
		PostedRetention:   postedRetention,
		OrphanGrace:       orphanGrace,
		Now: func() time.Time {
			cf.mu.Lock()
			defer cf.mu.Unlock()
			return cf.now
		},
		Logger: slog.New(slog.NewTextHandler(cf.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	cf.cleaner = c
	return cf
}

// setNow sets the cleaner's clock.
func (cf *cleanupFixture) setNow(t time.Time) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	cf.now = t
}

// sweep runs one pass that must succeed.
func (cf *cleanupFixture) sweep() SweepResult {
	cf.t.Helper()
	res, err := cf.cleaner.Sweep(context.Background())
	if err != nil {
		cf.t.Fatalf("Sweep: %v", err)
	}
	return res
}

// storeClip has intake accept a clip of size bytes for a new job and
// returns its row.
func (cf *cleanupFixture) storeClip(size int) store.Clip {
	cf.t.Helper()
	jobID := cf.enqueue()
	clip := newClip(cf.t, size)
	if _, err := cf.intake.Complete(context.Background(), jobID, clip.meta(), transcript, bytes.NewReader(clip.data)); err != nil {
		cf.t.Fatal(err)
	}
	return cf.clip(jobID)
}

func (cf *cleanupFixture) clip(jobID int64) store.Clip {
	cf.t.Helper()
	c, err := store.GetClipByJob(context.Background(), cf.store.Reader(), jobID)
	if err != nil {
		cf.t.Fatal(err)
	}
	return c
}

// review records a decision on c at at.
func (cf *cleanupFixture) review(c store.Clip, status store.ReviewStatus, at time.Time) store.Clip {
	cf.t.Helper()
	c.ReviewStatus, c.ReviewedAt = status, at
	if status == store.ReviewPending {
		c.ReviewedAt = time.Time{}
	}
	if err := store.UpdateClip(context.Background(), cf.store.Writer(), c); err != nil {
		cf.t.Fatal(err)
	}
	return cf.clip(c.JobID)
}

// upload adds an upload row of c to target; postedAt is set when status is
// done.
func (cf *cleanupFixture) upload(c store.Clip, target string, status store.UploadStatus, postedAt time.Time) store.Upload {
	cf.t.Helper()
	u := store.Upload{ClipID: c.ID, TargetPlatform: target, Status: status}
	if status == store.UploadDone {
		u.PostedAt = postedAt
		u.ExternalPostID = "post-" + target
	}
	u, err := store.InsertUpload(context.Background(), cf.store.Writer(), u)
	if err != nil {
		cf.t.Fatal(err)
	}
	return u
}

func (cf *cleanupFixture) setUpload(u store.Upload, status store.UploadStatus, postedAt time.Time) store.Upload {
	cf.t.Helper()
	u.Status, u.PostedAt = status, time.Time{}
	if status == store.UploadDone {
		u.PostedAt = postedAt
	}
	if err := store.UpdateUpload(context.Background(), cf.store.Writer(), u); err != nil {
		cf.t.Fatal(err)
	}
	return u
}

// abs returns the absolute path of a path relative to the data directory.
func (cf *cleanupFixture) abs(rel string) string {
	return filepath.Join(cf.dataDir, filepath.FromSlash(rel))
}

func (cf *cleanupFixture) exists(rel string) bool {
	cf.t.Helper()
	_, err := os.Lstat(cf.abs(rel))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		cf.t.Fatal(err)
	}
	return err == nil
}

// assertKept checks that c's file is there and its row says so.
func (cf *cleanupFixture) assertKept(c store.Clip, why string) {
	cf.t.Helper()
	if !cf.exists(c.FilePath) {
		cf.t.Errorf("%s: file %s was deleted", why, c.FilePath)
	}
	if got := cf.clip(c.JobID); !got.FileRemovedAt.IsZero() {
		cf.t.Errorf("%s: file_removed_at = %s, want unset", why, got.FileRemovedAt)
	}
}

// assertRemoved checks that c's file is gone and its row says so since at.
func (cf *cleanupFixture) assertRemoved(c store.Clip, at time.Time) {
	cf.t.Helper()
	if cf.exists(c.FilePath) {
		cf.t.Errorf("file %s still exists", c.FilePath)
	}
	if got := cf.clip(c.JobID); !got.FileRemovedAt.Equal(at.Truncate(time.Millisecond)) {
		cf.t.Errorf("file_removed_at = %s, want %s", got.FileRemovedAt, at)
	}
}

// writeFile creates a file below the data directory with the given
// modification time.
func (cf *cleanupFixture) writeFile(rel string, size int, mtime time.Time) {
	cf.t.Helper()
	p := cf.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		cf.t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		cf.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		cf.t.Fatal(err)
	}
}

func TestEmptyTemp(t *testing.T) {
	cf := newCleanupFixture(t)
	kept := cf.storeClip(1000)
	cf.writeFile("tmp/upload-1.mp4", 300, t0)
	cf.writeFile("tmp/upload-2.mp4", 200, t0)
	cf.writeFile("tmp/stray/inner.bin", 50, t0)
	outside := filepath.Join(t.TempDir(), "target.mp4")
	if err := os.WriteFile(outside, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cf.abs("tmp/link.mp4")); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := cf.cleaner.EmptyTemp()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 || freed != 500 {
		t.Errorf("EmptyTemp = %d entries, %d bytes; want 4 entries, 500 bytes", removed, freed)
	}
	entries, err := os.ReadDir(cf.abs(TempDir))
	if err != nil || len(entries) != 0 {
		t.Errorf("temp directory after EmptyTemp: %d entries, err %v", len(entries), err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("target of a link in the temp directory was deleted: %v", err)
	}
	cf.assertKept(kept, "accepted clip")

	// A second call, and a call without the directory, find nothing.
	if removed, freed, err := cf.cleaner.EmptyTemp(); removed != 0 || freed != 0 || err != nil {
		t.Errorf("second EmptyTemp = %d, %d, %v", removed, freed, err)
	}
	if err := os.Remove(cf.abs(TempDir)); err != nil {
		t.Fatal(err)
	}
	if removed, _, err := cf.cleaner.EmptyTemp(); removed != 0 || err != nil {
		t.Errorf("EmptyTemp without the directory = %d, %v", removed, err)
	}
	if !cf.exists(TempDir) {
		t.Error("EmptyTemp did not create the temp directory")
	}
}

func TestSweepRejectedAfterRetention(t *testing.T) {
	cf := newCleanupFixture(t)
	c := cf.review(cf.storeClip(1234), store.ReviewRejected, t0)

	cf.setNow(t0.Add(rejectedRetention - time.Millisecond))
	if res := cf.sweep(); res != (SweepResult{}) {
		t.Fatalf("Sweep before the retention is over = %+v", res)
	}
	cf.assertKept(c, "rejected clip before its retention is over")

	at := t0.Add(rejectedRetention)
	cf.setNow(at)
	if res := cf.sweep(); res != (SweepResult{Rejected: 1, FreedBytes: 1234}) {
		t.Fatalf("Sweep = %+v, want one rejected clip, 1234 bytes", res)
	}
	cf.assertRemoved(c, at)
	if out := cf.logs.String(); out != "" && strings.Contains(out, "level=ERROR") {
		t.Errorf("errors logged:\n%s", out)
	}

	// Idempotent: nothing left to do, the removal time stays.
	cf.setNow(at.Add(time.Hour))
	if res := cf.sweep(); res != (SweepResult{}) {
		t.Errorf("second Sweep = %+v, want nothing done", res)
	}
	cf.assertRemoved(c, at)
}

func TestSweepPostedAfterRetention(t *testing.T) {
	cf := newCleanupFixture(t)
	c := cf.review(cf.storeClip(2000), store.ReviewApproved, t0)
	late := t0.Add(100 * postedRetention)

	// No upload rows yet: approved but posted nowhere.
	cf.setNow(late)
	cf.sweep()
	cf.assertKept(c, "approved clip without uploads")

	first := t0.Add(time.Hour)
	cf.upload(c, "youtube", store.UploadDone, first)
	other := cf.upload(c, "tiktok", store.UploadPending, time.Time{})
	for _, status := range []store.UploadStatus{store.UploadPending, store.UploadUploading, store.UploadFailed} {
		other = cf.setUpload(other, status, time.Time{})
		if res := cf.sweep(); res != (SweepResult{}) {
			t.Errorf("one upload %s: Sweep = %+v", status, res)
		}
		cf.assertKept(c, "approved clip with an upload "+string(status))
	}

	last := t0.Add(5 * time.Hour)
	cf.setUpload(other, store.UploadDone, last)
	// The retention runs from the last post, not the first.
	cf.setNow(last.Add(postedRetention - time.Millisecond))
	if res := cf.sweep(); res != (SweepResult{}) {
		t.Errorf("Sweep before the retention is over = %+v", res)
	}
	cf.assertKept(c, "posted clip before its retention is over")

	at := last.Add(postedRetention)
	cf.setNow(at)
	if res := cf.sweep(); res != (SweepResult{Posted: 1, FreedBytes: 2000}) {
		t.Fatalf("Sweep = %+v, want one posted clip, 2000 bytes", res)
	}
	cf.assertRemoved(c, at)
	if res := cf.sweep(); res != (SweepResult{}) {
		t.Errorf("second Sweep = %+v", res)
	}
}

func TestSweepNeverDeletesUndecidedOrUnposted(t *testing.T) {
	cf := newCleanupFixture(t)
	pending := cf.storeClip(100)
	approved := cf.review(cf.storeClip(100), store.ReviewApproved, t0)
	partly := cf.review(cf.storeClip(100), store.ReviewApproved, t0)
	cf.upload(partly, "youtube", store.UploadDone, t0)
	cf.upload(partly, "tiktok", store.UploadUploading, time.Time{})
	failed := cf.review(cf.storeClip(100), store.ReviewApproved, t0)
	cf.upload(failed, "youtube", store.UploadFailed, time.Time{})
	// Rejected, then undone before the retention was over.
	undone := cf.review(cf.review(cf.storeClip(100), store.ReviewRejected, t0), store.ReviewPending, time.Time{})

	cf.setNow(t0.AddDate(10, 0, 0))
	if res := cf.sweep(); res != (SweepResult{}) {
		t.Errorf("Sweep = %+v, want nothing done", res)
	}
	cf.assertKept(pending, "pending clip")
	cf.assertKept(approved, "approved clip without uploads")
	cf.assertKept(partly, "approved clip still uploading to one target")
	cf.assertKept(failed, "approved clip whose upload failed")
	cf.assertKept(undone, "clip whose rejection was undone")
}

func TestSweepMissingFile(t *testing.T) {
	cf := newCleanupFixture(t)
	c := cf.review(cf.storeClip(100), store.ReviewRejected, t0)
	if err := os.Remove(cf.abs(c.FilePath)); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(rejectedRetention)
	cf.setNow(at)
	if res := cf.sweep(); res != (SweepResult{Rejected: 1, Missing: 1}) {
		t.Fatalf("Sweep = %+v, want one rejected clip whose file was missing", res)
	}
	cf.assertRemoved(c, at)
	if res := cf.sweep(); res != (SweepResult{}) {
		t.Errorf("second Sweep = %+v", res)
	}
}

func TestSweepOrphans(t *testing.T) {
	cf := newCleanupFixture(t)
	kept := cf.storeClip(100)
	// Make the accepted clip's file old: being old does not make it an
	// orphan.
	if err := os.Chtimes(cf.abs(kept.FilePath), t0.Add(-48*time.Hour), t0.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cf.writeFile("clips/2026-10/job-9001.mp4", 700, t0.Add(-orphanGrace-time.Millisecond))
	cf.writeFile("clips/stray.bin", 30, t0.Add(-orphanGrace))
	cf.writeFile("clips/2026-11/job-9002.mp4", 500, t0.Add(-orphanGrace+time.Millisecond))
	// A file whose row says it was removed (its deletion failed) is an
	// orphan too.
	removed := cf.storeClip(40)
	removed.FileRemovedAt = t0.Add(-time.Hour)
	if err := store.UpdateClip(context.Background(), cf.store.Writer(), removed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cf.abs(removed.FilePath), t0.Add(-2*orphanGrace), t0.Add(-2*orphanGrace)); err != nil {
		t.Fatal(err)
	}
	// A link inside the clips directory to a file outside it.
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cf.abs("clips/2026-10/link.mp4")); err != nil {
		t.Fatal(err)
	}

	cf.setNow(t0)
	if res := cf.sweep(); res != (SweepResult{Orphans: 3, FreedBytes: 770}) {
		t.Fatalf("Sweep = %+v, want 3 orphans, 770 bytes", res)
	}
	for _, rel := range []string{"clips/2026-10/job-9001.mp4", "clips/stray.bin", removed.FilePath} {
		if cf.exists(rel) {
			t.Errorf("orphan %s not removed", rel)
		}
	}
	if !cf.exists("clips/2026-11/job-9002.mp4") {
		t.Error("orphan younger than the grace period was removed")
	}
	if !cf.exists("clips/2026-10/link.mp4") {
		t.Error("symbolic link was removed")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside the clips directory: %v", err)
	}
	cf.assertKept(kept, "accepted clip")

	if res := cf.sweep(); res != (SweepResult{}) {
		t.Errorf("second Sweep = %+v", res)
	}
	// Once it is old enough, the young orphan goes too.
	cf.setNow(t0.Add(time.Millisecond))
	if res := cf.sweep(); res != (SweepResult{Orphans: 1, FreedBytes: 500}) {
		t.Errorf("Sweep after the grace period = %+v", res)
	}
}

// A sweep while a result is between its rename and its commit never takes
// the result's file for an orphan, even with a modification time far past
// the grace period.
func TestSweepSkipsResultInFlight(t *testing.T) {
	cf := newCleanupFixture(t)
	cf.setNow(t0.AddDate(1, 0, 0))
	jobID := cf.enqueue()
	rel := ClipPath(jobID, t0)
	var (
		res  SweepResult
		err  error
		done = make(chan struct{})
	)
	cf.intake.beforeCommit = func() {
		old := t0.Add(-24 * time.Hour)
		if err := os.Chtimes(cf.abs(rel), old, old); err != nil {
			t.Error(err)
		}
		go func() {
			defer close(done)
			res, err = cf.cleaner.Sweep(context.Background())
		}()
	}
	clip := newClip(t, 900)
	if _, err := cf.intake.Complete(context.Background(), jobID, clip.meta(), transcript, bytes.NewReader(clip.data)); err != nil {
		t.Fatal(err)
	}
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if res.Orphans != 0 {
		t.Errorf("Sweep = %+v, want no orphans", res)
	}
	cf.assertKept(cf.clip(jobID), "result committed during a sweep")
}

// A file_path that does not lie below the clips directory is never
// resolved, whatever the clip's state.
func TestSweepPathOutsideClipsDir(t *testing.T) {
	cf := newCleanupFixture(t)
	outsideDir := t.TempDir()
	absTarget := filepath.Join(outsideDir, "abs.mp4")
	sentinels := []string{
		cf.abs("pogfarm-outside.mp4"),
		filepath.Join(filepath.Dir(cf.dataDir), "escaped.mp4"),
		absTarget,
	}
	for _, p := range sentinels {
		if err := os.WriteFile(p, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(p) })
	}
	paths := []string{
		"pogfarm-outside.mp4",
		"../escaped.mp4",
		"clips/../pogfarm-outside.mp4",
		"clips/../../escaped.mp4",
		filepath.ToSlash(absTarget),
		"clips",
		"clips/",
		"./clips/x.mp4",
	}
	var rows []store.Clip
	for _, p := range paths {
		c := cf.storeClip(10)
		c.FilePath = p
		rows = append(rows, cf.review(c, store.ReviewRejected, t0))
	}
	cf.setNow(t0.AddDate(1, 0, 0))
	res, err := cf.cleaner.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The accepted files of these clips no longer have a row pointing at
	// them: they go as orphans.
	if res.Skipped != len(paths) || res.Rejected != 0 || res.Orphans != len(paths) {
		t.Errorf("Sweep = %+v, want %d skipped, 0 rejected, %d orphans", res, len(paths), len(paths))
	}
	for _, p := range sentinels {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file outside the clips directory: %v", err)
		}
	}
	for _, c := range rows {
		if got := cf.clip(c.JobID); !got.FileRemovedAt.IsZero() {
			t.Errorf("file_path %q: marked removed", c.FilePath)
		}
	}
	if !strings.Contains(cf.logs.String(), "not below the clips directory") {
		t.Errorf("skipped paths not logged:\n%s", cf.logs.String())
	}
}

func TestClipFileName(t *testing.T) {
	good := map[string]string{
		"clips/2026-11/job-1.mp4": filepath.FromSlash("2026-11/job-1.mp4"),
		"clips/x.mp4":             "x.mp4",
		ClipPath(42, t0):          filepath.FromSlash("2026-11/job-42.mp4"),
	}
	for p, want := range good {
		if got, ok := clipFileName(p); !ok || got != want {
			t.Errorf("clipFileName(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{
		"", "clips", "clips/", "/clips/x.mp4", "/etc/passwd", "../x.mp4", "clips/../x.mp4",
		"clips/a/../../x.mp4", "clips/./x.mp4", "clips//x.mp4", `clips\..\x.mp4`, "clipsx/y.mp4",
		"clips/x\x00.mp4", "tmp/upload-1.mp4", "x.mp4",
	} {
		if got, ok := clipFileName(p); ok {
			t.Errorf("clipFileName(%q) = %q, accepted", p, got)
		}
	}
}

func TestCleanerRunLoop(t *testing.T) {
	cf := newCleanupFixture(t)
	c := cf.review(cf.storeClip(100), store.ReviewRejected, t0)
	cf.setNow(t0.Add(rejectedRetention))

	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cf.cleaner.run(ctx, tick)
	}()
	// The tick is unbuffered: once the loop takes it, the pass before it
	// is over. The first pass runs at the start, without a tick.
	tick <- time.Time{}
	cf.assertRemoved(c, t0.Add(rejectedRetention))

	d := cf.review(cf.storeClip(100), store.ReviewRejected, t0.Add(rejectedRetention))
	cf.setNow(t0.Add(2 * rejectedRetention))
	tick <- time.Time{} // runs the pass that removes d
	tick <- time.Time{} // waits for it
	cf.assertRemoved(d, t0.Add(2*rejectedRetention))

	cancel()
	waitDone(t, done, "run")
	if out := cf.logs.String(); !strings.Contains(out, "freed_bytes=100") {
		t.Errorf("freed space not logged:\n%s", out)
	}
}

func TestCleanerRunStopsOnCancel(t *testing.T) {
	cf := newCleanupFixture(t)

	t.Run("while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var err error
		go func() {
			defer close(done)
			err = cf.cleaner.Run(ctx, time.Hour)
		}()
		cancel()
		waitDone(t, done, "Run")
		if err != nil {
			t.Errorf("Run = %v, want nil after cancel", err)
		}
	})

	t.Run("already cancelled", func(t *testing.T) {
		c := cf.review(cf.storeClip(100), store.ReviewRejected, t0)
		cf.setNow(t0.AddDate(1, 0, 0))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := cf.cleaner.Run(ctx, time.Hour); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
		cf.assertKept(c, "clip after a cancelled Run")
	})

	t.Run("bad interval", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Second} {
			if err := cf.cleaner.Run(context.Background(), d); err == nil {
				t.Errorf("Run with interval %s: no error", d)
			}
		}
	})
}

func TestNewCleaner(t *testing.T) {
	f := newFixture(t, 1<<20)
	ok := CleanupOptions{RejectedRetention: time.Hour, PostedRetention: time.Hour}
	c, err := f.intake.NewCleaner(ok)
	if err != nil {
		t.Fatal(err)
	}
	if c.grace != DefaultOrphanGrace {
		t.Errorf("orphan grace = %s, want the default %s", c.grace, DefaultOrphanGrace)
	}
	bad := []CleanupOptions{
		{PostedRetention: time.Hour},
		{RejectedRetention: time.Hour},
		{RejectedRetention: -time.Hour, PostedRetention: time.Hour},
		{RejectedRetention: time.Hour, PostedRetention: time.Hour, OrphanGrace: -time.Second},
	}
	for _, opts := range bad {
		if _, err := f.intake.NewCleaner(opts); err == nil {
			t.Errorf("NewCleaner(%+v) succeeded", opts)
		}
	}
}

// syncBuffer is a log destination a goroutine and the test can share.
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
