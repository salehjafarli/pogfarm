package config

import (
	"fmt"
	"strings"
	"time"
)

// Bounds of clipper.webcam_detection.frames.
const (
	MinDetectionFrames = 2
	MaxDetectionFrames = 32
)

// ClipperWebcamDetection is the "clipper.webcam_detection" section: how the
// clipper finds the streamer's face in each clip (package
// internal/clipper/detect), which decides the layout.
//
// Detection samples Frames frames across the clip with ffmpeg, runs the
// detect-faces program (YuNet through OpenCV) on them, and keeps a face that
// stays in roughly the same place in most frames. The numbers below are the
// rules of that decision; detect's package documentation explains each.
type ClipperWebcamDetection struct {
	// Enabled switches face detection on. false selects the off detector:
	// every clip uses the blur fill layout and the detector program and
	// its model are never looked for. When true, the clipper refuses to
	// start unless the program runs with its model.
	Enabled bool `yaml:"enabled" env:"CLIPPER_WEBCAM_DETECTION_ENABLED"`
	// DetectFacesPath is the detect-faces program: a path, or a name
	// looked up in PATH.
	DetectFacesPath string `yaml:"detect_faces_path" env:"CLIPPER_WEBCAM_DETECTION_DETECT_FACES_PATH"`
	// Model is the YuNet ONNX model file detect-faces loads.
	Model string `yaml:"model" env:"CLIPPER_WEBCAM_DETECTION_MODEL"`
	// FFmpegPath is the ffmpeg program that extracts the frames.
	FFmpegPath string `yaml:"ffmpeg_path" env:"CLIPPER_WEBCAM_DETECTION_FFMPEG_PATH"`
	// Timeout bounds the whole detection of one clip: frame extraction and
	// the detector. A clip whose detection times out uses blur fill.
	Timeout time.Duration `yaml:"timeout" env:"CLIPPER_WEBCAM_DETECTION_TIMEOUT"`
	// Frames is the number of frames sampled, spread evenly across the
	// clip.
	Frames int `yaml:"frames" env:"CLIPPER_WEBCAM_DETECTION_FRAMES"`

	// The rules below are file only: the environment cannot set fractional
	// numbers.

	// MinConfidence is the lowest detector confidence of a face that
	// counts, from 0 to 1.
	MinConfidence float64 `yaml:"min_confidence"`
	// MinFaceHeight is the smallest face that counts, as a share of the
	// frame height.
	MinFaceHeight float64 `yaml:"min_face_height"`
	// SteadyShare is the share of the sampled frames a face must be found
	// in, at roughly the same place, to count as steady.
	SteadyShare float64 `yaml:"steady_share"`
	// MaxMovement is how far a face's centre may be from where it is in
	// the other frames, in face heights, to count as the same face.
	MaxMovement float64 `yaml:"max_movement"`
	// Dominance is how many times taller than the next steady face the
	// largest one must be to be taken as the streamer's. Closer sizes make
	// the clip ambiguous, and it uses blur fill.
	Dominance float64 `yaml:"dominance"`
	// FaceCropHeight is the face height, as a share of the frame height,
	// above which the camera fills the picture and the clip uses the face
	// crop arrangement instead of stack.
	FaceCropHeight float64 `yaml:"face_crop_height"`
}

// defaultClipperWebcamDetection is the section when nothing is configured:
// detection on, with the program and model where the clipper image has
// them.
func defaultClipperWebcamDetection() ClipperWebcamDetection {
	return ClipperWebcamDetection{
		Enabled:         true,
		DetectFacesPath: "detect-faces",
		Model:           "/models/yunet/face_detection_yunet_2023mar.onnx",
		FFmpegPath:      "ffmpeg",
		Timeout:         time.Minute,
		Frames:          8,
		MinConfidence:   0.7,
		MinFaceHeight:   0.03,
		SteadyShare:     0.6,
		MaxMovement:     0.5,
		Dominance:       1.5,
		FaceCropHeight:  0.2,
	}
}

func (d ClipperWebcamDetection) validate() []string {
	var problems []string
	notEmpty := func(key, env, v string) {
		if strings.TrimSpace(v) == "" {
			problems = append(problems, fmt.Sprintf("clipper.webcam_detection.%s (%s) must not be empty", key, env))
		}
	}
	notEmpty("detect_faces_path", "CLIPPER_WEBCAM_DETECTION_DETECT_FACES_PATH", d.DetectFacesPath)
	notEmpty("model", "CLIPPER_WEBCAM_DETECTION_MODEL", d.Model)
	notEmpty("ffmpeg_path", "CLIPPER_WEBCAM_DETECTION_FFMPEG_PATH", d.FFmpegPath)
	if d.Timeout <= 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.webcam_detection.timeout (CLIPPER_WEBCAM_DETECTION_TIMEOUT) must be a positive duration such as \"1m\": got %s", d.Timeout))
	}
	if d.Frames < MinDetectionFrames || d.Frames > MaxDetectionFrames {
		problems = append(problems, fmt.Sprintf(
			"clipper.webcam_detection.frames (CLIPPER_WEBCAM_DETECTION_FRAMES) must be from %d to %d: got %d",
			MinDetectionFrames, MaxDetectionFrames, d.Frames))
	}
	// lo is exclusive when open is true.
	between := func(key string, v, lo, hi float64, open bool) {
		if !finite(v) || v > hi || v < lo || (open && v == lo) {
			want := fmt.Sprintf("from %v to %v", lo, hi)
			if open {
				want = fmt.Sprintf("above %v and at most %v", lo, hi)
			}
			problems = append(problems, fmt.Sprintf("clipper.webcam_detection.%s must be a number %s: got %v", key, want, v))
		}
	}
	between("min_confidence", d.MinConfidence, 0, 1, true)
	between("min_face_height", d.MinFaceHeight, 0, 0.5, true)
	between("steady_share", d.SteadyShare, 0, 1, true)
	between("max_movement", d.MaxMovement, 0, 5, true)
	between("dominance", d.Dominance, 1, 10, false)
	between("face_crop_height", d.FaceCropHeight, 0, 1, true)
	return problems
}

// Validate reports every problem of the section, in the words of the config
// file. Package detect uses it to check a section built in code.
func (d ClipperWebcamDetection) Validate() []string { return d.validate() }
