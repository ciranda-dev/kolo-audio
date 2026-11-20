#!/bin/sh
# Validation job for the Linux PulseAudio backend.
#
#   run.sh [arch ...]        arch is arm64 or amd64; default both
#   KOLO_VALIDATE_IMAGES="pipewire" run.sh   only these images (default: pulse alpine-pulse pipewire)
#   KOLO_VALIDATE_NOSERVER=0 run.sh          skip the runs without a sound server
#
# Builds the root package's test binary with CGO_ENABLED=0, builds the server
# images (this needs network access once, to install packages) and runs the
# binary in containers that have no network, no devices and no ports. Each
# image has a sound server whose only device is a null sink. Nothing here
# touches host audio. Run one job at a time, under the heavy lock.
#
# Must print, for each arch and image, "PASS" with TestPulseEnumerate,
# TestPulsePlayRecord and TestPulseSinkRemoved run (not skipped).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archs=${*:-arm64 amd64}
images=${KOLO_VALIDATE_IMAGES-pulse alpine-pulse pipewire}

for arch in $archs; do
	bin=$tmp/koloaudio-linux-$arch.test
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go test -c -o "$bin" .)
	file "$bin"
	file "$bin" | grep -q 'statically linked' || { echo "$bin is not static" >&2; exit 1; }
	plat=linux/$arch

	for image in $images; do
		docker build -q --platform "$plat" -f "$here/$image.Dockerfile" -t "kolo-audio-$image:$arch" "$here" >/dev/null
		case $image in pipewire) server=pipewire ;; *) server=pulse ;; esac
		echo "=== $image $plat"
		docker run --rm --network none --platform "$plat" \
			-v "$bin:/koloaudio.test:ro" "kolo-audio-$image:$arch" \
			/usr/local/bin/server.sh "$server" /koloaudio.test
	done

	[ "${KOLO_VALIDATE_NOSERVER:-1}" = 1 ] || continue
	# No server: the binary starts, the fake-server tests pass, the real-server tests skip.
	for base in debian:bookworm-slim alpine:3.22 gcr.io/distroless/static-debian12; do
		echo "=== no server on $base $plat"
		docker run --rm --network none --platform "$plat" \
			-v "$bin:/koloaudio.test:ro" "$base" \
			/koloaudio.test -test.count=1 -test.run 'Pulse|Backend'
	done
done
