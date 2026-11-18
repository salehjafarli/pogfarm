package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// transcribeClipperEnv sets what LoadClipper needs besides the transcribe section.
func transcribeClipperEnv(t *testing.T) {
	t.Helper()
	cleanEnv(t)
	t.Setenv("CLIPPER_POGFARM_URL", "http://pogfarm:8080")
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	t.Setenv("CLIPPER_NAME", "node-a")
}

func TestClipperTranscribeDefaults(t *testing.T) {
	transcribeClipperEnv(t)
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Transcribe, defaultClipperTranscribe()) {
		t.Errorf("transcribe defaults:\n got %+v\nwant %+v", cfg.Transcribe, defaultClipperTranscribe())
	}
	// The image's model path, language and compute type
	// (deploy/clipper-base.Dockerfile and its smoke test).
	tr := cfg.Transcribe
	if tr.Model != "/models/whisper/faster-whisper-small" || tr.Language != "en" || tr.ComputeType != "int8" {
		t.Errorf("defaults do not match the clipper base image: %+v", tr)
	}
	if !tr.Aligned() {
		t.Error("English should be aligned by default")
	}
}

func TestClipperTranscribeFromEnv(t *testing.T) {
	transcribeClipperEnv(t)
	t.Setenv("CLIPPER_TRANSCRIBE_WHISPERX_PATH", "/opt/venv/bin/whisperx")
	t.Setenv("CLIPPER_TRANSCRIBE_FFMPEG_PATH", "/usr/bin/ffmpeg")
	t.Setenv("CLIPPER_TRANSCRIBE_MODEL", "small")
	t.Setenv("CLIPPER_TRANSCRIBE_LANGUAGE", "de")
	t.Setenv("CLIPPER_TRANSCRIBE_COMPUTE_TYPE", "float32")
	t.Setenv("CLIPPER_TRANSCRIBE_TIMEOUT", "3m")

	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	want := defaultClipperTranscribe()
	want.WhisperXPath = "/opt/venv/bin/whisperx"
	want.FFmpegPath = "/usr/bin/ffmpeg"
	want.Model = "small"
	want.Language = "de"
	want.ComputeType = "float32"
	want.Timeout = 3 * time.Minute
	if !reflect.DeepEqual(cfg.Transcribe, want) {
		t.Errorf("got\n%+v\nwant\n%+v", cfg.Transcribe, want)
	}
	if cfg.Transcribe.Aligned() {
		t.Error("German has no alignment model by default")
	}
}

func TestClipperTranscribeFromFile(t *testing.T) {
	transcribeClipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  transcribe:
    language: de
    aligned_languages: [en, de]
    timeout: 90s
`)
	cfg, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultClipperTranscribe()
	want.Language = "de"
	want.AlignedLanguages = []string{"en", "de"}
	want.Timeout = 90 * time.Second
	if !reflect.DeepEqual(cfg.Transcribe, want) {
		t.Errorf("got\n%+v\nwant\n%+v", cfg.Transcribe, want)
	}
	if !cfg.Transcribe.Aligned() {
		t.Error("de is listed in aligned_languages")
	}
}

func TestClipperTranscribeValidation(t *testing.T) {
	transcribeClipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  transcribe:
    whisperx_path: ""
    ffmpeg_path: ""
    model: ""
    language: English
    aligned_languages: [en, EN]
    compute_type: float16
    timeout: 0s
`)
	_, err := LoadClipper(path)
	got := problemsOf(t, err)
	wants := []string{
		"clipper.transcribe.whisperx_path",
		"clipper.transcribe.ffmpeg_path",
		"clipper.transcribe.model",
		`clipper.transcribe.language (CLIPPER_TRANSCRIBE_LANGUAGE) must be a Whisper language code such as "en": got "English"`,
		`clipper.transcribe.aligned_languages must hold Whisper language codes such as "en": got "EN"`,
		"float16 needs a GPU",
		"clipper.transcribe.timeout",
	}
	if len(got) != len(wants) {
		t.Fatalf("want %d problems, got %d: %v", len(wants), len(got), got)
	}
	for i, w := range wants {
		if !strings.Contains(got[i], w) {
			t.Errorf("problem %d: want it to contain %q, got %q", i, w, got[i])
		}
	}
}

func TestClipperTranscribeExampleMatchesDefaults(t *testing.T) {
	transcribeClipperEnv(t)
	cfg, err := LoadClipper(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Transcribe, defaultClipperTranscribe()) {
		t.Errorf("config.example.yaml differs from the defaults:\n got %+v\nwant %+v", cfg.Transcribe, defaultClipperTranscribe())
	}
}
