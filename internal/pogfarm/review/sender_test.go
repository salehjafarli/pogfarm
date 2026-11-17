package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/intake"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// botCall is one call the fake Bot API received, from a JSON or a multipart
// body.
type botCall struct {
	method string
	// fields are the plain fields: form fields of a multipart body, or the
	// top-level values of a JSON body, as JSON text unless they are strings.
	fields map[string]string
	// file is the uploaded file of a multipart body.
	file     []byte
	fileName string
	fileType string
}

// fakeBotAPI is a local stand-in for the Bot API that understands uploads.
type fakeBotAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	calls  []botCall
	nextID int64
	// respond, when set, may answer a call itself: it returns the status
	// and body, or ok false for the default success answer.
	respond func(c botCall) (status int, body string, ok bool)
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{t: t, nextID: 100}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) client() *Client {
	c := NewClient(config.NewSecret(testToken))
	c.BaseURL = f.srv.URL
	return c
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok || r.Method != http.MethodPost {
		writeJSON(w, http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: body"}`)
		return
	}
	if r.ContentLength != int64(len(raw)) {
		f.t.Errorf("%s: Content-Length %d, body %d bytes", method, r.ContentLength, len(raw))
	}
	c := botCall{method: method, fields: map[string]string{}}
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mediaType {
	case "multipart/form-data":
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				f.t.Errorf("%s: bad multipart body: %v", method, err)
				break
			}
			data, _ := io.ReadAll(p)
			if p.FileName() != "" {
				c.file, c.fileName, c.fileType = data, p.FileName(), p.Header.Get("Content-Type")
			} else {
				c.fields[p.FormName()] = string(data)
			}
		}
	case "application/json":
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: bad JSON body: %v", method, err)
		}
		for k, v := range body {
			var s string
			if json.Unmarshal(v, &s) == nil {
				c.fields[k] = s
			} else {
				c.fields[k] = string(v)
			}
		}
	default:
		f.t.Errorf("%s: unexpected content type %q", method, mediaType)
	}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	respond := f.respond
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	if respond != nil {
		if status, body, ok := respond(c); ok {
			writeJSON(w, status, body)
			return
		}
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{
		"message_id": id, "date": 1700000000,
		"chat": map[string]any{"id": json.RawMessage(c.fields["chat_id"]), "type": "private"},
	}})
	writeJSON(w, http.StatusOK, string(out))
}

func (f *fakeBotAPI) setRespond(fn func(c botCall) (int, string, bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = fn
}

// take returns the calls received since the last take.
func (f *fakeBotAPI) take() []botCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// testClock is a manual clock. Its sleep advances the clock instead of
// waiting and records how long it was asked to wait.
type testClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err() == nil
}

func (c *testClock) takeSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sleeps
	c.sleeps = nil
	return s
}

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// senderFixture is a sender wired to a fake Bot API and a temporary store
// holding one streamer and one VOD.
type senderFixture struct {
	t       *testing.T
	api     *fakeBotAPI
	store   *store.Store
	dataDir string
	clock   *testClock
	sender  *Sender
	log     *bytes.Buffer
	vod     store.VOD
	created time.Time
}

