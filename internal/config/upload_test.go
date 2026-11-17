package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUploadDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Upload, defaultUpload()) {
		t.Errorf("upload defaults:\n got %+v\nwant %+v", cfg.Upload, defaultUpload())
	}
	if got := cfg.Upload.EnabledTargets(); len(got) != 0 {
		t.Errorf("no targets by default, got %q", got)
	}
}

func TestUploadFromEnv(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_UPLOAD_MAX_ATTEMPTS", "3")
	t.Setenv("POGFARM_UPLOAD_RETRY_BACKOFF", "1m")
	t.Setenv("POGFARM_UPLOAD_MAX_RETRY_BACKOFF", "1h")
	t.Setenv("POGFARM_UPLOAD_POLL_INTERVAL", "10s")
	t.Setenv("POGFARM_UPLOAD_ATTEMPT_TIMEOUT", "5m")

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	want := Upload{MaxAttempts: 3, RetryBackoff: time.Minute, MaxRetryBackoff: time.Hour,
		PollInterval: 10 * time.Second, AttemptTimeout: 5 * time.Minute}
	if !reflect.DeepEqual(cfg.Upload, want) {
		t.Errorf("got %+v, want %+v", cfg.Upload, want)
	}
}

func TestUploadTargetsFromFile(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  upload:
    targets:
      - name: youtube
        daily_cap: 5
        min_spacing: 2h
      - name: tiktok
        enabled: false
        daily_cap: 3
      - name: fake
        enabled: true
        daily_cap: 1
`)
	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Upload.EnabledTargets(), []string{"youtube", "fake"}; !reflect.DeepEqual(got, want) {
		t.Errorf("enabled targets: got %q, want %q", got, want)
	}
	if yt := cfg.Upload.Targets[0]; yt.DailyCap != 5 || yt.MinSpacing != 2*time.Hour || !yt.On() {
		t.Errorf("youtube: %+v", yt)
	}
}

func TestUploadValidation(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  upload:
    max_attempts: 0
    retry_backoff: 0s
    max_retry_backoff: -1s
    poll_interval: 0s
    attempt_timeout: -1m
    targets:
      - daily_cap: 1
      - name: YouTube
        daily_cap: 1
      - name: yt
        daily_cap: 0
      - name: yt
        daily_cap: 1
        min_spacing: -1s
`)
	_, err := LoadPogfarm(path)
	got := problemsOf(t, err)
	want := []string{
		"pogfarm.upload.max_attempts",
		"pogfarm.upload.retry_backoff",
		"pogfarm.upload.max_retry_backoff",
		"pogfarm.upload.poll_interval",
		"pogfarm.upload.attempt_timeout",
		"pogfarm.upload.targets[0].name is required",
		`pogfarm.upload.targets[1].name "YouTube" must be lower-case`,
		"pogfarm.upload.targets[2].daily_cap must be at least 1",
		`pogfarm.upload.targets[3].name "yt" is listed more than once`,
		"pogfarm.upload.targets[3].min_spacing must not be negative",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("problem %d: got %q, want it to contain %q", i, got[i], want[i])
		}
	}
}

func TestUploadExampleTargetsLoad(t *testing.T) {
	// The targets config.example.yaml shows commented out, uncommented.
	cleanEnv(t)
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	uncommented := 0
	in := false
	for _, line := range strings.Split(string(example), "\n") {
		switch {
		case strings.HasPrefix(line, "    # targets:"):
			in = true
		case in && !strings.HasPrefix(line, "    #"):
			in = false
		}
		if in {
			line = strings.Replace(line, "    # ", "    ", 1)
			uncommented++
		}
		lines = append(lines, line)
	}
	if uncommented == 0 {
		t.Fatal("config.example.yaml has no commented-out upload targets block")
	}
	path := writeFile(t, "config.yaml", strings.Join(lines, "\n"))
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_TWITCH_CLIENT_ID", twitchID)
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", twitchSecret)

	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Upload.EnabledTargets(); !reflect.DeepEqual(got, []string{"youtube"}) {
		t.Errorf("enabled targets: got %q", got)
	}
}
