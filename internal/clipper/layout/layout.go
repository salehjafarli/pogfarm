package layout

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// Size of every clip.
const (
	OutWidth  = 1080
	OutHeight = 1920
)

// BandHeight is the height of the stack's face band: one third of the clip.
// The gameplay takes the rest.
const BandHeight = 640

// Labels of the filter graph fragment, without brackets.
const (
	// InputLabel is the source video the fragment reads: the video stream
	// of ffmpeg's first input.
	InputLabel = "0:v"
	// OutputLabel is the laid-out 1080x1920 picture the fragment writes.
	OutputLabel = "layout"
)

// MinSourceSide is the smallest source width and height accepted, in pixels.
const MinSourceSide = 32

// MinCropPixels is the smallest width and height, in source pixels, of a
// crop framed on a face. A smaller one would be a blur of a few pixels blown
// up to the output, so the clip falls back to BlurFill instead.
const MinCropPixels = 32

// EyeLevel is where the eyes are in a face box, as a share of its height
// from its top. YuNet's box runs from the forehead to the chin; the eyes sit
// a little above its middle. The eye line setting places this point.
const EyeLevel = 0.4

// Blur fill background: the zoomed copy is shrunk to this size, blurred and
// scaled back up. Blurring the small copy is cheaper and blurs more.
const (
	blurWidth  = OutWidth / 4
	blurHeight = OutHeight / 4
	blurRadius = 12
	blurPower  = 2
)

// Arrangement names one of the three ways a clip is laid out.
type Arrangement string

const (
	// Stack puts a band framed on the face on top and the gameplay below.
	Stack Arrangement = "stack"
	// FaceCrop frames the whole clip on the face.
	FaceCrop Arrangement = "face_crop"
	// BlurFill puts the whole frame over a blurred, zoomed copy of itself.
	BlurFill Arrangement = "blur_fill"
)

// Box is a rectangle in the source frame, as fractions of its width and
// height: X and W are fractions of the width, Y and H of the height, and
// (X, Y) is the top-left corner. The whole frame is {0, 0, 1, 1}.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Reason says why a clip has no face to frame.
type Reason string

const (
	// ReasonNoFace: no steady face was found.
	ReasonNoFace Reason = "no_face"
	// ReasonAmbiguous: several steady faces of similar size.
	ReasonAmbiguous Reason = "ambiguous"
	// ReasonOff: detection is switched off.
	ReasonOff Reason = "detector_off"
	// ReasonError: the detector failed or timed out on this clip.
	ReasonError Reason = "detector_error"
)

// Detection is the result of face detection for one clip (package
// internal/clipper/detect produces it): either a face, with its box and the
// arrangement it calls for, or none, with a reason.
//
// The zero value is "none" without a reason; use NoFace.
type Detection struct {
	// Face reports that a face was found. When false, Box and Arrangement
	// are ignored.
	Face bool
	// Box is the face's box in the source frame.
	Box Box
	// Arrangement is Stack or FaceCrop: what the face calls for.
	Arrangement Arrangement
	// Reason is why there is no face. It is empty when Face is true.
	Reason Reason
	// Detail explains the decision in words, for logs and the summary, for
	// example "one steady face in 7 of 8 frames, 7.9% of the frame height".
	Detail string
}

// NoFace is the result for a clip without a face to frame.
func NoFace(reason Reason, detail string) Detection {
	return Detection{Reason: reason, Detail: detail}
}

// FaceAt is the result for a clip with the face in b, calling for a (Stack
// or FaceCrop).
func FaceAt(b Box, a Arrangement, detail string) Detection {
	return Detection{Face: true, Box: b, Arrangement: a, Detail: detail}
}

// Crop is a rectangle in source pixels: W x H at offset (X, Y).
type Crop struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// IsZero reports whether c is the zero Crop, meaning "not used".
func (c Crop) IsZero() bool { return c == Crop{} }

// filter is c as the arguments of ffmpeg's crop filter.
func (c Crop) filter() string { return fmt.Sprintf("crop=%d:%d:%d:%d", c.W, c.H, c.X, c.Y) }

// Result is the layout chosen for one clip.
type Result struct {
	// Arrangement is the arrangement applied.
	Arrangement Arrangement
	// Filter is the filter graph fragment, from [InputLabel] to
	// [OutputLabel].
	Filter string
	// FaceFrame is the part of the source framed on the face, in source
	// pixels: the face band of Stack, or the whole clip of FaceCrop. It is
	// zero for BlurFill.
	FaceFrame Crop
	// GameplayCrop is the part of the source shown below the face band of
	// Stack, in source pixels. It is zero otherwise.
	GameplayCrop Crop
	// Fallback says why a detected face was not used and BlurFill was
	// applied instead. It is empty otherwise.
	Fallback string
}

// Layouter chooses the layout of each clip. It is safe for concurrent use.
type Layouter struct {
	stackShare float64 // face height / band height
	cropShare  float64 // face height / clip height
	eyeLine    float64 // eyes from the top / framed height
}

