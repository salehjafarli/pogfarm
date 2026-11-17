package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// testToken has the shape of a real token but belongs to no bot.
const testToken = "123456789:TEST-token-that-must-never-be-logged"

// request is one Bot API call the fake server received.
type request struct {
	method string
	body   map[string]any
}

// fakeTelegram is a local stand-in for the Bot API. Tests never contact the
// real one.
type fakeTelegram struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []request
	// respond, when set, answers a call instead of the default behaviour. It
	// returns false to fall through to the default.
	respond func(w http.ResponseWriter, method string) bool
	// batches are handed out by getUpdates, one per call. With none left,
	// getUpdates holds the request open like an idle long poll.
	batches [][]Update

	sent     chan request
	shutdown chan struct{}
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{sent: make(chan request, 16), shutdown: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		close(f.shutdown)
		f.srv.Close()
	})
	return f
}

func (f *fakeTelegram) client() *Client {
	c := NewClient(config.NewSecret(testToken))
	c.BaseURL = f.srv.URL
	return c
}

func (f *fakeTelegram) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok || r.Method != http.MethodPost {
		writeJSON(w, http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	req := request{method, body}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil && respond(w, method) {
		return
	}

	var batch []Update
	haveBatch := false
	f.mu.Lock()
	if method == "getUpdates" && len(f.batches) > 0 {
		batch, f.batches, haveBatch = f.batches[0], f.batches[1:], true
	}
	f.mu.Unlock()

	switch method {
	case "getUpdates":
		if !haveBatch {
			select {
			case <-r.Context().Done():
			case <-f.shutdown:
			}
			batch = []Update{}
		}
		out, _ := json.Marshal(map[string]any{"ok": true, "result": batch})
		writeJSON(w, http.StatusOK, string(out))
	case "sendMessage":
		f.sent <- req
		out, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{
			"message_id": 77, "date": 1700000000, "text": body["text"],
			"chat": map[string]any{"id": body["chat_id"], "type": "private"},
		}})
		writeJSON(w, http.StatusOK, string(out))
	default:
		writeJSON(w, http.StatusOK, `{"ok":true,"result":true}`)
	}
}

func (f *fakeTelegram) setRespond(respond func(w http.ResponseWriter, method string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *fakeTelegram) recorded() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestClientGetUpdates(t *testing.T) {
	f := newFakeTelegram(t)
	f.setRespond(func(w http.ResponseWriter, _ string) bool {
		writeJSON(w, http.StatusOK, `{"ok":true,"result":[
			{"update_id":100,"message":{"message_id":5,"date":1700000000,"text":"/ping",
				"from":{"id":4242,"is_bot":false,"username":"owner"},"chat":{"id":4242,"type":"private"}}},
			{"update_id":101,"callback_query":{"id":"cb1","data":"approve:17","from":{"id":4242,"is_bot":false},
				"message":{"message_id":9,"date":1700000001,"chat":{"id":4242,"type":"private"}}}},
			{"update_id":102,"edited_message":{"message_id":5,"chat":{"id":4242,"type":"private"}}}
		]}`)
		return true
	})

	updates, err := f.client().GetUpdates(context.Background(), 100, 50*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{
		{UpdateID: 100, Message: &Message{MessageID: 5, Date: 1700000000, Text: "/ping",
			From: &User{ID: 4242, Username: "owner"}, Chat: Chat{ID: 4242, Type: "private"}}},
		{UpdateID: 101, CallbackQuery: &CallbackQuery{ID: "cb1", Data: "approve:17", From: User{ID: 4242},
			Message: &Message{MessageID: 9, Date: 1700000001, Chat: Chat{ID: 4242, Type: "private"}}}},
		{UpdateID: 102},
	}
	if !reflect.DeepEqual(updates, want) {
		t.Errorf("updates\n%+v\nwant\n%+v", updates, want)
	}

	reqs := f.recorded()
	if len(reqs) != 1 || reqs[0].method != "getUpdates" {
		t.Fatalf("unexpected requests: %+v", reqs)
	}
	wantBody := map[string]any{
		"offset": float64(100), "timeout": float64(50),
		"allowed_updates": []any{"message", "callback_query"},
	}
	if !reflect.DeepEqual(reqs[0].body, wantBody) {
		t.Errorf("request body %v, want %v", reqs[0].body, wantBody)
	}
}

func TestClientSendMessageAndAnswerCallback(t *testing.T) {
	f := newFakeTelegram(t)
	c := f.client()

	sent, err := c.SendMessage(context.Background(), 4242, "pong")
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != 77 || sent.Chat.ID != 4242 || sent.Text != "pong" {
		t.Errorf("unexpected sent message: %+v", sent)
	}
	if err := c.AnswerCallbackQuery(context.Background(), "cb1", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "getMe", nil, nil); err != nil {
		t.Fatal(err)
	}

	want := []request{
		{"sendMessage", map[string]any{"chat_id": float64(4242), "text": "pong"}},
		{"answerCallbackQuery", map[string]any{"callback_query_id": "cb1"}},
		{"getMe", map[string]any{}},
	}
	if got := f.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("requests\n%+v\nwant\n%+v", got, want)
	}
}

func TestClientAPIErrors(t *testing.T) {
	f := newFakeTelegram(t)
	c := f.client()

	f.setRespond(func(w http.ResponseWriter, _ string) bool {
		writeJSON(w, http.StatusTooManyRequests,
			`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 12","parameters":{"retry_after":12}}`)
		return true
	})
	_, err := c.GetUpdates(context.Background(), 0, time.Second)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Method != "getUpdates" || apiErr.Code != 429 || apiErr.RetryAfter != 12*time.Second {
		t.Errorf("unexpected error: %+v", apiErr)
	}
	if !strings.Contains(err.Error(), "429 Too Many Requests") {
		t.Errorf("unexpected message: %v", err)
	}

	// A proxy in front of Telegram answering with its own error page.
	f.setRespond(func(w http.ResponseWriter, _ string) bool {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>Bad Gateway</html>")
		return true
	})
	_, err = c.SendMessage(context.Background(), 4242, "pong")
	if err == nil || errors.As(err, &apiErr) || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("want a plain error naming the status, got %v", err)
	}

	// The wrong token: the fake, like Telegram, answers 401.
	wrong := NewClient(config.NewSecret("999:wrong"))
	wrong.BaseURL = f.srv.URL
	_, err = wrong.GetUpdates(context.Background(), 0, time.Second)
	if !errors.As(err, &apiErr) || apiErr.Code != 401 {
		t.Errorf("want a 401 *APIError, got %v", err)
	}
}

