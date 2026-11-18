package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// FramesDir is the directory, inside the work directory, that holds the
// sampled frames (frame-01.jpg, ...).
const FramesDir = "detect"

// Frame is one sampled frame and the faces found in it.
type Frame struct {
	// Time is where the frame is, in seconds from the start of the clip.
	Time float64 `json:"time"`
	Image
}

// Report is everything one detection found: the answer, and the frames,
// faces and groups behind it, for detection.json and detect.jpg.
type Report struct {
	// Detection is the answer the layout uses.
	Detection layout.Detection `json:"-"`
	// Frames are the sampled frames, in time order, with their faces.
	Frames []Frame `json:"frames"`
	// Groups are the faces followed across frames.
	Groups []Group `json:"groups"`
	// Chosen is the index in Groups of the face chosen, or -1.
	Chosen int `json:"chosen_group"`
	// Steady is the number of frames a face needed to be steady.
	Steady int `json:"steady_frames"`
}

// DebugFrame is the index of the frame best suited to draw the decision on:
// the frame with the most faces among those holding the chosen face (among
// all frames when none was chosen), the one nearest the middle of the clip
// on a tie. It is -1 when no frame was read.
func (r Report) DebugFrame() int {
	best, bestFaces, bestDist := -1, -1, len(r.Frames)
	mid := (len(r.Frames) - 1) / 2
	for i, f := range r.Frames {
		if f.Error != "" || f.Width <= 0 {
			continue
		}
		if r.Chosen >= 0 && !hasGroup(f.Faces, r.Chosen) {
			continue
		}
		dist := max(i-mid, mid-i)
		if len(f.Faces) > bestFaces || (len(f.Faces) == bestFaces && dist < bestDist) {
			best, bestFaces, bestDist = i, len(f.Faces), dist
		}
	}
	return best
}

func hasGroup(faces []Face, g int) bool {
	for _, f := range faces {
		if f.Group == g {
			return true
		}
	}
	return false
}

// Options are the parts of a Detector that can be replaced.
type Options struct {
	// Finder finds the faces. Nil means NewYuNet(cfg).
	Finder FaceFinder
}

// Detector finds the streamer's face in a clip: it samples frames with
// ffmpeg, has a FaceFinder look for faces in them, and decides with Decide.
// It is safe for concurrent use; each clip needs its own work directory.
type Detector struct {
	cfg    config.ClipperWebcamDetection
	rules  Rules
	finder FaceFinder
}

// New returns the Detector for the config section. It checks the section
// but starts nothing; Check is the startup check.
func New(cfg config.ClipperWebcamDetection, opts Options) (*Detector, error) {
	if problems := cfg.Validate(); len(problems) > 0 {
		return nil, fmt.Errorf("detect: %s", strings.Join(problems, "; "))
	}
	d := &Detector{cfg: cfg, rules: RulesFrom(cfg), finder: opts.Finder}
	if d.finder == nil {
		d.finder = NewYuNet(cfg)
	}
	return d, nil
}

// Name is the finder's name, "yunet" for the real one.
func (d *Detector) Name() string { return d.finder.Name() }

// Detect finds the face in video, which is length long, keeping the frames
// in workDir/FramesDir. The whole detection is bounded by the configured
// timeout. Every failure is an error, which the caller treats as "no face";
// a timeout says so.
func (d *Detector) Detect(ctx context.Context, video string, length time.Duration, workDir string) (Report, error) {
	if length <= 0 {
		return Report{}, fmt.Errorf("detect: the clip has no length (%s)", length)
	}
	inner, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	rep, err := d.detect(inner, video, length, workDir)
	if err != nil && ctx.Err() == nil && errors.Is(inner.Err(), context.DeadlineExceeded) {
		return rep, fmt.Errorf("detect: timed out after %s: %w", d.cfg.Timeout, err)
	}
	return rep, err
}

func (d *Detector) detect(ctx context.Context, video string, length time.Duration, workDir string) (Report, error) {
	dir := filepath.Join(workDir, FramesDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Report{}, fmt.Errorf("detect: %w", err)
	}
	times := FrameTimes(length, d.cfg.Frames)
	paths := make([]string, len(times))
	for i, at := range times {
		paths[i] = filepath.Join(dir, fmt.Sprintf("frame-%02d.jpg", i+1))
		// Never take an earlier run's frame for this one's.
		if err := os.Remove(paths[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Report{}, fmt.Errorf("detect: %w", err)
		}
		if _, err := runner.Run(ctx, runner.Command{Name: d.cfg.FFmpegPath, Args: frameArgs(video, at, paths[i])}); err != nil {
			return Report{}, fmt.Errorf("detect: extract the frame at %s: %w", at, err)
		}
		if fi, err := os.Stat(paths[i]); err != nil || fi.Size() == 0 {
			return Report{}, fmt.Errorf("detect: ffmpeg wrote no frame at %s", at)
		}
	}
	images, err := d.finder.FindFaces(ctx, paths)
	if err != nil {
		return Report{}, fmt.Errorf("detect: %w", err)
	}
	if len(images) != len(paths) {
		return Report{}, fmt.Errorf("detect: %s answered for %d frames, asked for %d", d.finder.Name(), len(images), len(paths))
	}
	dec := Decide(images, d.rules)
	rep := Report{Detection: dec.Detection, Groups: dec.Groups, Chosen: dec.Chosen, Steady: dec.Steady}
	if rep.Groups == nil {
		rep.Groups = []Group{}
	}
	for i, img := range dec.Images {
		rep.Frames = append(rep.Frames, Frame{Time: float64(times[i].Milliseconds()) / 1000, Image: img})
	}
	if dec.Analysed == 0 {
		return rep, fmt.Errorf("detect: no frame could be read: %s", images[0].Error)
	}
	return rep, nil
}

// FrameTimes spreads n frames evenly across a clip of the given length: the
// middle of each of n equal parts, rounded to the millisecond. No frame is
// at the very start or end, where a cut or a fade is likely.
func FrameTimes(length time.Duration, n int) []time.Duration {
	times := make([]time.Duration, n)
	for i := range n {
		times[i] = (length * time.Duration(2*i+1) / time.Duration(2*n)).Round(time.Millisecond)
	}
	return times
}

// frameArgs is the ffmpeg command line that writes the frame at the given
// time of video to out as a full-size JPEG. -ss before -i seeks; ffmpeg
// decodes from the keyframe before and drops the frames before the time.
func frameArgs(video string, at time.Duration, out string) []string {
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(at.Seconds(), 'f', 3, 64),
		"-i", video,
		"-map", "0:v:0", "-frames:v", "1", "-q:v", "2", "-update", "1",
		out,
	}
}