// New returns a Layouter for the clipper.layout config section.
func New(cfg config.ClipperLayout) (*Layouter, error) {
	if problems := cfg.Validate(); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return &Layouter{stackShare: cfg.StackFaceShare, cropShare: cfg.CropFaceShare, eyeLine: cfg.EyeLine}, nil
}

// Plan returns the layout of a clip whose source picture is srcW x srcH
// pixels, given the face detection result. A Detection with a face gives the
// arrangement it calls for, unless its box is unusable (see
// Result.Fallback); one without gives BlurFill. The only error is a source
// size below MinSourceSide.
func (l *Layouter) Plan(srcW, srcH int, d Detection) (Result, error) {
	if srcW < MinSourceSide || srcH < MinSourceSide {
		return Result{}, fmt.Errorf("layout: source size %dx%d is below the minimum of %dx%d",
			srcW, srcH, MinSourceSide, MinSourceSide)
	}
	if !d.Face {
		return blurFill(srcW, srcH, ""), nil
	}
	var outH int
	var share float64
	switch d.Arrangement {
	case Stack:
		outH, share = BandHeight, l.stackShare
	case FaceCrop:
		outH, share = OutHeight, l.cropShare
	default:
		return blurFill(srcW, srcH, fmt.Sprintf("the detection asks for an unknown arrangement %q", d.Arrangement)), nil
	}
	face, reason := faceRect(evenFloor(srcW), evenFloor(srcH), d.Box)
	if reason != "" {
		return blurFill(srcW, srcH, reason), nil
	}
	c := frameFace(evenFloor(srcW), evenFloor(srcH), face, OutWidth, outH, share, l.eyeLine)
	if c.W < MinCropPixels || c.H < MinCropPixels {
		return blurFill(srcW, srcH, fmt.Sprintf("face box %s leaves a %dx%d crop, below %d pixels a side",
			formatBox(d.Box), c.W, c.H, MinCropPixels)), nil
	}
	if d.Arrangement == Stack {
		return stack(srcW, srcH, c), nil
	}
	return faceCrop(c), nil
}

// rect is a rectangle in source pixels, not rounded.
type rect struct{ x0, y0, x1, y1 float64 }

func (r rect) w() float64 { return r.x1 - r.x0 }
func (r rect) h() float64 { return r.y1 - r.y0 }

// faceRect turns the face box into pixels of the fw x fh frame, keeping
// only the part inside it. It returns a reason instead when the box cannot
// be used.
func faceRect(fw, fh int, b Box) (rect, string) {
	for _, v := range []float64{b.X, b.Y, b.W, b.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return rect{}, fmt.Sprintf("face box %s is not a finite rectangle", formatBox(b))
		}
	}
	if b.W <= 0 || b.H <= 0 {
		return rect{}, fmt.Sprintf("face box %s has no area", formatBox(b))
	}
	r := rect{
		x0: clamp01(b.X) * float64(fw), x1: clamp01(b.X+b.W) * float64(fw),
		y0: clamp01(b.Y) * float64(fh), y1: clamp01(b.Y+b.H) * float64(fh),
	}
	if r.w() < 1 || r.h() < 1 {
		return rect{}, fmt.Sprintf("face box %s lies outside the frame", formatBox(b))
	}
	return r, ""
}

// frameFace returns the crop of the fw x fh frame (both even), with the
// aspect ow:oh, that frames face by the rules:
//
//   - size: the face is share of the crop's height. A crop that would be
//     larger than the frame is shrunk to fit, and the face is then larger;
//   - eye line: the eyes (EyeLevel down the face) sit eyeLine down the crop;
//   - centring: the crop is centred on the face horizontally;
//   - edges: the crop is shifted to stay inside the frame. When that moves
//     the face, it moves off-centre or off the eye line, but it is never
//     cut: the crop always contains the whole face when it is large enough
//     to. A face larger than the largest possible crop is cut evenly on
//     both sides.
//
// The crop's size and offset are even.
func frameFace(fw, fh int, face rect, ow, oh int, share, eyeLine float64) Crop {
	aspect := float64(ow) / float64(oh)
	ch := face.h() / share
	cw := ch * aspect
	if k := min(1, float64(fw)/cw, float64(fh)/ch); k < 1 {
		cw, ch = cw*k, ch*k
	}
	h := max(2, min(evenRound(ch), fh))
	w := max(2, min(evenRound(float64(h)*aspect), fw))

	cx := (face.x0 + face.x1) / 2
	eyeY := face.y0 + EyeLevel*face.h()
	x := place(cx-float64(w)/2, face.x0, face.x1, w, fw)
	y := place(eyeY-eyeLine*float64(h), face.y0, face.y1, h, fh)
	return Crop{X: x, Y: y, W: w, H: h}
}

