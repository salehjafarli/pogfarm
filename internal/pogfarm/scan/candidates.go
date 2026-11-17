package scan

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// CandidateParams configures BuildCandidates. Start from
// DefaultCandidateParams and change what needs changing; the zero value is
// not valid.
//
// The yaml tags let the struct be embedded in a config section as is.
// Durations are written like "25s" or "1m".
type CandidateParams struct {
	// PreRoll is how far before a chat spike its clip window starts. Chat
	// reacts a few seconds after the moment it reacts to, so the moment
	// itself lies before the spike.
	PreRoll time.Duration `yaml:"pre_roll"`
	// PostRoll is how far after the peak of a chat spike its clip window
	// ends.
	PostRoll time.Duration `yaml:"post_roll"`
	// ClipPadding is added before and after the range of a viewer clip to
	// get its clip window.
	ClipPadding time.Duration `yaml:"clip_padding"`
	// MinLength is the shortest clip window. A shorter window is widened
	// evenly on both sides, as far as the VOD allows. It must be positive.
	MinLength time.Duration `yaml:"min_length"`
	// MaxLength is the longest clip window. A longer window is cut to
	// MaxLength starting at the window of its strongest signal. It must be
	// at least MinLength.
	MaxLength time.Duration `yaml:"max_length"`
	// MaxPerVOD is how many candidates of one VOD are rendered: the
	// highest-scoring ones. The rest are stored as skipped. It must be at
	// least 1.
	MaxPerVOD int `yaml:"max_per_vod"`
	// ViewerClipScore is the score of a viewer clip with no views. A clip
	// with v views scores ViewerClipScore * (1 + log10(1 + v)), so 9 views
	// double it and 99 views triple it. The default puts an unwatched
	// viewer clip level with the weakest chat spike the default spike
	// parameters flag. It must be positive.
	ViewerClipScore float64 `yaml:"viewer_clip_score"`
}

// DefaultCandidateParams returns the parameters used when nothing is
// configured: chat spike windows from 25 seconds before the spike to 5
// seconds after its peak, viewer clips padded by 2 seconds on each side,
// windows between 15 and 60 seconds long, the top 5 candidates of a VOD
// rendered and a viewer clip scoring 3 plus more for its views.
func DefaultCandidateParams() CandidateParams {
	return CandidateParams{
		PreRoll:         25 * time.Second,
		PostRoll:        5 * time.Second,
		ClipPadding:     2 * time.Second,
		MinLength:       15 * time.Second,
		MaxLength:       60 * time.Second,
		MaxPerVOD:       5,
		ViewerClipScore: 3,
	}
}

