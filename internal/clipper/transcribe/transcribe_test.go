package transcribe

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// imageConfig is the transcribe configuration the clipper base image runs
// with: config's defaults.
func imageConfig() config.ClipperTranscribe {
	return config.ClipperTranscribe{
		WhisperXPath:     "whisperx",
		FFmpegPath:       "ffmpeg",
		Model:            "/models/whisper/faster-whisper-small",
		Language:         "en",
		AlignedLanguages: []string{"en"},
		ComputeType:      "int8",
		Timeout:          10 * time.Minute,
	}
}

func TestExtractArgs(t *testing.T) {
	got := strings.Join(extractArgs("/work/clip.mp4", "/work/audio.wav"), " ")
	want := "-hide_banner -nostdin -loglevel error -y -i /work/clip.mp4 -map 0:a:0 -ac 1 -ar 16000 -c:a pcm_s16le /work/audio.wav"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestWhisperXArgs(t *testing.T) {
	cfg := imageConfig()
	got := strings.Join(whisperXArgs(cfg, "/work/audio.wav", "/work/whisperx"), " ")
	// The command of deploy/clipper-base/test/run-tests.sh.
	want := "/work/audio.wav --model /models/whisper/faster-whisper-small --model_cache_only True" +
		" --language en --device cpu --compute_type int8 --output_format json --output_dir /work/whisperx"
	if got != want {
		t.Errorf("aligned:\n got %s\nwant %s", got, want)
	}

	cfg.Language = "de"
	cfg.ComputeType = "float32"
	cfg.Model = "small"
	got = strings.Join(whisperXArgs(cfg, "/w/audio.wav", "/w/whisperx"), " ")
	want = "/w/audio.wav --model small --model_cache_only True" +
		" --language de --device cpu --compute_type float32 --output_format json --output_dir /w/whisperx --no_align"
	if got != want {
		t.Errorf("no alignment model:\n got %s\nwant %s", got, want)
	}
}

// fakeWhisperX writes a script that stands in for whisperx. It records its
// arguments in <dir>/args, checks that the audio file exists, and then
// runs body, a shell snippet that can use $audio and $out (the output
// directory).
func fakeWhisperX(t *testing.T, body string) (path, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	path = filepath.Join(dir, "whisperx")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > '` + argsFile + `'
audio=$1
shift
out=
while [ $# -gt 0 ]; do
	case $1 in
	--output_dir) out=$2; shift 2 ;;
	*) shift ;;
	esac
done
[ -s "$audio" ] || { echo "fake whisperx: no audio at $audio" >&2; exit 2; }
[ -d "$out" ] || { echo "fake whisperx: no output directory $out" >&2; exit 2; }
[ "$HF_HUB_OFFLINE" = 1 ] || { echo "fake whisperx: not offline" >&2; exit 2; }
` + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

// copyFixture is a fakeWhisperX body that writes a fixture where WhisperX
// writes its JSON: <output dir>/<audio name without extension>.json.
func copyFixture(t *testing.T, name string) string {
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return `base=$(basename "$audio"); cp '` + abs + `' "$out/${base%.*}.json"`
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
}

// makeMedia runs ffmpeg with args (lavfi sources and encoders) and returns
// the file it wrote, named name in a temporary directory.
func makeMedia(t *testing.T, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	full := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
	full = append(full, path)
	if _, err := runner.Run(context.Background(), runner.Command{Name: "ffmpeg", Args: full, Timeout: time.Minute}); err != nil {
		t.Fatalf("generate %s: %v", name, err)
	}
	return path
}

// speechClip is a stand-in for a downloaded clip: stereo 44.1 kHz audio
// with video, so the extraction has to drop the video, downmix and resample.
func speechClip(t *testing.T) string {
	return makeMedia(t, "clip.mp4",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=1",
		"-ac", "2", "-c:v", "mpeg4", "-c:a", "aac", "-shortest")
}

// wavFormat returns the channel count and sample rate of a WAV file.
func wavFormat(t *testing.T, path string) (channels, rate int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("%s is not a WAV file", path)
	}
	for p := 12; p+8 <= len(data); {
		id := string(data[p : p+4])
		size := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
		if id == "fmt " && p+16 <= len(data) {
			return int(binary.LittleEndian.Uint16(data[p+10:])), int(binary.LittleEndian.Uint32(data[p+12:]))
		}
		p += 8 + size + size%2
	}
	t.Fatalf("%s has no fmt chunk", path)
	return 0, 0
}

func newTestTranscriber(cfg config.ClipperTranscribe) (*WhisperX, *bytes.Buffer) {
	var logs bytes.Buffer
	return NewWhisperX(cfg, slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

func TestTranscribeEndToEnd(t *testing.T) {
	needFFmpeg(t)
	input := speechClip(t)
	work := t.TempDir()

	cfg := imageConfig()
	var argsFile string
	cfg.WhisperXPath, argsFile = fakeWhisperX(t, copyFixture(t, "normal.json"))
	tr, logs := newTestTranscriber(cfg)

	got, err := tr.Transcribe(context.Background(), input, work)
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := Parse(readFixture(t, "normal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transcript:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Words) != 22 || got.Language != "en" {
		t.Errorf("want 22 English words, got %d in %q", len(got.Words), got.Language)
	}

	audio := filepath.Join(work, "audio.wav")
	if ch, rate := wavFormat(t, audio); ch != 1 || rate != 16000 {
		t.Errorf("audio: want mono 16000 Hz, got %d channels at %d Hz", ch, rate)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := strings.Join(whisperXArgs(cfg, audio, filepath.Join(work, "whisperx")), "\n") + "\n"
	if string(args) != wantArgs {
		t.Errorf("whisperx arguments:\n got %q\nwant %q", args, wantArgs)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no warning expected for a fully aligned transcript:\n%s", logs.String())
	}
}

func TestTranscribeSilence(t *testing.T) {
	needFFmpeg(t)
	input := makeMedia(t, "silence.m4a",
		"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000", "-t", "1", "-c:a", "aac")

	cfg := imageConfig()
	cfg.WhisperXPath, _ = fakeWhisperX(t, copyFixture(t, "silence.json"))
	tr, _ := newTestTranscriber(cfg)

	got, err := tr.Transcribe(context.Background(), input, t.TempDir())
	if err != nil {
		t.Fatalf("silence must not be an error: %v", err)
	}
	if got.Words == nil || len(got.Words) != 0 || got.Language != "en" {
		t.Errorf("want an empty English transcript, got %#v", got)
	}
}

func TestTranscribeWithoutAlignmentModel(t *testing.T) {
	needFFmpeg(t)
	input := speechClip(t)

	cfg := imageConfig()
	cfg.Language = "de"
	var argsFile string
	cfg.WhisperXPath, argsFile = fakeWhisperX(t, copyFixture(t, "unaligned_de.json"))
	tr, logs := newTestTranscriber(cfg)

	got, err := tr.Transcribe(context.Background(), input, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != "de" || len(got.Words) != 9 || got.Words[0].Text != "Grüße" {
		t.Errorf("got %+v", got)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "\n--no_align\n") {
		t.Errorf("want --no_align, got arguments:\n%s", args)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "no alignment model") {
		t.Errorf("want a warning about the missing alignment model, got:\n%s", logs.String())
	}
}

func TestTranscribeFailures(t *testing.T) {
	needFFmpeg(t)
	input := speechClip(t)

	t.Run("whisperx fails", func(t *testing.T) {
		cfg := imageConfig()
		cfg.WhisperXPath, _ = fakeWhisperX(t, `echo "ValueError: No default align-model for language: xx" >&2; exit 1`)
		tr, _ := newTestTranscriber(cfg)
		_, err := tr.Transcribe(context.Background(), input, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "No default align-model") {
			t.Errorf("want an error with whisperx's stderr, got %v", err)
		}
	})

	t.Run("no output, stale file ignored", func(t *testing.T) {
		work := t.TempDir()
		if err := os.MkdirAll(filepath.Join(work, "whisperx"), 0o755); err != nil {
			t.Fatal(err)
		}
		stale := filepath.Join(work, "whisperx", "audio.json")
		if err := os.WriteFile(stale, readFixture(t, "normal.json"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := imageConfig()
		cfg.WhisperXPath, _ = fakeWhisperX(t, `true`)
		tr, _ := newTestTranscriber(cfg)
		if _, err := tr.Transcribe(context.Background(), input, work); err == nil {
			t.Error("want an error when whisperx writes nothing")
		}
	})

	t.Run("malformed output", func(t *testing.T) {
		cfg := imageConfig()
		cfg.WhisperXPath, _ = fakeWhisperX(t, `printf '{"segments": [' > "$out/audio.json"`)
		tr, _ := newTestTranscriber(cfg)
		if _, err := tr.Transcribe(context.Background(), input, t.TempDir()); err == nil {
			t.Error("want an error for malformed JSON")
		}
	})

	t.Run("input without audio", func(t *testing.T) {
		video := makeMedia(t, "video.mp4",
			"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=1", "-c:v", "mpeg4")
		cfg := imageConfig()
		cfg.WhisperXPath, _ = fakeWhisperX(t, copyFixture(t, "normal.json"))
		tr, _ := newTestTranscriber(cfg)
		_, err := tr.Transcribe(context.Background(), video, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "extract audio") {
			t.Errorf("want an audio extraction error, got %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		cfg := imageConfig()
		cfg.WhisperXPath, _ = fakeWhisperX(t, copyFixture(t, "normal.json"))
		tr, _ := newTestTranscriber(cfg)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := tr.Transcribe(ctx, input, t.TempDir()); err == nil {
			t.Error("want an error for a cancelled context")
		}
	})
}

func TestWorkDirKept(t *testing.T) {
	needFFmpeg(t)
	work := t.TempDir()
	cfg := imageConfig()
	cfg.WhisperXPath, _ = fakeWhisperX(t, `exit 1`)
	tr, _ := newTestTranscriber(cfg)
	if _, err := tr.Transcribe(context.Background(), speechClip(t), work); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(work); err != nil {
		t.Errorf("work directory must survive a failure: %v", err)
	}
}
