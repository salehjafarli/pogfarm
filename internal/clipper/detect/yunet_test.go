//go:build linux || darwin

package detect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// fakeDetectFaces writes a shell script standing in for detect-faces: it
// records its arguments, runs block (shell code, may be empty), prints
// stdout and exits with code. It returns the script and the arguments file.
func fakeDetectFaces(t *testing.T, stdout, block string, code int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	script, args, out := filepath.Join(dir, "detect-faces"), filepath.Join(dir, "args"), filepath.Join(dir, "out.json")
	if err := os.WriteFile(out, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\n" + block + "\ncat '" + out + "'\necho 'detect-faces: something on stderr' >&2\nexit " +
		string(rune('0'+code)) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, args
}

func detectionConfig(t *testing.T) config.ClipperWebcamDetection {
	t.Helper()
	t.Setenv(config.EnvConfigPath, "")
	cfg, err := config.LoadClipperRender("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg.WebcamDetection
}

const twoImages = `{"detector": "yunet", "model": "m.onnx", "images": [
  {"path": "a.jpg", "width": 1280, "height": 720,
   "faces": [{"x": 151.2, "y": 499.8, "w": 36.5, "h": 46.1, "confidence": 0.93}], "error": null},
  {"path": "b.jpg", "width": 0, "height": 0, "faces": [], "error": "cannot read image"}
]}`

func TestYuNetFindFaces(t *testing.T) {
	cfg := detectionConfig(t)
	for _, code := range []int{0, 1} {
		script, argsFile := fakeDetectFaces(t, twoImages, "", code)
		cfg.DetectFacesPath, cfg.Model = script, "/models/yunet.onnx"
		got, err := NewYuNet(cfg).FindFaces(context.Background(), []string{"/w/a.jpg", "/w/b.jpg"})
		if err != nil {
			t.Fatalf("exit %d: %v", code, err)
		}
		want := []Image{
			{Path: "/w/a.jpg", Width: 1280, Height: 720, Faces: []Face{{X: 151.2, Y: 499.8, W: 36.5, H: 46.1, Confidence: 0.93}}},
			{Path: "/w/b.jpg", Faces: []Face{}, Error: "cannot read image"},
		}
		if len(got) != 2 || got[0].Path != want[0].Path || got[0].Width != 1280 || len(got[0].Faces) != 1 ||
			got[0].Faces[0] != want[0].Faces[0] || got[1].Error != want[1].Error || len(got[1].Faces) != 0 {
			t.Errorf("exit %d: got %+v", code, got)
		}
		args, _ := os.ReadFile(argsFile)
		if want := "--model\n/models/yunet.onnx\n--min-confidence\n0.5\n--max-side\n1280\n--\n/w/a.jpg\n/w/b.jpg\n"; string(args) != want {
			t.Errorf("arguments:\n%s\nwant:\n%s", args, want)
		}
	}
}

func TestYuNetFloorFollowsMinConfidence(t *testing.T) {
	cfg := detectionConfig(t)
	cfg.MinConfidence = 0.3
	if got := NewYuNet(cfg).args(nil); strings.Join(got, " ") != "--model "+cfg.Model+" --min-confidence 0.3 --max-side 1280 --" {
		t.Errorf("args %q", got)
	}
}

func TestYuNetFailures(t *testing.T) {
	cfg := detectionConfig(t)
	tests := []struct {
		name, stdout, block string
		code                int
		want                string
	}{
		{"model not loaded", "", "", 2, "something on stderr"},
		{"garbage", "not json", "", 0, "unreadable output"},
		{"wrong count", `{"images": []}`, "", 0, "answered for 0 images, asked for 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.DetectFacesPath, _ = fakeDetectFaces(t, tt.stdout, tt.block, tt.code)
			_, err := NewYuNet(cfg).FindFaces(context.Background(), []string{"a.jpg"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error with %q", err, tt.want)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		cfg.DetectFacesPath, _ = fakeDetectFaces(t, twoImages, "exec sleep 600", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := NewYuNet(cfg).FindFaces(ctx, []string{"a.jpg"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want a deadline error", err)
		}
	})
	t.Run("missing program", func(t *testing.T) {
		cfg.DetectFacesPath = filepath.Join(t.TempDir(), "nope")
		if _, err := NewYuNet(cfg).FindFaces(context.Background(), []string{"a.jpg"}); err == nil {
			t.Error("want an error")
		}
	})
}

const blankAnswer = `{"detector": "yunet", "model": "m", "images": [{"path": "x.png", "width": 64, "height": 64, "faces": [], "error": null}]}`

func TestCheck(t *testing.T) {
	model := filepath.Join(t.TempDir(), "yunet.onnx")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := detectionConfig(t)
	base.Model = model

	t.Run("works", func(t *testing.T) {
		cfg := base
		var argsFile string
		cfg.DetectFacesPath, argsFile = fakeDetectFaces(t, blankAnswer, "", 0)
		if err := Check(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		args, _ := os.ReadFile(argsFile)
		if lines := strings.Split(strings.TrimSpace(string(args)), "\n"); !strings.HasSuffix(lines[len(lines)-1], "blank.png") || lines[1] != model {
			t.Errorf("arguments: %q", lines)
		}
	})
	t.Run("disabled checks nothing", func(t *testing.T) {
		cfg := base
		cfg.Enabled, cfg.DetectFacesPath, cfg.Model = false, "/nowhere/detect-faces", "/nowhere/model.onnx"
		if err := Check(context.Background(), cfg); err != nil {
			t.Errorf("got %v", err)
		}
	})
	fails := []struct {
		name  string
		setup func(cfg *config.ClipperWebcamDetection)
		want  []string
	}{
		{"missing model", func(cfg *config.ClipperWebcamDetection) {
			cfg.DetectFacesPath, _ = fakeDetectFaces(t, blankAnswer, "", 0)
			cfg.Model = filepath.Join(t.TempDir(), "missing.onnx")
		}, []string{"its model", "missing.onnx", "enabled: false", "setup-clipper-macos.sh"}},
		{"missing program", func(cfg *config.ClipperWebcamDetection) {
			cfg.DetectFacesPath = "pogfarm-no-such-detect-faces"
		}, []string{`program "pogfarm-no-such-detect-faces" is not found`, "enabled: false"}},
		{"program fails", func(cfg *config.ClipperWebcamDetection) {
			cfg.DetectFacesPath, _ = fakeDetectFaces(t, "", "", 2)
		}, []string{"does not work on a test image", "something on stderr", "enabled: false"}},
		{"image unreadable", func(cfg *config.ClipperWebcamDetection) {
			cfg.DetectFacesPath, _ = fakeDetectFaces(t, strings.Replace(blankAnswer, `"error": null`, `"error": "cannot read image"`, 1), "", 1)
		}, []string{"does not work on a test image: cannot read image"}},
		{"times out", func(cfg *config.ClipperWebcamDetection) {
			cfg.DetectFacesPath, _ = fakeDetectFaces(t, blankAnswer, "exec sleep 600", 0)
			cfg.Timeout = 50 * time.Millisecond
		}, []string{"does not work on a test image", "timed out"}},
	}
	for _, tt := range fails {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.setup(&cfg)
			err := Check(context.Background(), cfg)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
		})
	}
}

// realDetector returns the detection section pointing at the real
// detect-faces and its model, from the environment that
// scripts/setup-clipper-macos.sh writes or the image's defaults, or skips
// the test when they are not installed. CI has neither.
func realDetector(t *testing.T) config.ClipperWebcamDetection {
	t.Helper()
	program, model := os.Getenv("CLIPPER_WEBCAM_DETECTION_DETECT_FACES_PATH"), os.Getenv("CLIPPER_WEBCAM_DETECTION_MODEL")
	cfg := detectionConfig(t)
	if program != "" {
		cfg.DetectFacesPath = program
	}
	if model != "" {
		cfg.Model = model
	}
	if _, err := exec.LookPath(cfg.DetectFacesPath); err != nil {
		t.Skipf("detect-faces is not installed (%v)", err)
	}
	if _, err := os.Stat(cfg.Model); err != nil {
		t.Skipf("the YuNet model is not installed (%v)", err)
	}
	return cfg
}

// TestYuNetReal runs the real detector on the base image's test frame,
// whose webcam picture covers x 24..344, y 456..696 of 1280x720, the same
// check the image's own test makes.
func TestYuNetReal(t *testing.T) {
	cfg := realDetector(t)
	if err := Check(context.Background(), cfg); err != nil {
		t.Fatalf("startup check: %v", err)
	}
	frame, err := filepath.Abs(filepath.Join("..", "..", "..", "deploy", "clipper-base", "test", "webcam-frame.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewYuNet(cfg).FindFaces(context.Background(), []string{frame})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Width != 1280 || got[0].Height != 720 {
		t.Errorf("size %dx%d", got[0].Width, got[0].Height)
	}
	inside := 0
	for _, f := range got[0].Faces {
		cx, cy := f.X+f.W/2, f.Y+f.H/2
		if cx >= 24 && cx <= 344 && cy >= 456 && cy <= 696 && f.Confidence >= cfg.MinConfidence {
			inside++
		}
	}
	if inside != 1 {
		t.Errorf("want one face in the webcam area, got %+v", got[0].Faces)
	}
	t.Logf("faces: %+v", got[0].Faces)
}
