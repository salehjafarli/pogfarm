package scan

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// The config package repeats the defaults and the validation rules of
// SpikeParams and CandidateParams, because it cannot import this package.
// These tests keep the copies in step.

// loadScanConfig loads pogfarm's configuration with the given scan section
// (YAML, indented under pogfarm.scan) and returns it, or the validation
// problems.
func loadScanConfig(t *testing.T, scanYAML string) (*config.Pogfarm, []string) {
	t.Helper()
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv("POGFARM_API_SECRET", "a-secret-that-is-long-enough")
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "pogfarm:\n  scan:\n" + scanYAML
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadPogfarm(path)
	if err == nil {
		return cfg, nil
	}
	ve, ok := err.(*config.ValidationError)
	if !ok {
		t.Fatalf("load: %v", err)
	}
	return nil, ve.Problems
}

func TestConfigDefaultsMatchScanDefaults(t *testing.T) {
	cfg, problems := loadScanConfig(t, "    workers: 1\n")
	if problems != nil {
		t.Fatal(problems)
	}
	s := NewScanner(nil, nil, nil, cfg.Scan, nil)
	if !reflect.DeepEqual(s.Spike, DefaultSpikeParams()) {
		t.Errorf("config spike defaults %+v, scan defaults %+v", s.Spike, DefaultSpikeParams())
	}
	if !reflect.DeepEqual(s.Candidates, DefaultCandidateParams()) {
		t.Errorf("config candidate defaults %+v, scan defaults %+v", s.Candidates, DefaultCandidateParams())
	}
	if s.Workers != DefaultScanWorkers || s.Interval != DefaultScanInterval || s.MaxAttempts != DefaultScanAttempts ||
		s.RetryBackoff != DefaultRetryBackoff || s.MaxRetryBackoff != DefaultMaxRetryBackoff {
		t.Errorf("config loop defaults differ from the scanner's: %+v", cfg.Scan)
	}
	if cfg.Discovery.Interval != DefaultDiscoveryInterval {
		t.Errorf("config discovery interval %s, scanner default %s", cfg.Discovery.Interval, DefaultDiscoveryInterval)
	}
}

func TestConfigValidatesParamsLikeScan(t *testing.T) {
	cases := []struct {
		yaml   string
		spike  func(*SpikeParams)
		cand   func(*CandidateParams)
		reject bool
	}{
		{yaml: "spike: {window: 0s}", spike: func(p *SpikeParams) { p.Window = 0 }, reject: true},
		{yaml: "spike: {window: 150s}", spike: func(p *SpikeParams) { p.Window = 150 * time.Second }},
		{yaml: "spike: {window: 151s}", spike: func(p *SpikeParams) { p.Window = 151 * time.Second }, reject: true},
		{yaml: "spike: {baseline: 10s}", spike: func(p *SpikeParams) { p.Baseline = 10 * time.Second }},
		{yaml: "spike: {baseline: 9s}", spike: func(p *SpikeParams) { p.Baseline = 9 * time.Second }, reject: true},
		{yaml: "spike: {baseline: 0s}", spike: func(p *SpikeParams) { p.Baseline = 0 }, reject: true},
		{yaml: "spike: {factor: 1}", spike: func(p *SpikeParams) { p.Factor = 1 }},
		{yaml: "spike: {factor: 0.99}", spike: func(p *SpikeParams) { p.Factor = 0.99 }, reject: true},
		{yaml: "spike: {factor: .nan}", spike: func(p *SpikeParams) { p.Factor = math.NaN() }, reject: true},
		{yaml: "spike: {min_messages: 0}", spike: func(p *SpikeParams) { p.MinMessages = 0 }, reject: true},
		{yaml: "spike: {merge_gap: -1s}", spike: func(p *SpikeParams) { p.MergeGap = -time.Second }, reject: true},
		{yaml: "spike: {merge_gap: 0s, skip_start: 0s}", spike: func(*SpikeParams) {}},
		{yaml: "spike: {skip_start: -1s}", spike: func(p *SpikeParams) { p.SkipStart = -time.Second }, reject: true},
		{yaml: "spike: {weights: {lol: 0, LOL: 0}}", spike: func(p *SpikeParams) { p.Weights = map[string]float64{"lol": 0, "LOL": 0} }},
		{yaml: "spike: {weights: {lol: 1, LOL: 2}}", spike: func(p *SpikeParams) { p.Weights = map[string]float64{"lol": 1, "LOL": 2} }, reject: true},
		{yaml: "spike: {weights: {\"a b\": 1}}", spike: func(p *SpikeParams) { p.Weights = map[string]float64{"a b": 1} }, reject: true},
		{yaml: "spike: {weights: {\"\": 1}}", spike: func(p *SpikeParams) { p.Weights = map[string]float64{"": 1} }, reject: true},
		{yaml: "spike: {weights: {x: -1}}", spike: func(p *SpikeParams) { p.Weights = map[string]float64{"x": -1} }, reject: true},
		{yaml: "candidates: {pre_roll: -1s}", cand: func(p *CandidateParams) { p.PreRoll = -time.Second }, reject: true},
		{yaml: "candidates: {post_roll: -1s}", cand: func(p *CandidateParams) { p.PostRoll = -time.Second }, reject: true},
		{yaml: "candidates: {clip_padding: -1s}", cand: func(p *CandidateParams) { p.ClipPadding = -time.Second }, reject: true},
		{yaml: "candidates: {pre_roll: 0s, post_roll: 0s, clip_padding: 0s}", cand: func(*CandidateParams) {}},
		{yaml: "candidates: {min_length: 0s}", cand: func(p *CandidateParams) { p.MinLength = 0 }, reject: true},
		{yaml: "candidates: {max_length: 15s}", cand: func(p *CandidateParams) { p.MaxLength = 15 * time.Second }},
		{yaml: "candidates: {max_length: 14s}", cand: func(p *CandidateParams) { p.MaxLength = 14 * time.Second }, reject: true},
		{yaml: "candidates: {max_per_vod: 0}", cand: func(p *CandidateParams) { p.MaxPerVOD = 0 }, reject: true},
		{yaml: "candidates: {viewer_clip_score: 0}", cand: func(p *CandidateParams) { p.ViewerClipScore = 0 }, reject: true},
		{yaml: "candidates: {viewer_clip_score: .inf}", cand: func(p *CandidateParams) { p.ViewerClipScore = math.Inf(1) }, reject: true},
		{yaml: "candidates: {viewer_clip_score: 0.1}", cand: func(p *CandidateParams) { p.ViewerClipScore = 0.1 }},
	}
	for _, tc := range cases {
		t.Run(tc.yaml, func(t *testing.T) {
			spike, cand := DefaultSpikeParams(), DefaultCandidateParams()
			if tc.spike != nil {
				tc.spike(&spike)
			}
			if tc.cand != nil {
				tc.cand(&cand)
			}
			scanRejects := spike.Validate() != nil || cand.Validate() != nil
			if scanRejects != tc.reject {
				t.Fatalf("scan rejects: %v, want %v", scanRejects, tc.reject)
			}

			_, problems := loadScanConfig(t, "    "+tc.yaml+"\n")
			if configRejects := problems != nil; configRejects != tc.reject {
				t.Errorf("config rejects: %v, want %v (%s)", configRejects, tc.reject, strings.Join(problems, "; "))
			}
		})
	}
}
