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

// MinWebcamPixels is the smallest width and height, in source pixels, of a
// webcam crop. A smaller one would be a blur of a few pixels blown up to the
// band, so the clip falls back to BlurFill instead.
const MinWebcamPixels = 32

// Blur fill background: the zoomed copy is shrunk to this size, blurred and
// scaled back up. Blurring the small copy is cheaper and blurs more.
const (
	blurWidth  = OutWidth / 4
	blurHeight = OutHeight / 4
	blurRadius = 12
	blurPower  = 2
)

// Arrangement names one of the two ways a clip is laid out.
type Arrangement string

const (
	// Stack puts the webcam on top and the gameplay below.
	Stack Arrangement = "stack"
	// BlurFill puts the whole frame over a blurred, zoomed copy of itself.
	BlurFill Arrangement = "blur_fill"
)

// Region is a rectangle in the source frame, as fractions of its width and
// height: X and W are fractions of the width, Y and H of the height, and
// (X, Y) is the top-left corner. The whole frame is {0, 0, 1, 1}.
type Region struct {
	X, Y, W, H float64
}

// Detection is the result of webcam detection for one clip: either no webcam
// (the zero value), or a webcam in Region. Package detect (#59) produces it.
type Detection struct {
	// Webcam reports that a webcam was found. When false, Region is
	// ignored.
	Webcam bool
	// Region is where the webcam is in the source frame.
	Region Region
}

// NoWebcam is the result for a clip without a webcam.
func NoWebcam() Detection { return Detection{} }

// WebcamAt is the result for a clip with its webcam in r.
func WebcamAt(r Region) Detection { return Detection{Webcam: true, Region: r} }

// Crop is a rectangle in source pixels: W x H at offset (X, Y).
type Crop struct {
	X, Y, W, H int
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
	// WebcamCrop is the part of the source shown in the webcam band, in
	// source pixels. It is zero for BlurFill.
	WebcamCrop Crop
	// GameplayCrop is the part of the source shown in the gameplay band,
	// in source pixels. It is zero for BlurFill.
	GameplayCrop Crop
	// Fallback says why a detected webcam was not used and BlurFill was
	// applied instead. It is empty otherwise.
	Fallback string
}

// Layouter chooses the layout of each clip. It is safe for concurrent use.
type Layouter struct {
	webcamHeight int // height of the stack's webcam band, even
}

// New returns a Layouter for the clipper.layout config section.
func New(cfg config.ClipperLayout) (*Layouter, error) {
	if problems := cfg.Validate(); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	h := evenRound(OutHeight * cfg.WebcamShare)
	h = max(2, min(h, OutHeight-2))
	return &Layouter{webcamHeight: h}, nil
}

// Plan returns the layout of a clip whose source picture is srcW x srcH
// pixels, given the webcam detection result. A Detection with a webcam gives
// Stack, unless its region is unusable (see Result.Fallback); one without
// gives BlurFill. The only error is a source size below MinSourceSide.
func (l *Layouter) Plan(srcW, srcH int, d Detection) (Result, error) {
	if srcW < MinSourceSide || srcH < MinSourceSide {
		return Result{}, fmt.Errorf("layout: source size %dx%d is below the minimum of %dx%d",
			srcW, srcH, MinSourceSide, MinSourceSide)
	}
	if !d.Webcam {
		return blurFill(srcW, srcH, ""), nil
	}
	cam, reason := l.webcamCrop(srcW, srcH, d.Region)
	if reason != "" {
		return blurFill(srcW, srcH, reason), nil
	}
	return l.stack(srcW, srcH, cam), nil
}

// webcamCrop turns the webcam region into a crop with the aspect of the
// webcam band, inside the frame. It returns a reason instead when the region
// cannot be used.
func (l *Layouter) webcamCrop(srcW, srcH int, r Region) (Crop, string) {
	for _, v := range []float64{r.X, r.Y, r.W, r.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Crop{}, fmt.Sprintf("webcam region %+v is not a finite rectangle", r)
		}
	}
	if r.W <= 0 || r.H <= 0 {
		return Crop{}, fmt.Sprintf("webcam region %+v has no area", r)
	}
	fw, fh := evenFloor(srcW), evenFloor(srcH)
	// Clamp: keep the part of the region inside the frame.
	x0 := int(math.Round(clamp01(r.X) * float64(fw)))
	x1 := int(math.Round(clamp01(r.X+r.W) * float64(fw)))
	y0 := int(math.Round(clamp01(r.Y) * float64(fh)))
	y1 := int(math.Round(clamp01(r.Y+r.H) * float64(fh)))
	if x1 <= x0 || y1 <= y0 {
		return Crop{}, fmt.Sprintf("webcam region %+v lies outside the frame", r)
	}
	c := cover(x0, y0, x1-x0, y1-y0, OutWidth, l.webcamHeight)
	if c.W < MinWebcamPixels || c.H < MinWebcamPixels {
		return Crop{}, fmt.Sprintf("webcam region %+v leaves a %dx%d crop, below %d pixels a side",
			r, c.W, c.H, MinWebcamPixels)
	}
	return c, ""
}

// stack lays out the webcam crop above a centre slice of the frame.
func (l *Layouter) stack(srcW, srcH int, cam Crop) Result {
	gameH := OutHeight - l.webcamHeight
	game := cover(0, 0, evenFloor(srcW), evenFloor(srcH), OutWidth, gameH)
	filter := fmt.Sprintf(
		"[%s]split=2[layout_cam_in][layout_game_in];"+
			"[layout_cam_in]%s,scale=%d:%d,setsar=1[layout_cam];"+
			"[layout_game_in]%s,scale=%d:%d,setsar=1[layout_game];"+
			"[layout_cam][layout_game]vstack=inputs=2,setsar=1[%s]",
		InputLabel,
		cam.filter(), OutWidth, l.webcamHeight,
		game.filter(), OutWidth, gameH,
		OutputLabel)
	return Result{Arrangement: Stack, Filter: filter, WebcamCrop: cam, GameplayCrop: game}
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

// evenFloor rounds a non-negative n down to an even number.
func evenFloor(n int) int { return n &^ 1 }

// evenRound rounds f to the nearest even integer.
func evenRound(f float64) int { return 2 * int(math.Round(f/2)) }

func clamp01(f float64) float64 { return max(0, min(f, 1)) }
