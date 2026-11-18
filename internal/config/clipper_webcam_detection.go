package config

// ClipperWebcamDetection is the "clipper.webcam_detection" section: whether
// the clipper looks for the streamer's webcam in each clip (issue #59).
//
// Only the off detector exists today, which answers "no webcam" for every
// clip, so every clip uses the blur fill layout whatever Enabled says. When
// the real detector lands, Enabled selects it, and false keeps the off
// detector.
type ClipperWebcamDetection struct {
	// Enabled switches webcam detection on. false selects the off
	// detector: every clip uses the blur fill layout and no detection
	// model or tool is ever looked for.
	Enabled bool `yaml:"enabled" env:"CLIPPER_WEBCAM_DETECTION_ENABLED"`
}

// defaultClipperWebcamDetection is the section when nothing is configured:
// detection on.
func defaultClipperWebcamDetection() ClipperWebcamDetection {
	return ClipperWebcamDetection{Enabled: true}
}

// validate has nothing to check yet: both values are valid. The check that
// the detector and its model are installed when Enabled is true comes with
// the real detector (#59).
func (ClipperWebcamDetection) validate() []string { return nil }
