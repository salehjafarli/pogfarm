package layout

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/runner"
)

// TestFragmentRunsInFFmpeg runs every fragment through a real ffmpeg and
// checks the output with ffprobe. It skips when ffmpeg or ffprobe is not
// installed; CI installs both.
func TestFragmentRunsInFFmpeg(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	l := newLayouter(t, defaults)
	detections := []struct {
		name string
		d    Detection
		want Arrangement
	}{
		{"stack", FaceAt(corner, Stack, ""), Stack},
		{"stack clamped", FaceAt(Box{X: 0.97, Y: -0.02, W: 0.05, H: 0.08}, Stack, ""), Stack},
		{"face crop", FaceAt(big, FaceCrop, ""), FaceCrop},
		{"face crop at the edge", FaceAt(Box{X: 0.9, Y: 0.6, W: 0.1, H: 0.25}, FaceCrop, ""), FaceCrop},
		{"blur fill", NoFace(ReasonNoFace, ""), BlurFill},
	}
	for _, size := range [][2]int{{1920, 1080}, {1280, 720}, {1920, 884}} {
		dir := t.TempDir()
		input := filepath.Join(dir, "source.mp4")
		ffmpeg(t, "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=10:duration=1", size[0], size[1]),
			"-pix_fmt", "yuv420p", input)
		for _, tt := range detections {
			t.Run(fmt.Sprintf("%dx%d %s", size[0], size[1], tt.name), func(t *testing.T) {
				r, err := l.Plan(size[0], size[1], tt.d)
				if err != nil {
					t.Fatal(err)
				}
				if r.Arrangement != tt.want {
					t.Fatalf("got %q (%s), want %q", r.Arrangement, r.Fallback, tt.want)
				}
				output := filepath.Join(t.TempDir(), "out.mp4")
				ffmpeg(t, "-i", input, "-filter_complex", r.Filter, "-map", "["+OutputLabel+"]",
					"-pix_fmt", "yuv420p", output)
				res, err := runner.Run(context.Background(), runner.Command{
					Name: "ffprobe",
					Args: []string{"-v", "error", "-select_streams", "v:0",
						"-show_entries", "stream=width,height,sample_aspect_ratio", "-of", "csv=p=0", output},
					Timeout: time.Minute,
				})
				if err != nil {
					t.Fatal(err)
				}
				got := strings.TrimSpace(string(res.Stdout))
				if got != "1080,1920,1:1" {
					t.Errorf("ffprobe width,height,sar: got %q, want 1080,1920,1:1", got)
				}
			})
		}
	}
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"-nostdin", "-hide_banner", "-v", "error", "-y"}, args...)
	if _, err := runner.Run(context.Background(), runner.Command{Name: "ffmpeg", Args: full, Timeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
}
