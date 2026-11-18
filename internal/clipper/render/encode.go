package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Audio of every clip: AAC at this bitrate, 48 kHz stereo.
const (
	AudioBitrate    = 128_000 // bits per second
	audioSampleRate = 48000
)

// Loudness normalisation: ffmpeg's loudnorm filter in a single pass, to the
// level short-video platforms normalise to.
const (
	loudnessTarget = -14  // integrated loudness, LUFS
	loudnessPeak   = -1.5 // true peak, dBTP
	loudnessRange  = 11   // loudness range, LU
)

// sizeMargin is the share of the size limit the bitrate cap plans for.
const sizeMargin = 0.95

// MinVideoBitrate is the lowest video bitrate cap accepted, in bits per
// second. A clip so long that the size limit leaves less fails with
// ErrTooLong instead of producing an unwatchable picture.
const MinVideoBitrate = 250_000

// DurationTolerance is how far the clip's duration may be from the length of
// the range. The frame rate conversion and the AAC frame size can each move
// the end by a frame.
const DurationTolerance = 500 * time.Millisecond

// Labels of the final filter graph's outputs, without brackets. Package
// layout's labels all start with "layout_", so these cannot clash with them.
const (
	videoLabel = "render_v"
	audioLabel = "render_a"
)

// VideoBitrateCap is the video bitrate limit, in bits per second, that keeps
// a clip of the given length under maxBytes.
//
// The plan is sizeMargin (95 %) of the limit, which leaves room for the MP4
// container and the AAC encoder's own variation. From that come the audio
// (AudioBitrate for the whole length) and one extra second of video: the
// encoder's buffer is one second long (-bufsize equals -maxrate), so the
// video may run up to one buffer above its average rate. The result is
// rounded down to a whole kilobit.
//
// A length of zero or less is an error, and a cap below MinVideoBitrate is
// ErrTooLong.
func VideoBitrateCap(maxBytes int64, length time.Duration) (int64, error) {
	sec := length.Seconds()
	if sec <= 0 {
		return 0, fmt.Errorf("render: clip length %s is not positive", length)
	}
	budget := float64(maxBytes) * 8 * sizeMargin
	video := (budget - AudioBitrate*sec) / (sec + 1)
	capped := int64(math.Floor(video/1000)) * 1000
	if capped < MinVideoBitrate {
		return 0, fmt.Errorf("render: a %s clip under %d bytes leaves a video bitrate of %d bit/s, below %d: %w",
			length, maxBytes, max(capped, 0), MinVideoBitrate, ErrTooLong)
	}
	return capped, nil
}

// EscapeFilterPath escapes a file name for use as an option value inside an
// ffmpeg filter graph given with -filter_complex.
//
// ffmpeg reads such a value through two levels of escaping (see "Notes on
// filtergraph escaping" in the ffmpeg-filters manual). First the filter
// graph is split into filters and their arguments, where \ ' [ ] , ; are
// special; then each filter's arguments are split into options, where
// \ ' : are special. So the name is escaped for the option level first and
// for the graph level second, each by putting a backslash before every
// special character. Spaces need no escaping inside a name.
func EscapeFilterPath(path string) string {
	return backslashEscape(backslashEscape(path, `\':`), `\'[],;`)
}

func backslashEscape(s, special string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// encodeSpec is everything the final encode's command line depends on.
type encodeSpec struct {
	Input, Output string
	// LayoutFilter is layout.Result.Filter: [0:v] to [layout].
	LayoutFilter string
	CaptionPath  string
	Preset       string
	CRF          int
	FrameRate    int
	// MaxRate is the video bitrate cap in bits per second.
	MaxRate int64
}

// filterGraph is the final encode's -filter_complex: the layout fragment,
// then the captions burned in with the ass filter on the layout's output,
// the constant frame rate and the pixel format; and the audio's loudness
// normalised, resampled back to 48 kHz (loudnorm works at 192 kHz).
func filterGraph(s encodeSpec) string {
	return s.LayoutFilter + ";" +
		"[" + layout.OutputLabel + "]ass=filename=" + EscapeFilterPath(s.CaptionPath) +
		",fps=" + strconv.Itoa(s.FrameRate) + ",format=yuv420p[" + videoLabel + "];" +
		fmt.Sprintf("[0:a:0]loudnorm=I=%d:TP=%g:LRA=%d,aresample=%d[%s]",
			loudnessTarget, loudnessPeak, loudnessRange, audioSampleRate, audioLabel)
}

// encodeArgs is the final encode's ffmpeg command line: one pass that lays
// the picture out, burns in the captions and writes an H.264/AAC MP4 with
// the fast-start flag, at a constant frame rate, with the video bitrate
// capped (CRF quality up to MaxRate, with a one-second buffer).
//
// Progress goes to stdout as key=value lines (-progress pipe:1).
func encodeArgs(s encodeSpec) []string {
	rate := strconv.FormatInt(s.MaxRate, 10)
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-progress", "pipe:1", "-y",
		"-i", s.Input,
		"-filter_complex", filterGraph(s),
		"-map", "[" + videoLabel + "]", "-map", "[" + audioLabel + "]",
		"-c:v", "libx264", "-preset", s.Preset, "-crf", strconv.Itoa(s.CRF),
		"-maxrate", rate, "-bufsize", rate,
		"-profile:v", "high", "-pix_fmt", "yuv420p",
		"-fps_mode", "cfr",
		"-c:a", "aac", "-b:a", strconv.Itoa(AudioBitrate), "-ar", strconv.Itoa(audioSampleRate), "-ac", "2",
		"-movflags", "+faststart",
		s.Output,
	}
}

