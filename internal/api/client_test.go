package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	clientTestSecret  = "client-test-secret-value"
	clientTestClipper = "node-a"
	// clientTestAttempts is MaxAttempts of the clients under test.
	clientTestAttempts = 3
)

// newClientTestServer serves handler behind the real secret check, as
// pogfarm does.
func newClientTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(RequireSecret(clientTestSecret, handler))
	t.Cleanup(srv.Close)
	return srv
}

// newTestClient returns a client for baseURL that retries almost at once, so
// tests do not wait. change adjusts the options.
func newTestClient(t *testing.T, baseURL string, change ...func(*ClientOptions)) *Client {
	t.Helper()
	opts := ClientOptions{
		BaseURL:        baseURL,
		Secret:         clientTestSecret,
		Clipper:        clientTestClipper,
		MaxAttempts:    clientTestAttempts,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  2 * time.Millisecond,
	}
	for _, f := range change {
		f(&opts)
	}
	c, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func clientWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientAnswerJSON returns a handler that answers with status and v.
func clientAnswerJSON(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { clientWriteJSON(w, status, v) }
}

// clientAnswerError returns a handler that answers with the contract's error
// document for code.
func clientAnswerError(code ErrorCode, message string) http.HandlerFunc {
	return clientAnswerJSON(code.HTTPStatus(), NewErrorResponse(code, message))
}

// clientAnswerRaw returns a handler that answers with a body that is not
// JSON-encoded by the test.
func clientAnswerRaw(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// clientDropConnection closes the connection without answering: a network
// failure as the client sees it.
func clientDropConnection(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

// clientTruncatedAnswer promises a longer body than it sends and then closes
// the connection: a network failure in the middle of a response.
func clientTruncatedAnswer(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{")
	_ = conn.Close()
}

// clientSequence returns a handler that counts its calls and answers the
// n-th one with the n-th step. The last step answers every later call.
func clientSequence(calls *atomic.Int32, steps ...http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(steps) {
			i = len(steps) - 1
		}
		steps[i](w, r)
	}
}

// clientBlock returns a handler that reads the request and then does not
// answer until the client goes away or the test ends.
func clientBlock(t *testing.T) http.HandlerFunc {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

// clientClip is a clip file on disk with metadata that describes it.
type clientClip struct {
	path string
	meta CompleteMetadata
}

var clientTestTranscript = Transcript{
	Language: "en",
	Words: []Word{
		{Text: "no", StartSeconds: 0.32, EndSeconds: 0.5},
		{Text: "way", StartSeconds: 0.5, EndSeconds: 0.91},
	},
}

// newClientClip writes a clip of size bytes. Only its start is written, so a
// large clip costs no time and little disk.
func newClientClip(t *testing.T, size int64) clientClip {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := []byte("not really an mp4, but bytes all the same")
	if int64(len(start)) > size {
		start = start[:size]
	}
	if _, err := f.Write(start); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return clientClip{path: path, meta: CompleteMetadata{
		SHA256:          hex.EncodeToString(h.Sum(nil)),
		SizeBytes:       size,
		DurationSeconds: 44.5,
		Width:           1080,
		Height:          1920,
		ToolVersions:    map[string]string{"ffmpeg": "7.1"},
	}}
}

// clientUpload is a complete request as a server following the contract
// reads it.
type clientUpload struct {
	meta          CompleteMetadata
	transcript    Transcript
	rawTranscript string
	clipSHA256    string
	clipSize      int64
	clipFileName  string
	contentLength int64
}

// clientReadUpload reads a complete request the way pogfarm must: as a
// stream, part by part, insisting on the contract's parts in the contract's
// order. The clip is hashed, never held.
func clientReadUpload(r *http.Request) (clientUpload, error) {
	up := clientUpload{contentLength: r.ContentLength}
	mr, err := r.MultipartReader()
	if err != nil {
		return up, err
	}
	next := func(name, contentType string) (*multipart.Part, error) {
		p, err := mr.NextPart()
		if err != nil {
			return nil, fmt.Errorf("part %s: %w", name, err)
		}
		if p.FormName() != name {
			return nil, fmt.Errorf("part %q where %q belongs", p.FormName(), name)
		}
		got, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil || got != contentType {
			return nil, fmt.Errorf("part %s has content type %q, want %q", name, p.Header.Get("Content-Type"), contentType)
		}
		return p, nil
	}

	p, err := next(PartMetadata, "application/json")
	if err != nil {
		return up, err
	}
	if err := json.NewDecoder(p).Decode(&up.meta); err != nil {
		return up, fmt.Errorf("metadata: %w", err)
	}

	p, err = next(PartTranscript, "application/json")
	if err != nil {
		return up, err
	}
	raw, err := io.ReadAll(p)
	if err != nil {
		return up, err
	}
	up.rawTranscript = string(raw)
	if err := json.Unmarshal(raw, &up.transcript); err != nil {
		return up, fmt.Errorf("transcript: %w", err)
	}

	p, err = next(PartClip, ClipContentType)
	if err != nil {
		return up, err
	}
	up.clipFileName = p.FileName()
	h := sha256.New()
	if up.clipSize, err = io.Copy(h, p); err != nil {
		return up, err
	}
	up.clipSHA256 = hex.EncodeToString(h.Sum(nil))

	if _, err := mr.NextPart(); err != io.EOF {
		return up, fmt.Errorf("after the clip part: got %v, want the end of the body", err)
	}
	return up, nil
}

// clientAcceptUpload returns a handler that reads an upload, stores what it
// saw in got when got is not nil, and answers like pogfarm: accepted when
// the checksum and size match the metadata.
func clientAcceptUpload(got *clientUpload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		up, err := clientReadUpload(r)
		if got != nil {
			*got = up
		}
		switch {
		case err != nil:
			clientWriteJSON(w, http.StatusBadRequest, NewErrorResponse(CodeInvalidRequest, err.Error()))
		case up.clipSHA256 != up.meta.SHA256 || up.clipSize != up.meta.SizeBytes:
			clientWriteJSON(w, http.StatusUnprocessableEntity, NewErrorResponse(CodeChecksumMismatch, "clip does not match sha256"))
		default:
			clientWriteJSON(w, http.StatusOK, CompleteResponse{Result: ResultAccepted})
		}
	}
}

// clientOp is one operation of the contract, so that behaviour shared by all
// four is tested on all four.
type clientOp struct {
	name  string
	route string
	// retried says whether the client retries the operation.
	retried bool
	// ok is a handler that answers the operation successfully.
	ok   http.HandlerFunc
	call func(ctx context.Context, c *Client, clip clientClip) error
}

const clientTestJobID = "42"

var clientOps = []clientOp{
	{
		name:  "claim",
		route: RouteClaim,
		ok:    clientAnswerJSON(http.StatusOK, ClaimResponse{Job: ptr(sampleJob())}),
		call: func(ctx context.Context, c *Client, _ clientClip) error {
			_, err := c.Claim(ctx)
			return err
		},
	},
	{
		name:    "heartbeat",
		route:   RouteHeartbeat,
		retried: true,
		ok:      clientAnswerJSON(http.StatusOK, HeartbeatResponse{LeaseExpiresAt: leaseTime}),
		call: func(ctx context.Context, c *Client, _ clientClip) error {
			_, err := c.Heartbeat(ctx, clientTestJobID)
			return err
		},
	},
	{
		name:    "complete",
		route:   RouteComplete,
		retried: true,
		ok:      clientAcceptUpload(nil),
		call: func(ctx context.Context, c *Client, clip clientClip) error {
			_, err := c.Complete(ctx, clientTestJobID, clip.meta, clientTestTranscript, clip.path)
			return err
		},
	},
	{
		name:    "fail",
		route:   RouteFail,
		retried: true,
		ok:      func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		call: func(ctx context.Context, c *Client, _ clientClip) error {
			return c.Fail(ctx, clientTestJobID, "VOD was deleted", false)
		},
	},
}

func ptr[T any](v T) *T { return &v }

// wantCalls returns how many requests the operation sends when every one of
// them fails in a way that may be retried.
func (op clientOp) wantCalls() int32 {
	if op.retried {
		return clientTestAttempts
	}
	return 1
}

// serve starts a server that answers the operation's route with handler and
// returns a client for it and a small clip to upload.
func (op clientOp) serve(t *testing.T, handler http.Handler, change ...func(*ClientOptions)) (*Client, clientClip) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(op.route, handler)
	srv := newClientTestServer(t, mux)
	return newTestClient(t, srv.URL, change...), newClientClip(t, 1024)
}

// forEachClientOp runs f as a subtest for each of the four operations.
func forEachClientOp(t *testing.T, f func(t *testing.T, op clientOp)) {
	t.Helper()
	for _, op := range clientOps {
		t.Run(op.name, func(t *testing.T) { f(t, op) })
	}
}

// clientAssertNoSecret fails when err spells out the shared secret.
func clientAssertNoSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), clientTestSecret) {
		t.Errorf("error contains the secret: %v", err)
	}
}

