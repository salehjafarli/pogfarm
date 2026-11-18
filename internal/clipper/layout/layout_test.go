package layout

import (
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// defaults is the default clipper.layout section.
var defaults = config.ClipperLayout{StackFaceShare: 0.45, CropFaceShare: 0.25, EyeLine: 1.0 / 3}

func newLayouter(t *testing.T, cfg config.ClipperLayout) *Layouter {
	t.Helper()
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// corner is a webcam face in the top-right corner, 8% of the frame high.
var corner = Box{X: 0.85, Y: 0.05, W: 0.05, H: 0.08}

// big is a face filling the picture, 30% of the frame high.
var big = Box{X: 0.4, Y: 0.2, W: 0.15, H: 0.3}

func TestFilterStrings(t *testing.T) {
	tests := []struct {
		name       string
		w, h       int
		detection  Detection
		want       Arrangement
		wantFilter string
		wantFace   Crop
		wantGame   Crop
	}{
		{
			name: "stack 1920x1080", w: 1920, h: 1080, detection: FaceAt(corner, Stack, ""), want: Stack,
			wantFilter: "[0:v]split=2[layout_face_in][layout_game_in];" +
				"[layout_face_in]crop=324:192:1518:24,scale=1080:640,setsar=1[layout_face];" +
				"[layout_game_in]crop=910:1080:504:0,scale=1080:1280,setsar=1[layout_game];" +
				"[layout_face][layout_game]vstack=inputs=2,setsar=1[layout]",
			wantFace: Crop{X: 1518, Y: 24, W: 324, H: 192},
			wantGame: Crop{X: 504, Y: 0, W: 910, H: 1080},
		},
		{
			name: "stack 1280x720", w: 1280, h: 720, detection: FaceAt(corner, Stack, ""), want: Stack,
			wantFilter: "[0:v]split=2[layout_face_in][layout_game_in];" +
				"[layout_face_in]crop=216:128:1012:16,scale=1080:640,setsar=1[layout_face];" +
				"[layout_game_in]crop=606:720:336:0,scale=1080:1280,setsar=1[layout_game];" +
				"[layout_face][layout_game]vstack=inputs=2,setsar=1[layout]",
			wantFace: Crop{X: 1012, Y: 16, W: 216, H: 128},
			wantGame: Crop{X: 336, Y: 0, W: 606, H: 720},
		},
		{
			// The crop for 25% would be taller than the frame, so it is
			// the whole height and the face is 30% of it.
			name: "face crop 1920x1080", w: 1920, h: 1080, detection: FaceAt(big, FaceCrop, ""), want: FaceCrop,
			wantFilter: "[0:v]crop=608:1080:608:0,scale=1080:1920,setsar=1[layout]",
			wantFace:   Crop{X: 608, Y: 0, W: 608, H: 1080},
		},
		{
			name: "blur fill 1920x1080", w: 1920, h: 1080, detection: NoFace(ReasonNoFace, ""), want: BlurFill,
			wantFilter: "[0:v]split=2[layout_bg_in][layout_fg_in];" +
				"[layout_bg_in]crop=606:1080:656:0,scale=270:480,boxblur=12:2,scale=1080:1920,setsar=1[layout_bg];" +
				"[layout_fg_in]crop=1920:1080:0:0,scale=1080:608,setsar=1[layout_fg];" +
				"[layout_bg][layout_fg]overlay=0:656,setsar=1[layout]",
		},
		{
			name: "blur fill 1280x720", w: 1280, h: 720, detection: NoFace(ReasonOff, ""), want: BlurFill,
			wantFilter: "[0:v]split=2[layout_bg_in][layout_fg_in];" +
				"[layout_bg_in]crop=404:720:438:0,scale=270:480,boxblur=12:2,scale=1080:1920,setsar=1[layout_bg];" +
				"[layout_fg_in]crop=1280:720:0:0,scale=1080:608,setsar=1[layout_fg];" +
				"[layout_bg][layout_fg]overlay=0:656,setsar=1[layout]",
		},
	}
	l := newLayouter(t, defaults)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Plan(tt.w, tt.h, tt.detection)
			if err != nil {
				t.Fatal(err)
			}
			if got.Arrangement != tt.want || got.Fallback != "" {
				t.Errorf("arrangement %q (fallback %q), want %q", got.Arrangement, got.Fallback, tt.want)
			}
			if got.Filter != tt.wantFilter {
				t.Errorf("filter\n got %s\nwant %s", got.Filter, tt.wantFilter)
			}
			if got.FaceFrame != tt.wantFace || got.GameplayCrop != tt.wantGame {
				t.Errorf("crops: face %+v gameplay %+v, want %+v %+v", got.FaceFrame, got.GameplayCrop, tt.wantFace, tt.wantGame)
			}
			checkInvariants(t, tt.w, tt.h, got)
		})
	}
}