func newSenderFixture(t *testing.T) *senderFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, store.Path(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	streamer, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform: api.PlatformTwitch, PlatformUserID: "1001", Login: "somestreamer",
		DisplayName: "SomeStreamer", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	vod, err := store.InsertVOD(ctx, st.Writer(), store.VOD{
		Platform: api.PlatformTwitch, PlatformVODID: "v1", StreamerID: streamer.ID,
		Title: "Ranked grind <b>day 3</b>", DurationSeconds: 4 * 3600,
		StartedAt: t0.Add(-5 * time.Hour), EndedAt: t0.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &senderFixture{
		t: t, api: newFakeBotAPI(t), store: st, dataDir: dataDir,
		clock: &testClock{now: t0}, log: &bytes.Buffer{}, vod: vod, created: t0.Add(-time.Hour),
	}
	logger := slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.sender = NewSender(f.api.client(), st, ownerChat, dataDir, logger)
	f.sender.now = f.clock.Now
	f.sender.sleep = f.clock.Sleep
	return f
}

// clipSpec describes a clip to add.
type clipSpec struct {
	offset float64
	words  []string
	// content is the file; nil leaves the file out.
	content []byte
	status  store.ReviewStatus
	// messageID marks the clip as already sent.
	messageID int64
}

// addClip stores a clip with its candidate and job, writes its file, and
// returns its id. Each clip is created a minute after the previous one.
func (f *senderFixture) addClip(spec clipSpec) int64 {
	f.t.Helper()
	ctx := context.Background()
	w := f.store.Writer()
	cand, err := store.InsertCandidate(ctx, w, store.Candidate{
		Platform: api.PlatformTwitch, VODID: f.vod.ID, StartSeconds: spec.offset, EndSeconds: spec.offset + 30,
		Score: 1, Signal: store.SignalChatSpike, Status: store.CandidateQueued,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	job, err := store.InsertJob(ctx, w, store.Job{
		Platform: api.PlatformTwitch, CandidateID: cand.ID, Status: store.JobDone, Clipper: "node-a",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	words := make([]api.Word, len(spec.words))
	for i, s := range spec.words {
		words[i] = api.Word{Text: s, StartSeconds: float64(i), EndSeconds: float64(i) + 0.5}
	}
	transcript, _ := json.Marshal(api.Transcript{Language: "en", Words: words})
	rel := intake.ClipPath(job.ID, t0)
	if spec.content != nil {
		path := filepath.Join(f.dataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, spec.content, 0o640); err != nil {
			f.t.Fatal(err)
		}
	}
	f.created = f.created.Add(time.Minute)
	c := store.Clip{
		Platform: api.PlatformTwitch, JobID: job.ID, FilePath: rel, SHA256: strings.Repeat("ab", 32),
		SizeBytes: int64(len(spec.content)), DurationSeconds: 29.6, Width: 1080, Height: 1920,
		TranscriptJSON: string(transcript), Clipper: "node-a", ReviewStatus: spec.status,
		CreatedAt: f.created,
	}
	if spec.status != "" && spec.status != store.ReviewPending {
		c.ReviewedAt = f.created
	}
	if spec.messageID != 0 {
		c.TelegramChatID, c.TelegramMessageID = ownerChat, spec.messageID
	}
	clip, err := store.InsertClip(ctx, w, c)
	if err != nil {
		f.t.Fatal(err)
	}
	return clip.ID
}

func (f *senderFixture) clip(id int64) store.Clip {
	f.t.Helper()
	c, err := store.GetClip(context.Background(), f.store.Reader(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// pass runs one pass that must not fail.
func (f *senderFixture) pass() SendResult {
	f.t.Helper()
	res, err := f.sender.SendPending(context.Background())
	if err != nil {
		f.t.Fatalf("SendPending: %v", err)
	}
	return res
}

func keyboardJSON(rows ...[]InlineKeyboardButton) string {
	out, _ := json.Marshal(InlineKeyboardMarkup{InlineKeyboard: rows})
	return string(out)
}

func reviewButtons(id int64) []InlineKeyboardButton {
	return []InlineKeyboardButton{
		{Text: "Approve", CallbackData: fmt.Sprintf("approve:%d", id)},
		{Text: "Reject", CallbackData: fmt.Sprintf("reject:%d", id)},
	}
}

func TestSenderSendsPendingClipOnce(t *testing.T) {
	f := newSenderFixture(t)
	video := bytes.Repeat([]byte("fake mp4 bytes "), 1000)
	approved := f.addClip(clipSpec{offset: 10, words: []string{"x"}, content: []byte("a"), status: store.ReviewApproved})
	rejected := f.addClip(clipSpec{offset: 20, words: []string{"x"}, content: []byte("r"), status: store.ReviewRejected})
	alreadySent := f.addClip(clipSpec{offset: 30, words: []string{"x"}, content: []byte("s"), messageID: 5})
	first := f.addClip(clipSpec{offset: 3723, words: []string{"no", "way", "he", "hit", "that"}, content: video})
	second := f.addClip(clipSpec{offset: 75, words: nil, content: []byte("second")})

	res := f.pass()
	if want := (SendResult{Sent: []int64{first, second}}); !reflect.DeepEqual(res, want) {
		t.Fatalf("pass = %+v, want %+v", res, want)
	}
	calls := f.api.take()
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.method != "sendVideo" {
		t.Fatalf("method %q, want sendVideo", c.method)
	}
	wantFields := map[string]string{
		"chat_id":            "4242",
		"supports_streaming": "true",
		"duration":           "30",
		"width":              "1080",
		"height":             "1920",
		"caption": fmt.Sprintf("Clip #%d · SomeStreamer\nRanked grind <b>day 3</b>\nAt 1:02:03 in the VOD · 0:29 long\n\n"+
			"“no way he hit that”", first),
		"reply_markup": keyboardJSON(reviewButtons(first)),
	}
	if !reflect.DeepEqual(c.fields, wantFields) {
		t.Errorf("fields\n%q\nwant\n%q", c.fields, wantFields)
	}
	if _, ok := c.fields["parse_mode"]; ok {
		t.Error("the caption must be sent as plain text, without parse_mode")
	}
	if !bytes.Equal(c.file, video) {
		t.Errorf("uploaded file is %d bytes and differs from the clip (%d bytes)", len(c.file), len(video))
	}
	if c.fileName != fmt.Sprintf("job-%d.mp4", f.clip(first).JobID) || c.fileType != "video/mp4" {
		t.Errorf("file part %q of type %q", c.fileName, c.fileType)
	}
	if got := calls[1].fields["caption"]; !strings.HasSuffix(got, "At 1:15 in the VOD · 0:29 long\n\n(no speech)") {
		t.Errorf("second caption %q", got)
	}
	if string(calls[1].file) != "second" {
		t.Errorf("second file %q", calls[1].file)
	}

	// The second send waited for the interval after the first.
	if got := f.clock.takeSleeps(); !reflect.DeepEqual(got, []time.Duration{DefaultSendInterval}) {
		t.Errorf("sleeps %v, want one interval", got)
	}

	for i, id := range []int64{first, second} {
		clip := f.clip(id)
		if clip.TelegramChatID != ownerChat || clip.TelegramMessageID != int64(101+i) {
			t.Errorf("clip %d: message %d/%d, want %d/%d", id, clip.TelegramChatID, clip.TelegramMessageID, ownerChat, 101+i)
		}
		if clip.ReviewStatus != store.ReviewPending {
			t.Errorf("clip %d: status %q", id, clip.ReviewStatus)
		}
	}
	for _, id := range []int64{approved, rejected} {
		if clip := f.clip(id); clip.TelegramMessageID != 0 {
			t.Errorf("decided clip %d was sent", id)
		}
	}
	if clip := f.clip(alreadySent); clip.TelegramMessageID != 5 {
		t.Errorf("already sent clip %d got message %d", alreadySent, clip.TelegramMessageID)
	}

	// A second pass, and a new sender as after a restart, send nothing.
	if res := f.pass(); !reflect.DeepEqual(res, SendResult{}) {
		t.Errorf("second pass = %+v", res)
	}
	restarted := NewSender(f.api.client(), f.store, ownerChat, f.dataDir, nil)
	restarted.now, restarted.sleep = f.clock.Now, f.clock.Sleep
	if res, err := restarted.SendPending(context.Background()); err != nil || !reflect.DeepEqual(res, SendResult{}) {
		t.Errorf("pass after restart = %+v, %v", res, err)
	}
	if calls := f.api.take(); len(calls) != 0 {
		t.Errorf("calls after the first pass: %+v", calls)
	}
}

func TestSenderMissingFile(t *testing.T) {
	f := newSenderFixture(t)
	id := f.addClip(clipSpec{offset: 60, words: []string{"hello"}})

	if res := f.pass(); !reflect.DeepEqual(res, SendResult{Notified: []int64{id}}) {
		t.Fatalf("pass = %+v", res)
	}
	calls := f.api.take()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls %+v, want one sendMessage", calls)
	}
	text := calls[0].fields["text"]
	for _, want := range []string{fmt.Sprintf("Clip #%d", id), "file is missing", "“hello”"} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q does not contain %q", text, want)
		}
	}
	wantKeyboard := keyboardJSON([]InlineKeyboardButton{{Text: "Reject", CallbackData: fmt.Sprintf("reject:%d", id)}})
	if got := calls[0].fields["reply_markup"]; got != wantKeyboard {
		t.Errorf("keyboard %s, want %s", got, wantKeyboard)
	}
	if clip := f.clip(id); clip.TelegramMessageID == 0 {
		t.Error("the message was not recorded")
	}
	// Recorded, so never tried again.
	f.clock.Advance(time.Hour)
	f.pass()
	if calls := f.api.take(); len(calls) != 0 {
		t.Errorf("tried again: %+v", calls)
	}
}

func TestSenderOversizeClip(t *testing.T) {
	f := newSenderFixture(t)
	f.sender.MaxUploadBytes = 10
	small := f.addClip(clipSpec{offset: 1, words: []string{"ok"}, content: []byte("0123456789")})
	big := f.addClip(clipSpec{offset: 2, words: []string{"big"}, content: []byte("0123456789A")})

	if res := f.pass(); !reflect.DeepEqual(res, SendResult{Sent: []int64{small}, Notified: []int64{big}}) {
		t.Fatalf("pass = %+v", res)
	}
	calls := f.api.take()
	if len(calls) != 2 || calls[0].method != "sendVideo" || calls[1].method != "sendMessage" {
		t.Fatalf("calls %+v", calls)
	}
	text := calls[1].fields["text"]
	for _, want := range []string{fmt.Sprintf("Clip #%d", big), "too large", "“big”",
		filepath.FromSlash(f.clip(big).FilePath)} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q does not contain %q", text, want)
		}
	}
	if got, want := calls[1].fields["reply_markup"], keyboardJSON(reviewButtons(big)); got != want {
		t.Errorf("keyboard %s, want %s", got, want)
	}
	if f.clip(big).TelegramMessageID == 0 {
		t.Error("the oversize message was not recorded")
	}
	f.pass()
	if calls := f.api.take(); len(calls) != 0 {
		t.Errorf("tried again: %+v", calls)
	}
}

func TestSenderHonoursRetryAfter(t *testing.T) {
	f := newSenderFixture(t)
	a := f.addClip(clipSpec{offset: 1, content: []byte("a")})
	b := f.addClip(clipSpec{offset: 2, content: []byte("b")})
	var limited atomic.Bool
	limited.Store(true)
	f.api.setRespond(func(c botCall) (int, string, bool) {
		if limited.Load() {
			return http.StatusTooManyRequests,
				`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`, true
		}
		return 0, "", false
	})

	res := f.pass()
	if !reflect.DeepEqual(res, SendResult{RetryAfter: 7 * time.Second}) {
		t.Fatalf("pass = %+v", res)
	}
	if calls := f.api.take(); len(calls) != 1 {
		t.Fatalf("want the pass to stop after the 429, got %d calls", len(calls))
	}

	// Within the wait nothing is sent, whatever Telegram would say.
	limited.Store(false)
	f.clock.Advance(7*time.Second - time.Millisecond)
	if res := f.pass(); res.RetryAfter != time.Millisecond || len(res.Sent) != 0 {
		t.Fatalf("pass within the wait = %+v", res)
	}
	if calls := f.api.take(); len(calls) != 0 {
		t.Fatalf("sent within the wait: %+v", calls)
	}

	f.clock.Advance(time.Millisecond)
	if res := f.pass(); !reflect.DeepEqual(res, SendResult{Sent: []int64{a, b}}) {
		t.Fatalf("pass after the wait = %+v", res)
	}
	if !strings.Contains(f.log.String(), "flood control") {
		t.Errorf("the 429 was not logged:\n%s", f.log)
	}
}

func TestSenderRunWaitsRetryAfter(t *testing.T) {
	f := newSenderFixture(t)
	id := f.addClip(clipSpec{offset: 1, content: []byte("a")})
	var calls atomic.Int32
	f.api.setRespond(func(c botCall) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusTooManyRequests,
				`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":9}}`, true
		}
		return 0, "", false
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sleeps []time.Duration
	f.sender.sleep = func(ctx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		f.clock.Advance(d)
		if len(sleeps) == 2 {
			cancel()
		}
		return ctx.Err() == nil
	}
	f.sender.Run(ctx) // returns once cancelled

	if want := []time.Duration{9 * time.Second, DefaultSendPollInterval}; !reflect.DeepEqual(sleeps, want) {
		t.Errorf("sleeps %v, want %v", sleeps, want)
	}
	if f.clip(id).TelegramMessageID == 0 {
		t.Error("the clip was not sent after the wait")
	}
}

func TestSenderFailingClipDoesNotBlockOthers(t *testing.T) {
	f := newSenderFixture(t)
	// No spacing between sends, so the clock moves only where the test
	// moves it and the backoffs below count from the start.
	f.sender.Interval = 0
	bad := f.addClip(clipSpec{offset: 1, content: []byte("bad")})
	good := f.addClip(clipSpec{offset: 2, content: []byte("good")})
	f.api.setRespond(func(c botCall) (int, string, bool) {
		if string(c.file) == "bad" {
			return http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`, true
		}
		return 0, "", false
	})

	if res := f.pass(); !reflect.DeepEqual(res, SendResult{Sent: []int64{good}, Deferred: []int64{bad}}) {
		t.Fatalf("pass = %+v", res)
	}
	f.api.take()
	if f.clip(bad).TelegramMessageID != 0 {
		t.Fatal("the failed clip was recorded as sent")
	}

	// The failed clip waits out its backoff, which doubles.
	for _, backoff := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		f.clock.Advance(backoff - time.Millisecond)
		f.pass()
		if calls := f.api.take(); len(calls) != 0 {
			t.Fatalf("retried before its backoff of %s: %+v", backoff, calls)
		}
		f.clock.Advance(time.Millisecond)
		if res := f.pass(); !reflect.DeepEqual(res.Deferred, []int64{bad}) {
			t.Fatalf("pass after %s = %+v", backoff, res)
		}
		f.api.take()
	}
	// Server errors are passing trouble: it stays a video, and is sent as
	// one once Telegram recovers.
	f.api.setRespond(nil)
	f.clock.Advance(4 * time.Minute)
	if res := f.pass(); !reflect.DeepEqual(res, SendResult{Sent: []int64{bad}}) {
		t.Fatalf("pass after recovery = %+v", res)
	}
}

func TestSenderRefusedVideoFallsBackToText(t *testing.T) {
	f := newSenderFixture(t)
	id := f.addClip(clipSpec{offset: 1, words: []string{"hi"}, content: []byte("vid")})
	f.api.setRespond(func(c botCall) (int, string, bool) {
		if c.method == "sendVideo" {
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: wrong file"}`, true
		}
		return 0, "", false
	})
	for i := range maxVideoRefusals {
		if res := f.pass(); !reflect.DeepEqual(res, SendResult{Deferred: []int64{id}}) {
			t.Fatalf("attempt %d: pass = %+v", i+1, res)
		}
		f.clock.Advance(time.Hour)
	}
	if res := f.pass(); !reflect.DeepEqual(res, SendResult{Notified: []int64{id}}) {
		t.Fatalf("pass after %d refusals = %+v", maxVideoRefusals, res)
	}
	calls := f.api.take()
	last := calls[len(calls)-1]
	if last.method != "sendMessage" || !strings.Contains(last.fields["text"], "refused the video") ||
		last.fields["reply_markup"] != keyboardJSON(reviewButtons(id)) {
		t.Errorf("fallback message %+v", last)
	}
	if f.clip(id).TelegramMessageID == 0 {
		t.Error("the fallback message was not recorded")
	}
}

func TestSenderNeverLogsTheToken(t *testing.T) {
	f := newSenderFixture(t)
	f.addClip(clipSpec{offset: 1, content: []byte("a")})
	f.api.srv.Close() // every call now fails at the transport

	res := f.pass()
	if len(res.Deferred) != 1 {
		t.Fatalf("pass = %+v", res)
	}
	out := f.log.String()
	if !strings.Contains(out, "sending clip failed") {
		t.Errorf("the failure was not logged:\n%s", out)
	}
	if strings.Contains(out, testToken) || strings.Contains(out, "TEST-token") {
		t.Errorf("the log leaks the token:\n%s", out)
	}
}

func TestSenderRunStopsOnCancel(t *testing.T) {
	f := newSenderFixture(t)
	f.sender.sleep = sleep // the real wait, which only cancellation ends
	f.sender.PollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.sender.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(waitLimit):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestClientSendVideoMissingFile(t *testing.T) {
	f := newFakeBotAPI(t)
	_, err := f.client().SendVideo(context.Background(), ownerChat, VideoUpload{Path: filepath.Join(t.TempDir(), "nope.mp4")})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want a not-exist error, got %v", err)
	}
	if calls := f.take(); len(calls) != 0 {
		t.Errorf("a request was made: %+v", calls)
	}
}

func TestCaptionCleaningAndTruncation(t *testing.T) {
	long := make([]string, 400)
	for i := range long {
		long[i] = "wörd😀"
	}
	info := clipInfo{
		ClipID:          7,
		Streamer:        "Bad\nName\x00",
		VODTitle:        "*bold* _it_ <a href=\"x\">link</a>\r\nsecond line\xff",
		OffsetSeconds:   59.9,
		DurationSeconds: 3600,
		TranscriptJSON:  mustTranscript(long...),
	}
	got := info.caption(MaxCaptionLength)
	if n := textLength(got); n > MaxCaptionLength {
		t.Errorf("caption is %d characters, over the limit", n)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("caption is not valid UTF-8 ending in an ellipsis: %q", got[len(got)-20:])
	}
	wantHead := "Clip #7 · Bad Name\n*bold* _it_ <a href=\"x\">link</a>  second line�\nAt 0:59 in the VOD · 1:00:00 long\n\n“wörd😀 "
	if !strings.HasPrefix(got, wantHead) {
		t.Errorf("caption starts\n%q\nwant\n%q", got[:len(wantHead)], wantHead)
	}

	// Unreadable transcripts and empty titles still give a caption.
	info = clipInfo{ClipID: 8, Streamer: "s", TranscriptJSON: "{"}
	if got := info.caption(MaxCaptionLength); got != "Clip #8 · s\n(untitled VOD)\nAt 0:00 in the VOD · 0:00 long\n\n(transcript unreadable)" {
		t.Errorf("caption %q", got)
	}
}

func mustTranscript(words ...string) string {
	ws := make([]api.Word, len(words))
	for i, w := range words {
		ws[i] = api.Word{Text: w}
	}
	out, _ := json.Marshal(api.Transcript{Words: ws})
	return string(out)
}

func TestTruncateText(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"hello world", 8, "hello w…"},
		{"hello world", 7, "hello…"},
		{"ééééé", 4, "ééé…"},
		// An emoji counts as two, as in UTF-16.
		{"a😀b", 3, "a…"},
		{"😀😀", 4, "😀😀"},
		{"😀😀😀", 4, "😀…"},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		got := truncateText(c.in, c.limit)
		if got != c.want {
			t.Errorf("truncateText(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
		}
		if textLength(got) > c.limit {
			t.Errorf("truncateText(%q, %d) is %d long", c.in, c.limit, textLength(got))
		}
	}
}

func TestCallbackData(t *testing.T) {
	for _, action := range []string{ActionApprove, ActionReject} {
		data := CallbackData(action, math.MaxInt64)
		if len(data) > MaxCallbackDataBytes {
			t.Errorf("%q is %d bytes", data, len(data))
		}
		gotAction, payload, _ := strings.Cut(data, ":")
		id, err := ParseClipID(payload)
		if gotAction != action || err != nil || id != math.MaxInt64 {
			t.Errorf("%q parses to %q, %d, %v", data, gotAction, id, err)
		}
	}
	for _, bad := range []string{"", "x", "-1", "0", "12x"} {
		if _, err := ParseClipID(bad); err == nil {
			t.Errorf("ParseClipID(%q) accepted", bad)
		}
	}
}