func TestNewClientValidatesOptions(t *testing.T) {
	valid := ClientOptions{BaseURL: "http://pogfarm:8080", Secret: "s", Clipper: "node-a"}
	if _, err := NewClient(valid); err != nil {
		t.Fatalf("valid options: %v", err)
	}

	if _, err := NewClient(ClientOptions{BaseURL: valid.BaseURL, Clipper: "node-a"}); !errors.Is(err, ErrNoSecret) {
		t.Errorf("empty secret: got %v, want ErrNoSecret", err)
	}

	bad := map[string]func(*ClientOptions){
		"empty clipper":       func(o *ClientOptions) { o.Clipper = "" },
		"empty url":           func(o *ClientOptions) { o.BaseURL = "" },
		"no scheme":           func(o *ClientOptions) { o.BaseURL = "pogfarm:8080" },
		"other scheme":        func(o *ClientOptions) { o.BaseURL = "ftp://pogfarm" },
		"no host":             func(o *ClientOptions) { o.BaseURL = "http://" },
		"unparsable":          func(o *ClientOptions) { o.BaseURL = "http://pog farm" },
		"query":               func(o *ClientOptions) { o.BaseURL = "http://pogfarm/?x=1" },
		"user info":           func(o *ClientOptions) { o.BaseURL = "http://user:pw@pogfarm" },
		"negative timeout":    func(o *ClientOptions) { o.CallTimeout = -1 },
		"negative upload":     func(o *ClientOptions) { o.UploadTimeout = -1 },
		"negative attempts":   func(o *ClientOptions) { o.MaxAttempts = -1 },
		"negative base delay": func(o *ClientOptions) { o.RetryBaseDelay = -1 },
		"negative max delay":  func(o *ClientOptions) { o.RetryMaxDelay = -1 },
	}
	for name, change := range bad {
		opts := valid
		change(&opts)
		if c, err := NewClient(opts); err == nil || c != nil {
			t.Errorf("%s: got client %v and error %v, want an error", name, c, err)
		}
	}
}

