#!/bin/bash
# Downloads the audio fixture of the smoke test into this directory and
# checks it against a pinned SHA-256. Audio files are never committed (see
# CLAUDE.md), so the clip is fetched before the test instead; the test itself
# still runs with networking disabled.
#
# jfk.mp3: 11 seconds of John F. Kennedy's 1961 inaugural address, a work of
# the US federal government and so in the public domain, as shipped in
# whisper.cpp's samples at a pinned commit.
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
url=https://raw.githubusercontent.com/ggml-org/whisper.cpp/aba02defd6bb6ff5de63721c21e3456645c153e2/samples/jfk.mp3
sha256=20d3323a2bcce6f25498b8911a397503a0a99fa92b6ba58d62788cb42b6e5459
out="$dir/jfk.mp3"

# check succeeds when file $1 has the pinned SHA-256. It uses sha256sum where
# it exists (Linux) and shasum otherwise (macOS).
check() {
	local sum
	if command -v sha256sum >/dev/null; then
		sum=$(sha256sum "$1")
	else
		sum=$(shasum -a 256 "$1")
	fi
	[ "${sum%% *}" = "$sha256" ]
}

if [ -f "$out" ] && check "$out" 2>/dev/null; then
	echo "jfk.mp3 already present"
	exit 0
fi
tmp=$(mktemp "$dir/.jfk.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL --retry 5 --retry-all-errors -o "$tmp" "$url"
check "$tmp" || { echo "jfk.mp3: checksum mismatch" >&2; exit 1; }
mv "$tmp" "$out"
trap - EXIT
echo "fetched jfk.mp3"
