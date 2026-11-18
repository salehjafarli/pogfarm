//go:build linux || darwin

package detect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

func TestFrameTimes(t *testing.T) {
	ms := time.Millisecond
	tests := []struct {
		length time.Duration
		n      int
		want   []time.Duration
	}{
		{8 * time.Second, 8, []time.Duration{500 * ms, 1500 * ms, 2500 * ms, 3500 * ms, 4500 * ms, 5500 * ms, 6500 * ms, 7500 * ms}},
		{15 * time.Second, 2, []time.Duration{3750 * ms, 11250 * ms}},
		{time.Second, 3, []time.Duration{167 * ms, 500 * ms, 833 * ms}},
	}
	for _, tt := range tests {
		got := FrameTimes(tt.length, tt.n)
		if len(got) != len(tt.want) {
			t.Fatalf("%s/%d: %v", tt.length, tt.n, got)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s/%d: %v, want %v", tt.length, tt.n, got, tt.want)
				break
			}
		}
	}
}

func TestFrameArgs(t *testing.T) {
	got := strings.Join(frameArgs("/w/source.mp4", 3750*time.Millisecond, "/w/detect/frame-02.jpg"), " ")
	want := "-hide_banner -nostdin -loglevel error -y -ss 3.750 -i /w/source.mp4 -map 0:v:0 -frames:v 1 -q:v 2 -update 1 /w/detect/frame-02.jpg"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestDebugFrame(t *testing.T) {
	face := func(g int) Face { return Face{Group: g, Kept: g >= 0} }
	frame := func(faces ...Face) Frame { return Frame{Image: Image{Width: 1920, Height: 1080, Faces: faces}} }
	tests := []struct {
		name string
		rep  Report
		want int
	}{
		{"no frames", Report{Chosen: -1}, -1},
		{"nothing read", Report{Chosen: -1, Frames: []Frame{{Image: Image{Error: "x"}}}}, -1},
		{"no faces: the middle", Report{Chosen: -1, Frames: []Frame{frame(), frame(), frame(), frame()}}, 1},
		{"most faces", Report{Chosen: -1, Frames: []Frame{frame(face(0)), frame(), frame(), frame(face(0), face(1))}}, 3},
		{"the chosen face first", Report{Chosen: 0, Frames: []Frame{frame(face(0)), frame(face(1), face(2)), frame(face(0), face(1)), frame()}}, 2},
	}
	for _, tt := range tests {
		if got := tt.rep.DebugFrame(); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
}

// fakeFinder answers a face at the same pixel box in every image, records
// the images it was asked about, and can fail or wait for its context.
type fakeFinder struct {
	face  *Face
	err   error
	block bool
	asked []string
}

func (*fakeFinder) Name() string { return "fake" }

func (f *fakeFinder) FindFaces(ctx context.Context, images []string) ([]Image, error) {
	f.asked = images
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Image, len(images))
	for i, p := range images {
		out[i] = Image{Path: p, Width: 1920, Height: 1080, Faces: []Face{}}
		if f.face != nil {
			out[i].Faces = []Face{*f.face}
		}
	}
	return out, nil
}

// fakeFFmpeg writes a script that writes a placeholder file at its last
// argument, as ffmpeg writes the frame.
func fakeFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nfor a in \"$@\"; do last=\"$a\"; done\nprintf 'jpeg' > \"$last\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDetectWithFakes(t *testing.T) {
	cfg := detectionConfig(t)
	cfg.FFmpegPath = fakeFFmpeg(t)
	corner := &Face{X: 1640, Y: 60, W: 64, H: 84, Confidence: 0.9}

	t.Run("corner face", func(t *testing.T) {
		finder := &fakeFinder{face: corner}
		d, err := New(cfg, Options{Finder: finder})
		if err != nil {
			t.Fatal(err)
		}
		workDir := t.TempDir()
		rep, err := d.Detect(context.Background(), "/w/source.mp4", 16*time.Second, workDir)
		if err != nil {
			t.Fatal(err)
		}
		if d.Name() != "fake" || len(finder.asked) != 8 || finder.asked[0] != filepath.Join(workDir, FramesDir, "frame-01.jpg") {
			t.Errorf("name %q, asked %q", d.Name(), finder.asked)
		}
		if !rep.Detection.Face || rep.Detection.Arrangement != layout.Stack || rep.Chosen != 0 || rep.Steady != 5 {
			t.Errorf("report: %+v", rep)
		}
		if len(rep.Frames) != 8 || rep.Frames[0].Time != 1 || rep.Frames[7].Time != 15 || !rep.Frames[3].Faces[0].Kept {
			t.Errorf("frames: %+v", rep.Frames)
		}
		if got := rep.DebugFrame(); got != 3 {
			t.Errorf("debug frame %d, want 3", got)
		}
	})
	t.Run("no face", func(t *testing.T) {
		d, _ := New(cfg, Options{Finder: &fakeFinder{}})
		rep, err := d.Detect(context.Background(), "/w/source.mp4", 4*time.Second, t.TempDir())
		if err != nil || rep.Detection.Face || rep.Detection.Reason != layout.ReasonNoFace || rep.Groups == nil {
			t.Errorf("got %+v, %v", rep, err)
		}
	})
	t.Run("finder fails", func(t *testing.T) {
		boom := errors.New("opencv exploded")
		d, _ := New(cfg, Options{Finder: &fakeFinder{err: boom}})
		if _, err := d.Detect(context.Background(), "/w/source.mp4", 4*time.Second, t.TempDir()); !errors.Is(err, boom) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		c := cfg
		c.Timeout = 50 * time.Millisecond
		d, _ := New(c, Options{Finder: &fakeFinder{block: true}})
		_, err := d.Detect(context.Background(), "/w/source.mp4", 4*time.Second, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("cancelled is not a timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d, _ := New(cfg, Options{Finder: &fakeFinder{block: true}})
		_, err := d.Detect(ctx, "/w/source.mp4", 4*time.Second, t.TempDir())
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("ffmpeg missing", func(t *testing.T) {
		c := cfg
		c.FFmpegPath = filepath.Join(t.TempDir(), "no-ffmpeg")
		d, _ := New(c, Options{Finder: &fakeFinder{}})
		if _, err := d.Detect(context.Background(), "/w/source.mp4", 4*time.Second, t.TempDir()); err == nil ||
			!strings.Contains(err.Error(), "extract the frame") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no length", func(t *testing.T) {
		d, _ := New(cfg, Options{Finder: &fakeFinder{}})
		if _, err := d.Detect(context.Background(), "/w/source.mp4", 0, t.TempDir()); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("bad config", func(t *testing.T) {
		c := cfg
		c.Frames = 0
		if _, err := New(c, Options{}); err == nil || !strings.Contains(err.Error(), "frames") {
			t.Errorf("got %v", err)
		}
	})
}

type unreadableFinder struct{}

func (unreadableFinder) Name() string { return "unreadable" }

func (unreadableFinder) FindFaces(_ context.Context, images []string) ([]Image, error) {
	out := make([]Image, len(images))
	for i := range out {
		out[i] = Image{Path: images[i], Error: "cannot read image"}
	}
	return out, nil
}

func TestDetectNothingReadable(t *testing.T) {
	cfg := detectionConfig(t)
	cfg.FFmpegPath = fakeFFmpeg(t)
	d, _ := New(cfg, Options{Finder: unreadableFinder{}})
	if _, err := d.Detect(context.Background(), "/w/source.mp4", 4*time.Second, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "no frame could be read: cannot read image") {
		t.Errorf("got %v", err)
	}
}

// TestDetectRealFFmpeg extracts the frames of a generated clip with the
// real ffmpeg; the faces come from a fake finder. It skips without ffmpeg.
func TestDetectRealFFmpeg(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "source.mp4")
	if _, err := runner.Run(context.Background(), runner.Command{Name: "ffmpeg", Args: []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=1920x884:rate=25:duration=4", "-pix_fmt", "yuv420p", video,
	}, Timeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := detectionConfig(t)
	finder := &fakeFinder{face: &Face{X: 1640, Y: 60, W: 64, H: 84, Confidence: 0.9}}
	d, err := New(cfg, Options{Finder: finder})
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	rep, err := d.Detect(context.Background(), video, 4*time.Second, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(finder.asked) != 8 || !rep.Detection.Face {
		t.Fatalf("asked %q, report %+v", finder.asked, rep)
	}
	for _, p := range finder.asked {
		res, err := runner.Run(context.Background(), runner.Command{Name: "ffprobe", Args: []string{
			"-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", p,
		}, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "mjpeg,1920,884" {
			t.Errorf("%s: %s, want a full-size JPEG", p, got)
		}
	}
	if fi, err := os.Stat(filepath.Join(workDir, FramesDir, "frame-0"+strconv.Itoa(8)+".jpg")); err != nil || fi.Size() == 0 {
		t.Errorf("last frame: %v", err)
	}
}
