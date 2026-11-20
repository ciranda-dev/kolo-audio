#!/bin/sh
# Runs the Windows binaries of build.sh in the Windows 11 ARM64 VM through the winrun.sh helper.
#
#   windows.sh <build.sh outdir> [--vectors <dir>] [arm64] [amd64]
#
# arm64 runs natively; amd64 runs under the VM's x64 emulation, so it shows that the binary works
# and says nothing about native amd64 behavior or speed. With --vectors (the RFC 8251 files, see
# vectors.sh) the opus conformance test runs too (about 3 minutes natively). Streams are never
# opened: the VM's microphone may be backed by the host's. Enumeration and the tests only.
#
# This script is for a maintainer with a Windows VM. KOLO_WINRUN must name a helper with this contract:
#   <helper> <exe-or-dir> [args...]   copies the file (or the directory, then EXE=<file in it> names the
#                                     program) to the VM, runs it with the arguments, prints its output,
#                                     exits with its status, and removes the copy. WINENV="A=1;B=2" sets
#                                     environment variables for the run. The helper locks the VM itself.
set -eu

winrun=${KOLO_WINRUN:?set KOLO_WINRUN to the helper that runs a Windows program in the VM (see the comments of this script)}
[ -x "$winrun" ] || { echo "windows.sh: $winrun not found" >&2; exit 1; }
out=${1:?usage: windows.sh <build.sh outdir> [--vectors <dir>] [arm64] [amd64]}
shift
vectors=""
if [ "${1:-}" = --vectors ]; then
	vectors=${2:?--vectors needs a directory}
	shift 2
fi
archs=${*:-arm64 amd64}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kolo-audio-windows.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# runwin <log name> <dir> <exe> [args...]: runs one binary in the VM, keeps its output in the log, stops on failure.
runwin() {
	log=$tmp/$1.log
	dir=$2
	exe=$3
	shift 3
	if ! EXE=$exe "$winrun" "$dir" "$@" >"$log" 2>&1; then
		tr -d '\r' <"$log" | tail -n 40
		echo "windows.sh: $exe failed" >&2
		exit 1
	fi
	tr -d '\r' <"$log" | grep -v '^Connection to ' >"$log.txt"
}

summary() {
	echo "$1: $(grep -c '^--- PASS' "$tmp/$2.log.txt") passed, $(grep -c '^--- SKIP' "$tmp/$2.log.txt") skipped, $(grep -c '^--- FAIL' "$tmp/$2.log.txt") failed"
}

for arch in $archs; do
	case $arch in
	arm64) how=native ;;
	amd64) how=emulated ;;
	*) echo "windows.sh: unknown arch $arch" >&2; exit 1 ;;
	esac
	echo "=== windows/$arch ($how)"

	d=$tmp/root-$arch
	mkdir -p "$d"
	cp "$out/bin/koloaudio-windows-$arch.test.exe" "$out/bin/passthrough-windows-$arch.exe" "$out/bin/opustone-windows-$arch.exe" "$d/"
	cp -R "$out/data/root/testdata" "$d/testdata"
	echo "-- passthrough -list"
	runwin "list-$arch" "$d" "passthrough-windows-$arch.exe" -list
	cat "$tmp/list-$arch.log.txt"
	echo "-- opustone"
	runwin "tone-$arch" "$d" "opustone-windows-$arch.exe"
	cat "$tmp/tone-$arch.log.txt"
	echo "-- root package tests"
	runwin "root-$arch" "$d" "koloaudio-windows-$arch.test.exe" -test.v -test.count=1
	grep -E '^--- PASS: Test(Wasapi(Enumerate|Notification|Vtable)|ResamplerGolden)|^PASS$' "$tmp/root-$arch.log.txt" || true
	summary root "root-$arch"

	d=$tmp/opus-$arch
	mkdir -p "$d"
	cp "$out/bin/opus-windows-$arch.test.exe" "$d/"
	cp -R "$out/data/opus/testdata" "$d/testdata"
	if [ -n "$vectors" ]; then
		cp -R "$vectors" "$d/vectors"
		WINENV="KOLO_OPUS_VECTORS=vectors"
		export WINENV
		runwin "opus-$arch" "$d" "opus-windows-$arch.test.exe" -test.v -test.count=1
		unset WINENV
	else
		runwin "opus-$arch" "$d" "opus-windows-$arch.test.exe" -test.v -test.count=1
	fi
	grep -E 'vectors=[0-9]+ range_mismatch|^--- (PASS|SKIP): Test(Conformance|Golden|Version)|^PASS$' "$tmp/opus-$arch.log.txt"
	summary opus "opus-$arch"
	echo "PASS windows/$arch ($how)"
done
