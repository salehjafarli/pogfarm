package config

import "fmt"

// Bounds of the clipper.layout framing settings.
const (
	// MinFaceShare and MaxFaceShare bound stack_face_share and
	// crop_face_share: the share of the framed area's height the face
	// takes.
	MinFaceShare = 0.05
	MaxFaceShare = 0.9
	// MinEyeLine and MaxEyeLine bound eye_line: where the eyes sit, as a
	// share of the framed area's height from its top.
	MinEyeLine = 0.1
	MaxEyeLine = 0.9
)

// ClipperLayout is the "clipper.layout" section: how the streamer's face is
// framed in the 9:16 clip. Package internal/clipper/layout documents the
// three arrangements; the stack's face band is always one third of the
// clip's height.
//
// The face is placed relative to its own size, so a small face is zoomed in
// and a big one zoomed out. These are the settings most likely to be tuned
// from the debug picture (detect.jpg). File only: the environment cannot
// set fractional numbers.
type ClipperLayout struct {
	// StackFaceShare is the face's height as a share of the stack's face
	// band (1080x640).
	StackFaceShare float64 `yaml:"stack_face_share"`
	// CropFaceShare is the face's height as a share of the whole clip
	// (1080x1920) in the face crop arrangement.
	CropFaceShare float64 `yaml:"crop_face_share"`
	// EyeLine is where the eyes sit, as a share of the framed area's
	// height from its top, in both arrangements.
	EyeLine float64 `yaml:"eye_line"`
}

// defaultClipperLayout is the layout section when nothing is configured.
func defaultClipperLayout() ClipperLayout {
	return ClipperLayout{StackFaceShare: 0.45, CropFaceShare: 0.25, EyeLine: 1.0 / 3}
}

func (l ClipperLayout) validate() []string {
	var problems []string
	between := func(key string, v, lo, hi float64) {
		if !finite(v) || v < lo || v > hi {
			problems = append(problems, fmt.Sprintf("clipper.layout.%s must be a number from %v to %v: got %v", key, lo, hi, v))
		}
	}
	between("stack_face_share", l.StackFaceShare, MinFaceShare, MaxFaceShare)
	between("crop_face_share", l.CropFaceShare, MinFaceShare, MaxFaceShare)
	between("eye_line", l.EyeLine, MinEyeLine, MaxEyeLine)
	return problems
}

// Validate reports every problem of the section, in the words of the config
// file. Package layout uses it to check a section built in code.
func (l ClipperLayout) Validate() []string { return l.validate() }