func TestArrangementChoice(t *testing.T) {
	l := newLayouter(t, defaults)
	for _, size := range [][2]int{{1920, 1080}, {1280, 720}, {1920, 884}} {
		for _, tt := range []struct {
			d    Detection
			want Arrangement
		}{
			{FaceAt(corner, Stack, ""), Stack},
			{FaceAt(big, FaceCrop, ""), FaceCrop},
			// The detection decides, not the size of the box.
			{FaceAt(big, Stack, ""), Stack},
			{FaceAt(corner, FaceCrop, ""), FaceCrop},
			{NoFace(ReasonNoFace, ""), BlurFill},
			{NoFace(ReasonAmbiguous, "two faces"), BlurFill},
			{NoFace(ReasonOff, ""), BlurFill},
			{NoFace(ReasonError, "timed out"), BlurFill},
		} {
			got, err := l.Plan(size[0], size[1], tt.d)
			if err != nil {
				t.Fatal(err)
			}
			if got.Arrangement != tt.want || got.Fallback != "" {
				t.Errorf("%v %+v: got %q (fallback %q), want %q", size, tt.d, got.Arrangement, got.Fallback, tt.want)
			}
			if tt.want == BlurFill && (!got.FaceFrame.IsZero() || !got.GameplayCrop.IsZero()) {
				t.Errorf("blur fill reports crops: %+v %+v", got.FaceFrame, got.GameplayCrop)
			}
			checkInvariants(t, size[0], size[1], got)
		}
	}
	// The zero Detection means no face, whatever its box says.
	got, _ := l.Plan(1920, 1080, Detection{Box: corner, Arrangement: Stack})
	if got.Arrangement != BlurFill {
		t.Errorf("zero Detection: got %q, want blur_fill", got.Arrangement)
	}
	got, _ = l.Plan(1920, 1080, Detection{Face: true, Box: corner, Arrangement: "sideways"})
	if got.Arrangement != BlurFill || !strings.Contains(got.Fallback, "unknown arrangement") {
		t.Errorf("unknown arrangement: got %q (%s)", got.Arrangement, got.Fallback)
	}
}

// framing is how a face sits in a crop: its height as a share of the
// crop's, where its eyes are as a share of the crop's height from the top,
// and its horizontal offset from the crop's centre in pixels.
type framing struct {
	share, eyeLine, offCentre float64
}

func measure(srcW, srcH int, b Box, c Crop) framing {
	fw, fh := float64(evenFloor(srcW)), float64(evenFloor(srcH))
	x0, y0, w, h := b.X*fw, b.Y*fh, b.W*fw, b.H*fh
	return framing{
		share:     h / float64(c.H),
		eyeLine:   (y0 + EyeLevel*h - float64(c.Y)) / float64(c.H),
		offCentre: (x0 + w/2) - (float64(c.X) + float64(c.W)/2),
	}
}

