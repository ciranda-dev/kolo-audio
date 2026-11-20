#!/bin/sh
# Regenerates the golden outputs from the native miniaudio resampler. Needs a C compiler and network.
# Usage: gen.sh   (fetches the pinned miniaudio.h into a temp directory; writes next to this script)
set -eu

VERSION=0.11.26
SHA256=6b376c7fbb1352ba1e0e432ab66069b335ea9405e45cb6c30b97ef0f3c9c3e0e
URL=https://raw.githubusercontent.com/mackron/miniaudio/$VERSION/miniaudio.h

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/miniaudio.h" "$URL"
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/miniaudio.h"); else got=$(shasum -a 256 "$tmp/miniaudio.h"); fi
[ "${got%% *}" = "$SHA256" ] || { echo "gen.sh: checksum mismatch for $URL" >&2; exit 1; }

cd "$here/../.."
CGO_ENABLED=1 CGO_CFLAGS="-I$tmp" KOLO_AUDIO_GEN_GOLDEN=1 go run ./testdata/resampler/gen -out "$here"