// Validate reports everything that is wrong with p, joined into one error,
// or nil when p is usable.
func (p CandidateParams) Validate() error {
	var errs []error
	if p.PreRoll < 0 {
		errs = append(errs, fmt.Errorf("pre_roll must not be negative, got %s", p.PreRoll))
	}
	if p.PostRoll < 0 {
		errs = append(errs, fmt.Errorf("post_roll must not be negative, got %s", p.PostRoll))
	}
	if p.ClipPadding < 0 {
		errs = append(errs, fmt.Errorf("clip_padding must not be negative, got %s", p.ClipPadding))
	}
	if p.MinLength <= 0 {
		errs = append(errs, fmt.Errorf("min_length must be positive, got %s", p.MinLength))
	}
	if p.MaxLength < p.MinLength {
		errs = append(errs, fmt.Errorf("max_length must be at least min_length (%s), got %s", p.MinLength, p.MaxLength))
	}
	if p.MaxPerVOD < 1 {
		errs = append(errs, fmt.Errorf("max_per_vod must be at least 1, got %d", p.MaxPerVOD))
	}
	if math.IsNaN(p.ViewerClipScore) || math.IsInf(p.ViewerClipScore, 0) || p.ViewerClipScore <= 0 {
		errs = append(errs, fmt.Errorf("viewer_clip_score must be a positive number, got %v", p.ViewerClipScore))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("scan: invalid candidate parameters: %w", errors.Join(errs...))
}

// Candidate is a moment of a VOD worth rendering.
type Candidate struct {
	// Start and End are the clip window, measured from the start of the
	// VOD. The window covers [Start, End).
	Start time.Duration
	End   time.Duration
	// Score ranks the candidates of one VOD: the score of its strongest
	// chat spike plus the score of its strongest viewer clip, either being
	// 0 when it has none. A moment both signals confirm therefore
	// outscores each of them alone.
	Score float64
	// Signal says which kinds of signal the window was built from.
	Signal store.Signal
	// Keep is true for the candidates within CandidateParams.MaxPerVOD,
	// which are rendered.
	Keep bool
}

// BuildCandidates turns the chat spikes and viewer clips of one VOD into
// ranked clip windows.
//
//  1. A chat spike becomes the window from p.PreRoll before its Start to
//     p.PostRoll after its Peak. A viewer clip becomes its own range with
//     p.ClipPadding added on both sides.
//  2. Each window is clamped to the VOD, [0, vodDuration), and widened to
//     p.MinLength. A vodDuration of zero or less means the length is
//     unknown and only the start is clamped. Windows wholly outside the VOD
//     are dropped, as are spikes with End <= Start or a score that is not a
//     finite number of at least zero.
//  3. Windows that overlap (touching is not overlapping) are merged into
//     one, which is then cut to p.MaxLength starting at the window of its
//     highest-scoring signal (the earliest on a tie).
//  4. The results are ordered by Score, highest first, then by Start and
//     End; the first p.MaxPerVOD have Keep set.
//
// The result does not depend on the order of the inputs, which are not
// modified. Candidates never overlap. It is nil when there are none.
func BuildCandidates(spikes []Spike, clips []source.ViewerClip, vodDuration time.Duration, p CandidateParams) ([]Candidate, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	b := candBounds{end: vodDuration, min: p.MinLength}

	windows := make([]candWindow, 0, len(spikes)+len(clips))
	for _, s := range spikes {
		if s.End <= s.Start || math.IsNaN(s.Score) || math.IsInf(s.Score, 0) || s.Score < 0 {
			continue
		}
		if w, ok := b.fit(s.Start-p.PreRoll, s.Peak+p.PostRoll); ok {
			w.score = s.Score
			windows = append(windows, w)
		}
	}
	for _, c := range clips {
		dur := max(c.Duration, 0)
		if w, ok := b.fit(c.Offset-p.ClipPadding, c.Offset+dur+p.ClipPadding); ok {
			w.isClip = true
			w.score = p.ViewerClipScore * (1 + math.Log10(1+float64(max(c.Views, 0))))
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, nil
	}

	// Sorting on every field makes the merge independent of input order.
	slices.SortFunc(windows, func(a, b candWindow) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end),
			candCompareBool(a.isClip, b.isClip), cmp.Compare(a.score, b.score))
	})

	var out []Candidate
	group := candGroup{start: windows[0].start, end: windows[0].end}
	group.add(windows[0])
	for _, w := range windows[1:] {
		if w.start < group.end {
			group.end = max(group.end, w.end)
			group.add(w)
			continue
		}
		out = append(out, group.candidate(p.MaxLength))
		group = candGroup{start: w.start, end: w.end}
		group.add(w)
	}
	out = append(out, group.candidate(p.MaxLength))

	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End))
	})
	for i := range out[:min(len(out), p.MaxPerVOD)] {
		out[i].Keep = true
	}
	return out, nil
}

// candWindow is the clip window of one signal: a chat spike when isClip is
// false, a viewer clip when it is true. score is the signal's score.
type candWindow struct {
	start, end time.Duration
	isClip     bool
	score      float64
}

// candBounds clamps windows to a VOD and widens them to the minimum length.
type candBounds struct {
	end time.Duration // end of the VOD; <= 0 when unknown
	min time.Duration
}

// fit returns [start, end) clamped to the VOD and widened to the minimum
// length, and false when nothing of it lies inside the VOD.
func (b candBounds) fit(start, end time.Duration) (candWindow, bool) {
	start = max(start, 0)
	if b.end > 0 {
		end = min(end, b.end)
	}
	if end <= start {
		return candWindow{}, false
	}
	if n := end - start; n < b.min {
		start -= (b.min - n) / 2
		end = start + b.min
		if start < 0 {
			start, end = 0, b.min
		}
		if b.end > 0 && end > b.end {
			start, end = max(0, b.end-b.min), b.end
		}
	}
	return candWindow{start: start, end: end}, true
}

// candGroup collects overlapping windows into one candidate.
type candGroup struct {
	start, end time.Duration
	// chat and clip are the best scores of each kind of signal; hasChat
	// and hasClip say whether there is one.
	chat, clip       float64
	hasChat, hasClip bool
	// anchor is the start of the highest-scoring window; it was added
	// first among equals because windows arrive ordered by start.
	anchor     time.Duration
	best       float64
	hasWindows bool
}

func (g *candGroup) add(w candWindow) {
	if w.isClip {
		g.clip, g.hasClip = max(g.clip, w.score), true
	} else {
		g.chat, g.hasChat = max(g.chat, w.score), true
	}
	if !g.hasWindows || w.score > g.best {
		g.anchor, g.best, g.hasWindows = w.start, w.score, true
	}
}

// candidate returns the group as a candidate cut to maxLen.
func (g *candGroup) candidate(maxLen time.Duration) Candidate {
	start, end := g.start, g.end
	if end-start > maxLen {
		start = min(max(g.anchor, start), end-maxLen)
		end = start + maxLen
	}
	signal := store.SignalBoth
	switch {
	case !g.hasClip:
		signal = store.SignalChatSpike
	case !g.hasChat:
		signal = store.SignalViewerClip
	}
	return Candidate{Start: start, End: end, Score: g.chat + g.clip, Signal: signal}
}

// candCompareBool orders false before true.
func candCompareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}
