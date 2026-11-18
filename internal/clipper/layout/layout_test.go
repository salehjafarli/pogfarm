package layout

import (
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/config"
)

func newLayouter(t *testing.T, share float64) *Layouter {
	t.Helper()
	l, err := New(config.ClipperLayout{WebcamShare: share})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// corner is a webcam in the top-right quarter of the width, 30% high.
var corner = Region{X: 0.75, Y: 0, W: 0.25, H: 0.3}

func TestFilterStrings(t *testing.T) {
	tests := []struct {
		name       string
		w, h       int
		detection  Detection
		want       Arrangement
		wantFilter string
		wantCam    Crop
		wantGame   Crop
	}{
		{
			name: "stack 1920x1080", w: 1920, h: 1080, detection: WebcamAt(corner), want: Stack,
			wantFilter: "[0:v]split=2[layout_cam_in][layout_game_in];" +
				"[layout_cam_in]crop=480:284:1440:20,scale=1080:640,setsar=1[layout_cam];" +
				"[layout_game_in]crop=910:1080:504:0,scale=1080:1280,setsar=1[layout_game];" +
				"[layout_cam][layout_game]vstack=inputs=2,setsar=1[layout]",
			wantCam:  Crop{X: 1440, Y: 20, W: 480, H: 284},
			wantGame: Crop{X: 504, Y: 0, W: 910, H: 1080},
		},
		{
			name: "stack 1280x720", w: 1280, h: 720, detection: WebcamAt(corner), want: Stack,
			wantFilter: "[0:v]split=2[layout_cam_in][layout_game_in];" +
				"[layout_cam_in]crop=320:188:960:14,scale=1080:640,setsar=1[layout_cam];" +
				"[layout_game_in]crop=606:720:336:0,scale=1080:1280,setsar=1[layout_game];" +
				"[layout_cam][layout_game]vstack=inputs=2,setsar=1[layout]",
			wantCam:  Crop{X: 960, Y: 14, W: 320, H: 188},
			wantGame: Crop{X: 336, Y: 0, W: 606, H: 720},
		},
		{
			name: "blur fill 1920x1080", w: 1920, h: 1080, detection: NoWebcam(), want: BlurFill,
			wantFilter: "[0:v]split=2[layout_bg_in][layout_fg_in];" +
				"[layout_bg_in]crop=606:1080:656:0,scale=270:480,boxblur=12:2,scale=1080:1920,setsar=1[layout_bg];" +
				"[layout_fg_in]crop=1920:1080:0:0,scale=1080:608,setsar=1[layout_fg];" +
				"[layout_bg][layout_fg]overlay=0:656,setsar=1[layout]",
		},
		{
			name: "blur fill 1280x720", w: 1280, h: 720, detection: NoWebcam(), want: BlurFill,
			wantFilter: "[0:v]split=2[layout_bg_in][layout_fg_in];" +
				"[layout_bg_in]crop=404:720:438:0,scale=270:480,boxblur=12:2,scale=1080:1920,setsar=1[layout_bg];" +
				"[layout_fg_in]crop=1280:720:0:0,scale=1080:608,setsar=1[layout_fg];" +
				"[layout_bg][layout_fg]overlay=0:656,setsar=1[layout]",
		},
	}
	l := newLayouter(t, 1.0/3)
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
			if got.WebcamCrop != tt.wantCam || got.GameplayCrop != tt.wantGame {
				t.Errorf("crops: webcam %+v gameplay %+v, want %+v %+v", got.WebcamCrop, got.GameplayCrop, tt.wantCam, tt.wantGame)
			}
			checkInvariants(t, tt.w, tt.h, got)
		})
	}
}

func TestArrangementChoice(t *testing.T) {
	l := newLayouter(t, 1.0/3)
	for _, size := range [][2]int{{1920, 1080}, {1280, 720}} {
		withCam, err := l.Plan(size[0], size[1], WebcamAt(corner))
		if err != nil {
			t.Fatal(err)
		}
		if withCam.Arrangement != Stack {
			t.Errorf("%v with a webcam: got %q, want stack", size, withCam.Arrangement)
		}
		without, err := l.Plan(size[0], size[1], NoWebcam())
		if err != nil {
			t.Fatal(err)
		}
		if without.Arrangement != BlurFill || without.Fallback != "" {
			t.Errorf("%v without a webcam: got %q (fallback %q), want blur_fill", size, without.Arrangement, without.Fallback)
		}
		if !without.WebcamCrop.IsZero() || !without.GameplayCrop.IsZero() {
			t.Errorf("blur fill reports crops: %+v %+v", without.WebcamCrop, without.GameplayCrop)
		}
	}
	// The zero Detection means no webcam, whatever its region says.
	got, _ := l.Plan(1920, 1080, Detection{Region: corner})
	if got.Arrangement != BlurFill {
		t.Errorf("zero Detection: got %q, want blur_fill", got.Arrangement)
	}
}

