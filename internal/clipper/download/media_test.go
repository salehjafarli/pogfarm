package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Tests in this file run the real ffmpeg and ffprobe and skip without them.
// CI installs both.

// mediaConfig returns a configuration with the absolute paths of ffmpeg and
// ffprobe, or skips the test when either is missing.
func mediaConfig(t *testing.T) config.ClipperDownload {
	t.Helper()
	cfg := testConfig()
	for _, p := range []*string{&cfg.FFmpegPath, &cfg.FFprobePath} {
		path, err := exec.LookPath(*p)
		if err != nil {
			t.Skipf("%s not installed", *p)
		}
		*p = path
	}
	return cfg
}

// ffmpeg runs ffmpeg with args and fails the test on error.
func ffmpeg(t *testing.T, cfg config.ClipperDownload, args ...string) []byte {
	t.Helper()
	res, err := runner.Run(context.Background(), runner.Command{
		Name:    cfg.FFmpegPath,
		Args:    append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...),
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("ffmpeg %s: %v", strings.Join(args, " "), err)
	}
	return res.Stdout
}

// testsrcFile writes a file of the given length with a testsrc picture and a
// sine tone.
func testsrcFile(t *testing.T, cfg config.ClipperDownload, path string, seconds int) {
	t.Helper()
	ffmpeg(t, cfg,
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=duration=%d:size=320x240:rate=25", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=1000:sample_rate=48000:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
		path)
}

// band is one solid-colour part of a generated video.
type band struct {
	color   string
	seconds float64
}

// bandsArgs is the ffmpeg arguments that write path as a video made of
// bands, with a tone under it. Keyframes are 10 seconds apart, so a cut by
// stream copy would land far from a band edge.
func bandsArgs(path string, bands ...band) []string {
	var args []string
	var total float64
	var concat string
	for i, b := range bands {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=%s:s=160x120:r=25:d=%g", b.color, b.seconds))
		total += b.seconds
		concat += fmt.Sprintf("[%d:v]", i)
	}
	n := len(bands)
	args = append(args,
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%g", total),
		"-filter_complex", fmt.Sprintf("%sconcat=n=%d:v=1:a=0[v]", concat, n),
		"-map", "[v]", "-map", fmt.Sprintf("%d:a", n),
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "250", "-pix_fmt", "yuv420p", "-c:a", "aac",
		path)
	return args
}

// frameLumas returns the mean brightness of every video frame of path, one
// byte per frame.
func frameLumas(t *testing.T, cfg config.ClipperDownload, path string) []byte {
	t.Helper()
	return ffmpeg(t, cfg, "-i", path, "-an", "-vf", "scale=1:1,format=gray", "-f", "rawvideo", "-")
}

// assertAllWhite fails unless every frame of path is white and there are
// frames frames.
func assertAllWhite(t *testing.T, cfg config.ClipperDownload, path string, frames int) {
	t.Helper()
	lumas := frameLumas(t, cfg, path)
	if len(lumas) < frames-1 || len(lumas) > frames+1 {
		t.Errorf("%d frames, want %d", len(lumas), frames)
	}
	for i, y := range lumas {
		if y < 128 {
			t.Errorf("frame %d of %d is dark (luma %d): the cut is not frame-accurate", i, len(lumas), y)
			break
		}
	}
}

func assertResult(t *testing.T, cfg config.ClipperDownload, res Result, workDir string, want time.Duration) {
	t.Helper()
	if filepath.Dir(res.Path) != workDir {
		t.Errorf("result %s is not in the work directory %s", res.Path, workDir)
	}
	tl := tools{ffmpeg: cfg.FFmpegPath, ffprobe: cfg.FFprobePath, timeout: timeoutFunc(time.Minute, time.Second)}
	p, err := tl.probe(context.Background(), res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasVideo || !p.HasAudio {
		t.Errorf("streams: video %v, audio %v; want both", p.HasVideo, p.HasAudio)
	}
	if d := p.Duration - want; d < -DurationTolerance || d > DurationTolerance {
		t.Errorf("duration %s, want %s within %s", p.Duration, want, DurationTolerance)
	}
	if res.Duration != p.Duration {
		t.Errorf("Result.Duration %s, ffprobe says %s", res.Duration, p.Duration)
	}
}

func TestLocalTrim(t *testing.T) {
	cfg := mediaConfig(t)
	in := filepath.Join(t.TempDir(), "stream.mp4")
	testsrcFile(t, cfg, in, 20)

	work := t.TempDir()
	res, err := New(cfg).Download(context.Background(), Request{
		Input: in, Start: 3500 * time.Millisecond, End: 11200 * time.Millisecond,
	}, work)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res.Path) != "source.mp4" {
		t.Errorf("result %s, want source.mp4", res.Path)
	}
	assertResult(t, cfg, res, work, 7700*time.Millisecond)
}

