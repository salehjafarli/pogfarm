package caption

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/salehjafarli/pogfarm/internal/clipper/transcribe"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// The script's resolution: that of the output frame.
const (
	PlayResX = 1080
	PlayResY = 1920
)

// marginSides keeps the captions this many pixels away from the left and
// right edges; a phrase too wide for the space left is wrapped by libass.
const marginSides = 60

// Result describes the file Write wrote.
type Result struct {
	// Lines is the number of caption lines (phrases) in the file.
	Lines int
	// Events is the number of Dialogue events: one per word of each
	// phrase, plus one per pause inside a phrase.
	Events int
}

// Writer writes caption files in one style.
type Writer struct {
	cfg config.ClipperCaption
	// Colours in ASS form.
	primary, highlight, outline, shadow string
	highlightTag                        string
}

// New returns a Writer for a validated caption configuration. It fails
// when a colour is not written "#RRGGBB".
func New(cfg config.ClipperCaption) (*Writer, error) {
	w := &Writer{cfg: cfg}
	var err error
	if w.primary, err = styleColor(cfg.PrimaryColor, 0); err != nil {
		return nil, fmt.Errorf("caption: primary colour: %w", err)
	}
	if w.outline, err = styleColor(cfg.OutlineColor, 0); err != nil {
		return nil, fmt.Errorf("caption: outline colour: %w", err)
	}
	// The shadow is the outline colour at half opacity.
	w.shadow, _ = styleColor(cfg.OutlineColor, 0x80)
	if w.highlight, err = tagColor(cfg.HighlightColor); err != nil {
		return nil, fmt.Errorf("caption: highlight colour: %w", err)
	}
	w.highlightTag = `{\c` + w.highlight
	if cfg.HighlightScale != 100 {
		s := strconv.Itoa(cfg.HighlightScale)
		w.highlightTag += `\fscx` + s + `\fscy` + s
	}
	w.highlightTag += "}"
	return w, nil
}

// Write turns the transcript into an ASS file at path, replacing any file
// there, and reports how many caption lines it holds. An empty transcript
// gives a valid file without events. See the package documentation for
// the rules.
func (w *Writer) Write(ctx context.Context, t transcribe.Transcript, path string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	data, res := w.build(t)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Result{}, fmt.Errorf("caption: write %s: %w", path, err)
	}
	return res, nil
}

// word is a transcript word ready to be written.
type word struct {
	text       string // cleaned and escaped
	runes      int    // characters shown, for the max_chars limit
	start, end time.Duration
}

// build returns the file's content.
func (w *Writer) build(t transcribe.Transcript) ([]byte, Result) {
	var b strings.Builder
	w.writeHeader(&b)

	var res Result
	phrases := w.group(w.prepare(t.Words))
	for i, p := range phrases {
		end := p[len(p)-1].end
		if i+1 < len(phrases) {
			end = min(end, phrases[i+1][0].start)
		}
		n := w.writePhrase(&b, p, end)
		if n > 0 {
			res.Lines++
			res.Events += n
		}
	}
	return []byte(b.String()), res
}

func (w *Writer) writeHeader(b *strings.Builder) {
	c := w.cfg
	fmt.Fprintf(b, `[Script Info]
; Captions written by the pogfarm clipper (internal/clipper/caption).
ScriptType: v4.00+
PlayResX: %d
PlayResY: %d
WrapStyle: 0
ScaledBorderAndShadow: yes
YCbCr Matrix: None

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,%s,%d,%s,%s,%s,%s,0,0,0,0,100,100,0,0,1,%d,%d,2,%d,%d,%d,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`, PlayResX, PlayResY,
		strings.TrimSpace(c.Font), c.FontSize, w.primary, w.primary, w.outline, w.shadow,
		c.Outline, c.Shadow, marginSides, marginSides, c.MarginBottom)
}

// prepare cleans, upper-cases and escapes the words, and drops those left
// empty.
func (w *Writer) prepare(words []transcribe.Word) []word {
	out := make([]word, 0, len(words))
	for _, tw := range words {
		text := clean(tw.Text)
		if w.cfg.Uppercase {
			text = strings.ToUpper(text)
		}
		if text == "" {
			continue
		}
		out = append(out, word{
			text:  escape(text),
			runes: utf8.RuneCountInString(text),
			start: tw.Start,
			end:   max(tw.End, tw.Start),
		})
	}
	return out
}

