#!/usr/bin/env bash
# Sets up a Mac to run `clipper render` natively, without Docker. macOS only;
# other systems need their own setup script.
#
# It installs the same tools the clipper image has, at the same pinned
# versions where macOS builds exist:
#
#   - ffmpeg and ffprobe, from Homebrew's ffmpeg-full (the plain ffmpeg
#     formula has no libass, so it cannot burn in captions)
#   - WhisperX, in its own Python virtual environment
#   - OpenCV (opencv-python-headless) in the same environment, for the face
#     detector
#   - the Whisper model, the alignment model, the sentence splitter and the
#     YuNet face model, each downloaded once and checked against its SHA-256
#   - detect-faces, the face detector program of the clipper image, as
#     <tools-dir>/bin/detect-faces: a copy of deploy/clipper-base/detect-faces
#     whose first line runs the environment's Python (run the script again
#     after detect-faces changes, to refresh the copy)
#   - the Montserrat ExtraBold caption font, into ~/Library/Fonts
#   - TwitchDownloaderCLI, only with --with-twitch (needed for --vod, not
#     for --input)
#
# Everything except Homebrew's ffmpeg-full and the font goes into one directory,
# ~/pogfarm-tools by default, so removing that directory undoes it.
#
# At the end it writes <tools-dir>/clipper.env with the settings that point
# the clipper at these tools, runs WhisperX offline on a spoken test sentence
# exactly as the clipper does, and runs the face detector on a blank picture
# and on the image's test frame, to prove the setup works.
#
# Usage:
#   scripts/setup-clipper-macos.sh [--tools-dir DIR] [--with-twitch] [--dry-run]
#
# Then:
#   source ~/pogfarm-tools/clipper.env
#   go run ./cmd/clipper render --input stream.mp4 --start 1:02:10 --end 1:02:55 -o test.mp4
#
# The pinned versions, URLs and checksums below mirror
# deploy/clipper-base.Dockerfile. Change both together.
set -euo pipefail

# ---------------------------------------------------------------------------
# Pinned downloads (same as deploy/clipper-base.Dockerfile).

WHISPER_REPO=Systran/faster-whisper-small
WHISPER_REVISION=536b0662742c02347bc0e980a01041f333bce120
WHISPER_FILES=(
  "config.json    b55496ac7940a7ae47d2c01eab40edfd8701feec1229d9cce3b40014383fb828"
  "tokenizer.json fb7b63191e9bb045082c79fd742a3106a12c99513ab30df4a0d47fa6cb6fd0ab"
  "vocabulary.txt 34ce3fe1c5041027b3f8d42912270993f986dbc4bb34cf27f951e34a1e453913"
  "model.bin      3e305921506d8872816023e4c273e75d2419fb89b24da97b4fe7bce14170d671"
)

ALIGN_URL=https://download.pytorch.org/torchaudio/models/wav2vec2_fairseq_base_ls960_asr_ls960.pth
ALIGN_SHA=488fd4f16de84438ffc945334278c1b9fb9b7159a806c1080b16111a958c945d

PUNKT_URL=https://raw.githubusercontent.com/nltk/nltk_data/4f15a3d89eefe9748ec1c05be495d91289197155/packages/tokenizers/punkt_tab.zip
PUNKT_SHA=e57f64187974277726a3417ca6f181ec5403676c717672eef6a748a7b20e0106

# YuNet face detector (2023mar), from opencv_zoo commit eb9f4f2.
YUNET_BASE=https://media.githubusercontent.com/media/opencv/opencv_zoo/eb9f4f2d4b79b58e5513b5d25d5f0243a8357bf4/models/face_detection_yunet
YUNET_SHA=8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4
YUNET_LICENSE_URL=https://raw.githubusercontent.com/opencv/opencv_zoo/eb9f4f2d4b79b58e5513b5d25d5f0243a8357bf4/models/face_detection_yunet/LICENSE
YUNET_LICENSE_SHA=c83b8120c50ccbd4c4f96edf53141bdd566ebb8f8e9227e415326aa1b1aba958