func TestWebcamShare(t *testing.T) {
	l := newLayouter(t, 0.4)
	got, err := l.Plan(1920, 1080, WebcamAt(corner))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"scale=1080:768,setsar=1[layout_cam]", "scale=1080:1152,setsar=1[layout_game]"} {
		if !strings.Contains(got.Filter, want) {
			t.Errorf("filter lacks %q:\n%s", want, got.Filter)
		}
	}
	checkInvariants(t, 1920, 1080, got)
}

func TestClamping(t *testing.T) {
	tests := []struct {
		name   string
		w, h   int
		region Region
		want   Crop
	}{
		// Sticks out of the top and the right: the part inside is used.
		{"partly outside top right", 1920, 1080, Region{X: 0.9, Y: -0.1, W: 0.3, H: 0.4}, Crop{X: 1728, Y: 106, W: 192, H: 112}},
		// Exactly on the bottom-left corner.
		{"bottom left edge", 1280, 720, Region{X: 0, Y: 0.7, W: 0.25, H: 0.3}, Crop{X: 0, Y: 518, W: 320, H: 188}},
		// Wider than the frame on both sides.
		{"wider than the frame", 1280, 720, Region{X: -0.5, Y: 0.4, W: 2, H: 0.2}, Crop{X: 518, Y: 288, W: 242, H: 144}},
		// The whole frame.
		{"whole frame", 1920, 1080, Region{X: 0, Y: 0, W: 1, H: 1}, Crop{X: 48, Y: 0, W: 1822, H: 1080}},
		// Past the right edge by a rounding error.
		{"rounding past the edge", 1920, 1080, Region{X: 0.8, Y: 0.8, W: 0.2000001, H: 0.2000001}, Crop{X: 1546, Y: 864, W: 364, H: 216}},
	}
	l := newLayouter(t, 1.0/3)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Plan(tt.w, tt.h, WebcamAt(tt.region))
			if err != nil {
				t.Fatal(err)
			}
			if got.Arrangement != Stack {
				t.Fatalf("got %q (%s), want stack", got.Arrangement, got.Fallback)
			}
			if got.WebcamCrop != tt.want {
				t.Errorf("webcam crop %+v, want %+v", got.WebcamCrop, tt.want)
			}
			checkInvariants(t, tt.w, tt.h, got)
		})
	}
}

func TestDegenerateRegionsFallBack(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	tests := []struct {
		name   string
		region Region
		reason string
	}{
		{"zero width", Region{X: 0.5, Y: 0.5, W: 0, H: 0.2}, "no area"},
		{"zero height", Region{X: 0.5, Y: 0.5, W: 0.2, H: 0}, "no area"},
		{"negative size", Region{X: 0.5, Y: 0.5, W: -0.2, H: 0.2}, "no area"},
		{"zero value", Region{}, "no area"},
		{"NaN x", Region{X: nan, Y: 0, W: 0.2, H: 0.2}, "not a finite"},
		{"NaN height", Region{X: 0, Y: 0, W: 0.2, H: nan}, "not a finite"},
		{"infinite width", Region{X: 0, Y: 0, W: inf, H: 0.2}, "not a finite"},
		{"right of the frame", Region{X: 1.2, Y: 0.1, W: 0.2, H: 0.2}, "outside the frame"},
		{"left of the frame", Region{X: -0.5, Y: 0.1, W: 0.4, H: 0.2}, "outside the frame"},
		{"below the frame", Region{X: 0.1, Y: 1, W: 0.2, H: 0.2}, "outside the frame"},
		{"too small", Region{X: 0.5, Y: 0.5, W: 0.01, H: 0.01}, "below 32 pixels"},
		{"a thin strip", Region{X: 0, Y: 0.5, W: 1, H: 0.02}, "below 32 pixels"},
	}
	l := newLayouter(t, 1.0/3)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Plan(1280, 720, WebcamAt(tt.region))
			if err != nil {
				t.Fatal(err)
			}
			if got.Arrangement != BlurFill || !strings.Contains(got.Fallback, tt.reason) {
				t.Errorf("got %q with fallback %q, want blur_fill because %q", got.Arrangement, got.Fallback, tt.reason)
			}
			want := blurFill(1280, 720, "")
			if got.Filter != want.Filter {
				t.Errorf("fallback filter differs from plain blur fill:\n%s", got.Filter)
			}
			checkInvariants(t, 1280, 720, got)
		})
	}
}

