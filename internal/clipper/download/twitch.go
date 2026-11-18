package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Files of a Twitch download inside the work directory.
const (
	// TwitchRawName is TwitchDownloaderCLI's output: the range plus a
	// lead-in up to the keyframe before the start. It is kept for
	// inspection.
	TwitchRawName = "twitch-raw.mp4"
	// TwitchTempDir is the temporary directory given to TwitchDownloaderCLI
	// for its segment cache.
	TwitchTempDir = "twitch-tmp"
	// TwitchOutputName is the exact range.
	TwitchOutputName = "source.mp4"
)

// maxLeadIn bounds the lead-in TwitchDownloaderCLI may leave before the
// requested start. It cuts within the first segment, and Twitch segments are
// about ten seconds long, so anything longer means the end was not where it
// was expected and the result cannot be trusted.
const maxLeadIn = 30 * time.Second

// Twitch is the Downloader for Twitch VODs. It runs TwitchDownloaderCLI
// videodownload for the range, then trims the lead-in off with ffmpeg.
type Twitch struct {
	// Program is the TwitchDownloaderCLI executable.
	Program string
	// Quality is passed as --quality.
	Quality string

	tools tools
}

// twitchArgs is the TwitchDownloaderCLI command line that downloads
// [start, end) of a VOD. Flags as defined by TwitchDownloaderCLI 1.56.5
// (VideoDownloadArgs.cs):
//
//   - --beginning and --ending in milliseconds ("#ms" is one of its time
//     formats);
//   - --trim-mode Exact, its default, spelled out because the lead-in
//     arithmetic in Download depends on it;
//   - --temp-path for its segment cache, inside the work directory;
//   - --collision Overwrite, so a retry in the same directory does not stop
//     at a prompt;
//   - --ffmpeg-path only when ffmpeg is configured as a path: the CLI turns
//     the value into an absolute path, which breaks a bare name, and finds
//     a bare "ffmpeg" in PATH on its own.
func twitchArgs(vodID, out, tempDir, quality, ffmpeg string, start, end time.Duration) []string {
	args := []string{
		"videodownload",
		"--id", vodID,
		"--output", out,
		"--quality", quality,
		"--beginning", strconv.FormatInt(start.Milliseconds(), 10) + "ms",
		"--ending", strconv.FormatInt(end.Milliseconds(), 10) + "ms",
		"--trim-mode", "Exact",
		"--temp-path", tempDir,
		"--collision", "Overwrite",
	}
	if isPath(ffmpeg) {
		args = append(args, "--ffmpeg-path", ffmpeg)
	}
	return args
}

// Download implements Downloader. It reads req.VODID, req.Start and req.End.
// The range is rounded to whole milliseconds, the precision of
// TwitchDownloaderCLI.
func (t *Twitch) Download(ctx context.Context, req Request, workDir string) (Result, error) {
	req.Start = req.Start.Round(time.Millisecond)
	req.End = req.End.Round(time.Millisecond)
	if err := validateRange(req); err != nil {
		return Result{}, err
	}
	if req.VODID == "" || strings.IndexFunc(req.VODID, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return Result{}, fmt.Errorf("download: twitch VOD id %q is not a number: %w", req.VODID, ErrInvalidRequest)
	}
	work, err := filepath.Abs(workDir)
	if err != nil {
		return Result{}, fmt.Errorf("download: work directory: %w", err)
	}
	tempDir := filepath.Join(work, TwitchTempDir)
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("download: %w", err)
	}
	raw := filepath.Join(work, TwitchRawName)
	ffmpeg := t.tools.ffmpeg
	if isPath(ffmpeg) {
		if abs, err := filepath.Abs(ffmpeg); err == nil {
			ffmpeg = abs
		}
	}

	length := req.Length()
	res, err := runner.Run(ctx, runner.Command{
		Name:    t.Program,
		Args:    twitchArgs(req.VODID, raw, tempDir, t.Quality, ffmpeg, req.Start, req.End),
		Timeout: t.tools.timeout(length),
	})
	if err != nil {
		if perm := classifyTwitch(res.Stdout, res.Stderr); perm != nil {
			return Result{}, fmt.Errorf("download: twitch VOD %s: %w (%w)", req.VODID, perm, err)
		}
		return Result{}, fmt.Errorf("download: twitch VOD %s: %w", req.VODID, err)
	}

	p, err := t.tools.probe(ctx, raw)
	if err != nil {
		return Result{}, err
	}
	// The video stream's own end is the most precise: the container's
	// duration also counts the padding of the last audio frame.
	got := p.VideoDuration
	if got <= 0 {
		got = p.Duration
	}
	leadIn, err := twitchLeadIn(got, length)
	if err != nil {
		return Result{}, fmt.Errorf("download: twitch VOD %s: %w", req.VODID, err)
	}
	out := filepath.Join(work, TwitchOutputName)
	if err := t.tools.trim(ctx, raw, out, leadIn, length); err != nil {
		return Result{}, err
	}
	return t.tools.verify(ctx, out, length)
}

// twitchLeadIn is where the requested start lies in TwitchDownloaderCLI's
// output. Its end is the requested end, so the start is length before it.
func twitchLeadIn(got, length time.Duration) (time.Duration, error) {
	leadIn := got - length
	switch {
	case leadIn < -DurationTolerance:
		// A VOD that is still being recorded can grow, so this may pass.
		return 0, fmt.Errorf("download is %s long, shorter than the %s asked for; the VOD may end before the range: %w",
			got, length, errVerify)
	case leadIn > maxLeadIn:
		return 0, fmt.Errorf("download is %s long for a range of %s, more lead-in than expected: %w",
			got, length, errVerify)
	case leadIn < 0:
		return 0, nil
	}
	return leadIn, nil
}

// twitchFailures maps messages of TwitchDownloaderCLI 1.56.5 to permanent
// errors. They come from TwitchDownloaderCore/VideoDownloader.cs and
// TwitchDownloaderCLI/Modes/DownloadVideo.cs and are matched without regard
// to case.
var twitchFailures = []struct {
	text string
	err  error
}{
	{"deleted/expired vod", ErrVODUnavailable},
	{"insufficient access to vod", ErrSubscriberOnly},
	{"unable to parse vod id", ErrInvalidRequest},
}

// classifyTwitch returns the permanent error a failed run's output shows,
// or nil when the failure is not a known permanent one.
func classifyTwitch(stdout, stderr []byte) error {
	out := strings.ToLower(string(stderr) + "\n" + string(stdout))
	for _, f := range twitchFailures {
		if strings.Contains(out, f.text) {
			return f.err
		}
	}
	return nil
}

// isPath reports whether a configured program is a path rather than a name
// to look up in PATH.
func isPath(program string) bool {
	return strings.ContainsRune(program, '/') || strings.ContainsRune(program, filepath.Separator)
}
