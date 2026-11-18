package detect

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// The fixtures in testdata are detect-faces answers in its exact output
// shape, for 8 frames of a 1920x1080 clip each.

func defaultRules(t *testing.T) Rules {
	t.Helper()
	t.Setenv(config.EnvConfigPath, "")
	cfg, err := config.LoadClipperRender("")
	if err != nil {
		t.Fatal(err)
	}
	return RulesFrom(cfg.WebcamDetection)
}

// fixture reads a recorded detect-faces answer through the same parser the
// real finder uses.
func fixture(t *testing.T, name string) []Image {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := range strings.Count(string(data), `"path"`) {
		paths = append(paths, fmt.Sprintf("/work/detect/frame-%02d.jpg", i+1))
	}
	images, err := parseOutput(data, paths)
	if err != nil {
		t.Fatal(err)
	}
	return images
}

func TestDecideFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		face    bool
		arrange layout.Arrangement
		reason  layout.Reason
		// For a face: the expected box height (fraction) and centre.
		h, cx, cy float64
		detail    string
	}{
		{fixture: "corner_face", face: true, arrange: layout.Stack, h: 84.0 / 1080, cx: 1672.0 / 1920, cy: 102.0 / 1080,
			detail: "steady face in 7 of 8 frames, 7.7% of the frame height, at most 20.0% for stack"},
		{fixture: "big_face", face: true, arrange: layout.FaceCrop, h: 390.0 / 1080, cx: 910.0 / 1920, cy: 415.0 / 1080,
			detail: "steady face in 8 of 8 frames, 36.1% of the frame height, above 20.0% for face crop"},
		{fixture: "game_faces", reason: layout.ReasonNoFace,
			detail: "no face found at the same place in 5 or more of 8 frames"},
		{fixture: "corner_with_game", face: true, arrange: layout.Stack, h: 84.0 / 1080, cx: 1672.0 / 1920, cy: 102.0 / 1080},
		{fixture: "two_faces_dominant", face: true, arrange: layout.Stack, h: 120.0 / 1080, cx: 347.5 / 1920, cy: 560.0 / 1080,
			detail: "times the next steady face"},
		{fixture: "two_faces_close", reason: layout.ReasonAmbiguous,
			detail: "2 steady faces; the tallest, 8.3% of the frame height, is 1.1"},
		{fixture: "no_faces", reason: layout.ReasonNoFace, detail: "no face found in 8 frames"},
		{fixture: "weak_faces", reason: layout.ReasonNoFace, detail: "no face found in 8 frames"},
		{fixture: "unreadable_frames", face: true, arrange: layout.Stack, h: 84.0 / 1080, cx: 1672.0 / 1920, cy: 102.0 / 1080,
			detail: "steady face in 5 of 6 frames"},
	}
	rules := defaultRules(t)
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			d := Decide(fixture(t, tt.fixture), rules)
			got := d.Detection
			if got.Face != tt.face || got.Reason != tt.reason {
				t.Fatalf("got face %v reason %q (%s), want face %v reason %q", got.Face, got.Reason, got.Detail, tt.face, tt.reason)
			}
			if tt.detail != "" && !strings.Contains(got.Detail, tt.detail) {
				t.Errorf("detail %q lacks %q", got.Detail, tt.detail)
			}
			if !tt.face {
				if d.Chosen != -1 {
					t.Errorf("chosen %d without a face", d.Chosen)
				}
				return
			}
			if got.Arrangement != tt.arrange {
				t.Errorf("arrangement %q, want %q", got.Arrangement, tt.arrange)
			}
			b := got.Box
			// The fixtures jitter by up to 4 pixels (12 for the big face).
			const tol = 0.012
			if math.Abs(b.H-tt.h) > tol || math.Abs(b.X+b.W/2-tt.cx) > tol || math.Abs(b.Y+b.H/2-tt.cy) > tol {
				t.Errorf("box %+v, want height %.3f centre %.3f,%.3f", b, tt.h, tt.cx, tt.cy)
			}
			if d.Chosen < 0 || d.Groups[d.Chosen].Box != b || !d.Groups[d.Chosen].Steady {
				t.Errorf("chosen group %d does not hold the box: %+v", d.Chosen, d.Groups)
			}
			t.Logf("%s: %s", tt.fixture, got.Detail)
		})
	}
}