// TestNoInvalidCrop throws random regions, including wild ones, at many
// source sizes, odd and portrait ones among them, and checks that every
// result is valid.
func TestNoInvalidCrop(t *testing.T) {
	rng := rand.New(rand.NewPCG(37, 1))
	sizes := [][2]int{{1920, 1080}, {1280, 720}, {1366, 768}, {1279, 719}, {854, 480}, {720, 1280}, {1080, 1920}, {32, 32}, {4096, 2160}}
	shares := []float64{config.MinWebcamShare, 1.0 / 3, 0.5, config.MaxWebcamShare}
	coord := func() float64 { return rng.Float64()*1.6 - 0.3 }
	for _, share := range shares {
		l := newLayouter(t, share)
		for _, size := range sizes {
			for range 300 {
				r := Region{X: coord(), Y: coord(), W: coord(), H: coord()}
				got, err := l.Plan(size[0], size[1], WebcamAt(r))
				if err != nil {
					t.Fatal(err)
				}
				checkInvariants(t, size[0], size[1], got)
				if t.Failed() {
					t.Fatalf("share %v, size %v, region %+v", share, size, r)
				}
			}
		}
	}
}

func TestSourceTooSmall(t *testing.T) {
	l := newLayouter(t, 1.0/3)
	for _, size := range [][2]int{{0, 0}, {-1920, 1080}, {1920, 31}, {31, 1080}} {
		if _, err := l.Plan(size[0], size[1], NoWebcam()); err == nil {
			t.Errorf("size %v: want an error", size)
		}
	}
}

func TestNewRejectsBadShare(t *testing.T) {
	for _, share := range []float64{0, 0.05, 1, math.NaN()} {
		if _, err := New(config.ClipperLayout{WebcamShare: share}); err == nil ||
			!strings.Contains(err.Error(), "clipper.layout.webcam_share") {
			t.Errorf("share %v: got %v, want a webcam_share error", share, err)
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
// positive, the two bands of a stack add up to the output, and the fragment
// reads and writes the documented labels.
func checkInvariants(t *testing.T, srcW, srcH int, r Result) {
	t.Helper()
	if !strings.HasPrefix(r.Filter, "[0:v]") || !strings.HasSuffix(r.Filter, "[layout]") {
		t.Errorf("fragment must read [0:v] and write [layout]: %s", r.Filter)
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	crops := cropPattern.FindAllStringSubmatch(r.Filter, -1)
	if len(crops) != 2 {
		t.Errorf("want 2 crops, got %d: %s", len(crops), r.Filter)
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
	for _, m := range scalePattern.FindAllStringSubmatch(r.Filter, -1) {
		w, h := atoi(m[1]), atoi(m[2])
		if w < 2 || h < 2 || w%2 != 0 || h%2 != 0 || w > OutWidth || h > OutHeight {
			t.Errorf("scale %s is not an even size within the output", m[0])
		}
	}
	switch r.Arrangement {
	case Stack:
		if r.Fallback != "" {
			t.Errorf("stack with a fallback reason %q", r.Fallback)
		}
		heights := scalePattern.FindAllStringSubmatch(r.Filter, -1)
		if len(heights) != 2 || atoi(heights[0][1]) != OutWidth || atoi(heights[1][1]) != OutWidth ||
			atoi(heights[0][2])+atoi(heights[1][2]) != OutHeight {
			t.Errorf("stack bands do not make %dx%d: %s", OutWidth, OutHeight, r.Filter)
		}
		for _, c := range []Crop{r.WebcamCrop, r.GameplayCrop} {
			if c.W < MinWebcamPixels || c.H < MinWebcamPixels || !strings.Contains(r.Filter, c.filter()+",") {
				t.Errorf("reported crop %+v is not in the filter or too small: %s", c, r.Filter)
			}
		}
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
