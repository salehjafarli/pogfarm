package scan

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// minBaseline is the lowest baseline a window is compared against, in
// weighted messages per window. It keeps the score finite in a chat that is
// silent around a burst and stops a handful of messages from scoring as an
// enormous multiple of nearly nothing.
const minBaseline = 1.0

// maxWindows bounds how many windows one chat may span. A chat is bucketed
// into a slice with one entry per window up to its last message, so a single
// message with an absurd offset would otherwise allocate without limit. With
// the default 5-second window this is about 240 days of stream.
const maxWindows = 1 << 22

// SpikeParams configures DetectSpikes. Start from DefaultSpikeParams and
// change what needs changing; the zero value is not valid.
//
// The yaml tags let the struct be embedded in a config section as is.
// Durations are written like "5s" or "5m".
type SpikeParams struct {
	// Window is the length of the buckets the chat is cut into. Spikes
	// start and end on window boundaries.
	Window time.Duration `yaml:"window"`
	// Baseline is the span of chat around a window that its message rate
	// is compared against: half of it before the window and half after,
	// rounded to whole windows and cut off at the ends of the chat. It
	// must be at least twice Window.
	Baseline time.Duration `yaml:"baseline"`
	// Factor is how many times its baseline a window must reach to be
	// flagged. It must be at least 1.
	Factor float64 `yaml:"factor"`
	// MinMessages is the lowest number of messages a window must hold to
	// be flagged, whatever its baseline. It keeps a few messages in an
	// otherwise dead chat from counting as a burst. Messages with weight
	// zero are not counted. It must be at least 1.
	MinMessages int `yaml:"min_messages"`
	// MergeGap is the longest stretch of unflagged chat between two
	// flagged windows that still joins them into one spike. Zero merges
	// only windows that touch.
	MergeGap time.Duration `yaml:"merge_gap"`
	// SkipStart is how much of the start of the VOD is ignored, because
	// streams open with a burst of greetings. Messages before it are
	// dropped and the first window starts at it. Zero ignores nothing.
	SkipStart time.Duration `yaml:"skip_start"`
	// Weights makes messages that contain certain emotes or keywords count
	// for more (or less) than one message, for example laugh and hype
	// emotes. The key is the emote name or word, matched without regard to
	// case against the message's Emotes and against the words of its Text,
	// where a word may carry surrounding punctuation ("lol!!" matches
	// "lol"). A message that matches several keys gets the largest of
	// their weights; a message that matches none weighs 1. Weight 0 makes
	// matching messages invisible to the detector, which suits bot
	// commands. Keys must not contain whitespace and weights must not be
	// negative.
	Weights map[string]float64 `yaml:"weights"`
}

// DefaultSpikeParams returns the parameters used when nothing is configured:
// 5-second windows compared against the surrounding 5 minutes, flagged at 3
// times the baseline with at least 8 messages, only touching windows merged,
// nothing skipped at the start and no weights.
func DefaultSpikeParams() SpikeParams {
	return SpikeParams{
		Window:      5 * time.Second,
		Baseline:    5 * time.Minute,
		Factor:      3,
		MinMessages: 8,
	}
}

// Validate reports everything that is wrong with p, joined into one error,
// or nil when p is usable.
func (p SpikeParams) Validate() error {
	var errs []error
	if p.Window <= 0 {
		errs = append(errs, fmt.Errorf("window must be positive, got %s", p.Window))
	}
	if p.Baseline <= 0 {
		errs = append(errs, fmt.Errorf("baseline must be positive, got %s", p.Baseline))
	} else if p.Window > 0 && p.Baseline/2 < p.Window {
		errs = append(errs, fmt.Errorf("baseline must be at least twice the window (%s), got %s", p.Window, p.Baseline))
	}
	if math.IsNaN(p.Factor) || math.IsInf(p.Factor, 0) || p.Factor < 1 {
		errs = append(errs, fmt.Errorf("factor must be a number of at least 1, got %v", p.Factor))
	}
	if p.MinMessages < 1 {
		errs = append(errs, fmt.Errorf("min_messages must be at least 1, got %d", p.MinMessages))
	}
	if p.MergeGap < 0 {
		errs = append(errs, fmt.Errorf("merge_gap must not be negative, got %s", p.MergeGap))
	}
	if p.SkipStart < 0 {
		errs = append(errs, fmt.Errorf("skip_start must not be negative, got %s", p.SkipStart))
	}
	if _, err := newWeigher(p.Weights); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("scan: invalid spike parameters: %w", errors.Join(errs...))
}

// Spike is one burst of chat activity.
type Spike struct {
	// Start is the start of the first flagged window of the burst,
	// measured from the start of the VOD.
	Start time.Duration
	// End is the end of the last flagged window of the burst. It is
	// exclusive: the burst covers [Start, End).
	End time.Duration
	// Peak is the middle of the highest-scoring window of the burst, the
	// earliest one on a tie. Start < Peak < End.
	Peak time.Duration
	// Score is how many times its baseline the peak window reached: the
	// window's weighted message count divided by the average weighted
	// count of the windows around it (never taken as less than one
	// message per window). It is at least the configured Factor and
	// comparable between spikes of the same VOD.
	Score float64
	// Messages is the number of messages in [Start, End), not counting
	// messages with weight zero.
	Messages int
}

