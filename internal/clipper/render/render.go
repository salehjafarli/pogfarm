package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/caption"
	"github.com/salehjafarli/pogfarm/internal/clipper/download"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/clipper/transcribe"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// File names inside the work directory. The downloaded range is named by
// package download (download.LocalOutputBase and its Twitch counterpart);
// the transcriber keeps its own files there too.
const (
	DetectionFile  = "detection.json"
	CaptionFile    = "captions.ass"
	TranscriptFile = "transcript.json"
	ClipFile       = "clip.mp4"
)

// Step names, in the order Render runs them. Result.Steps uses them.
const (
	StepDownload   = "download"
	StepDetect     = "detect"
	StepLayout     = "layout"
	StepTranscribe = "transcribe"
	StepCaption    = "caption"
	StepEncode     = "encode"
	StepVerify     = "verify"
)

// Request names the source and the range of one clip. It is a struct so
// that later options (a layout or caption override, for example) can be
// added without changing Render's signature.
type Request struct {
	// Platform and VODID name a VOD on a stream platform, for example
	// "twitch" and "2412345678". Set either these or Input.
	Platform string
	VODID    string
	// Input is a local video file to use instead of a platform VOD.
	Input string
	// Start and End are the range, measured from the start of the source.
	// A VOD needs both, with End > Start. A local file may leave both zero
	// to use the whole file; otherwise End > Start.
	Start time.Duration
	End   time.Duration
}

// Validate checks the request without touching any file or network. Its
// errors match download.ErrInvalidRequest and so ErrPermanent.
func (r Request) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("render: "+format+": %w", append(args, download.ErrInvalidRequest)...)
	}
	switch {
	case r.Input == "" && r.VODID == "":
		return invalid("name a source: a VOD id or an input file")
	case r.Input != "" && r.VODID != "":
		return invalid("name one source, not both a VOD id (%s) and an input file (%s)", r.VODID, r.Input)
	case r.VODID != "" && r.Platform == "":
		return invalid("VOD %s has no platform", r.VODID)
	case r.Start < 0:
		return invalid("start %s is negative", r.Start)
	case r.End < 0:
		return invalid("end %s is negative", r.End)
	case r.VODID != "" && r.End == 0:
		return invalid("a VOD needs a range: give a start and an end")
	case r.End == 0 && r.Start != 0:
		return invalid("start %s given without an end", r.Start)
	case r.End != 0 && r.End <= r.Start:
		return invalid("end %s is not after start %s", r.End, r.Start)
	}
	return nil
}

// Length is End - Start, or zero for a whole file.
func (r Request) Length() time.Duration {
	if r.End == 0 {
		return 0
	}
	return r.End - r.Start
}

func (r Request) download() download.Request {
	return download.Request{Platform: r.Platform, VODID: r.VODID, Input: r.Input, Start: r.Start, End: r.End}
}

// StepTime is how long one step of a render took.
type StepTime struct {
	Name string
	Took time.Duration
}

// Result is a finished clip and how it was made.
type Result struct {
	// Path is the clip, inside the work directory.
	Path string
	// TranscriptPath is the transcript as JSON (see MarshalTranscript),
	// inside the work directory.
	TranscriptPath string
	// CaptionPath is the ASS subtitle file burned into the clip.
	CaptionPath string
	// SourcePath is the downloaded range, inside the work directory.
	SourcePath string

	// Transcript is the speech of the clip, words with times measured from
	// the start of the clip.
	Transcript transcribe.Transcript

	// Duration, Width, Height and Size describe the clip as ffprobe and the
	// file system report it. Size is in bytes.
	Duration time.Duration
	Width    int
	Height   int
	Size     int64
	// VideoBitrateCap is the video bitrate limit given to the encoder, in
	// bits per second, derived from the size limit and the clip's length.
	VideoBitrateCap int64

	// SourceWidth and SourceHeight are the picture size of the source.
	SourceWidth  int
	SourceHeight int

	// Detector names the webcam detector used, for example "off".
	Detector string
	// Detection is what the detector answered. DetectionError holds the
	// error it gave instead, in which case Detection is "no webcam".
	Detection      layout.Detection
	DetectionError string
	// Layout is the layout applied: its arrangement, its crops and, when a
	// detected webcam could not be used, the reason (Layout.Fallback).
	Layout layout.Result

	// CaptionLines is the number of caption lines (phrases) burned in.
	CaptionLines int

	// Steps is the time each step took, in the order they ran.
	Steps []StepTime
	// Tools holds the version of each tool used, by name ("ffmpeg").
	Tools map[string]string
}

// Total is the sum of the step times.
func (r Result) Total() time.Duration {
	var d time.Duration
	for _, s := range r.Steps {
		d += s.Took
	}
	return d
}

// Downloader gets the range of a request into the work directory.
// *download.Service implements it.
type Downloader interface {
	Download(ctx context.Context, req download.Request, workDir string) (download.Result, error)
}

