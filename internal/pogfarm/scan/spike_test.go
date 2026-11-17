package scan

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const (
	s = time.Second
	m = time.Minute
)

// every returns one message at from, from+step, ... up to but not including
// to: a perfectly steady chat.
func every(from, to, step time.Duration, text string) []source.ChatMessage {
	var chat []source.ChatMessage
	for off := from; off < to; off += step {
		chat = append(chat, source.ChatMessage{Offset: off, User: "steady", Text: text})
	}
	return chat
}

// burst returns n messages spread evenly over [from, to).
func burst(from, to time.Duration, n int, text string) []source.ChatMessage {
	chat := make([]source.ChatMessage, n)
	for i := range chat {
		chat[i] = source.ChatMessage{
			Offset: from + (to-from)*time.Duration(i)/time.Duration(n),
			User:   "burst",
			Text:   text,
		}
	}
	return chat
}

// join concatenates chats and sorts the result by offset, as a source
// returns it.
func join(parts ...[]source.ChatMessage) []source.ChatMessage {
	chat := slices.Concat(parts...)
	slices.SortStableFunc(chat, func(a, b source.ChatMessage) int {
		return int(a.Offset - b.Offset)
	})
	return chat
}

// with returns the default parameters after applying change.
func with(change func(*SpikeParams)) SpikeParams {
	p := DefaultSpikeParams()
	if change != nil {
		change(&p)
	}
	return p
}

// busy is half an hour of chat at one message a second: 5 per default
// window, which is below the default minimum of 8 on its own.
func busy() []source.ChatMessage { return every(0, 30*m, s, "hello") }