// DetectSpikes finds the bursts in the chat of one VOD.
//
// The chat is cut into windows of p.Window. A window is flagged when it holds
// at least p.MinMessages messages and its weighted message count is at least
// p.Factor times its baseline, the average weighted count of the other
// windows within p.Baseline around it. Flagged windows that touch, or lie no
// more than p.MergeGap apart, become one spike.
//
// The messages may be in any order and are not modified. Messages with a
// negative offset or an offset before p.SkipStart are ignored. Spikes are
// returned ordered by Start and do not overlap; the result is nil when there
// are none. The time taken is linear in the number of messages plus the
// number of windows up to the last message.
//
// An error is returned when p is invalid or the chat reaches further than
// about four million windows, which no real VOD does.
func DetectSpikes(chat []source.ChatMessage, p SpikeParams) ([]Spike, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	weights, _ := newWeigher(p.Weights) // Validate already checked it

	last := time.Duration(-1)
	for i := range chat {
		if chat[i].Offset > last {
			last = chat[i].Offset
		}
	}
	if last < p.SkipStart {
		return nil, nil
	}
	if (last-p.SkipStart)/p.Window >= maxWindows {
		return nil, fmt.Errorf("scan: chat reaches %s, more than %d windows of %s", last, maxWindows, p.Window)
	}
	n := int((last-p.SkipStart)/p.Window) + 1

	// weight[i] and count[i] describe the window starting at
	// SkipStart + i*Window.
	weight := make([]float64, n)
	count := make([]int, n)
	for i := range chat {
		off := chat[i].Offset
		if off < p.SkipStart || off < 0 {
			continue
		}
		w := weights.of(&chat[i])
		if w == 0 {
			continue
		}
		b := int((off - p.SkipStart) / p.Window)
		weight[b] += w
		count[b]++
	}

	// prefix[i] is the total weight of windows [0, i), so the weight of
	// any run of windows is one subtraction.
	prefix := make([]float64, n+1)
	for i, w := range weight {
		prefix[i+1] = prefix[i] + w
	}

	// The baseline of a window covers half windows on each side of it.
	half := int((p.Baseline/2 + p.Window/2) / p.Window)

	var spikes []Spike
	lastFlagged := -1 // index of the last flagged window, part of the last spike
	for i := range n {
		if count[i] < p.MinMessages {
			continue
		}
		lo, hi := max(0, i-half), min(n-1, i+half)
		baseline := 0.0
		if others := hi - lo; others > 0 {
			baseline = (prefix[hi+1] - prefix[lo] - weight[i]) / float64(others)
		}
		baseline = max(baseline, minBaseline)
		if weight[i] < p.Factor*baseline {
			continue
		}
		score := weight[i] / baseline
		start := p.SkipStart + time.Duration(i)*p.Window

		if lastFlagged >= 0 && time.Duration(i-lastFlagged-1)*p.Window <= p.MergeGap {
			s := &spikes[len(spikes)-1]
			for j := lastFlagged + 1; j <= i; j++ {
				s.Messages += count[j]
			}
			s.End = start + p.Window
			if score > s.Score {
				s.Score = score
				s.Peak = start + p.Window/2
			}
		} else {
			spikes = append(spikes, Spike{
				Start:    start,
				End:      start + p.Window,
				Peak:     start + p.Window/2,
				Score:    score,
				Messages: count[i],
			})
		}
		lastFlagged = i
	}
	return spikes, nil
}

// weigher holds the configured weights by lower-cased key.
type weigher map[string]float64

func newWeigher(weights map[string]float64) (weigher, error) {
	if len(weights) == 0 {
		return nil, nil
	}
	var errs []error
	w := make(weigher, len(weights))
	for key, v := range weights {
		if key == "" || strings.ContainsFunc(key, unicode.IsSpace) {
			errs = append(errs, fmt.Errorf("weights: key %q must be one word", key))
			continue
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			errs = append(errs, fmt.Errorf("weights: weight of %q must be a number of at least 0, got %v", key, v))
			continue
		}
		lower := strings.ToLower(key)
		if prev, ok := w[lower]; ok && prev != v {
			errs = append(errs, fmt.Errorf("weights: %q is given more than once, in different case, with different weights", lower))
			continue
		}
		w[lower] = v
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return w, nil
}

// of returns the weight of one message: the largest weight among the keys it
// contains, or 1 when it contains none.
func (w weigher) of(m *source.ChatMessage) float64 {
	if len(w) == 0 {
		return 1
	}
	best, found := 0.0, false
	for _, e := range m.Emotes {
		if v, ok := w.lookup(e); ok && (!found || v > best) {
			best, found = v, true
		}
	}
	for word := range strings.FieldsSeq(m.Text) {
		if v, ok := w.lookup(word); ok && (!found || v > best) {
			best, found = v, true
		}
	}
	if !found {
		return 1
	}
	return best
}

// lookup finds the weight of one token, first as written and then with
// surrounding punctuation removed.
func (w weigher) lookup(token string) (float64, bool) {
	token = strings.ToLower(token)
	if v, ok := w[token]; ok {
		return v, true
	}
	trimmed := strings.TrimFunc(token, unicode.IsPunct)
	if trimmed == token || trimmed == "" {
		return 0, false
	}
	v, ok := w[trimmed]
	return v, ok
}
