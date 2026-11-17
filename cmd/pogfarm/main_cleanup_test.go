package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/intake"
)

// An upload left in the temp directory by an earlier run is deleted at
// startup, before the job API accepts new uploads.
func TestRunDeletesUnfinishedUploads(t *testing.T) {
	cfg := testConfig(t)
	tmp := filepath.Join(cfg.DataDir, intake.TempDir)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(tmp, "upload-left-over.mp4")
	if err := os.WriteFile(leftover, []byte("half a clip"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // run sets everything up, then stops at once.
	if err := run(ctx, cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unfinished upload still there: stat error %v", err)
	}
}

// A cleanup configuration the cleaner refuses stops startup instead of
// running without cleanup.
func TestRunRefusesInvalidCleanupSettings(t *testing.T) {
	cfg := testConfig(t)
	cfg.Cleanup.RejectedRetention = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1"); err == nil {
		t.Error("run started with a zero rejected retention")
	}
}
