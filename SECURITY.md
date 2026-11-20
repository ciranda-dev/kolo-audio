# Security Policy

## Reporting a vulnerability

Please do not report security vulnerabilities in public issues, pull
requests or discussions.

Report them privately through GitHub:
[open a private security advisory](https://github.com/ciranda-dev/kolo-audio/security/advisories/new)
(Security tab, "Report a vulnerability"). Only the maintainers can see it.

Include a description of the issue, the affected version and, if possible,
steps to reproduce it. We will acknowledge your report and keep you updated
while we work on a fix.

If private reporting is not available, open a public issue that only asks
for a private contact, with no details about the vulnerability.

## Supported versions

This library is experimental and at v0, so its API may change. Only the
latest v0.x release receives security fixes.

## What the library trusts

kolo-audio has no cgo in any package. It runs code the build produced, calls operating system interfaces and reads data from a sound server. Each boundary is described here so that a report can say which one it concerns.

### Translated code (libopus and the resampler)

- The Opus codec is libopus 1.6.1 and the resampler is miniaudio 0.11.26's, each compiled to WebAssembly by a pinned compiler and translated to Go by wasm2go with `-unsafe`. The generated Go is committed. CI rebuilds the wasm and checks its hash, and regenerates the Go and fails on any difference, so what the module zip carries is what the pinned sources produce. No wasm is loaded, downloaded or extracted at run time.
- The module is trusted code. Nothing runs a wasm module supplied by an application or a peer.
- Memory safety is kept. Every access to the module's linear memory is bounds-checked, also with `-unsafe`. A fault in the codec (a bounds violation, an `unreachable` from libopus's own assertions, a divide by zero) is an ordinary Go panic.
- A panic is recovered at the wrapper boundary and returned as `ErrInternal` (opus) or `ErrBackend` (resampler). A faulted instance is never reused: the module's shadow stack pointer is not restored after a trap, and the codec state is undefined. The next call creates a new module with the same settings and fresh codec state, under the same lock. One hostile packet therefore costs one frame, not a permanently dead decoder. A failure to create the replacement is returned and retried by the next call.
- Memory is bounded. The libopus module is linked with a maximum of 16 pages (1 MiB) and a 256 KiB stack; the resampler with 8 pages. Growth beyond the limit makes allocation return an error; it never grows to the 4 GiB a module could declare. Input is checked before a call: an Opus packet longer than 61296 bytes is `ErrInvalidPacket`, and the frame counts of `DecodeFEC` and `DecodeLost` are capped at 120 ms. The values and their sources are in [docs/limits.md](docs/limits.md).
- A module has no locking of its own and must never be used from two goroutines at once. Each `Encoder`, `Decoder` and `Resampler` holds a mutex around every call. Do not copy one of these values.
- A Go stack overflow is the only fault that cannot be recovered. libopus's recursion is bounded, so this is theoretical.
- The translated codec decodes malformed packets as libopus does. A hostile packet is tested with fuzzing (`FuzzDecode`) and with random and bit-flipped packets; a decode never writes past the caller's buffer.

### System interfaces

- **ALSA (`ioctl` on `/dev/snd`).** The structures are those of the kernel's `<sound/asound.h>`; their sizes and offsets are checked against the request numbers and, in tests, against a size table. The values the kernel installs are checked before use. A device node is opened only from a name parsed by a strict parser, and a card ID is checked against the kernel's character set. The kernel is trusted; a user needs permission on `/dev/snd`, and denial is `ErrBackend` wrapping `fs.ErrPermission`, plus `ErrPermission` when capture is refused.
- **COM and WASAPI (`golang.org/x/sys/windows`).** Calls go through COM vtables with raw pointers, so the layer is `unsafe`. The vtable indexes were checked against the interface layouts in miniaudio 0.11.26's Windows declarations; out-parameters are heap allocated and cannot move during a call. The `IMMNotificationClient` vtable is created once per process, because `NewCallback` has a process-wide limit and never frees. Its callbacks only set a flag and signal an event; they never block and never call back into COM. Windows is trusted.
- **CoreAudio (purego).** The system frameworks are opened with `dlopen` by purego, with no cgo. Callbacks arrive on threads the system created and are found again through a table, not through a Go pointer held in C memory. Three callbacks exist per process (purego has a limit). The frameworks are platform binaries.
- **AVFoundation (purego and the Objective-C runtime).** `AccessStatus` and `RequestAccess` send two class messages to `AVCaptureDevice` for the audio media type, inside an autorelease pool on a locked thread. The completion block of `RequestAccess` runs on a system dispatch queue and only offers the answer to a buffered channel, so it never blocks; an answer after `ctx` ends is dropped. purego's Objective-C package opens `libobjc` and creates one callback when the program starts, and one more callback is created by the first `RequestAccess` that prompts. Neither call opens a device. The null backend that the tests use never loads AVFoundation.
- **PulseAudio protocol (own client, unix socket or `tcp4`).** The server is not fully trusted. The tag reader checks every length against what is left in the buffer. A frame is at most 16 MiB, and its buffer grows with the bytes that arrive (64 KiB steps) and is dropped before the next packet when it is over 1 MiB, so a header alone allocates nothing; a playback write waits at most `StallTimeout`; every request, dial and write has a timeout; a malformed server string, an IPv6 address of any form or a reply of the wrong type is an error, never a panic (`FuzzPulseDecode`, `FuzzPulseServerString`). The authentication cookie is sent as is, so it travels in clear text to a `tcp4` server; it is opened without blocking and only a regular file is read. It is read only from `$PULSE_COOKIE`, `$XDG_CONFIG_HOME/pulse/cookie` or `~/.pulse-cookie`, is sent only to the server, and is never logged or put in an error. Without one, 256 zero bytes are sent, which a server accepts for the same user on a unix socket.

### macOS library validation and the hardened runtime

- kolo-audio loads no library of its own: nothing is extracted to disk, downloaded or JIT-compiled, and it never calls `dlopen` on a file it ships. The only `dlopen` calls are for system frameworks, which are platform binaries, so library validation does not block them.
- An application signed with the hardened runtime needs no `disable-library-validation` and no `allow-unsigned-executable-memory` entitlement because of kolo-audio. A test binary signed ad hoc with `codesign -s - -f -o runtime` ran its enumeration, listener and unit tests. Developer ID signing and notarization were not tried.
- When the first stream is opened, the CoreAudio backend sets the process-wide HAL run loop property to NULL (listing devices does not), which moves the notifications of every other CoreAudio client in the same application to a HAL thread. A listener callback may run once more on a HAL thread after its removal returns; the library's callbacks never block.
- Capturing audio needs the user's consent (TCC); an app bundle needs `NSMicrophoneUsageDescription` in its `Info.plist` and, per Apple's documentation (not tested here), the `com.apple.security.device.audio-input` entitlement with the hardened runtime.

### Privacy

Device IDs and names identify hardware on the machine (a Bluetooth ID can hold its address). They are returned to the application and are never logged or sent by the library; applications should not send them to peers.
