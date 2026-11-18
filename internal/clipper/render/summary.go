package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
)

// Summary is a short, human-readable account of a render, one fact per
// line: the arrangement and why it was chosen, the face, the caption lines,
// the clip's length, picture and size, the time per step and the tool
// versions. clipper render prints it, and clipper work logs it for every
// job.
func (r Result) Summary() string {
	var b strings.Builder
	line := func(key, value string) { fmt.Fprintf(&b, "%-14s%s\n", key, value) }
	line("arrangement", string(r.Layout.Arrangement)+": "+r.Why())
	line("face", r.faceSummary())
	line("caption lines", fmt.Sprint(r.CaptionLines))
	line("clip", fmt.Sprintf("%s, %dx%d, %s", roundDuration(r.Duration), r.Width, r.Height, FormatBytes(r.Size)))
	steps := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		steps = append(steps, s.Name+" "+roundDuration(s.Took).String())
	}
	line("steps", strings.Join(steps, ", ")+" (total "+roundDuration(r.Total()).String()+")")
	if v, ok := r.Tools["ffmpeg"]; ok {
		line("ffmpeg", v)
	}
	return b.String()
}

// Why says in words why the arrangement was chosen: the detector's detail
// for a face, or the reason there is none.
func (r Result) Why() string {
	d := r.Detection
	switch {
	case r.DetectionError != "":
		return "detector failed: " + r.DetectionError
	case r.Layout.Fallback != "":
		return "face not usable: " + r.Layout.Fallback
	case d.Face && d.Detail != "":
		return d.Detail
	case d.Face:
		return "face found"
	}
	why := map[layout.Reason]string{
		layout.ReasonNoFace:    "no steady face",
		layout.ReasonAmbiguous: "ambiguous",
		layout.ReasonOff:       "face detection off",
		layout.ReasonError:     "detector failed",
	}[d.Reason]
	if why == "" {
		why = "no face"
	}
	if d.Detail != "" {
		why += ": " + d.Detail
	}
	return why
}

// faceSummary says where the face is and how it was framed, or that there
// is none, and names the detector.
func (r Result) faceSummary() string {
	det := "detector " + r.Detector
	if !r.Detection.Face {
		return fmt.Sprintf("none (%s)", det)
	}
	if r.Layout.FaceFrame.IsZero() {
		return fmt.Sprintf("%s, not framed (%s)", formatBox(r.Detection.Box), det)
	}
	return fmt.Sprintf("%s, framed by %s (%s)", formatBox(r.Detection.Box), formatCrop(r.Layout.FaceFrame), det)
}

func formatBox(g layout.Box) string {
	return fmt.Sprintf("x=%.3f y=%.3f w=%.3f h=%.3f", g.X, g.Y, g.W, g.H)
}

func formatCrop(c layout.Crop) string {
	return fmt.Sprintf("%dx%d at %d,%d", c.W, c.H, c.X, c.Y)
}

// roundDuration rounds a step time for display: to 10 ms below a second, to
// 100 ms above.
func roundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(10 * time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// FormatBytes formats a file size in decimal units, as Telegram states its
// limits: "812 B", "4.7 kB", "12.3 MB".
func FormatBytes(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}