// TestDecideAnnotatesFaces checks that every face says whether it was kept
// and which group it joined, and that the input is not changed.
func TestDecideAnnotatesFaces(t *testing.T) {
	images := fixture(t, "corner_with_game")
	d := Decide(images, defaultRules(t))
	for _, img := range images {
		for _, f := range img.Faces {
			if f.Kept || f.Group != 0 {
				t.Fatal("Decide changed its input")
			}
		}
	}
	steadyFaces := 0
	for fi, img := range d.Images {
		for _, f := range img.Faces {
			if !f.Kept || f.Group < 0 || f.Group >= len(d.Groups) {
				t.Errorf("frame %d: face %+v not annotated", fi, f)
				continue
			}
			if !containsInt(d.Groups[f.Group].Frames, fi) {
				t.Errorf("frame %d: face in group %d, which does not list the frame", fi, f.Group)
			}
			if f.Group == d.Chosen {
				steadyFaces++
			}
		}
	}
	if steadyFaces != 8 {
		t.Errorf("the corner face is in %d frames, want 8", steadyFaces)
	}
	weak := Decide(fixture(t, "weak_faces"), defaultRules(t))
	for _, img := range weak.Images {
		for _, f := range img.Faces {
			if f.Kept || f.Group != -1 {
				t.Errorf("weak face %+v should be dropped", f)
			}
		}
	}
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// img is a 1920x1080 frame with faces given as fractions: x, y, w, h,
// confidence.
func img(faces ...[5]float64) Image {
	im := Image{Width: 1920, Height: 1080, Faces: []Face{}}
	for _, f := range faces {
		im.Faces = append(im.Faces, Face{X: f[0] * 1920, Y: f[1] * 1080, W: f[2] * 1920, H: f[3] * 1080, Confidence: f[4]})
	}
	return im
}

// TestDecideRules checks each rule and its setting with small hand-made
// inputs.
func TestDecideRules(t *testing.T) {
	corner := [5]float64{0.85, 0.05, 0.04, 0.08, 0.9}
	frames := func(n int, faces ...[5]float64) []Image {
		out := make([]Image, n)
		for i := range out {
			out[i] = img(faces...)
		}
		return out
	}
	base := defaultRules(t)

	t.Run("steady share", func(t *testing.T) {
		// Found in 5 of 8: steady at 0.6. In 4 of 8: not.
		in := append(frames(5, corner), frames(3)...)
		if d := Decide(in, base); !d.Detection.Face {
			t.Errorf("5 of 8 should be steady: %s", d.Detection.Detail)
		}
		in = append(frames(4, corner), frames(4)...)
		if d := Decide(in, base); d.Detection.Face {
			t.Error("4 of 8 should not be steady at 0.6")
		}
		r := base
		r.SteadyShare = 0.5
		if d := Decide(in, r); !d.Detection.Face {
			t.Error("4 of 8 should be steady at 0.5")
		}
	})

	t.Run("movement", func(t *testing.T) {
		// The face shifts right by 0.4 face heights every frame: within
		// 0.5 of the median of the group, it stays one face for a while,
		// then becomes another.
		var in []Image
		for i := range 8 {
			shift := float64(i) * 0.4 * 0.08 * 1080 / 1920
			in = append(in, img([5]float64{0.3 + shift, 0.4, 0.04, 0.08, 0.9}))
		}
		if d := Decide(in, base); d.Detection.Face {
			t.Errorf("a face sliding across the frame should not be steady: %+v", d.Groups)
		}
		r := base
		r.MaxMovement = 5
		if d := Decide(in, r); !d.Detection.Face {
			t.Errorf("with max_movement 5 it is one face: %s", d.Detection.Detail)
		}
	})

	t.Run("min confidence and size", func(t *testing.T) {
		in := frames(8, [5]float64{0.5, 0.5, 0.04, 0.08, 0.65})
		if d := Decide(in, base); d.Detection.Face {
			t.Error("a 0.65 face should be dropped at 0.7")
		}
		r := base
		r.MinConfidence = 0.6
		if d := Decide(in, r); !d.Detection.Face {
			t.Error("a 0.65 face should be kept at 0.6")
		}
		in = frames(8, [5]float64{0.5, 0.5, 0.01, 0.025, 0.9})
		if d := Decide(in, base); d.Detection.Face {
			t.Error("a 2.5% face should be dropped at 3%")
		}
	})

	t.Run("dominance", func(t *testing.T) {
		small := [5]float64{0.1, 0.5, 0.04, 0.08, 0.9}
		for _, tt := range []struct {
			h    float64
			face bool
		}{{0.08 * 1.501, true}, {0.08 * 1.49, false}, {0.08 * 3, true}, {0.08, false}} {
			large := [5]float64{0.6, 0.3, tt.h * 0.5, tt.h, 0.9}
			d := Decide(frames(8, large, small), base)
			if d.Detection.Face != tt.face {
				t.Errorf("height %.3f against 0.08: face %v, want %v (%s)", tt.h, d.Detection.Face, tt.face, d.Detection.Detail)
			}
			if tt.face && math.Abs(d.Detection.Box.H-tt.h) > 1e-9 {
				t.Errorf("chose the box %+v, want the larger face", d.Detection.Box)
			}
		}
	})

	t.Run("face crop threshold", func(t *testing.T) {
		for _, tt := range []struct {
			h    float64
			want layout.Arrangement
		}{{0.19, layout.Stack}, {0.199, layout.Stack}, {0.201, layout.FaceCrop}, {0.6, layout.FaceCrop}} {
			d := Decide(frames(8, [5]float64{0.4, 0.2, tt.h * 0.5, tt.h, 0.9}), base)
			if d.Detection.Arrangement != tt.want {
				t.Errorf("height %.2f: %q, want %q", tt.h, d.Detection.Arrangement, tt.want)
			}
		}
	})

	t.Run("median box", func(t *testing.T) {
		var in []Image
		for _, x := range []float64{0.50, 0.51, 0.49, 0.52, 0.50, 0.48, 0.50, 0.70} {
			in = append(in, img([5]float64{x, 0.4, 0.05, 0.1, 0.9}))
		}
		d := Decide(in, base)
		if !d.Detection.Face || d.Detection.Box.X != 0.5 {
			t.Errorf("median x %v, want 0.5 (the outlier at 0.7 does not move it): %+v", d.Detection.Box.X, d.Groups)
		}
	})

	t.Run("nothing read", func(t *testing.T) {
		d := Decide([]Image{{Error: "cannot read image"}, {Error: "cannot read image"}}, base)
		if d.Analysed != 0 || d.Detection.Face || d.Detection.Reason != layout.ReasonNoFace {
			t.Errorf("got %+v", d)
		}
	})

	t.Run("bad numbers", func(t *testing.T) {
		in := frames(8, [5]float64{math.NaN(), 0.4, 0.05, 0.1, 0.9}, [5]float64{0.5, 0.4, 0, 0.1, 0.9})
		if d := Decide(in, base); d.Detection.Face {
			t.Errorf("faces with NaN or no width should be dropped: %+v", d.Groups)
		}
	})
}