// group splits the words into phrases by count, characters and pauses.
func (w *Writer) group(words []word) [][]word {
	var phrases [][]word
	var cur []word
	chars := 0
	for _, wd := range words {
		if len(cur) > 0 {
			last := cur[len(cur)-1]
			if len(cur) >= w.cfg.MaxWords ||
				chars+1+wd.runes > w.cfg.MaxChars ||
				wd.start-last.end > w.cfg.MaxGap {
				phrases = append(phrases, cur)
				cur, chars = nil, 0
			}
		}
		if len(cur) > 0 {
			chars++ // the space before this word
		}
		cur = append(cur, wd)
		chars += wd.runes
	}
	if len(cur) > 0 {
		phrases = append(phrases, cur)
	}
	return phrases
}

// writePhrase writes the events of one phrase, shown until end, and
// returns how many it wrote. hl is the index of the highlighted word, -1
// for none.
func (w *Writer) writePhrase(b *strings.Builder, p []word, end time.Duration) int {
	n := 0
	emit := func(from, to time.Duration, hl int) {
		cf, ct := centis(from), centis(to)
		if ct <= cf {
			return
		}
		n++
		fmt.Fprintf(b, "Dialogue: 0,%s,%s,Default,,0,0,0,,%s\n", formatCentis(cf), formatCentis(ct), w.phraseText(p, hl))
	}

	start := p[0].start
	end = max(end, start)
	t := start
	for i, wd := range p {
		hs := min(max(wd.start, t), end)
		he := wd.end
		if i+1 < len(p) {
			he = min(he, p[i+1].start)
		}
		he = max(min(he, end), hs)
		emit(t, hs, -1) // the pause before this word, if any
		emit(hs, he, i)
		t = he
	}
	emit(t, end, -1) // the phrase may outlast its last highlight
	return n
}

// phraseText is the phrase's text with word hl highlighted.
func (w *Writer) phraseText(p []word, hl int) string {
	var b strings.Builder
	for i, wd := range p {
		if i > 0 {
			b.WriteByte(' ')
		}
		if i == hl {
			b.WriteString(w.highlightTag)
			b.WriteString(wd.text)
			b.WriteString(`{\r}`)
		} else {
			b.WriteString(wd.text)
		}
	}
	return b.String()
}

// clean turns white space inside a word (line breaks, tabs) into single
// spaces, removes other control characters and trims the result.
func clean(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// wordJoiner is U+2060, an invisible character that stops a backslash
// from being read together with the character after it.
const wordJoiner = "⁠"

// escape makes text safe inside an ASS Dialogue line: braces become
// libass's escapes \{ and \}, and a backslash is followed by a word
// joiner so it never starts an escape or a tag.
func escape(s string) string {
	if !strings.ContainsAny(s, `{}\`) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '{':
			b.WriteString(`\{`)
		case '}':
			b.WriteString(`\}`)
		case '\\':
			b.WriteString(`\` + wordJoiner)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// centis rounds d to the nearest hundredth of a second. Negative times
// become zero.
func centis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + 5*time.Millisecond) / (10 * time.Millisecond))
}

// formatCentis writes a time in hundredths of a second as ASS's H:MM:SS.cc.
func formatCentis(cs int64) string {
	h := cs / 360000
	m := cs / 6000 % 60
	s := cs / 100 % 60
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs%100)
}

// Timestamp formats d as an ASS time, H:MM:SS.cc, rounded to the nearest
// hundredth of a second.
func Timestamp(d time.Duration) string { return formatCentis(centis(d)) }

// parseHex reads "#RRGGBB" into its three components.
func parseHex(hex string) (r, g, b uint8, err error) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, fmt.Errorf("want a colour written #RRGGBB, got %q", hex)
	}
	v, perr := strconv.ParseUint(hex[1:], 16, 32)
	if perr != nil {
		return 0, 0, 0, fmt.Errorf("want a colour written #RRGGBB, got %q", hex)
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), nil
}

// styleColor converts "#RRGGBB" to the style form &HAABBGGRR, with alpha
// as the transparency (0 is opaque).
func styleColor(hex string, alpha uint8) (string, error) {
	r, g, b, err := parseHex(hex)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("&H%02X%02X%02X%02X", alpha, b, g, r), nil
}

// tagColor converts "#RRGGBB" to the override-tag form &HBBGGRR&.
func tagColor(hex string) (string, error) {
	r, g, b, err := parseHex(hex)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("&H%02X%02X%02X&", b, g, r), nil
}
