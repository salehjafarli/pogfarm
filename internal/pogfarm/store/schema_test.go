package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
)

var coreTables = []string{"streamers", "vods", "candidates", "jobs", "clips", "uploads"}

// t0 is a fixed time with a sub-second part, so tests notice lost precision.
var t0 = time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC)

const testSHA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// openCore opens a new database with the real, embedded migrations.
func openCore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Path(t.TempDir()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

// fixture inserts valid rows; n keeps the rows of one test distinct.
type fixture struct {
	t *testing.T
	s *Store
	n int
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, s: openCore(t)}
}

func (f *fixture) next() int {
	f.n++
	return f.n
}

func (f *fixture) streamer() Streamer {
	f.t.Helper()
	n := f.next()
	s, err := InsertStreamer(context.Background(), f.s.Writer(), Streamer{
		Platform:       api.PlatformTwitch,
		PlatformUserID: fmt.Sprintf("u%d", n),
		Login:          fmt.Sprintf("login%d", n),
		DisplayName:    fmt.Sprintf("Login %d", n),
		Enabled:        true,
	})
	if err != nil {
		f.t.Fatalf("insert streamer: %v", err)
	}
	return s
}

func (f *fixture) vod() VOD {
	f.t.Helper()
	st := f.streamer()
	v, err := InsertVOD(context.Background(), f.s.Writer(), VOD{
		Platform:        st.Platform,
		PlatformVODID:   fmt.Sprintf("v%d", f.next()),
		StreamerID:      st.ID,
		Title:           "a stream",
		DurationSeconds: 3600,
		StartedAt:       t0,
		EndedAt:         t0.Add(time.Hour),
	})
	if err != nil {
		f.t.Fatalf("insert vod: %v", err)
	}
	return v
}

func (f *fixture) candidate() Candidate {
	f.t.Helper()
	v := f.vod()
	c, err := InsertCandidate(context.Background(), f.s.Writer(), Candidate{
		Platform: v.Platform, VODID: v.ID, StartSeconds: 100.5, EndSeconds: 130.25, Score: 7.5, Signal: SignalChatSpike,
	})
	if err != nil {
		f.t.Fatalf("insert candidate: %v", err)
	}
	return c
}

func (f *fixture) job() Job {
	f.t.Helper()
	c := f.candidate()
	j, err := InsertJob(context.Background(), f.s.Writer(), Job{Platform: c.Platform, CandidateID: c.ID})
	if err != nil {
		f.t.Fatalf("insert job: %v", err)
	}
	return j
}

func clipFor(j Job) Clip {
	return Clip{
		Platform:        j.Platform,
		JobID:           j.ID,
		FilePath:        fmt.Sprintf("clips/2026-03/%d.mp4", j.ID),
		SHA256:          testSHA,
		SizeBytes:       1 << 20,
		DurationSeconds: 29.75,
		Width:           1080,
		Height:          1920,
		TranscriptJSON:  `{"language":"en","words":[{"text":"pog","start_seconds":0.5,"end_seconds":0.9}]}`,
		Clipper:         "laptop-1",
	}
}

func (f *fixture) clip() Clip {
	f.t.Helper()
	c, err := InsertClip(context.Background(), f.s.Writer(), clipFor(f.job()))
	if err != nil {
		f.t.Fatalf("insert clip: %v", err)
	}
	return c
}

func (f *fixture) upload() Upload {
	f.t.Helper()
	c := f.clip()
	u, err := InsertUpload(context.Background(), f.s.Writer(), Upload{ClipID: c.ID, TargetPlatform: "youtube"})
	if err != nil {
		f.t.Fatalf("insert upload: %v", err)
	}
	return u
}

// exec runs a hand-written statement on the writer.
func (f *fixture) exec(query string, args ...any) error {
	_, err := f.s.Writer().Exec(query, args...)
	return err
}

func wantDuplicate(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("%s: error = %v, want ErrDuplicate", what, err)
	}
}

func wantNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("%s: error = %v, want ErrNotFound", what, err)
	}
}

