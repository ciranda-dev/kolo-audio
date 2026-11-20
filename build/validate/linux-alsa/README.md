# ALSA kernel backend validation

The direct ALSA backend talks to the kernel through `/dev/snd`. Docker
Desktop's kernel has no sound modules, so this job uses a Debian arm64 VM
whose only sound card is the kernel's `snd-aloop` loopback. The VM gets no
sound device from the host and no outbound network.

Requirements on the Mac: Homebrew `qemu` (`qemu-system-aarch64`, `qemu-img`,
the edk2 firmware in `share/qemu`), `hdiutil`, `ssh`, `curl`, Go.

```sh
build/validate/linux-alsa/vm.sh fetch   # Debian 13 generic arm64 image 20261111-2659, SHA512 checked against SHA512SUMS
build/validate/linux-alsa/vm.sh seed    # throwaway SSH key and cloud-init seed ISO
build/validate/linux-alsa/vm.sh start   # QEMU hvf, 2 CPUs, 2 GiB; SSH on 127.0.0.1:2222
build/validate/linux-alsa/run.sh        # build the test binary, run the three steps in the VM
build/validate/linux-alsa/vm.sh stop
```

Files: `${TMPDIR:-/tmp}/kolo-linux-vm/` (override with
`KOLO_VM_DIR`): the base image, `disk.qcow2` (overlay on the base image),
`vars.fd`, `seed.iso`, `id_ed25519` (key for user `kolo`, used only for this
VM), `known_hosts`, `serial.log`, `vm.pid`. `vm.sh reset` deletes the overlay
so the next start boots a fresh VM.

The image's stock kernel (`linux-image-arm64`) already has `snd-aloop`;
cloud-init loads it and lists it in `/etc/modules-load.d/`. The user `kolo`
is in the `audio` group, so the tests run without root; only the removal
step uses `sudo` to unbind the driver.

`run.sh` must print `PASS` for the fake-layer tests and the whole package,
`PASS` for `TestALSARealEnumerate`, `TestALSARealNegotiateMatchesFake` and
the `TestALSARealLoopback` cases, and `--- PASS: TestALSARealRemoval`.
`modprobe -r snd-aloop` is refused while a PCM is open (the kernel holds a
module reference per open PCM), so the removal step unbinds the platform
device instead, which disconnects the card the way an unplugged USB device
does.
