package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// msgSink is a slog handler that hands every record's message to a channel
// and keeps them all.
type msgSink struct {
	msgs chan string
	mu   sync.Mutex
	all  []string
}

func (s *msgSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *msgSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	s.all = append(s.all, r.Message)
	s.mu.Unlock()
	select {
	case s.msgs <- r.Message:
	default:
	}
	return nil
}
func (s *msgSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *msgSink) WithGroup(string) slog.Handler      { return s }

// await reads messages until one equals want. It has no deadline of its
// own, so a loaded machine cannot fail it; a message that never comes runs
// into go test's timeout.
func (s *msgSink) await(want string) {
	for msg := range s.msgs {
		if msg == want {
			return
		}
	}
}

func (s *msgSink) contains(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.all {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

// TestRunStartsScanning wires the loops with Twitch credentials set, an
// empty allowlist (so discovery calls nobody) and a stand-in for
// TwitchDownloaderCLI that always fails: the scanner takes the recorded VOD,
// fails to fetch its chat and schedules a retry, all without the network.
func TestRunStartsScanning(t *testing.T) {
	for _, name := range []string{config.EnvConfigPath, "POGFARM_TELEGRAM_BOT_TOKEN", "POGFARM_TELEGRAM_OWNER_CHAT_ID"} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	tool := filepath.Join(t.TempDir(), "TwitchDownloaderCLI")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho 'download refused' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POGFARM_API_SECRET", "a-secret-that-is-long-enough")
	t.Setenv("POGFARM_DATA_DIR", dir)
	t.Setenv("POGFARM_TWITCH_CLIENT_ID", "made-up-client-id")
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", "made-up-client-secret")
	t.Setenv("POGFARM_TWITCH_DOWNLOADER_PATH", tool)
	cfg, err := config.LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}

	// A VOD recorded by an earlier run.
	ctx := context.Background()
	st, err := store.Open(ctx, store.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	streamer, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform: api.PlatformTwitch, PlatformUserID: "1", Login: "someone", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	vod, err := store.InsertVOD(ctx, st.Writer(), store.VOD{
		Platform: api.PlatformTwitch, PlatformVODID: "2000000001", StreamerID: streamer.ID,
		DurationSeconds: 3600, StartedAt: started, EndedAt: started.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	sink := &msgSink{msgs: make(chan string, 1000)}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(runCtx, cfg, slog.New(sink), "http://127.0.0.1:1") }()

	sink.await("vod scan failed, will retry")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.contains("discovery and scanner started") {
		t.Error("the loops were not reported as started")
	}

	// run closed the store; it opens again and shows the failed attempt.
	st, err = store.Open(ctx, store.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := store.GetVOD(ctx, st.Reader(), vod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScanStatus != store.ScanFailed || got.ScanAttempts != 1 || got.NextScanAt.IsZero() ||
		!strings.Contains(got.ScanError, "fetch chat") {
		t.Errorf("vod after the failed scan: %+v", got)
	}
}

func TestRunWithoutTwitchSkipsScanning(t *testing.T) {
	sink := &msgSink{msgs: make(chan string, 100)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, &config.Pogfarm{}, slog.New(sink), "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if !sink.contains("discovery and scanning are off") {
		t.Error("no warning that scanning is off")
	}
}

func TestRunFailsWhenTheStoreCannotOpen(t *testing.T) {
	// The data directory is a file, so the database cannot be created in it.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Pogfarm{
		DataDir: file,
		Twitch:  config.Twitch{ClientID: "id", ClientSecret: config.NewSecret("secret")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := run(ctx, cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1"); err == nil {
		t.Error("run started without a store")
	}
}
