package scan

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

const candS = time.Second

// candParams returns parameters with round numbers that make expected
// windows easy to work out: pre-roll 20s, post-roll 5s, padding 2s, length
// 10s to 60s, top 3, viewer clip score 3.
func candParams(change func(*CandidateParams)) CandidateParams {
	p := CandidateParams{
		PreRoll:         20 * candS,
		PostRoll:        5 * candS,
		ClipPadding:     2 * candS,
		MinLength:       10 * candS,
		MaxLength:       60 * candS,
		MaxPerVOD:       3,
		ViewerClipScore: 3,
	}
	if change != nil {
		change(&p)
	}
	return p
}

// candSpike is a 5-second spike starting at start, peaking in its middle.
func candSpike(start time.Duration, score float64) Spike {
	return Spike{Start: start, End: start + 5*candS, Peak: start + 2500*time.Millisecond, Score: score, Messages: 10}
}

// candClip is a viewer clip of [offset, offset+dur) with views views.
func candClip(offset, dur time.Duration, views int) source.ViewerClip {
	return source.ViewerClip{Offset: offset, Duration: dur, Views: views, Title: "clip"}
}

// candClipScore is the score candParams gives a viewer clip with views views.
func candClipScore(views int) float64 { return 3 * (1 + math.Log10(1+float64(views))) }

func TestDefaultCandidateParams(t *testing.T) {
	p := DefaultCandidateParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("defaults are invalid: %v", err)
	}
	if p.PreRoll < 20*candS || p.PreRoll > 30*candS {
		t.Errorf("default pre-roll %s, want about 25s", p.PreRoll)
	}
}

func TestCandidateParamsValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*CandidateParams)
		want   []string // substrings of the error; nil for valid
	}{
		{name: "valid", change: nil},
		{name: "zero paddings are fine", change: func(p *CandidateParams) {
			p.PreRoll, p.PostRoll, p.ClipPadding = 0, 0, 0
		}},
		{name: "min equals max", change: func(p *CandidateParams) { p.MaxLength = p.MinLength }},
		{name: "negative pre-roll", change: func(p *CandidateParams) { p.PreRoll = -1 }, want: []string{"pre_roll"}},
		{name: "negative post-roll", change: func(p *CandidateParams) { p.PostRoll = -1 }, want: []string{"post_roll"}},
		{name: "negative padding", change: func(p *CandidateParams) { p.ClipPadding = -1 }, want: []string{"clip_padding"}},
		{name: "zero min length", change: func(p *CandidateParams) { p.MinLength = 0 }, want: []string{"min_length"}},
		{name: "max below min", change: func(p *CandidateParams) { p.MaxLength = p.MinLength - 1 }, want: []string{"max_length"}},
		{name: "zero top-N", change: func(p *CandidateParams) { p.MaxPerVOD = 0 }, want: []string{"max_per_vod"}},
		{name: "zero clip score", change: func(p *CandidateParams) { p.ViewerClipScore = 0 }, want: []string{"viewer_clip_score"}},
		{name: "NaN clip score", change: func(p *CandidateParams) { p.ViewerClipScore = math.NaN() }, want: []string{"viewer_clip_score"}},
		{name: "everything reported", change: func(p *CandidateParams) {
			*p = CandidateParams{PreRoll: -1, MaxLength: -1}
		}, want: []string{"pre_roll", "min_length", "max_length", "max_per_vod", "viewer_clip_score"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := candParams(tt.change).Validate()
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate: nil, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Validate: %v, want it to mention %q", err, w)
				}
			}
		})
	}
	if _, err := BuildCandidates(nil, nil, 0, CandidateParams{}); err == nil {
		t.Error("BuildCandidates with zero parameters: nil error, want one")
	}
}

