package config

import "testing"

func TestClipperWebcamDetectionDefault(t *testing.T) {
	clipperEnv(t)
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebcamDetection.Enabled {
		t.Error("webcam detection should be enabled by default")
	}
}

func TestClipperWebcamDetectionOverrides(t *testing.T) {
	clipperEnv(t)
	path := writeFile(t, "config.yaml", "clipper:\n  webcam_detection:\n    enabled: false\n")
	cfg, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebcamDetection.Enabled {
		t.Error("file: enabled: false was not applied")
	}

	t.Setenv("CLIPPER_WEBCAM_DETECTION_ENABLED", "true")
	cfg, err = LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebcamDetection.Enabled {
		t.Error("the environment should override the file")
	}

	t.Setenv("CLIPPER_WEBCAM_DETECTION_ENABLED", "maybe")
	_, err = LoadClipper(path)
	got := problemsOf(t, err)
	if len(got) != 1 || got[0] != `CLIPPER_WEBCAM_DETECTION_ENABLED: not true or false: "maybe"` {
		t.Errorf("problems: %q", got)
	}
}
