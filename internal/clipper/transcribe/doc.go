// Package transcribe turns the speech of a clip into words with start and
// end times, for the captions.
//
// The step is an interface, Transcriber, so the tool behind it can be
// replaced without touching the rest of the clipper. This package owns the
// result types, Transcript and Word. They do not depend on the job contract
// in internal/api; the caller maps them to it.
//
// # WhisperX
//
// The one implementation, WhisperX, runs two programs through
// internal/runner:
//
//  1. ffmpeg extracts the audio track as mono 16 kHz 16-bit WAV
//     (<workDir>/audio.wav), the format Whisper works on.
//  2. whisperx transcribes it on CPU and writes JSON
//     (<workDir>/whisperx/audio.json). Its alignment step gives each word
//     its own start and end, which is what word-by-word highlighted
//     captions need.
//
// The command is the one the clipper base image is tested with
// (deploy/clipper-base/test/run-tests.sh):
//
//	whisperx <workDir>/audio.wav --model <model> --model_cache_only True
//	  --language <language> --device cpu --compute_type <compute type>
//	  --output_format json --output_dir <workDir>/whisperx
//
// plus --no_align when the language has no alignment model (see below).
// The model is passed as a path and --model_cache_only stops downloads;
// the image also sets the offline switches of Hugging Face, and the
// process gets HF_HUB_OFFLINE=1 and TRANSFORMERS_OFFLINE=1 here as well,
// so no network is used at job time. --model_dir is never passed: torchaudio
// would then look for the alignment model there instead of in $TORCH_HOME.
//
// # Reading the output
//
// The output shape is that of WhisperX 3.8.6 (the version in
// deploy/clipper-base/uv.lock), checked against its source:
// whisperx/transcribe.py writes {"segments", "word_segments", "language"}
// after alignment and {"segments", "language"} without it;
// whisperx/alignment.py builds the segments. Only the segments are read.
// A segment has "start", "end" and "text", and, when aligned, "words",
// each with "word" and, when the word could be aligned, "start", "end" and
// "score". WhisperX writes the JSON with Python's json module, which turns
// a missing number into the bare token NaN; such tokens are read as absent.
//
// The word list is built by these rules:
//
//   - No speech. When voice activity detection finds nothing, WhisperX
//     writes no segments, and the transcript has an empty word list. That
//     is not an error.
//   - Words without timestamps. WhisperX leaves out "start" and "end" for
//     a word with no character its alignment model knows, such as a number
//     ("1961") or a symbol, when its whole sentence has none either. A word
//     counts as timed only when it has both. Each run of consecutive
//     untimed words shares the gap between the end of the timed word before
//     it (or the segment start) and the start of the timed word after it
//     (or the segment end), split evenly in order.
//   - Segments without words. A segment WhisperX could not align (it then
//     writes "words": []), and every segment of a run with --no_align, has
//     only a segment time. Its text is split at white space and the segment
//     time is split evenly across those words. A warning is logged. For
//     languages written without spaces (Japanese, Chinese) this gives one
//     word per segment.
//   - Order and clamping. Words keep WhisperX's order and are never
//     sorted. Times are clamped so that no time is negative, starts and
//     ends never decrease from one word to the next, and no word ends
//     before it starts.
//   - Language. The transcript's language is the "language" WhisperX
//     wrote, which is the configured one; there is no language detection.
//
// # Languages without an alignment model
//
// The image holds the English alignment model only, and WhisperX fails
// when asked to align a language whose model it cannot load. For a
// configured language not listed in clipper.transcribe.aligned_languages,
// WhisperX runs with --no_align, a warning is logged, and every segment's
// time is split evenly across its words as above.
//
// # Running it for real
//
// The tests use a fake whisperx script that writes a recorded JSON file.
// To check the real program, build the clipper base image (deploy/README.md)
// and run the command above inside it on any clip with speech, with
// networking disabled (docker run --network none). The base image's own
// smoke test does exactly that on linux/amd64 and linux/arm64.
package transcribe
