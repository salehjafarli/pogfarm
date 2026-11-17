#!/bin/bash
# Smoke test of the clipper base image. Runs inside the built image with
# networking disabled; this directory is mounted read-only at /test:
#
#   docker run --rm --network none -v "$PWD/deploy/clipper-base/test:/test:ro" \
#     pogfarm-clipper-base:dev bash /test/run-tests.sh
#
# Checks: not root; ffmpeg has libass and libx264 and burns a caption in the
# caption font; ffprobe reads audio; TwitchDownloaderCLI starts; WhisperX
# transcribes real English speech with word timestamps; the face detector
# finds the face in a stream-like frame and none in an empty one.
set -euo pipefail

fixtures=/test
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

step() { printf '\n=== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*"; failures=$((failures + 1)); }
pass() { printf 'ok: %s\n' "$*"; }
now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }

step "environment"
uname -m
id
if [ "$(id -u)" -eq 0 ]; then fail "running as root"; else pass "running as uid $(id -u)"; fi
if python3.13 -c 'import socket; socket.create_connection(("1.1.1.1", 53), timeout=3)' 2>/dev/null; then
	echo "warning: the network is reachable; run with --network none for a real offline test"
else
	pass "network is unreachable"
fi

step "ffmpeg"
ffmpeg -hide_banner -version | head -n 1
buildconf=$(ffmpeg -hide_banner -buildconf)
for lib in libass libx264; do
	if grep -q -- "--enable-$lib" <<<"$buildconf"; then pass "ffmpeg built with $lib"; else fail "ffmpeg lacks $lib"; fi
done

# Burn a caption with libass in the caption font and encode with libx264.
fc-match 'Montserrat:extrabold'
if fc-match 'Montserrat:extrabold' | grep -q 'Montserrat-ExtraBold'; then pass "fontconfig finds Montserrat ExtraBold"; else fail "caption font not found"; fi
cat >"$work/caption.ass" <<'EOF'
[Script Info]
ScriptType: v4.00+
PlayResX: 720
PlayResY: 1280

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, OutlineColour, BackColour, Bold, Italic, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV
Style: Default,Montserrat ExtraBold,80,&H00FFFFFF,&H00000000,&H00000000,0,0,1,6,0,2,40,40,200

[Events]
Format: Layer, Start, End, Style, Text
Dialogue: 0,0:00:00.00,0:00:01.00,Default,ASK NOT WHAT YOUR COUNTRY
EOF
start=$(now)
if ffmpeg -hide_banner -loglevel verbose -f lavfi -i color=c=black:s=720x1280:d=1 \
	-vf "subtitles=$work/caption.ass" -c:v libx264 -preset veryfast -pix_fmt yuv420p "$work/caption.mp4" 2>"$work/ffmpeg.log"; then
	pass "burned a caption with libass and encoded with libx264 in $(elapsed "$start")s"
else
	tail -n 30 "$work/ffmpeg.log"
	fail "caption burn-in failed"
fi
# libass logs the font file it picked for each font request.
grep -i 'fontselect' "$work/ffmpeg.log" || true
if grep -i 'fontselect' "$work/ffmpeg.log" | grep -q 'Montserrat-ExtraBold'; then
	pass "libass used Montserrat ExtraBold"
else
	fail "libass did not use the caption font"
fi

step "ffprobe"
duration=$(ffprobe -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 "$fixtures/jfk.mp3")
echo "jfk.mp3 duration: ${duration}s"
if awk -v d="$duration" 'BEGIN { exit !(d > 10 && d < 12) }'; then pass "ffprobe reads audio"; else fail "unexpected duration $duration"; fi

step "TwitchDownloaderCLI"
# The argument parser may exit non-zero after printing the version, so only
# the printed version is checked.
out=$(TwitchDownloaderCLI --version 2>&1) || true
echo "$out"
if grep -q '1\.56\.5' <<<"$out"; then pass "TwitchDownloaderCLI 1.56.5 runs"; else fail "TwitchDownloaderCLI did not report version 1.56.5"; fi

step "WhisperX"
start=$(now)
if whisperx "$fixtures/jfk.mp3" \
	--model "$POGFARM_WHISPER_MODEL" --model_cache_only True \
	--language en --device cpu --compute_type int8 \
	--output_format json --output_dir "$work/whisperx" >"$work/whisperx.log" 2>&1; then
	transcribe_time=$(elapsed "$start")
	pass "whisperx finished in ${transcribe_time}s"
else
	cat "$work/whisperx.log"
	fail "whisperx failed"
fi
echo "--- whisperx log (last 20 lines)"
tail -n 20 "$work/whisperx.log"
if [ -f "$work/whisperx/jfk.json" ]; then
	if python3.13 - "$work/whisperx/jfk.json" <<'EOF'; then pass "transcript has the expected words with timestamps"; else fail "transcript check failed"; fi
import json, sys
doc = json.load(open(sys.argv[1]))
text = " ".join(s["text"].strip() for s in doc["segments"])
words = [w for s in doc["segments"] for w in s.get("words", [])]
print("text:", text)
print("words:", len(words))
for w in words[:8]:
    print(f'  {w.get("start", "-"):>6} {w.get("end", "-"):>6} {w.get("score", "-")} {w["word"]}')
timed = [w for w in words if "start" in w and "end" in w]
ok = True
if "country" not in text.lower():
    print("expected the word 'country' in the transcript"); ok = False
if len(words) < 15 or len(timed) < len(words) - 2:
    print("expected at least 15 words, nearly all with start and end"); ok = False
if any(w["end"] < w["start"] for w in timed):
    print("a word ends before it starts"); ok = False
sys.exit(0 if ok else 1)
EOF
else
	fail "whisperx wrote no JSON"
fi

step "face detector"
ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=gray:s=1280x720 -frames:v 1 "$work/empty.png"
start=$(now)
if detect-faces "$fixtures/webcam-frame.jpg" "$work/empty.png" >"$work/faces.json"; then
	pass "detect-faces finished in $(elapsed "$start")s"
else
	fail "detect-faces failed"
fi
cat "$work/faces.json"
if python3.13 - "$work/faces.json" <<'EOF'; then pass "face found inside the webcam area, none in the empty frame"; else fail "face detector check failed"; fi
import json, sys
doc = json.load(open(sys.argv[1]))
frame, empty = doc["images"]
ok = True
# The webcam picture in webcam-frame.jpg covers x 24..344, y 456..696.
inside = [f for f in frame["faces"]
          if 24 <= f["x"] + f["w"] / 2 <= 344 and 456 <= f["y"] + f["h"] / 2 <= 696]
if len(inside) != 1:
    print("expected one face in the webcam area, got", frame["faces"]); ok = False
if empty["faces"]:
    print("expected no face in the empty frame, got", empty["faces"]); ok = False
sys.exit(0 if ok else 1)
EOF

step "result"
if [ "$failures" -ne 0 ]; then
	echo "$failures check(s) failed"
	exit 1
fi
echo "all checks passed"