func TestNewClientDefaults(t *testing.T) {
	c, err := NewClient(ClientOptions{BaseURL: "http://pogfarm:8080/", Secret: "s", Clipper: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if c.callTimeout != DefaultCallTimeout || c.uploadTimeout != DefaultUploadTimeout ||
		c.maxAttempts != DefaultMaxAttempts || c.retryBase != DefaultRetryBaseDelay || c.retryMax != DefaultRetryMaxDelay {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.callTimeout >= c.uploadTimeout {
		t.Errorf("call timeout %v is not shorter than upload timeout %v", c.callTimeout, c.uploadTimeout)
	}
	if c.base != "http://pogfarm:8080" {
		t.Errorf("base = %q, want the trailing slash removed", c.base)
	}
}

// clientDecodeRequest checks the framing of a JSON request and decodes it.
func clientDecodeRequest[T any](t *testing.T, r *http.Request) T {
	t.Helper()
	var v T
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Errorf("request body: %v", err)
	}
	return v
}

func TestClientClaimReturnsJob(t *testing.T) {
	var calls atomic.Int32
	c, _ := clientOps[0].serve(t, clientSequence(&calls, func(w http.ResponseWriter, r *http.Request) {
		if got := clientDecodeRequest[ClaimRequest](t, r); got.Clipper != clientTestClipper {
			t.Errorf("clipper = %q, want %q", got.Clipper, clientTestClipper)
		}
		// Sent as the contract's document, with a field this client does
		// not know.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"job": `+sampleJobJSON+`, "added_later": true}`)
	}))

	job, err := c.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if want := sampleJob(); job != want {
		t.Errorf("job = %+v, want %+v", job, want)
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests, want 1", calls.Load())
	}
}

func TestClientClaimNoJob(t *testing.T) {
	c, _ := clientOps[0].serve(t, clientAnswerRaw(http.StatusOK, `{"job": null, "retry_after_seconds": 10}`))

	job, err := c.Claim(context.Background())
	if !errors.Is(err, ErrNoJob) {
		t.Fatalf("Claim: got %v, want ErrNoJob", err)
	}
	var noJob *NoJobError
	if !errors.As(err, &noJob) || noJob.RetryAfter != 10*time.Second {
		t.Errorf("got %#v, want a *NoJobError with RetryAfter 10s", err)
	}
	if job != (Job{}) {
		t.Errorf("job = %+v, want the zero job", job)
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		t.Errorf("no job is not an error answer, got %#v", apiErr)
	}
}

func TestClientHeartbeatReturnsNewExpiry(t *testing.T) {
	want := leaseTime.Add(2 * time.Minute)
	c, _ := clientOps[1].serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("id"); got != clientTestJobID {
			t.Errorf("job id = %q, want %q", got, clientTestJobID)
		}
		if got := clientDecodeRequest[HeartbeatRequest](t, r); got.Clipper != clientTestClipper {
			t.Errorf("clipper = %q, want %q", got.Clipper, clientTestClipper)
		}
		clientWriteJSON(w, http.StatusOK, HeartbeatResponse{LeaseExpiresAt: want})
	}))

	got, err := c.Heartbeat(context.Background(), clientTestJobID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("expiry = %v, want %v", got, want)
	}
}

func TestClientFailSendsReport(t *testing.T) {
	var calls atomic.Int32
	c, _ := clientOps[3].serve(t, clientSequence(&calls, func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("id"); got != clientTestJobID {
			t.Errorf("job id = %q, want %q", got, clientTestJobID)
		}
		got := clientDecodeRequest[FailRequest](t, r)
		want := FailRequest{Clipper: clientTestClipper, Reason: "disk full", Retryable: true}
		if got != want {
			t.Errorf("request = %+v, want %+v", got, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := c.Fail(context.Background(), clientTestJobID, "disk full", true); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests, want 1", calls.Load())
	}
}

func TestClientCompleteUploadsAllParts(t *testing.T) {
	for _, result := range []CompleteResult{ResultAccepted, ResultAlreadyCompleted} {
		t.Run(string(result), func(t *testing.T) {
			var got clientUpload
			c, clip := clientOps[2].serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if id := r.PathValue("id"); id != clientTestJobID {
					t.Errorf("job id = %q, want %q", id, clientTestJobID)
				}
				var err error
				if got, err = clientReadUpload(r); err != nil {
					t.Errorf("upload does not follow the contract: %v", err)
				}
				clientWriteJSON(w, http.StatusOK, CompleteResponse{Result: result})
			}))

			meta := clip.meta
			meta.Clipper = "somebody-else"
			res, err := c.Complete(context.Background(), clientTestJobID, meta, clientTestTranscript, clip.path)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if res != result {
				t.Errorf("result = %q, want %q", res, result)
			}

			wantMeta := clip.meta
			wantMeta.Clipper = clientTestClipper
			if fmt.Sprint(got.meta) != fmt.Sprint(wantMeta) {
				t.Errorf("metadata = %+v, want %+v", got.meta, wantMeta)
			}
			if fmt.Sprint(got.transcript) != fmt.Sprint(clientTestTranscript) {
				t.Errorf("transcript = %+v, want %+v", got.transcript, clientTestTranscript)
			}
			if got.clipSHA256 != clip.meta.SHA256 || got.clipSize != clip.meta.SizeBytes {
				t.Errorf("clip arrived as %d bytes with sha256 %s, want %d bytes with %s",
					got.clipSize, got.clipSHA256, clip.meta.SizeBytes, clip.meta.SHA256)
			}
			if got.clipFileName == "" {
				t.Error("clip part has no file name")
			}
			if got.contentLength <= clip.meta.SizeBytes {
				t.Errorf("Content-Length = %d, want the size of the whole body", got.contentLength)
			}
		})
	}
}

