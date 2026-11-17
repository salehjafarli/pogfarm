package config

import (
	"strings"
	"testing"
	"time"
)

// loadCleanup loads a config whose cleanup section holds key: file (left
// out when file is empty), with env set to envValue when that is not empty.
func loadCleanup(t *testing.T, key, file, env, envValue string) (*Pogfarm, error) {
	t.Helper()
	cleanEnv(t)
	content := "pogfarm:\n  api_secret: " + goodSecret + "\n"
	if file != "" {
		content += "  cleanup:\n    " + key + ": " + file + "\n"
	}
	if envValue != "" {
		t.Setenv(env, envValue)
	}
	return LoadPogfarm(writeFile(t, "config.yaml", content))
}

func TestCleanupDurations(t *testing.T) {
	type test struct {
		name    string
		file    string // value of the key; "" leaves the key out
		env     string
		want    time.Duration
		problem string
	}
	keys := []struct {
		key, env string
		get      func(*Pogfarm) time.Duration
		tests    []test
	}{
		{
			key: "interval", env: "POGFARM_CLEANUP_INTERVAL",
			get: func(c *Pogfarm) time.Duration { return c.Cleanup.Interval },
			tests: []test{
				{name: "default", want: 30 * time.Minute},
				{name: "from file", file: "10m", want: 10 * time.Minute},
				{name: "env overrides file", file: "10m", env: "2h", want: 2 * time.Hour},
				{name: "minimum", env: "1m", want: time.Minute},
				{name: "too short", file: "59s", problem: "pogfarm.cleanup.interval (POGFARM_CLEANUP_INTERVAL) must be at least 1m0s: got 59s"},
				{name: "zero", env: "0s", problem: "must be at least 1m0s: got 0s"},
				{name: "negative", file: "-5m", problem: "must be at least 1m0s: got -5m0s"},
				{name: "not a duration", env: "hourly", problem: "POGFARM_CLEANUP_INTERVAL: not a duration"},
			},
		},
		{
			key: "rejected_retention", env: "POGFARM_CLEANUP_REJECTED_RETENTION",
			get: func(c *Pogfarm) time.Duration { return c.Cleanup.RejectedRetention },
			tests: []test{
				{name: "default", want: 24 * time.Hour},
				{name: "from file", file: "48h", want: 48 * time.Hour},
				{name: "env overrides file", file: "48h", env: "6h", want: 6 * time.Hour},
				{name: "minimum", env: "1h", want: time.Hour},
				{name: "too short", file: "59m", problem: "pogfarm.cleanup.rejected_retention (POGFARM_CLEANUP_REJECTED_RETENTION) must be at least 1h0m0s: got 59m0s"},
				{name: "zero", env: "0s", problem: "must be at least 1h0m0s: got 0s"},
				{name: "not a duration", env: "a day", problem: "POGFARM_CLEANUP_REJECTED_RETENTION: not a duration"},
			},
		},
		{
			key: "posted_retention", env: "POGFARM_CLEANUP_POSTED_RETENTION",
			get: func(c *Pogfarm) time.Duration { return c.Cleanup.PostedRetention },
			tests: []test{
				{name: "default", want: 7 * 24 * time.Hour},
				{name: "from file", file: "720h", want: 720 * time.Hour},
				{name: "env overrides file", file: "720h", env: "24h", want: 24 * time.Hour},
				{name: "minimum", env: "1h", want: time.Hour},
				{name: "too short", file: "30m", problem: "pogfarm.cleanup.posted_retention (POGFARM_CLEANUP_POSTED_RETENTION) must be at least 1h0m0s: got 30m0s"},
				{name: "negative", env: "-1h", problem: "must be at least 1h0m0s: got -1h0m0s"},
				{name: "not a duration", env: "a week", problem: "POGFARM_CLEANUP_POSTED_RETENTION: not a duration"},
			},
		},
	}
	for _, k := range keys {
		for _, tt := range k.tests {
			t.Run(k.key+"/"+tt.name, func(t *testing.T) {
				cfg, err := loadCleanup(t, k.key, tt.file, k.env, tt.env)
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
				if got := k.get(cfg); got != tt.want {
					t.Errorf("%s = %s, want %s", k.key, got, tt.want)
				}
			})
		}
	}
}
