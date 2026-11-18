package caption

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/transcribe"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// defaults is the caption section's default configuration, as loaded from
// an empty config file.
func defaults(t *testing.T) config.ClipperCaption {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("clipper:\n  pogfarm_url: http://pogfarm:8080\n  api_secret: \"0123456789abcdef0123456789abcdef\"\n  name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Caption
}

func newWriter(t *testing.T, cfg config.ClipperCaption) *Writer {
	t.Helper()
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// words builds a word list from text and [start, end] pairs in
// milliseconds.
func words(text string, times ...int) []transcribe.Word {
	fields := strings.Fields(text)
	if len(times) != 2*len(fields) {
		panic("words: need a start and an end per word")
	}
	out := make([]transcribe.Word, len(fields))
	for i, f := range fields {
		out[i] = transcribe.Word{Text: f, Start: ms(times[2*i]), End: ms(times[2*i+1])}
	}
	return out
}

// steady returns n words of the given texts (cycled), each 200 ms long
// with 50 ms between them, starting at from milliseconds.
func steady(from int, texts ...string) []transcribe.Word {
	out := make([]transcribe.Word, len(texts))
	for i, s := range texts {
		start := from + i*250
		out[i] = transcribe.Word{Text: s, Start: ms(start), End: ms(start + 200)}
	}
	return out
}

type goldenCase struct {
	name  string
	cfg   func(*config.ClipperCaption)
	words []transcribe.Word
	lines int
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{
			// The opening of the base image's sample speech, with the times
			// WhisperX gives it; mixed case, default limits.
			name: "normal",
			cfg:  func(c *config.ClipperCaption) { c.Uppercase = false },
			words: words("And so, my fellow Americans, ask not what your country can do for you.",
				421, 541, 642, 962, 1302, 1442, 1522, 1802, 1882, 2563,
				3404, 3604, 3664, 3884, 3964, 4144, 4224, 4384, 4444, 4945,
				5345, 5525, 5605, 5745, 5805, 6046, 7193, 7594),
			// "And so, my" (3 words) | "fellow Americans," (characters) |
			// "ask not what" (pause) | "your country can" | "do for" | "you."
			lines: 6,
		},
		{
			// Upper-case on (the default), including letters outside ASCII.
			name:  "uppercase",
			words: words("Ça va? Straße élan ok", 0, 300, 300, 600, 700, 1100, 1200, 1500, 1500, 1700),
			lines: 2,
		},
		{
			// Short words with no pauses: only the word count splits them.
			name: "split_by_count",
			cfg: func(c *config.ClipperCaption) {
				c.MaxWords = 2
				c.MaxChars = 100
				c.Uppercase = false
			},
			words: steady(1000, "one", "two", "three", "four", "five", "six", "seven"),
			lines: 4,
		},
		{
			// Long words: only the character count splits them, and a word
			// longer than the limit is a phrase of its own.
			name: "split_by_chars",
			cfg: func(c *config.ClipperCaption) {
				c.MaxWords = 10
				c.MaxChars = 12
				c.Uppercase = false
			},
			words: steady(0, "absolutely", "no", "way", "incomprehensibilities", "is", "a", "word"),
			lines: 4,
		},
		{
			// Pauses: a gap longer than max_gap starts a new phrase, one
			// equal to it does not. Inside a phrase a pause shows the phrase
			// without a highlight.
			name: "split_by_pause",
			cfg: func(c *config.ClipperCaption) {
				c.MaxWords = 10
				c.MaxChars = 100
				c.MaxGap = 500 * time.Millisecond
				c.Uppercase = false
			},
			words: words("wait for it ... now go",
				0, 200, 300, 500, 1000, 1200, // 500 ms gap: same phrase
				1800, 2000, // 600 ms gap: new phrase
				4000, 4200, 4300, 4500),
			lines: 3,
		},
		{
			// Characters with a meaning in ASS, and line breaks inside words.
			name: "escaping",
			cfg:  func(c *config.ClipperCaption) { c.Uppercase = false },
			words: []transcribe.Word{
				{Text: `{\b1}bold`, Start: ms(0), End: ms(400)},
				{Text: `back\slash`, Start: ms(400), End: ms(800)},
				{Text: `\N`, Start: ms(800), End: ms(1000)},
				{Text: "line\nbreak", Start: ms(1000), End: ms(1400)},
				{Text: "cr\r\nlf\ttab", Start: ms(1400), End: ms(1800)},
				{Text: "}", Start: ms(1800), End: ms(1900)},
				{Text: `end\`, Start: ms(1900), End: ms(2300)},
				{Text: "\n\x00", Start: ms(2300), End: ms(2400)}, // nothing left: dropped
				{Text: "a,b;c", Start: ms(2400), End: ms(2600)},
			},
			lines: 5,
		},
		{
			// Non-Latin scripts and emoji are written unchanged.
			name: "unicode",
			cfg: func(c *config.ClipperCaption) {
				c.Uppercase = false
				c.MaxChars = 30
			},
			words: words("Grüße aus Köln 日本語 です 🔥🔥 Ελληνικά שלום",
				0, 300, 300, 500, 500, 900, 1000, 1400, 1400, 1600,
				1700, 1900, 2000, 2400, 2400, 2800),
			lines: 3,
		},
		{
			name:  "empty",
			words: []transcribe.Word{},
			lines: 0,
		},
	}
}

func buildCase(t *testing.T, c goldenCase) ([]byte, Result) {
	t.Helper()
	cfg := defaults(t)
	if c.cfg != nil {
		c.cfg(&cfg)
	}
	path := filepath.Join(t.TempDir(), "captions.ass")
	res, err := newWriter(t, cfg).Write(context.Background(), transcribe.Transcript{Language: "en", Words: c.words}, path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data, res
}

func TestGolden(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			got, res := buildCase(t, c)
			golden := filepath.Join("testdata", c.name+".ass")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading the golden file (run the test with -update to create it): %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("output differs from %s (run the test with -update to accept)\n--- got\n%s--- want\n%s", golden, got, want)
			}
			if res.Lines != c.lines {
				t.Errorf("lines: got %d, want %d", res.Lines, c.lines)
			}
			if n := strings.Count(string(got), "\nDialogue: "); res.Events != n {
				t.Errorf("events: reported %d, file has %d", res.Events, n)
			}
			checkOneAtATime(t, string(got))
		})
	}
}

var dialogue = regexp.MustCompile(`(?m)^Dialogue: 0,(\d+:\d\d:\d\d\.\d\d),(\d+:\d\d:\d\d\.\d\d),Default,,0,0,0,,(.*)$`)

// checkOneAtATime checks that every event lasts a while and that no two
// events are visible at the same time.
func checkOneAtATime(t *testing.T, file string) {
	t.Helper()
	var prevEnd string
	for _, m := range dialogue.FindAllStringSubmatch(file, -1) {
		start, end := m[1], m[2]
		// The fixed-width parts compare as strings for times under 10 h.
		if end <= start {
			t.Errorf("event %s-%s is empty or inverted", start, end)
		}
		if start < prevEnd {
			t.Errorf("event at %s starts before the previous one ends at %s", start, prevEnd)
		}
		prevEnd = end
	}
}

func TestEmptyTranscriptHasNoEvents(t *testing.T) {
	for _, ws := range [][]transcribe.Word{nil, {}} {
		got, res := buildCase(t, goldenCase{words: ws})
		s := string(got)
		if res != (Result{}) {
			t.Errorf("result: %+v", res)
		}
		if strings.Contains(s, "Dialogue:") {
			t.Errorf("unexpected events:\n%s", s)
		}
		for _, part := range []string{"[Script Info]", "PlayResX: 1080", "PlayResY: 1920", "[V4+ Styles]", "Style: Default,Montserrat ExtraBold,80,", "[Events]\nFormat: "} {
			if !strings.Contains(s, part) {
				t.Errorf("missing %q in:\n%s", part, s)
			}
		}
	}
}

func TestHighlightTag(t *testing.T) {
	cfg := defaults(t)
	cfg.HighlightColor = "#112233"
	w := newWriter(t, cfg)
	if want := `{\c&H332211&\fscx110\fscy110}`; w.highlightTag != want {
		t.Errorf("got %q, want %q", w.highlightTag, want)
	}
	cfg.HighlightScale = 100
	w = newWriter(t, cfg)
	if want := `{\c&H332211&}`; w.highlightTag != want {
		t.Errorf("scale 100: got %q, want %q", w.highlightTag, want)
	}
}

// Overlapping words and words without duration: the phrase ends where the
// next one starts, highlights never overlap, and a phrase that rounds to
// no time at all is not written.
func TestOverlapsAndZeroLengthWords(t *testing.T) {
	cfg := defaults(t)
	cfg.MaxWords = 2
	cfg.Uppercase = false
	tr := []transcribe.Word{
		{Text: "a", Start: ms(0), End: ms(700)},     // overlaps "b"
		{Text: "b", Start: ms(500), End: ms(1200)},  // overlaps the next phrase
		{Text: "c", Start: ms(1000), End: ms(1500)}, // phrase 2
		{Text: "d", Start: ms(1500), End: ms(1500)}, // zero length, end of phrase 2
		{Text: "3", Start: ms(3000), End: ms(3000)}, // phrase 3: no time
		{Text: "2", Start: ms(3000), End: ms(3000)},
		{Text: "x", Start: ms(3002), End: ms(3004)}, // phrase 4: rounds to nothing
	}
	got, res := buildCase(t, goldenCase{cfg: func(c *config.ClipperCaption) { *c = cfg }, words: tr})
	want := strings.Join([]string{
		`Dialogue: 0,0:00:00.00,0:00:00.50,Default,,0,0,0,,{\c&H35E1FF&\fscx110\fscy110}a{\r} b`,
		`Dialogue: 0,0:00:00.50,0:00:01.00,Default,,0,0,0,,a {\c&H35E1FF&\fscx110\fscy110}b{\r}`,
		`Dialogue: 0,0:00:01.00,0:00:01.50,Default,,0,0,0,,{\c&H35E1FF&\fscx110\fscy110}c{\r} d`,
		"",
	}, "\n")
	s := string(got)
	if i := strings.Index(s, "Dialogue:"); i < 0 || s[i:] != want {
		t.Errorf("events:\n%s\nwant:\n%s", s[max(i, 0):], want)
	}
	if res != (Result{Lines: 2, Events: 3}) {
		t.Errorf("result: %+v", res)
	}
}

func TestTimestamp(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0:00:00.00"},
		{-time.Second, "0:00:00.00"},
		{ms(4), "0:00:00.00"},
		{ms(5), "0:00:00.01"},
		{ms(421), "0:00:00.42"},
		{ms(995), "0:00:01.00"},
		{ms(59_994), "0:00:59.99"},
		{ms(59_995), "0:01:00.00"},
		{61*time.Minute + 2*time.Second + ms(340), "1:01:02.34"},
		{10*time.Hour + ms(10), "10:00:00.01"},
	}
	for _, c := range cases {
		if got := Timestamp(c.d); got != c.want {
			t.Errorf("Timestamp(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestColorConversion(t *testing.T) {
	cases := []struct {
		hex          string
		alpha        uint8
		style, inTag string
	}{
		{"#FFFFFF", 0, "&H00FFFFFF", "&HFFFFFF&"},
		{"#000000", 0x80, "&H80000000", "&H000000&"},
		{"#FF0000", 0, "&H000000FF", "&H0000FF&"}, // red is last in ASS
		{"#0000FF", 0, "&H00FF0000", "&HFF0000&"},
		{"#FFE135", 0, "&H0035E1FF", "&H35E1FF&"},
		{"#a1b2c3", 0x10, "&H10C3B2A1", "&HC3B2A1&"}, // lower case in, upper case out
	}
	for _, c := range cases {
		got, err := styleColor(c.hex, c.alpha)
		if err != nil || got != c.style {
			t.Errorf("styleColor(%q, %#x) = %q, %v; want %q", c.hex, c.alpha, got, err, c.style)
		}
		got, err = tagColor(c.hex)
		if err != nil || got != c.inTag {
			t.Errorf("tagColor(%q) = %q, %v; want %q", c.hex, got, err, c.inTag)
		}
	}
	for _, bad := range []string{"", "FFFFFF", "#FFF", "#GGGGGG", "#FFFFFFF", "#+12345", "#0x1234"} {
		if _, err := styleColor(bad, 0); err == nil {
			t.Errorf("styleColor(%q): want an error", bad)
		}
		if _, err := tagColor(bad); err == nil {
			t.Errorf("tagColor(%q): want an error", bad)
		}
	}
}

func TestNewRejectsBadColors(t *testing.T) {
	cfg := defaults(t)
	cfg.HighlightColor = "yellow"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "highlight colour") {
		t.Errorf("got %v", err)
	}
}

func TestEscape(t *testing.T) {
	cases := map[string]string{
		"plain":      "plain",
		"{":          `\{`,
		"}":          `\}`,
		`{\b1}x`:     `\{\` + wordJoiner + `b1\}x`,
		`\N`:         `\` + wordJoiner + `N`,
		`a\`:         `a\` + wordJoiner,
		"Grüße 🔥 日本": "Grüße 🔥 日本",
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"word":            "word",
		"  word  ":        "word",
		"line\nbreak":     "line break",
		"a\r\n\r\nb":      "a b",
		"tab\there":       "tab here",
		"nul\x00bell\x07": "nulbell",
		"\u2028x\u2029":   "x",
		"\n":              "",
	}
	for in, want := range cases {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "captions.ass")
	_, err := newWriter(t, defaults(t)).Write(ctx, transcribe.Transcript{Words: []transcribe.Word{}}, path)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("no file should be written: %v", statErr)
	}
}

// TestFFmpegRendersGoldenFiles burns every golden file into a generated
// 1080x1920 video with ffmpeg's ass filter (libass). It checks that the
// files are valid, not how they look: the font may be missing on the
// machine running the test, and libass then falls back to another one.
func TestFFmpegRendersGoldenFiles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	goldens, err := filepath.Glob(filepath.Join("testdata", "*.ass"))
	if err != nil {
		t.Fatal(err)
	}
	if len(goldens) != len(goldenCases()) {
		t.Fatalf("want %d golden files, found %d", len(goldenCases()), len(goldens))
	}
	for _, golden := range goldens {
		t.Run(filepath.Base(golden), func(t *testing.T) {
			data, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			// ffmpeg runs in the temporary directory with a plain relative
			// name, so the path needs no filter-graph escaping.
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "captions.ass"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err = runner.Run(context.Background(), runner.Command{
				Name: "ffmpeg",
				Args: []string{
					"-hide_banner", "-nostdin", "-y",
					"-f", "lavfi", "-i", "color=c=0x336699:s=1080x1920:r=5:d=12",
					"-vf", "ass=captions.ass",
					"-c:v", "mpeg4", "-q:v", "10",
					"out.mp4",
				},
				Dir:     dir,
				Timeout: 2 * time.Minute,
			})
			if err != nil {
				t.Fatalf("ffmpeg: %v", err)
			}
			info, err := os.Stat(filepath.Join(dir, "out.mp4"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() == 0 {
				t.Error("ffmpeg wrote an empty file")
			}
		})
	}
}
