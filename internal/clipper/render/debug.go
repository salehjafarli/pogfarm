package render

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Colours of detect.jpg, as ffmpeg's drawbox names them.
const (
	colourFaceFrame = "cyan"    // the crop framed on the face
	colourGameplay  = "magenta" // the stack's gameplay crop
	colourKept      = "yellow"  // a face that passed the rules
	colourDropped   = "red"     // a face below the confidence or size rules
	colourChosen    = "lime"    // the face chosen: its median box
)

// mark is one rectangle drawn on detect.jpg, in pixels of the frame.
type mark struct {
	x, y, w, h int
	colour     string
	thick      int
}

// debugMarks are the rectangles drawn on frame: the crops first, then every
// face found in the frame, then the face chosen (its median box across the
// frames) on top. Faces have no confidence label: drawtext needs fontconfig,
// and the numbers are in detection.json.
func debugMarks(frame detect.Frame, d layout.Detection, l layout.Result) []mark {
	thick := max(2, frame.Height/270)
	var marks []mark
	crop := func(c layout.Crop, colour string) {
		if !c.IsZero() {
			marks = append(marks, mark{c.X, c.Y, c.W, c.H, colour, thick})
		}
	}
	crop(l.FaceFrame, colourFaceFrame)
	crop(l.GameplayCrop, colourGameplay)
	px := func(v float64) int { return int(math.Round(v)) }
	for _, f := range frame.Faces {
		colour := colourDropped
		if f.Kept {
			colour = colourKept
		}
		marks = append(marks, mark{px(f.X), px(f.Y), px(f.W), px(f.H), colour, max(1, thick/2)})
	}
	if d.Face {
		fw, fh := float64(frame.Width), float64(frame.Height)
		marks = append(marks, mark{px(d.Box.X * fw), px(d.Box.Y * fh), px(d.Box.W * fw), px(d.Box.H * fh), colourChosen, thick})
	}
	return marks
}

// debugFilter is the -vf chain that draws the marks.
func debugFilter(marks []mark) string {
	if len(marks) == 0 {
		return "null"
	}
	parts := make([]string, len(marks))
	for i, m := range marks {
		parts[i] = fmt.Sprintf("drawbox=x=%d:y=%d:w=%d:h=%d:color=%s:t=%d", m.x, m.y, m.w, m.h, m.colour, m.thick)
	}
	return strings.Join(parts, ",")
}

// drawDebugImage writes detect.jpg into workDir: the report's DebugFrame
// with the marks drawn by ffmpeg. It returns "" without an error when there
// is no frame to draw on (the off detector, or a failed detection).
func (r *Renderer) drawDebugImage(ctx context.Context, res Result, workDir string) (string, error) {
	out := filepath.Join(workDir, DebugImageFile)
	if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	i := res.DetectionReport.DebugFrame()
	if i < 0 {
		return "", nil
	}
	frame := res.DetectionReport.Frames[i]
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", frame.Path,
		"-vf", debugFilter(debugMarks(frame, res.Detection, res.Layout)),
		"-frames:v", "1", "-q:v", "3", "-update", "1",
		out,
	}
	if _, err := runner.Run(ctx, runner.Command{Name: r.tools.ffmpeg, Args: args, Timeout: r.tools.base}); err != nil {
		return "", err
	}
	return out, nil
}

// detectionFile is the form of detection.json.
type detectionFile struct {
	Detector string `json:"detector"`
	// Error is the detector's error, when it failed.
	Error    string       `json:"error,omitempty"`
	Decision decisionJSON `json:"decision"`
	Layout   layoutJSON   `json:"layout"`
	// DebugFrame is the index of the frame drawn on detect.jpg.
	DebugFrame *int `json:"debug_frame,omitempty"`
	// The frames, faces (in pixels) and groups, when there are any.
	*detect.Report
}

type decisionJSON struct {
	Face        bool               `json:"face"`
	Arrangement layout.Arrangement `json:"arrangement,omitempty"`
	Box         *layout.Box        `json:"box,omitempty"`
	Reason      layout.Reason      `json:"reason,omitempty"`
	Detail      string             `json:"detail,omitempty"`
}

type layoutJSON struct {
	SourceWidth  int                `json:"source_width"`
	SourceHeight int                `json:"source_height"`
	Arrangement  layout.Arrangement `json:"arrangement"`
	FaceFrame    *layout.Crop       `json:"face_frame,omitempty"`
	Gameplay     *layout.Crop       `json:"gameplay,omitempty"`
	Fallback     string             `json:"fallback,omitempty"`
}

// detectionDoc is detection.json for a render that has chosen its layout:
// the detector, its decision and why, the layout applied, and every frame's
// faces with the groups they formed.
func detectionDoc(res Result) detectionFile {
	d := res.Detection
	doc := detectionFile{
		Detector: res.Detector,
		Error:    res.DetectionError,
		Decision: decisionJSON{Face: d.Face, Reason: d.Reason, Detail: d.Detail},
		Layout: layoutJSON{
			SourceWidth: res.SourceWidth, SourceHeight: res.SourceHeight,
			Arrangement: res.Layout.Arrangement, Fallback: res.Layout.Fallback,
		},
	}
	if d.Face {
		box := d.Box
		doc.Decision.Arrangement, doc.Decision.Box = d.Arrangement, &box
	}
	if c := res.Layout.FaceFrame; !c.IsZero() {
		doc.Layout.FaceFrame = &c
	}
	if c := res.Layout.GameplayCrop; !c.IsZero() {
		doc.Layout.Gameplay = &c
	}
	if len(res.DetectionReport.Frames) > 0 {
		rep := res.DetectionReport
		doc.Report = &rep
		if i := rep.DebugFrame(); i >= 0 {
			doc.DebugFrame = &i
		}
	}
	return doc
}