// tools runs ffmpeg and ffprobe for the render.
type tools struct {
	ffmpeg, ffprobe  string
	base, perSecond  time.Duration
	versionMu        sync.Mutex
	versionOfFFmpeg  string
	versionCollected bool
}

func newTools(cfg config.ClipperRender) *tools {
	return &tools{ffmpeg: cfg.FFmpegPath, ffprobe: cfg.FFprobePath, base: cfg.TimeoutBase, perSecond: cfg.TimeoutPerSecond}
}

// encodeTimeout is the final encode's timeout for a clip of the given
// length: proportional to the length, plus a fixed part.
func (t *tools) encodeTimeout(length time.Duration) time.Duration {
	return t.base + time.Duration(max(length, 0).Seconds()*float64(t.perSecond))
}

// ffmpegVersion runs ffmpeg -version once per Renderer and returns the
// version it names, for example "6.1.1-3ubuntu5".
func (t *tools) ffmpegVersion(ctx context.Context) (string, error) {
	t.versionMu.Lock()
	defer t.versionMu.Unlock()
	if t.versionCollected {
		return t.versionOfFFmpeg, nil
	}
	res, err := runner.Run(ctx, runner.Command{Name: t.ffmpeg, Args: []string{"-hide_banner", "-version"}, Timeout: t.base})
	if err != nil {
		return "", fmt.Errorf("render: ffmpeg -version: %w", err)
	}
	t.versionOfFFmpeg = parseVersion(res.Stdout)
	t.versionCollected = true
	return t.versionOfFFmpeg, nil
}

// parseVersion reads the version from the first line of ffmpeg -version,
// "ffmpeg version <version> Copyright ...". It returns the whole line when
// it has another shape.
func parseVersion(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	f := strings.Fields(line)
	if len(f) >= 3 && f[1] == "version" {
		return f[2]
	}
	return strings.TrimSpace(line)
}

