#!/bin/sh
# Builds the host-architecture test binaries, signs them ad hoc with the hardened runtime and runs them.
#
#   macos-hardened.sh
#   KOLO_OPUS_VECTORS=<dir> macos-hardened.sh   also runs the RFC 8251 conformance test (about 150 s)
#
# Only enumeration happens: no stream is opened, no microphone is read, no permission is requested.
# KOLO_AUDIO_HW_TEST is removed from the environment. Prints PASS at the end, or exits non-zero.
set -eu

[ "$(uname -s)" = Darwin ] || { echo "macos-hardened.sh: macOS only" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kolo-audio-hardened.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

export CGO_ENABLED=0
(cd "$repo" &&
	go test -c -o "$tmp/koloaudio.test" . &&
	go test -c -o "$tmp/opus.test" ./opus &&
	go build -o "$tmp/passthrough" ./examples/passthrough)
mkdir -p "$tmp/root" "$tmp/opus"
cp -R "$repo/testdata" "$tmp/root/testdata"
cp -R "$repo/opus/testdata" "$tmp/opus/testdata"

for b in koloaudio.test opus.test passthrough; do
	codesign -s - -f -o runtime "$tmp/$b"
	codesign -dv "$tmp/$b" 2>&1 | grep -E '^CodeDirectory' | grep -q 'flags=0x[0-9a-f]*(adhoc,runtime)' ||
		{ echo "macos-hardened.sh: $b is not signed with the hardened runtime" >&2; exit 1; }
	codesign -dv "$tmp/$b" 2>&1 | grep -E '^CodeDirectory'
done

# run <name> <dir> <binary> <grep pattern for the lines to show>
run() {
	name=$1 dir=$2 bin=$3 show=$4
	if ! (cd "$dir" && env -u KOLO_AUDIO_HW_TEST "$bin" -test.v -test.count=1 >"$tmp/$name.log" 2>&1); then
		tail -n 40 "$tmp/$name.log"
		echo "macos-hardened.sh: $name failed" >&2
		exit 1
	fi
	grep -E "$show" "$tmp/$name.log"
	echo "$name: $(grep -c '^--- PASS' "$tmp/$name.log") passed, $(grep -c '^--- SKIP' "$tmp/$name.log") skipped"
}

echo "== enumeration (passthrough -list)"
env -u KOLO_AUDIO_HW_TEST "$tmp/passthrough" -list

echo "== root package"
run root "$tmp/root" "$tmp/koloaudio.test" '^--- PASS: TestCoreAudio(Enumeration|Listeners)|^PASS$'

echo "== opus package"
KOLO_OPUS_VECTORS=${KOLO_OPUS_VECTORS:-}
export KOLO_OPUS_VECTORS
run opus "$tmp/opus" "$tmp/opus.test" 'vectors=[0-9]+ range_mismatch|^--- (PASS|SKIP): TestConformance|^PASS$'
[ -n "$KOLO_OPUS_VECTORS" ] || echo "conformance skipped: KOLO_OPUS_VECTORS is not set"
echo PASS
