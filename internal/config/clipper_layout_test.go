package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// clipperFile writes a clipper config file with the required keys followed by
// extra, which is indented as part of the clipper section.
func clipperFile(t *testing.T, extra string) string {
	t.Helper()
	return writeFile(t, "config.yaml", `
clipper:
  pogfarm_url: http://pogfarm:8080
  api_secret: `+goodSecret+`
  name: node-a
`+extra)
}

func TestClipperLayoutDefault(t *testing.T) {
	cleanEnv(t)
	cfg, err := LoadClipper(clipperFile(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperLayout{StackFaceShare: 0.45, CropFaceShare: 0.25, EyeLine: 1.0 / 3}
	if cfg.Layout != want || defaultClipperLayout() != want {
		t.Errorf("layout default: got %+v, want %+v", cfg.Layout, want)
	}
}

func TestClipperLayoutFromFile(t *testing.T) {
	cleanEnv(t)
	cfg, err := LoadClipper(clipperFile(t, "  layout:\n    stack_face_share: 0.5\n    crop_face_share: 0.3\n    eye_line: 0.4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (ClipperLayout{StackFaceShare: 0.5, CropFaceShare: 0.3, EyeLine: 0.4}); cfg.Layout != want {
		t.Errorf("layout: got %+v, want %+v", cfg.Layout, want)
	}
}

func TestClipperLayoutExampleIsDefault(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CLIPPER_NAME", "example-node")
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	cfg, err := LoadClipper(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Layout != defaultClipperLayout() {
		t.Errorf("the example file should show the default: got %+v, want %+v", cfg.Layout, defaultClipperLayout())
	}
}

func TestClipperLayoutInvalid(t *testing.T) {
	keys := []struct{ key, msg string }{
		{"stack_face_share", "clipper.layout.stack_face_share must be a number from 0.05 to 0.9"},
		{"crop_face_share", "clipper.layout.crop_face_share must be a number from 0.05 to 0.9"},
		{"eye_line", "clipper.layout.eye_line must be a number from 0.1 to 0.9"},
	}
	for _, k := range keys {
		for _, value := range []string{"0", "0.01", "0.95", "1", "-0.3", ".nan", ".inf"} {
			t.Run(k.key+"="+value, func(t *testing.T) {
				cleanEnv(t)
				_, err := LoadClipper(clipperFile(t, "  layout:\n    "+k.key+": "+value+"\n"))
				got := problemsOf(t, err)
				if len(got) != 1 || !strings.Contains(got[0], k.msg) {
					t.Errorf("problems: %q", got)
				}
			})
		}
	}
}

func TestClipperLayoutReportsEveryProblem(t *testing.T) {
	if got := (ClipperLayout{}).Validate(); len(got) != 3 {
		t.Errorf("want three problems, got %q", got)
	}
}

func TestClipperLayoutBounds(t *testing.T) {
	for _, l := range []ClipperLayout{
		{StackFaceShare: MinFaceShare, CropFaceShare: MinFaceShare, EyeLine: MinEyeLine},
		{StackFaceShare: MaxFaceShare, CropFaceShare: MaxFaceShare, EyeLine: MaxEyeLine},
	} {
		if p := l.Validate(); p != nil {
			t.Errorf("%+v should be accepted: %q", l, p)
		}
	}
}

// The old webcam_share key was replaced by the framing settings; a file
// that still has it is refused like any unknown key.
func TestClipperLayoutOldKeyRefused(t *testing.T) {
	cleanEnv(t)
	if _, err := LoadClipper(clipperFile(t, "  layout:\n    webcam_share: 0.4\n")); err == nil ||
		!strings.Contains(err.Error(), "webcam_share") {
		t.Errorf("got %v, want an error naming webcam_share", err)
	}
}
