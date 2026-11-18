package config

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Scan is the "scan" section of the main service's configuration: the
// scanner loop that turns recorded VODs into candidates and render jobs.
type Scan struct {
	// Interval is how long the scanner waits, when no VOD is due, before it
	// looks again.
	Interval time.Duration `yaml:"interval" env:"POGFARM_SCAN_INTERVAL"`
	// Workers is how many VODs are scanned at the same time.
	Workers int `yaml:"workers" env:"POGFARM_SCAN_WORKERS"`
	// MaxAttempts is how many scans of one VOD are started before a VOD
	// whose scans keep failing is given up.
	MaxAttempts int `yaml:"max_attempts" env:"POGFARM_SCAN_MAX_ATTEMPTS"`
	// RetryBackoff is the wait before the first retry of a failed scan. It
	// doubles with every further failure, up to MaxRetryBackoff.
	RetryBackoff time.Duration `yaml:"retry_backoff" env:"POGFARM_SCAN_RETRY_BACKOFF"`
	// MaxRetryBackoff caps the wait between two scans of one VOD.
	MaxRetryBackoff time.Duration `yaml:"max_retry_backoff" env:"POGFARM_SCAN_MAX_RETRY_BACKOFF"`

	// ChatSpikes switches chat spike detection on or off. Off, the chat of
	// a VOD is not downloaded at all.
	ChatSpikes bool `yaml:"chat_spikes" env:"POGFARM_SCAN_CHAT_SPIKES"`
	// ViewerClips switches viewer-made clips on or off as a signal. Off,
	// the platform is not asked for them. At least one of the two is on.
	ViewerClips bool `yaml:"viewer_clips" env:"POGFARM_SCAN_VIEWER_CLIPS"`

	// Spike tunes chat spike detection. File only.
	Spike SpikeParams `yaml:"spike"`
	// Candidates tunes how spikes and viewer clips become clip windows.
	// File only.
	Candidates CandidateParams `yaml:"candidates"`
}

// SpikeParams is the "scan.spike" section. It has the fields of
// scan.SpikeParams, which documents them, and converts to it. It is defined
// here because package scan imports this package.
type SpikeParams struct {
	Window      time.Duration      `yaml:"window"`
	Baseline    time.Duration      `yaml:"baseline"`
	Factor      float64            `yaml:"factor"`
	MinMessages int                `yaml:"min_messages"`
	MergeGap    time.Duration      `yaml:"merge_gap"`
	SkipStart   time.Duration      `yaml:"skip_start"`
	Weights     map[string]float64 `yaml:"weights"`
}

// CandidateParams is the "scan.candidates" section. It has the fields of
// scan.CandidateParams, which documents them, and converts to it.
type CandidateParams struct {
	PreRoll         time.Duration `yaml:"pre_roll"`
	PostRoll        time.Duration `yaml:"post_roll"`
	ClipPadding     time.Duration `yaml:"clip_padding"`
	MinLength       time.Duration `yaml:"min_length"`
	MaxLength       time.Duration `yaml:"max_length"`
	MaxPerVOD       int           `yaml:"max_per_vod"`
	ViewerClipScore float64       `yaml:"viewer_clip_score"`
}

// defaultScan is the scan section when nothing is configured. The spike and
// candidate parameters are those of scan.DefaultSpikeParams and
// scan.DefaultCandidateParams; a test in package scan keeps them equal.
func defaultScan() Scan {
	return Scan{
		Interval:        time.Minute,
		Workers:         1,
		MaxAttempts:     5,
		RetryBackoff:    10 * time.Minute,
		MaxRetryBackoff: 6 * time.Hour,
		ChatSpikes:      true,
		ViewerClips:     true,
		Spike: SpikeParams{
			Window:      5 * time.Second,
			Baseline:    5 * time.Minute,
			Factor:      3,
			MinMessages: 8,
		},
		Candidates: CandidateParams{
			PreRoll:         25 * time.Second,
			PostRoll:        5 * time.Second,
			ClipPadding:     2 * time.Second,
			MinLength:       15 * time.Second,
			MaxLength:       60 * time.Second,
			MaxPerVOD:       5,
			ViewerClipScore: 3,
		},
	}
}

