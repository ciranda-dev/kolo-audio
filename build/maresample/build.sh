#!/bin/sh
# Rebuilds maresample.wasm and SHA256SUMS in a throwaway container. Usage: build.sh [outdir]
set -eu

MA_VERSION=0.11.26
MA_SHA256=6b376c7fbb1352ba1e0e432ab66069b335ea9405e45cb6c30b97ef0f3c9c3e0e
MA_URL=https://raw.githubusercontent.com/mackron/miniaudio/$MA_VERSION/miniaudio.h
SDK=wasi-sdk-34.0-arm64-linux
SDK_SHA256=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971
SDK_URL=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/$SDK.tar.gz
IMAGE=debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
STACK=4096
MAX_MEMORY=524288

if [ "${1:-}" = --inner ]; then
	echo "$MA_SHA256  /cache/miniaudio-$MA_VERSION.h" | sha256sum -c -
	echo "$SDK_SHA256  /cache/$SDK.tar.gz" | sha256sum -c -
	w=/tmp/build
	rm -rf "$w"
	mkdir -p "$w"
	cd "$w"
	tar xzf "/cache/$SDK.tar.gz"
	cp "/cache/miniaudio-$MA_VERSION.h" miniaudio.h
	exports=$(sed 's/^/-Wl,--export=/' /in/exports.txt)
	# shellcheck disable=SC2086
	"$w/$SDK/bin/clang" --target=wasm32-wasip1 -mexec-model=reactor -O2 -I "$w" \
		/in/rs.c $exports -Wl,--strip-all -Wl,-z,stack-size=$STACK -Wl,--max-memory=$MAX_MEMORY \
		-o /out/maresample.wasm
	chmod 644 /out/maresample.wasm
	"$w/$SDK/bin/clang" --version | head -1
	(cd /out && sha256sum maresample.wasm >SHA256SUMS && cat SHA256SUMS)
	exit 0
fi

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cache=${KOLO_AUDIO_BUILD_CACHE:-${TMPDIR:-/tmp}/kolo-audio-build-cache}
mkdir -p "$cache"

hostsha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
fetch() {
	[ "$(hostsha "$cache/$3" 2>/dev/null || true)" = "$2" ] && return
	curl -fsSL -o "$cache/$3.part" "$1"
	[ "$(hostsha "$cache/$3.part")" = "$2" ] || { rm -f "$cache/$3.part"; echo "build.sh: checksum mismatch for $1" >&2; exit 1; }
	mv "$cache/$3.part" "$cache/$3"
}
fetch "$MA_URL" "$MA_SHA256" "miniaudio-$MA_VERSION.h"
fetch "$SDK_URL" "$SDK_SHA256" "$SDK.tar.gz"

exec docker run --rm --platform linux/arm64 --network none --user "$(id -u):$(id -g)" \
	-v "$here:/in:ro" -v "$cache:/cache:ro" -v "$out:/out" "$IMAGE" sh /in/build.sh --inner