// place returns the even offset of a crop of even size n inside a frame of
// even size total, as near to want as the rules allow: the crop holds the
// span [lo, hi] (the face) whole when it is large enough, and it stays
// inside the frame.
func place(want, lo, hi float64, n, total int) int {
	// Offsets that keep the face whole, on the even grid.
	first := evenCeil(int(math.Ceil(hi)) - n)
	last := evenFloor(int(math.Floor(lo)))
	first, last = max(first, 0), min(last, total-n)
	if first > last {
		// The face does not fit: centre the crop on it.
		return clampInt(evenRound((lo+hi)/2-float64(n)/2), 0, total-n)
	}
	return clampInt(evenRound(want), first, last)
}

// stack lays out the face band above a centre slice of the frame.
func stack(srcW, srcH int, face Crop) Result {
	gameH := OutHeight - BandHeight
	game := cover(0, 0, evenFloor(srcW), evenFloor(srcH), OutWidth, gameH)
	filter := fmt.Sprintf(
		"[%s]split=2[layout_face_in][layout_game_in];"+
			"[layout_face_in]%s,scale=%d:%d,setsar=1[layout_face];"+
			"[layout_game_in]%s,scale=%d:%d,setsar=1[layout_game];"+
			"[layout_face][layout_game]vstack=inputs=2,setsar=1[%s]",
		InputLabel,
		face.filter(), OutWidth, BandHeight,
		game.filter(), OutWidth, gameH,
		OutputLabel)
	return Result{Arrangement: Stack, Filter: filter, FaceFrame: face, GameplayCrop: game}
}

// faceCrop lays out the whole clip framed on the face.
func faceCrop(face Crop) Result {
	filter := fmt.Sprintf("[%s]%s,scale=%d:%d,setsar=1[%s]", InputLabel, face.filter(), OutWidth, OutHeight, OutputLabel)
	return Result{Arrangement: FaceCrop, Filter: filter, FaceFrame: face}
}

// blurFill lays out the whole frame over a blurred, zoomed copy of itself.
func blurFill(srcW, srcH int, fallback string) Result {
	fw, fh := evenFloor(srcW), evenFloor(srcH)
	// Background: the centre of the frame with the output's aspect, so the
	// zoomed copy fills the whole output.
	bg := cover(0, 0, fw, fh, OutWidth, OutHeight)
	// Foreground: the whole frame fitted inside the output. A 16:9 source
	// is scaled to the output width; a source narrower than 9:16 is scaled
	// to the output height instead.
	fgW, fgH := OutWidth, evenRound(float64(OutWidth)*float64(fh)/float64(fw))
	if fgH > OutHeight {
		fgW, fgH = evenRound(float64(OutHeight)*float64(fw)/float64(fh)), OutHeight
	}
	fgW, fgH = max(2, min(fgW, OutWidth)), max(2, min(fgH, OutHeight))
	x, y := evenFloor((OutWidth-fgW)/2), evenFloor((OutHeight-fgH)/2)
	filter := fmt.Sprintf(
		"[%s]split=2[layout_bg_in][layout_fg_in];"+
			"[layout_bg_in]%s,scale=%d:%d,boxblur=%d:%d,scale=%d:%d,setsar=1[layout_bg];"+
			"[layout_fg_in]%s,scale=%d:%d,setsar=1[layout_fg];"+
			"[layout_bg][layout_fg]overlay=%d:%d,setsar=1[%s]",
		InputLabel,
		bg.filter(), blurWidth, blurHeight, blurRadius, blurPower, OutWidth, OutHeight,
		Crop{W: fw, H: fh}.filter(), fgW, fgH,
		x, y, OutputLabel)
	return Result{Arrangement: BlurFill, Filter: filter, Fallback: fallback}
}

// cover returns the largest rectangle with the aspect tw:th that fits in the
// area w x h at (x, y), centred in it. Its size and offset are even, and it
// never leaves the area's right or bottom edge. The area must have a size of
// at least 2x2.
func cover(x, y, w, h, tw, th int) Crop {
	var cw, ch int
	if w*th > h*tw { // the area is wider than the target
		ch, cw = h, h*tw/th
	} else {
		cw, ch = w, w*th/tw
	}
	cw, ch = max(2, evenFloor(cw)), max(2, evenFloor(ch))
	// Flooring the offset to even moves the crop left or up by at most one
	// pixel, which keeps it inside the frame: x+(w-cw)/2 >= 0, and the right
	// edge stays at or before x+w.
	return Crop{X: evenFloor(x + (w-cw)/2), Y: evenFloor(y + (h-ch)/2), W: cw, H: ch}
}

func formatBox(b Box) string {
	return fmt.Sprintf("x=%.3g y=%.3g w=%.3g h=%.3g", b.X, b.Y, b.W, b.H)
}

// evenFloor rounds n down to an even number.
func evenFloor(n int) int { return n &^ 1 }

// evenCeil rounds n up to an even number.
func evenCeil(n int) int { return (n + 1) &^ 1 }

// evenRound rounds f to the nearest even integer.
func evenRound(f float64) int { return 2 * int(math.Round(f/2)) }

func clampInt(n, lo, hi int) int { return max(lo, min(n, hi)) }

func clamp01(f float64) float64 { return max(0, min(f, 1)) }