func wantRejected(t *testing.T, what, kind string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted, want a %s error", what, kind)
		return
	}
	if !strings.Contains(err.Error(), kind) {
		t.Errorf("%s: error = %v, want a %s error", what, err, kind)
	}
	if errors.Is(err, ErrDuplicate) || errors.Is(err, ErrNotFound) {
		t.Errorf("%s: error %v must not be ErrDuplicate or ErrNotFound", what, err)
	}
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCoreTablesMigration(t *testing.T) {
	path := Path(t.TempDir())
	for i := range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if got, want := strings.Join(appliedList(t, s.Reader()), ","), "1:core_tables,2:upload_attention"; got != want {
			t.Errorf("open %d: applied migrations = %q, want %q", i, got, want)
		}
		for _, table := range coreTables {
			var strict int
			err := s.Reader().QueryRow(`SELECT "strict" FROM pragma_table_list WHERE schema = 'main' AND name = ?`, table).Scan(&strict)
			if err != nil {
				t.Errorf("table %s: %v", table, err)
			} else if strict != 1 {
				t.Errorf("table %s is not STRICT", table)
			}
		}
		var appliedAt string
		if err := s.Reader().QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&appliedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseTime(appliedAt); err != nil {
			t.Errorf("applied_at is not in the stored time format: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// The schema has no violations of its own foreign keys and passes SQLite's
// integrity check after every table has rows.
func TestCoreTablesIntegrity(t *testing.T) {
	f := newFixture(t)
	f.upload()
	rows, err := f.s.Reader().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("foreign_key_check reports a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := f.s.Reader().QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Errorf("integrity_check = %q, %v", result, err)
	}
	for _, table := range coreTables {
		if got := count(t, f.s, table); got != 1 {
			t.Errorf("%s has %d rows, want 1", table, got)
		}
	}
}

func TestTimeFormat(t *testing.T) {
	loc := time.FixedZone("plus4", 4*3600)
	in := time.Date(2026, 1, 2, 3, 4, 5, 6_789_000, loc)
	s := FormatTime(in)
	if want := "2026-01-01T23:04:05.006Z"; s != want {
		t.Errorf("FormatTime = %q, want %q", s, want)
	}
	back, err := ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	if want := in.UTC().Truncate(time.Millisecond); back != want {
		t.Errorf("ParseTime = %v, want %v", back, want)
	}
	// A whole second keeps its zeros, so every stored time has one width
	// and string order is time order.
	times := []time.Time{t0.Add(-time.Hour), t0.Truncate(time.Second), t0, t0.Add(time.Millisecond), t0.Add(24 * time.Hour)}
	for i, tm := range times {
		if got := FormatTime(tm); len(got) != len(TimeLayout) {
			t.Errorf("FormatTime(%v) = %q, width %d, want %d", tm, got, len(got), len(TimeLayout))
		}
		if i > 0 && FormatTime(times[i-1]) >= FormatTime(tm) {
			t.Errorf("%q does not sort before %q", FormatTime(times[i-1]), FormatTime(tm))
		}
	}
	for _, bad := range []string{"", "2026-01-02T03:04:05Z", "2026-01-02 03:04:05.000", "2026-01-02T03:04:05.000+04:00"} {
		if _, err := ParseTime(bad); err == nil {
			t.Errorf("ParseTime(%q) succeeded", bad)
		}
	}
}

func TestStreamerInsertGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	in := f.streamer()
	if in.ID == 0 || in.CreatedAt.Before(before) || in.UpdatedAt != in.CreatedAt {
		t.Errorf("inserted row = %+v", in)
	}
	got, err := GetStreamer(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("get = %+v, want %+v", got, in)
	}
	byID, err := GetStreamerByPlatformID(ctx, f.s.Reader(), api.PlatformTwitch, in.PlatformUserID)
	if err != nil || byID != in {
		t.Errorf("get by platform id = %+v, %v", byID, err)
	}
	var platform string
	if err := f.s.Reader().QueryRow(`SELECT platform FROM streamers WHERE id = ?`, in.ID).Scan(&platform); err != nil || platform != "twitch" {
		t.Errorf("stored platform = %q, %v; want the contract's name twitch", platform, err)
	}

	got.Login, got.DisplayName, got.Enabled = "renamed", "Renamed", false
	if err := UpdateStreamer(ctx, f.s.Writer(), got); err != nil {
		t.Fatal(err)
	}
	after, err := GetStreamer(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Login != "renamed" || after.DisplayName != "Renamed" || after.Enabled {
		t.Errorf("after update = %+v", after)
	}
	if after.CreatedAt != in.CreatedAt || after.UpdatedAt.Before(in.UpdatedAt) {
		t.Errorf("timestamps after update: created %v (was %v), updated %v", after.CreatedAt, in.CreatedAt, after.UpdatedAt)
	}

	_, err = GetStreamer(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing", err)
	_, err = GetStreamerByPlatformID(ctx, f.s.Reader(), "other", in.PlatformUserID)
	wantNotFound(t, "get on another platform", err)
	wantNotFound(t, "update missing", UpdateStreamer(ctx, f.s.Writer(), Streamer{ID: 999, Login: "x"}))
}

func TestStreamerUnique(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.streamer()
	_, err := InsertStreamer(ctx, f.s.Writer(), Streamer{Platform: a.Platform, PlatformUserID: a.PlatformUserID, Login: "other", DisplayName: "Other"})
	wantDuplicate(t, "same platform user id", err)
	// The same id on another platform is a different streamer.
	if _, err := InsertStreamer(ctx, f.s.Writer(), Streamer{Platform: "other", PlatformUserID: a.PlatformUserID, Login: a.Login, DisplayName: "x"}); err != nil {
		t.Errorf("same user id on another platform: %v", err)
	}
	if got := count(t, f.s, "streamers"); got != 2 {
		t.Errorf("streamers = %d, want 2", got)
	}
}

func TestVODInsertGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := f.vod()
	if in.ScanStatus != ScanNew {
		t.Errorf("scan status = %q, want new", in.ScanStatus)
	}
	got, err := GetVOD(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("get = %+v, want %+v", got, in)
	}
	if got.StartedAt != t0 || got.EndedAt != t0.Add(time.Hour) || !got.NextScanAt.IsZero() {
		t.Errorf("times = %v, %v, %v", got.StartedAt, got.EndedAt, got.NextScanAt)
	}
	byID, err := GetVODByPlatformID(ctx, f.s.Reader(), in.Platform, in.PlatformVODID)
	if err != nil || byID != in {
		t.Errorf("get by platform id = %+v, %v", byID, err)
	}

	retry := t0.Add(2 * time.Hour)
	for _, status := range []ScanStatus{ScanNew, ScanScanning, ScanScanned, ScanFailed} {
		got.ScanStatus, got.ScanAttempts, got.ScanError, got.NextScanAt = status, 2, "chat download failed", retry
		got.Title, got.DurationSeconds = "new title", 1800.5
		if err := UpdateVOD(ctx, f.s.Writer(), got); err != nil {
			t.Fatalf("update to %s: %v", status, err)
		}
		after, err := GetVOD(ctx, f.s.Reader(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.ScanStatus != status || after.ScanAttempts != 2 || after.ScanError != "chat download failed" ||
			after.NextScanAt != retry || after.Title != "new title" || after.DurationSeconds != 1800.5 {
			t.Errorf("after update to %s = %+v", status, after)
		}
	}
	got.NextScanAt = time.Time{}
	if err := UpdateVOD(ctx, f.s.Writer(), got); err != nil {
		t.Fatal(err)
	}
	if after, _ := GetVOD(ctx, f.s.Reader(), in.ID); !after.NextScanAt.IsZero() {
		t.Errorf("next scan time was not cleared: %v", after.NextScanAt)
	}

	_, err = GetVOD(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing", err)
	_, err = GetVODByPlatformID(ctx, f.s.Reader(), in.Platform, "nope")
	wantNotFound(t, "get missing by platform id", err)
	wantNotFound(t, "update missing", UpdateVOD(ctx, f.s.Writer(), VOD{ID: 999, StartedAt: t0, EndedAt: t0, ScanStatus: ScanNew}))
}

func TestVODRecordedOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.vod()

	again := first
	again.ID, again.Title, again.ScanStatus = 0, "changed", ScanScanned
	_, err := InsertVOD(ctx, f.s.Writer(), again)
	wantDuplicate(t, "same platform VOD id", err)

	if got := count(t, f.s, "vods"); got != 1 {
		t.Errorf("vods = %d, want 1", got)
	}
	kept, err := GetVODByPlatformID(ctx, f.s.Reader(), first.Platform, first.PlatformVODID)
	if err != nil || kept != first {
		t.Errorf("existing row changed: %+v, %v", kept, err)
	}

	// The duplicate is rejected inside a transaction too, and the
	// transaction can go on.
	err = f.s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := InsertVOD(ctx, tx, again); !errors.Is(err, ErrDuplicate) {
			return fmt.Errorf("in transaction: %v, want ErrDuplicate", err)
		}
		again.PlatformVODID = "another"
		_, err := InsertVOD(ctx, tx, again)
		return err
	})
	if err != nil {
		t.Error(err)
	}
	if got := count(t, f.s, "vods"); got != 2 {
		t.Errorf("vods = %d, want 2", got)
	}

	// The same VOD id on another platform is a different VOD.
	other, err := InsertStreamer(ctx, f.s.Writer(), Streamer{Platform: "other", PlatformUserID: "1", Login: "o", DisplayName: "O"})
	if err != nil {
		t.Fatal(err)
	}
	again.Platform, again.StreamerID, again.PlatformVODID = "other", other.ID, first.PlatformVODID
	if _, err := InsertVOD(ctx, f.s.Writer(), again); err != nil {
		t.Errorf("same VOD id on another platform: %v", err)
	}
}

func TestCandidateInsertGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := f.candidate()
	if in.Status != CandidateNew {
		t.Errorf("status = %q, want new", in.Status)
	}
	got, err := GetCandidate(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("get = %+v, want %+v", got, in)
	}
	for _, status := range []CandidateStatus{CandidateNew, CandidateQueued, CandidateSkipped} {
		for _, signal := range []Signal{SignalChatSpike, SignalViewerClip, SignalBoth} {
			got.Status, got.Signal, got.Score = status, signal, 9.25
			if err := UpdateCandidate(ctx, f.s.Writer(), got); err != nil {
				t.Fatalf("update to %s/%s: %v", status, signal, err)
			}
			after, err := GetCandidate(ctx, f.s.Reader(), in.ID)
			if err != nil || after.Status != status || after.Signal != signal || after.Score != 9.25 {
				t.Errorf("after update to %s/%s = %+v, %v", status, signal, after, err)
			}
		}
	}
	_, err = GetCandidate(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing", err)
	wantNotFound(t, "update missing", UpdateCandidate(ctx, f.s.Writer(), Candidate{ID: 999, Signal: SignalBoth, Status: CandidateNew}))
}

func TestCandidateUnique(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.candidate()
	dup := a
	dup.ID, dup.Score, dup.Signal = 0, 1, SignalBoth
	_, err := InsertCandidate(ctx, f.s.Writer(), dup)
	wantDuplicate(t, "same VOD and window", err)
	dup.EndSeconds++
	if _, err := InsertCandidate(ctx, f.s.Writer(), dup); err != nil {
		t.Errorf("another window of the same VOD: %v", err)
	}
}

func TestJobInsertGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := f.job()
	if in.Status != JobQueued || in.Attempts != 0 || !in.LeaseExpiresAt.IsZero() {
		t.Errorf("inserted job = %+v", in)
	}
	got, err := GetJob(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("get = %+v, want %+v", got, in)
	}
	byCandidate, err := GetJobByCandidate(ctx, f.s.Reader(), in.CandidateID)
	if err != nil || byCandidate != in {
		t.Errorf("get by candidate = %+v, %v", byCandidate, err)
	}

	lease := t0.Add(2 * time.Minute)
	steps := []Job{
		{Status: JobLeased, Attempts: 1, Clipper: "laptop-1", LeaseExpiresAt: lease, LastHeartbeatAt: t0},
		{Status: JobQueued, Attempts: 1, Clipper: "laptop-1", LastHeartbeatAt: t0, LastError: "lease expired"},
		{Status: JobLeased, Attempts: 2, Clipper: "pc-1", LeaseExpiresAt: lease.Add(time.Hour), LastHeartbeatAt: t0.Add(time.Hour)},
		{Status: JobDone, Attempts: 2, Clipper: "pc-1", LastHeartbeatAt: t0.Add(time.Hour)},
		{Status: JobFailed, Attempts: 3, Clipper: "pc-1", LastHeartbeatAt: t0.Add(time.Hour), LastError: "vod deleted"},
	}
	for _, step := range steps {
		step.ID = in.ID
		if err := UpdateJob(ctx, f.s.Writer(), step); err != nil {
			t.Fatalf("update to %s: %v", step.Status, err)
		}
		after, err := GetJob(ctx, f.s.Reader(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != step.Status || after.Attempts != step.Attempts || after.Clipper != step.Clipper ||
			after.LeaseExpiresAt != step.LeaseExpiresAt || after.LastHeartbeatAt != step.LastHeartbeatAt ||
			after.LastError != step.LastError {
			t.Errorf("after update to %s = %+v", step.Status, after)
		}
		if after.Platform != in.Platform || after.CandidateID != in.CandidateID || after.CreatedAt != in.CreatedAt {
			t.Errorf("update changed fixed fields: %+v", after)
		}
	}

	_, err = GetJob(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing", err)
	_, err = GetJobByCandidate(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing by candidate", err)
	wantNotFound(t, "update missing", UpdateJob(ctx, f.s.Writer(), Job{ID: 999, Status: JobQueued}))
}

func TestJobUniquePerCandidate(t *testing.T) {
	f := newFixture(t)
	a := f.job()
	_, err := InsertJob(context.Background(), f.s.Writer(), Job{Platform: a.Platform, CandidateID: a.CandidateID})
	wantDuplicate(t, "second job for a candidate", err)
	if got := count(t, f.s, "jobs"); got != 1 {
		t.Errorf("jobs = %d, want 1", got)
	}
}

func TestClipInsertGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := f.clip()
	if in.ReviewStatus != ReviewPending || !in.ReviewedAt.IsZero() || in.TelegramMessageID != 0 {
		t.Errorf("inserted clip = %+v", in)
	}
	got, err := GetClip(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("get = %+v, want %+v", got, in)
	}
	byJob, err := GetClipByJob(ctx, f.s.Reader(), in.JobID)
	if err != nil || byJob != in {
		t.Errorf("get by job = %+v, %v", byJob, err)
	}

	// Sent for review. Telegram chat ids of groups are negative.
	got.TelegramChatID, got.TelegramMessageID = -1001234567890, 42
	if err := UpdateClip(ctx, f.s.Writer(), got); err != nil {
		t.Fatal(err)
	}
	reviewed := t0.Add(time.Minute)
	removed := t0.Add(48 * time.Hour)
	steps := []Clip{
		{ReviewStatus: ReviewApproved, ReviewedAt: reviewed},
		{ReviewStatus: ReviewPending}, // undo
		{ReviewStatus: ReviewRejected, ReviewedAt: reviewed},
		{ReviewStatus: ReviewRejected, ReviewedAt: reviewed, FileRemovedAt: removed},
	}
	for _, step := range steps {
		got.ReviewStatus, got.ReviewedAt, got.FileRemovedAt = step.ReviewStatus, step.ReviewedAt, step.FileRemovedAt
		got.FilePath = "clips/2026-03/moved.mp4"
		if err := UpdateClip(ctx, f.s.Writer(), got); err != nil {
			t.Fatalf("update to %s: %v", step.ReviewStatus, err)
		}
		after, err := GetClip(ctx, f.s.Reader(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.ReviewStatus != step.ReviewStatus || after.ReviewedAt != step.ReviewedAt ||
			after.FileRemovedAt != step.FileRemovedAt || after.FilePath != "clips/2026-03/moved.mp4" ||
			after.TelegramChatID != -1001234567890 || after.TelegramMessageID != 42 {
			t.Errorf("after update to %s = %+v", step.ReviewStatus, after)
		}
		if after.SHA256 != in.SHA256 || after.TranscriptJSON != in.TranscriptJSON || after.SizeBytes != in.SizeBytes ||
			after.DurationSeconds != in.DurationSeconds || after.Width != in.Width || after.Height != in.Height ||
			after.Clipper != in.Clipper || after.JobID != in.JobID {
			t.Errorf("update changed fixed fields: %+v", after)
		}
	}

	_, err = GetClip(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing", err)
	_, err = GetClipByJob(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing by job", err)
	wantNotFound(t, "update missing", UpdateClip(ctx, f.s.Writer(), Clip{ID: 999, ReviewStatus: ReviewPending}))
}

// A job has at most one clip, whatever way the second one is written.
func TestSecondClipForAJobIsImpossible(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.clip()
	job, err := GetJob(ctx, f.s.Reader(), first.JobID)
	if err != nil {
		t.Fatal(err)
	}

	second := clipFor(job)
	second.FilePath, second.Clipper = "clips/2026-03/late.mp4", "pc-1"
	_, err = InsertClip(ctx, f.s.Writer(), second)
	wantDuplicate(t, "second clip through InsertClip", err)

	err = f.exec(`INSERT INTO clips (platform, job_id, file_path, sha256, size_bytes, duration_seconds, width, height,
			transcript_json, created_at, updated_at)
		VALUES ('twitch', ?, 'x.mp4', ?, 1, 1, 1, 1, '{}', ?, ?)`, job.ID, testSHA, FormatTime(t0), FormatTime(t0))
	wantRejected(t, "second clip through plain SQL", "UNIQUE constraint failed: clips.job_id", err)

	err = f.exec(`INSERT OR IGNORE INTO clips (platform, job_id, file_path, sha256, size_bytes, duration_seconds, width, height,
			transcript_json, created_at, updated_at)
		VALUES ('twitch', ?, 'x.mp4', ?, 1, 1, 1, 1, '{}', ?, ?)`, job.ID, testSHA, FormatTime(t0), FormatTime(t0))
	if err != nil {
		t.Errorf("INSERT OR IGNORE: %v", err)
	}

	// Moving another job's clip onto this job is rejected as well.
	other := f.clip()
	err = f.exec(`UPDATE clips SET job_id = ? WHERE id = ?`, job.ID, other.ID)
	wantRejected(t, "moving a clip onto the job", "UNIQUE constraint failed: clips.job_id", err)

	var n int
	if err := f.s.Reader().QueryRow(`SELECT count(*) FROM clips WHERE job_id = ?`, job.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("clips of the job = %d, %v; want 1", n, err)
	}
	kept, err := GetClipByJob(ctx, f.s.Reader(), job.ID)
	if err != nil || kept != first {
		t.Errorf("the first result changed: %+v, %v", kept, err)
	}
}

func TestClipTelegramMessageUnique(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, c := f.clip(), f.clip(), f.clip()
	// Several unsent clips coexist (a, b and c were all inserted unsent).
	a.TelegramChatID, a.TelegramMessageID = 7, 100
	if err := UpdateClip(ctx, f.s.Writer(), a); err != nil {
		t.Fatal(err)
	}
	b.TelegramChatID, b.TelegramMessageID = 7, 100
	wantDuplicate(t, "same chat and message", UpdateClip(ctx, f.s.Writer(), b))
	c.TelegramChatID, c.TelegramMessageID = 7, 101
	if err := UpdateClip(ctx, f.s.Writer(), c); err != nil {
		t.Errorf("another message in the same chat: %v", err)
	}
}

func TestUploadInsertGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := f.upload()
	if in.Status != UploadPending || in.Attempts != 0 || !in.PostedAt.IsZero() {
		t.Errorf("inserted upload = %+v", in)
	}
	got, err := GetUpload(ctx, f.s.Reader(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("get = %+v, want %+v", got, in)
	}
	byTarget, err := GetUploadByTarget(ctx, f.s.Reader(), in.ClipID, "youtube")
	if err != nil || byTarget != in {
		t.Errorf("get by target = %+v, %v", byTarget, err)
	}

	retry := t0.Add(10 * time.Minute)
	posted := t0.Add(20 * time.Minute)
	steps := []Upload{
		{Status: UploadUploading, Attempts: 1},
		{Status: UploadPending, Attempts: 1, LastError: "timeout", NextAttemptAt: retry},
		{Status: UploadUploading, Attempts: 2},
		{Status: UploadDone, Attempts: 2, ExternalPostID: "abc123", PostedAt: posted},
		{Status: UploadFailed, Attempts: 3, LastError: "rejected by the platform"},
	}
	for _, step := range steps {
		step.ID = in.ID
		if err := UpdateUpload(ctx, f.s.Writer(), step); err != nil {
			t.Fatalf("update to %s: %v", step.Status, err)
		}
		after, err := GetUpload(ctx, f.s.Reader(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != step.Status || after.Attempts != step.Attempts || after.LastError != step.LastError ||
			after.ExternalPostID != step.ExternalPostID || after.NextAttemptAt != step.NextAttemptAt ||
			after.PostedAt != step.PostedAt {
			t.Errorf("after update to %s = %+v", step.Status, after)
		}
		if after.ClipID != in.ClipID || after.TargetPlatform != "youtube" {
			t.Errorf("update changed fixed fields: %+v", after)
		}
	}

	_, err = GetUpload(ctx, f.s.Reader(), 999)
	wantNotFound(t, "get missing", err)
	_, err = GetUploadByTarget(ctx, f.s.Reader(), in.ClipID, "tiktok")
	wantNotFound(t, "get missing by target", err)
	wantNotFound(t, "update missing", UpdateUpload(ctx, f.s.Writer(), Upload{ID: 999, Status: UploadPending}))
}

func TestUploadUniquePerClipAndTarget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.upload()
	_, err := InsertUpload(ctx, f.s.Writer(), Upload{ClipID: a.ClipID, TargetPlatform: a.TargetPlatform})
	wantDuplicate(t, "same clip and target", err)
	if _, err := InsertUpload(ctx, f.s.Writer(), Upload{ClipID: a.ClipID, TargetPlatform: "tiktok"}); err != nil {
		t.Errorf("same clip, another target: %v", err)
	}
	b := f.clip()
	if _, err := InsertUpload(ctx, f.s.Writer(), Upload{ClipID: b.ID, TargetPlatform: a.TargetPlatform}); err != nil {
		t.Errorf("another clip, same target: %v", err)
	}
	if got := count(t, f.s, "uploads"); got != 3 {
		t.Errorf("uploads = %d, want 3", got)
	}
}

func TestForeignKeys(t *testing.T) {
	const fk = "FOREIGN KEY constraint failed"
	f := newFixture(t)
	ctx := context.Background()
	w := f.s.Writer()
	up := f.upload()
	clip, _ := GetClip(ctx, w, up.ClipID)
	job, _ := GetJob(ctx, w, clip.JobID)
	cand, _ := GetCandidate(ctx, w, job.CandidateID)
	vod, _ := GetVOD(ctx, w, cand.VODID)
	if vod.ID == 0 {
		t.Fatal("fixture chain is incomplete")
	}
	// A second streamer on another platform, with rows down to a job, for
	// the platform mismatch cases.
	other, err := InsertStreamer(ctx, w, Streamer{Platform: "other", PlatformUserID: "1", Login: "o", DisplayName: "O"})
	if err != nil {
		t.Fatal(err)
	}

	// Missing parents.
	_, err = InsertVOD(ctx, w, VOD{Platform: api.PlatformTwitch, PlatformVODID: "x1", StreamerID: 999, StartedAt: t0, EndedAt: t0})
	wantRejected(t, "vod without streamer", fk, err)
	_, err = InsertCandidate(ctx, w, Candidate{Platform: api.PlatformTwitch, VODID: 999, StartSeconds: 1, EndSeconds: 2, Signal: SignalBoth})
	wantRejected(t, "candidate without vod", fk, err)
	_, err = InsertJob(ctx, w, Job{Platform: api.PlatformTwitch, CandidateID: 999})
	wantRejected(t, "job without candidate", fk, err)
	_, err = InsertClip(ctx, w, clipFor(Job{ID: 999, Platform: api.PlatformTwitch}))
	wantRejected(t, "clip without job", fk, err)
	_, err = InsertUpload(ctx, w, Upload{ClipID: 999, TargetPlatform: "youtube"})
	wantRejected(t, "upload without clip", fk, err)

	// A child cannot name another platform than its parent.
	_, err = InsertVOD(ctx, w, VOD{Platform: api.PlatformTwitch, PlatformVODID: "x2", StreamerID: other.ID, StartedAt: t0, EndedAt: t0})
	wantRejected(t, "vod on another platform than its streamer", fk, err)
	_, err = InsertCandidate(ctx, w, Candidate{Platform: "other", VODID: vod.ID, StartSeconds: 1, EndSeconds: 2, Signal: SignalBoth})
	wantRejected(t, "candidate on another platform than its vod", fk, err)
	cand2 := f.candidate()
	_, err = InsertJob(ctx, w, Job{Platform: "other", CandidateID: cand2.ID})
	wantRejected(t, "job on another platform than its candidate", fk, err)
	job2 := f.job()
	wrong := clipFor(job2)
	wrong.Platform = "other"
	_, err = InsertClip(ctx, w, wrong)
	wantRejected(t, "clip on another platform than its job", fk, err)
	_, err = InsertClip(ctx, w, clipFor(Job{ID: job2.ID}))
	wantRejected(t, "clip without a platform", "constraint failed", err)

	// Nor can the platform of a row with children change.
	wantRejected(t, "change a streamer's platform", fk, f.exec(`UPDATE streamers SET platform = 'other2' WHERE id = ?`, vod.StreamerID))
	wantRejected(t, "change a vod's platform", fk, f.exec(`UPDATE vods SET platform = 'other' WHERE id = ?`, vod.ID))
	wantRejected(t, "change a job's platform", fk, f.exec(`UPDATE jobs SET platform = 'other' WHERE id = ?`, job.ID))

	// A row that others refer to cannot be deleted.
	wantRejected(t, "delete streamer with vods", fk, f.exec(`DELETE FROM streamers WHERE id = ?`, vod.StreamerID))
	wantRejected(t, "delete vod with candidates", fk, f.exec(`DELETE FROM vods WHERE id = ?`, vod.ID))
	wantRejected(t, "delete candidate with a job", fk, f.exec(`DELETE FROM candidates WHERE id = ?`, cand.ID))
	wantRejected(t, "delete job with a clip", fk, f.exec(`DELETE FROM jobs WHERE id = ?`, job.ID))
	wantRejected(t, "delete clip with uploads", fk, f.exec(`DELETE FROM clips WHERE id = ?`, clip.ID))

	// Bottom-up it works.
	for _, step := range []struct {
		table string
		id    int64
	}{{"uploads", up.ID}, {"clips", clip.ID}, {"jobs", job.ID}, {"candidates", cand.ID}, {"vods", vod.ID}, {"streamers", vod.StreamerID}} {
		if err := f.exec(`DELETE FROM `+step.table+` WHERE id = ?`, step.id); err != nil {
			t.Errorf("delete from %s: %v", step.table, err)
		}
	}
}

// Every CHECK and NOT NULL of the schema: a valid row, one column changed to
// a value that must not be stored.
func TestChecks(t *testing.T) {
	const (
		check   = "CHECK constraint failed"
		notNull = "NOT NULL constraint failed"
		strict  = "cannot store"
	)
	f := newFixture(t)
	up := f.upload()
	clip, _ := GetClip(context.Background(), f.s.Reader(), up.ClipID)
	job, _ := GetJob(context.Background(), f.s.Reader(), clip.JobID)
	cand, _ := GetCandidate(context.Background(), f.s.Reader(), job.CandidateID)
	vod, _ := GetVOD(context.Background(), f.s.Reader(), cand.VODID)
	ids := map[string]int64{
		"streamers": vod.StreamerID, "vods": vod.ID, "candidates": cand.ID,
		"jobs": job.ID, "clips": clip.ID, "uploads": up.ID,
	}
	const ts = "'2026-03-04T05:06:07.890Z'"

	cases := []struct {
		table, set, want string
	}{
		{"streamers", `platform = ''`, check},
		{"streamers", `platform_user_id = ''`, check},
		{"streamers", `login = ''`, check},
		{"streamers", `enabled = 2`, check},
		{"streamers", `enabled = NULL`, notNull},
		{"streamers", `display_name = NULL`, notNull},
		{"streamers", `created_at = '2026-03-04T05:06:07Z'`, check},
		{"streamers", `updated_at = '2026-03-04 05:06:07.890'`, check},
		{"streamers", `created_at = NULL`, notNull},
		{"streamers", `enabled = 'yes'`, strict},

		{"vods", `platform_vod_id = ''`, check},
		{"vods", `streamer_id = NULL`, notNull},
		{"vods", `title = NULL`, notNull},
		{"vods", `duration_seconds = -1`, check},
		{"vods", `duration_seconds = 'long'`, strict},
		{"vods", `started_at = '2026-03-04T05:06:07Z'`, check},
		{"vods", `ended_at = '2026-03-04T05:06:07+00:00'`, check},
		{"vods", `ended_at = '2020-01-01T00:00:00.000Z'`, check}, // before started_at
		{"vods", `ended_at = NULL`, notNull},
		{"vods", `scan_status = 'done'`, check},
		{"vods", `scan_status = ''`, check},
		{"vods", `scan_status = NULL`, notNull},
		{"vods", `scan_attempts = -1`, check},
		{"vods", `scan_error = NULL`, notNull},
		{"vods", `next_scan_at = 'soon'`, check},
		{"vods", `updated_at = 'now'`, check},

		{"candidates", `start_seconds = -0.5`, check},
		{"candidates", `end_seconds = start_seconds`, check},
		{"candidates", `end_seconds = start_seconds - 1`, check},
		{"candidates", `score = NULL`, notNull},
		{"candidates", `score = 'high'`, strict},
		{"candidates", `signal = 'spike'`, check},
		{"candidates", `signal = NULL`, notNull},
		{"candidates", `status = 'kept'`, check},
		{"candidates", `created_at = '2026'`, check},

		{"jobs", `status = 'running'`, check},
		{"jobs", `status = NULL`, notNull},
		{"jobs", `attempts = -1`, check},
		{"jobs", `attempts = 'two'`, strict},
		{"jobs", `candidate_id = NULL`, notNull},
		{"jobs", `clipper = NULL`, notNull},
		{"jobs", `last_error = NULL`, notNull},
		{"jobs", `status = 'leased', clipper = 'c'`, check},                          // leased without an expiry
		{"jobs", `status = 'leased', clipper = '', lease_expires_at = ` + ts, check}, // leased by nobody
		{"jobs", `lease_expires_at = ` + ts, check},                                  // queued with a lease
		{"jobs", `status = 'done', lease_expires_at = ` + ts, check},                 // done with a lease
		{"jobs", `status = 'failed', lease_expires_at = ` + ts, check},               // failed with a lease
		{"jobs", `status = 'leased', clipper = 'c', lease_expires_at = '2026-03-04T05:06:07Z'`, check},
		{"jobs", `last_heartbeat_at = '12:00'`, check},

		{"clips", `job_id = NULL`, notNull},
		{"clips", `file_path = NULL`, notNull},
		{"clips", `sha256 = 'abc'`, check},
		{"clips", `sha256 = upper(sha256)`, check},
		{"clips", `sha256 = replace(sha256, '9', 'g')`, check},
		{"clips", `sha256 = sha256 || '0'`, check},
		{"clips", `size_bytes = -1`, check},
		{"clips", `size_bytes = 1.5`, strict},
		{"clips", `duration_seconds = 0`, check},
		{"clips", `width = 0`, check},
		{"clips", `height = -1920`, check},
		{"clips", `transcript_json = 'not json'`, check},
		{"clips", `transcript_json = ''`, check},
		{"clips", `transcript_json = NULL`, notNull},
		{"clips", `review_status = 'maybe'`, check},
		{"clips", `review_status = 'approved'`, check}, // decided without a time
		{"clips", `review_status = 'rejected'`, check}, // decided without a time
		{"clips", `reviewed_at = ` + ts, check},        // pending with a decision time
		{"clips", `telegram_chat_id = 5`, check},       // chat without message
		{"clips", `telegram_message_id = 5`, check},    // message without chat
		{"clips", `telegram_message_id = 'x', telegram_chat_id = 1`, strict},
		{"clips", `file_removed_at = 'yesterday'`, check},
		{"clips", `reviewed_at = '2026-03-04', review_status = 'approved'`, check},

		{"uploads", `target_platform = ''`, check},
		{"uploads", `target_platform = NULL`, notNull},
		{"uploads", `clip_id = NULL`, notNull},
		{"uploads", `status = 'posted'`, check},
		{"uploads", `status = 'done'`, check},   // done without a posted time
		{"uploads", `posted_at = ` + ts, check}, // pending with a posted time
		{"uploads", `status = 'uploading', posted_at = ` + ts, check},
		{"uploads", `status = 'failed', posted_at = ` + ts, check},
		{"uploads", `attempts = -1`, check},
		{"uploads", `external_post_id = NULL`, notNull},
		{"uploads", `last_error = NULL`, notNull},
		{"uploads", `next_attempt_at = '2026-03-04T05:06:07.89Z'`, check},
		{"uploads", `posted_at = 'x', status = 'done'`, check},
	}
	for _, tc := range cases {
		t.Run(tc.table+"/"+tc.set, func(t *testing.T) {
			err := f.exec(`UPDATE `+tc.table+` SET `+tc.set+` WHERE id = ?`, ids[tc.table])
			wantRejected(t, tc.set, tc.want, err)
		})
	}

	// None of the rejected changes left a trace, and the same statements
	// with good values pass, so the cases above fail for the reason given.
	good := []struct{ table, set string }{
		{"streamers", `enabled = 0`},
		{"vods", `next_scan_at = ` + ts + `, scan_status = 'failed', scan_attempts = 1`},
		{"candidates", `status = 'skipped', signal = 'both'`},
		{"jobs", `status = 'leased', clipper = 'c', lease_expires_at = ` + ts + `, last_heartbeat_at = ` + ts},
		{"clips", `review_status = 'approved', reviewed_at = ` + ts + `, telegram_chat_id = 5, telegram_message_id = 6`},
		{"uploads", `status = 'done', posted_at = ` + ts + `, next_attempt_at = ` + ts},
	}
	for _, g := range good {
		if err := f.exec(`UPDATE `+g.table+` SET `+g.set+` WHERE id = ?`, ids[g.table]); err != nil {
			t.Errorf("valid change %s on %s: %v", g.set, g.table, err)
		}
	}
}

// The Insert functions report a broken CHECK as an error too, and write
// nothing.
func TestInsertRejectsInvalidRows(t *testing.T) {
	const check = "CHECK constraint failed"
	f := newFixture(t)
	ctx := context.Background()
	w := f.s.Writer()
	job := f.job()
	cand, _ := GetCandidate(ctx, w, job.CandidateID)
	vod, _ := GetVOD(ctx, w, cand.VODID)

	_, err := InsertStreamer(ctx, w, Streamer{Platform: api.PlatformTwitch, Login: "x", DisplayName: "X"})
	wantRejected(t, "streamer without a platform user id", check, err)
	_, err = InsertVOD(ctx, w, VOD{Platform: vod.Platform, PlatformVODID: "bad", StreamerID: vod.StreamerID, StartedAt: t0, EndedAt: t0, ScanStatus: "queued"})
	wantRejected(t, "vod with an unknown scan status", check, err)
	_, err = InsertVOD(ctx, w, VOD{Platform: vod.Platform, PlatformVODID: "bad", StreamerID: vod.StreamerID, StartedAt: t0, EndedAt: t0.Add(-time.Second)})
	wantRejected(t, "vod that ends before it starts", check, err)
	_, err = InsertCandidate(ctx, w, Candidate{Platform: vod.Platform, VODID: vod.ID, StartSeconds: 50, EndSeconds: 40, Signal: SignalBoth})
	wantRejected(t, "candidate with an empty window", check, err)
	_, err = InsertCandidate(ctx, w, Candidate{Platform: vod.Platform, VODID: vod.ID, StartSeconds: 50, EndSeconds: 60})
	wantRejected(t, "candidate without a signal", check, err)
	cand2 := f.candidate()
	_, err = InsertJob(ctx, w, Job{Platform: cand2.Platform, CandidateID: cand2.ID, Status: JobLeased, Clipper: "c"})
	wantRejected(t, "leased job without an expiry", check, err)
	bad := clipFor(job)
	bad.SHA256 = "deadbeef"
	_, err = InsertClip(ctx, w, bad)
	wantRejected(t, "clip with a short checksum", check, err)
	bad = clipFor(job)
	bad.TranscriptJSON = "{"
	_, err = InsertClip(ctx, w, bad)
	wantRejected(t, "clip with a broken transcript", check, err)
	bad = clipFor(job)
	bad.TelegramMessageID = 9
	_, err = InsertClip(ctx, w, bad)
	wantRejected(t, "clip with a message id but no chat id", check, err)
	clip := f.clip()
	_, err = InsertUpload(ctx, w, Upload{ClipID: clip.ID, TargetPlatform: "youtube", Status: UploadDone})
	wantRejected(t, "done upload without a posted time", check, err)
	_, err = InsertUpload(ctx, w, Upload{ClipID: clip.ID})
	wantRejected(t, "upload without a target", check, err)

	for table, want := range map[string]int{"streamers": 3, "vods": 3, "candidates": 3, "jobs": 2, "clips": 1, "uploads": 0} {
		if got := count(t, f.s, table); got != want {
			t.Errorf("%s = %d rows, want %d", table, got, want)
		}
	}
}

// Rows written with explicit non-default values read back unchanged.
func TestInsertKeepsGivenValues(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.s.Writer()
	loc := time.FixedZone("plus4", 4*3600)
	created := time.Date(2026, 1, 2, 3, 4, 5, 123_456_789, loc)
	wantCreated := time.Date(2026, 1, 1, 23, 4, 5, 123_000_000, time.UTC)

	st, err := InsertStreamer(ctx, w, Streamer{Platform: "other", PlatformUserID: "9", Login: "l", DisplayName: "", Enabled: false, CreatedAt: created})
	if err != nil {
		t.Fatal(err)
	}
	if st.CreatedAt != wantCreated || st.UpdatedAt != wantCreated {
		t.Errorf("streamer times = %v, %v; want %v", st.CreatedAt, st.UpdatedAt, wantCreated)
	}
	if got, err := GetStreamer(ctx, w, st.ID); err != nil || got != st {
		t.Errorf("streamer = %+v, %v; want %+v", got, err, st)
	}

	vod, err := InsertVOD(ctx, w, VOD{
		Platform: "other", PlatformVODID: "v", StreamerID: st.ID, DurationSeconds: 0.5,
		StartedAt: created, EndedAt: created.Add(500 * time.Millisecond),
		ScanStatus: ScanFailed, ScanAttempts: 3, ScanError: "boom", NextScanAt: created.Add(time.Hour), CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetVOD(ctx, w, vod.ID); err != nil || got != vod {
		t.Errorf("vod = %+v, %v; want %+v", got, err, vod)
	}
	if vod.NextScanAt != wantCreated.Add(time.Hour) {
		t.Errorf("next scan = %v", vod.NextScanAt)
	}

	cand, err := InsertCandidate(ctx, w, Candidate{Platform: "other", VODID: vod.ID, StartSeconds: 0, EndSeconds: 0.25, Score: -1.5, Signal: SignalViewerClip, Status: CandidateSkipped})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetCandidate(ctx, w, cand.ID); err != nil || got != cand {
		t.Errorf("candidate = %+v, %v; want %+v", got, err, cand)
	}

	job, err := InsertJob(ctx, w, Job{
		Platform: "other", CandidateID: cand.ID, Status: JobLeased, Attempts: 2, Clipper: "c",
		LeaseExpiresAt: created.Add(time.Minute), LastHeartbeatAt: created, LastError: "earlier",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetJob(ctx, w, job.ID); err != nil || got != job {
		t.Errorf("job = %+v, %v; want %+v", got, err, job)
	}

	in := clipFor(job)
	in.ReviewStatus, in.ReviewedAt = ReviewRejected, created
	in.TelegramChatID, in.TelegramMessageID, in.FileRemovedAt = -5, 6, created.Add(time.Hour)
	clip, err := InsertClip(ctx, w, in)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetClip(ctx, w, clip.ID); err != nil || got != clip {
		t.Errorf("clip = %+v, %v; want %+v", got, err, clip)
	}

	up, err := InsertUpload(ctx, w, Upload{
		ClipID: clip.ID, TargetPlatform: "tiktok", Status: UploadDone, ExternalPostID: "p1", Attempts: 1,
		LastError: "earlier", NextAttemptAt: created, PostedAt: created.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetUpload(ctx, w, up.ID); err != nil || got != up {
		t.Errorf("upload = %+v, %v; want %+v", got, err, up)
	}
}

// The query functions take part in a transaction: a rollback undoes them.
func TestQueriesInTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	job := f.job()
	boom := errors.New("boom")

	err := f.s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := InsertClip(ctx, tx, clipFor(job)); err != nil {
			return err
		}
		job.Status = JobDone
		if err := UpdateJob(ctx, tx, job); err != nil {
			return err
		}
		if got, err := GetJob(ctx, tx, job.ID); err != nil || got.Status != JobDone {
			return fmt.Errorf("job inside the transaction = %+v, %v", got, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
	if got := count(t, f.s, "clips"); got != 0 {
		t.Errorf("clips after rollback = %d, want 0", got)
	}
	if got, err := GetJob(ctx, f.s.Reader(), job.ID); err != nil || got.Status != JobQueued {
		t.Errorf("job after rollback = %+v, %v", got, err)
	}

	err = f.s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := InsertClip(ctx, tx, clipFor(job)); err != nil {
			return err
		}
		return UpdateJob(ctx, tx, job)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetJob(ctx, f.s.Reader(), job.ID); err != nil || got.Status != JobDone {
		t.Errorf("job after commit = %+v, %v", got, err)
	}
	if _, err := GetClipByJob(ctx, f.s.Reader(), job.ID); err != nil {
		t.Errorf("clip after commit: %v", err)
	}
	// The read-only pool cannot write.
	if _, err := InsertStreamer(ctx, f.s.Reader(), Streamer{Platform: "p", PlatformUserID: "1", Login: "l"}); err == nil {
		t.Error("insert through the reader succeeded")
	}
}

// Expired leases are found by comparing stored times in SQL, down to the
// millisecond.
func TestLeaseExpiryComparesAsTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	whole := t0.Truncate(time.Second) // .000
	expiries := []time.Duration{-time.Hour, 0, 200 * time.Millisecond, 900 * time.Millisecond, time.Second, 24 * time.Hour}
	ids := map[int64]time.Duration{}
	for _, d := range expiries {
		j := f.job()
		j.Status, j.Clipper, j.LeaseExpiresAt = JobLeased, "c", whole.Add(d)
		if err := UpdateJob(ctx, f.s.Writer(), j); err != nil {
			t.Fatal(err)
		}
		ids[j.ID] = d
	}
	f.job() // queued: never expired

	for _, now := range []time.Duration{-2 * time.Hour, 0, 500 * time.Millisecond, 999 * time.Millisecond, time.Second, 48 * time.Hour} {
		rows, err := f.s.Reader().Query(`SELECT id FROM jobs WHERE status = ? AND lease_expires_at <= ?`,
			JobLeased, FormatTime(whole.Add(now)))
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got++
			if ids[id] > now {
				t.Errorf("at %v: job with expiry %v reported as expired", now, ids[id])
			}
		}
		rows.Close()
		want := 0
		for _, d := range expiries {
			if d <= now {
				want++
			}
		}
		if got != want {
			t.Errorf("at %v: %d expired jobs, want %d", now, got, want)
		}
	}
}

// The queries the later packages run use the indexes made for them.
func TestIndexesServeTheQueries(t *testing.T) {
	f := newFixture(t)
	f.upload()
	now := FormatTime(t0)
	cases := []struct {
		name, index, query string
		args               []any
	}{
		{"claim the oldest queued job", "jobs_status_created",
			`SELECT id FROM jobs WHERE status = ? ORDER BY created_at, id LIMIT 1`, []any{JobQueued}},
		{"expired leases", "jobs_status_lease",
			`SELECT id FROM jobs WHERE status = ? AND lease_expires_at <= ?`, []any{JobLeased, now}},
		{"job of a candidate", "sqlite_autoindex_jobs_1",
			`SELECT id FROM jobs WHERE candidate_id = ?`, []any{1}},
		{"vods to scan", "vods_scan",
			`SELECT id FROM vods WHERE scan_status = ? ORDER BY next_scan_at`, []any{ScanNew}},
		{"failed vods due for a retry", "vods_scan",
			`SELECT id FROM vods WHERE scan_status = ? AND next_scan_at <= ?`, []any{ScanFailed, now}},
		{"vods of a streamer", "vods_streamer",
			`SELECT id FROM vods WHERE streamer_id = ? ORDER BY started_at DESC`, []any{1}},
		{"vod by platform id", "sqlite_autoindex_vods_1",
			`SELECT id FROM vods WHERE platform = ? AND platform_vod_id = ?`, []any{"twitch", "v"}},
		{"streamer by login", "streamers_platform_login",
			`SELECT id FROM streamers WHERE platform = ? AND login = ?`, []any{"twitch", "l"}},
		{"candidates of a vod", "sqlite_autoindex_candidates_1",
			`SELECT id FROM candidates WHERE vod_id = ?`, []any{1}},
		{"clips awaiting review, not sent", "clips_review",
			`SELECT id FROM clips WHERE review_status = ? AND telegram_message_id IS NULL ORDER BY created_at`, []any{ReviewPending}},
		{"clip by review message", "sqlite_autoindex_clips_2",
			`SELECT id FROM clips WHERE telegram_chat_id = ? AND telegram_message_id = ?`, []any{1, 2}},
		{"rejected clips to clean up", "clips_cleanup",
			`SELECT id FROM clips WHERE review_status = ? AND file_removed_at IS NULL AND reviewed_at <= ?`, []any{ReviewRejected, now}},
		{"uploads due", "uploads_status_due",
			`SELECT id FROM uploads WHERE status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`, []any{UploadPending, now}},
		{"uploads of a clip", "sqlite_autoindex_uploads_1",
			`SELECT id FROM uploads WHERE clip_id = ?`, []any{1}},
		{"latest post on a target", "uploads_target_posted",
			`SELECT max(posted_at) FROM uploads WHERE target_platform = ?`, []any{"youtube"}},
		{"posts on a target since", "uploads_target_posted",
			`SELECT count(*) FROM uploads WHERE target_platform = ? AND posted_at >= ?`, []any{"youtube", now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := f.s.Reader().Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(plan, "; ")
			if !strings.Contains(got, tc.index) {
				t.Errorf("plan = %q, want it to use %s", got, tc.index)
			}
			if strings.Contains(got, "USE TEMP B-TREE") {
				t.Errorf("plan = %q sorts instead of reading the index in order", got)
			}
		})
	}
}
