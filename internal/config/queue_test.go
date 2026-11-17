package config

import (
	"strings"
	"testing"
	"time"
)

// loadQueue loads a config whose queue section holds key: file (left out
// when file is empty), with env set to envValue when that is not empty.
func loadQueue(t *testing.T, key, file, env, envValue string) (*Pogfarm, error) {
	t.Helper()
	cleanEnv(t)
	content := "pogfarm:\n  api_secret: " + goodSecret + "\n"
	if file != "" {
		content += "  queue:\n    " + key + ": " + file + "\n"
	}
	if envValue != "" {
		t.Setenv(env, envValue)
	}
	return LoadPogfarm(writeFile(t, "config.yaml", content))
}

func TestQueueMaxAttempts(t *testing.T) {
	tests := []struct {
		name    string
		file    string // value of pogfarm.queue.max_attempts; "" leaves the key out
		env     string
		want    int
		problem string
	}{
		{name: "default", want: 3},
		{name: "from file", file: "5", want: 5},
		{name: "env overrides file", file: "5", env: "2", want: 2},
		{name: "minimum", env: "1", want: 1},
		{name: "zero", file: "0", problem: "pogfarm.queue.max_attempts (POGFARM_QUEUE_MAX_ATTEMPTS) must be at least 1: got 0"},
		{name: "negative", env: "-1", problem: "must be at least 1: got -1"},
		{name: "not a number", env: "many", problem: "POGFARM_QUEUE_MAX_ATTEMPTS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadQueue(t, "max_attempts", tt.file, "POGFARM_QUEUE_MAX_ATTEMPTS", tt.env)
			if tt.problem != "" {
				problems := problemsOf(t, err)
				if len(problems) != 1 || !strings.Contains(problems[0], tt.problem) {
					t.Fatalf("problems = %q, want one containing %q", problems, tt.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Queue.MaxAttempts != tt.want {
				t.Errorf("max attempts = %d, want %d", cfg.Queue.MaxAttempts, tt.want)
			}
		})
	}
}

func TestQueueReaperInterval(t *testing.T) {
	tests := []struct {
		name    string
		file    string // value of pogfarm.queue.reaper_interval; "" leaves the key out
		env     string
		want    time.Duration
		problem string
	}{
		{name: "default", want: 15 * time.Second},
		{name: "from file", file: "30s", want: 30 * time.Second},
		{name: "env overrides file", file: "30s", env: "1m", want: time.Minute},
		{name: "minimum", env: "1s", want: time.Second},
		{name: "too short", file: "500ms", problem: "pogfarm.queue.reaper_interval (POGFARM_QUEUE_REAPER_INTERVAL) must be at least 1s: got 500ms"},
		{name: "zero", env: "0s", problem: "must be at least 1s: got 0s"},
		{name: "negative", file: "-5s", problem: "must be at least 1s: got -5s"},
		{name: "not a duration", env: "often", problem: "POGFARM_QUEUE_REAPER_INTERVAL: not a duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadQueue(t, "reaper_interval", tt.file, "POGFARM_QUEUE_REAPER_INTERVAL", tt.env)
			if tt.problem != "" {
				problems := problemsOf(t, err)
				if len(problems) != 1 || !strings.Contains(problems[0], tt.problem) {
					t.Fatalf("problems = %q, want one containing %q", problems, tt.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Queue.ReaperInterval != tt.want {
				t.Errorf("reaper interval = %s, want %s", cfg.Queue.ReaperInterval, tt.want)
			}
		})
	}
}