func TestLocalCopyWhole(t *testing.T) {
	cfg := mediaConfig(t)
	in := filepath.Join(t.TempDir(), "Stream.MKV")
	testsrcFile(t, cfg, in, 20)

	work := t.TempDir()
	res, err := New(cfg).Download(context.Background(), Request{Input: in}, work)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res.Path) != "source.mkv" {
		t.Errorf("result %s, want source.mkv", res.Path)
	}
	want, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("copy differs from the input: %d bytes, want %d", len(got), len(want))
	}
	assertResult(t, cfg, res, work, 20*time.Second)
}

// TestLocalTrimIsFrameAccurate cuts the white band out of black, white,
// black. The input has a keyframe every 10 seconds, so only a re-encoding
// cut gets every frame right.
func TestLocalTrimIsFrameAccurate(t *testing.T) {
	cfg := mediaConfig(t)
	in := filepath.Join(t.TempDir(), "bands.mp4")
	ffmpeg(t, cfg, bandsArgs(in, band{"black", 3.6}, band{"white", 4.4}, band{"black", 4})...)

	work := t.TempDir()
	res, err := New(cfg).Download(context.Background(), Request{
		Input: in, Start: 3600 * time.Millisecond, End: 8 * time.Second,
	}, work)
	if err != nil {
		t.Fatal(err)
	}
	assertResult(t, cfg, res, work, 4400*time.Millisecond)
	assertAllWhite(t, cfg, res.Path, 110)
}

