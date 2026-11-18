package render

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Tests in this file run the real ffmpeg and ffprobe and skip without them.
// CI installs both. WhisperX is replaced by fakeTranscriber.

// needTools skips the test unless ffmpeg and ffprobe are in PATH.
func needTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not installed", name)
		}
	}
}

// generateSource writes a 1920x1080, 25 fps testsrc picture with a sine
// tone, seconds long, to path.
func generateSource(t *testing.T, path string, seconds int) {
	t.Helper()
	_, err := runner.Run(context.Background(), runner.Command{
		Name: "ffmpeg",
		Args: []string{
			"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=duration=" + strconv.Itoa(seconds) + ":size=1920x1080:rate=25",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + strconv.Itoa(seconds),
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
			path,
		},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
}

// streamCodecs is what an independent ffprobe run says about a file.
func streamCodecs(t *testing.T, path string) map[string]string {
	t.Helper()
	res, err := runner.Run(context.Background(), runner.Command{
		Name:    "ffprobe",
		Args:    []string{"-v", "error", "-show_entries", "stream=codec_type,codec_name,width,height,r_frame_rate", "-of", "json", path},
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Streams []map[string]any `json:"streams"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range out.Streams {
		kind, _ := s["codec_type"].(string)
		name, _ := s["codec_name"].(string)
		got[kind] = name
		if kind == "video" {
			got["size"] = strconv.Itoa(int(s["width"].(float64))) + "x" + strconv.Itoa(int(s["height"].(float64)))
			got["rate"], _ = s["r_frame_rate"].(string)
		}
	}
	return got
}

// TestRenderReal renders 1s to 4s of a generated 6-second 1920x1080 file
// with the real local-file downloader, layout, caption writer and ffmpeg,
// once per arrangement. The work directory's name needs every kind of
// filter graph escaping, so the caption path is checked by ffmpeg itself.
func TestRenderReal(t *testing.T) {
	needTools(t)
	input := filepath.Join(t.TempDir(), "stream.mp4")
	generateSource(t, input, 6)

	for _, tt := range []struct {
		name     string
		detector Detector
		want     layout.Arrangement
	}{
		{"blur fill", nil, layout.BlurFill},
		{"stack", fixedDetector{detection: layout.WebcamAt(layout.Region{X: 0.7, Y: 0.05, W: 0.25, H: 0.3})}, layout.Stack},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t)
			r, err := New(cfg, Options{Detector: tt.detector, Transcriber: &fakeTranscriber{transcript: fixedTranscript()}})
			if err != nil {
				t.Fatal(err)
			}
			workDir := filepath.Join(t.TempDir(), "work [dir]: it's, ok; yes")
			if err := os.Mkdir(workDir, 0o755); err != nil {
				t.Fatal(err)
			}

			res, err := r.Render(context.Background(), Request{Input: input, Start: time.Second, End: 4 * time.Second}, workDir)
			if err != nil {
				t.Fatal(err)
			}
			if res.Layout.Arrangement != tt.want {
				t.Errorf("arrangement %s, want %s", res.Layout.Arrangement, tt.want)
			}
			if res.Width != 1080 || res.Height != 1920 {
				t.Errorf("picture %dx%d", res.Width, res.Height)
			}
			if d := res.Duration - 3*time.Second; d < -DurationTolerance || d > DurationTolerance {
				t.Errorf("duration %s, want about 3s", res.Duration)
			}
			fi, err := os.Stat(res.Path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Size() != res.Size || res.Size <= 0 || res.Size > cfg.Render.MaxBytes {
				t.Errorf("size %d (file %d), limit %d", res.Size, fi.Size(), cfg.Render.MaxBytes)
			}
			if res.SourceWidth != 1920 || res.SourceHeight != 1080 {
				t.Errorf("source %dx%d", res.SourceWidth, res.SourceHeight)
			}
			if res.CaptionLines != 2 || len(res.Transcript.Words) != 5 {
				t.Errorf("captions %d, words %d", res.CaptionLines, len(res.Transcript.Words))
			}
			if res.Tools["ffmpeg"] == "" || len(res.Steps) != 7 || res.VideoBitrateCap <= 0 {
				t.Errorf("result incomplete: tools %v, steps %+v, cap %d", res.Tools, res.Steps, res.VideoBitrateCap)
			}
			for _, f := range []string{TranscriptFile, CaptionFile, DetectionFile} {
				if _, err := os.Stat(filepath.Join(workDir, f)); err != nil {
					t.Errorf("work file: %v", err)
				}
			}

			got := streamCodecs(t, res.Path)
			want := map[string]string{"video": "h264", "audio": "aac", "size": "1080x1920", "rate": "30/1"}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("ffprobe %s: got %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
			t.Logf("summary:\n%s", res.Summary())
		})
	}
}
