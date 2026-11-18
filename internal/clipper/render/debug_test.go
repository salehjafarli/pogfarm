package render

import (
	"testing"

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
)

func TestDebugFilter(t *testing.T) {
	frame := detect.Frame{Image: detect.Image{Width: 1920, Height: 1080, Faces: []detect.Face{
		{X: 1632.4, Y: 54, W: 96, H: 86.4, Confidence: 0.92, Kept: true, Group: 0},
		{X: 900, Y: 900, W: 40, H: 50, Confidence: 0.55, Group: -1},
	}}}
	d := layout.FaceAt(layout.Box{X: 0.85, Y: 0.05, W: 0.05, H: 0.08}, layout.Stack, "")
	l := layout.Result{Arrangement: layout.Stack,
		FaceFrame: layout.Crop{X: 1518, Y: 24, W: 324, H: 192}, GameplayCrop: layout.Crop{X: 504, W: 910, H: 1080}}
	want := "drawbox=x=1518:y=24:w=324:h=192:color=cyan:t=4," +
		"drawbox=x=504:y=0:w=910:h=1080:color=magenta:t=4," +
		"drawbox=x=1632:y=54:w=96:h=86:color=yellow:t=2," +
		"drawbox=x=900:y=900:w=40:h=50:color=red:t=2," +
		"drawbox=x=1632:y=54:w=96:h=86:color=lime:t=4"
	if got := debugFilter(debugMarks(frame, d, l)); got != want {
		t.Errorf("filter\n got %s\nwant %s", got, want)
	}

	// No face and no crop: only the faces found, thinner on a small frame.
	small := detect.Frame{Image: detect.Image{Width: 640, Height: 360, Faces: []detect.Face{{X: 10, Y: 20, W: 30, H: 40, Group: -1}}}}
	if got := debugFilter(debugMarks(small, layout.NoFace(layout.ReasonNoFace, ""), layout.Result{Arrangement: layout.BlurFill})); got !=
		"drawbox=x=10:y=20:w=30:h=40:color=red:t=1" {
		t.Errorf("small frame: %s", got)
	}
	if got := debugFilter(nil); got != "null" {
		t.Errorf("no marks: %s", got)
	}
}
