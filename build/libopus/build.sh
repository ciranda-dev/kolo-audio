#!/bin/sh
# Rebuilds libopus.wasm and SHA256SUMS in a throwaway container. Usage: build.sh [outdir]
set -eu

OPUS_VERSION=1.6.1
OPUS_SHA256=6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1
OPUS_URL=https://downloads.xiph.org/releases/opus/opus-$OPUS_VERSION.tar.gz
SDK=wasi-sdk-34.0-arm64-linux
SDK_SHA256=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971
SDK_URL=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/$SDK.tar.gz
IMAGE=debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
STACK=262144
MAXMEM=1048576
SOURCES=137

if [ "${1:-}" = --inner ]; then
	echo "$OPUS_SHA256  /cache/opus-$OPUS_VERSION.tar.gz" | sha256sum -c -
	echo "$SDK_SHA256  /cache/$SDK.tar.gz" | sha256sum -c -
	w=/tmp/build
	rm -rf "$w"
	mkdir -p "$w/cfg"
	cd "$w"
	tar xzf "/cache/opus-$OPUS_VERSION.tar.gz"
	tar xzf "/cache/$SDK.tar.gz"
	echo "#define PACKAGE_VERSION \"$OPUS_VERSION\"" >cfg/config.h
	cd "opus-$OPUS_VERSION"
	mk() { sed -n "/^$2 = /,/^\$/p" "$1" | grep -o '[A-Za-z_0-9/]*\.c'; }
	srcs=$( { mk celt_sources.mk CELT_SOURCES; mk silk_sources.mk SILK_SOURCES; mk silk_sources.mk SILK_SOURCES_FLOAT
		mk opus_sources.mk OPUS_SOURCES; mk opus_sources.mk OPUS_SOURCES_FLOAT; } )
	n=$(echo "$srcs" | wc -l)
	[ "$n" -eq "$SOURCES" ] || { echo "build.sh: $n sources, want $SOURCES" >&2; exit 1; }
	exports=$(sed 's/^/-Wl,--export=/' /in/exports.txt)
	# shellcheck disable=SC2086
	"$w/$SDK/bin/clang" --target=wasm32-wasip1 -mexec-model=reactor -O2 -std=gnu99 \
		-DNDEBUG -DDISABLE_DEBUG_FLOAT -DENABLE_HARDENING -DHAVE_ALLOCA_H -DHAVE_CONFIG_H \
		-DHAVE_LRINT -DHAVE_LRINTF -DOPUS_BUILD -DVAR_ARRAYS \
		-I include -I "$w/cfg" -I . -I dnn -I celt -I silk -I silk/float \
		$srcs /in/shim.c $exports -Wl,--strip-all -Wl,-z,stack-size=$STACK -Wl,--max-memory=$MAXMEM -o /out/libopus.wasm
	chmod 644 /out/libopus.wasm
	"$w/$SDK/bin/clang" --version | head -1
	(cd /out && sha256sum libopus.wasm >SHA256SUMS && cat SHA256SUMS)
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
fetch "$OPUS_URL" "$OPUS_SHA256" "opus-$OPUS_VERSION.tar.gz"
fetch "$SDK_URL" "$SDK_SHA256" "$SDK.tar.gz"

exec docker run --rm --platform linux/arm64 --network none --user "$(id -u):$(id -g)" \
	-v "$here:/in:ro" -v "$cache:/cache:ro" -v "$out:/out" "$IMAGE" sh /in/build.sh --inner