func TestClientErrorsNeverContainTheToken(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "TEST-token") {
			t.Errorf("the error leaks the token: %v", err)
		}
		if !strings.Contains(err.Error(), "telegram getUpdates") {
			t.Errorf("the error should name the method: %v", err)
		}
	}

	t.Run("connection refused", func(t *testing.T) {
		f := newFakeTelegram(t)
		c := f.client()
		f.srv.Close()
		_, err := c.GetUpdates(context.Background(), 0, time.Second)
		check(t, err)
	})

	t.Run("connection dropped mid-request", func(t *testing.T) {
		f := newFakeTelegram(t)
		f.setRespond(func(w http.ResponseWriter, _ string) bool {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return true
		})
		_, err := f.client().GetUpdates(context.Background(), 0, time.Second)
		check(t, err)
	})

	t.Run("cancelled", func(t *testing.T) {
		f := newFakeTelegram(t)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		_, err := f.client().GetUpdates(ctx, 0, 50*time.Second)
		check(t, err)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled to be detectable, got %v", err)
		}
		if time.Since(start) > waitLimit {
			t.Errorf("the long poll did not stop when cancelled")
		}
	})

	t.Run("unusable base URL", func(t *testing.T) {
		c := NewClient(config.NewSecret(testToken))
		c.BaseURL = "http://bad host"
		_, err := c.GetUpdates(context.Background(), 0, time.Second)
		check(t, err)

		c.BaseURL = "nonsense://127.0.0.1:1"
		_, err = c.GetUpdates(context.Background(), 0, time.Second)
		check(t, err)
	})
}

// TestBotAgainstHTTP runs the bot and the real client together against the
// fake server: the owner's /ping is answered, a stranger's is not, a failing
// network is retried, and the token appears in no log line.
func TestBotAgainstHTTP(t *testing.T) {
	f := newFakeTelegram(t)
	f.batches = [][]Update{
		{textUpdate(1, strangerChat, "/ping")},
		{textUpdate(2, ownerChat, "/ping")},
	}
	// The first three polls fail in three different ways before any succeeds.
	var polls atomic.Int32
	f.setRespond(func(w http.ResponseWriter, method string) bool {
		if method != "getUpdates" {
			return false
		}
		switch polls.Add(1) {
		default:
			return false
		case 1:
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		case 2:
			writeJSON(w, http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
		case 3:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>Bad Gateway</html>")
		}
		return true
	})

	var log bytes.Buffer
	b := NewBot(f.client(), ownerChat, slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b.minBackoff = time.Millisecond
	b.maxBackoff = 4 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()

	var pong request
	select {
	case pong = <-f.sent:
	case <-time.After(waitLimit):
		t.Fatal("no answer to /ping")
	}
	if want := map[string]any{"chat_id": float64(ownerChat), "text": "pong"}; !reflect.DeepEqual(pong.body, want) {
		t.Errorf("sendMessage body %v, want %v", pong.body, want)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(waitLimit):
		t.Fatal("Run did not return after cancellation")
	}

	select {
	case extra := <-f.sent:
		t.Errorf("an unexpected message was sent: %v", extra.body)
	default:
	}
	out := log.String()
	if n := strings.Count(out, "poll failed"); n != 3 {
		t.Errorf("want 3 failed polls logged, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "ignored update from a chat that is not allowed") {
		t.Errorf("the stranger's update should be logged as ignored:\n%s", out)
	}
	if strings.Contains(out, testToken) || strings.Contains(out, "TEST-token") {
		t.Errorf("the log leaks the token:\n%s", out)
	}
}
