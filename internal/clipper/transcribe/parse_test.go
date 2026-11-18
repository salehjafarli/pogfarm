package transcribe

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The fixtures in testdata are written by hand in the shape WhisperX 3.8.6
// writes (whisperx/transcribe.py, whisperx/alignment.py, WriteJSON in
// whisperx/utils.py: json.dump with ensure_ascii=False, so non-ASCII text is
// raw UTF-8 and missing numbers are the bare token NaN):
//
//   - normal.json: aligned output for the sample speech of the base image's
//     smoke test, two sentences, every word timed. The first words' times
//     are those the base image's CI run printed.
//   - missing_timestamps.json: aligned output where numbers and symbols
//     have no alignment timestamps, a segment whose alignment failed
//     ("words": [], "chars": null) and a sentence of digits only, whose
//     start and end WhisperX writes as NaN.
//   - silence.json: no speech found; alignment is skipped and only the
//     transcription result is written.
//   - unaligned_de.json: German run with --no_align, non-ASCII text,
//     segments without words.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestParseNormal(t *testing.T) {
	tr, stats, err := Parse(readFixture(t, "normal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Language != "en" {
		t.Errorf("language: got %q", tr.Language)
	}
	if stats != (ParseStats{}) {
		t.Errorf("nothing should be made up: %+v", stats)
	}
	if len(tr.Words) != 22 {
		t.Fatalf("want 22 words, got %d: %v", len(tr.Words), tr.Words)
	}
	var texts []string
	for _, w := range tr.Words {
		texts = append(texts, w.Text)
	}
	if got, want := strings.Join(texts, " "),
		"And so, my fellow Americans, ask not what your country can do for you. Ask what you can do for your country."; got != want {
		t.Errorf("text:\n got %q\nwant %q", got, want)
	}
	checks := map[int]Word{
		0:  {"And", ms(421), ms(541)},
		1:  {"so,", ms(642), ms(962)},
		13: {"you.", ms(7193), ms(7594)},
		14: {"Ask", ms(8255), ms(8475)},
		21: {"country.", ms(10056), ms(10556)},
	}
	for i, want := range checks {
		if tr.Words[i] != want {
			t.Errorf("word %d: got %+v, want %+v", i, tr.Words[i], want)
		}
	}
}

