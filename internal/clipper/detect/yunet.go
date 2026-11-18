package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Face is one face the detector found, in pixels of its image: (X, Y) is
// the top-left corner.
type Face struct {
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	W          float64 `json:"w"`
	H          float64 `json:"h"`
	Confidence float64 `json:"confidence"`
	// Kept and Group are filled in by Decide: whether the face passed the
	// confidence and size rules, and the index of its group in
	// Decision.Groups (-1 when it was not kept). A FaceFinder leaves them
	// zero.
	Kept  bool `json:"kept"`
	Group int  `json:"group"`
}

// Image is the detector's answer for one image.
type Image struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Faces  []Face `json:"faces"`
	// Error is set when the image could not be read; it then has no faces.
	Error string `json:"error,omitempty"`
}

// FaceFinder finds faces in still images: frames in, face boxes with
// confidence out. It answers one Image per path, in the same order.
// YuNet is the real one; tests use fakes.
type FaceFinder interface {
	// Name identifies the finder in logs and in the render's result.
	Name() string
	FindFaces(ctx context.Context, images []string) ([]Image, error)
}

// reportFloor is the lowest confidence detect-faces is asked to report.
// Faces between it and the rules' MinConfidence are not used, but they
// appear in detection.json, which helps tuning.
const reportFloor = 0.5

// maxSide is the size detect-faces scales larger frames down to before
// detection (its own default); boxes come back in full-size pixels.
const maxSide = 1280

// YuNet runs the detect-faces program of the clipper image, which runs
// OpenCV's YuNet face detector. See deploy/clipper-base/detect-faces.
type YuNet struct {
	program, model string
	floor          float64
}

var _ FaceFinder = (*YuNet)(nil)

// NewYuNet returns the finder for the config section. It starts nothing.
func NewYuNet(cfg config.ClipperWebcamDetection) *YuNet {
	return &YuNet{program: cfg.DetectFacesPath, model: cfg.Model, floor: min(reportFloor, cfg.MinConfidence)}
}

// Name implements FaceFinder.
func (*YuNet) Name() string { return "yunet" }

// args is the detect-faces command line for the images.
func (y *YuNet) args(images []string) []string {
	args := []string{
		"--model", y.model,
		"--min-confidence", strconv.FormatFloat(y.floor, 'f', -1, 64),
		"--max-side", strconv.Itoa(maxSide),
		"--",
	}
	return append(args, images...)
}

// FindFaces implements FaceFinder. It runs detect-faces once on all the
// images; the context bounds it. detect-faces exits 1 when some image could
// not be read; its answer is still used, and those images carry an Error.
// Any other failure is an error.
func (y *YuNet) FindFaces(ctx context.Context, images []string) ([]Image, error) {
	res, err := runner.Run(ctx, runner.Command{Name: y.program, Args: y.args(images)})
	if err != nil {
		var re *runner.Error
		if !errors.As(err, &re) || re.ExitCode != 1 {
			return nil, fmt.Errorf("detect-faces: %w", err)
		}
	}
	return parseOutput(res.Stdout, images)
}

// output is detect-faces' JSON document.
type output struct {
	Detector string `json:"detector"`
	Images   []struct {
		Path   string  `json:"path"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
		Faces  []Face  `json:"faces"`
		Error  *string `json:"error"`
	} `json:"images"`
}

// parseOutput reads detect-faces' answer for the images.
func parseOutput(data []byte, images []string) ([]Image, error) {
	var out output
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("detect-faces: unreadable output: %w", err)
	}
	if len(out.Images) != len(images) {
		return nil, fmt.Errorf("detect-faces: answered for %d images, asked for %d", len(out.Images), len(images))
	}
	result := make([]Image, len(images))
	for i, im := range out.Images {
		result[i] = Image{Path: images[i], Width: im.Width, Height: im.Height, Faces: im.Faces}
		if im.Error != nil {
			result[i].Error = *im.Error
			result[i].Faces = nil
		}
		if result[i].Faces == nil {
			result[i].Faces = []Face{}
		}
	}
	return result, nil
}

// Check makes sure face detection can run, so that a node without the
// detector refuses to start instead of laying every clip out without a
// face. clipper render calls it before anything else, and clipper work will
// before it claims a job.
//
// It does nothing when detection is disabled. Otherwise it checks that the
// model file exists and that detect-faces runs on a small generated image
// and answers cleanly, within the configured timeout. That catches a
// missing program, Python, OpenCV or model. The error names what is missing
// and says how to fix it.
func Check(ctx context.Context, cfg config.ClipperWebcamDetection) error {
	if !cfg.Enabled {
		return nil
	}
	fix := "install it (the clipper image has it; on a Mac run scripts/setup-clipper-macos.sh) " +
		"or switch face detection off with clipper.webcam_detection.enabled: false (CLIPPER_WEBCAM_DETECTION_ENABLED=false)"
	if _, err := os.Stat(cfg.Model); err != nil {
		return fmt.Errorf("face detection is enabled, but its model %s cannot be read (%w); %s", cfg.Model, err, fix)
	}
	if _, err := exec.LookPath(cfg.DetectFacesPath); err != nil {
		return fmt.Errorf("face detection is enabled, but the detect-faces program %q is not found (%w); %s", cfg.DetectFacesPath, err, fix)
	}
	dir, err := os.MkdirTemp("", "clipper-detect-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	img := filepath.Join(dir, "blank.png")
	if err := writeBlankPNG(img); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	got, err := NewYuNet(cfg).FindFaces(ctx, []string{img})
	if err == nil && got[0].Error != "" {
		err = errors.New(got[0].Error)
	}
	if err != nil {
		return fmt.Errorf("face detection is enabled, but %s with the model %s does not work on a test image: %w; %s",
			cfg.DetectFacesPath, cfg.Model, err, fix)
	}
	return nil
}

// writeBlankPNG writes a small grey picture with no face.
func writeBlankPNG(path string) error {
	img := image.NewGray(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