// TestFramingRules checks face share, eye line and centring for faces far
// from every edge, at the source sizes the clipper meets, the owner's
// 1920x884 test video and odd sizes among them.
func TestFramingRules(t *testing.T) {
	sizes := [][2]int{{1920, 1080}, {1280, 720}, {1920, 884}, {1279, 719}, {2560, 1440}}
	tests := []struct {
		name  string
		cfg   config.ClipperLayout
		a     Arrangement
		box   Box
		share float64
		eye   float64
	}{
		{"stack default", defaults, Stack, Box{X: 0.45, Y: 0.4, W: 0.04, H: 0.07}, 0.45, 1.0 / 3},
		{"stack small face", defaults, Stack, Box{X: 0.5, Y: 0.5, W: 0.03, H: 0.05}, 0.45, 1.0 / 3},
		{"stack tuned", config.ClipperLayout{StackFaceShare: 0.3, CropFaceShare: 0.25, EyeLine: 0.4}, Stack,
			Box{X: 0.45, Y: 0.4, W: 0.05, H: 0.09}, 0.3, 0.4},
		{"face crop default", defaults, FaceCrop, Box{X: 0.45, Y: 0.4, W: 0.06, H: 0.12}, 0.25, 1.0 / 3},
		{"face crop tuned", config.ClipperLayout{StackFaceShare: 0.45, CropFaceShare: 0.2, EyeLine: 0.3}, FaceCrop,
			Box{X: 0.45, Y: 0.35, W: 0.08, H: 0.15}, 0.2, 0.3},
	}
	for _, tt := range tests {
		l := newLayouter(t, tt.cfg)
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s %dx%d", tt.name, size[0], size[1]), func(t *testing.T) {
				got, err := l.Plan(size[0], size[1], FaceAt(tt.box, tt.a, ""))
				if err != nil {
					t.Fatal(err)
				}
				if got.Arrangement != tt.a {
					t.Fatalf("got %q (%s)", got.Arrangement, got.Fallback)
				}
				checkInvariants(t, size[0], size[1], got)
				m := measure(size[0], size[1], tt.box, got.FaceFrame)
				// Even rounding moves the crop by at most two pixels and
				// its size by one, so the tolerance depends on the size.
				tol := 3 / float64(got.FaceFrame.H)
				if math.Abs(m.share-tt.share) > tol {
					t.Errorf("face share %.4f, want %.4f (crop %+v)", m.share, tt.share, got.FaceFrame)
				}
				if math.Abs(m.eyeLine-tt.eye) > tol {
					t.Errorf("eye line %.4f, want %.4f (crop %+v)", m.eyeLine, tt.eye, got.FaceFrame)
				}
				if math.Abs(m.offCentre) > 2 {
					t.Errorf("face is %.1f pixels off centre (crop %+v)", m.offCentre, got.FaceFrame)
				}
				wantAspect := float64(OutWidth) / float64(OutHeight)
				if tt.a == Stack {
					wantAspect = float64(OutWidth) / float64(BandHeight)
				}
				if a := float64(got.FaceFrame.W) / float64(got.FaceFrame.H); math.Abs(a-wantAspect)/wantAspect > 0.02 {
					t.Errorf("crop aspect %.4f, want %.4f", a, wantAspect)
				}
			})
		}
	}
}

// TestFramingSmallFaceZoomsIn checks that the crop follows the face's
// size: a face half as tall gets a crop half as tall.
func TestFramingSmallFaceZoomsIn(t *testing.T) {
	l := newLayouter(t, defaults)
	small, _ := l.Plan(1920, 1080, FaceAt(Box{X: 0.5, Y: 0.5, W: 0.03, H: 0.05}, Stack, ""))
	large, _ := l.Plan(1920, 1080, FaceAt(Box{X: 0.5, Y: 0.4, W: 0.06, H: 0.1}, Stack, ""))
	if r := float64(large.FaceFrame.H) / float64(small.FaceFrame.H); math.Abs(r-2) > 0.05 {
		t.Errorf("crop heights %d and %d, want a ratio of 2", small.FaceFrame.H, large.FaceFrame.H)
	}
}