FONT_URL=https://raw.githubusercontent.com/JulietaUla/Montserrat/555facfb2a18c72c3c0380f0d9c0f060453a9058/fonts/ttf/Montserrat-ExtraBold.ttf
FONT_SHA=d3ac6a843d3ba6d5cafd44cf39e437055c8aed7e261010f595f57d3c7b3e2c1b

# The image uses the Linux builds of this release; these are its macOS builds.
# Checksums from the release's asset list on GitHub.
TWITCHDOWNLOADER_VERSION=1.56.5
TWITCHDOWNLOADER_ARM64_SHA=17a0182f83689a0ae429f48cf03c26f1701eed7edcd787fa3d261898a6023d76
TWITCHDOWNLOADER_X64_SHA=efaa61d6109dfbce4ddedc28da55e23dc1de30caa2f6a35dc611f9b3bb48b724

# ---------------------------------------------------------------------------
# Options.

TOOLS_DIR="$HOME/pogfarm-tools"
WITH_TWITCH=0
DRY_RUN=0

# usage prints the comment at the top of this file.
usage() {
  awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --tools-dir) TOOLS_DIR="${2:?--tools-dir needs a directory}"; shift 2 ;;
    --tools-dir=*) TOOLS_DIR="${1#*=}"; shift ;;
    --with-twitch) WITH_TWITCH=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
VENV="$TOOLS_DIR/venv"
MODELS="$TOOLS_DIR/models"
WHISPER_DIR="$MODELS/whisper/faster-whisper-small"
TORCH_HOME_DIR="$MODELS/torch"
NLTK_DIR="$MODELS/nltk"
YUNET_MODEL="$MODELS/yunet/face_detection_yunet_2023mar.onnx"
DETECT_FACES="$TOOLS_DIR/bin/detect-faces"
FONT_DEST="$HOME/Library/Fonts/Montserrat-ExtraBold.ttf"
# ffmpeg-full is keg-only: it is not linked into Homebrew's bin, so it never
# replaces a plain ffmpeg. The clipper is pointed at it by full path.
FFMPEG_BIN="$(brew --prefix 2>/dev/null || echo /opt/homebrew)/opt/ffmpeg-full/bin"
TWITCH_DIR="$TOOLS_DIR/twitchdownloader"
ENV_FILE="$TOOLS_DIR/clipper.env"

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# run prints a command and runs it, or only prints it with --dry-run.
run() {
  printf '    $'; printf ' %q' "$@"; printf '\n'
  if [ "$DRY_RUN" -eq 0 ]; then "$@"; fi
}

sha_of() { shasum -a 256 "$1" | awk '{print $1}'; }

# fetch downloads url to dest unless dest already has the expected SHA-256,
# and fails if the download does not match it.
fetch() {
  local url="$1" sum="$2" dest="$3"
  if [ -f "$dest" ] && [ "$(sha_of "$dest")" = "$sum" ]; then
    info "already present: $dest"
    return
  fi
  run mkdir -p "$(dirname "$dest")"
  run curl -fL --retry 3 --progress-bar -o "$dest.part" "$url"
  if [ "$DRY_RUN" -eq 0 ]; then
    local got
    got="$(sha_of "$dest.part")"
    if [ "$got" != "$sum" ]; then
      rm -f "$dest.part"
      die "checksum mismatch for $url: got $got, want $sum"
    fi
  fi
  run mv "$dest.part" "$dest"
}

# ---------------------------------------------------------------------------
# Checks.

# macOS only. Other systems get their own script (none exists yet for
# Windows); on Linux the clipper image has every tool.
[ "$(uname -s)" = Darwin ] || die "this script is for macOS only (this system is $(uname -s)); on Linux use the clipper image (deploy/README.md)"
case "$(uname -m)" in
  arm64) TWITCH_ASSET=MacOSArm64; TWITCH_SHA=$TWITCHDOWNLOADER_ARM64_SHA ;;
  x86_64) TWITCH_ASSET=MacOS-x64; TWITCH_SHA=$TWITCHDOWNLOADER_X64_SHA ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac
