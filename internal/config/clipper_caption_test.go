package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClipperCaptionDefaults(t *testing.T) {
	transcribeClipperEnv(t)
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Caption, defaultClipperCaption()) {
		t.Errorf("caption defaults:\n got %+v\nwant %+v", cfg.Caption, defaultClipperCaption())
	}
	// The font the clipper base image installs (deploy/clipper-base.Dockerfile).
	if cfg.Caption.Font != "Montserrat ExtraBold" {
		t.Errorf("default font %q is not the one in the image", cfg.Caption.Font)
	}
}

func TestClipperCaptionFromEnv(t *testing.T) {
	transcribeClipperEnv(t)
	t.Setenv("CLIPPER_CAPTION_FONT", "DejaVu Sans")
	t.Setenv("CLIPPER_CAPTION_FONT_SIZE", "64")
	t.Setenv("CLIPPER_CAPTION_PRIMARY_COLOR", "#eeeeee")
	t.Setenv("CLIPPER_CAPTION_HIGHLIGHT_COLOR", "#00FF00")
	t.Setenv("CLIPPER_CAPTION_HIGHLIGHT_SCALE", "100")
	t.Setenv("CLIPPER_CAPTION_OUTLINE", "0")
	t.Setenv("CLIPPER_CAPTION_OUTLINE_COLOR", "#102030")
	t.Setenv("CLIPPER_CAPTION_SHADOW", "0")
	t.Setenv("CLIPPER_CAPTION_MARGIN_BOTTOM", "700")
	t.Setenv("CLIPPER_CAPTION_MAX_WORDS", "5")
	t.Setenv("CLIPPER_CAPTION_MAX_CHARS", "30")
	t.Setenv("CLIPPER_CAPTION_MAX_GAP", "1s")
	t.Setenv("CLIPPER_CAPTION_UPPERCASE", "false")

	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperCaption{
		Font:           "DejaVu Sans",
		FontSize:       64,
		PrimaryColor:   "#eeeeee",
		HighlightColor: "#00FF00",
		HighlightScale: 100,
		Outline:        0,
		OutlineColor:   "#102030",
		Shadow:         0,
		MarginBottom:   700,
		MaxWords:       5,
		MaxChars:       30,
		MaxGap:         time.Second,
		Uppercase:      false,
	}
	if !reflect.DeepEqual(cfg.Caption, want) {
		t.Errorf("got\n%+v\nwant\n%+v", cfg.Caption, want)
	}
}

func TestClipperCaptionFromFile(t *testing.T) {
	transcribeClipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  caption:
    font_size: 96
    highlight_color: "#FF00FF"
    max_words: 2
    max_gap: 250ms
    uppercase: false
`)
	cfg, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultClipperCaption()
	want.FontSize = 96
	want.HighlightColor = "#FF00FF"
	want.MaxWords = 2
	want.MaxGap = 250 * time.Millisecond
	want.Uppercase = false
	if !reflect.DeepEqual(cfg.Caption, want) {
		t.Errorf("got\n%+v\nwant\n%+v", cfg.Caption, want)
	}
}

func TestClipperCaptionValidation(t *testing.T) {
	transcribeClipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  caption:
    font: "Comic, Sans"
    primary_color: "white"
    highlight_color: "#FFF"
    outline_color: "000000"
    font_size: 5
    highlight_scale: 90
    outline: -1
    shadow: 51
    margin_bottom: 1900
    max_words: 0
    max_chars: 0
    max_gap: -1s
`)
	_, err := LoadClipper(path)
	got := problemsOf(t, err)
	wants := []string{
		`clipper.caption.font (CLIPPER_CAPTION_FONT) must not contain a comma or a line break: got "Comic, Sans"`,
		`clipper.caption.primary_color (CLIPPER_CAPTION_PRIMARY_COLOR) must be a colour written #RRGGBB: got "white"`,
		`clipper.caption.highlight_color (CLIPPER_CAPTION_HIGHLIGHT_COLOR) must be a colour written #RRGGBB: got "#FFF"`,
		`clipper.caption.outline_color (CLIPPER_CAPTION_OUTLINE_COLOR) must be a colour written #RRGGBB: got "000000"`,
		"clipper.caption.font_size (CLIPPER_CAPTION_FONT_SIZE) must be between 10 and 400: got 5",
		"clipper.caption.highlight_scale (CLIPPER_CAPTION_HIGHLIGHT_SCALE) must be between 100 and 200: got 90",
		"clipper.caption.outline (CLIPPER_CAPTION_OUTLINE) must be between 0 and 50: got -1",
		"clipper.caption.shadow (CLIPPER_CAPTION_SHADOW) must be between 0 and 50: got 51",
		"clipper.caption.margin_bottom (CLIPPER_CAPTION_MARGIN_BOTTOM) must be between 0 and 1800: got 1900",
		"clipper.caption.max_words (CLIPPER_CAPTION_MAX_WORDS) must be between 1 and 50: got 0",
		"clipper.caption.max_chars (CLIPPER_CAPTION_MAX_CHARS) must be between 1 and 500: got 0",
		"clipper.caption.max_gap (CLIPPER_CAPTION_MAX_GAP) must not be negative: got -1s",
	}
	if len(got) != len(wants) {
		t.Fatalf("want %d problems, got %d:\n%s", len(wants), len(got), strings.Join(got, "\n"))
	}
	for i, w := range wants {
		if got[i] != w {
			t.Errorf("problem %d:\n got %q\nwant %q", i, got[i], w)
		}
	}
}

func TestClipperCaptionEmptyFont(t *testing.T) {
	transcribeClipperEnv(t)
	path := writeFile(t, "config.yaml", "clipper:\n  caption:\n    font: \"  \"\n")
	_, err := LoadClipper(path)
	got := problemsOf(t, err)
	if len(got) != 1 || !strings.Contains(got[0], "clipper.caption.font (CLIPPER_CAPTION_FONT) must not be empty") {
		t.Errorf("got %v", got)
	}
}

func TestClipperCaptionExampleMatchesDefaults(t *testing.T) {
	transcribeClipperEnv(t)
	cfg, err := LoadClipper(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Caption, defaultClipperCaption()) {
		t.Errorf("config.example.yaml differs from the defaults:\n got %+v\nwant %+v", cfg.Caption, defaultClipperCaption())
	}
}
