package config

import (
	"strings"
	"testing"
)

func TestScanSignalsAreOnByDefault(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Scan.ChatSpikes || !cfg.Scan.ViewerClips {
		t.Errorf("chat_spikes %v, viewer_clips %v; want both on by default", cfg.Scan.ChatSpikes, cfg.Scan.ViewerClips)
	}
}

func TestScanSignalsFromFileAndEnv(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  scan:
    chat_spikes: false
`)
	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scan.ChatSpikes || !cfg.Scan.ViewerClips {
		t.Errorf("from file: chat_spikes %v, viewer_clips %v; want off, on", cfg.Scan.ChatSpikes, cfg.Scan.ViewerClips)
	}

	// The environment overrides the file.
	t.Setenv("POGFARM_SCAN_CHAT_SPIKES", "true")
	t.Setenv("POGFARM_SCAN_VIEWER_CLIPS", "false")
	cfg, err = LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Scan.ChatSpikes || cfg.Scan.ViewerClips {
		t.Errorf("from env: chat_spikes %v, viewer_clips %v; want on, off", cfg.Scan.ChatSpikes, cfg.Scan.ViewerClips)
	}
}

func TestScanSignalsBothOffIsRejected(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_SCAN_CHAT_SPIKES", "false")
	t.Setenv("POGFARM_SCAN_VIEWER_CLIPS", "false")

	_, err := LoadPogfarm("")
	got := problemsOf(t, err)
	if len(got) != 1 || !strings.Contains(got[0], "pogfarm.scan.chat_spikes") || !strings.Contains(got[0], "pogfarm.scan.viewer_clips") {
		t.Errorf("want one problem naming both keys, got %q", got)
	}
}

func TestScanSignalEnvNotABool(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_SCAN_VIEWER_CLIPS", "sometimes")

	_, err := LoadPogfarm("")
	if got := strings.Join(problemsOf(t, err), "\n"); !strings.Contains(got, "POGFARM_SCAN_VIEWER_CLIPS: not true or false") {
		t.Errorf("unexpected problems: %s", got)
	}
}