command -v brew >/dev/null || die "Homebrew is required: https://brew.sh"
command -v curl >/dev/null || die "curl is required"

WHISPERX_VERSION="$(sed -n 's/.*"whisperx==\([0-9.]*\)".*/\1/p' "$REPO_DIR/deploy/clipper-base/pyproject.toml")"
[ -n "$WHISPERX_VERSION" ] || die "could not read the WhisperX version from deploy/clipper-base/pyproject.toml"
OPENCV_VERSION="$(sed -n 's/.*"opencv-python-headless==\([0-9.]*\)".*/\1/p' "$REPO_DIR/deploy/clipper-base/pyproject.toml")"
[ -n "$OPENCV_VERSION" ] || die "could not read the OpenCV version from deploy/clipper-base/pyproject.toml"
[ -f "$REPO_DIR/deploy/clipper-base/detect-faces" ] || die "deploy/clipper-base/detect-faces is missing from $REPO_DIR"

# WhisperX 3.8 needs Python 3.10 to 3.13; the image uses 3.13.
PYTHON=""
for candidate in python3.13 python3.12 python3.11 python3.10 python3; do
  if command -v "$candidate" >/dev/null &&
     "$candidate" -c 'import sys; sys.exit(0 if (3,10) <= sys.version_info[:2] <= (3,13) else 1)' 2>/dev/null; then
    PYTHON="$(command -v "$candidate")"
    break
  fi
done
[ -n "$PYTHON" ] || die "Python 3.10 to 3.13 is required (for example: brew install python@3.13)"

if [ "$DRY_RUN" -eq 1 ]; then
  echo "Dry run: commands are printed, nothing is installed or downloaded."
fi
info "tools directory: $TOOLS_DIR"
info "python:          $PYTHON ($("$PYTHON" --version 2>&1))"
info "whisperx:        $WHISPERX_VERSION"
info "opencv:          $OPENCV_VERSION"

# ---------------------------------------------------------------------------
# 1. ffmpeg.

step "ffmpeg and ffprobe (Homebrew ffmpeg-full, with libass for captions)"
if brew list --formula ffmpeg-full >/dev/null 2>&1; then
  info "already installed: $("$FFMPEG_BIN/ffmpeg" -hide_banner -version 2>/dev/null | head -n 1)"
else
  run brew install ffmpeg-full
fi
if [ "$DRY_RUN" -eq 0 ]; then
  # The final encode needs the ass filter (libass), libx264 and loudnorm.
  filters="$("$FFMPEG_BIN/ffmpeg" -hide_banner -filters 2>/dev/null)"
  encoders="$("$FFMPEG_BIN/ffmpeg" -hide_banner -encoders 2>/dev/null)"
  for need in " ass " " loudnorm "; do
    printf '%s\n' "$filters" | grep -q "$need" || die "$FFMPEG_BIN/ffmpeg has no${need}filter"
  done
  printf '%s\n' "$encoders" | grep -q " libx264 " || die "$FFMPEG_BIN/ffmpeg has no libx264 encoder"
  info "has the ass and loudnorm filters and the libx264 encoder"
fi

# ---------------------------------------------------------------------------
# 2. WhisperX.

step "WhisperX $WHISPERX_VERSION in $VENV"
if [ -x "$VENV/bin/whisperx" ] &&
   [ "$("$VENV/bin/python" -c 'import importlib.metadata as m; print(m.version("whisperx"))' 2>/dev/null)" = "$WHISPERX_VERSION" ]; then
  info "already installed"
else
  [ -d "$VENV" ] || run "$PYTHON" -m venv "$VENV"
  run "$VENV/bin/python" -m pip install --quiet --upgrade pip
  run "$VENV/bin/python" -m pip install --quiet "whisperx==$WHISPERX_VERSION"