// validate checks the scan section. The rules for the spike and candidate
// parameters are those of their Validate methods in package scan, repeated
// here so every problem is reported at startup under its config key; a test
// in package scan keeps the two in step.
func (s Scan) validate() []string {
	var problems []string
	positive := func(key, env string, d time.Duration) {
		if d <= 0 {
			problems = append(problems, fmt.Sprintf(
				"pogfarm.scan.%s (%s) must be a positive duration: got %s", key, env, d))
		}
	}
	positive("interval", "POGFARM_SCAN_INTERVAL", s.Interval)
	if s.Workers < 1 {
		problems = append(problems, fmt.Sprintf("pogfarm.scan.workers (POGFARM_SCAN_WORKERS) must be at least 1: got %d", s.Workers))
	}
	if s.MaxAttempts < 1 {
		problems = append(problems, fmt.Sprintf("pogfarm.scan.max_attempts (POGFARM_SCAN_MAX_ATTEMPTS) must be at least 1: got %d", s.MaxAttempts))
	}
	positive("retry_backoff", "POGFARM_SCAN_RETRY_BACKOFF", s.RetryBackoff)
	if s.MaxRetryBackoff < s.RetryBackoff {
		problems = append(problems, fmt.Sprintf(
			"pogfarm.scan.max_retry_backoff (POGFARM_SCAN_MAX_RETRY_BACKOFF) must be at least retry_backoff (%s): got %s",
			s.RetryBackoff, s.MaxRetryBackoff))
	}
	if !s.ChatSpikes && !s.ViewerClips {
		problems = append(problems, "pogfarm.scan.chat_spikes (POGFARM_SCAN_CHAT_SPIKES) and pogfarm.scan.viewer_clips "+
			"(POGFARM_SCAN_VIEWER_CLIPS) are both off: the scanner would never find a moment to clip; turn at least one on")
	}
	problems = append(problems, s.Spike.validate()...)
	problems = append(problems, s.Candidates.validate()...)
	return problems
}

func (p SpikeParams) validate() []string {
	const key = "pogfarm.scan.spike."
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, key+fmt.Sprintf(format, args...)) }
	if p.Window <= 0 {
		add("window must be positive: got %s", p.Window)
	}
	if p.Baseline <= 0 {
		add("baseline must be positive: got %s", p.Baseline)
	} else if p.Window > 0 && p.Baseline/2 < p.Window {
		add("baseline must be at least twice the window (%s): got %s", p.Window, p.Baseline)
	}
	if !finite(p.Factor) || p.Factor < 1 {
		add("factor must be a number of at least 1: got %v", p.Factor)
	}
	if p.MinMessages < 1 {
		add("min_messages must be at least 1: got %d", p.MinMessages)
	}
	if p.MergeGap < 0 {
		add("merge_gap must not be negative: got %s", p.MergeGap)
	}
	if p.SkipStart < 0 {
		add("skip_start must not be negative: got %s", p.SkipStart)
	}
	// Sorted, so the problems come out in the same order every time.
	keys := make([]string, 0, len(p.Weights))
	for k := range p.Weights {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	lower := make(map[string]float64, len(keys))
	for _, k := range keys {
		v := p.Weights[k]
		switch {
		case k == "" || strings.ContainsFunc(k, unicode.IsSpace):
			add("weights: key %q must be one word", k)
		case !finite(v) || v < 0:
			add("weights: weight of %q must be a number of at least 0: got %v", k, v)
		default:
			l := strings.ToLower(k)
			if prev, ok := lower[l]; ok && prev != v {
				add("weights: %q is given more than once, in different case, with different weights", l)
			}
			lower[l] = v
		}
	}
	return problems
}

func (p CandidateParams) validate() []string {
	const key = "pogfarm.scan.candidates."
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, key+fmt.Sprintf(format, args...)) }
	if p.PreRoll < 0 {
		add("pre_roll must not be negative: got %s", p.PreRoll)
	}
	if p.PostRoll < 0 {
		add("post_roll must not be negative: got %s", p.PostRoll)
	}
	if p.ClipPadding < 0 {
		add("clip_padding must not be negative: got %s", p.ClipPadding)
	}
	if p.MinLength <= 0 {
		add("min_length must be positive: got %s", p.MinLength)
	}
	if p.MaxLength < p.MinLength {
		add("max_length must be at least min_length (%s): got %s", p.MinLength, p.MaxLength)
	}
	if p.MaxPerVOD < 1 {
		add("max_per_vod must be at least 1: got %d", p.MaxPerVOD)
	}
	if !finite(p.ViewerClipScore) || p.ViewerClipScore <= 0 {
		add("viewer_clip_score must be a positive number: got %v", p.ViewerClipScore)
	}
	return problems
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
