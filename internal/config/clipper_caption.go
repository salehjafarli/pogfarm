package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ClipperCaption is the "clipper.caption" section: how the words of a clip
// become the subtitle file burned into it (package internal/clipper/caption).
//
// Sizes and positions are in pixels of the 1080x1920 output frame, the
// subtitle file's PlayResX and PlayResY, so they keep their proportions
// whatever the renderer does. Colours are written "#RRGGBB"; the caption
// package converts them to ASS's &HAABBGGRR form.
type ClipperCaption struct {
	// Font is the font family or full name, as fontconfig knows it. The
	// clipper base image ships "Montserrat ExtraBold". When the font is
	// missing, libass falls back to another one.
	Font string `yaml:"font" env:"CLIPPER_CAPTION_FONT"`
	// FontSize is the font size in pixels of the 1920-pixel-high frame.
	FontSize int `yaml:"font_size" env:"CLIPPER_CAPTION_FONT_SIZE"`
	// PrimaryColor is the colour of the words, "#RRGGBB".
	PrimaryColor string `yaml:"primary_color" env:"CLIPPER_CAPTION_PRIMARY_COLOR"`
	// HighlightColor is the colour of the word being spoken, "#RRGGBB".
	HighlightColor string `yaml:"highlight_color" env:"CLIPPER_CAPTION_HIGHLIGHT_COLOR"`
	// HighlightScale is the size of the word being spoken, in percent of
	// the others. 100 turns the scaling off.
	HighlightScale int `yaml:"highlight_scale" env:"CLIPPER_CAPTION_HIGHLIGHT_SCALE"`
	// Outline is the width of the outline around each letter, in pixels.
	Outline int `yaml:"outline" env:"CLIPPER_CAPTION_OUTLINE"`
	// OutlineColor is the colour of the outline, "#RRGGBB". The shadow
	// uses the same colour at half opacity.
	OutlineColor string `yaml:"outline_color" env:"CLIPPER_CAPTION_OUTLINE_COLOR"`
	// Shadow is the offset of the drop shadow, in pixels. 0 turns it off.
	Shadow int `yaml:"shadow" env:"CLIPPER_CAPTION_SHADOW"`
	// MarginBottom is the distance from the bottom of the frame to the
	// bottom of the caption, in pixels. The captions are centred
	// horizontally.
	MarginBottom int `yaml:"margin_bottom" env:"CLIPPER_CAPTION_MARGIN_BOTTOM"`
	// MaxWords is the largest number of words in one phrase.
	MaxWords int `yaml:"max_words" env:"CLIPPER_CAPTION_MAX_WORDS"`
	// MaxChars is the largest number of characters in one phrase, counting
	// the spaces between words. A single word longer than this is a phrase
	// of its own.
	MaxChars int `yaml:"max_chars" env:"CLIPPER_CAPTION_MAX_CHARS"`
	// MaxGap is the longest pause between two words of one phrase. A
	// longer pause starts a new phrase.
	MaxGap time.Duration `yaml:"max_gap" env:"CLIPPER_CAPTION_MAX_GAP"`
	// Uppercase writes the captions in capital letters.
	Uppercase bool `yaml:"uppercase" env:"CLIPPER_CAPTION_UPPERCASE"`
}

// defaultClipperCaption is the caption section when nothing is configured:
// white words with a yellow highlight and a black outline, in the font of
// the clipper base image, a little below the middle of the lower third.
func defaultClipperCaption() ClipperCaption {
	return ClipperCaption{
		Font:           "Montserrat ExtraBold",
		FontSize:       80,
		PrimaryColor:   "#FFFFFF",
		HighlightColor: "#FFE135",
		HighlightScale: 110,
		Outline:        6,
		OutlineColor:   "#000000",
		Shadow:         2,
		MarginBottom:   560,
		MaxWords:       3,
		MaxChars:       18,
		MaxGap:         500 * time.Millisecond,
		Uppercase:      true,
	}
}

// captionColor matches a colour written "#RRGGBB".
var captionColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func (c ClipperCaption) validate() []string {
	var problems []string
	switch {
	case strings.TrimSpace(c.Font) == "":
		problems = append(problems, "clipper.caption.font (CLIPPER_CAPTION_FONT) must not be empty")
	case strings.ContainsAny(c.Font, ",\r\n"):
		problems = append(problems, fmt.Sprintf(
			"clipper.caption.font (CLIPPER_CAPTION_FONT) must not contain a comma or a line break: got %q", c.Font))
	}
	colors := []struct {
		key, env, value string
	}{
		{"primary_color", "PRIMARY_COLOR", c.PrimaryColor},
		{"highlight_color", "HIGHLIGHT_COLOR", c.HighlightColor},
		{"outline_color", "OUTLINE_COLOR", c.OutlineColor},
	}
	for _, col := range colors {
		if !captionColor.MatchString(col.value) {
			problems = append(problems, fmt.Sprintf(
				"clipper.caption.%s (CLIPPER_CAPTION_%s) must be a colour written #RRGGBB: got %q", col.key, col.env, col.value))
		}
	}
	ints := []struct {
		key, env      string
		value, lo, hi int
	}{
		{"font_size", "FONT_SIZE", c.FontSize, 10, 400},
		{"highlight_scale", "HIGHLIGHT_SCALE", c.HighlightScale, 100, 200},
		{"outline", "OUTLINE", c.Outline, 0, 50},
		{"shadow", "SHADOW", c.Shadow, 0, 50},
		{"margin_bottom", "MARGIN_BOTTOM", c.MarginBottom, 0, 1800},
		{"max_words", "MAX_WORDS", c.MaxWords, 1, 50},
		{"max_chars", "MAX_CHARS", c.MaxChars, 1, 500},
	}
	for _, n := range ints {
		if n.value < n.lo || n.value > n.hi {
			problems = append(problems, fmt.Sprintf(
				"clipper.caption.%s (CLIPPER_CAPTION_%s) must be between %d and %d: got %d", n.key, n.env, n.lo, n.hi, n.value))
		}
	}
	if c.MaxGap < 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.caption.max_gap (CLIPPER_CAPTION_MAX_GAP) must not be negative: got %s", c.MaxGap))
	}
	return problems
}