fi

step "OpenCV $OPENCV_VERSION (opencv-python-headless) in $VENV, for the face detector"
if [ "$("$VENV/bin/python" -c 'import importlib.metadata as m; print(m.version("opencv-python-headless"))' 2>/dev/null)" = "$OPENCV_VERSION" ]; then
  info "already installed"
else
  run "$VENV/bin/python" -m pip install --quiet "opencv-python-headless==$OPENCV_VERSION"
fi

# ---------------------------------------------------------------------------
# 3. Models.

step "Whisper model ($WHISPER_REPO at ${WHISPER_REVISION:0:7})"
for entry in "${WHISPER_FILES[@]}"; do
  read -r name sum <<<"$entry"
  fetch "https://huggingface.co/$WHISPER_REPO/resolve/$WHISPER_REVISION/$name" "$sum" "$WHISPER_DIR/$name"
done

step "Alignment model (torchaudio WAV2VEC2_ASR_BASE_960H)"
fetch "$ALIGN_URL" "$ALIGN_SHA" "$TORCH_HOME_DIR/hub/checkpoints/$(basename "$ALIGN_URL")"

step "Sentence splitter (NLTK punkt_tab)"
if [ -d "$NLTK_DIR/tokenizers/punkt_tab" ]; then
  info "already present: $NLTK_DIR/tokenizers/punkt_tab"
else
  fetch "$PUNKT_URL" "$PUNKT_SHA" "$TOOLS_DIR/downloads/punkt_tab.zip"
  run mkdir -p "$NLTK_DIR/tokenizers"
  run unzip -q -o "$TOOLS_DIR/downloads/punkt_tab.zip" -d "$NLTK_DIR/tokenizers"
fi

step "Face model (YuNet 2023mar)"
fetch "$YUNET_BASE/face_detection_yunet_2023mar.onnx" "$YUNET_SHA" "$YUNET_MODEL"
fetch "$YUNET_LICENSE_URL" "$YUNET_LICENSE_SHA" "$(dirname "$YUNET_MODEL")/LICENSE"

# ---------------------------------------------------------------------------
# 3b. The face detector program.

# The image installs deploy/clipper-base/detect-faces as is: its first line
# runs /opt/venv/bin/python. Here it is copied with that line replaced by
# this environment's Python. A copy, not a wrapper that runs the file in
# the repository, so it keeps working when the checkout moves or is a
# temporary worktree; running this script again refreshes it.
step "detect-faces: $DETECT_FACES"
detect_faces_src="$REPO_DIR/deploy/clipper-base/detect-faces"
detect_faces_new="$(printf '#!%s\n' "$VENV/bin/python"; tail -n +2 "$detect_faces_src")"
if [ -x "$DETECT_FACES" ] && [ "$(cat "$DETECT_FACES")" = "$detect_faces_new" ]; then
  info "already installed and up to date"
elif [ "$DRY_RUN" -eq 1 ]; then
  info "would copy $detect_faces_src to $DETECT_FACES, its first line replaced by #!$VENV/bin/python"
else
  mkdir -p "$(dirname "$DETECT_FACES")"
  printf '%s\n' "$detect_faces_new" > "$DETECT_FACES.part"
  chmod 0755 "$DETECT_FACES.part"
  mv "$DETECT_FACES.part" "$DETECT_FACES"
  info "copied from $detect_faces_src, first line #!$VENV/bin/python"
fi

# ---------------------------------------------------------------------------
# 4. Caption font.

step "Caption font (Montserrat ExtraBold)"
fetch "$FONT_URL" "$FONT_SHA" "$FONT_DEST"

# ---------------------------------------------------------------------------
# 5. TwitchDownloaderCLI (optional).

