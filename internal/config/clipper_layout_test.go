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
	if cfg.Layout != defaultClipperLayout() || cfg.Layout.WebcamShare != 1.0/3 {
		t.Errorf("layout default: got %+v, want webcam_share 1/3", cfg.Layout)
	}
}

func TestClipperLayoutFromFile(t *testing.T) {
	cleanEnv(t)
	cfg, err := LoadClipper(clipperFile(t, "  layout:\n    webcam_share: 0.4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Layout.WebcamShare != 0.4 {
		t.Errorf("webcam_share: got %v, want 0.4", cfg.Layout.WebcamShare)
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
	for _, value := range []string{"0", "0.05", "0.95", "1", "-0.3", ".nan", ".inf"} {
		t.Run(value, func(t *testing.T) {
			cleanEnv(t)
			_, err := LoadClipper(clipperFile(t, "  layout:\n    webcam_share: "+value+"\n"))
			got := problemsOf(t, err)
			if len(got) != 1 || !strings.Contains(got[0], "clipper.layout.webcam_share must be a number from 0.1 to 0.9") {
				t.Errorf("problems: %q", got)
			}
		})
	}
}

func TestClipperLayoutBounds(t *testing.T) {
	for _, share := range []float64{MinWebcamShare, 0.5, MaxWebcamShare} {
		if p := (ClipperLayout{WebcamShare: share}).Validate(); p != nil {
			t.Errorf("share %v should be accepted: %q", share, p)
		}
	}
}
