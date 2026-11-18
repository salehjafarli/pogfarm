package render

import (
	"context"
	"log/slog"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// Detector finds the streamer's webcam in a clip (issue #59). The render
// core always asks one and always gets an answer: a webcam region, or no
// webcam. An error is logged and treated as no webcam; it never fails the
// render, unless the context was cancelled.
type Detector interface {
	// Name identifies the detector in logs and in Result.Detector.
	Name() string
	// Detect looks at video, the downloaded range, and may keep its own
	// files (extracted frames, for example) in workDir.
	Detect(ctx context.Context, video, workDir string) (layout.Detection, error)
}

// OffDetector answers "no webcam" for every clip without looking at it, so
// every clip uses the blur fill layout.
type OffDetector struct{}

var _ Detector = OffDetector{}

// Name implements Detector.
func (OffDetector) Name() string { return "off" }

// Detect implements Detector: always layout.NoWebcam().
func (OffDetector) Detect(context.Context, string, string) (layout.Detection, error) {
	return layout.NoWebcam(), nil
}

// NewDetector picks the detector from clipper.webcam_detection, once, at
// startup, and logs the choice.
//
// No real detector exists yet (#59), so both values select OffDetector.
// When the real one lands, enabled: true selects it and checks that its tool
// and model are installed, and enabled: false keeps OffDetector.
func NewDetector(cfg config.ClipperWebcamDetection, logger *slog.Logger) Detector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Enabled {
		logger.Info("webcam detection is enabled but no detector is built yet (#59); using the off detector, every clip uses blur fill")
	} else {
		logger.Info("webcam detection is off (clipper.webcam_detection.enabled: false); every clip uses blur fill")
	}
	return OffDetector{}
}
