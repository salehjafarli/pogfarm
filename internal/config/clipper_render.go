package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// MinRenderMaxBytes is the smallest clip size limit accepted: 1 MiB. A
// smaller one leaves no room for a watchable clip of any length.
const MinRenderMaxBytes = 1 << 20

// RenderPresets are the libx264 presets accepted for clipper.render.preset,
// fastest first.
var RenderPresets = []string{
	"ultrafast", "superfast", "veryfast", "faster", "fast",
	"medium", "slow", "slower", "veryslow",
}

// ClipperRender is the "clipper.render" section: the final encode of a clip
// (package internal/clipper/render), the one ffmpeg run that applies the
// layout, burns in the captions and writes the 1080x1920 H.264/AAC file.
type ClipperRender struct {
	// FFmpegPath is the ffmpeg program of the final encode: a path, or a
	// name looked up in PATH.
	FFmpegPath string `yaml:"ffmpeg_path" env:"CLIPPER_RENDER_FFMPEG_PATH"`
	// FFprobePath is the ffprobe program that reads the source's picture
	// size and checks the finished clip.
	FFprobePath string `yaml:"ffprobe_path" env:"CLIPPER_RENDER_FFPROBE_PATH"`
	// Preset is the libx264 preset: slower presets give a smaller file at
	// the same quality and take longer.
	Preset string `yaml:"preset" env:"CLIPPER_RENDER_PRESET"`
	// CRF is libx264's constant rate factor, 0 to 51: lower is better
	// quality and a bigger file. The size cap (MaxBytes) can lower the
	// quality further on a long clip.
	CRF int `yaml:"crf" env:"CLIPPER_RENDER_CRF"`
	// FrameRate is the clip's constant frame rate, in frames per second.
	FrameRate int `yaml:"frame_rate" env:"CLIPPER_RENDER_FRAME_RATE"`
	// MaxBytes is the largest clip file allowed, in bytes. The video
	// bitrate is capped from it and the clip's length, and a clip above it
	// fails the render's check. The default stays under Telegram's 50 MB
	// upload limit for bots, because every clip is sent there for review.
	MaxBytes int64 `yaml:"max_bytes" env:"CLIPPER_RENDER_MAX_BYTES"`
	// TimeoutBase and TimeoutPerSecond bound the final encode: ffmpeg is
	// killed after TimeoutBase plus TimeoutPerSecond for every second of
	// the clip. ffprobe runs get TimeoutBase.
	TimeoutBase      time.Duration `yaml:"timeout_base" env:"CLIPPER_RENDER_TIMEOUT_BASE"`
	TimeoutPerSecond time.Duration `yaml:"timeout_per_second" env:"CLIPPER_RENDER_TIMEOUT_PER_SECOND"`
}

// defaultClipperRender is the render section when nothing is configured.
func defaultClipperRender() ClipperRender {
	return ClipperRender{
		FFmpegPath:       "ffmpeg",
		FFprobePath:      "ffprobe",
		Preset:           "fast",
		CRF:              21,
		FrameRate:        30,
		MaxBytes:         45_000_000,
		TimeoutBase:      2 * time.Minute,
		TimeoutPerSecond: 10 * time.Second,
	}
}

func (r ClipperRender) validate() []string {
	var problems []string
	notEmpty := func(key, env, v string) {
		if strings.TrimSpace(v) == "" {
			problems = append(problems, fmt.Sprintf("clipper.render.%s (%s) must not be empty", key, env))
		}
	}
	notEmpty("ffmpeg_path", "CLIPPER_RENDER_FFMPEG_PATH", r.FFmpegPath)
	notEmpty("ffprobe_path", "CLIPPER_RENDER_FFPROBE_PATH", r.FFprobePath)
	if !slices.Contains(RenderPresets, r.Preset) {
		problems = append(problems, fmt.Sprintf(
			"clipper.render.preset (CLIPPER_RENDER_PRESET) must be one of %s: got %q",
			strings.Join(RenderPresets, ", "), r.Preset))
	}
	if r.CRF < 0 || r.CRF > 51 {
		problems = append(problems, fmt.Sprintf(
			"clipper.render.crf (CLIPPER_RENDER_CRF) must be between 0 and 51: got %d", r.CRF))
	}
	if r.FrameRate < 10 || r.FrameRate > 60 {
		problems = append(problems, fmt.Sprintf(
			"clipper.render.frame_rate (CLIPPER_RENDER_FRAME_RATE) must be between 10 and 60: got %d", r.FrameRate))
	}
	if r.MaxBytes < MinRenderMaxBytes {
		problems = append(problems, fmt.Sprintf(
			"clipper.render.max_bytes (CLIPPER_RENDER_MAX_BYTES) must be at least %d bytes: got %d", MinRenderMaxBytes, r.MaxBytes))
	}
	if r.TimeoutBase <= 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.render.timeout_base (CLIPPER_RENDER_TIMEOUT_BASE) must be a positive duration such as \"2m\": got %s", r.TimeoutBase))
	}
	if r.TimeoutPerSecond <= 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.render.timeout_per_second (CLIPPER_RENDER_TIMEOUT_PER_SECOND) must be a positive duration such as \"10s\": got %s", r.TimeoutPerSecond))
	}
	return problems
}

// Validate reports every problem of the section, in the words of the config
// file. Package render uses it to check a section built in code.
func (r ClipperRender) Validate() []string { return r.validate() }

// LoadClipperRender loads the clipper's configuration for clipper render,
// which renders one clip by hand and never talks to pogfarm. It reads the
// file and the environment exactly like LoadClipper, but validates only the
// sections a render uses (layout, download, transcribe, caption, render and
// webcam_detection): pogfarm_url, api_secret, scratch_dir and name are
// neither required nor checked. clipper work uses LoadClipper, which checks
// everything.
func LoadClipperRender(path string) (*Clipper, error) {
	f := defaults()
	if err := readFile(resolvePath(path), &f); err != nil {
		return nil, err
	}
	cfg := &f.Clipper
	problems := applyEnv(cfg)
	problems = append(problems, cfg.validateSteps()...)
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return cfg, nil
}
