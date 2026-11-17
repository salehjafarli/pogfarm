package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScanDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discovery.Interval != 15*time.Minute {
		t.Errorf("discovery.interval default: got %s, want 15m", cfg.Discovery.Interval)
	}
	if !reflect.DeepEqual(cfg.Scan, defaultScan()) {
		t.Errorf("scan defaults:\n got %+v\nwant %+v", cfg.Scan, defaultScan())
	}
	if cfg.Scan.Workers != 1 {
		t.Errorf("one VOD at a time by default, got %d workers", cfg.Scan.Workers)
	}
}

func TestScanFromEnv(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_DISCOVERY_INTERVAL", "5m")
	t.Setenv("POGFARM_SCAN_INTERVAL", "30s")
	t.Setenv("POGFARM_SCAN_WORKERS", "2")
	t.Setenv("POGFARM_SCAN_MAX_ATTEMPTS", "7")
	t.Setenv("POGFARM_SCAN_RETRY_BACKOFF", "1m")
	t.Setenv("POGFARM_SCAN_MAX_RETRY_BACKOFF", "2h")

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Scan
	if cfg.Discovery.Interval != 5*time.Minute || s.Interval != 30*time.Second || s.Workers != 2 ||
		s.MaxAttempts != 7 || s.RetryBackoff != time.Minute || s.MaxRetryBackoff != 2*time.Hour {
		t.Errorf("env not applied: discovery %s, scan %+v", cfg.Discovery.Interval, s)
	}
}

func TestScanFromFile(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  scan:
    workers: 3
    spike:
      factor: 2.5
      skip_start: 2m
      weights:
        LUL: 2
        "!command": 0
    candidates:
      max_per_vod: 8
`)
	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultScan()
	want.Workers = 3
	want.Spike.Factor = 2.5
	want.Spike.SkipStart = 2 * time.Minute
	want.Spike.Weights = map[string]float64{"LUL": 2, "!command": 0}
	want.Candidates.MaxPerVOD = 8
	if !reflect.DeepEqual(cfg.Scan, want) {
		t.Errorf("got\n%+v\nwant\n%+v", cfg.Scan, want)
	}
}

func TestScanUnknownKeyIsAnError(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  scan:
    spike:
      windw: 5s
`)
	if _, err := LoadPogfarm(path); err == nil || !strings.Contains(err.Error(), "windw") {
		t.Errorf("a misspelt key should be an error, got %v", err)
	}
}

func TestScanValidation(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  discovery:
    interval: 0s
  scan:
    interval: -1s
    workers: 0
    max_attempts: 0
    retry_backoff: 0s
    max_retry_backoff: -1s
    spike:
      window: 10s
      baseline: 15s
      factor: 0.5
      min_messages: 0
      merge_gap: -1s
      skip_start: -1s
      weights:
        "two words": 1
        lol: -1
        Pog: 1
        pog: 2
    candidates:
      pre_roll: -1s
      post_roll: -1s
      clip_padding: -1s
      min_length: 0s
      max_length: -1s
      max_per_vod: 0
      viewer_clip_score: 0
`)
	_, err := LoadPogfarm(path)
	got := problemsOf(t, err)
	want := []string{
		"pogfarm.discovery.interval (POGFARM_DISCOVERY_INTERVAL) must be a positive duration",
		"pogfarm.scan.interval (POGFARM_SCAN_INTERVAL) must be a positive duration",
		"pogfarm.scan.workers (POGFARM_SCAN_WORKERS) must be at least 1",
		"pogfarm.scan.max_attempts (POGFARM_SCAN_MAX_ATTEMPTS) must be at least 1",
		"pogfarm.scan.retry_backoff (POGFARM_SCAN_RETRY_BACKOFF) must be a positive duration",
		"pogfarm.scan.max_retry_backoff (POGFARM_SCAN_MAX_RETRY_BACKOFF) must be at least retry_backoff",
		"pogfarm.scan.spike.baseline must be at least twice the window",
		"pogfarm.scan.spike.factor must be a number of at least 1",
		"pogfarm.scan.spike.min_messages must be at least 1",
		"pogfarm.scan.spike.merge_gap must not be negative",
		"pogfarm.scan.spike.skip_start must not be negative",
		`pogfarm.scan.spike.weights: weight of "lol" must be a number of at least 0`,
		`pogfarm.scan.spike.weights: "pog" is given more than once`,
		`pogfarm.scan.spike.weights: key "two words" must be one word`,
		"pogfarm.scan.candidates.pre_roll must not be negative",
		"pogfarm.scan.candidates.post_roll must not be negative",
		"pogfarm.scan.candidates.clip_padding must not be negative",
		"pogfarm.scan.candidates.min_length must be positive",
		"pogfarm.scan.candidates.max_length must be at least min_length",
		"pogfarm.scan.candidates.max_per_vod must be at least 1",
		"pogfarm.scan.candidates.viewer_clip_score must be a positive number",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d problems, got %d:\n%s", len(want), len(got), strings.Join(got, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("problem %d: got %q, want it to contain %q", i, got[i], w)
		}
	}
}

func TestScanEnvNotADuration(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_SCAN_RETRY_BACKOFF", "later")
	_, err := LoadPogfarm("")
	if got := strings.Join(problemsOf(t, err), "\n"); !strings.Contains(got, "POGFARM_SCAN_RETRY_BACKOFF: not a duration") {
		t.Errorf("unexpected problems: %s", got)
	}
}

// The example file shows the scanner and discovery settings with their
// defaults, so copying it changes nothing.
func TestExampleShowsScanDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_TWITCH_CLIENT_ID", twitchID)
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", twitchSecret)

	cfg, err := LoadPogfarm(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Scan, defaultScan()) {
		t.Errorf("example scan section differs from the defaults:\n got %+v\nwant %+v", cfg.Scan, defaultScan())
	}
	if cfg.Discovery.Interval != defaults().Pogfarm.Discovery.Interval {
		t.Errorf("example discovery.interval %s differs from the default", cfg.Discovery.Interval)
	}
}
