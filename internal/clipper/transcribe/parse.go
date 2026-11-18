package transcribe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ParseStats counts the words whose times Parse had to make up.
type ParseStats struct {
	// FilledWords is the number of words of aligned segments that had no
	// timestamps of their own and were timed from their neighbours.
	FilledWords int
	// EvenSplitSegments is the number of segments with text but no words,
	// whose time was split evenly across the words of their text.
	EvenSplitSegments int
}

// whisperXOutput is the part of WhisperX's JSON output that is read.
// Absent numbers are nil.
type whisperXOutput struct {
	Segments []struct {
		Start *float64 `json:"start"`
		End   *float64 `json:"end"`
		Text  string   `json:"text"`
		Words []struct {
			Word  string   `json:"word"`
			Start *float64 `json:"start"`
			End   *float64 `json:"end"`
		} `json:"words"`
	} `json:"segments"`
	Language string `json:"language"`
}

// Parse reads the JSON file WhisperX writes and returns its words, by the
// rules in the package documentation. Malformed JSON is an error; output
// without segments is an empty transcript.
func Parse(data []byte) (Transcript, ParseStats, error) {
	var out whisperXOutput
	if err := json.Unmarshal(nanToNull(data), &out); err != nil {
		return Transcript{}, ParseStats{}, fmt.Errorf("read whisperx output: %w", err)
	}

	var stats ParseStats
	words := []Word{}
	// cursor is where the previous segment ended, the fallback for a
	// segment without times of its own.
	var cursor float64
	for _, seg := range out.Segments {
		segStart := orDefault(seg.Start, cursor)
		segEnd := orDefault(seg.End, segStart)

		if len(seg.Words) == 0 {
			texts := strings.Fields(seg.Text)
			if len(texts) > 0 {
				stats.EvenSplitSegments++
				words = appendSpread(words, texts, segStart, segEnd)
			}
			cursor = segEnd
			continue
		}

		// Untimed words are collected in pending and spread over the gap
		// between prev (the end of the last timed word, or the segment
		// start) and the start of the next timed word (or the segment end).
		prev := segStart
		var pending []string
		for _, w := range seg.Words {
			text := strings.TrimSpace(w.Word)
			if text == "" {
				continue
			}
			if w.Start == nil || w.End == nil {
				pending = append(pending, text)
				continue
			}
			if len(pending) > 0 {
				stats.FilledWords += len(pending)
				words = appendSpread(words, pending, prev, *w.Start)
				pending = pending[:0]
			}
			words = append(words, Word{Text: text, Start: seconds(*w.Start), End: seconds(*w.End)})
			prev = *w.End
		}
		if len(pending) > 0 {
			stats.FilledWords += len(pending)
			words = appendSpread(words, pending, prev, segEnd)
		}
		cursor = segEnd
	}
	clamp(words)
	return Transcript{Language: out.Language, Words: words}, stats, nil
}

// appendSpread appends texts as words that split [from, to] evenly, in
// order. An empty or inverted range gives every word the time from.
func appendSpread(words []Word, texts []string, from, to float64) []Word {
	step := (to - from) / float64(len(texts))
	if step < 0 {
		step = 0
	}
	for i, text := range texts {
		start := from + step*float64(i)
		words = append(words, Word{Text: text, Start: seconds(start), End: seconds(start + step)})
	}
	return words
}

// clamp makes times non-negative and non-decreasing, and every end at least
// its start, without changing the order of the words.
func clamp(words []Word) {
	var lastStart, lastEnd time.Duration
	for i := range words {
		w := &words[i]
		w.Start = max(w.Start, lastStart)
		w.End = max(w.End, w.Start, lastEnd)
		lastStart, lastEnd = w.Start, w.End
	}
}

func orDefault(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

// seconds converts WhisperX's seconds to a Duration, rounded to the
// millisecond, the precision WhisperX writes. Negative times become zero.
func seconds(s float64) time.Duration {
	if s <= 0 || math.IsNaN(s) {
		return 0
	}
	return time.Duration(math.Round(s*1000)) * time.Millisecond
}

// nanToNull replaces the tokens NaN, Infinity and -Infinity outside strings
// with null. Python's json module writes them for numbers that are not
// finite; they are not JSON, and a missing number is what they mean here.
func nanToNull(data []byte) []byte {
	if !bytes.Contains(data, []byte("NaN")) && !bytes.Contains(data, []byte("Infinity")) {
		return data
	}
	var b bytes.Buffer
	b.Grow(len(data))
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			b.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(data) {
					i++
					b.WriteByte(data[i])
				}
			case '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		replaced := false
		for _, tok := range []string{"NaN", "-Infinity", "Infinity"} {
			if bytes.HasPrefix(data[i:], []byte(tok)) {
				b.WriteString("null")
				i += len(tok) - 1
				replaced = true
				break
			}
		}
		if !replaced {
			b.WriteByte(c)
		}
	}
	return b.Bytes()
}
