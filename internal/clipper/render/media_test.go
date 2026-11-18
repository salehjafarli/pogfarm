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

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
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

// cornerFinder stands in for detect-faces, which CI does not have: it
// answers a webcam face in the top-right corner of every frame, 8% of the
// height, and a face elsewhere that moves from frame to frame, as faces in
// a game do.
type cornerFinder struct{}

func (cornerFinder) Name() string { return "corner" }

func (cornerFinder) FindFaces(_ context.Context, images []string) ([]detect.Image, error) {
	out := make([]detect.Image, len(images))
	for i, p := range images {
		out[i] = detect.Image{Path: p, Width: 1920, Height: 1080, Faces: []detect.Face{
			{X: 1632, Y: 54, W: 96, H: 86, Confidence: 0.92},
			{X: 200 + 150*float64(i), Y: 500, W: 90, H: 120, Confidence: 0.8},
			{X: 900, Y: 900, W: 40, H: 50, Confidence: 0.55},
		}}
	}
	return out, nil
}

// TestRenderReal renders 1s to 4s of a generated 6-second 1920x1080 file
// with the real local-file downloader, layout, caption writer and ffmpeg,
// once per arrangement, the detector faked. The work directory's name needs
// every kind of filter graph escaping, so the caption path is checked by
// ffmpeg itself. The "detected" case runs the real detect.Detector, whose
// frames ffmpeg extracts, with cornerFinder in place of detect-faces, and
// checks detect.jpg and detection.json.
func TestRenderReal(t *testing.T) {
	needTools(t)
	input := filepath.Join(t.TempDir(), "stream.mp4")
	generateSource(t, input, 6)

	detCfg := testConfig(t).WebcamDetection
	detCfg.FFmpegPath = "ffmpeg"
	detected, err := detect.New(detCfg, detect.Options{Finder: cornerFinder{}})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name     string
		detector Detector
		want     layout.Arrangement
	}{
		{"blur fill", nil, layout.BlurFill},
		{"stack", fixedDetector{detection: layout.FaceAt(cornerFace, layout.Stack, "")}, layout.Stack},
		{"face crop", fixedDetector{detection: layout.FaceAt(bigFace, layout.FaceCrop, "")}, layout.FaceCrop},
		{"detected", detected, layout.Stack},
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
				t.Errorf("arrangement %s (%s), want %s", res.Layout.Arrangement, res.Why(), tt.want)
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
			if tt.name == "detected" {
				checkDetected(t, res, workDir)
			}
			t.Logf("summary:\n%s", res.Summary())
		})
	}
}

// checkDetected checks what the real detector left: eight frames, the
// corner face chosen, detect.jpg at the source's size and detection.json
// with every frame's faces.
func checkDetected(t *testing.T, res Result, workDir string) {
	t.Helper()
	if res.Detector != "corner" || !res.Detection.Face || res.Detection.Arrangement != layout.Stack {
		t.Errorf("detection %q %+v", res.Detector, res.Detection)
	}
	if b := res.Detection.Box; b.X < 0.84 || b.X > 0.86 || b.H < 0.07 || b.H > 0.09 {
		t.Errorf("box %+v, want the corner face", b)
	}
	if res.DebugImage != filepath.Join(workDir, DebugImageFile) {
		t.Fatalf("debug image %q", res.DebugImage)
	}
	got := streamCodecs(t, res.DebugImage)
	if got["video"] != "mjpeg" || got["size"] != "1920x1080" {
		t.Errorf("detect.jpg: %v", got)
	}
	var doc struct {
		Detector string         `json:"detector"`
		Frames   []detect.Frame `json:"frames"`
		Groups   []detect.Group `json:"groups"`
		Chosen   int            `json:"chosen_group"`
		Decision struct {
			Face        bool   `json:"face"`
			Arrangement string `json:"arrangement"`
			Detail      string `json:"detail"`
		} `json:"decision"`
		Layout struct {
			FaceFrame *layout.Crop `json:"face_frame"`
		} `json:"layout"`
	}
	data, err := os.ReadFile(filepath.Join(workDir, DetectionFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Detector != "corner" || len(doc.Frames) != 8 || len(doc.Frames[0].Faces) != 3 || !doc.Decision.Face ||
		doc.Decision.Arrangement != "stack" || doc.Layout.FaceFrame == nil || doc.Chosen < 0 || !doc.Groups[doc.Chosen].Steady {
		t.Errorf("detection.json:\n%s", data)
	}
	for _, f := range doc.Frames {
		if _, err := os.Stat(f.Path); err != nil {
			t.Errorf("frame: %v", err)
		}
	}
}