// Options are the parts of a Renderer that can be replaced. A nil field
// takes the default built from the configuration.
type Options struct {
	// Logger receives progress and decisions. Nil discards them.
	Logger *slog.Logger
	// Downloader defaults to download.New(cfg.Download).
	Downloader Downloader
	// Detector defaults to NewDetector(cfg.WebcamDetection, Logger).
	Detector Detector
	// Transcriber defaults to transcribe.NewWhisperX(cfg.Transcribe,
	// Logger).
	Transcriber transcribe.Transcriber
}

// Renderer turns a Request into a clip. It is safe for concurrent use; each
// render needs its own work directory.
type Renderer struct {
	cfg         config.ClipperRender
	logger      *slog.Logger
	downloader  Downloader
	detector    Detector
	layouter    *layout.Layouter
	transcriber transcribe.Transcriber
	captions    *caption.Writer
	tools       *tools
}

// New builds a Renderer from the clipper configuration. It checks the
// render, layout and caption sections and fails on a problem; it starts
// no tool.
func New(cfg config.Clipper, opts Options) (*Renderer, error) {
	if problems := cfg.Render.Validate(); len(problems) > 0 {
		return nil, fmt.Errorf("render: %s", strings.Join(problems, "; "))
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	lay, err := layout.New(cfg.Layout)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	capw, err := caption.New(cfg.Caption)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	r := &Renderer{
		cfg:         cfg.Render,
		logger:      logger,
		downloader:  opts.Downloader,
		detector:    opts.Detector,
		layouter:    lay,
		transcriber: opts.Transcriber,
		captions:    capw,
		tools:       newTools(cfg.Render),
	}
	if r.downloader == nil {
		r.downloader = download.New(cfg.Download)
	}
	if r.detector == nil {
		r.detector = NewDetector(cfg.WebcamDetection, logger)
	}
	if r.transcriber == nil {
		r.transcriber = transcribe.NewWhisperX(cfg.Transcribe, logger)
	}
	return r, nil
}

// Render turns req into a finished clip inside workDir, which must exist.
// It runs, in order: download, detect, layout, transcribe, caption, encode
// and verify, and returns once the clip is written and checked.
//
// Render never deletes workDir; the caller owns it. Cancelling ctx stops the
// render at once: the tool running is killed, and the error matches
// ctx.Err(). Errors that a retry will not fix match ErrPermanent.
func (r *Renderer) Render(ctx context.Context, req Request, workDir string) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		return Result{}, fmt.Errorf("render: work directory %s does not exist", workDir)
	}
	log := r.logger.With("source", req.source(), "start", req.Start, "end", req.End)
	var res Result
	step := func(name string, fn func() error) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("render: %w", err)
		}
		began := time.Now()
		err := fn()
		took := time.Since(began)
		res.Steps = append(res.Steps, StepTime{Name: name, Took: took})
		if err != nil {
			// A cancellation shows as such, whichever tool it killed.
			if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
				err = fmt.Errorf("%w (%w)", err, ctxErr)
			}
			return err
		}
		log.Debug("render: step done", "step", name, "took", took)
		return nil
	}

	// The ffmpeg version first: a missing ffmpeg fails before the
	// download, not after it.
	ffmpegVersion, err := r.tools.ffmpegVersion(ctx)
	if err != nil {
		return Result{}, err
	}
	res.Tools = map[string]string{"ffmpeg": ffmpegVersion}

	var src download.Result
	if err := step(StepDownload, func() (err error) {
		src, err = r.downloader.Download(ctx, req.download(), workDir)
		return err
	}); err != nil {
		return res, err
	}
	res.SourcePath = src.Path
	length := src.Duration
	if req.End != 0 {
		length = req.Length()
	}
	maxRate, err := VideoBitrateCap(r.cfg.MaxBytes, length)
	if err != nil {
		return res, err
	}
	res.VideoBitrateCap = maxRate

	if err := step(StepDetect, func() error {
		res.Detector = r.detector.Name()
		d, err := r.detector.Detect(ctx, src.Path, workDir)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("render: detect: %w", err)
			}
			// Detection never fails a clip: no webcam, and say why.
			log.Warn("render: webcam detection failed; using no webcam", "detector", res.Detector, "err", err)
			res.DetectionError = err.Error()
			d = layout.NoWebcam()
		}
		res.Detection = d
		return writeJSON(filepath.Join(workDir, DetectionFile), detectionJSON(res.Detector, d, res.DetectionError))
	}); err != nil {
		return res, err
	}

	if err := step(StepLayout, func() error {
		w, h, err := r.tools.pictureSize(ctx, src.Path)
		if err != nil {
			return err
		}
		res.SourceWidth, res.SourceHeight = w, h
		res.Layout, err = r.layouter.Plan(w, h, res.Detection)
		if err != nil {
			return fmt.Errorf("render: %w: %w", err, download.ErrInvalidRequest)
		}
		log.Info("render: layout chosen", "arrangement", res.Layout.Arrangement,
			"detector", res.Detector, "webcam", res.Detection.Webcam, "fallback", res.Layout.Fallback)
		return nil
	}); err != nil {
		return res, err
	}

	if err := step(StepTranscribe, func() (err error) {
		res.Transcript, err = r.transcriber.Transcribe(ctx, src.Path, workDir)
		if err != nil {
			return err
		}
		if res.Transcript.Words == nil {
			res.Transcript.Words = []transcribe.Word{}
		}
		res.TranscriptPath = filepath.Join(workDir, TranscriptFile)
		data, err := MarshalTranscript(res.Transcript)
		if err != nil {
			return fmt.Errorf("render: %w", err)
		}
		if err := os.WriteFile(res.TranscriptPath, data, 0o644); err != nil {
			return fmt.Errorf("render: %w", err)
		}
		return nil
	}); err != nil {
		return res, err
	}

	if err := step(StepCaption, func() error {
		res.CaptionPath = filepath.Join(workDir, CaptionFile)
		c, err := r.captions.Write(ctx, res.Transcript, res.CaptionPath)
		if err != nil {
			return err
		}
		res.CaptionLines = c.Lines
		return nil
	}); err != nil {
		return res, err
	}

	out := filepath.Join(workDir, ClipFile)
	if err := step(StepEncode, func() error {
		// Never take an earlier run's file for this one's.
		if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("render: %w", err)
		}
		args := encodeArgs(encodeSpec{
			Input:        src.Path,
			Output:       out,
			LayoutFilter: res.Layout.Filter,
			CaptionPath:  res.CaptionPath,
			Preset:       r.cfg.Preset,
			CRF:          r.cfg.CRF,
			FrameRate:    r.cfg.FrameRate,
			MaxRate:      maxRate,
		})
		return r.tools.encode(ctx, args, length, log)
	}); err != nil {
		return res, err
	}

	if err := step(StepVerify, func() error {
		v, err := r.tools.verify(ctx, out, length, r.cfg.MaxBytes)
		if err != nil {
			return err
		}
		res.Path, res.Duration, res.Width, res.Height, res.Size = out, v.Duration, v.Width, v.Height, v.Size
		return nil
	}); err != nil {
		return res, err
	}
	log.Info("render: clip done", "path", res.Path, "duration", res.Duration, "bytes", res.Size,
		"arrangement", res.Layout.Arrangement, "caption_lines", res.CaptionLines, "took", res.Total())
	return res, nil
}