func TestBuildCandidates(t *testing.T) {
	const vod = 3600 * candS
	tests := []struct {
		name   string
		spikes []Spike
		clips  []source.ViewerClip
		dur    time.Duration
		params CandidateParams
		want   []Candidate
	}{
		{
			name:   "no spikes and no clips",
			dur:    vod,
			params: candParams(nil),
			want:   nil,
		},
		{
			name:   "spike gets pre-roll and post-roll",
			spikes: []Spike{candSpike(100*candS, 4)},
			dur:    vod,
			params: candParams(nil),
			// [100-20, 102.5+5)
			want: []Candidate{{Start: 80 * candS, End: 107500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true}},
		},
		{
			name:   "default pre-roll starts well before the spike",
			spikes: []Spike{candSpike(100*candS, 4)},
			dur:    vod,
			params: DefaultCandidateParams(),
			want:   []Candidate{{Start: 75 * candS, End: 107500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true}},
		},
		{
			name:   "clip gets padding on both sides",
			clips:  []source.ViewerClip{candClip(200*candS, 30*candS, 99)},
			dur:    vod,
			params: candParams(nil),
			want:   []Candidate{{Start: 198 * candS, End: 232 * candS, Score: candClipScore(99), Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "unwatched clip scores viewer clip score",
			clips:  []source.ViewerClip{candClip(200*candS, 30*candS, 0)},
			dur:    vod,
			params: candParams(nil),
			want:   []Candidate{{Start: 198 * candS, End: 232 * candS, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "spike and clip on the same moment become one candidate with both",
			spikes: []Spike{candSpike(100*candS, 4)},
			clips:  []source.ViewerClip{candClip(85*candS, 30*candS, 9)},
			dur:    vod,
			params: candParams(nil),
			// spike [80, 107.5), clip [83, 117): union [80, 117).
			want: []Candidate{{Start: 80 * candS, End: 117 * candS, Score: 4 + candClipScore(9), Signal: store.SignalBoth, Keep: true}},
		},
		{
			name:   "both outranks stronger single signals",
			spikes: []Spike{candSpike(100*candS, 4), candSpike(1000*candS, 5)},
			clips:  []source.ViewerClip{candClip(85*candS, 30*candS, 0), candClip(2000*candS, 30*candS, 0)},
			dur:    vod,
			params: candParams(nil),
			want: []Candidate{
				{Start: 80 * candS, End: 117 * candS, Score: 7, Signal: store.SignalBoth, Keep: true},
				{Start: 980 * candS, End: 1007500 * time.Millisecond, Score: 5, Signal: store.SignalChatSpike, Keep: true},
				{Start: 1998 * candS, End: 2032 * candS, Score: 3, Signal: store.SignalViewerClip, Keep: true},
			},
		},
		{
			name:   "overlapping spikes merge and keep the best score",
			spikes: []Spike{candSpike(100*candS, 4), candSpike(115*candS, 6)},
			dur:    vod,
			params: candParams(nil),
			// [80, 107.5) and [95, 122.5): union [80, 122.5).
			want: []Candidate{{Start: 80 * candS, End: 122500 * time.Millisecond, Score: 6, Signal: store.SignalChatSpike, Keep: true}},
		},
		{
			name:   "touching windows do not merge",
			spikes: []Spike{candSpike(100*candS, 4), candSpike(127500*time.Millisecond, 4)},
			dur:    vod,
			params: candParams(nil),
			// [80, 107.5) and [107.5, 135).
			want: []Candidate{
				{Start: 80 * candS, End: 107500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true},
				{Start: 107500 * time.Millisecond, End: 135 * candS, Score: 4, Signal: store.SignalChatSpike, Keep: true},
			},
		},
		{
			name: "merged window longer than max is cut at its strongest signal",
			spikes: []Spike{
				candSpike(100*candS, 3), candSpike(120*candS, 3), candSpike(140*candS, 9), candSpike(160*candS, 3),
			},
			dur:    vod,
			params: candParams(func(p *CandidateParams) { p.MaxLength = 40 * candS }),
			// union [80, 167.5); strongest window starts at 120.
			want: []Candidate{{Start: 120 * candS, End: 160 * candS, Score: 9, Signal: store.SignalChatSpike, Keep: true}},
		},
		{
			name: "cut does not run past the end of the merged window",
			spikes: []Spike{
				candSpike(100*candS, 3), candSpike(115*candS, 3), candSpike(130*candS, 9),
			},
			dur:    vod,
			params: candParams(func(p *CandidateParams) { p.MaxLength = 40 * candS }),
			// union [80, 137.5); strongest starts at 110, so the cut
			// is the last 40s.
			want: []Candidate{{Start: 97500 * time.Millisecond, End: 137500 * time.Millisecond, Score: 9, Signal: store.SignalChatSpike, Keep: true}},
		},
		{
			name:   "long viewer clip is cut to max length",
			clips:  []source.ViewerClip{candClip(100*candS, 120*candS, 0)},
			dur:    vod,
			params: candParams(nil),
			want:   []Candidate{{Start: 98 * candS, End: 158 * candS, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "short window is widened to min length",
			clips:  []source.ViewerClip{candClip(100*candS, 2*candS, 0)},
			dur:    vod,
			params: candParams(func(p *CandidateParams) { p.MinLength = 20 * candS }),
			// [98, 104) is 6s; widened by 7s on each side.
			want: []Candidate{{Start: 91 * candS, End: 111 * candS, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "clamped at the start of the VOD",
			spikes: []Spike{candSpike(10*candS, 4)},
			dur:    vod,
			params: candParams(nil),
			// [-10, 17.5) becomes [0, 17.5).
			want: []Candidate{{Start: 0, End: 17500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true}},
		},
		{
			name:   "widening at the start of the VOD shifts right",
			clips:  []source.ViewerClip{candClip(0, 2*candS, 0)},
			dur:    vod,
			params: candParams(func(p *CandidateParams) { p.MinLength = 20 * candS }),
			want:   []Candidate{{Start: 0, End: 20 * candS, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "clamped at the end of the VOD",
			clips:  []source.ViewerClip{candClip(3580*candS, 30*candS, 0)},
			dur:    vod,
			params: candParams(nil),
			want:   []Candidate{{Start: 3578 * candS, End: vod, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "widening at the end of the VOD shifts left",
			clips:  []source.ViewerClip{candClip(3599*candS, 30*candS, 0)},
			dur:    vod,
			params: candParams(func(p *CandidateParams) { p.MinLength = 20 * candS }),
			want:   []Candidate{{Start: 3580 * candS, End: vod, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "VOD shorter than min length gives the whole VOD",
			clips:  []source.ViewerClip{candClip(2*candS, 2*candS, 0)},
			dur:    8 * candS,
			params: candParams(nil),
			want:   []Candidate{{Start: 0, End: 8 * candS, Score: 3, Signal: store.SignalViewerClip, Keep: true}},
		},
		{
			name:   "signals past the end of the VOD are dropped",
			spikes: []Spike{candSpike(3700*candS, 4)},
			clips:  []source.ViewerClip{candClip(3600*candS, 30*candS, 0)},
			dur:    vod,
			params: candParams(func(p *CandidateParams) { p.PreRoll = 0; p.ClipPadding = 0 }),
			want:   nil,
		},
		{
			name:   "unknown VOD length only clamps the start",
			spikes: []Spike{candSpike(10*candS, 4), candSpike(99000*candS, 5)},
			dur:    0,
			params: candParams(nil),
			want: []Candidate{
				{Start: 98980 * candS, End: 99007500 * time.Millisecond, Score: 5, Signal: store.SignalChatSpike, Keep: true},
				{Start: 0, End: 17500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true},
			},
		},
		{
			name: "malformed spikes are ignored",
			spikes: []Spike{
				{Start: 100 * candS, End: 100 * candS, Peak: 100 * candS, Score: 4},
				{Start: 200 * candS, End: 205 * candS, Peak: 202 * candS, Score: math.NaN()},
				{Start: 300 * candS, End: 305 * candS, Peak: 302 * candS, Score: -1},
			},
			dur:    vod,
			params: candParams(nil),
			want:   nil,
		},
		{
			name: "top-N keeps the best and ties go to the earlier window",
			spikes: []Spike{
				candSpike(500*candS, 4), candSpike(100*candS, 4), candSpike(900*candS, 8),
				candSpike(300*candS, 2), candSpike(700*candS, 4),
			},
			dur:    vod,
			params: candParams(nil),
			want: []Candidate{
				{Start: 880 * candS, End: 907500 * time.Millisecond, Score: 8, Signal: store.SignalChatSpike, Keep: true},
				{Start: 80 * candS, End: 107500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true},
				{Start: 480 * candS, End: 507500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: true},
				{Start: 680 * candS, End: 707500 * time.Millisecond, Score: 4, Signal: store.SignalChatSpike, Keep: false},
				{Start: 280 * candS, End: 307500 * time.Millisecond, Score: 2, Signal: store.SignalChatSpike, Keep: false},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spikes, clips := slices.Clone(tt.spikes), slices.Clone(tt.clips)
			got, err := BuildCandidates(spikes, clips, tt.dur, tt.params)
			if err != nil {
				t.Fatalf("BuildCandidates: %v", err)
			}
			candCheck(t, got, tt.want)
			if !slices.Equal(spikes, tt.spikes) && !candHasNaN(tt.spikes) {
				t.Error("BuildCandidates modified its spikes")
			}
			if !slices.Equal(clips, tt.clips) {
				t.Error("BuildCandidates modified its clips")
			}
			candCheckInvariants(t, got, tt.dur, tt.params)

			// Reversed and shuffled inputs give the same result.
			rng := rand.New(rand.NewPCG(1, 2))
			for i := range 5 {
				s, c := slices.Clone(tt.spikes), slices.Clone(tt.clips)
				if i == 0 {
					slices.Reverse(s)
					slices.Reverse(c)
				} else {
					rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
					rng.Shuffle(len(c), func(a, b int) { c[a], c[b] = c[b], c[a] })
				}
				again, err := BuildCandidates(s, c, tt.dur, tt.params)
				if err != nil {
					t.Fatalf("BuildCandidates on reordered input: %v", err)
				}
				candCheck(t, again, tt.want)
			}
		})
	}
}

// TestBuildCandidatesFromDetectedSpikes runs the builder on what the
// detector finds in a planted burst, the way the scanner chains them.
func TestBuildCandidatesFromDetectedSpikes(t *testing.T) {
	chat := join(busy(), burst(10*m, 10*m+5*s, 60, "LUL"))
	spikes, err := DetectSpikes(chat, DefaultSpikeParams())
	if err != nil {
		t.Fatal(err)
	}
	if len(spikes) != 1 {
		t.Fatalf("DetectSpikes found %d spikes, want 1", len(spikes))
	}
	got, err := BuildCandidates(spikes, []source.ViewerClip{candClip(10*m-20*s, 30*s, 50)}, 30*m, DefaultCandidateParams())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signal != store.SignalBoth || !got[0].Keep {
		t.Fatalf("got %+v, want one kept candidate with both signals", got)
	}
	if got[0].Start > 10*m-20*s || got[0].End <= 10*m {
		t.Errorf("window [%s, %s) does not cover the moment before the burst at %s", got[0].Start, got[0].End, 10*m)
	}
}

// candCheck compares candidates, allowing for rounding in the scores.
func candCheck(t *testing.T, got, want []Candidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d:\n got %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.Start != w.Start || g.End != w.End || g.Signal != w.Signal || g.Keep != w.Keep ||
			math.Abs(g.Score-w.Score) > 1e-9 {
			t.Errorf("candidate %d:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

// candCheckInvariants checks what holds for every result: windows inside
// the VOD, within the length limits where the VOD allows, not overlapping,
// ranked, and the top N kept.
func candCheckInvariants(t *testing.T, got []Candidate, dur time.Duration, p CandidateParams) {
	t.Helper()
	for i, c := range got {
		if c.Start < 0 || c.End <= c.Start || (dur > 0 && c.End > dur) {
			t.Errorf("candidate %d [%s, %s) is outside a VOD of %s", i, c.Start, c.End, dur)
		}
		if n := c.End - c.Start; n > p.MaxLength || (n < p.MinLength && (dur <= 0 || dur >= p.MinLength)) {
			t.Errorf("candidate %d is %s long, want %s to %s", i, n, p.MinLength, p.MaxLength)
		}
		if c.Keep != (i < p.MaxPerVOD) {
			t.Errorf("candidate %d: Keep %v with max_per_vod %d", i, c.Keep, p.MaxPerVOD)
		}
		if i > 0 && c.Score > got[i-1].Score {
			t.Errorf("candidate %d scores %v, more than the one before it", i, c.Score)
		}
		for j := range i {
			if o := got[j]; c.Start < o.End && o.Start < c.End {
				t.Errorf("candidates %d and %d overlap", j, i)
			}
		}
	}
}

func candHasNaN(spikes []Spike) bool {
	return slices.ContainsFunc(spikes, func(s Spike) bool { return math.IsNaN(s.Score) })
}