func TestClientCompleteSendsEmptyWordsAsArray(t *testing.T) {
	var got clientUpload
	c, clip := clientOps[2].serve(t, clientAcceptUpload(&got))

	if _, err := c.Complete(context.Background(), clientTestJobID, clip.meta, Transcript{}, clip.path); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := `{"language":"","words":[]}`; got.rawTranscript != want {
		t.Errorf("transcript sent as %s, want %s", got.rawTranscript, want)
	}
}

// The upload must not hold the clip in memory. The test uploads a clip far
// larger than everything the process may allocate while doing so. The server
// is in the same process and hashes the clip as it arrives, so its
// allocations count as well.
func TestClientCompleteStreamsLargeFile(t *testing.T) {
	const (
		size     = 128 << 20
		maxAlloc = 16 << 20
	)
	var got clientUpload
	c, _ := clientOps[2].serve(t, clientAcceptUpload(&got))
	clip := newClientClip(t, size)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res, err := c.Complete(context.Background(), clientTestJobID, clip.meta, clientTestTranscript, clip.path)
	runtime.ReadMemStats(&after)

	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res != ResultAccepted {
		t.Errorf("result = %q, want %q", res, ResultAccepted)
	}
	if got.clipSize != size || got.clipSHA256 != clip.meta.SHA256 {
		t.Errorf("clip arrived as %d bytes with sha256 %s, want %d bytes with %s",
			got.clipSize, got.clipSHA256, size, clip.meta.SHA256)
	}
	// TotalAlloc only ever grows, so the difference is every byte allocated
	// during the upload, whether or not it was freed again.
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("uploading %d MiB allocated %d KiB", size>>20, allocated>>10)
	if allocated > maxAlloc {
		t.Errorf("uploading %d bytes allocated %d bytes, want at most %d", size, allocated, maxAlloc)
	}
}

func TestClientCompleteMissingFile(t *testing.T) {
	var calls atomic.Int32
	c, clip := clientOps[2].serve(t, clientSequence(&calls, clientAcceptUpload(nil)))

	missing := filepath.Join(t.TempDir(), "nothing.mp4")
	_, err := c.Complete(context.Background(), clientTestJobID, clip.meta, clientTestTranscript, missing)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want a file-not-found error", err)
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests, want none", calls.Load())
	}
}

// A retried upload sends the whole clip again, from the start of the file.
func TestClientCompleteRetrySendsWholeClipAgain(t *testing.T) {
	var calls atomic.Int32
	var first, second clientUpload
	c, _ := clientOps[2].serve(t, clientSequence(&calls,
		func(w http.ResponseWriter, r *http.Request) {
			first, _ = clientReadUpload(r)
			clientAnswerError(CodeInternal, "disk hiccup")(w, r)
		},
		clientAcceptUpload(&second),
	))
	clip := newClientClip(t, 300<<10)

	res, err := c.Complete(context.Background(), clientTestJobID, clip.meta, clientTestTranscript, clip.path)
	if err != nil || res != ResultAccepted {
		t.Fatalf("Complete: got %q, %v, want accepted", res, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d requests, want 2", calls.Load())
	}
	for i, up := range []clientUpload{first, second} {
		if up.clipSHA256 != clip.meta.SHA256 || up.clipSize != clip.meta.SizeBytes {
			t.Errorf("attempt %d carried %d bytes with sha256 %s, want %d bytes with %s",
				i+1, up.clipSize, up.clipSHA256, clip.meta.SizeBytes, clip.meta.SHA256)
		}
	}
}

// A clip that does not match its checksum is rejected by the server, and the
// client reports it without sending it again.
func TestClientCompleteChecksumMismatch(t *testing.T) {
	var calls atomic.Int32
	c, clip := clientOps[2].serve(t, clientSequence(&calls, clientAcceptUpload(nil)))

	meta := clip.meta
	meta.SHA256 = strings.Repeat("0", 64)
	_, err := c.Complete(context.Background(), clientTestJobID, meta, clientTestTranscript, clip.path)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("got %v, want ErrChecksumMismatch", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests, want 1", calls.Load())
	}
}

// Every error code of the contract arrives as its typed error, from every
// operation. 4xx answers are never retried.
func TestClientMapsErrorCodes(t *testing.T) {
	codes := []struct {
		code ErrorCode
		want error
	}{
		{CodeUnauthenticated, ErrUnauthorized},
		{CodeInvalidRequest, ErrInvalidRequest},
		{CodeUnknownJob, ErrUnknownJob},
		{CodeLeaseLost, ErrLeaseLost},
		{CodeAlreadyCompleted, ErrAlreadyCompleted},
		{CodePayloadTooLarge, ErrPayloadTooLarge},
		{CodeChecksumMismatch, ErrChecksumMismatch},
		{CodeInternal, ErrInternal},
	}
	if len(codes) != len(codeErrors) {
		t.Fatalf("test covers %d codes, the client maps %d", len(codes), len(codeErrors))
	}
	sentinels := []error{ErrNoJob, ErrBadResponse}
	for _, c := range codes {
		sentinels = append(sentinels, c.want)
	}

	forEachClientOp(t, func(t *testing.T, op clientOp) {
		for _, tc := range codes {
			t.Run(string(tc.code), func(t *testing.T) {
				const message = "job 42 is in a state"
				var calls atomic.Int32
				c, clip := op.serve(t, clientSequence(&calls, clientAnswerError(tc.code, message)))

				err := op.call(context.Background(), c, clip)
				for _, s := range sentinels {
					if got, want := errors.Is(err, s), s == tc.want; got != want {
						t.Errorf("errors.Is(%v, %v) = %v, want %v", err, s, got, want)
					}
				}
				var apiErr *Error
				if !errors.As(err, &apiErr) {
					t.Fatalf("got %#v, want an *Error", err)
				}
				want := Error{Status: tc.code.HTTPStatus(), Code: tc.code, Message: message}
				if *apiErr != want {
					t.Errorf("error = %+v, want %+v", *apiErr, want)
				}
				if !strings.Contains(err.Error(), op.name) || !strings.Contains(err.Error(), string(tc.code)) {
					t.Errorf("error %q does not name the operation and the code", err)
				}

				wantCalls := int32(1)
				if tc.code == CodeInternal {
					wantCalls = op.wantCalls()
				}
				if calls.Load() != wantCalls {
					t.Errorf("%d requests, want %d", calls.Load(), wantCalls)
				}
				if got, want := apiErr.Transient(), tc.code == CodeInternal; got != want {
					t.Errorf("Transient() = %v, want %v", got, want)
				}
			})
		}
	})
}

// A client with the wrong secret is turned away by the real middleware and
// learns it as ErrUnauthorized, after one request.
func TestClientWrongSecret(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var reached atomic.Int32
		const wrong = "not-the-secret"
		c, clip := op.serve(t, clientSequence(&reached, op.ok), func(o *ClientOptions) { o.Secret = wrong })

		err := op.call(context.Background(), c, clip)
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("got %v, want ErrUnauthorized", err)
		}
		if reached.Load() != 0 {
			t.Errorf("handler reached %d times behind the secret check", reached.Load())
		}
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("error contains the secret: %v", err)
		}
	})
}