// source names the request's source for logs and file names.
func (r Request) source() string {
	if r.Input != "" {
		return r.Input
	}
	return r.Platform + ":" + r.VODID
}

// ErrPermanent matches every render error that a retry will not fix, on this
// node or another. It is download.ErrPermanent, so the download step's
// classification reaches callers unchanged.
var ErrPermanent = download.ErrPermanent

// ErrTooLong is the error for a clip so long that the size limit leaves too
// small a video bitrate for it. It matches ErrPermanent.
var ErrTooLong = permanentError("clip too long for the size limit")

// IsPermanent reports whether err is a failure that a retry will not fix.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

type permanentErr struct{ msg string }

func permanentError(msg string) error { return &permanentErr{msg} }

func (e *permanentErr) Error() string        { return e.msg }
func (e *permanentErr) Is(target error) bool { return target == ErrPermanent }

// transcriptJSON is the file form of a transcript. Times are in seconds.
type transcriptJSON struct {
	Language string     `json:"language"`
	Words    []wordJSON `json:"words"`
}

type wordJSON struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// MarshalTranscript writes a transcript as indented JSON:
//
//	{"language": "en", "words": [{"text": "hi", "start": 0.12, "end": 0.4}]}
//
// with times in seconds from the start of the clip.
func MarshalTranscript(t transcribe.Transcript) ([]byte, error) {
	out := transcriptJSON{Language: t.Language, Words: make([]wordJSON, 0, len(t.Words))}
	for _, w := range t.Words {
		out.Words = append(out.Words, wordJSON{Text: w.Text, Start: secondsOf(w.Start), End: secondsOf(w.End)})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// secondsOf is d in seconds, rounded to the millisecond.
func secondsOf(d time.Duration) float64 {
	return float64(d.Round(time.Millisecond).Milliseconds()) / 1000
}

func detectionJSON(detector string, d layout.Detection, detErr string) any {
	type region struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	v := struct {
		Detector string  `json:"detector"`
		Webcam   bool    `json:"webcam"`
		Region   *region `json:"region,omitempty"`
		Error    string  `json:"error,omitempty"`
	}{Detector: detector, Webcam: d.Webcam, Error: detErr}
	if d.Webcam {
		v.Region = &region{d.Region.X, d.Region.Y, d.Region.W, d.Region.H}
	}
	return v
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	return nil
}
