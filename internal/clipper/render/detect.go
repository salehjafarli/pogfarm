package render

import (
	"context"
	"log/slog"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// Detector finds the streamer's face in a clip (package
// internal/clipper/detect). The render core always asks one and always gets
// an answer: a face, or none with a reason. An error is logged and treated
// as none (layout.ReasonError); it never fails the render, unless the
// context was cancelled. *detect.Detector implements it.
type Detector interface {
	// Name identifies the detector in logs and in Result.Detector.
	Name() string
	// Detect looks at video, the downloaded range, which is length long,
	// and may keep its own files (the sampled frames) in workDir. The
	// report's Detection is the answer; its frames, faces and groups go
	// to detection.json and detect.jpg.
	Detect(ctx context.Context, video string, length time.Duration, workDir string) (detect.Report, error)
}

// OffDetector answers "no face, detector off" for every clip without
// looking at it, so every clip uses the blur fill layout.
type OffDetector struct{}

var _ Detector = OffDetector{}
var _ Detector = (*detect.Detector)(nil)

// Name implements Detector.
func (OffDetector) Name() string { return "off" }

// Detect implements Detector: always layout.ReasonOff.
func (OffDetector) Detect(context.Context, string, time.Duration, string) (detect.Report, error) {
	return detect.Report{Detection: layout.NoFace(layout.ReasonOff, ""), Chosen: -1}, nil
}

// NewDetector picks the detector from clipper.webcam_detection, once, and
// logs the choice: the real one (detect.New) when enabled, OffDetector
// otherwise. It starts nothing; call detect.Check first to make sure the
// real one can run.
func NewDetector(cfg config.ClipperWebcamDetection, logger *slog.Logger) (Detector, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if !cfg.Enabled {
		logger.Info("face detection is off (clipper.webcam_detection.enabled: false); every clip uses blur fill")
		return OffDetector{}, nil
	}
	d, err := detect.New(cfg, detect.Options{})
	if err != nil {
		return nil, err
	}
	logger.Info("face detection is on", "detector", d.Name(), "program", cfg.DetectFacesPath, "model", cfg.Model, "frames", cfg.Frames)
	return d, nil
}
