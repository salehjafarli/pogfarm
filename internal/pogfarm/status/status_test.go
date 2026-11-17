package status

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// now is where every test's clock stands: half an hour into a UTC day.
var now = time.Date(2026, 11, 17, 0, 30, 0, 0, time.UTC)

type fixture struct {
	t     *testing.T
	store *store.Store
	queue *queue.Queue
	clock time.Time
	vod   store.VOD
	// next keeps candidate windows apart.
	next float64
	// vods counts the VODs added, for their ids.
	vods int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, store: st, clock: now}
	if f.queue, err = queue.New(st, queue.Options{Now: func() time.Time { return f.clock }}); err != nil {
		t.Fatal(err)
	}
	streamer, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform: api.PlatformTwitch, PlatformUserID: "1", Login: "somestreamer", DisplayName: "SomeStreamer", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.vod = f.addVOD(streamer.ID, store.ScanScanned, time.Time{})
	return f
}

func (f *fixture) reader(opts Options) *Reader {
	f.t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return f.clock }
	}
	r, err := New(f.store, f.queue, opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) snapshot(opts Options) Snapshot {
	f.t.Helper()
	s, err := f.reader(opts).Snapshot(context.Background())
	if err != nil {
		f.t.Fatalf("Snapshot: %v", err)
	}
	return s
}

