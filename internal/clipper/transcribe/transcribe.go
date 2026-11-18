package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// Word is one spoken word. Start and End are measured from the start of the
// clip.
type Word struct {
	Text  string
	Start time.Duration
	End   time.Duration
}

// Transcript is what a Transcriber returns.
type Transcript struct {
	// Language is the language code of the speech, for example "en".
	Language string
	// Words are in spoken order. Starts and ends never decrease from one
	// word to the next, and no word ends before it starts. It is empty, not
	// nil, for a clip with no speech.
	Words []Word
}

// Transcriber turns the speech of a media file into words with timestamps.
type Transcriber interface {
	// Transcribe reads input, a video or audio file, and keeps its own
	// files inside workDir, which must exist. It never deletes workDir. A
	// clip with no speech gives an empty word list, not an error.
	Transcribe(ctx context.Context, input, workDir string) (Transcript, error)
}

// File names inside the work directory.
const (
	audioFile    = "audio.wav"
	whisperXDir  = "whisperx"
	whisperXJSON = "audio.json" // WhisperX names it after the audio file
)

// WhisperX is the Transcriber that runs ffmpeg and the WhisperX program.
type WhisperX struct {
	cfg    config.ClipperTranscribe
	logger *slog.Logger
}

var _ Transcriber = (*WhisperX)(nil)

// NewWhisperX returns a WhisperX transcriber for a validated configuration.
// A nil logger discards the warnings.
func NewWhisperX(cfg config.ClipperTranscribe, logger *slog.Logger) *WhisperX {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &WhisperX{cfg: cfg, logger: logger}
}

// Transcribe extracts the audio of input into workDir, runs WhisperX on it
// and reads its output. See the package documentation for the rules that
// turn the output into words.
func (w *WhisperX) Transcribe(ctx context.Context, input, workDir string) (Transcript, error) {
	audio := filepath.Join(workDir, audioFile)
	if _, err := runner.Run(ctx, runner.Command{
		Name:    w.cfg.FFmpegPath,
		Args:    extractArgs(input, audio),
		Timeout: w.cfg.Timeout,
	}); err != nil {
		return Transcript{}, fmt.Errorf("transcribe: extract audio: %w", err)
	}

	outDir := filepath.Join(workDir, whisperXDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Transcript{}, fmt.Errorf("transcribe: %w", err)
	}
	out := filepath.Join(outDir, whisperXJSON)
	// Never read a file left by an earlier run in the same directory.
	if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Transcript{}, fmt.Errorf("transcribe: %w", err)
	}

	aligned := w.cfg.Aligned()
	if !aligned {
		w.logger.Warn("transcribe: no alignment model for this language; word times are each segment's time split evenly",
			"language", w.cfg.Language)
	}
	if _, err := runner.Run(ctx, runner.Command{
		Name: w.cfg.WhisperXPath,
		Args: whisperXArgs(w.cfg, audio, outDir),
		// The image sets these already; repeated so no download is ever
		// tried at job time, whatever the environment.
		Env:     []string{"HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1"},
		Timeout: w.cfg.Timeout,
	}); err != nil {
		return Transcript{}, fmt.Errorf("transcribe: %w", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		return Transcript{}, fmt.Errorf("transcribe: whisperx output: %w", err)
	}
	t, stats, err := Parse(data)
	if err != nil {
		return Transcript{}, fmt.Errorf("transcribe: %s: %w", out, err)
	}
	if t.Language == "" {
		t.Language = w.cfg.Language
	}
	if aligned && stats.EvenSplitSegments > 0 {
		w.logger.Warn("transcribe: segments without word alignment; their time is split evenly across their words",
			"segments", stats.EvenSplitSegments)
	}
	if stats.FilledWords > 0 {
		w.logger.Info("transcribe: words without alignment timestamps were timed from their neighbours",
			"words", stats.FilledWords)
	}
	return t, nil
}

// extractArgs is the ffmpeg command line that writes the audio of input as
// mono 16 kHz 16-bit PCM WAV, the format Whisper works on.
func extractArgs(input, audio string) []string {
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", input,
		"-map", "0:a:0",
		"-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le",
		audio,
	}
}

// whisperXArgs is the WhisperX command line: CPU, JSON output, models from
// the local cache only. A language without an alignment model runs with
// --no_align.
func whisperXArgs(cfg config.ClipperTranscribe, audio, outDir string) []string {
	args := []string{
		audio,
		"--model", cfg.Model,
		"--model_cache_only", "True",
		"--language", cfg.Language,
		"--device", "cpu",
		"--compute_type", cfg.ComputeType,
		"--output_format", "json",
		"--output_dir", outDir,
	}
	if !cfg.Aligned() {
		args = append(args, "--no_align")
	}
	return args
}
