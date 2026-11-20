# Platforms

What runs where, and how each platform was validated. "Run" means a test binary executed on that platform; "cross-compiled" means it builds and passes `go vet` and was not executed.

kolo-audio has no cgo in any package, so the same `go get` and `go build` work on every platform.

`CGO_ENABLED=0` always works and gives static Linux binaries. Go turns cgo off by itself when no C compiler is on `PATH`, so a clean machine needs nothing. With cgo on, a working C compiler is needed on macOS (purego imports `runtime/cgo`) and on Linux (Go's `net` package does), and a Linux binary links libc dynamically. Windows needs no compiler either way.

## Matrix

| Platform | Device backend | Validated by running | Only cross-compiled | Not validated |
|---|---|---|---|---|
| macOS arm64 | CoreAudio through purego | `go test` and `go test -race`; enumeration of the Mac's real devices (7 capture, 4 playback, same list as the previous cgo build, IDs identical); listener registration on the system object and on every device; tests with an ad-hoc signed hardened-runtime binary; with `KOLO_AUDIO_HW_TEST=1`, on one Mac (Apple M5), streams on the built-in default devices: playback of digital silence and capture at 16000 Hz mono, 44100 Hz stereo and 48000 Hz mono (frame counts on time, no overruns or underruns, captured level above digital silence), also under `-race` | | Audible playback; Bluetooth, USB and other devices; reroute, device loss, the permission error on a real denial |
| macOS amd64 | same | The test binary under Rosetta: pass, same device list | | A native Intel Mac; streams |
| Windows arm64 | WASAPI and `IMMNotificationClient` through COM, `golang.org/x/sys/windows` | Native, in a Windows 11 ARM64 VM that exposes virtual sound devices (no stream was opened on them): the whole root package, the Opus tests with the 24 RFC 8251 conformance cases, enumeration of the VM's 4 capture and 1 playback endpoints, the notification client (registration, calls of every vtable entry, 2500 registrations), the default playback endpoint's period read | | Initializing or starting a stream on any endpoint; a notification from a real device change; Windows Server |
| Windows amd64 | same | The same tests, including the 24 conformance cases, under the VM's x64 emulation: they show that the binary works, not native behavior or speed | | A native amd64 machine; streams |
| Linux, PulseAudio protocol | Own client of the PulseAudio native protocol over its socket | Real servers in Docker, with a null sink only and no devices: PulseAudio 16.1 on Debian and on Alpine, PipeWire 0.3.65 with WirePlumber and its pulse server. linux/arm64 natively; linux/amd64 under emulation. Enumeration, 1 s of tone played and recorded from the monitor, following a new default sink, sink removal reported as `ErrDeviceLost`, over `tcp4` with a cookie and over the unix socket. The static binary also starts on Debian, Alpine and distroless without a server | | Real sound hardware behind a server; other PipeWire versions; a real suspend |
| Linux, ALSA kernel interface | `ioctl` on `/dev/snd` through `golang.org/x/sys/unix` | Real Linux kernel 6.12.116 (Debian 13 arm64, QEMU VM) whose only sound card is `snd-aloop`: enumeration, 240 negotiations equal to the test fake, a 997 Hz tone played into the loopback and recorded from the other side in 8 format cases, and device removal | | A real sound card or USB device; linux/amd64 on a real kernel; suspend and resume; devices that offer only non-interleaved access |
| Any platform | Null device | The tests use a timer-driven virtual device and run on every platform above | | |
| linux/386 | none: `ErrUnsupported` for devices | The root, `opus` and internal test suites as a static 386 binary under emulation (Docker): 0 failures, only the opt-in hardware tests skipped (and the conformance test unless vectors are given); the Opus golden output is the same as on 64-bit | | Real 32-bit hardware |
| linux/riscv64, other GOOS | none: `ErrUnsupported` for devices | | `go vet` and builds; the resampler and Opus work | Running anything on them |

The Opus encoder output was checked bit for bit on darwin/arm64, darwin/amd64 (Rosetta), linux/arm64 and linux/amd64 (emulated) through `opus/testdata/golden.txt`. The CI workflow in `.github/workflows` has not run on GitHub yet.

## macOS

- The backend calls CoreAudio, AudioToolbox and CoreFoundation by `dlopen` through purego, and `AccessStatus` and `RequestAccess` call AVFoundation through purego and the Objective-C runtime. They ship with macOS and are platform binaries, so library validation never blocks them.
- **cgo.** With `CGO_ENABLED=0` nothing else is needed. With cgo on, purego imports `runtime/cgo`, which needs a working `cc`. Go turns cgo on by default when `cc` exists, so a Mac with a broken `cc` fails the build in `runtime/cgo`; measured with a failing `cc` first on `PATH`: `CGO_ENABLED=0` builds, and unset or `1` fails. A Mac used for Go development has the Command Line Tools, because git needs them.
- **Signing and the hardened runtime.** The test binaries signed ad hoc with `codesign -s - -f -o runtime` (flags `adhoc,runtime`) ran the enumeration, listener and unit tests without any entitlement. The Opus codec is plain Go, so it needs no executable-memory entitlement either. This was measured on a Mac with ad-hoc signatures; a Developer ID signature and notarization were not tried.
- **Microphone.** Capture needs the user's consent (TCC). An app bundle needs `NSMicrophoneUsageDescription` in its `Info.plist`; a command-line tool inherits its terminal's permission. Apple's documentation adds the `com.apple.security.device.audio-input` entitlement for the hardened runtime (and the Audio Input capability in the App Sandbox), and says that macOS ends an app that asks for access or captures without the key or the entitlement. That was not tested: no app bundle was signed, sandboxed or notarized here, and capture streams were opened only from an unsigned test binary run in a terminal that already had access. A command-line test binary signed ad hoc with the hardened runtime read `authorized` with and without the entitlement, which shows only that reading the status works. Opening capture reads the authorization status first: when it is denied or restricted, `OpenInput` fails with an error that wraps `ErrPermission`, `ErrBackend` and `fs.ErrPermission` before CoreAudio is loaded; this is tested against a fake AVFoundation, and no real denial was observed. When the status is not determined the system decides. The CoreAudio status codes that also map to it come from the SDK headers. `AccessStatus` read the real authorization status on this Mac (authorized: the terminal had been granted access before); `RequestAccess` was run only against a fake, never on a real system, because it can prompt; its completion block was run through its C entry point by a Go stand-in for AVFoundation.
- Listing devices does not ask for consent.
- When the first stream is opened, the backend sets the process's CoreAudio run loop (`kAudioHardwarePropertyRunLoop`) to NULL, so that notifications need no main run loop. Listing devices does not change it. This applies to the whole process: the HAL notifications of every other CoreAudio client in the same application then arrive on a HAL thread instead of the main run loop.

## Windows

- MMDevAPI and WASAPI ship with every Windows 10 and 11 desktop edition, including N editions, and run through the Windows Audio service.
- On Windows Server that service is disabled by default (from Microsoft's documentation; not verified here). Enumeration then reports `AUDCLNT_E_SERVICE_NOT_RUNNING` or no endpoints, as `ErrNoDevice`.
- Streams are shared mode, event driven, with automatic format conversion, on a COM thread locked per stream. Nothing has initialized a stream on a real endpoint.

## Linux

- A desktop has PipeWire or PulseAudio, and kolo-audio talks to its pulse socket. A minimal container image has neither: with no socket and no `/dev/snd`, devices return `ErrNoDevice` and the message lists what was tried. Give a container `--device /dev/snd` or mount a sound server's socket.
- Nothing needs `libasound` or `libpulse`, and `CGO_ENABLED=0` binaries are static, so they start on Alpine and distroless images.
- Zero install was checked in a clean `golang:1.27-alpine` container (linux/arm64) with no C compiler, no `pkg-config`, `CC` and `CGO_ENABLED` unset and no network after `go mod download`: the module builds for seven targets, the examples run and `go test ./...` passes. There `Devices` returns `ErrNoDevice`, as it should without a sound server.
- JACK-only systems need a pulse-protocol bridge; PipeWire provides one. DirectSound, WinMM, sndio, audio4, OSS, AAudio, OpenSL and WebAudio are not supported.
- Only IPv4 (`tcp4`) and unix sockets are used for the sound server. Every IPv6 form is refused.

## Run the validation yourself

The scripts are in [build/validate](../build/validate/README.md). They build every binary with `CGO_ENABLED=0` on one machine and run it on the target.

```sh
build/validate/build.sh /tmp/kolo-validate        # all binaries, SHA256SUMS
build/validate/macos-hardened.sh                  # macOS: sign, enumerate, test; prints PASS
build/validate/linux/run.sh                       # Docker: Pulse and PipeWire servers
build/validate/linux-alsa/run.sh                  # QEMU VM with snd-aloop
KOLO_WINRUN=<helper> build/validate/windows.sh /tmp/kolo-validate   # maintainers: a Windows VM and a helper that runs a program in it
```

No script opens a microphone, plays audible sound or asks for a permission. Tests that open real devices run only with `KOLO_AUDIO_HW_TEST=1`, and only a person at the machine should set it. With it, `TestHardwareRequestAccess` calls `RequestAccess` and may show the macOS microphone prompt; it opens no device (`KOLO_AUDIO_HW_TEST=1 go test -run TestHardwareRequestAccess -v .`).
