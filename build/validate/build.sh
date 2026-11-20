#!/bin/sh
# Builds every validation binary with CGO_ENABLED=0 into one directory and writes SHA256SUMS.
#
#   build.sh [outdir]     outdir must not exist or be empty; default: a new temp directory
#
# Layout of outdir:
#   bin/koloaudio-<os>-<arch>.test[.exe]   root package tests
#   bin/opus-<os>-<arch>.test[.exe]        opus package tests
#   bin/passthrough-<os>-<arch>[.exe]      examples/passthrough
#   bin/opustone-<os>-<arch>[.exe]         examples/opustone
#   data/root/testdata, data/opus/testdata working directories the test binaries expect
#   SHA256SUMS                             sha256 of every file in bin/
#
# Prints the outdir on the last line. Platforms: darwin, linux and windows on amd64 and arm64.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out=${1:-$(mktemp -d "${TMPDIR:-/tmp}/kolo-audio-validate.XXXXXX")}
mkdir -p "$out/bin" "$out/data"
[ -z "$(ls -A "$out/bin")" ] || { echo "build.sh: $out/bin is not empty" >&2; exit 1; }

export CGO_ENABLED=0
cd "$repo"
for os in darwin linux windows; do
	for arch in arm64 amd64; do
		ext=""
		[ "$os" = windows ] && ext=.exe
		echo "== $os/$arch"
		GOOS=$os GOARCH=$arch go test -c -o "$out/bin/koloaudio-$os-$arch.test$ext" .
		GOOS=$os GOARCH=$arch go test -c -o "$out/bin/opus-$os-$arch.test$ext" ./opus
		GOOS=$os GOARCH=$arch go build -o "$out/bin/passthrough-$os-$arch$ext" ./examples/passthrough
		GOOS=$os GOARCH=$arch go build -o "$out/bin/opustone-$os-$arch$ext" ./examples/opustone
	done
done

mkdir -p "$out/data/root" "$out/data/opus"
cp -R testdata "$out/data/root/testdata"
cp -R opus/testdata "$out/data/opus/testdata"

(
	cd "$out/bin"
	if command -v sha256sum >/dev/null; then sha256sum ./* ; else shasum -a 256 ./*; fi |
		sed 's|  \./|  bin/|' >"$out/SHA256SUMS"
)
echo "$(find "$out/bin" -type f | wc -l | tr -d ' ') binaries"
cat "$out/SHA256SUMS"
echo "$out"
