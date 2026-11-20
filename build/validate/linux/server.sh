#!/bin/sh
# Runs inside a container as an unprivileged user.
#   server.sh pulse|pipewire <test binary>
# Starts a sound server whose only device is a null sink, then runs the Pulse
# tests of the binary against it. PulseAudio is tested over tcp4 on 127.0.0.1
# with cookie authentication, then over its unix socket with credentials.
set -eu
kind=$1
bin=$2
export HOME=/home/probe
export XDG_RUNTIME_DIR=/tmp/xdg
sock=$XDG_RUNTIME_DIR/pulse/native
log=$XDG_RUNTIME_DIR

wait_for_socket() {
	i=0
	while [ ! -S "$1" ]; do
		i=$((i + 1))
		if [ "$i" -gt 300 ]; then
			echo "no server socket at $1" >&2
			tail -n 40 "$log"/*.log >&2 || true
			exit 1
		fi
		sleep 0.1
	done
}

run_tests() {
	PULSE_SERVER=$1 KOLO_AUDIO_PULSE_TEST=1 "$bin" -test.v -test.count=1 -test.run "$2"
}

case $kind in
pulse)
	rm -rf "$XDG_RUNTIME_DIR"
	mkdir -p -m 700 "$XDG_RUNTIME_DIR/pulse"
	pulseaudio -n --daemonize=no --exit-idle-time=-1 --log-target=stderr \
		-L "module-native-protocol-unix socket=$sock" \
		-L "module-native-protocol-tcp listen=127.0.0.1 port=4713" \
		-L "module-null-sink sink_name=kolo_null rate=48000 channels=1" \
		>"$log/pulseaudio.log" 2>&1 &
	wait_for_socket "$sock"
	echo "== tcp4 127.0.0.1, cookie authentication"
	run_tests tcp4:127.0.0.1:4713 'TestPulseEnumerate|TestPulsePlayRecord'
	echo "== unix socket, credentials authentication"
	run_tests "unix:$sock" 'Pulse|Backend'
	;;
pipewire)
	rm -rf "$XDG_RUNTIME_DIR"
	mkdir -p -m 700 "$XDG_RUNTIME_DIR"
	exec dbus-run-session -- "$0" pipewire-session "$bin"
	;;
pipewire-session)
	pipewire >"$log/pipewire.log" 2>&1 &
	wait_for_socket "$XDG_RUNTIME_DIR/pipewire-0"
	wireplumber >"$log/wireplumber.log" 2>&1 &
	sleep 3
	pipewire-pulse >"$log/pipewire-pulse.log" 2>&1 &
	wait_for_socket "$sock"
	sleep 2
	export PULSE_SERVER=unix:$sock
	pactl info | head -n 8
	pactl load-module module-null-sink sink_name=kolo_null rate=48000 channels=1
	sleep 2
	pactl list short sinks
	pactl list short sources
	wpctl status || true
	tail -n 15 "$log/wireplumber.log"
	echo "== stock client: record 1 s from the monitor"
	timeout 10 parec --device=kolo_null.monitor --raw --format=s16le --rate=48000 --channels=1 | head -c 96000 | wc -c || true
	run_tests "unix:$sock" 'Pulse|Backend' || {
		status=$?
		tail -n 30 "$log"/*.log
		exit $status
	}
	;;
esac