func TestDetectSpikes(t *testing.T) {
	tests := []struct {
		name   string
		chat   []source.ChatMessage
		params SpikeParams
		want   []Spike
	}{
		{
			name:   "nil chat",
			chat:   nil,
			params: DefaultSpikeParams(),
		},
		{
			name:   "empty chat",
			chat:   []source.ChatMessage{},
			params: DefaultSpikeParams(),
		},
		{
			name:   "single message",
			chat:   []source.ChatMessage{{Offset: 42 * s, Text: "hi"}},
			params: DefaultSpikeParams(),
		},
		{
			name:   "chat shorter than one window",
			chat:   burst(0, 3*s, 4, "hi"),
			params: DefaultSpikeParams(),
		},
		{
			// With nothing to compare against, the baseline is the floor
			// of one message per window.
			name:   "chat shorter than one window but dense",
			chat:   burst(0, 3*s, 20, "hi"),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 0, End: 5 * s, Peak: 2500 * time.Millisecond, Score: 20, Messages: 20}},
		},
		{
			name:   "steady chat",
			chat:   busy(),
			params: DefaultSpikeParams(),
		},
		{
			// 20 messages in every window: far above the minimum count,
			// but never above its own baseline.
			name:   "steady busy chat without bursts",
			chat:   every(0, 30*m, 250*time.Millisecond, "hello"),
			params: DefaultSpikeParams(),
		},
		{
			name:   "busy chat with a mild rise below the factor",
			chat:   join(every(0, 30*m, 250*time.Millisecond, "hello"), burst(10*m, 10*m+5*s, 30, "LUL")),
			params: DefaultSpikeParams(),
		},
		{
			name:   "quiet chat",
			chat:   every(0, 30*m, 30*s, "hello"),
			params: DefaultSpikeParams(),
		},
		{
			// 7 messages in a dead chat are a huge multiple of the
			// baseline but too few to matter.
			name:   "quiet chat with a burst below the minimum count",
			chat:   join(every(0, 30*m, 30*s, "hello"), burst(10*m, 10*m+5*s, 6, "LUL")),
			params: DefaultSpikeParams(),
		},
		{
			name:   "quiet chat with a burst and a lower minimum count",
			chat:   join(every(0, 30*m, 30*s, "hello"), burst(10*m, 10*m+5*s, 6, "LUL")),
			params: with(func(p *SpikeParams) { p.MinMessages = 5 }),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 7, Messages: 7}},
		},
		{
			name:   "clear burst",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL")),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			name:   "clear burst with a higher factor than it reaches",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.Factor = 9.5 }),
		},
		{
			name:   "clear burst exactly at the factor",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.Factor = 9 }),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			name:   "clear burst in unsorted chat",
			chat:   slices.Concat(burst(10*m, 10*m+5*s, 40, "LUL"), reversed(busy())),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			// The second window is the stronger one, so it is the peak.
			// Its baseline holds 59 ordinary windows and the first burst
			// window: (59*5 + 45) / 60.
			name:   "adjacent bursts merged",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL"), burst(10*m+5*s, 10*m+10*s, 60, "LUL")),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 10 * m, End: 10*m + 10*s, Peak: 10*m + 7500*time.Millisecond, Score: 65 / (340.0 / 60), Messages: 110}},
		},
		{
			name:   "one long burst over four windows",
			chat:   join(busy(), burst(10*m, 10*m+20*s, 160, "LUL")),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 10 * m, End: 10*m + 20*s, Peak: 10*m + 2500*time.Millisecond, Score: 45 / ((57*5 + 3*45) / 60.0), Messages: 180}},
		},
		{
			name:   "two bursts close together stay apart by default",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL"), burst(10*m+15*s, 10*m+20*s, 40, "LUL")),
			params: DefaultSpikeParams(),
			want: []Spike{
				{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 45 / (340.0 / 60), Messages: 45},
				{Start: 10*m + 15*s, End: 10*m + 20*s, Peak: 10*m + 17500*time.Millisecond, Score: 45 / (340.0 / 60), Messages: 45},
			},
		},
		{
			name:   "two bursts close together with a merge gap that is too short",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL"), burst(10*m+15*s, 10*m+20*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.MergeGap = 9 * s }),
			want: []Spike{
				{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 45 / (340.0 / 60), Messages: 45},
				{Start: 10*m + 15*s, End: 10*m + 20*s, Peak: 10*m + 17500*time.Millisecond, Score: 45 / (340.0 / 60), Messages: 45},
			},
		},
		{
			// The peak is the earlier of the two equal windows, and the
			// messages of the gap count towards the spike.
			name:   "two bursts close together merged across the gap",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL"), burst(10*m+15*s, 10*m+20*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.MergeGap = 10 * s }),
			want:   []Spike{{Start: 10 * m, End: 10*m + 20*s, Peak: 10*m + 2500*time.Millisecond, Score: 45 / (340.0 / 60), Messages: 100}},
		},
		{
			name:   "two bursts far apart",
			chat:   join(busy(), burst(5*m, 5*m+5*s, 40, "LUL"), burst(20*m, 20*m+5*s, 60, "LUL")),
			params: DefaultSpikeParams(),
			want: []Spike{
				{Start: 5 * m, End: 5*m + 5*s, Peak: 5*m + 2500*time.Millisecond, Score: 9, Messages: 45},
				{Start: 20 * m, End: 20*m + 5*s, Peak: 20*m + 2500*time.Millisecond, Score: 13, Messages: 65},
			},
		},
		{
			name:   "burst at the very start",
			chat:   join(busy(), burst(0, 5*s, 40, "hi")),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 0, End: 5 * s, Peak: 2500 * time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			name:   "burst at the very start skipped",
			chat:   join(busy(), burst(0, 5*s, 40, "hi")),
			params: with(func(p *SpikeParams) { p.SkipStart = 2 * m }),
		},
		{
			name:   "burst at the very end",
			chat:   join(busy(), burst(30*m-5*s, 30*m, 40, "gg")),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 30*m - 5*s, End: 30 * m, Peak: 30*m - 2500*time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			// The windows start at the skip, not at zero: 92s + 102*5s.
			name:   "windows are aligned to the skipped start",
			chat:   join(busy(), burst(10*m+2*s, 10*m+7*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.SkipStart = 92 * s }),
			want:   []Spike{{Start: 10*m + 2*s, End: 10*m + 7*s, Peak: 10*m + 4500*time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			name:   "everything before the skipped start",
			chat:   join(every(0, m, s, "hello"), burst(30*s, 35*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.SkipStart = 2 * m }),
		},
		{
			name:   "negative offsets are ignored",
			chat:   join(busy(), burst(-10*s, -5*s, 40, "early"), burst(10*m, 10*m+5*s, 40, "LUL")),
			params: DefaultSpikeParams(),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 9, Messages: 45}},
		},
		{
			name:   "only negative offsets",
			chat:   burst(-10*s, -5*s, 40, "early"),
			params: DefaultSpikeParams(),
		},
		{
			name:   "larger window",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "LUL")),
			params: with(func(p *SpikeParams) { p.Window = 10 * s }),
			want:   []Spike{{Start: 10 * m, End: 10*m + 10*s, Peak: 10*m + 5*s, Score: 5, Messages: 50}},
		},
		{
			// 11 messages against a baseline of 5 is 2.2 times: no spike
			// until the laugh emote counts three times.
			name:   "small burst of an unweighted emote",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 6, "KEKW")),
			params: DefaultSpikeParams(),
		},
		{
			name:   "small burst of a weighted emote",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 6, "KEKW")),
			params: with(func(p *SpikeParams) { p.Weights = map[string]float64{"kekw": 3} }),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 4.6, Messages: 11}},
		},
		{
			name:   "burst of ignored bot commands",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "!drop")),
			params: with(func(p *SpikeParams) { p.Weights = map[string]float64{"!drop": 0} }),
		},
		{
			// Half weight on everything said in the burst: 5 + 40*0.25
			// is 15, exactly three times the baseline.
			name:   "weights below one damp a burst",
			chat:   join(busy(), burst(10*m, 10*m+5*s, 40, "spam")),
			params: with(func(p *SpikeParams) { p.Weights = map[string]float64{"spam": 0.25} }),
			want:   []Spike{{Start: 10 * m, End: 10*m + 5*s, Peak: 10*m + 2500*time.Millisecond, Score: 3, Messages: 45}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.chat)

			got, err := DetectSpikes(tt.chat, tt.params)
			if err != nil {
				t.Fatalf("DetectSpikes: %v", err)
			}
			if !sameSpikes(got, tt.want) {
				t.Errorf("spikes differ\n got: %s\nwant: %s", formatSpikes(got), formatSpikes(tt.want))
			}
			checkInvariants(t, got, tt.params)

			if !slices.EqualFunc(tt.chat, before, sameMessage) {
				t.Error("DetectSpikes modified its input")
			}
			again, err := DetectSpikes(tt.chat, tt.params)
			if err != nil {
				t.Fatalf("second DetectSpikes: %v", err)
			}
			if !slices.Equal(got, again) {
				t.Errorf("second run differs\nfirst:  %s\nsecond: %s", formatSpikes(got), formatSpikes(again))
			}
		})
	}
}

