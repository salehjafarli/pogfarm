package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Encoding of the trimmed range. It is an intermediate file that the render
// encodes again, so it favours quality and speed over size.
const (
	trimPreset       = "veryfast"
	trimCRF          = "18"
	trimAudioBitrate = "192k"
)

// tools runs ffmpeg and ffprobe.
type tools struct {
	ffmpeg  string
	ffprobe string
	// timeout gives the timeout of a tool working on a range of the given
	// length.
	timeout func(length time.Duration) time.Duration
}

// seconds formats d for ffmpeg, in seconds with up to microsecond precision.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Round(time.Microsecond).Seconds(), 'f', -1, 64)
}

// trimArgs is the ffmpeg command line that cuts [start, start+length) out of
// in, re-encoding so that both ends are frame-accurate.
//
// -ss before -i seeks the input; because the streams are re-encoded, ffmpeg
// decodes from the keyframe before start and drops every frame before it
// (accurate seek, the default). -t after -i limits the output to length.
func trimArgs(in, out string, start, length time.Duration) []string {
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-ss", seconds(start),
		"-i", in,
		"-t", seconds(length),
		"-map", "0:v:0", "-map", "0:a:0",
		"-c:v", "libx264", "-preset", trimPreset, "-crf", trimCRF, "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", trimAudioBitrate,
		"-avoid_negative_ts", "make_zero",
		"-movflags", "+faststart",
		out,
	}
}

func (t tools) trim(ctx context.Context, in, out string, start, length time.Duration) error {
	_, err := runner.Run(ctx, runner.Command{
		Name:    t.ffmpeg,
		Args:    trimArgs(in, out, start, length),
		Timeout: t.timeout(length),
	})
	if err != nil {
		return fmt.Errorf("download: trim with ffmpeg: %w", err)
	}
	return nil
}

// probeArgs is the ffprobe command line that reports the streams and the
// duration of path as JSON.
func probeArgs(path string) []string {
	return []string{
		"-v", "error",
		"-show_entries", "format=duration:stream=codec_type,duration",
		"-of", "json",
		path,
	}
}

// probe is what ffprobe reports about a file.
type probe struct {
	// Duration is the container's duration: the longest stream.
	Duration time.Duration
	HasVideo bool
	HasAudio bool
	// VideoDuration is the first video stream's own duration, zero when
	// the container does not record it.
	VideoDuration time.Duration
}

type probeOutput struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func parseProbe(out []byte) (probe, error) {
	var po probeOutput
	if err := json.Unmarshal(out, &po); err != nil {
		return probe{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	var p probe
	for _, s := range po.Streams {
		switch s.CodecType {
		case "video":
			if !p.HasVideo {
				// Absent or "N/A" in some containers: leave it zero.
				p.VideoDuration, _ = parseSeconds(s.Duration)
			}
			p.HasVideo = true
		case "audio":
			p.HasAudio = true
		}
	}
	if po.Format.Duration != "" {
		d, err := parseSeconds(po.Format.Duration)
		if err != nil {
			return probe{}, fmt.Errorf("parse ffprobe output: %w", err)
		}
		p.Duration = d
	}
	return p, nil
}

// parseSeconds parses a duration ffprobe gives in seconds.
func parseSeconds(s string) (time.Duration, error) {
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 {
		return 0, fmt.Errorf("duration %q is not a number of seconds", s)
	}
	return time.Duration(sec * float64(time.Second)), nil
}

func (t tools) probe(ctx context.Context, path string) (probe, error) {
	res, err := runner.Run(ctx, runner.Command{
		Name:    t.ffprobe,
		Args:    probeArgs(path),
		Timeout: t.timeout(0),
	})
	if err != nil {
		return probe{}, fmt.Errorf("download: ffprobe %s: %w", path, err)
	}
	p, err := parseProbe(res.Stdout)
	if err != nil {
		return probe{}, fmt.Errorf("download: ffprobe %s: %w", path, err)
	}
	return p, nil
}

// errVerify is the cause of a result that failed verification. It is
// retryable.
var errVerify = errors.New("result failed verification")

// checkProbe checks a result: video and audio present, and a duration within
// DurationTolerance of want, or just positive when want is zero.
func checkProbe(p probe, want time.Duration) error {
	switch {
	case !p.HasVideo:
		return fmt.Errorf("no video stream: %w", errVerify)
	case !p.HasAudio:
		return fmt.Errorf("no audio stream: %w", errVerify)
	case p.Duration <= 0:
		return fmt.Errorf("duration is zero: %w", errVerify)
	case want > 0 && (p.Duration < want-DurationTolerance || p.Duration > want+DurationTolerance):
		return fmt.Errorf("duration %s, want %s within %s: %w", p.Duration, want, DurationTolerance, errVerify)
	}
	return nil
}

// verify checks the file at path and returns the Result for it.
func (t tools) verify(ctx context.Context, path string, want time.Duration) (Result, error) {
	if fi, err := os.Stat(path); err != nil {
		return Result{}, fmt.Errorf("download: result %s: %w", path, err)
	} else if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return Result{}, fmt.Errorf("download: result %s is empty or not a file: %w", path, errVerify)
	}
	p, err := t.probe(ctx, path)
	if err != nil {
		return Result{}, err
	}
	if err := checkProbe(p, want); err != nil {
		return Result{}, fmt.Errorf("download: result %s: %w", path, err)
	}
	return Result{Path: path, Duration: p.Duration}, nil
}