func (f *fixture) addVOD(streamerID int64, status store.ScanStatus, next time.Time) store.VOD {
	f.t.Helper()
	f.vods++
	v, err := store.InsertVOD(context.Background(), f.store.Writer(), store.VOD{
		Platform: api.PlatformTwitch, PlatformVODID: fmt.Sprint("v", f.vods), StreamerID: streamerID,
		Title: "VOD", DurationSeconds: 3600, StartedAt: now.Add(-3 * time.Hour), EndedAt: now.Add(-2 * time.Hour),
		ScanStatus: status, NextScanAt: next,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// addJob stores a job in the given state, last changed at changed.
// clipper and heartbeat are those of its last holder; a leased job's lease
// runs a minute past its heartbeat.
func (f *fixture) addJob(status store.JobStatus, changed time.Time, clipper string, heartbeat time.Time) store.Job {
	f.t.Helper()
	ctx := context.Background()
	f.next += 100
	cand, err := store.InsertCandidate(ctx, f.store.Writer(), store.Candidate{
		Platform: api.PlatformTwitch, VODID: f.vod.ID, StartSeconds: f.next, EndSeconds: f.next + 30,
		Score: 1, Signal: store.SignalChatSpike, Status: store.CandidateQueued,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	j := store.Job{Platform: api.PlatformTwitch, CandidateID: cand.ID, Status: status,
		Clipper: clipper, LastHeartbeatAt: heartbeat, CreatedAt: changed}
	if status == store.JobLeased {
		j.LeaseExpiresAt = heartbeat.Add(time.Minute)
	}
	job, err := store.InsertJob(ctx, f.store.Writer(), j)
	if err != nil {
		f.t.Fatal(err)
	}
	return job
}

// addClip stores a clip of a new done job.
func (f *fixture) addClip(review store.ReviewStatus, created time.Time, messageID int64) store.Clip {
	f.t.Helper()
	job := f.addJob(store.JobDone, now.Add(-48*time.Hour), "", time.Time{})
	c := store.Clip{
		Platform: api.PlatformTwitch, JobID: job.ID, FilePath: "clips/x.mp4", SHA256: strings.Repeat("ab", 32),
		SizeBytes: 1, DurationSeconds: 30, Width: 1080, Height: 1920, TranscriptJSON: `{}`,
		ReviewStatus: review, CreatedAt: created,
	}
	if review != store.ReviewPending {
		c.ReviewedAt = created
	}
	if messageID != 0 {
		c.TelegramChatID, c.TelegramMessageID = 42, messageID
	}
	clip, err := store.InsertClip(context.Background(), f.store.Writer(), c)
	if err != nil {
		f.t.Fatal(err)
	}
	return clip
}

func (f *fixture) addUpload(clipID int64, target string, status store.UploadStatus) {
	f.t.Helper()
	u := store.Upload{ClipID: clipID, TargetPlatform: target, Status: status}
	if status == store.UploadDone {
		u.PostedAt = now
	}
	if _, err := store.InsertUpload(context.Background(), f.store.Writer(), u); err != nil {
		f.t.Fatal(err)
	}
}

func TestSnapshotEmpty(t *testing.T) {
	f := newFixture(t)
	got := f.snapshot(Options{})
	want := Snapshot{At: now, OfflineAfter: 2 * queue.DefaultLeaseDuration}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Snapshot of an empty database =\n%+v\nwant\n%+v", got, want)
	}
	clips, total, err := f.reader(Options{}).Pending(context.Background(), 20)
	if err != nil || len(clips) != 0 || total != 0 {
		t.Errorf("Pending = %v, %d, %v; want none", clips, total, err)
	}
}

func TestSnapshotCounts(t *testing.T) {
	f := newFixture(t)
	old := now.Add(-48 * time.Hour)
	for range 2 {
		f.addJob(store.JobQueued, old, "", time.Time{})
	}
	f.addJob(store.JobLeased, old, "node-a", now.Add(-10*time.Second))
	for range 3 {
		f.addJob(store.JobFailed, old, "node-b", now.Add(-time.Hour))
	}
	f.addJob(store.JobDone, now.Add(-time.Minute), "node-b", now.Add(-time.Hour))

	f.addVOD(f.vod.StreamerID, store.ScanNew, time.Time{})
	for range 2 {
		f.addVOD(f.vod.StreamerID, store.ScanScanning, time.Time{})
	}
	f.addVOD(f.vod.StreamerID, store.ScanFailed, now.Add(time.Hour)) // retrying
	for range 3 {
		f.addVOD(f.vod.StreamerID, store.ScanFailed, time.Time{}) // given up
	}

	sent := f.addClip(store.ReviewPending, now.Add(-time.Hour), 7)
	f.addClip(store.ReviewPending, now.Add(-time.Hour), 0)
	f.addClip(store.ReviewPending, now.Add(-time.Hour), 0)
	approved := f.addClip(store.ReviewApproved, now.Add(-time.Hour), 8)
	approved2 := f.addClip(store.ReviewApproved, now.Add(-time.Hour), 9)
	f.addClip(store.ReviewRejected, now.Add(-time.Hour), 10)

	f.addUpload(approved.ID, "youtube", store.UploadPending)
	f.addUpload(approved.ID, "tiktok", store.UploadPending)
	f.addUpload(approved.ID, "fake", store.UploadUploading)
	f.addUpload(approved2.ID, "youtube", store.UploadDone)
	f.addUpload(approved2.ID, "tiktok", store.UploadFailed)
	_ = sent

	got := f.snapshot(Options{})
	// Six done jobs carry clips, changed two days ago; one is done today.
	if want := (Jobs{Queued: 2, Leased: 1, DoneToday: 1, Failed: 3}); got.Jobs != want {
		t.Errorf("Jobs = %+v, want %+v", got.Jobs, want)
	}
	if want := (VODs{New: 1, Scanning: 2, Retrying: 1, Failed: 3}); got.VODs != want {
		t.Errorf("VODs = %+v, want %+v", got.VODs, want)
	}
	if want := (Clips{AwaitingReview: 3, Unsent: 2}); got.Clips != want {
		t.Errorf("Clips = %+v, want %+v", got.Clips, want)
	}
	if want := (Uploads{Pending: 2, Uploading: 1, Failed: 1}); got.Uploads != want {
		t.Errorf("Uploads = %+v, want %+v", got.Uploads, want)
	}
	wantClippers := []Clipper{
		{Name: "node-a", LastSeen: now.Add(-10 * time.Second), JobID: f.leasedJobOf("node-a"), Online: true},
		{Name: "node-b", LastSeen: now.Add(-time.Hour), Online: false},
	}
	if !reflect.DeepEqual(got.Clippers, wantClippers) {
		t.Errorf("Clippers =\n%+v\nwant\n%+v", got.Clippers, wantClippers)
	}
}

func (f *fixture) leasedJobOf(clipper string) int64 {
	f.t.Helper()
	var id int64
	if err := f.store.Reader().QueryRow(`SELECT id FROM jobs WHERE status = 'leased' AND clipper = ?`, clipper).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// TestDoneTodayBoundary checks that "today" starts at midnight UTC: a job
// done at the last millisecond of yesterday is not counted, one done at
// midnight is, and both leave the count when the next day starts.
func TestDoneTodayBoundary(t *testing.T) {
	f := newFixture(t)
	midnight := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	f.addJob(store.JobDone, midnight.Add(-time.Millisecond), "", time.Time{})
	f.addJob(store.JobDone, midnight, "", time.Time{})
	f.addJob(store.JobDone, midnight.Add(23*time.Hour+59*time.Minute), "", time.Time{})

	for _, tc := range []struct {
		clock time.Time
		want  int
	}{
		{midnight.Add(-time.Millisecond), 1}, // yesterday: only its last job
		{midnight, 2},
		{now, 2},
		{midnight.Add(24*time.Hour - time.Millisecond), 2},
		{midnight.Add(24 * time.Hour), 0},
		// A clock in another zone means the same instant.
		{midnight.In(time.FixedZone("UTC+4", 4*3600)), 2},
	} {
		f.clock = tc.clock
		if got := f.snapshot(Options{}).Jobs.DoneToday; got != tc.want {
			t.Errorf("at %s: DoneToday = %d, want %d", tc.clock, got, tc.want)
		}
	}
	if got := StartOfDay(time.Date(2026, 11, 17, 2, 0, 0, 0, time.FixedZone("UTC+4", 4*3600))); !got.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("StartOfDay = %s, want the UTC day the instant falls on", got)
	}
}

func TestOfflineThreshold(t *testing.T) {
	f := newFixture(t)
	f.addJob(store.JobLeased, now, "edge", now.Add(-4*time.Minute))
	f.addJob(store.JobLeased, now, "past", now.Add(-4*time.Minute-time.Millisecond))

	online := func(s Snapshot) map[string]bool {
		m := map[string]bool{}
		for _, c := range s.Clippers {
			m[c.Name] = c.Online
		}
		return m
	}
	// Default: twice the two-minute lease. Exactly at the threshold is
	// still online.
	s := f.snapshot(Options{})
	if s.OfflineAfter != 4*time.Minute {
		t.Errorf("default OfflineAfter = %s, want 4m", s.OfflineAfter)
	}
	if got, want := online(s), map[string]bool{"edge": true, "past": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("online with the default threshold = %v, want %v", got, want)
	}
	// A threshold from the options.
	if got, want := online(f.snapshot(Options{OfflineAfter: time.Minute})), map[string]bool{"edge": false, "past": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("online with a 1m threshold = %v, want %v", got, want)
	}
	if got, want := online(f.snapshot(Options{OfflineAfter: time.Hour})), map[string]bool{"edge": true, "past": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("online with a 1h threshold = %v, want %v", got, want)
	}
	if _, err := New(f.store, f.queue, Options{OfflineAfter: -time.Second}); err == nil {
		t.Error("New accepted a negative threshold")
	}
}

func TestPending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Created out of order; listed oldest first, ties by id.
	c3 := f.addClip(store.ReviewPending, now.Add(-time.Hour), 0)
	c1 := f.addClip(store.ReviewPending, now.Add(-3*time.Hour), 5)
	c2 := f.addClip(store.ReviewPending, now.Add(-3*time.Hour), 0)
	f.addClip(store.ReviewApproved, now.Add(-5*time.Hour), 6)
	f.addClip(store.ReviewRejected, now.Add(-5*time.Hour), 0)

	r := f.reader(Options{})
	clips, total, err := r.Pending(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []PendingClip{
		{ID: c1.ID, Streamer: "SomeStreamer", VODTitle: "VOD", CreatedAt: now.Add(-3 * time.Hour), Sent: true},
		{ID: c2.ID, Streamer: "SomeStreamer", VODTitle: "VOD", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: c3.ID, Streamer: "SomeStreamer", VODTitle: "VOD", CreatedAt: now.Add(-time.Hour)},
	}
	if total != 3 || !reflect.DeepEqual(clips, want) {
		t.Errorf("Pending =\n%+v, %d\nwant\n%+v, 3", clips, total, want)
	}

	clips, total, err = r.Pending(ctx, 2)
	if err != nil || total != 3 || !reflect.DeepEqual(clips, want[:2]) {
		t.Errorf("Pending(2) = %+v, %d, %v; want the oldest two of 3", clips, total, err)
	}

	// A streamer without a display name is shown by login.
	if _, err := f.store.Writer().Exec(`UPDATE streamers SET display_name = ''`); err != nil {
		t.Fatal(err)
	}
	clips, _, err = r.Pending(ctx, 1)
	if err != nil || len(clips) != 1 || clips[0].Streamer != "somestreamer" {
		t.Errorf("Pending without a display name = %+v, %v; want the login", clips, err)
	}
}