func reversed(chat []source.ChatMessage) []source.ChatMessage {
	chat = slices.Clone(chat)
	slices.Reverse(chat)
	return chat
}

func sameMessage(a, b source.ChatMessage) bool {
	return a.Offset == b.Offset && a.User == b.User && a.Text == b.Text && slices.Equal(a.Emotes, b.Emotes)
}

// sameSpikes compares spikes exactly except for the score, which is a ratio
// of sums of floats.
func sameSpikes(got, want []Spike) bool {
	return slices.EqualFunc(got, want, func(a, b Spike) bool {
		return a.Start == b.Start && a.End == b.End && a.Peak == b.Peak &&
			a.Messages == b.Messages && math.Abs(a.Score-b.Score) < 1e-9
	})
}

func formatSpikes(spikes []Spike) string {
	if len(spikes) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, sp := range spikes {
		fmt.Fprintf(&b, "\n  start=%s end=%s peak=%s score=%.3f messages=%d", sp.Start, sp.End, sp.Peak, sp.Score, sp.Messages)
	}
	return b.String()
}

// checkInvariants checks what holds for every result whatever the input.
func checkInvariants(t *testing.T, spikes []Spike, p SpikeParams) {
	t.Helper()
	for i, sp := range spikes {
		if !(sp.Start < sp.Peak && sp.Peak < sp.End) {
			t.Errorf("spike %d: want Start < Peak < End, got %s, %s, %s", i, sp.Start, sp.Peak, sp.End)
		}
		if sp.Start < p.SkipStart {
			t.Errorf("spike %d starts at %s, before the skipped start %s", i, sp.Start, p.SkipStart)
		}
		if (sp.Start-p.SkipStart)%p.Window != 0 || (sp.End-sp.Start)%p.Window != 0 {
			t.Errorf("spike %d [%s, %s) is not on window boundaries", i, sp.Start, sp.End)
		}
		if sp.Score < p.Factor || math.IsInf(sp.Score, 0) || math.IsNaN(sp.Score) {
			t.Errorf("spike %d has score %v, want a finite number of at least the factor %v", i, sp.Score, p.Factor)
		}
		if sp.Messages < p.MinMessages {
			t.Errorf("spike %d has %d messages, fewer than the minimum %d", i, sp.Messages, p.MinMessages)
		}
		if i > 0 && sp.Start-spikes[i-1].End <= p.MergeGap {
			t.Errorf("spike %d starts at %s, within the merge gap of the previous one ending at %s", i, sp.Start, spikes[i-1].End)
		}
	}
}