// A 5xx answer is retried by the idempotent operations until it succeeds.
func TestClientRetriesServerErrors(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls,
			clientAnswerError(CodeInternal, "first failure"),
			clientAnswerRaw(http.StatusBadGateway, "<html>bad gateway</html>"),
			op.ok,
		))

		err := op.call(context.Background(), c, clip)
		if op.retried {
			if err != nil {
				t.Errorf("got %v, want success on the third attempt", err)
			}
			if calls.Load() != 3 {
				t.Errorf("%d requests, want 3", calls.Load())
			}
			return
		}
		if !errors.Is(err, ErrInternal) {
			t.Errorf("got %v, want ErrInternal", err)
		}
		if calls.Load() != 1 {
			t.Errorf("%d requests, want 1: claim is not retried", calls.Load())
		}
	})
}

// A network failure is retried by the idempotent operations until it
// succeeds, whether the connection breaks before or during the answer.
func TestClientRetriesNetworkErrors(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls, clientDropConnection, clientTruncatedAnswer, op.ok))

		err := op.call(context.Background(), c, clip)
		clientAssertNoSecret(t, err)
		if op.retried {
			if err != nil {
				t.Errorf("got %v, want success on the third attempt", err)
			}
			if calls.Load() != 3 {
				t.Errorf("%d requests, want 3", calls.Load())
			}
			return
		}
		if err == nil {
			t.Error("got success, want the network error: claim is not retried")
		}
		if calls.Load() != 1 {
			t.Errorf("%d requests, want 1", calls.Load())
		}
	})
}

// When the network keeps failing the client gives up after MaxAttempts and
// reports an error that is none of the typed answers.
func TestClientGivesUpOnNetworkErrors(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls, clientDropConnection))

		err := op.call(context.Background(), c, clip)
		if err == nil {
			t.Fatal("got success from a server that never answers")
		}
		clientAssertNoSecret(t, err)
		var apiErr *Error
		if errors.As(err, &apiErr) || errors.Is(err, ErrBadResponse) || errors.Is(err, ErrNoJob) {
			t.Errorf("got %#v, want a plain network error", err)
		}
		if !strings.Contains(err.Error(), op.name) {
			t.Errorf("error %q does not name the operation", err)
		}
		if calls.Load() != op.wantCalls() {
			t.Errorf("%d requests, want %d", calls.Load(), op.wantCalls())
		}
	})
}

// Nothing listens at the address at all.
func TestClientServerUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	forEachClientOp(t, func(t *testing.T, op clientOp) {
		c := newTestClient(t, url)
		err := op.call(context.Background(), c, newClientClip(t, 1024))
		if err == nil {
			t.Fatal("got success from a closed server")
		}
		clientAssertNoSecret(t, err)
		var apiErr *Error
		if errors.As(err, &apiErr) {
			t.Errorf("got %#v, want a network error", err)
		}
	})
}

// MaxAttempts 1 switches retries off.
func TestClientMaxAttemptsOne(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls, clientAnswerError(CodeInternal, "down"), op.ok),
			func(o *ClientOptions) { o.MaxAttempts = 1 })

		if err := op.call(context.Background(), c, clip); !errors.Is(err, ErrInternal) {
			t.Errorf("got %v, want ErrInternal", err)
		}
		if calls.Load() != 1 {
			t.Errorf("%d requests, want 1", calls.Load())
		}
	})
}

