//go:build linux || darwin

package render

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
	"github.com/salehjafarli/pogfarm/internal/clipper/download"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
)

// Tests in this file drive Render with fake ffmpeg and ffprobe programs
// (shell scripts), so the order of the steps, the files written, the
// command line and the error handling are checked on any Unix machine.
// media_test.go runs the real tools.

const fakeClipBytes = "not really a video"

const sourceProbe = `{"streams": [
	{"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080, "pix_fmt": "yuv420p"},
	{"codec_type": "audio", "codec_name": "aac"}],
	"format": {"duration": "3.000000"}}`

const clipProbe = `{"streams": [
	{"codec_type": "video", "codec_name": "h264", "width": 1080, "height": 1920, "pix_fmt": "yuv420p"},
	{"codec_type": "audio", "codec_name": "aac"}],
	"format": {"duration": "3.021000"}}`

// fakeTools writes fake ffmpeg and ffprobe programs into a new directory.
// The fake ffmpeg answers -version, records its arguments in argsFile, runs
// block (shell code, may be empty), prints progress and writes a small
// output file. The fake ffprobe prints clipJSON for a path ending in
// clip.mp4 and sourceProbe for any other.
type fakeTools struct {
	ffmpeg, ffprobe, argsFile string
}

func newFakeTools(t *testing.T, block, clipJSON string) fakeTools {
	t.Helper()
	dir := t.TempDir()
	ft := fakeTools{
		ffmpeg:   filepath.Join(dir, "ffmpeg"),
		ffprobe:  filepath.Join(dir, "ffprobe"),
		argsFile: filepath.Join(dir, "args"),
	}
	writeFile(t, filepath.Join(dir, "source.json"), sourceProbe)
	writeFile(t, filepath.Join(dir, "clip.json"), clipJSON)
	ffmpeg := `#!/bin/sh
for a in "$@"; do last="$a"; done
if [ "$2" = "-version" ]; then
	echo "ffmpeg version 9.9-fake Copyright (c) the fakers"
	exit 0
fi
printf '%s\n' "$@" > '` + ft.argsFile + `'
` + block + `
printf 'out_time_us=1500000\nprogress=continue\nout_time_us=3000000\nprogress=end\n'
printf '` + fakeClipBytes + `' > "$last"
`
	ffprobe := `#!/bin/sh
for a in "$@"; do last="$a"; done
case "$last" in
*clip.mp4) cat '` + filepath.Join(dir, "clip.json") + `' ;;
*) cat '` + filepath.Join(dir, "source.json") + `' ;;
esac
`
	for path, script := range map[string]string{ft.ffmpeg: ffmpeg, ft.ffprobe: ffprobe} {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return ft
}

// args is the command line the fake ffmpeg's last encode got.
func (ft fakeTools) args(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(ft.argsFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeDownloader writes a placeholder source file and reports the
// requested length, or fails with err.
type fakeDownloader struct{ err error }

func (f fakeDownloader) Download(ctx context.Context, req download.Request, workDir string) (download.Result, error) {
	if f.err != nil {
		return download.Result{}, f.err
	}
	path := filepath.Join(workDir, download.LocalOutputBase+".mp4")
	if err := os.WriteFile(path, []byte("source"), 0o644); err != nil {
		return download.Result{}, err
	}
	return download.Result{Path: path, Duration: req.End - req.Start}, nil
}

// fakeRenderer is a Renderer with the fake tools and downloader.
func fakeRenderer(t *testing.T, ft fakeTools, opts Options) *Renderer {
	t.Helper()
	cfg := testConfig(t)
	cfg.Render.FFmpegPath, cfg.Render.FFprobePath = ft.ffmpeg, ft.ffprobe
	if opts.Downloader == nil {
		opts.Downloader = fakeDownloader{}
	}
	if opts.Transcriber == nil {
		opts.Transcriber = &fakeTranscriber{transcript: fixedTranscript()}
	}
	r, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// awkwardWorkDir is a work directory whose path needs escaping in a filter
// graph: a space, a colon and a quote.
func awkwardWorkDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work dir: it's")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

var rangeReq = Request{Input: "stream.mp4", Start: time.Second, End: 4 * time.Second}

func TestRenderSteps(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	tr := &fakeTranscriber{transcript: fixedTranscript()}
	r := fakeRenderer(t, ft, Options{Transcriber: tr})
	workDir := awkwardWorkDir(t)

	res, err := r.Render(context.Background(), rangeReq, workDir)
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(workDir, "source.mp4")
	if res.Path != filepath.Join(workDir, ClipFile) || res.SourcePath != source ||
		res.CaptionPath != filepath.Join(workDir, CaptionFile) || res.TranscriptPath != filepath.Join(workDir, TranscriptFile) {
		t.Errorf("paths: %+v", res)
	}
	if res.Width != 1080 || res.Height != 1920 || res.Duration != 3021*ms || res.Size != int64(len(fakeClipBytes)) {
		t.Errorf("clip: %dx%d, %s, %d bytes", res.Width, res.Height, res.Duration, res.Size)
	}
	if res.SourceWidth != 1920 || res.SourceHeight != 1080 {
		t.Errorf("source size %dx%d", res.SourceWidth, res.SourceHeight)
	}
	if res.Detector != "off" || res.Detection.Face || res.Detection.Reason != layout.ReasonOff || res.Layout.Arrangement != layout.BlurFill {
		t.Errorf("detection and layout: %q %+v %s", res.Detector, res.Detection, res.Layout.Arrangement)
	}
	if res.CaptionLines != 2 { // "hello chat look" and "at this"
		t.Errorf("caption lines: %d", res.CaptionLines)
	}
	if res.Tools["ffmpeg"] != "9.9-fake" {
		t.Errorf("tools: %v", res.Tools)
	}
	wantCap, _ := VideoBitrateCap(45_000_000, 3*time.Second)
	if res.VideoBitrateCap != wantCap {
		t.Errorf("bitrate cap %d, want %d", res.VideoBitrateCap, wantCap)
	}
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "download,detect,layout,transcribe,caption,encode,verify" {
		t.Errorf("steps: %s", got)
	}
	if len(tr.inputs) != 1 || tr.inputs[0] != source {
		t.Errorf("transcriber read %q, want the downloaded range", tr.inputs)
	}
	if len(res.Transcript.Words) != 5 {
		t.Errorf("transcript: %+v", res.Transcript)
	}

	// The files left in the work directory.
	wantTranscript, _ := MarshalTranscript(fixedTranscript())
	if got, _ := os.ReadFile(res.TranscriptPath); string(got) != string(wantTranscript) {
		t.Errorf("transcript.json:\n%s", got)
	}
	wantDetection := `{
  "detector": "off",
  "decision": {
    "face": false,
    "reason": "detector_off"
  },
  "layout": {
    "source_width": 1920,
    "source_height": 1080,
    "arrangement": "blur_fill"
  }
}
`
	if got, _ := os.ReadFile(filepath.Join(workDir, DetectionFile)); string(got) != wantDetection {
		t.Errorf("detection.json:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, DebugImageFile)); !os.IsNotExist(err) || res.DebugImage != "" {
		t.Errorf("the off detector has no frame to draw: %v %q", err, res.DebugImage)
	}
	if ass, _ := os.ReadFile(res.CaptionPath); !strings.Contains(string(ass), "HELLO CHAT LOOK") {
		t.Errorf("captions.ass lacks the words:\n%s", ass)
	}

	// The encode got the source, the layout, the escaped caption path and
	// the output.
	args := ft.args(t)
	graph := args[indexOf(args, "-filter_complex")+1]
	if args[indexOf(args, "-i")+1] != source || args[len(args)-1] != res.Path {
		t.Errorf("input or output: %q", args)
	}
	if !strings.HasPrefix(graph, res.Layout.Filter+";[layout]ass=filename="+EscapeFilterPath(res.CaptionPath)+",") {
		t.Errorf("filter graph: %s", graph)
	}
	if !strings.Contains(graph, `work dir\\: it\\\'s`) {
		t.Errorf("caption path not escaped: %s", graph)
	}
	if args[indexOf(args, "-maxrate")+1] != strconv.FormatInt(wantCap, 10) {
		t.Errorf("maxrate: %q", args)
	}
}

func TestRenderStack(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	r := fakeRenderer(t, ft, Options{Detector: fixedDetector{detection: layout.FaceAt(cornerFace, layout.Stack, "steady")}})
	workDir := t.TempDir()
	res, err := r.Render(context.Background(), rangeReq, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Layout.Arrangement != layout.Stack || res.Detector != "fixed" || !res.Detection.Face {
		t.Errorf("got %s from %q %+v", res.Layout.Arrangement, res.Detector, res.Detection)
	}
	if graph := ft.args(t)[indexOf(ft.args(t), "-filter_complex")+1]; !strings.Contains(graph, "vstack") {
		t.Errorf("filter graph is not a stack: %s", graph)
	}
	got, _ := os.ReadFile(filepath.Join(workDir, DetectionFile))
	for _, want := range []string{`"face": true`, `"arrangement": "stack"`, `"x": 0.85`, `"face_frame": {`, `"gameplay": {`, `"detail": "steady"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("detection.json lacks %s:\n%s", want, got)
		}
	}
	if res.DebugImage != "" {
		t.Errorf("a detector without frames gives no picture: %q", res.DebugImage)
	}
}

func TestRenderFaceCrop(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	r := fakeRenderer(t, ft, Options{Detector: fixedDetector{detection: layout.FaceAt(bigFace, layout.FaceCrop, "big")}})
	res, err := r.Render(context.Background(), rangeReq, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.Layout.Arrangement != layout.FaceCrop || res.Layout.FaceFrame.IsZero() || !res.Layout.GameplayCrop.IsZero() {
		t.Errorf("layout %+v", res.Layout)
	}
	if graph := ft.args(t)[indexOf(ft.args(t), "-filter_complex")+1]; !strings.HasPrefix(graph, "[0:v]crop=608:1080:608:0,scale=1080:1920,setsar=1[layout];") {
		t.Errorf("filter graph is not a face crop: %s", graph)
	}
}

func TestRenderDetectorErrorMeansNoFace(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	r := fakeRenderer(t, ft, Options{Detector: fixedDetector{err: errors.New("model crashed")}})
	workDir := t.TempDir()
	res, err := r.Render(context.Background(), rangeReq, workDir)
	if err != nil {
		t.Fatalf("a detector error must not fail the render: %v", err)
	}
	if res.Layout.Arrangement != layout.BlurFill || res.Detection.Face || res.Detection.Reason != layout.ReasonError ||
		res.DetectionError != "model crashed" {
		t.Errorf("got %s, %+v, %q", res.Layout.Arrangement, res.Detection, res.DetectionError)
	}
	if why := res.Why(); why != "detector failed: model crashed" {
		t.Errorf("why: %q", why)
	}
	got, _ := os.ReadFile(filepath.Join(workDir, DetectionFile))
	if !strings.Contains(string(got), `"error": "model crashed"`) || !strings.Contains(string(got), `"reason": "detector_error"`) {
		t.Errorf("detection.json:\n%s", got)
	}
}

// reportDetector answers a report with sampled frames, as the real
// detector does, so the render draws detect.jpg.
type reportDetector struct{ rep detect.Report }

func (reportDetector) Name() string { return "report" }

func (d reportDetector) Detect(context.Context, string, time.Duration, string) (detect.Report, error) {
	return d.rep, nil
}

func TestRenderDrawsDebugImage(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	workDir := t.TempDir()
	frame := filepath.Join(workDir, "detect", "frame-01.jpg")
	rep := detect.Report{
		Detection: layout.FaceAt(cornerFace, layout.Stack, "steady"),
		Frames: []detect.Frame{{Time: 1.5, Image: detect.Image{Path: frame, Width: 1920, Height: 1080,
			Faces: []detect.Face{{X: 1632, Y: 54, W: 96, H: 86.4, Confidence: 0.9, Kept: true, Group: 0}}}}},
		Groups: []detect.Group{{Frames: []int{0}, Box: cornerFace, Steady: true}},
		Chosen: 0, Steady: 1,
	}
	r := fakeRenderer(t, ft, Options{Detector: reportDetector{rep}})
	res, err := r.Render(context.Background(), rangeReq, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.DebugImage != filepath.Join(workDir, DebugImageFile) {
		t.Errorf("debug image %q", res.DebugImage)
	}
	if _, err := os.Stat(res.DebugImage); err != nil {
		t.Error(err)
	}
	var doc struct {
		Frames     []detect.Frame `json:"frames"`
		Chosen     int            `json:"chosen_group"`
		DebugFrame *int           `json:"debug_frame"`
	}
	data, _ := os.ReadFile(filepath.Join(workDir, DetectionFile))
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Frames) != 1 || doc.Frames[0].Faces[0].Confidence != 0.9 ||
		doc.DebugFrame == nil || *doc.DebugFrame != 0 || doc.Chosen != 0 {
		t.Errorf("detection.json (%v):\n%s", err, data)
	}
}

func TestRenderKeepsDownloadClassification(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	for _, tt := range []struct {
		err       error
		permanent bool
	}{
		{download.ErrVODUnavailable, true},
		{download.ErrSubscriberOnly, true},
		{errors.New("connection reset"), false},
	} {
		r := fakeRenderer(t, ft, Options{Downloader: fakeDownloader{err: tt.err}})
		res, err := r.Render(context.Background(), Request{Platform: "twitch", VODID: "1", Start: time.Second, End: 4 * time.Second}, t.TempDir())
		if !errors.Is(err, tt.err) || IsPermanent(err) != tt.permanent {
			t.Errorf("%v: got %v, permanent %v", tt.err, err, IsPermanent(err))
		}
		if len(res.Steps) != 1 || res.Steps[0].Name != StepDownload {
			t.Errorf("steps after a failed download: %+v", res.Steps)
		}
	}
}

func TestRenderTranscribeError(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	boom := errors.New("whisperx crashed")
	r := fakeRenderer(t, ft, Options{Transcriber: &fakeTranscriber{err: boom}})
	if _, err := r.Render(context.Background(), rangeReq, t.TempDir()); !errors.Is(err, boom) || IsPermanent(err) {
		t.Errorf("got %v", err)
	}
}

func TestRenderVerifyFails(t *testing.T) {
	landscape := strings.Replace(strings.Replace(clipProbe, `"width": 1080`, `"width": 1920`, 1), `"height": 1920`, `"height": 1080`, 1)
	ft := newFakeTools(t, "", landscape)
	r := fakeRenderer(t, ft, Options{})
	_, err := r.Render(context.Background(), rangeReq, t.TempDir())
	if !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), "picture 1920x1080") {
		t.Errorf("got %v", err)
	}
}

func TestRenderRefusesBadInput(t *testing.T) {
	ft := newFakeTools(t, "", clipProbe)
	r := fakeRenderer(t, ft, Options{})
	if _, err := r.Render(context.Background(), Request{}, t.TempDir()); !IsPermanent(err) {
		t.Errorf("empty request: %v", err)
	}
	if _, err := os.Stat(ft.argsFile); err == nil {
		t.Error("ffmpeg ran for an invalid request")
	}
	if _, err := r.Render(context.Background(), rangeReq, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing work directory should be an error")
	}
}

// TestRenderCancel cancels the context while ffmpeg encodes. Render must
// return with the context's error, and ffmpeg must be gone.
func TestRenderCancel(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "started")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake ffmpeg reports its pid through the fifo, then becomes a
	// long sleep with the same pid.
	ft := newFakeTools(t, `echo $$ > '`+fifo+`'
exec sleep 600`, clipProbe)
	r := fakeRenderer(t, ft, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Render(ctx, rangeReq, t.TempDir())
		done <- outcome{res, err}
	}()
	started := make(chan string, 1)
	go func() {
		data, _ := os.ReadFile(fifo) // blocks until ffmpeg writes
		started <- strings.TrimSpace(string(data))
	}()

	var pid int
	select {
	case s := <-started:
		var err error
		if pid, err = strconv.Atoi(s); err != nil {
			t.Fatalf("pid %q: %v", s, err)
		}
	case o := <-done:
		t.Fatalf("Render returned before the encode started: %v", o.err)
	}
	cancel()
	o := <-done
	if !errors.Is(o.err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", o.err)
	}
	if IsPermanent(o.err) {
		t.Error("a cancellation is not permanent")
	}
	if n := len(o.res.Steps); n == 0 || o.res.Steps[n-1].Name != StepEncode {
		t.Errorf("steps: %+v", o.res.Steps)
	}
	// runner has waited for ffmpeg, so its pid no longer exists.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("ffmpeg (pid %d) is still there: kill -0 gave %v", pid, err)
	}
}