func TestWeigher(t *testing.T) {
	w, err := newWeigher(map[string]float64{"KEKW": 3, "lol": 2, "!drop": 0, "meh": 0.5})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		msg  source.ChatMessage
		want float64
	}{
		{"no match", source.ChatMessage{Text: "what a play"}, 1},
		{"empty message", source.ChatMessage{}, 1},
		{"emote in text", source.ChatMessage{Text: "KEKW"}, 3},
		{"emote in other case", source.ChatMessage{Text: "kekw"}, 3},
		{"keyword in other case", source.ChatMessage{Text: "LOL"}, 2},
		{"emote among words", source.ChatMessage{Text: "no way KEKW he did it"}, 3},
		{"emote only in the emote list", source.ChatMessage{Text: "", Emotes: []string{"KEKW"}}, 3},
		{"emote in both", source.ChatMessage{Text: "KEKW KEKW", Emotes: []string{"KEKW"}}, 3},
		{"keyword with punctuation", source.ChatMessage{Text: "lol!!!"}, 2},
		{"keyword in quotes", source.ChatMessage{Text: `"lol", he said`}, 2},
		{"key that is itself punctuated", source.ChatMessage{Text: "!drop"}, 0},
		{"part of a longer word", source.ChatMessage{Text: "lollipop"}, 1},
		{"largest of several", source.ChatMessage{Text: "lol KEKW meh"}, 3},
		{"largest of several, other order", source.ChatMessage{Text: "KEKW lol"}, 3},
		{"zero loses to a real weight", source.ChatMessage{Text: "!drop lol"}, 2},
		{"below one alone", source.ChatMessage{Text: "meh"}, 0.5},
		{"only punctuation", source.ChatMessage{Text: "!!! ???"}, 1},
		{"extra whitespace", source.ChatMessage{Text: "  \t lol \n"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := w.of(&tt.msg); got != tt.want {
				t.Errorf("weight of %q %v = %v, want %v", tt.msg.Text, tt.msg.Emotes, got, tt.want)
			}
		})
	}

	t.Run("no weights configured", func(t *testing.T) {
		var none weigher
		if got := none.of(&source.ChatMessage{Text: "KEKW"}); got != 1 {
			t.Errorf("weight = %v, want 1", got)
		}
	})
}

func TestDefaultSpikeParamsAreValid(t *testing.T) {
	if err := DefaultSpikeParams().Validate(); err != nil {
		t.Errorf("default parameters are invalid: %v", err)
	}
}

func TestSpikeParamsValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*SpikeParams)
		want   []string // substrings of the error; none means valid
	}{
		{"defaults", nil, nil},
		{"zero window", func(p *SpikeParams) { p.Window = 0 }, []string{"window must be positive"}},
		{"negative window", func(p *SpikeParams) { p.Window = -5 * s }, []string{"window must be positive"}},
		{"tiny window", func(p *SpikeParams) { p.Window = 100 * time.Millisecond }, nil},
		{"zero baseline", func(p *SpikeParams) { p.Baseline = 0 }, []string{"baseline must be positive"}},
		{"negative baseline", func(p *SpikeParams) { p.Baseline = -m }, []string{"baseline must be positive"}},
		{"baseline shorter than two windows", func(p *SpikeParams) { p.Baseline = 9 * s }, []string{"baseline must be at least twice the window"}},
		{"baseline of exactly two windows", func(p *SpikeParams) { p.Baseline = 10 * s }, nil},
		{"zero factor", func(p *SpikeParams) { p.Factor = 0 }, []string{"factor must be"}},
		{"negative factor", func(p *SpikeParams) { p.Factor = -3 }, []string{"factor must be"}},
		{"factor below one", func(p *SpikeParams) { p.Factor = 0.99 }, []string{"factor must be"}},
		{"factor of one", func(p *SpikeParams) { p.Factor = 1 }, nil},
		{"factor not a number", func(p *SpikeParams) { p.Factor = math.NaN() }, []string{"factor must be"}},
		{"infinite factor", func(p *SpikeParams) { p.Factor = math.Inf(1) }, []string{"factor must be"}},
		{"zero minimum count", func(p *SpikeParams) { p.MinMessages = 0 }, []string{"min_messages must be at least 1"}},
		{"negative minimum count", func(p *SpikeParams) { p.MinMessages = -1 }, []string{"min_messages must be at least 1"}},
		{"minimum count of one", func(p *SpikeParams) { p.MinMessages = 1 }, nil},
		{"negative merge gap", func(p *SpikeParams) { p.MergeGap = -s }, []string{"merge_gap must not be negative"}},
		{"negative skipped start", func(p *SpikeParams) { p.SkipStart = -s }, []string{"skip_start must not be negative"}},
		{"empty weights", func(p *SpikeParams) { p.Weights = map[string]float64{} }, nil},
		{"good weights", func(p *SpikeParams) { p.Weights = map[string]float64{"KEKW": 2, "!drop": 0, "meh": 0.5} }, nil},
		{"same key twice with the same weight", func(p *SpikeParams) { p.Weights = map[string]float64{"LUL": 2, "lul": 2} }, nil},
		{"same key twice with different weights", func(p *SpikeParams) { p.Weights = map[string]float64{"LUL": 2, "lul": 3} }, []string{`"lul" is given more than once`}},
		{"negative weight", func(p *SpikeParams) { p.Weights = map[string]float64{"LUL": -1} }, []string{`weight of "LUL"`}},
		{"weight not a number", func(p *SpikeParams) { p.Weights = map[string]float64{"LUL": math.NaN()} }, []string{`weight of "LUL"`}},
		{"infinite weight", func(p *SpikeParams) { p.Weights = map[string]float64{"LUL": math.Inf(1)} }, []string{`weight of "LUL"`}},
		{"empty weight key", func(p *SpikeParams) { p.Weights = map[string]float64{"": 2} }, []string{`key "" must be one word`}},
		{"weight key of two words", func(p *SpikeParams) { p.Weights = map[string]float64{"so good": 2} }, []string{`key "so good" must be one word`}},
		{
			"everything wrong at once",
			func(p *SpikeParams) {
				*p = SpikeParams{MergeGap: -1, SkipStart: -1, Weights: map[string]float64{"x": -1}}
			},
			[]string{"window must be", "baseline must be", "factor must be", "min_messages must be", "merge_gap must not", "skip_start must not", `weight of "x"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := with(tt.change)
			err := p.Validate()

			// DetectSpikes must agree with Validate, and report the
			// problem even when there is no chat to look at.
			chat := join(busy(), burst(10*m, 10*m+5*s, 40, "LUL"))
			for _, c := range [][]source.ChatMessage{nil, chat} {
				spikes, detectErr := DetectSpikes(c, p)
				if (detectErr == nil) != (err == nil) {
					t.Errorf("DetectSpikes error = %v, Validate error = %v", detectErr, err)
				}
				if detectErr != nil && spikes != nil {
					t.Errorf("DetectSpikes returned spikes along with an error")
				}
			}

			if len(tt.want) == 0 {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.want)
			}
			for _, sub := range tt.want {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("Validate() = %q, want it to contain %q", err, sub)
				}
			}
		})
	}
}

func TestDetectSpikesRejectsAbsurdOffsets(t *testing.T) {
	chat := []source.ChatMessage{{Offset: 0}, {Offset: math.MaxInt64}}
	spikes, err := DetectSpikes(chat, DefaultSpikeParams())
	if err == nil {
		t.Fatalf("got %d spikes and no error for a message at offset %s", len(spikes), chat[1].Offset)
	}
	if !strings.Contains(err.Error(), "windows") {
		t.Errorf("error = %q, want it to mention the number of windows", err)
	}
}

// A planted burst of the synthetic chat: extra messages per second on top of
// the background, for a while.
type planted struct {
	at, length time.Duration
	rate       float64
}

func (b planted) end() time.Duration { return b.at + b.length }

// syntheticBursts are the reactions planted in the synthetic 3-hour chat.
// Most do not line up with the 5-second windows, and the third and fourth
// are two reactions a few seconds apart.
var syntheticBursts = []planted{
	{at: 12 * m, length: 10 * s, rate: 20},
	{at: 47*m + 32*s, length: 15 * s, rate: 15},
	{at: 91 * m, length: 8 * s, rate: 25},
	{at: 91*m + 21*s, length: 8 * s, rate: 20},
	{at: 135*m + 3*s, length: 20 * s, rate: 12},
	{at: 178 * m, length: 10 * s, rate: 30},
}

const syntheticLength = 3 * time.Hour

var (
	smallTalk = []string{"hello", "what a game", "gg", "nice", "who is winning?", "first time here", "that map again", "brb", "true", "no shot"}
	reactions = []string{"KEKW", "LUL", "OMEGALUL", "LMAO", "lol", "KEKW KEKW KEKW", "NO WAY", "LUL LUL", "clip it!"}
)

var syntheticWeights = map[string]float64{"KEKW": 2, "LUL": 2, "OMEGALUL": 2, "LMAO": 1.5, "lol": 1.5}

// syntheticChat generates a chat that behaves like a recorded one: a
// background rate that drifts between 1.5 and 4.5 messages a second over each
// hour, a rush of greetings in the first seconds, random arrival times, and the
// planted bursts, during which most messages are laughter. scale multiplies
// every rate.
//
// It stands in for a recorded chat so that no real chat log has to live in
// the repository. The generator uses its own random numbers and no
// floating-point functions, so the same seed gives the same chat on every
// platform and Go version, which the golden test relies on.
func syntheticChat(seed uint64, scale float64, bursts []planted) []source.ChatMessage {
	rng := splitMix(seed)
	var chat []source.ChatMessage
	for sec := range int(syntheticLength / s) {
		at := time.Duration(sec) * s

		phase := float64(sec%3600) / 1800 // 0..2 over an hour
		rate := 1.5 + 3*min(phase, 2-phase)
		if at < 15*s {
			rate += 12
		}
		extra := 0.0
		for _, b := range bursts {
			if at >= b.at && at < b.end() {
				extra += b.rate
			}
		}

		// One chance of a message in every millisecond.
		chance := (rate + extra) * scale / 1000
		for ms := range 1000 {
			if rng.float() >= chance {
				continue
			}
			words := smallTalk
			if extra > 0 && rng.float() < 0.8 {
				words = reactions
			}
			chat = append(chat, source.ChatMessage{
				Offset: at + time.Duration(ms)*time.Millisecond,
				User:   fmt.Sprintf("viewer%d", rng.next()%500),
				Text:   words[rng.next()%uint64(len(words))],
			})
		}
	}
	return chat
}

// splitMix is the SplitMix64 generator.
type splitMix uint64

func (r *splitMix) next() uint64 {
	*r += 0x9e3779b97f4a7c15
	z := uint64(*r)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// float returns a number in [0, 1).
func (r *splitMix) float() float64 { return float64(r.next()>>11) / (1 << 53) }

func syntheticParams() SpikeParams {
	return with(func(p *SpikeParams) {
		p.SkipStart = 2 * m
		p.Weights = syntheticWeights
	})
}

func TestDetectSpikesGolden(t *testing.T) {
	chat := syntheticChat(22, 1, syntheticBursts)
	params := syntheticParams()

	spikes, err := DetectSpikes(chat, params)
	if err != nil {
		t.Fatalf("DetectSpikes: %v", err)
	}
	checkInvariants(t, spikes, params)

	// Plausible: every planted reaction is found, nothing else is, and
	// the greetings at the start are not mistaken for a reaction.
	for _, b := range syntheticBursts {
		found := slices.ContainsFunc(spikes, func(sp Spike) bool {
			return sp.Start < b.end() && b.at < sp.End && sp.Peak >= b.at && sp.Peak <= b.end()
		})
		if !found {
			t.Errorf("no spike peaks inside the burst planted at %s", b.at)
		}
	}
	for _, sp := range spikes {
		inBurst := slices.ContainsFunc(syntheticBursts, func(b planted) bool {
			return sp.Start < b.end() && b.at < sp.End
		})
		if !inBurst {
			t.Errorf("spike [%s, %s) with score %.2f is not a planted burst", sp.Start, sp.End, sp.Score)
		}
	}

	// Without the skip, the greetings are the first thing found.
	unskipped, err := DetectSpikes(chat, with(func(p *SpikeParams) { p.Weights = syntheticWeights }))
	if err != nil {
		t.Fatalf("DetectSpikes without skip: %v", err)
	}
	if len(unskipped) == 0 || unskipped[0].Start >= m {
		t.Errorf("without a skipped start the greetings should be flagged, got %s", formatSpikes(unskipped))
	}

	// Stable: the order of the input does not matter, and the list is
	// the one recorded in the golden file.
	shuffled := slices.Clone(chat)
	rng := splitMix(7)
	for i := len(shuffled) - 1; i > 0; i-- {
		j := int(rng.next() % uint64(i+1))
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	fromShuffled, err := DetectSpikes(shuffled, params)
	if err != nil {
		t.Fatalf("DetectSpikes on shuffled chat: %v", err)
	}
	if !sameSpikes(fromShuffled, spikes) {
		t.Errorf("shuffled chat gives different spikes\nsorted:   %s\nshuffled: %s", formatSpikes(spikes), formatSpikes(fromShuffled))
	}

	got := fmt.Sprintf("# %d messages over %s%s\n", len(chat), syntheticLength, formatSpikes(spikes))
	golden := filepath.Join("testdata", "synthetic_chat_spikes.golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading the golden file (run the test with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("spikes differ from %s (run the test with -update to accept)\n--- got\n%s--- want\n%s", golden, got, want)
	}
}

// A 3-hour chat of a large channel: about 30 messages a second, well over
// three hundred thousand in all.
func largeChat() []source.ChatMessage { return syntheticChat(23, 10, syntheticBursts) }

func TestDetectSpikesThreeHourChatIsFast(t *testing.T) {
	chat := largeChat()
	if len(chat) < 300_000 {
		t.Fatalf("large chat has only %d messages", len(chat))
	}
	params := syntheticParams()

	begin := time.Now()
	spikes, err := DetectSpikes(chat, params)
	took := time.Since(begin)
	if err != nil {
		t.Fatalf("DetectSpikes: %v", err)
	}
	t.Logf("%d messages, %d spikes, %s", len(chat), len(spikes), took)

	if took > time.Second {
		t.Errorf("DetectSpikes took %s for %d messages, want well under a second", took, len(chat))
	}
	if len(spikes) != len(syntheticBursts) {
		t.Errorf("got %d spikes, want one for each of the %d planted bursts: %s", len(spikes), len(syntheticBursts), formatSpikes(spikes))
	}
}

func BenchmarkDetectSpikes(b *testing.B) {
	chat := largeChat()
	for _, bc := range []struct {
		name   string
		params SpikeParams
	}{
		{"unweighted", with(func(p *SpikeParams) { p.SkipStart = 2 * m })},
		{"weighted", syntheticParams()},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := DetectSpikes(chat, bc.params); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(chat)), "messages")
		})
	}
}