// TestFramingEdges puts faces against every edge and corner. The crop must
// stay inside the frame, touch the edge, and hold the whole face, which is
// then off-centre or off the eye line instead.
func TestFramingEdges(t *testing.T) {
	tests := []struct {
		name string
		box  Box
		// edges the crop must touch: left, top, right, bottom
		left, top, right, bottom bool
	}{
		{"top left", Box{X: 0, Y: 0, W: 0.05, H: 0.08}, true, true, false, false},
		{"top right", Box{X: 0.95, Y: 0, W: 0.05, H: 0.08}, false, true, true, false},
		{"bottom left", Box{X: 0, Y: 0.92, W: 0.05, H: 0.08}, true, false, false, true},
		{"bottom right", Box{X: 0.95, Y: 0.92, W: 0.05, H: 0.08}, false, false, true, true},
		{"left middle", Box{X: 0.01, Y: 0.5, W: 0.05, H: 0.08}, true, false, false, false},
		{"right middle", Box{X: 0.94, Y: 0.5, W: 0.05, H: 0.08}, false, false, true, false},
		{"top middle", Box{X: 0.5, Y: 0.005, W: 0.05, H: 0.08}, false, true, false, false},
		{"bottom middle", Box{X: 0.5, Y: 0.91, W: 0.05, H: 0.08}, false, false, false, true},
		// Partly outside the frame: the part inside is framed.
		{"sticking out top right", Box{X: 0.97, Y: -0.02, W: 0.05, H: 0.08}, false, true, true, false},
	}
	l := newLayouter(t, defaults)
	for _, a := range []Arrangement{Stack, FaceCrop} {
		for _, size := range [][2]int{{1920, 1080}, {1280, 720}, {1920, 884}, {1279, 719}} {
			fw, fh := evenFloor(size[0]), evenFloor(size[1])
			for _, tt := range tests {
				t.Run(fmt.Sprintf("%s %dx%d %s", a, size[0], size[1], tt.name), func(t *testing.T) {
					got, err := l.Plan(size[0], size[1], FaceAt(tt.box, a, ""))
					if err != nil {
						t.Fatal(err)
					}
					if got.Arrangement != a {
						t.Fatalf("got %q (%s)", got.Arrangement, got.Fallback)
					}
					checkInvariants(t, size[0], size[1], got)
					c := got.FaceFrame
					for _, e := range []struct {
						want, at bool
						name     string
					}{
						{tt.left, c.X == 0, "left"}, {tt.top, c.Y == 0, "top"},
						{tt.right, c.X+c.W == fw, "right"}, {tt.bottom, c.Y+c.H == fh, "bottom"},
					} {
						if e.want && !e.at {
							t.Errorf("crop %+v does not touch the %s edge of %dx%d", c, e.name, fw, fh)
						}
					}
					checkFaceWhole(t, fw, fh, tt.box, c)
				})
			}
		}
	}
}

// checkFaceWhole checks that the part of the face inside the frame is
// inside the crop.
func checkFaceWhole(t *testing.T, fw, fh int, b Box, c Crop) {
	t.Helper()
	x0, x1 := clamp01(b.X)*float64(fw), clamp01(b.X+b.W)*float64(fw)
	y0, y1 := clamp01(b.Y)*float64(fh), clamp01(b.Y+b.H)*float64(fh)
	if x0 < float64(c.X) || y0 < float64(c.Y) || x1 > float64(c.X+c.W) || y1 > float64(c.Y+c.H) {
		t.Errorf("face %.1f,%.1f-%.1f,%.1f is cut by crop %+v", x0, y0, x1, y1, c)
	}
}

// TestFramingGivesUpForBigFaces: a face too big for the framing makes the
// crop as large as the frame allows; the face is then larger than its share
// but still whole, and centred when it cannot sit on the eye line.
func TestFramingGivesUpForBigFaces(t *testing.T) {
	l := newLayouter(t, defaults)
	// Stack: a 40% face wants a 889-pixel band, more than the frame's
	// 1080 allows at 1080:640, so the band is the full height.
	got, _ := l.Plan(1920, 1080, FaceAt(Box{X: 0.45, Y: 0.3, W: 0.12, H: 0.4}, Stack, ""))
	checkInvariants(t, 1920, 1080, got)
	checkFaceWhole(t, 1920, 1080, Box{X: 0.45, Y: 0.3, W: 0.12, H: 0.4}, got.FaceFrame)
	if got.FaceFrame.H > 1080 {
		t.Errorf("crop %+v taller than the frame", got.FaceFrame)
	}
	// Face crop: a face wider than the widest 9:16 crop (608 pixels) is
	// cut evenly on both sides.
	wide := Box{X: 0.2, Y: 0.1, W: 0.5, H: 0.8}
	got, _ = l.Plan(1920, 1080, FaceAt(wide, FaceCrop, ""))
	checkInvariants(t, 1920, 1080, got)
	if got.FaceFrame != (Crop{X: 560, Y: 0, W: 608, H: 1080}) {
		t.Errorf("wide face crop %+v, want 608x1080 at 560,0 (centred on the face)", got.FaceFrame)
	}
}

