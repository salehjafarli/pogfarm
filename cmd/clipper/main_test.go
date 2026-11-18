package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/clipper/render"
	"github.com/salehjafarli/pogfarm/internal/clipper/transcribe"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// runCmd runs the command line and returns its exit code and output.
func runCmd(t *testing.T, tr transcribe.Transcriber, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, env{stdout: &stdout, stderr: &stderr, transcriber: tr})
	return code, stdout.String(), stderr.String()
}

func TestRunDispatch(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		code, out, _ := runCmd(t, nil, arg)
		if code != exitOK || out != "clipper dev\n" {
			t.Errorf("%s: code %d, output %q", arg, code, out)
		}
	}
	if code, _, errOut := runCmd(t, nil); code != exitUsage || !strings.Contains(errOut, "usage:") {
		t.Errorf("no arguments: code %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runCmd(t, nil, "frobnicate"); code != exitUsage || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: code %d, stderr %q", code, errOut)
	}
	if code, out, _ := runCmd(t, nil, "help"); code != exitOK || !strings.Contains(out, "clipper render") {
		t.Errorf("help: code %d, output %q", code, out)
	}
	if code, _, errOut := runCmd(t, nil, "render", "--bogus"); code != exitUsage || !strings.Contains(errOut, "flag provided but not defined") {
		t.Errorf("bad flag: code %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runCmd(t, nil, "render", "--input", "a.mp4", "extra"); code != exitUsage || !strings.Contains(errOut, `unexpected argument "extra"`) {
		t.Errorf("extra argument: code %d, stderr %q", code, errOut)
	}
}

