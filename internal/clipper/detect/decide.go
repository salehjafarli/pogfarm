package detect

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// Rules are the numbers of the decision, from clipper.webcam_detection.
// The package documentation explains each.
type Rules struct {
	MinConfidence  float64
	MinFaceHeight  float64
	SteadyShare    float64
	MaxMovement    float64
	Dominance      float64
	FaceCropHeight float64
}

// RulesFrom takes the rules from the config section.
func RulesFrom(cfg config.ClipperWebcamDetection) Rules {
	return Rules{
		MinConfidence:  cfg.MinConfidence,
		MinFaceHeight:  cfg.MinFaceHeight,
		SteadyShare:    cfg.SteadyShare,
		MaxMovement:    cfg.MaxMovement,
		Dominance:      cfg.Dominance,
		FaceCropHeight: cfg.FaceCropHeight,
	}
}

// Group is one face followed across frames: the faces found at roughly the
// same place in different frames.
type Group struct {
	// Frames are the indexes of the frames the face was found in.
	Frames []int `json:"frames"`
	// Box is the median of the face's boxes, as fractions of the frame.
	Box layout.Box `json:"box"`
	// Steady reports that the face was found in enough frames.
	Steady bool `json:"steady"`
}

// Decision is the outcome of Decide.
type Decision struct {
	// Detection is the answer: a face and its arrangement, or none and why.
	Detection layout.Detection
	// Images are the detector's images, with every face's Kept and Group
	// filled in.
	Images []Image
	// Groups are the faces followed across frames, in the order they were
	// first seen.
	Groups []Group
	// Chosen is the index in Groups of the face chosen, or -1.
	Chosen int
	// Analysed is the number of images the detector could read.
	Analysed int
	// Steady is the number of frames a face needed to be steady.
	Steady int
}

// Decide finds the streamer's face in the detector's answer for the frames
// of one clip. It is pure: the same images and rules always give the same
// decision.
//
//  1. Faces below MinConfidence or shorter than MinFaceHeight of the frame
//     are dropped.
//  2. The others are grouped across frames by position: a face joins the
//     nearest group whose median centre is at most MaxMovement of the
//     group's face height away, and that has no face from the same frame
//     yet; otherwise it starts a group. Faces are taken in the detector's
//     order, highest confidence first.
//  3. A group found in at least SteadyShare of the frames read is steady.
//  4. No steady face: none (no_face). Several: the tallest must be at least
//     Dominance times as tall as the next, or the clip is ambiguous.
//  5. The face's box is the median of its boxes. Taller than
//     FaceCropHeight of the frame: FaceCrop; otherwise Stack.
//
// Size means the box's height throughout, the same measure as the framing
// rules use.
func Decide(images []Image, r Rules) Decision {
	d := Decision{Chosen: -1, Images: make([]Image, len(images))}
	type group struct {
		frames []int
		boxes  []layout.Box
		median layout.Box
	}
	var groups []*group
	for fi, img := range images {
		img.Faces = slices.Clone(img.Faces)
		d.Images[fi] = img
		if img.Error != "" || img.Width <= 0 || img.Height <= 0 {
			continue
		}
		d.Analysed++
		fw, fh := float64(img.Width), float64(img.Height)
		taken := map[int]bool{}
		for i := range img.Faces {
			f := &img.Faces[i]
			f.Group, f.Kept = -1, false
			b := layout.Box{X: f.X / fw, Y: f.Y / fh, W: f.W / fw, H: f.H / fh}
			if !finite(b.X, b.Y, b.W, b.H, f.Confidence) || b.W <= 0 ||
				f.Confidence < r.MinConfidence || b.H < r.MinFaceHeight {
				continue
			}
			f.Kept = true
			cx, cy := b.X+b.W/2, b.Y+b.H/2
			best, bestDist := -1, math.Inf(1)
			for gi, g := range groups {
				if taken[gi] {
					continue
				}
				gcx, gcy := g.median.X+g.median.W/2, g.median.Y+g.median.H/2
				dist := math.Hypot((cx-gcx)*fw, (cy-gcy)*fh) / (g.median.H * fh)
				if dist <= r.MaxMovement && dist < bestDist {
					best, bestDist = gi, dist
				}
			}
			if best < 0 {
				groups = append(groups, &group{})
				best = len(groups) - 1
			}
			g := groups[best]
			g.frames = append(g.frames, fi)
			g.boxes = append(g.boxes, b)
			g.median = medianBox(g.boxes)
			taken[best] = true
			f.Group = best
		}
	}

	d.Steady = max(1, int(math.Ceil(r.SteadyShare*float64(d.Analysed)-1e-9)))
	var steady []int
	for gi, g := range groups {
		s := len(g.frames) >= d.Steady
		d.Groups = append(d.Groups, Group{Frames: g.frames, Box: g.median, Steady: s})
		if s {
			steady = append(steady, gi)
		}
	}
	// Tallest first; equal heights keep the order they were seen in.
	slices.SortStableFunc(steady, func(a, b int) int { return cmp.Compare(d.Groups[b].Box.H, d.Groups[a].Box.H) })

	pct := func(h float64) string { return fmt.Sprintf("%.1f%%", h*100) }
	switch {
	case d.Analysed == 0:
		d.Detection = layout.NoFace(layout.ReasonNoFace, "no frame could be read")
		return d
	case len(steady) == 0:
		detail := fmt.Sprintf("no face found in %d frames", d.Analysed)
		if len(groups) > 0 {
			detail = fmt.Sprintf("no face found at the same place in %d or more of %d frames", d.Steady, d.Analysed)
		}
		d.Detection = layout.NoFace(layout.ReasonNoFace, detail)
		return d
	}
	top := d.Groups[steady[0]]
	var ratio float64
	if len(steady) > 1 {
		next := d.Groups[steady[1]]
		ratio = top.Box.H / next.Box.H
		if ratio < r.Dominance {
			d.Detection = layout.NoFace(layout.ReasonAmbiguous, fmt.Sprintf(
				"%d steady faces; the tallest, %s of the frame height, is %.2f times the next, below %.2g",
				len(steady), pct(top.Box.H), ratio, r.Dominance))
			return d
		}
	}
	d.Chosen = steady[0]
	detail := fmt.Sprintf("steady face in %d of %d frames, %s of the frame height", len(top.Frames), d.Analysed, pct(top.Box.H))
	if ratio > 0 {
		detail += fmt.Sprintf(", %.2f times the next steady face", ratio)
	}
	arrangement := layout.Stack
	if top.Box.H > r.FaceCropHeight {
		arrangement = layout.FaceCrop
		detail += fmt.Sprintf(", above %s for face crop", pct(r.FaceCropHeight))
	} else {
		detail += fmt.Sprintf(", at most %s for stack", pct(r.FaceCropHeight))
	}
	d.Detection = layout.FaceAt(top.Box, arrangement, detail)
	return d
}

// medianBox is the median of each of the boxes' coordinates.
func medianBox(boxes []layout.Box) layout.Box {
	pick := func(get func(layout.Box) float64) float64 {
		v := make([]float64, len(boxes))
		for i, b := range boxes {
			v[i] = get(b)
		}
		slices.Sort(v)
		n := len(v)
		if n%2 == 1 {
			return v[n/2]
		}
		return (v[n/2-1] + v[n/2]) / 2
	}
	return layout.Box{
		X: pick(func(b layout.Box) float64 { return b.X }),
		Y: pick(func(b layout.Box) float64 { return b.Y }),
		W: pick(func(b layout.Box) float64 { return b.W }),
		H: pick(func(b layout.Box) float64 { return b.H }),
	}
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