func TestParseMissingTimestamps(t *testing.T) {
	tr, stats, err := Parse(readFixture(t, "missing_timestamps.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Word{
		// Untimed first word: from the segment start to the next timed word.
		{"1961", ms(1000), ms(1500)},
		{"was", ms(1500), ms(1700)},
		{"the", ms(1800), ms(1900)},
		{"year", ms(2000), ms(2400)},
		// Two untimed words share the gap between their timed neighbours.
		{"20", ms(2400), ms(2700)},
		{"21", ms(2700), ms(3000)},
		{"began.", ms(3000), ms(3500)},
		// Untimed last word: up to the segment end.
		{"$5", ms(3500), ms(4000)},
		// Segment whose alignment failed: the segment time.
		{"100%", ms(5000), ms(6000)},
		// NaN segment times: the previous segment's end, zero length.
		{"3", ms(6000), ms(6000)},
		{"2", ms(6000), ms(6000)},
		{"1", ms(6000), ms(6000)},
	}
	if !reflect.DeepEqual(tr.Words, want) {
		t.Errorf("words:\n got %v\nwant %v", tr.Words, want)
	}
	if stats != (ParseStats{FilledWords: 7, EvenSplitSegments: 1}) {
		t.Errorf("stats: got %+v", stats)
	}
}

func TestParseSilence(t *testing.T) {
	tr, stats, err := Parse(readFixture(t, "silence.json"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Words == nil || len(tr.Words) != 0 {
		t.Errorf("want an empty, non-nil word list, got %#v", tr.Words)
	}
	if tr.Language != "en" || stats != (ParseStats{}) {
		t.Errorf("got language %q, stats %+v", tr.Language, stats)
	}
}

func TestParseUnalignedNonASCII(t *testing.T) {
	tr, stats, err := Parse(readFixture(t, "unaligned_de.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 1.469 s over three words, 2.5 s over six, rounded to the millisecond.
	want := []Word{
		{"Grüße", ms(31), ms(521)},
		{"aus", ms(521), ms(1010)},
		{"München!", ms(1010), ms(1500)},
		{"Das", ms(2000), ms(2417)},
		{"Spiel", ms(2417), ms(2833)},
		{"ist", ms(2833), ms(3250)},
		{"großartig,", ms(3250), ms(3667)},
		{"ça", ms(3667), ms(4083)},
		{"va?", ms(4083), ms(4500)},
	}
	if !reflect.DeepEqual(tr.Words, want) {
		t.Errorf("words:\n got %v\nwant %v", tr.Words, want)
	}
	if tr.Language != "de" || stats != (ParseStats{EvenSplitSegments: 2}) {
		t.Errorf("got language %q, stats %+v", tr.Language, stats)
	}
}

func TestParseMalformed(t *testing.T) {
	for name, input := range map[string]string{
		"empty":           "",
		"not json":        "Traceback (most recent call last):",
		"truncated":       `{"segments": [{"start": 1.0, "end": 2.0, "text": " hi", "words": [{"word": "hi", "start"`,
		"segments object": `{"segments": {"start": 1.0}}`,
		"word not text":   `{"segments": [{"start": 0, "end": 1, "text": "x", "words": [{"word": 5}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Parse([]byte(input)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestParseClamps(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []Word
	}{
		{
			name: "negative times become zero",
			json: `{"segments": [{"start": -0.2, "end": 0.5, "text": "a b", "words": [
				{"word": "a", "start": -0.05, "end": -0.01}, {"word": "b", "start": 0.1, "end": 0.5}]}]}`,
			want: []Word{{"a", 0, 0}, {"b", ms(100), ms(500)}},
		},
		{
			name: "starts and ends never decrease, order kept",
			json: `{"segments": [{"start": 1, "end": 2, "text": "a b c", "words": [
				{"word": "a", "start": 1.0, "end": 2.0}, {"word": "b", "start": 1.5, "end": 1.8},
				{"word": "c", "start": 0.5, "end": 0.7}]}]}`,
			want: []Word{{"a", ms(1000), ms(2000)}, {"b", ms(1500), ms(2000)}, {"c", ms(1500), ms(2000)}},
		},
		{
			name: "end before start",
			json: `{"segments": [{"start": 3, "end": 4, "text": "a", "words": [{"word": "a", "start": 3.0, "end": 2.0}]}]}`,
			want: []Word{{"a", ms(3000), ms(3000)}},
		},
		{
			name: "segment earlier than the one before",
			json: `{"segments": [{"start": 5, "end": 6, "text": "late"}, {"start": 1, "end": 2, "text": "early"}]}`,
			want: []Word{{"late", ms(5000), ms(6000)}, {"early", ms(5000), ms(6000)}},
		},
		{
			name: "untimed words in an inverted gap",
			json: `{"segments": [{"start": 0, "end": 3, "text": "a 1 b", "words": [
				{"word": "a", "start": 0.0, "end": 2.0}, {"word": "1"}, {"word": "b", "start": 1.0, "end": 3.0}]}]}`,
			want: []Word{{"a", 0, ms(2000)}, {"1", ms(2000), ms(2000)}, {"b", ms(2000), ms(3000)}},
		},
		{
			name: "blank words are dropped",
			json: `{"segments": [{"start": 0, "end": 1, "text": "a", "words": [{"word": " "}, {"word": " a ", "start": 0.2, "end": 0.4}]}]}`,
			want: []Word{{"a", ms(200), ms(400)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, _, err := Parse([]byte(tt.json))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tr.Words, tt.want) {
				t.Errorf("got %v, want %v", tr.Words, tt.want)
			}
		})
	}
}

func TestNaNToNull(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"a": 1}`, `{"a": 1}`},
		{`{"a": NaN, "b": -Infinity, "c": [Infinity]}`, `{"a": null, "b": null, "c": [null]}`},
		{`{"t": "NaN and Infinity", "u": "q\"NaN", "v": NaN}`, `{"t": "NaN and Infinity", "u": "q\"NaN", "v": null}`},
		{`{"t": "\\", "v": NaN}`, `{"t": "\\", "v": null}`},
	}
	for _, tt := range tests {
		if got := string(nanToNull([]byte(tt.in))); got != tt.want {
			t.Errorf("nanToNull(%s):\n got %s\nwant %s", tt.in, got, tt.want)
		}
	}
}
