package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClipperRenderDefaults(t *testing.T) {
	clipperEnv(t)
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperRender{
		FFmpegPath:       "ffmpeg",
		FFprobePath:      "ffprobe",
		Preset:           "fast",
		CRF:              21,
		FrameRate:        30,
		MaxBytes:         45_000_000,
		TimeoutBase:      2 * time.Minute,
		TimeoutPerSecond: 10 * time.Second,
	}
	if cfg.Render != want {
		t.Errorf("render defaults:\n got %+v\nwant %+v", cfg.Render, want)
	}
	// Telegram's bot upload limit is 50 MB; whether that means 10^6 or 2^20
	// bytes, the default is below it.
	if cfg.Render.MaxBytes >= 50_000_000 {
		t.Errorf("max_bytes default %d is not safely under 50 MB", cfg.Render.MaxBytes)
	}
}

func TestClipperRenderFromEnv(t *testing.T) {
	clipperEnv(t)
	t.Setenv("CLIPPER_RENDER_FFMPEG_PATH", "/usr/bin/ffmpeg")
	t.Setenv("CLIPPER_RENDER_FFPROBE_PATH", "/usr/bin/ffprobe")
	t.Setenv("CLIPPER_RENDER_PRESET", "veryfast")
	t.Setenv("CLIPPER_RENDER_CRF", "23")
	t.Setenv("CLIPPER_RENDER_FRAME_RATE", "60")
	t.Setenv("CLIPPER_RENDER_MAX_BYTES", "20000000")
	t.Setenv("CLIPPER_RENDER_TIMEOUT_BASE", "1m")
	t.Setenv("CLIPPER_RENDER_TIMEOUT_PER_SECOND", "4s")
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperRender{
		FFmpegPath:       "/usr/bin/ffmpeg",
		FFprobePath:      "/usr/bin/ffprobe",
		Preset:           "veryfast",
		CRF:              23,
		FrameRate:        60,
		MaxBytes:         20_000_000,
		TimeoutBase:      time.Minute,
		TimeoutPerSecond: 4 * time.Second,
	}
	if cfg.Render != want {
		t.Errorf("got %+v, want %+v", cfg.Render, want)
	}
}

func TestClipperRenderFromFile(t *testing.T) {
	clipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  render:
    preset: slow
    crf: 18
`)
	cfg, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultClipperRender()
	want.Preset = "slow"
	want.CRF = 18
	if cfg.Render != want {
		t.Errorf("got %+v, want %+v", cfg.Render, want)
	}
}

func TestClipperRenderValidation(t *testing.T) {
	clipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  render:
    ffmpeg_path: " "
    ffprobe_path: ""
    preset: turbo
    crf: 52
    frame_rate: 5
    max_bytes: 1000
    timeout_base: 0s
    timeout_per_second: -1s
`)
	_, err := LoadClipper(path)
	got := problemsOf(t, err)
	want := []string{
		"clipper.render.ffmpeg_path (CLIPPER_RENDER_FFMPEG_PATH) must not be empty",
		"clipper.render.ffprobe_path (CLIPPER_RENDER_FFPROBE_PATH) must not be empty",
		"clipper.render.preset (CLIPPER_RENDER_PRESET) must be one of",
		"clipper.render.crf (CLIPPER_RENDER_CRF) must be between 0 and 51: got 52",
		"clipper.render.frame_rate (CLIPPER_RENDER_FRAME_RATE) must be between 10 and 60: got 5",
		"clipper.render.max_bytes (CLIPPER_RENDER_MAX_BYTES) must be at least 1048576 bytes: got 1000",
		"clipper.render.timeout_base (CLIPPER_RENDER_TIMEOUT_BASE) must be a positive duration",
		"clipper.render.timeout_per_second (CLIPPER_RENDER_TIMEOUT_PER_SECOND) must be a positive duration",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d:\n got %q\nwant prefix %q", i, got[i], want[i])
		}
	}
}

func TestClipperRenderBounds(t *testing.T) {
	for _, mod := range []func(*ClipperRender){
		func(r *ClipperRender) { r.CRF = 0 },
		func(r *ClipperRender) { r.CRF = 51 },
		func(r *ClipperRender) { r.FrameRate = 10 },
		func(r *ClipperRender) { r.FrameRate = 60 },
		func(r *ClipperRender) { r.MaxBytes = MinRenderMaxBytes },
		func(r *ClipperRender) { r.Preset = "ultrafast" },
		func(r *ClipperRender) { r.Preset = "veryslow" },
	} {
		r := defaultClipperRender()
		mod(&r)
		if p := r.Validate(); p != nil {
			t.Errorf("%+v should be accepted: %q", r, p)
		}
	}
}

func TestClipperRenderExampleIsDefault(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CLIPPER_NAME", "example-node")
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	cfg, err := LoadClipper(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Render != defaultClipperRender() {
		t.Errorf("the example file should show the default: got %+v, want %+v", cfg.Render, defaultClipperRender())
	}
	if cfg.WebcamDetection != defaultClipperWebcamDetection() {
		t.Errorf("the example file should show the default: got %+v, want %+v", cfg.WebcamDetection, defaultClipperWebcamDetection())
	}
}

// TestLoadClipperRenderWithoutPogfarm checks that clipper render loads a
// configuration without pogfarm's address or secret, which LoadClipper
// refuses.
func TestLoadClipperRenderWithoutPogfarm(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CLIPPER_NAME", "")
	path := writeFile(t, "config.yaml", `
clipper:
  render:
    crf: 19
`)
	if _, err := LoadClipper(path); err == nil {
		t.Fatal("LoadClipper accepted a config without pogfarm_url and api_secret")
	}
	cfg, err := LoadClipperRender(path)
	if err != nil {
		t.Fatalf("LoadClipperRender: %v", err)
	}
	if cfg.Render.CRF != 19 || cfg.Download != defaultClipperDownload() {
		t.Errorf("file and defaults not applied: %+v", cfg)
	}

	// The example file, with its placeholder secret, loads as it is.
	if _, err := LoadClipperRender(filepath.Join("..", "..", "config.example.yaml")); err != nil {
		t.Errorf("example file: %v", err)
	}
}

func TestLoadClipperRenderChecksSteps(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CLIPPER_RENDER_CRF", "99")
	t.Setenv("CLIPPER_DOWNLOAD_QUALITY", " ")
	t.Setenv("CLIPPER_CAPTION_FONT_SIZE", "nope")
	_, err := LoadClipperRender("")
	got := problemsOf(t, err)
	want := []string{
		"CLIPPER_CAPTION_FONT_SIZE: not a whole number",
		"clipper.download.quality (CLIPPER_DOWNLOAD_QUALITY) must not be empty",
		"clipper.render.crf (CLIPPER_RENDER_CRF) must be between 0 and 51: got 99",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d:\n got %q\nwant prefix %q", i, got[i], want[i])
		}
	}
	for _, p := range got {
		if strings.Contains(p, "pogfarm_url") || strings.Contains(p, "api_secret") {
			t.Errorf("render config should not check pogfarm settings: %q", p)
		}
	}
}
