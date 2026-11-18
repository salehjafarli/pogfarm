package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClipperWebcamDetectionDefault(t *testing.T) {
	clipperEnv(t)
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperWebcamDetection{
		Enabled:         true,
		DetectFacesPath: "detect-faces",
		Model:           "/models/yunet/face_detection_yunet_2023mar.onnx",
		FFmpegPath:      "ffmpeg",
		Timeout:         time.Minute,
		Frames:          8,
		MinConfidence:   0.7,
		MinFaceHeight:   0.03,
		SteadyShare:     0.6,
		MaxMovement:     0.5,
		Dominance:       1.5,
		FaceCropHeight:  0.2,
	}
	if cfg.WebcamDetection != want {
		t.Errorf("default:\n got %+v\nwant %+v", cfg.WebcamDetection, want)
	}
}

func TestClipperWebcamDetectionExampleIsDefault(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CLIPPER_NAME", "example-node")
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	cfg, err := LoadClipper(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebcamDetection != defaultClipperWebcamDetection() {
		t.Errorf("the example file should show the default: got %+v", cfg.WebcamDetection)
	}
}

func TestClipperWebcamDetectionOverrides(t *testing.T) {
	clipperEnv(t)
	path := writeFile(t, "config.yaml", `clipper:
  webcam_detection:
    enabled: false
    detect_faces_path: /opt/bin/detect-faces
    model: /m/yunet.onnx
    ffmpeg_path: /opt/bin/ffmpeg
    timeout: 30s
    frames: 12
    min_confidence: 0.8
    min_face_height: 0.05
    steady_share: 0.75
    max_movement: 1
    dominance: 2
    face_crop_height: 0.25
`)
	cfg, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperWebcamDetection{
		DetectFacesPath: "/opt/bin/detect-faces", Model: "/m/yunet.onnx", FFmpegPath: "/opt/bin/ffmpeg",
		Timeout: 30 * time.Second, Frames: 12, MinConfidence: 0.8, MinFaceHeight: 0.05,
		SteadyShare: 0.75, MaxMovement: 1, Dominance: 2, FaceCropHeight: 0.25,
	}
	if cfg.WebcamDetection != want {
		t.Errorf("file:\n got %+v\nwant %+v", cfg.WebcamDetection, want)
	}

	t.Setenv("CLIPPER_WEBCAM_DETECTION_ENABLED", "true")
	t.Setenv("CLIPPER_WEBCAM_DETECTION_DETECT_FACES_PATH", "/env/detect-faces")
	t.Setenv("CLIPPER_WEBCAM_DETECTION_MODEL", "/env/yunet.onnx")
	t.Setenv("CLIPPER_WEBCAM_DETECTION_FFMPEG_PATH", "/env/ffmpeg")
	t.Setenv("CLIPPER_WEBCAM_DETECTION_TIMEOUT", "2m")
	t.Setenv("CLIPPER_WEBCAM_DETECTION_FRAMES", "4")
	cfg, err = LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.WebcamDetection
	if !d.Enabled || d.DetectFacesPath != "/env/detect-faces" || d.Model != "/env/yunet.onnx" ||
		d.FFmpegPath != "/env/ffmpeg" || d.Timeout != 2*time.Minute || d.Frames != 4 {
		t.Errorf("the environment should override the file: %+v", d)
	}

	t.Setenv("CLIPPER_WEBCAM_DETECTION_ENABLED", "maybe")
	_, err = LoadClipper(path)
	got := problemsOf(t, err)
	if len(got) != 1 || got[0] != `CLIPPER_WEBCAM_DETECTION_ENABLED: not true or false: "maybe"` {
		t.Errorf("problems: %q", got)
	}
}

func TestClipperWebcamDetectionInvalid(t *testing.T) {
	d := ClipperWebcamDetection{
		DetectFacesPath: " ", Model: "", FFmpegPath: "", Timeout: 0, Frames: 1,
		MinConfidence: 0, MinFaceHeight: 0.6, SteadyShare: 1.5, MaxMovement: -1, Dominance: 0.9, FaceCropHeight: 1.2,
	}
	got := d.Validate()
	for _, want := range []string{
		"clipper.webcam_detection.detect_faces_path (CLIPPER_WEBCAM_DETECTION_DETECT_FACES_PATH) must not be empty",
		"clipper.webcam_detection.model (CLIPPER_WEBCAM_DETECTION_MODEL) must not be empty",
		"clipper.webcam_detection.ffmpeg_path (CLIPPER_WEBCAM_DETECTION_FFMPEG_PATH) must not be empty",
		"clipper.webcam_detection.timeout (CLIPPER_WEBCAM_DETECTION_TIMEOUT) must be a positive duration",
		"clipper.webcam_detection.frames (CLIPPER_WEBCAM_DETECTION_FRAMES) must be from 2 to 32: got 1",
		"clipper.webcam_detection.min_confidence must be a number above 0 and at most 1: got 0",
		"clipper.webcam_detection.min_face_height must be a number above 0 and at most 0.5: got 0.6",
		"clipper.webcam_detection.steady_share must be a number above 0 and at most 1: got 1.5",
		"clipper.webcam_detection.max_movement must be a number above 0 and at most 5: got -1",
		"clipper.webcam_detection.dominance must be a number from 1 to 10: got 0.9",
		"clipper.webcam_detection.face_crop_height must be a number above 0 and at most 1: got 1.2",
	} {
		found := false
		for _, p := range got {
			found = found || strings.HasPrefix(p, want)
		}
		if !found {
			t.Errorf("missing problem %q in:\n%s", want, strings.Join(got, "\n"))
		}
	}
	if len(got) != 11 {
		t.Errorf("want 11 problems, got %d", len(got))
	}
	if p := defaultClipperWebcamDetection().Validate(); p != nil {
		t.Errorf("default is invalid: %q", p)
	}
}
