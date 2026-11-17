package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/intake"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

const (
	secret      = "server-test-shared-secret"
	wrongSecret = "server-test-wrong-secret!"
	lease       = 2 * time.Minute
	retryAfter  = 7 * time.Second
	maxClip     = 1 << 20
)

var t0 = time.Date(2026, 11, 17, 14, 30, 0, 0, time.UTC)

// clock is a clock the test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

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

// syncBuffer is a log destination that is safe for concurrent writes.
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

// fixture is a server on a fresh database holding one VOD.
type fixture struct {
	t       *testing.T
	dataDir string
	store   *store.Store
	queue   *queue.Queue
	clock   *clock
	server  *Server
	log     *syncBuffer
	vod     store.VOD
	next    float64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, dataDir: t.TempDir(), clock: &clock{now: t0}, log: &syncBuffer{}}
	st, err := store.Open(ctx, store.Path(f.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.store = st
	if f.queue, err = queue.New(st, queue.Options{LeaseDuration: lease, MaxAttempts: 3, Now: f.clock.Now}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(f.log, nil))
	in, err := intake.New(st, intake.Options{DataDir: f.dataDir, MaxClipBytes: maxClip, Now: f.clock.Now, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	f.server, err = New(Options{
		Secret: secret, Store: st, Queue: f.queue, Intake: in, Logger: logger, RetryAfter: retryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}

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
	return f
}

// serve serves the handler over HTTP for the rest of the test.
func (f *fixture) serve() *httptest.Server {
	f.t.Helper()
	ts := httptest.NewServer(f.server.Handler())
	f.t.Cleanup(ts.Close)
	return ts
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

// leased queues a job and lets clipper claim it directly on the queue.
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

func (f *fixture) job(id int64) store.Job {
	f.t.Helper()
	j, err := store.GetJob(context.Background(), f.store.Reader(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func newClient(t *testing.T, baseURL, secret, clipper string) *api.Client {
	t.Helper()
	c, err := api.NewClient(api.ClientOptions{BaseURL: baseURL, Secret: secret, Clipper: clipper, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// clipFile writes a clip of n bytes and returns its path and metadata.
func clipFile(t *testing.T, n int) (string, api.CompleteMetadata) {
	t.Helper()
	data := bytes.Repeat([]byte("pogfarm-clip "), n/13+1)[:n]
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, api.CompleteMetadata{
		SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(n), DurationSeconds: 44.5, Width: 1080, Height: 1920,
	}
}

var transcript = api.Transcript{Language: "en", Words: []api.Word{{Text: "pog", StartSeconds: 0.5, EndSeconds: 0.9}}}

func TestNew(t *testing.T) {
	f := newFixture(t)
	in, err := intake.New(f.store, intake.Options{DataDir: f.dataDir, MaxClipBytes: maxClip})
	if err != nil {
		t.Fatal(err)
	}
	valid := Options{Secret: secret, Store: f.store, Queue: f.queue, Intake: in}
	for name, mutate := range map[string]func(*Options){
		"no secret":        func(o *Options) { o.Secret = "" },
		"no store":         func(o *Options) { o.Store = nil },
		"no queue":         func(o *Options) { o.Queue = nil },
		"no intake":        func(o *Options) { o.Intake = nil },
		"negative timeout": func(o *Options) { o.ShutdownTimeout = -time.Second },
	} {
		opts := valid
		mutate(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	s, err := New(valid)
	if err != nil {
		t.Fatal(err)
	}
	if s.retryAfter != int(DefaultRetryAfter/time.Second) || s.uploadTimeout != DefaultUploadTimeout ||
		s.shutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("defaults not applied: retry after %ds, upload timeout %s, shutdown timeout %s",
			s.retryAfter, s.uploadTimeout, s.shutdownTimeout)
	}
	valid.RetryAfter = 1500 * time.Millisecond
	if s, err = New(valid); err != nil {
		t.Fatal(err)
	}
	if s.retryAfter != 2 {
		t.Errorf("retry after 1.5s: got %d seconds, want 2", s.retryAfter)
	}
}

func TestClaimWithNoJob(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	_, err := newClient(t, ts.URL, secret, "node-a").Claim(context.Background())
	var noJob *api.NoJobError
	if !errors.As(err, &noJob) || !errors.Is(err, api.ErrNoJob) {
		t.Fatalf("got %v, want a NoJobError", err)
	}
	if noJob.RetryAfter != retryAfter {
		t.Errorf("retry after %v, want %v", noJob.RetryAfter, retryAfter)
	}

	// The wire form is exactly the contract's.
	status, body := post(t, ts.URL+api.PathClaim, secret, `{"clipper":"node-a"}`)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"job":null,"retry_after_seconds":7}` {
		t.Errorf("got %d %s", status, body)
	}
}

// TestJobLifecycle drives claim, heartbeat and complete through the typed
// client against the real handlers.
func TestJobLifecycle(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	ctx := context.Background()
	c := newClient(t, ts.URL, secret, "node-a")
	id := f.enqueue()

	job, err := c.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := api.Job{
		ID: queue.JobID(id), Attempt: 1, Platform: api.PlatformTwitch, VODID: "2301234567",
		StartSeconds: 100, EndSeconds: 145,
		Streamer:       api.Streamer{Login: "somestreamer", DisplayName: "SomeStreamer"},
		LeaseExpiresAt: t0.Add(lease), HeartbeatIntervalSeconds: int(lease / 4 / time.Second),
	}
	if !job.LeaseExpiresAt.Equal(want.LeaseExpiresAt) {
		t.Errorf("lease expires at %v, want %v", job.LeaseExpiresAt, want.LeaseExpiresAt)
	}
	job.LeaseExpiresAt = want.LeaseExpiresAt
	if job != want {
		t.Errorf("claimed\n%+v\nwant\n%+v", job, want)
	}

	// A heartbeat later on moves the lease forward.
	f.clock.Advance(30 * time.Second)
	expires, err := c.Heartbeat(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wantExp := t0.Add(30*time.Second + lease); !expires.Equal(wantExp) {
		t.Errorf("heartbeat: lease expires at %v, want %v", expires, wantExp)
	}
	if got := f.job(id).LeaseExpiresAt; !got.Equal(expires) {
		t.Errorf("stored lease %v, want %v", got, expires)
	}

	// Complete uploads the clip and finishes the job.
	clip, meta := clipFile(t, 64<<10)
	res, err := c.Complete(ctx, job.ID, meta, transcript, clip)
	if err != nil {
		t.Fatal(err)
	}
	if res != api.ResultAccepted {
		t.Errorf("complete: %q, want accepted", res)
	}
	if st := f.job(id).Status; st != store.JobDone {
		t.Errorf("job status %s, want done", st)
	}
	stored, err := store.GetClipByJob(ctx, f.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.dataDir, stored.FilePath))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != meta.SHA256 || stored.Clipper != "node-a" {
		t.Errorf("stored clip %+v does not match the upload", stored)
	}

	// After the result, a second upload is discarded, and heartbeat and
	// fail are told the job is done.
	if res, err := newClient(t, ts.URL, secret, "node-b").Complete(ctx, job.ID, meta, transcript, clip); err != nil || res != api.ResultAlreadyCompleted {
		t.Errorf("second complete: %q, %v", res, err)
	}
	if _, err := c.Heartbeat(ctx, job.ID); !errors.Is(err, api.ErrAlreadyCompleted) {
		t.Errorf("heartbeat after complete: %v", err)
	}
	if err := c.Fail(ctx, job.ID, "too late", true); !errors.Is(err, api.ErrAlreadyCompleted) {
		t.Errorf("fail after complete: %v", err)
	}
}

func TestFailRequeuesAndFails(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	ctx := context.Background()
	c := newClient(t, ts.URL, secret, "node-a")
	id := f.enqueue()

	job, err := c.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Fail(ctx, job.ID, "network error", true); err != nil {
		t.Fatal(err)
	}
	if j := f.job(id); j.Status != store.JobQueued || j.LastError != "network error" {
		t.Errorf("after retryable fail: %+v", j)
	}

	// The job comes back with the next attempt, and a final failure ends it.
	job, err = c.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != queue.JobID(id) || job.Attempt != 2 {
		t.Errorf("claimed again: %+v", job)
	}
	if err := c.Fail(ctx, job.ID, "VOD was deleted", false); err != nil {
		t.Fatal(err)
	}
	if j := f.job(id); j.Status != store.JobFailed {
		t.Errorf("after final fail: %+v", j)
	}

	// The wire form of a recorded fail is 204 without a body.
	id = f.leased("node-a")
	status, body := post(t, ts.URL+api.PathFail(queue.JobID(id)), secret,
		`{"clipper":"node-a","reason":"shutdown","retryable":true}`)
	if status != http.StatusNoContent || body != "" {
		t.Errorf("fail: got %d %q, want 204 and no body", status, body)
	}
}

// post sends body to url with the secret and returns the status and body.
func post(t *testing.T, url, secret, body string) (int, string) {
	t.Helper()
	return send(t, http.MethodPost, url, secret, "application/json", strings.NewReader(body))
}

func send(t *testing.T, method, url, secret, contentType string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

// errorBody decodes a contract error body, failing the test if it is not
// one.
func errorBody(t *testing.T, body string) api.ErrorBody {
	t.Helper()
	var resp api.ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Error.Code == "" {
		t.Fatalf("not an error body: %q", body)
	}
	return resp.Error
}

// multipartBody builds a complete request with the given parts, in order.
func multipartBody(t *testing.T, parts ...[2]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		pw, err := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="` + p[0] + `"`}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(pw, p[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestErrorCodes checks every error of docs/api.md that the server itself
// produces: its status, its code and that it has the contract's body.
func TestErrorCodes(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	ctx := context.Background()

	held := queue.JobID(f.leased("node-a"))
	done := queue.JobID(f.leased("node-a"))
	clip, meta := clipFile(t, 1000)
	meta.Clipper = "node-a"
	if _, err := newClient(t, ts.URL, secret, "node-a").Complete(ctx, done, meta, transcript, clip); err != nil {
		t.Fatal(err)
	}
	queued := queue.JobID(f.enqueue())

	tooLarge := `{"clipper":"node-a","reason":"` + strings.Repeat("x", MaxJSONBodyBytes) + `"}`
	clipBytes := strings.Repeat("pogfarm-clip ", 1000/13+1)[:1000]
	completeOK := func(clip string) (*bytes.Buffer, string) {
		return multipartBody(t, [2]string{api.PartMetadata, mustJSON(t, meta)},
			[2]string{api.PartTranscript, mustJSON(t, transcript)}, [2]string{api.PartClip, clip})
	}

	type tc struct {
		name, path, body string
		code             api.ErrorCode
	}
	for _, c := range []tc{
		{"claim malformed", api.PathClaim, `{"clipper":`, api.CodeInvalidRequest},
		{"claim empty body", api.PathClaim, ``, api.CodeInvalidRequest},
		{"claim wrong type", api.PathClaim, `{"clipper":42}`, api.CodeInvalidRequest},
		{"claim no clipper", api.PathClaim, `{}`, api.CodeInvalidRequest},
		{"claim too large", api.PathClaim, tooLarge, api.CodeInvalidRequest},

		{"heartbeat unknown job", api.PathHeartbeat("999"), `{"clipper":"node-a"}`, api.CodeUnknownJob},
		{"heartbeat malformed id", api.PathHeartbeat("abc"), `{"clipper":"node-a"}`, api.CodeUnknownJob},
		{"heartbeat padded id", api.PathHeartbeat("01"), `{"clipper":"node-a"}`, api.CodeUnknownJob},
		{"heartbeat malformed", api.PathHeartbeat(held), `[`, api.CodeInvalidRequest},
		{"heartbeat no clipper", api.PathHeartbeat(held), `{"clipper":""}`, api.CodeInvalidRequest},
		{"heartbeat too large", api.PathHeartbeat(held), tooLarge, api.CodeInvalidRequest},
		{"heartbeat other holder", api.PathHeartbeat(held), `{"clipper":"node-b"}`, api.CodeLeaseLost},
		{"heartbeat queued job", api.PathHeartbeat(queued), `{"clipper":"node-a"}`, api.CodeLeaseLost},
		{"heartbeat done job", api.PathHeartbeat(done), `{"clipper":"node-a"}`, api.CodeAlreadyCompleted},

		{"fail unknown job", api.PathFail("999"), `{"clipper":"node-a"}`, api.CodeUnknownJob},
		{"fail malformed", api.PathFail(held), `nope`, api.CodeInvalidRequest},
		{"fail no clipper", api.PathFail(held), `{"reason":"x"}`, api.CodeInvalidRequest},
		{"fail too large", api.PathFail(held), tooLarge, api.CodeInvalidRequest},
		{"fail other holder", api.PathFail(held), `{"clipper":"node-b","retryable":true}`, api.CodeLeaseLost},
		{"fail done job", api.PathFail(done), `{"clipper":"node-a"}`, api.CodeAlreadyCompleted},
	} {
		status, body := post(t, ts.URL+c.path, secret, c.body)
		if status != c.code.HTTPStatus() {
			t.Errorf("%s: status %d, want %d (%s)", c.name, status, c.code.HTTPStatus(), body)
			continue
		}
		if got := errorBody(t, body); got.Code != c.code || got.Message == "" {
			t.Errorf("%s: error %+v, want code %s and a message", c.name, got, c.code)
		}
	}

	// The complete route answers through the result intake.
	wrongSum := strings.Repeat("y", len(clipBytes))
	for _, c := range []struct {
		name string
		id   string
		body func() (*bytes.Buffer, string)
		code api.ErrorCode
	}{
		{"complete unknown job", "999", func() (*bytes.Buffer, string) { return completeOK(clipBytes) }, api.CodeUnknownJob},
		{"complete missing parts", held, func() (*bytes.Buffer, string) {
			return multipartBody(t, [2]string{api.PartMetadata, mustJSON(t, meta)})
		}, api.CodeInvalidRequest},
		{"complete not multipart", held, func() (*bytes.Buffer, string) {
			return bytes.NewBufferString(`{}`), "application/json"
		}, api.CodeInvalidRequest},
		{"complete too large", held, func() (*bytes.Buffer, string) {
			m := meta
			m.SizeBytes = maxClip + 1
			return multipartBody(t, [2]string{api.PartMetadata, mustJSON(t, m)},
				[2]string{api.PartTranscript, mustJSON(t, transcript)}, [2]string{api.PartClip, clipBytes})
		}, api.CodePayloadTooLarge},
		{"complete checksum mismatch", held, func() (*bytes.Buffer, string) { return completeOK(wrongSum) }, api.CodeChecksumMismatch},
	} {
		body, contentType := c.body()
		status, resp := send(t, http.MethodPost, ts.URL+api.PathComplete(c.id), secret, contentType, body)
		if status != c.code.HTTPStatus() {
			t.Errorf("%s: status %d, want %d (%s)", c.name, status, c.code.HTTPStatus(), resp)
			continue
		}
		if got := errorBody(t, resp); got.Code != c.code {
			t.Errorf("%s: code %s, want %s", c.name, got.Code, c.code)
		}
	}

	// None of the rejected calls changed the jobs.
	if j := f.job(mustParse(t, held)); j.Status != store.JobLeased || j.Clipper != "node-a" {
		t.Errorf("held job changed: %+v", j)
	}
	if j := f.job(mustParse(t, queued)); j.Status != store.JobQueued {
		t.Errorf("queued job changed: %+v", j)
	}
}

func mustParse(t *testing.T, id string) int64 {
	t.Helper()
	n, err := queue.ParseJobID(id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTypedClientErrors checks that the client sees the server's answers as
// the errors the contract names.
func TestTypedClientErrors(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	ctx := context.Background()
	held := queue.JobID(f.leased("node-a"))
	other := newClient(t, ts.URL, secret, "node-b")

	if _, err := other.Heartbeat(ctx, "999"); !errors.Is(err, api.ErrUnknownJob) {
		t.Errorf("heartbeat unknown job: %v", err)
	}
	if _, err := other.Heartbeat(ctx, held); !errors.Is(err, api.ErrLeaseLost) {
		t.Errorf("heartbeat by another clipper: %v", err)
	}
	if err := other.Fail(ctx, held, "x", true); !errors.Is(err, api.ErrLeaseLost) {
		t.Errorf("fail by another clipper: %v", err)
	}
	clip, meta := clipFile(t, 1000)
	meta.SHA256 = strings.Repeat("0", 64)
	if _, err := other.Complete(ctx, held, meta, transcript, clip); !errors.Is(err, api.ErrChecksumMismatch) {
		t.Errorf("complete with a wrong checksum: %v", err)
	}
	meta.SizeBytes = maxClip + 1
	if _, err := other.Complete(ctx, held, meta, transcript, clip); !errors.Is(err, api.ErrPayloadTooLarge) {
		t.Errorf("complete too large: %v", err)
	}
}

// TestInternalErrorsDoNotLeak closes the database under the server: every
// operation answers 500 internal with a fixed message, and the cause is
// logged instead.
func TestInternalErrorsDoNotLeak(t *testing.T) {
	f := newFixture(t)
	held := queue.JobID(f.leased("node-a"))
	ts := f.serve()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{api.PathClaim, api.PathHeartbeat(held), api.PathFail(held)} {
		status, body := post(t, ts.URL+path, secret, `{"clipper":"node-a"}`)
		if status != http.StatusInternalServerError {
			t.Errorf("%s: status %d (%s)", path, status, body)
			continue
		}
		if got := errorBody(t, body); got.Code != api.CodeInternal || got.Message != internalMessage {
			t.Errorf("%s: error %+v", path, got)
		}
	}
	_, err := newClient(t, ts.URL, secret, "node-a").Claim(context.Background())
	if !errors.Is(err, api.ErrInternal) {
		t.Errorf("client claim: %v", err)
	}
	ts.Close()
	if out := f.log.String(); !strings.Contains(out, "database is closed") {
		t.Errorf("the cause is not logged:\n%s", out)
	}
}

// TestSecretRequired sends every /v1 route, and an unknown one, with a
// wrong secret and without one: all are 401, and nothing is changed.
func TestSecretRequired(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	ctx := context.Background()
	held := queue.JobID(f.leased("node-a"))
	queued := f.enqueue()

	c := newClient(t, ts.URL, wrongSecret, "node-a")
	if _, err := c.Claim(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("claim: %v", err)
	}
	if _, err := c.Heartbeat(ctx, held); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("heartbeat: %v", err)
	}
	clip, meta := clipFile(t, 1000)
	if _, err := c.Complete(ctx, held, meta, transcript, clip); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("complete: %v", err)
	}
	if err := c.Fail(ctx, held, "x", true); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("fail: %v", err)
	}

	paths := []string{api.PathClaim, api.PathHeartbeat(held), api.PathComplete(held), api.PathFail(held),
		api.PathPrefix + "/no/such/route", api.PathPrefix + "/"}
	for _, key := range []string{"", wrongSecret} {
		for _, path := range paths {
			status, body := post(t, ts.URL+path, key, `{"clipper":"node-a"}`)
			if status != http.StatusUnauthorized || errorBody(t, body).Code != api.CodeUnauthenticated {
				t.Errorf("%s with secret %q: %d %s", path, key, status, body)
			}
		}
	}

	if j := f.job(mustParse(t, held)); j.Status != store.JobLeased || !j.LeaseExpiresAt.Equal(t0.Add(lease)) {
		t.Errorf("held job changed: %+v", j)
	}
	if j := f.job(queued); j.Status != store.JobQueued {
		t.Errorf("queued job changed: %+v", j)
	}
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		status, body := send(t, method, ts.URL+PathHealth, "", "", nil)
		if status != http.StatusOK {
			t.Errorf("%s %s: %d %s", method, PathHealth, status, body)
		}
		if method == http.MethodGet && strings.TrimSpace(body) != `{"status":"ok"}` {
			t.Errorf("health body %q", body)
		}
	}
	if status, _ := send(t, http.MethodPost, ts.URL+PathHealth, "", "", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("POST %s: %d, want 405", PathHealth, status)
	}

	// Without a database the service is not healthy.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	status, body := send(t, http.MethodGet, ts.URL+PathHealth, "", "", nil)
	if status != http.StatusServiceUnavailable || strings.TrimSpace(body) != `{"status":"unavailable"}` {
		t.Errorf("health without a database: %d %s", status, body)
	}
}

func TestUnknownRoutes(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	for _, c := range []struct {
		method, path, secret string
		status               int
	}{
		{http.MethodGet, "/", "", http.StatusNotFound},
		{http.MethodGet, "/no/such/route", "", http.StatusNotFound},
		{http.MethodGet, "/v2/jobs/claim", secret, http.StatusNotFound},
		{http.MethodPost, api.PathPrefix + "/no/such/route", secret, http.StatusNotFound},
		{http.MethodGet, api.PathClaim, secret, http.StatusMethodNotAllowed},
	} {
		status, body := send(t, c.method, ts.URL+c.path, c.secret, "", nil)
		if status != c.status {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, status, c.status, body)
		}
	}
}

// TestRequestLog checks that every request is logged, and that neither the
// secret nor the body or query string ever is.
func TestRequestLog(t *testing.T) {
	f := newFixture(t)
	ts := f.serve()
	post(t, ts.URL+api.PathClaim+"?token=query-secret-value", secret, `{"clipper":"body-clipper-name"}`)
	post(t, ts.URL+api.PathClaim, wrongSecret, `{"clipper":"node-a"}`)
	send(t, http.MethodGet, ts.URL+PathHealth, "", "", nil)
	ts.Close() // waits for every handler, and so for every log line

	out := f.log.String()
	for _, want := range []string{
		"path=" + api.PathClaim + " status=200",
		"level=WARN msg=\"http request\" method=POST path=" + api.PathClaim + " status=401",
		"path=" + PathHealth + " status=200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{secret, wrongSecret, "query-secret-value", "body-clipper-name", "Bearer"} {
		if strings.Contains(out, leak) {
			t.Errorf("log contains %q:\n%s", leak, out)
		}
	}
}

// startServe runs Serve on a fresh loopback listener and returns its base
// URL, a cancel function that starts the shutdown, and the channel Serve's
// result arrives on.
func startServe(t *testing.T, s *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	return "http://" + ln.Addr().String(), cancel, done
}

// TestGracefulShutdown starts an upload, begins the shutdown while the
// upload is in flight, and checks that the upload still completes and that
// Serve then returns cleanly.
func TestGracefulShutdown(t *testing.T) {
	f := newFixture(t)
	held := queue.JobID(f.leased("node-a"))

	arrived := make(chan struct{})
	shuttingDown := make(chan struct{})
	f.server.onShutdown = func() { close(shuttingDown) }
	inner := f.server.handler
	f.server.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == api.PathComplete(held) {
			close(arrived)
			<-shuttingDown
		}
		inner.ServeHTTP(w, r)
	})
	url, shutdown, served := startServe(t, f.server)

	clip, meta := clipFile(t, 256<<10)
	type result struct {
		res api.CompleteResult
		err error
	}
	uploaded := make(chan result, 1)
	go func() {
		res, err := newClient(t, url, secret, "node-a").Complete(context.Background(), held, meta, transcript, clip)
		uploaded <- result{res, err}
	}()

	<-arrived
	shutdown()
	<-shuttingDown

	r := <-uploaded
	if r.err != nil || r.res != api.ResultAccepted {
		t.Fatalf("upload in flight during shutdown: %q, %v", r.res, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if st := f.job(mustParse(t, held)).Status; st != store.JobDone {
		t.Errorf("job status %s, want done", st)
	}
	// The listener is closed: nothing is accepted any more.
	if _, err := newClient(t, url, secret, "node-a").Claim(context.Background()); err == nil || errors.Is(err, api.ErrNoJob) {
		t.Errorf("claim after shutdown: %v, want a connection error", err)
	}
}

// TestShutdownDeadline checks that a request still running at the end of
// the shutdown timeout is cut off and reported.
func TestShutdownDeadline(t *testing.T) {
	f := newFixture(t)
	f.server.shutdownTimeout = time.Nanosecond
	arrived := make(chan struct{})
	f.server.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done() // until the connection is closed
	})
	url, shutdown, served := startServe(t, f.server)

	answered := make(chan error, 1)
	go func() {
		resp, err := http.Get(url + PathHealth)
		if err == nil {
			resp.Body.Close()
		}
		answered <- err
	}()
	<-arrived
	shutdown()
	if err := <-served; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Serve: %v, want a deadline error", err)
	}
	if err := <-answered; err == nil {
		t.Error("the cut-off request got an answer")
	}
}

func TestServeListenerFailure(t *testing.T) {
	f := newFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if err := f.server.Serve(context.Background(), ln); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}

func TestErrorCode(t *testing.T) {
	for err, want := range map[error]api.ErrorCode{
		queue.ErrNoClipper:        api.CodeInvalidRequest,
		errInvalidBody:            api.CodeInvalidRequest,
		queue.ErrUnknownJob:       api.CodeUnknownJob,
		queue.ErrLeaseLost:        api.CodeLeaseLost,
		queue.ErrAlreadyCompleted: api.CodeAlreadyCompleted,
		queue.ErrNoJob:            api.CodeInternal, // handled before mapping
		errors.New("disk full"):   api.CodeInternal,
	} {
		if got := errorCode(err); got != want {
			t.Errorf("errorCode(%v) = %s, want %s", err, got, want)
		}
	}
}