func TestLocalRangePastEnd(t *testing.T) {
	cfg := mediaConfig(t)
	in := filepath.Join(t.TempDir(), "stream.mp4")
	testsrcFile(t, cfg, in, 5)

	_, err := New(cfg).Download(context.Background(), Request{Input: in, Start: 3 * time.Second, End: 9 * time.Second}, t.TempDir())
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

// fakeTwitchDownloader writes a TwitchDownloaderCLI stand-in that records its
// arguments in args.txt next to it and runs body.
func fakeTwitchDownloader(t *testing.T, body string) (program, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	program = filepath.Join(dir, "TwitchDownloaderCLI")
	argsFile = filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" + body
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return program, argsFile
}

// fakeVideoBody is a fake TwitchDownloaderCLI that behaves like the real one
// in Exact trim mode: it writes the requested range preceded by a lead-in of
// 1.2 seconds, as if the start had been cut at an earlier keyframe. The
// lead-in is black and the range is white.
func fakeVideoBody(ffmpegPath string) string {
	return `out=; begin=; end=
while [ $# -gt 0 ]; do
  case "$1" in
    --output) out=$2; shift ;;
    --beginning) begin=$2; shift ;;
    --ending) end=$2; shift ;;
  esac
  shift
done
len=$(awk -v b="${begin%ms}" -v e="${end%ms}" 'BEGIN { printf "%.3f", (e - b) / 1000 }')
total=$(awk -v l="$len" 'BEGIN { printf "%.3f", l + 1.2 }')
exec '` + ffmpegPath + `' -hide_banner -nostdin -loglevel error -y \
  -f lavfi -i color=c=black:s=160x120:r=25:d=1.2 \
  -f lavfi -i "color=c=white:s=160x120:r=25:d=$len" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=$total" \
  -filter_complex '[0:v][1:v]concat=n=2:v=1:a=0[v]' -map '[v]' -map 2:a \
  -c:v libx264 -preset ultrafast -g 250 -pix_fmt yuv420p -c:a aac "$out"
`
}

func TestTwitchWithFakeDownloader(t *testing.T) {
	cfg := mediaConfig(t)
	program, argsFile := fakeTwitchDownloader(t, fakeVideoBody(cfg.FFmpegPath))
	cfg.TwitchDownloaderPath = program
	cfg.Quality = "720p60"

	work := t.TempDir()
	res, err := New(cfg).Download(context.Background(), Request{
		Platform: PlatformTwitch, VODID: "2412345678",
		Start: time.Hour + 10*time.Second, End: time.Hour + 17600*time.Millisecond,
	}, work)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != filepath.Join(work, TwitchOutputName) {
		t.Errorf("result %s, want %s", res.Path, filepath.Join(work, TwitchOutputName))
	}
	assertResult(t, cfg, res, work, 7600*time.Millisecond)
	assertAllWhite(t, cfg, res.Path, 190)

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(twitchArgs("2412345678", filepath.Join(work, TwitchRawName), filepath.Join(work, TwitchTempDir),
		"720p60", cfg.FFmpegPath, time.Hour+10*time.Second, time.Hour+17600*time.Millisecond), "\n") + "\n"
	if string(args) != want {
		t.Errorf("arguments:\n%s\nwant:\n%s", args, want)
	}
	if fi, err := os.Stat(filepath.Join(work, TwitchTempDir)); err != nil || !fi.IsDir() {
		t.Errorf("temp directory not created in the work directory: %v", err)
	}
}

func TestTwitchFakeDownloaderErrors(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      error
		permanent bool
	}{
		{
			name:      "deleted",
			body:      "echo 'Unhandled exception. System.AggregateException: One or more errors occurred. (Invalid VOD, deleted/expired VOD possibly?)' >&2\nexit 134\n",
			want:      ErrVODUnavailable,
			permanent: true,
		},
		{
			name:      "subscriber only",
			body:      "echo '[ERROR] - Insufficient access to VOD, OAuth may be required.'\nexit 1\n",
			want:      ErrSubscriberOnly,
			permanent: true,
		},
		{
			name: "network",
			body: "echo 'System.Net.Http.HttpRequestException: Name or service not known' >&2\nexit 1\n",
		},
		{
			name: "killed",
			body: "kill -9 $$\n",
		},
		{
			name: "no output file",
			body: "exit 0\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			program, _ := fakeTwitchDownloader(t, tt.body)
			cfg := testConfig()
			cfg.TwitchDownloaderPath = program
			// No output file means ffprobe fails, which needs no real
			// ffprobe to be retryable: a missing program fails too.
			cfg.FFprobePath = filepath.Join(t.TempDir(), "no-ffprobe")

			_, err := New(cfg).Download(context.Background(), Request{
				Platform: PlatformTwitch, VODID: "2412345678", Start: 10 * time.Second, End: 20 * time.Second,
			}, t.TempDir())
			if err == nil {
				t.Fatal("want an error")
			}
			if IsPermanent(err) != tt.permanent {
				t.Errorf("permanent = %v, want %v: %v", IsPermanent(err), tt.permanent, err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
			var re *runner.Error
			if tt.want != nil && !errors.As(err, &re) {
				t.Errorf("the tool's error should stay reachable: %v", err)
			}
		})
	}
}

// TestTwitchRealDownload downloads from Twitch. It runs only when
// POGFARM_TEST_TWITCH_VOD names a public VOD; see the package documentation.
func TestTwitchRealDownload(t *testing.T) {
	vod := os.Getenv("POGFARM_TEST_TWITCH_VOD")
	if vod == "" {
		t.Skip("set POGFARM_TEST_TWITCH_VOD to a public VOD id to run against Twitch")
	}
	cfg := mediaConfig(t)
	if _, err := exec.LookPath(cfg.TwitchDownloaderPath); err != nil {
		t.Skip("TwitchDownloaderCLI not installed")
	}
	work := t.TempDir()
	res, err := New(cfg).Download(context.Background(), Request{
		Platform: PlatformTwitch, VODID: vod, Start: time.Minute, End: time.Minute + 20*time.Second,
	}, work)
	if err != nil {
		t.Fatal(err)
	}
	assertResult(t, cfg, res, work, 20*time.Second)
	t.Logf("downloaded %s (%s)", res.Path, res.Duration)
}
