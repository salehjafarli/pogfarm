package config

import (
	"strings"
	"testing"
	"time"
)

func TestReviewDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.UndoWindow != 5*time.Minute {
		t.Errorf("review.undo_window default: got %s, want 5m", cfg.Review.UndoWindow)
	}
}

func TestReviewUndoWindow(t *testing.T) {
	tests := []struct {
		name    string
		file    string // value of the key; "" leaves the key out
		env     string
		want    time.Duration
		problem bool
	}{
		{name: "file", file: `"2m"`, want: 2 * time.Minute},
		{name: "env wins", file: `"2m"`, env: "30s", want: 30 * time.Second},
		{name: "longest", file: `"1h"`, want: time.Hour},
		{name: "zero", file: `"0s"`, problem: true},
		{name: "negative", env: "-1m", problem: true},
		{name: "too long", file: `"61m"`, problem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			content := "pogfarm:\n  api_secret: " + goodSecret + "\n"
			if tt.file != "" {
				content += "  review:\n    undo_window: " + tt.file + "\n"
			}
			if tt.env != "" {
				t.Setenv("POGFARM_REVIEW_UNDO_WINDOW", tt.env)
			}
			cfg, err := LoadPogfarm(writeFile(t, "config.yaml", content))
			if tt.problem {
				if err == nil || !strings.Contains(err.Error(), "pogfarm.review.undo_window (POGFARM_REVIEW_UNDO_WINDOW)") {
					t.Fatalf("want an undo_window problem, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Review.UndoWindow != tt.want {
				t.Errorf("got %s, want %s", cfg.Review.UndoWindow, tt.want)
			}
		})
	}
}
