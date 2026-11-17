#!/bin/sh
# Download one file and check its SHA-256 before keeping it.
# Used only while building deploy/clipper-base.Dockerfile.
#
#   fetch.sh <url> <sha256> <destination path>
set -eu

if [ "$#" -ne 3 ]; then
	echo "usage: fetch.sh <url> <sha256> <destination>" >&2
	exit 2
fi
url=$1
sum=$2
dest=$3

mkdir -p "$(dirname "$dest")"
curl --fail --silent --show-error --location \
	--retry 5 --retry-all-errors --connect-timeout 30 \
	--output "$dest.part" "$url"
if ! echo "$sum  $dest.part" | sha256sum --check --status -; then
	echo "fetch.sh: checksum mismatch for $url" >&2
	echo "  expected $sum" >&2
	echo "  got      $(sha256sum "$dest.part" | cut -d' ' -f1)" >&2
	rm -f "$dest.part"
	exit 1
fi
mv "$dest.part" "$dest"
echo "fetched $dest ($(wc -c <"$dest") bytes)"
