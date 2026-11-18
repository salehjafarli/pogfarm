package config

import "fmt"

// Bounds of clipper.layout.webcam_share.
const (
	// MinWebcamShare is the smallest share of the output height the webcam
	// band of the stack arrangement may take.
	MinWebcamShare = 0.1
	// MaxWebcamShare is the largest share of the output height the webcam
	// band of the stack arrangement may take.
	MaxWebcamShare = 0.9
)

// ClipperLayout is the "clipper.layout" section: how the 16:9 source is
// arranged in the 9:16 clip. Package internal/clipper/layout documents the
// two arrangements. The webcam band's height is the only setting; everything
// else about the layout is fixed.
type ClipperLayout struct {
	// WebcamShare is the share of the output height given to the webcam
	// band when the stack arrangement is used; the gameplay takes the rest.
	// File only: the environment cannot set fractional numbers.
	WebcamShare float64 `yaml:"webcam_share"`
}

// defaultClipperLayout is the layout section when nothing is configured:
// the webcam takes one third of the height.
func defaultClipperLayout() ClipperLayout {
	return ClipperLayout{WebcamShare: 1.0 / 3}
}

func (l ClipperLayout) validate() []string {
	if !finite(l.WebcamShare) || l.WebcamShare < MinWebcamShare || l.WebcamShare > MaxWebcamShare {
		return []string{fmt.Sprintf("clipper.layout.webcam_share must be a number from %v to %v: got %v",
			MinWebcamShare, MaxWebcamShare, l.WebcamShare)}
	}
	return nil
}

// Validate reports every problem of the section, in the words of the config
// file. Package layout uses it to check a section built in code.
func (l ClipperLayout) Validate() []string { return l.validate() }