func TestDegenerateBoxesFallBack(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	tests := []struct {
		name   string
		box    Box
		reason string
	}{
		{"zero width", Box{X: 0.5, Y: 0.5, W: 0, H: 0.2}, "no area"},
		{"zero height", Box{X: 0.5, Y: 0.5, W: 0.2, H: 0}, "no area"},
		{"negative size", Box{X: 0.5, Y: 0.5, W: -0.2, H: 0.2}, "no area"},
		{"zero value", Box{}, "no area"},
		{"NaN x", Box{X: nan, Y: 0, W: 0.2, H: 0.2}, "not a finite"},
		{"NaN height", Box{X: 0, Y: 0, W: 0.2, H: nan}, "not a finite"},
		{"infinite width", Box{X: 0, Y: 0, W: inf, H: 0.2}, "not a finite"},
		{"right of the frame", Box{X: 1.2, Y: 0.1, W: 0.2, H: 0.2}, "outside the frame"},
		{"left of the frame", Box{X: -0.5, Y: 0.1, W: 0.4, H: 0.2}, "outside the frame"},
		{"below the frame", Box{X: 0.1, Y: 1, W: 0.2, H: 0.2}, "outside the frame"},
		{"too small", Box{X: 0.5, Y: 0.5, W: 0.005, H: 0.008}, "below 32 pixels"},
	}
	l := newLayouter(t, defaults)
	for _, a := range []Arrangement{Stack, FaceCrop} {
		for _, tt := range tests {
			t.Run(string(a)+" "+tt.name, func(t *testing.T) {
				got, err := l.Plan(1280, 720, FaceAt(tt.box, a, ""))
				if err != nil {
					t.Fatal(err)
				}
				if got.Arrangement != BlurFill || !strings.Contains(got.Fallback, tt.reason) {
					t.Errorf("got %q with fallback %q, want blur_fill because %q", got.Arrangement, got.Fallback, tt.reason)
				}
				if want := blurFill(1280, 720, ""); got.Filter != want.Filter {
					t.Errorf("fallback filter differs from plain blur fill:\n%s", got.Filter)
				}
				checkInvariants(t, 1280, 720, got)
			})
		}
	}
}

// TestNoInvalidCrop throws random boxes, including wild ones, at many source
// sizes, odd and portrait ones among them, with the settings at their
// bounds, and checks that every result is valid and never cuts a face that
// fits.
func TestNoInvalidCrop(t *testing.T) {
	rng := rand.New(rand.NewPCG(59, 1))
	sizes := [][2]int{{1920, 1080}, {1280, 720}, {1920, 884}, {1366, 768}, {1279, 719}, {854, 480}, {720, 1280}, {1080, 1920}, {32, 32}, {4096, 2160}}
	cfgs := []config.ClipperLayout{
		defaults,
		{StackFaceShare: config.MinFaceShare, CropFaceShare: config.MinFaceShare, EyeLine: config.MinEyeLine},
		{StackFaceShare: config.MaxFaceShare, CropFaceShare: config.MaxFaceShare, EyeLine: config.MaxEyeLine},
		{StackFaceShare: config.MaxFaceShare, CropFaceShare: config.MaxFaceShare, EyeLine: config.MinEyeLine},
	}
	coord := func() float64 { return rng.Float64()*1.6 - 0.3 }
	for _, cfg := range cfgs {
		l := newLayouter(t, cfg)
		for _, size := range sizes {
			for range 200 {
				b := Box{X: coord(), Y: coord(), W: coord(), H: coord()}
				for _, a := range []Arrangement{Stack, FaceCrop} {
					got, err := l.Plan(size[0], size[1], FaceAt(b, a, ""))
					if err != nil {
						t.Fatal(err)
					}
					checkInvariants(t, size[0], size[1], got)
					if got.Arrangement != BlurFill {
						fw, fh := evenFloor(size[0]), evenFloor(size[1])
						c := got.FaceFrame
						// Only a face larger than the crop may be cut.
						if clamp01(b.X+b.W)*float64(fw)-clamp01(b.X)*float64(fw) < float64(c.W-2) &&
							clamp01(b.Y+b.H)*float64(fh)-clamp01(b.Y)*float64(fh) < float64(c.H-2) {
							checkFaceWhole(t, fw, fh, b, c)
						}
					}
					if t.Failed() {
						t.Fatalf("config %+v, size %v, box %+v, %s", cfg, size, b, a)
					}
				}
			}
		}
	}
}