// Answers that are not the contract's error document are treated by status:
// 5xx is retried, 4xx is not.
func TestClientUnknownErrorAnswers(t *testing.T) {
	tests := []struct {
		name      string
		answer    http.HandlerFunc
		want      Error
		transient bool
	}{
		{
			name:   "unknown code with 4xx",
			answer: clientAnswerJSON(http.StatusTeapot, NewErrorResponse("brand_new_code", "added later")),
			want:   Error{Status: http.StatusTeapot, Code: "brand_new_code", Message: "added later"},
		},
		{
			name:      "unknown code with 5xx",
			answer:    clientAnswerJSON(http.StatusServiceUnavailable, NewErrorResponse("overloaded", "too busy")),
			want:      Error{Status: http.StatusServiceUnavailable, Code: "overloaded", Message: "too busy"},
			transient: true,
		},
		{
			name:      "5xx from a proxy",
			answer:    clientAnswerRaw(http.StatusBadGateway, "<html>bad gateway</html>"),
			want:      Error{Status: http.StatusBadGateway},
			transient: true,
		},
		{
			name:   "4xx from a proxy",
			answer: clientAnswerRaw(http.StatusForbidden, "<html>forbidden</html>"),
			want:   Error{Status: http.StatusForbidden},
		},
		{
			name:   "4xx with other JSON",
			answer: clientAnswerRaw(http.StatusConflict, `{"detail": "conflict"}`),
			want:   Error{Status: http.StatusConflict},
		},
		{
			name:   "4xx with an empty body",
			answer: clientAnswerRaw(http.StatusNotFound, ""),
			want:   Error{Status: http.StatusNotFound},
		},
		{
			name:   "304",
			answer: clientAnswerRaw(http.StatusNotModified, ""),
			want:   Error{Status: http.StatusNotModified},
		},
	}
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var calls atomic.Int32
				c, clip := op.serve(t, clientSequence(&calls, tc.answer))

				err := op.call(context.Background(), c, clip)
				var apiErr *Error
				if !errors.As(err, &apiErr) {
					t.Fatalf("got %#v, want an *Error", err)
				}
				if *apiErr != tc.want {
					t.Errorf("error = %+v, want %+v", *apiErr, tc.want)
				}
				for _, s := range codeErrors {
					if errors.Is(err, s) {
						t.Errorf("error %v matches %v, want no typed match", err, s)
					}
				}
				if apiErr.Transient() != tc.transient {
					t.Errorf("Transient() = %v, want %v", apiErr.Transient(), tc.transient)
				}
				wantCalls := int32(1)
				if tc.transient {
					wantCalls = op.wantCalls()
				}
				if calls.Load() != wantCalls {
					t.Errorf("%d requests, want %d", calls.Load(), wantCalls)
				}
			})
		}
	})
}

