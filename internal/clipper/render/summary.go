package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
)

// Summary is a short, human-readable account of a render, one fact per
// line: the layout and the webcam decision, the caption lines, the clip's
// length, picture and size, the time per step and the tool versions.
// clipper render prints it, and clipper work logs it for every job.
func (r Result) Summary() string {
	var b strings.Builder
	line := func(key, value string) { fmt.Fprintf(&b, "%-14s%s\n", key, value) }
	line("arrangement", string(r.Layout.Arrangement))
	line("webcam", r.webcamSummary())
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

// webcamSummary says where the webcam is, or why there is none.
func (r Result) webcamSummary() string {
	det := "detector " + r.Detector
	switch {
	case r.DetectionError != "":
		return fmt.Sprintf("none (%s failed: %s)", det, r.DetectionError)
	case !r.Detection.Webcam:
		return fmt.Sprintf("none (%s)", det)
	case r.Layout.Fallback != "":
		return fmt.Sprintf("found at %s but not used: %s (%s)", formatRegion(r.Detection.Region), r.Layout.Fallback, det)
	}
	return fmt.Sprintf("%s, crop %s (%s)", formatRegion(r.Detection.Region), formatCrop(r.Layout.WebcamCrop), det)
}

func formatRegion(g layout.Region) string {
	return fmt.Sprintf("x=%.2f y=%.2f w=%.2f h=%.2f", g.X, g.Y, g.W, g.H)
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
