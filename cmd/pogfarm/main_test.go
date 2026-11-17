package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/lock"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

const (
	// testToken has the shape of a real token but belongs to no bot.
	testToken = "123456789:TEST-token-that-must-never-be-logged"
	ownerChat = 4242
	// testSecret is the shared API secret of the test configurations.
	testSecret = "main-test-shared-api-secret"
	// waitLimit only guards against a hang; nothing waits for it to pass.
	waitLimit = 30 * time.Second
)

// testConfig is a minimal valid configuration: every default of the config
// loader, a fresh data directory, a loopback port chosen by the system, and
// no Telegram bot. It is loaded from a file, so it gets the same defaults
// the binary does, and environment variables still override it.
func testConfig(t *testing.T) *config.Pogfarm {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "pogfarm:\n" +
		"  listen_addr: \"127.0.0.1:0\"\n" +
		"  data_dir: \"" + filepath.ToSlash(filepath.Join(dir, "data")) + "\"\n" +
		"  api_secret: \"" + testSecret + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// logBuffer is a log destination that is safe for concurrent use and can
// be waited on.
type logBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written chan struct{}
}

func newLogBuffer() *logBuffer { return &logBuffer{written: make(chan struct{}, 1)} }

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	select {
	case b.written <- struct{}{}:
	default:
	}
	return n, err
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor waits until the log matches re and returns the first submatch.
func (b *logBuffer) waitFor(t *testing.T, re *regexp.Regexp) string {
	t.Helper()
	timeout := time.After(waitLimit)
	for {
		if m := re.FindStringSubmatch(b.String()); m != nil {
			return m[len(m)-1]
		}
		select {
		case <-b.written:
		case <-timeout:
			t.Fatalf("log never matched %s:\n%s", re, b.String())
		}
	}
}

var listeningRE = regexp.MustCompile(`msg="job API listening" addr=(\S+)`)

// start runs the service in the background and returns a function that
// stops it and returns run's result.
func start(t *testing.T, cfg *config.Pogfarm, log io.Writer, telegramURL string) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.NewTextHandler(log, nil)), telegramURL) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(waitLimit):
				t.Fatal("run did not return after cancellation")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// TestBotStartsWithPogfarm runs the service against a local stand-in for the
// Bot API: the owner's /ping is answered with pong, and cancelling the context
// stops everything.
func TestBotStartsWithPogfarm(t *testing.T) {
	var polls atomic.Int32
	sent := make(chan map[string]any, 4)
	shutdown := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bot" + testToken + "/getUpdates":
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":
					{"message_id":1,"date":1700000000,"text":"/ping","chat":{"id":4242,"type":"private"}}}]}`)
				return
			}
			// An idle long poll.
			select {
			case <-r.Context().Done():
			case <-shutdown:
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case "/bot" + testToken + "/sendMessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent <- body
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":2,"date":1700000001,"chat":{"id":4242,"type":"private"}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		}
	}))
	defer srv.Close()
	defer close(shutdown)

	cfg := testConfig(t)
	cfg.Telegram = config.Telegram{
		BotToken:    config.NewSecret(testToken),
		OwnerChatID: ownerChat,
	}
	log := newLogBuffer()
	stop := start(t, cfg, log, srv.URL)

	select {
	case body := <-sent:
		if body["text"] != "pong" || body["chat_id"] != float64(ownerChat) {
			t.Errorf("unexpected reply: %v", body)
		}
	case <-time.After(waitLimit):
		t.Fatal("no answer to /ping")
	}

	if err := stop(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out := log.String(); strings.Contains(out, testToken) || strings.Contains(out, "TEST-token") {
		t.Errorf("the log leaks the token:\n%s", out)
	}
}

func TestRunWithoutTelegram(t *testing.T) {
	log := newLogBuffer()
	// An address nothing listens on: the bot must not be started.
	stop := start(t, testConfig(t), log, "http://127.0.0.1:1")
	if err := stop(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out := log.String(); !strings.Contains(out, "telegram bot is off") || strings.Contains(out, "poll failed") {
		t.Errorf("unexpected log:\n%s", out)
	}
}

// TestRunServesJobAPI starts the service with a minimal configuration and
// talks to it: health without the secret, the job API with it. A second
// instance on the same data directory is refused while the first runs, and
// stopping releases the data directory.
func TestRunServesJobAPI(t *testing.T) {
	cfg := testConfig(t)
	log := newLogBuffer()
	stop := start(t, cfg, log, "http://127.0.0.1:1")
	base := "http://" + log.waitFor(t, listeningRE)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", resp.StatusCode)
	}

	ctx := context.Background()
	client, err := api.NewClient(api.ClientOptions{BaseURL: base, Secret: testSecret, Clipper: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(ctx); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("claim: %v, want ErrNoJob", err)
	}
	wrong, err := api.NewClient(api.ClientOptions{BaseURL: base, Secret: "not-the-shared-secret", Clipper: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Claim(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("claim with a wrong secret: %v, want ErrUnauthorized", err)
	}

	// A second pogfarm on the same data directory refuses to start, even
	// on another port.
	// Its context is already cancelled, so a run that wrongly starts returns
	// at once instead of hanging the test.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	second := *cfg
	err = run(cancelled, &second, slog.New(slog.DiscardHandler), "http://127.0.0.1:1")
	if !errors.Is(err, lock.ErrHeld) {
		t.Errorf("second instance: %v, want lock.ErrHeld", err)
	}

	if err := stop(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Error("the job API still answers after shutdown")
	}
	if out := log.String(); strings.Contains(out, testSecret) {
		t.Errorf("the log leaks the API secret:\n%s", out)
	}

	// The data directory is free again.
	l, err := lock.Acquire(cfg.DataDir)
	if err != nil {
		t.Fatalf("lock after shutdown: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestRunRefusesLockedDataDir checks that pogfarm gives up before touching
// the data directory when another holder has the lock.
func TestRunRefusesLockedDataDir(t *testing.T) {
	cfg := testConfig(t)
	held, err := lock.Acquire(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a run that wrongly starts returns at once
	err = run(ctx, cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1")
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("run: %v, want lock.ErrHeld", err)
	}
	if _, err := os.Stat(store.Path(cfg.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database was created: %v", err)
	}
}

// TestRunBadListenAddr checks that an unusable address fails startup and
// still releases the data directory.
func TestRunBadListenAddr(t *testing.T) {
	cfg := testConfig(t)
	cfg.ListenAddr = "127.0.0.1:-1"
	if err := run(context.Background(), cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1"); err == nil {
		t.Fatal("run with a bad listen address succeeded")
	}
	l, err := lock.Acquire(cfg.DataDir)
	if err != nil {
		t.Fatalf("lock after failed start: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}