if [ "$WITH_TWITCH" -eq 1 ]; then
  step "TwitchDownloaderCLI $TWITCHDOWNLOADER_VERSION ($TWITCH_ASSET)"
  if [ -x "$TWITCH_DIR/TwitchDownloaderCLI" ]; then
    info "already installed"
  else
    zip="$TOOLS_DIR/downloads/TwitchDownloaderCLI-$TWITCHDOWNLOADER_VERSION-$TWITCH_ASSET.zip"
    fetch "https://github.com/lay295/TwitchDownloader/releases/download/$TWITCHDOWNLOADER_VERSION/TwitchDownloaderCLI-$TWITCHDOWNLOADER_VERSION-$TWITCH_ASSET.zip" \
      "$TWITCH_SHA" "$zip"
    run mkdir -p "$TWITCH_DIR"
    run unzip -q -o "$zip" -d "$TWITCH_DIR"
    run chmod 0755 "$TWITCH_DIR/TwitchDownloaderCLI"
  fi
fi

# ---------------------------------------------------------------------------
# 6. Settings for the clipper.

step "Settings: $ENV_FILE"
env_content="# Written by scripts/setup-clipper-macos.sh. Load with: source $ENV_FILE
export CLIPPER_DOWNLOAD_FFMPEG_PATH=\"$FFMPEG_BIN/ffmpeg\"
export CLIPPER_DOWNLOAD_FFPROBE_PATH=\"$FFMPEG_BIN/ffprobe\"
export CLIPPER_TRANSCRIBE_FFMPEG_PATH=\"$FFMPEG_BIN/ffmpeg\"
export CLIPPER_RENDER_FFMPEG_PATH=\"$FFMPEG_BIN/ffmpeg\"
export CLIPPER_RENDER_FFPROBE_PATH=\"$FFMPEG_BIN/ffprobe\"
# WhisperX runs ffmpeg itself, by name.
export PATH=\"$FFMPEG_BIN:\$PATH\"
export CLIPPER_TRANSCRIBE_WHISPERX_PATH=\"$VENV/bin/whisperx\"
export CLIPPER_TRANSCRIBE_MODEL=\"$WHISPER_DIR\"
export TORCH_HOME=\"$TORCH_HOME_DIR\"
export NLTK_DATA=\"$NLTK_DIR\"
# Face detection: the detector program, its model, and the ffmpeg that
# extracts the frames. POGFARM_YUNET_MODEL is for running detect-faces by hand.
export CLIPPER_WEBCAM_DETECTION_DETECT_FACES_PATH=\"$DETECT_FACES\"
export CLIPPER_WEBCAM_DETECTION_MODEL=\"$YUNET_MODEL\"
export CLIPPER_WEBCAM_DETECTION_FFMPEG_PATH=\"$FFMPEG_BIN/ffmpeg\"
export POGFARM_YUNET_MODEL=\"$YUNET_MODEL\""
if [ "$WITH_TWITCH" -eq 1 ]; then
  env_content="$env_content
export CLIPPER_DOWNLOAD_TWITCH_DOWNLOADER_PATH=\"$TWITCH_DIR/TwitchDownloaderCLI\""
fi
if [ "$DRY_RUN" -eq 1 ]; then
  printf '%s\n' "$env_content" | sed 's/^/    /'
else
  mkdir -p "$TOOLS_DIR"
  printf '%s\n' "$env_content" > "$ENV_FILE"
  info "written"
fi

# ---------------------------------------------------------------------------
# 7. Self-test: WhisperX offline, exactly as the clipper runs it.

step "Self-test: transcribe a spoken sentence offline"
if [ "$DRY_RUN" -eq 1 ]; then
  info "skipped in a dry run"