// encode runs the final encode and logs its progress every tenth of the
// clip.
func (t *tools) encode(ctx context.Context, args []string, length time.Duration, log *slog.Logger) error {
	p := progress{length: length, log: log}
	_, err := runner.Run(ctx, runner.Command{
		Name:    t.ffmpeg,
		Args:    args,
		Timeout: t.encodeTimeout(length),
		OnLine: func(s runner.Stream, line string) {
			if s == runner.Stdout {
				p.line(line)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("render: encode with ffmpeg: %w", err)
	}
	return nil
}

// progress turns ffmpeg's -progress output into log lines, one per tenth of
// the clip encoded.
type progress struct {
	length time.Duration
	log    *slog.Logger
	logged int // tenths already logged
}

func (p *progress) line(line string) {
	v, ok := strings.CutPrefix(line, "out_time_us=")
	if !ok || p.length <= 0 {
		return
	}
	us, err := strconv.ParseInt(v, 10, 64)
	if err != nil || us < 0 {
		return
	}
	done := time.Duration(us) * time.Microsecond
	tenth := min(int(10*done/p.length), 10)
	if tenth > p.logged {
		p.logged = tenth
		p.log.Info("render: encoding", "done", done.Round(100*time.Millisecond), "of", p.length, "percent", tenth*10)
	}
}

// probeArgs is the ffprobe command line that reports the streams, the
// duration and the size of path as JSON.
func probeArgs(path string) []string {
	return []string{
		"-v", "error",
		"-show_entries", "format=duration,size:stream=codec_type,codec_name,width,height,pix_fmt",
		"-of", "json",
		path,
	}
}

type probeOutput struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		PixFmt    string `json:"pix_fmt"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// media is what ffprobe reports about a file: its first video and audio
// streams and its duration.
type media struct {
	Duration      time.Duration
	HasVideo      bool
	VideoCodec    string
	Width, Height int
	PixFmt        string
	HasAudio      bool
	AudioCodec    string
}

func parseProbe(out []byte) (media, error) {
	var po probeOutput
	if err := json.Unmarshal(out, &po); err != nil {
		return media{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	var m media
	for _, s := range po.Streams {
		switch {
		case s.CodecType == "video" && !m.HasVideo:
			m.HasVideo, m.VideoCodec, m.Width, m.Height, m.PixFmt = true, s.CodecName, s.Width, s.Height, s.PixFmt
		case s.CodecType == "audio" && !m.HasAudio:
			m.HasAudio, m.AudioCodec = true, s.CodecName
		}
	}
	if po.Format.Duration != "" && po.Format.Duration != "N/A" {
		sec, err := strconv.ParseFloat(po.Format.Duration, 64)
		if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 {
			return media{}, fmt.Errorf("parse ffprobe output: duration %q is not a number of seconds", po.Format.Duration)
		}
		m.Duration = time.Duration(sec * float64(time.Second))
	}
	return m, nil
}

func (t *tools) probe(ctx context.Context, path string) (media, error) {
	res, err := runner.Run(ctx, runner.Command{Name: t.ffprobe, Args: probeArgs(path), Timeout: t.base})
	if err != nil {
		return media{}, fmt.Errorf("render: ffprobe %s: %w", path, err)
	}
	m, err := parseProbe(res.Stdout)
	if err != nil {
		return media{}, fmt.Errorf("render: ffprobe %s: %w", path, err)
	}
	return m, nil
}

// pictureSize is the width and height of path's first video stream.
func (t *tools) pictureSize(ctx context.Context, path string) (int, int, error) {
	m, err := t.probe(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	if !m.HasVideo || m.Width <= 0 || m.Height <= 0 {
		return 0, 0, fmt.Errorf("render: %s has no video picture size: %w", path, ErrVerification)
	}
	return m.Width, m.Height, nil
}

// ErrVerification is the cause of a clip, or a source, that failed the
// render's checks. It is retryable: the files may have been damaged by a
// crash or a full disk.
var ErrVerification = errors.New("failed verification")

// verified is a clip that passed the checks.
type verified struct {
	Duration      time.Duration
	Width, Height int
	Size          int64
}

// checkClip checks what ffprobe and the file system report about a clip:
// H.264 video at OutWidth x OutHeight in yuv420p, AAC audio, a duration
// within DurationTolerance of want, and a size of at most maxBytes.
func checkClip(m media, size int64, want time.Duration, maxBytes int64) error {
	var problems []string
	switch {
	case !m.HasVideo:
		problems = append(problems, "no video stream")
	default:
		if m.VideoCodec != "h264" {
			problems = append(problems, fmt.Sprintf("video codec %q, want h264", m.VideoCodec))
		}
		if m.Width != layout.OutWidth || m.Height != layout.OutHeight {
			problems = append(problems, fmt.Sprintf("picture %dx%d, want %dx%d", m.Width, m.Height, layout.OutWidth, layout.OutHeight))
		}
		if m.PixFmt != "yuv420p" {
			problems = append(problems, fmt.Sprintf("pixel format %q, want yuv420p", m.PixFmt))
		}
	}
	switch {
	case !m.HasAudio:
		problems = append(problems, "no audio stream")
	case m.AudioCodec != "aac":
		problems = append(problems, fmt.Sprintf("audio codec %q, want aac", m.AudioCodec))
	}
	if d := m.Duration - want; d < -DurationTolerance || d > DurationTolerance {
		problems = append(problems, fmt.Sprintf("duration %s, want %s within %s", m.Duration, want, DurationTolerance))
	}
	if size > maxBytes {
		problems = append(problems, fmt.Sprintf("size %d bytes, above the limit of %d", size, maxBytes))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s: %w", strings.Join(problems, "; "), ErrVerification)
	}
	return nil
}

// verify checks the finished clip at path.
func (t *tools) verify(ctx context.Context, path string, want time.Duration, maxBytes int64) (verified, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return verified{}, fmt.Errorf("render: clip: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return verified{}, fmt.Errorf("render: clip %s is empty or not a file: %w", path, ErrVerification)
	}
	m, err := t.probe(ctx, path)
	if err != nil {
		return verified{}, err
	}
	if err := checkClip(m, fi.Size(), want, maxBytes); err != nil {
		return verified{}, fmt.Errorf("render: clip %s: %w", path, err)
	}
	return verified{Duration: m.Duration, Width: m.Width, Height: m.Height, Size: fi.Size()}, nil
}