func TestWorkIsNotBuilt(t *testing.T) {
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv("CLIPPER_POGFARM_URL", "")
	t.Setenv("CLIPPER_API_SECRET", "")
	code, _, errOut := runCmd(t, nil, "work")
	if code != exitFail || !strings.Contains(errOut, "not built yet") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
	// Unlike clipper render, clipper work needs pogfarm's address and secret.
	if !strings.Contains(errOut, "clipper.pogfarm_url") || !strings.Contains(errOut, "clipper.api_secret") {
		t.Errorf("work should check the pogfarm settings: %q", errOut)
	}

	t.Setenv("CLIPPER_POGFARM_URL", "http://pogfarm:8080")
	t.Setenv("CLIPPER_API_SECRET", "correct-horse-battery-staple")
	t.Setenv("CLIPPER_NAME", "node-a")
	code, _, errOut = runCmd(t, nil, "work")
	if code != exitFail || strings.TrimSpace(errOut) != "clipper work: "+errWorkNotBuilt.Error() {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestParseRenderFlags(t *testing.T) {
	f, err := parseRenderFlags([]string{"--vod", "123", "--start", "1h", "--end", "1h00m30s", "-o", "x.mp4", "--keep-work", "--config", "c.yaml"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := renderFlags{vod: "123", platform: "twitch", start: "1h", end: "1h00m30s", output: "x.mp4", keepWork: true, configPath: "c.yaml"}
	if f != want {
		t.Errorf("got %+v, want %+v", f, want)
	}
}

func TestParseTime(t *testing.T) {
	const s = time.Second
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true},
		{"0", 0, true},
		{"45", 45 * s, true},
		{"3730.5", 3730*s + 500*time.Millisecond, true},
		{"1h02m10s", 3730 * s, true},
		{"62m10s", 3730 * s, true},
		{"1h", time.Hour, true},
		{"500ms", 500 * time.Millisecond, true},
		{"1:02:10", 3730 * s, true},
		{"62:10", 3730 * s, true},
		{"1:02:10.25", 3730*s + 250*time.Millisecond, true},
		{"0:05", 5 * s, true},
		{"-5", 0, false},
		{"-1m", 0, false},
		{"1:60", 0, false},
		{"1:2:3:4", 0, false},
		{"1.5:00", 0, false},
		{"soon", 0, false},
		{"1h02", 0, false},
	}
	for _, tt := range tests {
		got, err := parseTime("--start", tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("parseTime(%q) = %s, %v; want %s, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func TestRenderRequest(t *testing.T) {
	tests := []struct {
		name string
		f    renderFlags
		want render.Request
		err  string
	}{
		{"vod", renderFlags{vod: "42", platform: "twitch", start: "1h02m10s", end: "1h02m55s"},
			render.Request{Platform: "twitch", VODID: "42", Start: 3730 * time.Second, End: 3775 * time.Second}, ""},
		{"input whole", renderFlags{input: "a.mp4", platform: "twitch"}, render.Request{Input: "a.mp4"}, ""},
		{"input range", renderFlags{input: "a.mp4", platform: "twitch", start: "1", end: "4"},
			render.Request{Input: "a.mp4", Start: time.Second, End: 4 * time.Second}, ""},
		{"neither", renderFlags{platform: "twitch", start: "1", end: "4"}, render.Request{}, "name a source"},
		{"both", renderFlags{vod: "42", input: "a.mp4", platform: "twitch", start: "1", end: "4"}, render.Request{}, "not both"},
		{"end before start", renderFlags{input: "a.mp4", start: "10", end: "5"}, render.Request{}, "end 5s is not after start 10s"},
		{"end zero", renderFlags{input: "a.mp4", end: "0"}, render.Request{}, "--end must be after"},
		{"start only", renderFlags{input: "a.mp4", start: "10"}, render.Request{}, "--start needs --end"},
		{"vod without range", renderFlags{vod: "42", platform: "twitch"}, render.Request{}, "a VOD needs a range"},
		{"bad time", renderFlags{input: "a.mp4", start: "later", end: "5"}, render.Request{}, `--start "later" is not a time`},
	}
	for _, tt := range tests {
		got, err := renderRequest(tt.f)
		if tt.err == "" {
			if err != nil || got != tt.want {
				t.Errorf("%s: got %+v, %v; want %+v", tt.name, got, err, tt.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s: got %v, want an error containing %q", tt.name, err, tt.err)
		}
	}
}

func TestDefaultOutputName(t *testing.T) {
	const s = time.Second
	tests := []struct {
		req  render.Request
		want string
	}{
		{render.Request{Platform: "twitch", VODID: "2412345678", Start: 3730 * s, End: 3775 * s}, "twitch-2412345678_1h02m10s-1h02m55s.mp4"},
		{render.Request{Input: "/videos/stream.mp4"}, "stream_full.mp4"},
		{render.Request{Input: "stream.mkv", Start: 0, End: 45 * s}, "stream_0s-45s.mp4"},
		{render.Request{Input: "my stream (final).mp4", Start: 125 * s, End: 130500 * time.Millisecond}, "my_stream__final__2m05s-2m10.5s.mp4"},
		{render.Request{Input: "../.mp4"}, "clip_full.mp4"},
	}
	for _, tt := range tests {
		if got := defaultOutputName(tt.req); got != tt.want {
			t.Errorf("defaultOutputName(%+v) = %q, want %q", tt.req, got, tt.want)
		}
	}
}

func TestTranscriptPath(t *testing.T) {
	for in, want := range map[string]string{
		"/out/clip.mp4":  "/out/clip.json",
		"/out/clip":      "/out/clip.json",
		"/out/a.b.c.mp4": "/out/a.b.c.json",
	} {
		if got := transcriptPath(in); got != want {
			t.Errorf("transcriptPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSummary(t *testing.T) {
	res := render.Result{
		Duration: 3 * time.Second, Width: 1080, Height: 1920, Size: 2_500_000,
		Detector: "off", Layout: layout.Result{Arrangement: layout.BlurFill}, CaptionLines: 2,
		Steps: []render.StepTime{{Name: "download", Took: time.Second}, {Name: "encode", Took: 2 * time.Second}},
		Tools: map[string]string{"ffmpeg": "6.1"},
	}
	want := "" +
		"arrangement   blur_fill\n" +
		"webcam        none (detector off)\n" +
		"caption lines 2\n" +
		"clip          3s, 1080x1920, 2.5 MB\n" +
		"steps         download 1s, encode 2s (total 3s)\n" +
		"ffmpeg        6.1\n" +
		"output        /out/clip.mp4 (2.5 MB)\n" +
		"transcript    /out/clip.json\n"
	if got := formatSummary(res, "/out/clip.mp4", "/out/clip.json"); got != want {
		t.Errorf("summary:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("clip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "clip" {
		t.Errorf("dst holds %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src still there: %v", err)
	}
}

func TestRenderRefusesBeforeWork(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "stream.mp4")
	if code, _, errOut := runCmd(t, nil, "render", "--input", in, "-o", in); code != exitFail || !strings.Contains(errOut, "would replace the input") {
		t.Errorf("output over input: code %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runCmd(t, nil, "render", "--input", in, "-o", filepath.Join(dir, "nope", "x.mp4")); code != exitFail || !strings.Contains(errOut, "does not exist") {
		t.Errorf("missing output directory: code %d, stderr %q", code, errOut)
	}
}

type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(context.Context, string, string) (transcribe.Transcript, error) {
	ms := time.Millisecond
	return transcribe.Transcript{Language: "en", Words: []transcribe.Word{
		{Text: "no", Start: 100 * ms, End: 400 * ms},
		{Text: "way", Start: 450 * ms, End: 900 * ms},
		{Text: "chat", Start: 1600 * ms, End: 2000 * ms},
	}}, nil
}

// TestRenderCommand runs clipper render --input on a generated file with
// the real ffmpeg and a fake transcriber, with and without --keep-work. The
// config file has no pogfarm settings at all.
func TestRenderCommand(t *testing.T) {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not installed", name)
		}
	}
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv("CLIPPER_POGFARM_URL", "")
	t.Setenv("CLIPPER_API_SECRET", "")
	// Every temporary directory of the test is made before TMPDIR points
	// at tmp, so tmp holds only the command's work directories.
	dir, cwd, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", tmp)

	input := filepath.Join(dir, "stream.mp4")
	if _, err := runner.Run(context.Background(), runner.Command{
		Name: "ffmpeg",
		Args: []string{
			"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=duration=6:size=1920x1080:rate=25",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=6",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", input,
		},
		Timeout: 5 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("clipper:\n  render:\n    preset: ultrafast\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("output and keep-work", func(t *testing.T) {
		out := filepath.Join(dir, "my clip.mp4")
		code, stdout, stderr := runCmd(t, fakeTranscriber{}, "render", "--input", input, "--start", "1", "--end", "0:04",
			"-o", out, "--keep-work", "--config", cfgFile)
		if code != exitOK {
			t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		for _, want := range []string{"arrangement   blur_fill", "webcam        none (detector off)", "caption lines 2",
			"output        " + out + " (", "transcript    " + filepath.Join(dir, "my clip.json"), "work dir      "} {
			if !strings.Contains(stdout, want) {
				t.Errorf("summary lacks %q:\n%s", want, stdout)
			}
		}
		if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
			t.Errorf("clip: %v", err)
		}
		var tr struct {
			Language string `json:"language"`
			Words    []struct {
				Text string `json:"text"`
			} `json:"words"`
		}
		data, err := os.ReadFile(filepath.Join(dir, "my clip.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &tr); err != nil || tr.Language != "en" || len(tr.Words) != 3 {
			t.Errorf("transcript %s: %v", data, err)
		}
		// The kept work directory holds the intermediate files.
		_, workDir, _ := strings.Cut(stdout, "work dir      ")
		workDir = strings.TrimSpace(workDir)
		if !strings.HasPrefix(workDir, tmp) {
			t.Fatalf("work dir %q is not under TMPDIR", workDir)
		}
		for _, f := range []string{"source.mp4", render.CaptionFile, render.DetectionFile} {
			if _, err := os.Stat(filepath.Join(workDir, f)); err != nil {
				t.Errorf("kept work dir: %v", err)
			}
		}
		os.RemoveAll(workDir)
	})

	t.Run("default name and cleanup", func(t *testing.T) {
		t.Chdir(cwd)
		code, stdout, stderr := runCmd(t, fakeTranscriber{}, "render", "--input", input, "--start", "1s", "--end", "4s", "--config", cfgFile)
		if code != exitOK {
			t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		for _, f := range []string{"stream_1s-4s.mp4", "stream_1s-4s.json"} {
			if _, err := os.Stat(filepath.Join(cwd, f)); err != nil {
				t.Errorf("output: %v", err)
			}
		}
		if strings.Contains(stdout, "work dir") {
			t.Errorf("work dir printed without --keep-work:\n%s", stdout)
		}
		left, err := os.ReadDir(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("work directory not deleted: %v", left)
		}
	})
}
