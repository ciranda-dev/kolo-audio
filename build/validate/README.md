# Validation scripts

Build every test binary on one machine and run it where it belongs. Nothing here opens a microphone, plays audible sound, asks for an OS permission or binds a non-loopback address. Tests that open real devices run only with `KOLO_AUDIO_HW_TEST=1`, which no script sets.

| Script | Runs on | What it does |
|---|---|---|
| `build.sh [outdir]` | any machine with Go | Builds, with `CGO_ENABLED=0`, for darwin, linux and windows on amd64 and arm64: the root and `opus` test binaries, `examples/passthrough` and `examples/opustone`. Writes `SHA256SUMS` and the `testdata` directories the tests expect. |
| `vectors.sh <dir>` | any machine with curl | Downloads the RFC 8251 Opus vectors, checks sha256 `6b26a22f9ba87b2b836906a9bb7afec5f8e54d49553b1200382520ee6fedfa55`, prints the directory for `KOLO_OPUS_VECTORS`. |
| `check-timeouts.sh <workflow.yml>` | any machine | Fails when a CI job has no `timeout-minutes` (run by the `workflow` CI job). |
| `macos-hardened.sh` | macOS | Builds the host binaries, signs them ad hoc with the hardened runtime (`codesign -s - -f -o runtime`), enumerates devices and runs the root and `opus` tests. Prints `PASS`. |
| `windows.sh <outdir> [--vectors <dir>]` | A machine with a Windows 11 ARM64 VM (maintainers) | Runs the Windows binaries through a helper named by `KOLO_WINRUN` (its contract is in the script's header): arm64 natively, amd64 under the VM's x64 emulation. |
| `linux/run.sh [arch ...]` | Docker | PulseAudio, Alpine and PipeWire servers with a null sink; no network, no devices. |
| `linux-alsa/run.sh` | QEMU VM | A Debian arm64 kernel whose only card is `snd-aloop`. See [linux-alsa/README.md](linux-alsa/README.md). |

Run one job at a time: they are heavy.

## Expected output

| Binary | Command | Must print |
|---|---|---|
| `koloaudio-<os>-<arch>.test` | `-test.v` in a directory that holds `testdata/` | `PASS`; on Windows `TestWasapiEnumerate` and the notification tests pass, and the stream test with `KOLO_AUDIO_HW_TEST` skips |
| `opus-<os>-<arch>.test` | `-test.v` with `KOLO_OPUS_VECTORS=<dir>` | `vectors=24 range_mismatch=0 compare_fail=0` and `PASS` (about 150 s on a Mac, 180 s on the Windows VM) |
| `passthrough-<os>-<arch>` | `-list` | The device list (possibly empty) and exit status 0 |
| `opustone-<os>-<arch>` | none | `packet 25` recovered by FEC and exit status 0 |
| `macos-hardened.sh` | none | `CodeDirectory ... flags=0x10002(adhoc,runtime)` for each binary, then `PASS` |
| `linux/run.sh` | none | For every server image and arch, `PASS` with `TestPulseEnumerate`, `TestPulsePlayRecord` and `TestPulseSinkRemoved` run |

The amd64 Windows run is emulation. It shows that the binary works and says nothing about native amd64 speed or behavior. The Linux amd64 Docker run on an arm64 host is emulation too.

## Running by hand

```sh
build/validate/build.sh /tmp/kolo-validate
build/validate/vectors.sh /tmp/kolo-vectors                       # prints the vector directory
KOLO_OPUS_VECTORS=/tmp/kolo-vectors/opus_newvectors build/validate/macos-hardened.sh
KOLO_WINRUN=<helper> build/validate/windows.sh /tmp/kolo-validate --vectors /tmp/kolo-vectors/opus_newvectors
cd /tmp/kolo-validate/data/root && ../../bin/koloaudio-darwin-arm64.test -test.v   # a binary needs its testdata directory
```

`windows.sh` needs the VM started and `KOLO_WINRUN` set to the helper. The helper copies the binaries to the VM, runs them and removes them, and should lock the VM so only one job runs at a time.

The Windows VM exposes virtual sound devices, and its microphone may be backed by the host's, so no script opens a stream there: enumeration and the test binaries only.
