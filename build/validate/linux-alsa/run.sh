#!/bin/sh
# Validation job for the ALSA kernel backend on a real Linux kernel.
#
#   run.sh        build the linux/arm64 test binary, run it in the VM, print the results
#
# Needs a running VM (vm.sh fetch, vm.sh seed, vm.sh start). The VM's only
# sound card is snd-aloop, so no real audio device is opened. Steps:
#   1. the fake-layer ALSA tests and the whole root package on the VM kernel
#   2. KOLO_AUDIO_ALSA_TEST=1: enumeration, negotiation against the fake
#      kernel model, and tones through the loopback in several formats
#   3. removal: streams run while the snd-aloop driver is unbound; both must
#      fail with ErrDeviceLost. Then the module is loaded again.
# Must print "PASS" for steps 1 and 2 and "--- PASS: TestALSARealRemoval".
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
vm=$here/vm.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

bin=$tmp/koloaudio-linux-arm64.test
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o "$bin" .)
file "$bin" | grep -q 'statically linked' || { echo "$bin is not static" >&2; exit 1; }
shasum -a 256 "$bin"
"$vm" put "$bin"
(cd "$repo" && tar cf - testdata) | "$vm" ssh 'rm -rf testdata && tar xf -'
t=./koloaudio-linux-arm64.test

echo "=== VM"
"$vm" ssh 'uname -srvm; id; cat /proc/asound/cards; ls -l /dev/snd'

echo "=== 1. fake-layer tests and the whole package on the VM kernel"
"$vm" ssh "$t -test.count=1 -test.run 'ALSA|Backend' && $t -test.count=1"

echo "=== 2. real kernel: enumeration, negotiation, loopback tones"
"$vm" ssh "KOLO_AUDIO_ALSA_TEST=1 $t -test.count=1 -test.v -test.run 'TestALSAReal(Enumerate|NegotiateMatchesFake|Loopback)\$'"

echo "=== 3. real kernel: card removal while streams run"
"$vm" ssh "rm -f removal.log; KOLO_AUDIO_ALSA_TEST=1 KOLO_AUDIO_ALSA_REMOVAL=1 nohup $t -test.count=1 -test.v -test.run 'TestALSARealRemoval\$' >removal.log 2>&1 & echo started"
"$vm" ssh 'i=0; until grep -q "kolo-alsa: streams running" removal.log; do i=$((i+1)); [ $i -lt 100 ] || exit 1; sleep 0.1; done; echo streams running'
"$vm" ssh 'sudo modprobe -r snd-aloop 2>&1 && echo "unexpected: unloaded while open" || echo "modprobe -r refused while the PCMs are open (expected: the module is in use)"'
"$vm" ssh 'ls /sys/bus/platform/drivers/snd_aloop/ | grep snd_aloop; (echo snd_aloop.0 | sudo tee /sys/bus/platform/drivers/snd_aloop/unbind >/dev/null &) ; echo unbind requested'
"$vm" ssh 'i=0; while pgrep -f "^./koloaudio-linux-arm64.test" >/dev/null; do i=$((i+1)); [ $i -lt 300 ] || exit 1; sleep 0.2; done; cat removal.log'
"$vm" ssh 'sleep 1; sudo modprobe -r snd-aloop && sudo modprobe snd-aloop && cat /proc/asound/cards'
"$vm" ssh 'grep -q "^--- PASS: TestALSARealRemoval" removal.log'
echo "validation done"