func TestSourceTooSmall(t *testing.T) {
	l := newLayouter(t, defaults)
	for _, size := range [][2]int{{0, 0}, {-1920, 1080}, {1920, 31}, {31, 1080}} {
		if _, err := l.Plan(size[0], size[1], NoFace(ReasonNoFace, "")); err == nil {
			t.Errorf("size %v: want an error", size)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, cfg := range []config.ClipperLayout{
		{StackFaceShare: 0, CropFaceShare: 0.25, EyeLine: 0.3},
		{StackFaceShare: 0.45, CropFaceShare: math.NaN(), EyeLine: 0.3},
		{StackFaceShare: 0.45, CropFaceShare: 0.25, EyeLine: 1},
	} {
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "clipper.layout.") {
			t.Errorf("%+v: got %v, want a clipper.layout error", cfg, err)
		}
	}
}

var (
	cropPattern  = regexp.MustCompile(`crop=(-?\d+):(-?\d+):(-?\d+):(-?\d+)`)
	scalePattern = regexp.MustCompile(`scale=(-?\d+):(-?\d+)`)
	numPattern   = regexp.MustCompile(`overlay=(-?\d+):(-?\d+)`)
)

// checkInvariants checks what every result must satisfy: every crop lies
// inside the source frame, every crop and scale size and offset is even and
// positive, the arrangement fills 1080x1920, and the fragment reads and
// writes the documented labels.
func checkInvariants(t *testing.T, srcW, srcH int, r Result) {
	t.Helper()
	if !strings.HasPrefix(r.Filter, "[0:v]") || !strings.HasSuffix(r.Filter, "[layout]") {
		t.Errorf("fragment must read [0:v] and write [layout]: %s", r.Filter)
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	crops := cropPattern.FindAllStringSubmatch(r.Filter, -1)
	wantCrops := map[Arrangement]int{Stack: 2, FaceCrop: 1, BlurFill: 2}[r.Arrangement]
	if len(crops) != wantCrops {
		t.Errorf("want %d crops, got %d: %s", wantCrops, len(crops), r.Filter)
	}
	for _, m := range crops {
		w, h, x, y := atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])
		if w < 2 || h < 2 || x < 0 || y < 0 || x+w > srcW || y+h > srcH {
			t.Errorf("crop %s leaves the %dx%d frame", m[0], srcW, srcH)
		}
		if w%2 != 0 || h%2 != 0 || x%2 != 0 || y%2 != 0 {
			t.Errorf("crop %s is not even", m[0])
		}
	}
	scales := scalePattern.FindAllStringSubmatch(r.Filter, -1)
	for _, m := range scales {
		w, h := atoi(m[1]), atoi(m[2])
		if w < 2 || h < 2 || w%2 != 0 || h%2 != 0 || w > OutWidth || h > OutHeight {
			t.Errorf("scale %s is not an even size within the output", m[0])
		}
	}
	reported := func(c Crop) {
		if c.W < MinCropPixels || c.H < MinCropPixels || !strings.Contains(r.Filter, c.filter()+",") {
			t.Errorf("reported crop %+v is not in the filter or too small: %s", c, r.Filter)
		}
	}
	switch r.Arrangement {
	case Stack:
		if r.Fallback != "" {
			t.Errorf("stack with a fallback reason %q", r.Fallback)
		}
		if len(scales) != 2 || atoi(scales[0][1]) != OutWidth || atoi(scales[1][1]) != OutWidth ||
			atoi(scales[0][2]) != BandHeight || atoi(scales[0][2])+atoi(scales[1][2]) != OutHeight {
			t.Errorf("stack bands do not make %dx%d: %s", OutWidth, OutHeight, r.Filter)
		}
		reported(r.FaceFrame)
		reported(r.GameplayCrop)
	case FaceCrop:
		if r.Fallback != "" || !r.GameplayCrop.IsZero() {
			t.Errorf("face crop with a fallback %q or a gameplay crop %+v", r.Fallback, r.GameplayCrop)
		}
		if len(scales) != 1 || atoi(scales[0][1]) != OutWidth || atoi(scales[0][2]) != OutHeight {
			t.Errorf("face crop does not make %dx%d: %s", OutWidth, OutHeight, r.Filter)
		}
		reported(r.FaceFrame)
	case BlurFill:
		m := numPattern.FindStringSubmatch(r.Filter)
		if m == nil || atoi(m[1])%2 != 0 || atoi(m[2])%2 != 0 || atoi(m[1]) < 0 || atoi(m[2]) < 0 {
			t.Errorf("overlay position is not even and inside: %s", r.Filter)
		}
		if !strings.Contains(r.Filter, "scale=1080:1920,setsar=1[layout_bg]") {
			t.Errorf("background does not fill the output: %s", r.Filter)
		}
	default:
		t.Errorf("unknown arrangement %q", r.Arrangement)
	}
}
