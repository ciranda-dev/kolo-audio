#!/bin/sh
# Checks the committed artifacts: wasm hashes, generated Go and licence notices.
# Usage: verify.sh [--rebuild]   (--rebuild also rebuilds every wasm in Docker and compares hashes)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sumcheck() {
	if command -v sha256sum >/dev/null; then sha256sum -c "$1"; else shasum -a 256 -c "$1"; fi
}

for d in "$here"/*/; do
	[ -f "$d/SHA256SUMS" ] || continue
	(cd "$d" && sumcheck SHA256SUMS)
done

"$here/regen.sh" "$tmp/regen"
(cd "$tmp/regen" && find . -type f) | while read -r f; do
	cmp "$tmp/regen/$f" "$root/$f" || { echo "verify.sh: $f differs from regen.sh output" >&2; exit 1; }
	head -n 1 "$root/$f" | grep -Eq '^// Code generated .* DO NOT EDIT\.$' || { echo "verify.sh: $f has no generated-code header" >&2; exit 1; }
	echo "$f: matches regen.sh"
done

lic=$root/third_party/licenses
for f in LICENSE-libopus LICENSE-wasi-libc LICENSE-wasm2go LICENSE-miniaudio; do
	[ -s "$lic/$f" ] || { echo "verify.sh: $f is missing" >&2; exit 1; }
done
for f in LICENSE-libopus LICENSE-wasi-libc LICENSE-wasm2go; do
	cmp "$lic/$f" "$root/opus/internal/libopus/$f"
done
for f in LICENSE-miniaudio LICENSE-wasi-libc LICENSE-wasm2go; do
	cmp "$lic/$f" "$root/internal/maresample/$f"
done

if [ "${1:-}" = --rebuild ]; then
	for d in "$here"/*/; do
		[ -f "$d/build.sh" ] && [ -f "$d/SHA256SUMS" ] || continue
		"$d/build.sh" "$tmp/build/$(basename "$d")"
		cmp "$d/SHA256SUMS" "$tmp/build/$(basename "$d")/SHA256SUMS"
		echo "$(basename "$d"): rebuild matches SHA256SUMS"
	done
	# The build fetched the pinned header; its licence block is LICENSE-miniaudio, without the closing "*/".
	ma=${KOLO_AUDIO_BUILD_CACHE:-${TMPDIR:-/tmp}/kolo-audio-build-cache}/miniaudio-$(sed -n 's/^MA_VERSION=//p' "$here/maresample/build.sh").h
	start=$(grep -n '^This software is available as a choice' "$ma" | tail -1 | cut -d: -f1)
	sed -n "$start,\$p" "$ma" | sed '$d' | cmp - "$lic/LICENSE-miniaudio"
	echo "LICENSE-miniaudio: matches the pinned header"
fi
echo "verify.sh: ok"
