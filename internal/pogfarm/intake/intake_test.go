package intake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

const secret = "intake-test-shared-secret"

// t0 is the fixed time of every test's clock.
var t0 = time.Date(2026, 11, 17, 14, 30, 0, 123456789, time.UTC)

// fixture is an intake and a queue on a fresh database holding one VOD, with
// the complete route served over HTTP.
type fixture struct {
	t       *testing.T
	store   *store.Store
	queue   *queue.Queue
	intake  *Intake
	dataDir string
	server  *httptest.Server
	vod     store.VOD
	next    float64
}

func newFixture(t *testing.T, maxClip int64) *fixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, store.Path(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	now := func() time.Time { return t0 }
	q, err := queue.New(st, queue.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	in, err := New(st, Options{DataDir: dataDir, MaxClipBytes: maxClip, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: st, queue: q, intake: in, dataDir: dataDir}

	streamer, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform: api.PlatformTwitch, PlatformUserID: "1001", Login: "somestreamer", DisplayName: "SomeStreamer", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.vod, err = store.InsertVOD(ctx, st.Writer(), store.VOD{
		Platform: api.PlatformTwitch, PlatformVODID: "2301234567", StreamerID: streamer.ID,
		DurationSeconds: 100000, StartedAt: t0.Add(-30 * time.Hour), EndedAt: t0.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle(api.RouteComplete, in)
	f.server = httptest.NewServer(api.RequireSecret(secret, mux))
	t.Cleanup(f.server.Close)
	return f
}

// enqueue queues a new job and returns its row id.
func (f *fixture) enqueue() int64 {
	f.t.Helper()
	f.next += 100
	c, err := store.InsertCandidate(context.Background(), f.store.Writer(), store.Candidate{
		Platform: api.PlatformTwitch, VODID: f.vod.ID, StartSeconds: f.next, EndSeconds: f.next + 45,
		Score: 1, Signal: store.SignalChatSpike,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	job, _, err := f.queue.Enqueue(context.Background(), c.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return job.ID
}

// leased queues a job and lets clipper claim it.
func (f *fixture) leased(clipper string) int64 {
	f.t.Helper()
	id := f.enqueue()
	job, err := f.queue.Claim(context.Background(), clipper)
	if err != nil {
		f.t.Fatal(err)
	}
	if job.ID != queue.JobID(id) {
		f.t.Fatalf("claimed job %s, want %d", job.ID, id)
	}
	return id
}

func (f *fixture) client(clipper string) *api.Client {
	f.t.Helper()
	c, err := api.NewClient(api.ClientOptions{
		BaseURL: f.server.URL, Secret: secret, Clipper: clipper, MaxAttempts: 1,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) job(id int64) store.Job {
	f.t.Helper()
	j, err := store.GetJob(context.Background(), f.store.Reader(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// clipCount returns the number of clip rows.
func (f *fixture) clipCount() int {
	f.t.Helper()
	var n int
	if err := f.store.Reader().QueryRow(`SELECT count(*) FROM clips`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// files returns the files below dir (relative to the data directory), with
// forward slashes.
func (f *fixture) files(dir string) []string {
	f.t.Helper()
	var out []string
	root := filepath.Join(f.dataDir, dir)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(f.dataDir, path)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// assertNothingStored checks that a rejected result left no row, no clip
// file and no temp file, and that the job is not done.
func (f *fixture) assertNothingStored(jobID int64) {
	f.t.Helper()
	if n := f.clipCount(); n != 0 {
		f.t.Errorf("%d clip rows, want 0", n)
	}
	if got := f.files(ClipsDir); len(got) != 0 {
		f.t.Errorf("clip files left behind: %q", got)
	}
	if got := f.files(TempDir); len(got) != 0 {
		f.t.Errorf("temp files left behind: %q", got)
	}
	if jobID != 0 {
		if j := f.job(jobID); j.Status == store.JobDone {
			f.t.Errorf("job %d is done", jobID)
		}
	}
}

// clipFile is a clip on the clipper's disk.
type clipFile struct {
	path string
	data []byte
	sum  string
}

func newClip(t *testing.T, size int) clipFile {
	t.Helper()
	data := make([]byte, size)
	r := rand.New(rand.NewPCG(uint64(size), rand.Uint64()))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return clipFile{path: path, data: data, sum: hex.EncodeToString(sum[:])}
}

// meta is the metadata the clipper sends for c.
func (c clipFile) meta() api.CompleteMetadata {
	return api.CompleteMetadata{
		Clipper: "set-by-client", SHA256: c.sum, SizeBytes: int64(len(c.data)),
		DurationSeconds: 42.5, Width: 1080, Height: 1920, ToolVersions: map[string]string{"ffmpeg": "7.1"},
	}
}

var transcript = api.Transcript{
	Language: "en",
	Words: []api.Word{
		{Text: "no", StartSeconds: 0.32, EndSeconds: 0.5},
		{Text: "way", StartSeconds: 0.5, EndSeconds: 0.91},
	},
}

func TestCompleteAccepted(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	clip := newClip(t, 300<<10)

	res, err := f.client("node-a").Complete(context.Background(), queue.JobID(jobID), clip.meta(), transcript, clip.path)
	if err != nil {
		t.Fatal(err)
	}
	if res != api.ResultAccepted {
		t.Fatalf("result = %q, want accepted", res)
	}

	row, err := store.GetClipByJob(context.Background(), f.store.Reader(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	want := ClipPath(jobID, t0)
	if want != fmt.Sprintf("clips/2026-11/job-%d.mp4", jobID) {
		t.Fatalf("ClipPath = %q", want)
	}
	if row.FilePath != want || row.SHA256 != clip.sum || row.SizeBytes != int64(len(clip.data)) ||
		row.DurationSeconds != 42.5 || row.Width != 1080 || row.Height != 1920 || row.Clipper != "node-a" ||
		row.ReviewStatus != store.ReviewPending || !row.ReviewedAt.IsZero() || row.TelegramMessageID != 0 ||
		row.Platform != api.PlatformTwitch || !row.CreatedAt.Equal(t0.Truncate(time.Millisecond)) {
		t.Errorf("clip row = %+v", row)
	}
	var got api.Transcript
	if err := json.Unmarshal([]byte(row.TranscriptJSON), &got); err != nil {
		t.Fatalf("stored transcript is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, transcript) {
		t.Errorf("stored transcript = %+v, want %+v", got, transcript)
	}
	var valid int
	if err := f.store.Reader().QueryRow(`SELECT json_valid(transcript_json) FROM clips WHERE job_id = ?`, jobID).Scan(&valid); err != nil || valid != 1 {
		t.Errorf("json_valid = %d, %v", valid, err)
	}

	data, err := os.ReadFile(filepath.Join(f.dataDir, filepath.FromSlash(row.FilePath)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, clip.data) {
		t.Error("stored file differs from the upload")
	}

	job := f.job(jobID)
	if job.Status != store.JobDone || !job.LeaseExpiresAt.IsZero() {
		t.Errorf("job = %+v, want done without a lease", job)
	}
	if got := f.files(TempDir); len(got) != 0 {
		t.Errorf("temp files left behind: %q", got)
	}
	if got := f.files(ClipsDir); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("clip files = %q, want %q", got, want)
	}
}

func TestCompleteEmptyTranscript(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	clip := newClip(t, 1000)
	// The client turns nil words into [], as the contract requires.
	res, err := f.client("node-a").Complete(context.Background(), queue.JobID(jobID), clip.meta(), api.Transcript{}, clip.path)
	if err != nil || res != api.ResultAccepted {
		t.Fatalf("Complete = %q, %v", res, err)
	}
	row, err := store.GetClipByJob(context.Background(), f.store.Reader(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TranscriptJSON != `{"language":"","words":[]}` {
		t.Errorf("transcript = %s", row.TranscriptJSON)
	}
}

// Holding the lease is not required: the first result is accepted whoever
// sends it and whatever state the job is in.
func TestCompleteWithoutLease(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture) int64
	}{
		{"held by another clipper", func(f *fixture) int64 { return f.leased("node-b") }},
		{"queued again", func(f *fixture) int64 {
			id := f.leased("node-a")
			if _, err := f.queue.Fail(context.Background(), id, "node-a", "network", true); err != nil {
				f.t.Fatal(err)
			}
			return id
		}},
		{"failed", func(f *fixture) int64 {
			id := f.leased("node-a")
			if _, err := f.queue.Fail(context.Background(), id, "node-a", "gone", false); err != nil {
				f.t.Fatal(err)
			}
			return id
		}},
		{"never claimed", func(f *fixture) int64 { return f.enqueue() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, 1<<20)
			jobID := tt.setup(f)
			clip := newClip(t, 2000)
			res, err := f.client("node-a").Complete(context.Background(), queue.JobID(jobID), clip.meta(), transcript, clip.path)
			if err != nil || res != api.ResultAccepted {
				t.Fatalf("Complete = %q, %v; want accepted", res, err)
			}
			if j := f.job(jobID); j.Status != store.JobDone || !j.LeaseExpiresAt.IsZero() {
				t.Errorf("job = %+v, want done", j)
			}
			if n := f.clipCount(); n != 1 {
				t.Errorf("%d clip rows, want 1", n)
			}
		})
	}
}

func TestCompleteRejected(t *testing.T) {
	tests := []struct {
		name    string
		maxClip int64
		size    int
		meta    func(m *api.CompleteMetadata)
		want    error
	}{
		{name: "checksum mismatch", size: 5000, want: api.ErrChecksumMismatch,
			meta: func(m *api.CompleteMetadata) { m.SHA256 = strings.Repeat("ab", 32) }},
		{name: "size larger than sent", size: 5000, want: api.ErrInvalidRequest,
			meta: func(m *api.CompleteMetadata) { m.SizeBytes++ }},
		{name: "size smaller than sent", size: 5000, want: api.ErrInvalidRequest,
			meta: func(m *api.CompleteMetadata) { m.SizeBytes-- }},
		{name: "declared size over the limit", maxClip: 4096, size: 5000, want: api.ErrPayloadTooLarge},
		{name: "clip over the limit, size understated", maxClip: 4096, size: 5000, want: api.ErrPayloadTooLarge,
			meta: func(m *api.CompleteMetadata) { m.SizeBytes = 100 }},
		{name: "sha256 not hex", size: 100, want: api.ErrInvalidRequest,
			meta: func(m *api.CompleteMetadata) { m.SHA256 = strings.ToUpper(m.SHA256) }},
		{name: "zero duration", size: 100, want: api.ErrInvalidRequest,
			meta: func(m *api.CompleteMetadata) { m.DurationSeconds = 0 }},
		{name: "zero width", size: 100, want: api.ErrInvalidRequest,
			meta: func(m *api.CompleteMetadata) { m.Width = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxClip := tt.maxClip
			if maxClip == 0 {
				maxClip = 1 << 20
			}
			f := newFixture(t, maxClip)
			jobID := f.leased("node-a")
			clip := newClip(t, tt.size)
			meta := clip.meta()
			if tt.meta != nil {
				tt.meta(&meta)
			}
			_, err := f.client("node-a").Complete(context.Background(), queue.JobID(jobID), meta, transcript, clip.path)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Complete error = %v, want %v", err, tt.want)
			}
			f.assertNothingStored(jobID)
			if j := f.job(jobID); j.Status != store.JobLeased || j.Clipper != "node-a" {
				t.Errorf("job = %+v, want still leased to node-a", j)
			}
		})
	}
}

func TestCompleteAtTheLimit(t *testing.T) {
	f := newFixture(t, 4096)
	jobID := f.leased("node-a")
	clip := newClip(t, 4096)
	res, err := f.client("node-a").Complete(context.Background(), queue.JobID(jobID), clip.meta(), transcript, clip.path)
	if err != nil || res != api.ResultAccepted {
		t.Fatalf("Complete = %q, %v; want accepted", res, err)
	}
}

func TestCompleteUnknownJob(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.leased("node-a")
	clip := newClip(t, 1000)
	for _, id := range []string{"999", "abc", "0", "-1", "01"} {
		_, err := f.client("node-a").Complete(context.Background(), id, clip.meta(), transcript, clip.path)
		if !errors.Is(err, api.ErrUnknownJob) {
			t.Errorf("job %q: error = %v, want ErrUnknownJob", id, err)
		}
	}
	f.assertNothingStored(0)
}

func TestCompleteSecondResult(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	first := newClip(t, 3000)
	ctx := context.Background()

	res, err := f.client("node-a").Complete(ctx, queue.JobID(jobID), first.meta(), transcript, first.path)
	if err != nil || res != api.ResultAccepted {
		t.Fatalf("first Complete = %q, %v", res, err)
	}

	// An identical retry of the same upload, and a different result from
	// another clipper that redid the job, are both discarded.
	second := newClip(t, 4000)
	for _, try := range []struct {
		clipper string
		clip    clipFile
	}{{"node-a", first}, {"node-b", second}} {
		res, err := f.client(try.clipper).Complete(ctx, queue.JobID(jobID), try.clip.meta(), transcript, try.clip.path)
		if err != nil || res != api.ResultAlreadyCompleted {
			t.Fatalf("%s: Complete = %q, %v; want already_completed", try.clipper, res, err)
		}
	}

	if n := f.clipCount(); n != 1 {
		t.Fatalf("%d clip rows, want 1", n)
	}
	row, err := store.GetClipByJob(ctx, f.store.Reader(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Clipper != "node-a" || row.SHA256 != first.sum {
		t.Errorf("clip row = %+v, want the first result", row)
	}
	data, err := os.ReadFile(filepath.Join(f.dataDir, filepath.FromSlash(row.FilePath)))
	if err != nil || !bytes.Equal(data, first.data) {
		t.Errorf("stored file is not the first result (err %v)", err)
	}
	if got := f.files(ClipsDir); len(got) != 1 {
		t.Errorf("clip files = %q, want one", got)
	}
	if got := f.files(TempDir); len(got) != 0 {
		t.Errorf("temp files left behind: %q", got)
	}
}

func TestCompleteConcurrentResults(t *testing.T) {
	f := newFixture(t, 1<<20)
	const jobs = 8
	clippers := []string{"node-a", "node-b"}
	ctx := context.Background()

	for range jobs {
		// node-a lost the lease, the job went back to the queue and node-b
		// claimed it; both finish and upload at the same time.
		jobID := f.leased("node-a")
		if _, err := f.queue.Fail(ctx, jobID, "node-a", "lease expired", true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.queue.Claim(ctx, "node-b"); err != nil {
			t.Fatal(err)
		}

		clips := []clipFile{newClip(t, 50<<10), newClip(t, 60<<10)}
		results := make([]api.CompleteResult, len(clippers))
		errs := make([]error, len(clippers))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, name := range clippers {
			client := f.client(name)
			wg.Go(func() {
				<-start
				results[i], errs[i] = client.Complete(ctx, queue.JobID(jobID), clips[i].meta(), transcript, clips[i].path)
			})
		}
		close(start)
		wg.Wait()

		winner := -1
		for i := range clippers {
			if errs[i] != nil {
				t.Fatalf("job %d, %s: %v", jobID, clippers[i], errs[i])
			}
			if results[i] == api.ResultAccepted {
				if winner >= 0 {
					t.Fatalf("job %d: both results accepted", jobID)
				}
				winner = i
			} else if results[i] != api.ResultAlreadyCompleted {
				t.Fatalf("job %d, %s: result %q", jobID, clippers[i], results[i])
			}
		}
		if winner < 0 {
			t.Fatalf("job %d: no result accepted", jobID)
		}
		row, err := store.GetClipByJob(ctx, f.store.Reader(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Clipper != clippers[winner] || row.SHA256 != clips[winner].sum {
			t.Errorf("job %d: row from %s, accepted was %s", jobID, row.Clipper, clippers[winner])
		}
		data, err := os.ReadFile(filepath.Join(f.dataDir, filepath.FromSlash(row.FilePath)))
		if err != nil || !bytes.Equal(data, clips[winner].data) {
			t.Errorf("job %d: stored file is not the accepted result (err %v)", jobID, err)
		}
	}
	if n := f.clipCount(); n != jobs {
		t.Errorf("%d clip rows, want %d", n, jobs)
	}
	if got := f.files(ClipsDir); len(got) != jobs {
		t.Errorf("%d clip files, want %d: %q", len(got), jobs, got)
	}
	if got := f.files(TempDir); len(got) != 0 {
		t.Errorf("temp files left behind: %q", got)
	}
}

// A crash between the rename and the commit leaves an orphan file and no
// row; the next result for the job replaces it and is recorded.
func TestCompleteReplacesOrphan(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	orphan := filepath.Join(f.dataDir, filepath.FromSlash(ClipPath(jobID, t0)))
	if err := os.MkdirAll(filepath.Dir(orphan), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("left by a crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	clip := newClip(t, 2000)
	res, err := f.client("node-a").Complete(context.Background(), queue.JobID(jobID), clip.meta(), transcript, clip.path)
	if err != nil || res != api.ResultAccepted {
		t.Fatalf("Complete = %q, %v", res, err)
	}
	data, err := os.ReadFile(orphan)
	if err != nil || !bytes.Equal(data, clip.data) {
		t.Errorf("orphan not replaced by the accepted clip (err %v)", err)
	}
}

// When the transaction does not commit, the file already moved into place
// is removed again, so neither a row nor a file remains.
func TestCompleteCommitFails(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	clip := newClip(t, 2000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dst := filepath.Join(f.dataDir, filepath.FromSlash(ClipPath(jobID, t0)))
	f.intake.beforeCommit = func() {
		// The file is in place and the database does not know it yet: what
		// a crash at this point leaves behind.
		if _, err := os.Stat(dst); err != nil {
			t.Errorf("clip not in place before commit: %v", err)
		}
		if n := f.clipCount(); n != 0 {
			t.Errorf("%d clip rows before commit", n)
		}
		cancel()
	}
	meta := clip.meta()
	meta.Clipper = "node-a"
	_, err := f.intake.Complete(ctx, jobID, meta, transcript, bytes.NewReader(clip.data))
	if err == nil || Code(err) != api.CodeInternal {
		t.Fatalf("Complete error = %v, want an internal error", err)
	}
	f.assertNothingStored(jobID)
}

// failingReader returns its data and then an error.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestCompleteBrokenReader(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	clip := newClip(t, 2000)
	meta := clip.meta()
	_, err := f.intake.Complete(context.Background(), jobID, meta, transcript,
		&failingReader{data: clip.data[:1000], err: io.ErrUnexpectedEOF})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	f.assertNothingStored(jobID)
}

func TestNew(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, store.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, opts := range []Options{{MaxClipBytes: 1}, {DataDir: dir}, {DataDir: dir, MaxClipBytes: -1}} {
		if _, err := New(st, opts); err == nil {
			t.Errorf("New(%+v) succeeded", opts)
		}
	}
	if _, err := New(nil, Options{DataDir: dir, MaxClipBytes: 1}); err == nil {
		t.Error("New(nil store) succeeded")
	}
	in, err := New(st, Options{DataDir: dir, MaxClipBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if in.MaxClipBytes() != 10 {
		t.Errorf("MaxClipBytes = %d", in.MaxClipBytes())
	}
	for _, sub := range []string{ClipsDir, TempDir} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Errorf("%s not created: %v", sub, err)
		}
	}
}
