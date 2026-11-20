#!/bin/sh
# Debian arm64 VM with snd-aloop for the ALSA kernel backend.
#
#   vm.sh fetch    download the pinned Debian cloud image and check its SHA512
#   vm.sh seed     make a throwaway SSH key and the cloud-init seed ISO
#   vm.sh start    boot the VM (QEMU with hvf, 2 CPUs, 2 GiB) and wait for SSH
#   vm.sh ssh ...  run a command in the VM as user kolo
#   vm.sh put f    copy a file to the VM home directory
#   vm.sh stop     power the VM off and wait for QEMU to exit
#   vm.sh reset    delete the VM disk overlay (the next start boots a fresh VM)
#
# Every file lives in $KOLO_VM_DIR (default ${TMPDIR:-/tmp}/kolo-linux-vm).
# The VM has no sound device from the host: no -audiodev, no sound card. Its
# only sound card is the kernel's snd-aloop loopback. Networking is QEMU user
# mode restricted to the guest (no outbound access); SSH is forwarded from
# 127.0.0.1:$KOLO_VM_PORT (default 2222) only.
set -eu

dir=${KOLO_VM_DIR:-${TMPDIR:-/tmp}/kolo-linux-vm}
port=${KOLO_VM_PORT:-2222}
build=20261111-2659
image=debian-13-generic-arm64-$build.qcow2
url=https://cloud.debian.org/images/cloud/trixie/$build
qemu=${KOLO_VM_QEMU:-qemu-system-aarch64}
firmware=${KOLO_VM_FIRMWARE:-$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu/edk2-aarch64-code.fd}
vars=$(dirname "$firmware")/edk2-arm-vars.fd

sshvm() {
	ssh -F /dev/null -i "$dir/id_ed25519" -p "$port" \
		-o UserKnownHostsFile="$dir/known_hosts" -o StrictHostKeyChecking=accept-new \
		-o ConnectTimeout=5 -o BatchMode=yes kolo@127.0.0.1 "$@"
}

case ${1:-} in
fetch)
	mkdir -p "$dir"
	cd "$dir"
	curl -fsSL -o SHA512SUMS "$url/SHA512SUMS"
	[ -f "$image" ] || curl -fL -o "$image.part" "$url/$image"
	[ -f "$image" ] || mv "$image.part" "$image"
	want=$(awk -v f="$image" '$2 == f { print $1 }' SHA512SUMS)
	got=$(shasum -a 512 "$image" | awk '{ print $1 }')
	[ -n "$want" ] && [ "$want" = "$got" ] || { echo "SHA512 mismatch for $image" >&2; rm -f "$image"; exit 1; }
	echo "$image sha512 $got (matches SHA512SUMS)"
	;;
seed)
	mkdir -p "$dir/seed"
	cd "$dir"
	[ -f id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -C kolo-linux-vm -f id_ed25519
	cat >seed/meta-data <<-EOF
	instance-id: kolo-alsa-1
	local-hostname: kolo-alsa
	EOF
	cat >seed/user-data <<-EOF
	#cloud-config
	users:
	  - name: kolo
	    groups: audio
	    sudo: ALL=(ALL) NOPASSWD:ALL
	    shell: /bin/bash
	    ssh_authorized_keys:
	      - $(cat id_ed25519.pub)
	ssh_pwauth: false
	write_files:
	  - path: /etc/modules-load.d/snd-aloop.conf
	    content: "snd-aloop\n"
	runcmd:
	  - [modprobe, snd-aloop]
	EOF
	rm -f seed.iso
	hdiutil makehybrid -quiet -iso -joliet -default-volume-name cidata -o seed.iso seed
	echo "$dir/seed.iso"
	;;
start)
	cd "$dir"
	[ -f "$image" ] && [ -f seed.iso ] || { echo "run fetch and seed first" >&2; exit 1; }
	if [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null; then echo "already running"; exit 0; fi
	[ -f disk.qcow2 ] || qemu-img create -q -f qcow2 -F qcow2 -b "$dir/$image" disk.qcow2 8G
	[ -f vars.fd ] || cp "$vars" vars.fd
	"$qemu" -name kolo-alsa -machine virt -accel hvf -cpu host -smp 2 -m 2048 \
		-drive if=pflash,format=raw,readonly=on,file="$firmware" \
		-drive if=pflash,format=raw,file=vars.fd \
		-drive if=virtio,format=qcow2,file=disk.qcow2 \
		-drive if=virtio,format=raw,readonly=on,file=seed.iso \
		-nic user,model=virtio-net-pci,restrict=on,hostfwd=tcp:127.0.0.1:"$port"-:22 \
		-display none -serial file:serial.log -daemonize -pidfile vm.pid
	i=0
	until sshvm true 2>/dev/null; do
		i=$((i + 1))
		[ $i -lt 120 ] || { echo "no SSH after 600 s; see $dir/serial.log" >&2; exit 1; }
		sleep 5
	done
	sshvm 'until [ -e /dev/snd/controlC0 ]; do sleep 1; done; uname -r; cat /proc/asound/cards'
	;;
ssh)
	shift
	sshvm "$@"
	;;
put)
	scp -F /dev/null -q -i "$dir/id_ed25519" -P "$port" \
		-o UserKnownHostsFile="$dir/known_hosts" -o StrictHostKeyChecking=accept-new \
		-o BatchMode=yes "$2" kolo@127.0.0.1:
	;;
stop)
	cd "$dir"
	[ -f vm.pid ] || exit 0
	pid=$(cat vm.pid)
	sshvm 'sudo systemctl poweroff' 2>/dev/null || true
	i=0
	while kill -0 "$pid" 2>/dev/null; do
		i=$((i + 1))
		[ $i -lt 60 ] || { kill "$pid"; break; }
		sleep 1
	done
	rm -f vm.pid
	echo stopped
	;;
reset)
	rm -f "$dir/disk.qcow2" "$dir/vars.fd" "$dir/known_hosts"
	;;
*)
	sed -n '2,15p' "$0" >&2
	exit 2
	;;
esac