else
  test_dir="$(mktemp -d)"
  trap 'rm -rf "$test_dir"' EXIT
  say -o "$test_dir/speech.aiff" "This is a short test of the clipper transcription setup."
  "$FFMPEG_BIN/ffmpeg" -hide_banner -nostdin -loglevel error -y -i "$test_dir/speech.aiff" \
    -ac 1 -ar 16000 -c:a pcm_s16le "$test_dir/speech.wav"
  # The same arguments and offline settings as internal/clipper/transcribe.
  PATH="$FFMPEG_BIN:$PATH" HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 TORCH_HOME="$TORCH_HOME_DIR" NLTK_DATA="$NLTK_DIR" \
    "$VENV/bin/whisperx" "$test_dir/speech.wav" \
      --model "$WHISPER_DIR" --model_cache_only True --language en \
      --device cpu --compute_type int8 --output_format json --output_dir "$test_dir" \
      >"$test_dir/whisperx.log" 2>&1 ||
    { cat "$test_dir/whisperx.log" >&2; die "WhisperX failed; its output is above"; }
  words="$("$VENV/bin/python" -c '
import json, sys
d = json.load(open(sys.argv[1]))
words = [w for s in d.get("segments", []) for w in s.get("words", []) if "start" in w]
print(len(words))
print(" ".join(w["word"] for w in words))
' "$test_dir/speech.json")"
  count="$(printf '%s\n' "$words" | head -n 1)"
  [ "${count:-0}" -gt 0 ] || die "WhisperX ran but returned no timed words"
  info "$count timed words: $(printf '%s\n' "$words" | tail -n 1)"
fi

# ---------------------------------------------------------------------------
# 8. Self-test: the face detector, as the clipper runs it.

step "Self-test: the face detector on a blank picture and on a webcam frame"
if [ "$DRY_RUN" -eq 1 ]; then
  info "skipped in a dry run"
else
  face_dir="$(mktemp -d)"
  trap 'rm -rf "${test_dir:-}" "$face_dir"' EXIT
  # A generated picture with no face proves the program, Python, OpenCV and
  # the model work; the clipper's startup check does the same.
  "$FFMPEG_BIN/ffmpeg" -hide_banner -nostdin -loglevel error -y -f lavfi -i color=c=gray:s=640x360 \
    -frames:v 1 "$face_dir/blank.png"
  frame="$REPO_DIR/deploy/clipper-base/test/webcam-frame.jpg"
  "$DETECT_FACES" --model "$YUNET_MODEL" -- "$face_dir/blank.png" "$frame" >"$face_dir/faces.json" 2>"$face_dir/faces.log" ||
    { cat "$face_dir/faces.log" >&2; die "detect-faces failed; its output is above"; }
  # The webcam picture in webcam-frame.jpg covers x 24..344, y 456..696; the
  # clipper image's own test checks the same.
  result="$("$VENV/bin/python" -c '
import json, sys
blank, frame = json.load(open(sys.argv[1]))["images"]
inside = [f for f in frame["faces"]
          if 24 <= f["x"] + f["w"] / 2 <= 344 and 456 <= f["y"] + f["h"] / 2 <= 696]
ok = not blank["error"] and not blank["faces"] and not frame["error"] and len(inside) == 1
print("ok" if ok else "bad", len(blank["faces"]), len(frame["faces"]), inside[0]["confidence"] if inside else "-")
' "$face_dir/faces.json")"
  read -r verdict blank_faces frame_faces confidence <<<"$result"
  [ "$verdict" = ok ] || { cat "$face_dir/faces.json" >&2; die "the face detector answered wrongly; its output is above"; }
  info "blank picture: $blank_faces faces; webcam frame: $frame_faces face(s), the webcam face at confidence $confidence"
fi

# ---------------------------------------------------------------------------

step "Done"
cat <<EOF
    Load the settings in each new shell, then render:

      source $ENV_FILE
      go run ./cmd/clipper render --input stream.mp4 --start 1:02:10 --end 1:02:55 -o test.mp4 --keep-work

    The kept work directory holds detect.jpg and detection.json, which show
    the faces found and how the clip was framed.
EOF
if [ "$WITH_TWITCH" -eq 0 ]; then
  echo "    For --vod, run this script again with --with-twitch."
fi