// A success status with a body the client cannot use is ErrBadResponse and
// is not retried.
func TestClientBadResponseBody(t *testing.T) {
	tests := []struct {
		op   clientOp
		name string
		body string
	}{
		{clientOps[0], "not json", "<html>hello</html>"},
		{clientOps[0], "empty", ""},
		{clientOps[0], "wrong type", `{"job": "42"}`},
		{clientOps[0], "job without id", `{"job": {"attempt": 1}}`},
		{clientOps[1], "not json", "ok"},
		{clientOps[1], "empty", ""},
		{clientOps[1], "bad time", `{"lease_expires_at": "tomorrow"}`},
		{clientOps[2], "not json", "ok"},
		{clientOps[2], "empty", ""},
		{clientOps[2], "unknown result", `{"result": "maybe"}`},
		{clientOps[2], "no result", `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.op.name+"/"+tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, clip := tc.op.serve(t, clientSequence(&calls, clientAnswerRaw(http.StatusOK, tc.body)))

			err := tc.op.call(context.Background(), c, clip)
			if !errors.Is(err, ErrBadResponse) {
				t.Errorf("got %v, want ErrBadResponse", err)
			}
			var apiErr *Error
			if errors.As(err, &apiErr) {
				t.Errorf("got %#v, want no *Error for a success status", err)
			}
			if calls.Load() != 1 {
				t.Errorf("%d requests, want 1", calls.Load())
			}
		})
	}
}

// Fail has no response body to read, so any 2xx answer is success.
func TestClientFailIgnoresBody(t *testing.T) {
	c, _ := clientOps[3].serve(t, clientAnswerRaw(http.StatusOK, "recorded, thanks"))
	if err := c.Fail(context.Background(), clientTestJobID, "broken", true); err != nil {
		t.Errorf("Fail: %v", err)
	}
}

func TestClientContextAlreadyCancelled(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls, op.ok))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := op.call(ctx, c, clip); !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
		if calls.Load() != 0 {
			t.Errorf("%d requests, want none", calls.Load())
		}
	})
}

// Cancelling the context ends a call that is waiting for the answer, at
// once and without a retry.
func TestClientContextCancelledInFlight(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		started := make(chan struct{}, clientTestAttempts)
		block := clientBlock(t)
		c, clip := op.serve(t, clientSequence(&calls, func(w http.ResponseWriter, r *http.Request) {
			started <- struct{}{}
			block(w, r)
		}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-started
			cancel()
		}()

		begin := time.Now()
		err := op.call(ctx, c, clip)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
		if took := time.Since(begin); took > 5*time.Second {
			t.Errorf("call returned after %v, want at once", took)
		}
		if calls.Load() != 1 {
			t.Errorf("%d requests, want 1", calls.Load())
		}
	})
}

// Cancelling the context also ends the wait between two attempts.
func TestClientContextCancelledDuringBackoff(t *testing.T) {
	for _, op := range clientOps {
		if !op.retried {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			var calls atomic.Int32
			answered := make(chan struct{}, clientTestAttempts)
			c, clip := op.serve(t, clientSequence(&calls, func(w http.ResponseWriter, r *http.Request) {
				clientAnswerError(CodeInternal, "down")(w, r)
				answered <- struct{}{}
			}), func(o *ClientOptions) {
				o.RetryBaseDelay = time.Hour
				o.RetryMaxDelay = time.Hour
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-answered
				cancel()
			}()

			begin := time.Now()
			err := op.call(ctx, c, clip)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("got %v, want context.Canceled", err)
			}
			if took := time.Since(begin); took > 5*time.Second {
				t.Errorf("call returned after %v, want at once", took)
			}
			if calls.Load() != 1 {
				t.Errorf("%d requests, want 1", calls.Load())
			}
		})
	}
}

// A deadline on the caller's context ends the call like a cancellation.
func TestClientContextDeadline(t *testing.T) {
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls, clientBlock(t)))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		if err := op.call(ctx, c, clip); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want context.DeadlineExceeded", err)
		}
		if calls.Load() != 1 {
			t.Errorf("%d requests, want 1", calls.Load())
		}
	})
}

// Each attempt has its own timeout. An attempt that times out counts as a
// transient failure: idempotent operations try again.
func TestClientAttemptTimeout(t *testing.T) {
	short := func(o *ClientOptions) {
		o.CallTimeout = 100 * time.Millisecond
		o.UploadTimeout = 100 * time.Millisecond
	}
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		var calls atomic.Int32
		c, clip := op.serve(t, clientSequence(&calls, clientBlock(t)), short)

		begin := time.Now()
		err := op.call(context.Background(), c, clip)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want context.DeadlineExceeded", err)
		}
		clientAssertNoSecret(t, err)
		if calls.Load() != op.wantCalls() {
			t.Errorf("%d requests, want %d", calls.Load(), op.wantCalls())
		}
		if took := time.Since(begin); took > 10*time.Second {
			t.Errorf("call returned after %v", took)
		}
	})
}

func TestClientRetriesAfterAttemptTimeout(t *testing.T) {
	long := func(o *ClientOptions) {
		o.CallTimeout = 500 * time.Millisecond
		o.UploadTimeout = 500 * time.Millisecond
	}
	for _, op := range clientOps {
		if !op.retried {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			var calls atomic.Int32
			c, clip := op.serve(t, clientSequence(&calls, clientBlock(t), op.ok), long)

			if err := op.call(context.Background(), c, clip); err != nil {
				t.Errorf("got %v, want success on the second attempt", err)
			}
			if calls.Load() != 2 {
				t.Errorf("%d requests, want 2", calls.Load())
			}
		})
	}
}

// The upload has its own, longer timeout: an answer that takes longer than
// CallTimeout is fine for complete and too slow for the other operations.
func TestClientUploadUsesItsOwnTimeout(t *testing.T) {
	const serverDelay = 400 * time.Millisecond
	forEachClientOp(t, func(t *testing.T, op clientOp) {
		slow := func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(serverDelay):
			case <-r.Context().Done():
			}
			op.ok(w, r)
		}
		c, clip := op.serve(t, http.HandlerFunc(slow), func(o *ClientOptions) {
			o.CallTimeout = 50 * time.Millisecond
			o.UploadTimeout = time.Minute
			o.MaxAttempts = 1
		})

		err := op.call(context.Background(), c, clip)
		if op.name == "complete" {
			if err != nil {
				t.Errorf("got %v, want success within the upload timeout", err)
			}
			return
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want context.DeadlineExceeded", err)
		}
	})
}

// The client never follows a redirect, so the secret is never presented to
// the place a redirect points to: neither another host nor another path.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("the secret was sent to another host")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(other.Close)

	targets := map[string]string{
		"other host": other.URL + "/v1/jobs/claim",
		"same host":  "/elsewhere",
	}
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for name, target := range targets {
			forEachClientOp(t, func(t *testing.T, op clientOp) {
				t.Run(fmt.Sprintf("%d to %s", status, name), func(t *testing.T) {
					var calls, followed atomic.Int32
					mux := http.NewServeMux()
					mux.Handle(op.route, clientSequence(&calls, func(w http.ResponseWriter, r *http.Request) {
						http.Redirect(w, r, target, status)
					}))
					mux.Handle("/elsewhere", clientSequence(&followed, op.ok))
					srv := newClientTestServer(t, mux)
					c := newTestClient(t, srv.URL)

					err := op.call(context.Background(), c, newClientClip(t, 1024))
					var apiErr *Error
					if !errors.As(err, &apiErr) || apiErr.Status != status {
						t.Fatalf("got %#v, want an *Error with status %d", err, status)
					}
					if apiErr.Transient() {
						t.Error("a redirect is reported as transient")
					}
					if calls.Load() != 1 {
						t.Errorf("%d requests, want 1", calls.Load())
					}
					if followed.Load() != 0 || elsewhere.Load() != 0 {
						t.Errorf("redirect followed: %d requests to the same host, %d to another",
							followed.Load(), elsewhere.Load())
					}
				})
			})
		}
	}
}

// The base URL may end in a slash and may carry a path, for a pogfarm behind
// a reverse proxy.
func TestClientBaseURLForms(t *testing.T) {
	for _, suffix := range []string{"", "/", "/behind/proxy", "/behind/proxy/"} {
		t.Run("suffix "+suffix, func(t *testing.T) {
			var gotPath atomic.Value
			srv := newClientTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath.Store(r.Method + " " + r.URL.Path)
				clientWriteJSON(w, http.StatusOK, HeartbeatResponse{LeaseExpiresAt: leaseTime})
			}))
			c := newTestClient(t, srv.URL+suffix)

			if _, err := c.Heartbeat(context.Background(), clientTestJobID); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			want := "POST " + strings.TrimSuffix(suffix, "/") + PathHeartbeat(clientTestJobID)
			if got := gotPath.Load(); got != want {
				t.Errorf("request was %q, want %q", got, want)
			}
		})
	}
}

// A job ID is opaque: whatever it contains stays one path segment.
func TestClientEscapesJobID(t *testing.T) {
	const id = "a/b c?d#e"
	const wantSegment = "a%2Fb%20c%3Fd%23e"
	calls := map[string]func(c *Client, clip clientClip) error{
		"heartbeat": func(c *Client, _ clientClip) error {
			_, err := c.Heartbeat(context.Background(), id)
			return err
		},
		"complete": func(c *Client, clip clientClip) error {
			_, err := c.Complete(context.Background(), id, clip.meta, clientTestTranscript, clip.path)
			return err
		},
		"fail": func(c *Client, _ clientClip) error {
			return c.Fail(context.Background(), id, "broken", true)
		},
	}
	for _, op := range clientOps[1:] {
		t.Run(op.name, func(t *testing.T) {
			var gotPath, gotID atomic.Value
			mux := http.NewServeMux()
			mux.HandleFunc(op.route, func(w http.ResponseWriter, r *http.Request) {
				gotPath.Store(r.URL.EscapedPath())
				gotID.Store(r.PathValue("id"))
				op.ok(w, r)
			})
			srv := newClientTestServer(t, mux)
			c := newTestClient(t, srv.URL)

			if err := calls[op.name](c, newClientClip(t, 1024)); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}
			if want := "/v1/jobs/" + wantSegment + "/" + op.name; gotPath.Load() != want {
				t.Errorf("path = %q, want %q", gotPath.Load(), want)
			}
			if gotID.Load() != id {
				t.Errorf("server read job id %q, want %q", gotID.Load(), id)
			}
		})
	}
}

func TestClientRejectsEmptyJobID(t *testing.T) {
	var calls atomic.Int32
	srv := newClientTestServer(t, clientSequence(&calls, clientAnswerError(CodeInternal, "should not be asked")))
	c := newTestClient(t, srv.URL)
	clip := newClientClip(t, 16)
	ctx := context.Background()

	if _, err := c.Heartbeat(ctx, ""); err == nil {
		t.Error("Heartbeat with an empty job id succeeded")
	}
	if _, err := c.Complete(ctx, "", clip.meta, clientTestTranscript, clip.path); err == nil {
		t.Error("Complete with an empty job id succeeded")
	}
	if err := c.Fail(ctx, "", "broken", true); err == nil {
		t.Error("Fail with an empty job id succeeded")
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests, want none", calls.Load())
	}
}

// Heartbeats must keep going during a slow upload, so one client serves
// both at the same time.
func TestClientHeartbeatDuringUpload(t *testing.T) {
	heartbeatSeen := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc(RouteComplete, func(w http.ResponseWriter, r *http.Request) {
		if _, err := clientReadUpload(r); err != nil {
			t.Errorf("upload: %v", err)
		}
		// The upload is only answered once a heartbeat got through.
		select {
		case <-heartbeatSeen:
		case <-r.Context().Done():
		}
		clientWriteJSON(w, http.StatusOK, CompleteResponse{Result: ResultAccepted})
	})
	mux.HandleFunc(RouteHeartbeat, func(w http.ResponseWriter, _ *http.Request) {
		clientWriteJSON(w, http.StatusOK, HeartbeatResponse{LeaseExpiresAt: leaseTime})
	})
	srv := newClientTestServer(t, mux)
	c := newTestClient(t, srv.URL)
	clip := newClientClip(t, 1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uploaded := make(chan error, 1)
	go func() {
		_, err := c.Complete(ctx, clientTestJobID, clip.meta, clientTestTranscript, clip.path)
		uploaded <- err
	}()

	if _, err := c.Heartbeat(ctx, clientTestJobID); err != nil {
		t.Errorf("Heartbeat during the upload: %v", err)
	}
	close(heartbeatSeen)
	if err := <-uploaded; err != nil {
		t.Errorf("Complete: %v", err)
	}
}

func TestClientBackoffGrowsAndIsCapped(t *testing.T) {
	c, err := NewClient(ClientOptions{
		BaseURL: "http://pogfarm", Secret: "s", Clipper: "node-a",
		RetryBaseDelay: 100 * time.Millisecond, RetryMaxDelay: 350 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The wait is somewhere in the upper half of the nominal delay.
	nominal := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 350 * time.Millisecond, 350 * time.Millisecond, 350 * time.Millisecond}
	for i, want := range nominal {
		for range 200 {
			if got := c.backoff(i + 1); got < want/2 || got > want {
				t.Fatalf("backoff(%d) = %v, want between %v and %v", i+1, got, want/2, want)
			}
		}
	}
	if got := c.backoff(1000); got > 350*time.Millisecond {
		t.Errorf("backoff(1000) = %v, want it capped", got)
	}
}

func TestClientErrorStrings(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&Error{Status: 409, Code: CodeLeaseLost, Message: "job 42 is held by another clipper"}, "pogfarm answered 409 lease_lost: job 42 is held by another clipper"},
		{&Error{Status: 409, Code: CodeLeaseLost}, "pogfarm answered 409 lease_lost"},
		{&Error{Status: 502}, "pogfarm answered 502"},
		{&NoJobError{RetryAfter: 10 * time.Second}, "api: no job available, retry after 10s"},
	}
	for _, tc := range tests {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
