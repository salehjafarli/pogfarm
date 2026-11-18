package render

import (
	"context"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/clipper/transcribe"
	"github.com/salehjafarli/pogfarm/internal/config"
)

const ms = time.Millisecond

// fixedTranscript is a few words in the first 2.4 seconds of a clip.
func fixedTranscript() transcribe.Transcript {
	return transcribe.Transcript{Language: "en", Words: []transcribe.Word{
		{Text: "hello", Start: 200 * ms, End: 600 * ms},
		{Text: "chat", Start: 700 * ms, End: 1100 * ms},
		{Text: "look", Start: 1500 * ms, End: 1800 * ms},
		{Text: "at", Start: 1800 * ms, End: 1900 * ms},
		{Text: "this", Start: 1900 * ms, End: 2400 * ms},
	}}
}

// fakeTranscriber stands in for WhisperX, which CI does not have. It
// returns the same words for any input.
type fakeTranscriber struct {
	transcript transcribe.Transcript
	err        error
	inputs     []string
}

func (f *fakeTranscriber) Transcribe(ctx context.Context, input, _ string) (transcribe.Transcript, error) {
	f.inputs = append(f.inputs, input)
	if err := ctx.Err(); err != nil {
		return transcribe.Transcript{}, err
	}
	return f.transcript, f.err
}

// fixedDetector answers the same detection, or error, for every clip, with
// no frames behind it.
type fixedDetector struct {
	detection layout.Detection
	err       error
}

func (fixedDetector) Name() string { return "fixed" }

func (d fixedDetector) Detect(context.Context, string, time.Duration, string) (detect.Report, error) {
	return detect.Report{Detection: d.detection, Chosen: -1}, d.err
}

// testConfig is the default clipper configuration as LoadClipperRender
// gives it without a file, with the fastest encoder preset and face
// detection off, so a Renderer without a Detector option uses OffDetector:
// detect-faces is not installed where the tests run.
func testConfig(t *testing.T) config.Clipper {
	t.Helper()
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv("CLIPPER_WEBCAM_DETECTION_ENABLED", "")
	cfg, err := config.LoadClipperRender("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Render.Preset = "ultrafast"
	cfg.WebcamDetection.Enabled = false
	return *cfg
}
