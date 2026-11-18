package config

import (
	"fmt"
	"regexp"
	"slices"
	"time"
)

// ClipperTranscribe is the "clipper.transcribe" section: speech to words
// with timestamps, done by WhisperX (package internal/clipper/transcribe).
// The defaults match the clipper base image (deploy/clipper-base.Dockerfile).
type ClipperTranscribe struct {
	// WhisperXPath is the WhisperX command line program: a path, or a name
	// looked up in PATH.
	WhisperXPath string `yaml:"whisperx_path" env:"CLIPPER_TRANSCRIBE_WHISPERX_PATH"`
	// FFmpegPath is the ffmpeg program that extracts the audio track.
	FFmpegPath string `yaml:"ffmpeg_path" env:"CLIPPER_TRANSCRIBE_FFMPEG_PATH"`
	// Model is the Whisper model, as a directory holding a CTranslate2
	// (faster-whisper) model, or a model name such as "small" that is
	// already in the Hugging Face cache. Models are never downloaded at job
	// time.
	Model string `yaml:"model" env:"CLIPPER_TRANSCRIBE_MODEL"`
	// Language is the Whisper language code of the speech, for example
	// "en". Language detection is not offered: WhisperX would then align
	// with the English model whatever the language, and report "en".
	Language string `yaml:"language" env:"CLIPPER_TRANSCRIBE_LANGUAGE"`
	// AlignedLanguages lists the languages whose alignment model is
	// installed. For any other language WhisperX runs without alignment
	// and every segment's time is split evenly across its words. File only.
	AlignedLanguages []string `yaml:"aligned_languages"`
	// ComputeType is the CTranslate2 compute type: "int8", "float32" or
	// "default" (float32 on CPU). float16 needs a GPU and is refused.
	ComputeType string `yaml:"compute_type" env:"CLIPPER_TRANSCRIBE_COMPUTE_TYPE"`
	// Timeout bounds each tool run: the audio extraction and WhisperX.
	Timeout time.Duration `yaml:"timeout" env:"CLIPPER_TRANSCRIBE_TIMEOUT"`
}

// defaultClipperTranscribe is the transcribe section when nothing is
// configured: the programs on PATH and the model, language and compute
// type the clipper base image is built and tested with.
func defaultClipperTranscribe() ClipperTranscribe {
	return ClipperTranscribe{
		WhisperXPath:     "whisperx",
		FFmpegPath:       "ffmpeg",
		Model:            "/models/whisper/faster-whisper-small",
		Language:         "en",
		AlignedLanguages: []string{"en"},
		ComputeType:      "int8",
		Timeout:          10 * time.Minute,
	}
}

// transcribeLanguageCode matches a Whisper language code: two or three lower-case
// letters ("en", "haw", "yue").
var transcribeLanguageCode = regexp.MustCompile(`^[a-z]{2,3}$`)

var transcribeComputeTypes = []string{"int8", "float32", "default"}

func (t ClipperTranscribe) validate() []string {
	var problems []string
	if t.WhisperXPath == "" {
		problems = append(problems, "clipper.transcribe.whisperx_path (CLIPPER_TRANSCRIBE_WHISPERX_PATH) must not be empty")
	}
	if t.FFmpegPath == "" {
		problems = append(problems, "clipper.transcribe.ffmpeg_path (CLIPPER_TRANSCRIBE_FFMPEG_PATH) must not be empty")
	}
	if t.Model == "" {
		problems = append(problems, "clipper.transcribe.model (CLIPPER_TRANSCRIBE_MODEL) must not be empty")
	}
	if !transcribeLanguageCode.MatchString(t.Language) {
		problems = append(problems, fmt.Sprintf(
			"clipper.transcribe.language (CLIPPER_TRANSCRIBE_LANGUAGE) must be a Whisper language code such as \"en\": got %q", t.Language))
	}
	for _, l := range t.AlignedLanguages {
		if !transcribeLanguageCode.MatchString(l) {
			problems = append(problems, fmt.Sprintf(
				"clipper.transcribe.aligned_languages must hold Whisper language codes such as \"en\": got %q", l))
		}
	}
	if !slices.Contains(transcribeComputeTypes, t.ComputeType) {
		problems = append(problems, fmt.Sprintf(
			"clipper.transcribe.compute_type (CLIPPER_TRANSCRIBE_COMPUTE_TYPE) must be one of int8, float32 or default (float16 needs a GPU): got %q", t.ComputeType))
	}
	if t.Timeout <= 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.transcribe.timeout (CLIPPER_TRANSCRIBE_TIMEOUT) must be a positive duration: got %s", t.Timeout))
	}
	return problems
}

// Aligned reports whether the configured language has an alignment model.
func (t ClipperTranscribe) Aligned() bool {
	return slices.Contains(t.AlignedLanguages, t.Language)
}
