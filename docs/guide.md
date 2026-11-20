# Guide

How to use kolo-audio beyond the README quick start. Every bound is in [limits.md](limits.md); what runs on which operating system is in [platforms.md](platforms.md).

## Devices

```go
mics, err := koloaudio.Devices(koloaudio.Capture)
```

- `Devices` lists `Capture` or `Playback` devices and never opens one. A `Device` has an opaque `ID`, a `Name`, its `Kind` and `Default`, which is true for the system default of its kind.
- `Devices`, `OpenInput` and `OpenOutput` take no context, and none blocks without bound: a PulseAudio server that does not answer costs at most 30 s per step, and `$PULSE_SERVER` entries are tried in turn (see [limits.md](limits.md#blocking-calls)). Call them from a goroutine if the caller must not wait.
- IDs and names identify hardware on this machine (a Bluetooth ID can hold its address). Do not send them to peers.
- An ID is the lowercase hex of the backend's identifier, so it stays the same while the system keeps that identifier:

  | Platform | What the ID encodes |
  |---|---|
  | macOS | the CoreAudio device UID |
  | Windows | the UTF-16LE endpoint ID |
  | Linux, sound server | the sink or source name; monitor sources are listed as capture devices |
  | Linux, ALSA | `hw:CARD=<id>,DEV=<n>` |

- `Config.DeviceID` selects a device. Empty means the system default, and on macOS and Windows the stream follows the default when it changes (PulseAudio moves the stream itself). An explicit ID pins the stream to that device. An ID that `Devices` did not list returns `ErrNoDevice`.
- On Linux the PulseAudio protocol is tried first: `$PULSE_SERVER`, then `$XDG_RUNTIME_DIR/pulse/native`, then `/run/user/<uid>/pulse/native`. PipeWire answers on the same socket. The ALSA kernel interface is used only when no sound server answers; a server that refuses authentication is reported, not skipped.
- ALSA devices are hardware PCMs: there is no `dmix` or `plug` sharing, and a device is exclusive while open. A Linux user needs access to `/dev/snd` (the `audio` group or the login session's ACLs), otherwise the error wraps `fs.ErrPermission`.
- On macOS, capture needs the user's consent. A command-line tool inherits its terminal's permission. An app bundle needs `NSMicrophoneUsageDescription` in its `Info.plist`; with the hardened runtime (required for notarization) it also needs the `com.apple.security.device.audio-input` entitlement, and in the App Sandbox the Audio Input capability. Apple's documentation says that without the key or the entitlement macOS ends the app when it asks for access or uses the microphone, so `RequestAccess` cannot return an error then. This comes from Apple's documentation and was not tested here: no app bundle was signed or notarized. `AccessStatus` reads the state (`AccessNotDetermined`, `AccessRestricted`, `AccessDenied`, `AccessAuthorized`) without asking. `RequestAccess(ctx)` shows the system prompt if the user was not asked yet and returns when access is decided: nil, or an error that wraps `ErrPermission` and `fs.ErrPermission`. The prompt waits for the person, so bound the wait with `ctx`: when it ends, `RequestAccess` returns `ctx.Err()` and the prompt stays until it is answered. Neither opens a device. If the user or a policy has refused the microphone (`AccessDenied` or `AccessRestricted`), `OpenInput` returns an error that wraps `ErrPermission`, `ErrBackend` and `fs.ErrPermission` before it opens a device, because CoreAudio would open the device and deliver silence. If the user was not asked yet (`AccessNotDetermined`), `OpenInput` leaves the decision to the system, so call `RequestAccess` first. Listing devices does not ask.
- On Linux and Windows there is no consent state to read or ask for: `AccessStatus` returns `AccessAuthorized`, which there only means that nothing can be checked, and `RequestAccess` returns nil. `OpenInput` reports the refusals the backend returns (permissions on `/dev/snd`, `E_ACCESSDENIED` from WASAPI) as an error that wraps `ErrPermission` and `fs.ErrPermission`. A system-wide microphone block that the system answers with silence instead of an error, such as a Windows privacy setting, is not detected, and no real block was tried here. Where devices are not supported both return `ErrUnsupported`.

```go
if err := koloaudio.RequestAccess(ctx); err != nil { // macOS: may show the system prompt
	log.Fatal(err)
}
in, err := koloaudio.OpenInput(koloaudio.Config{DeviceID: mics[0].ID})
if err != nil {
	log.Fatal(err)
}
defer in.Close()
buf := make([]int16, 960) // 20 ms of mono 48 kHz
n, err := in.Read(ctx, buf)
```

## Formats and resampling

- Samples are interleaved signed 16-bit PCM in a Go `[]int16`. A frame holds one sample per channel. `EncodeS16LE` and `DecodeS16LE` convert to and from little-endian bytes; `DecodeS16LE` rejects an odd byte count.
- `Config.Format` is the format your `Read` or `Write` sees: 48000 Hz and 1 channel by default. The backend converts between it and the device's own format, so a request the device cannot meet through conversion returns `ErrUnsupportedFormat`. On macOS a pair that needs another channel conversion (for example 8 to 2) fails the same way.
- `len(p)` in `Read` and `Write` must be a positive multiple of the channel count (`ErrInvalidArgument` otherwise). The counts are in samples, not frames.
- `NewResampler(channels, inRate, outRate)` converts rates with linear interpolation and a 4th-order low-pass filter, in chunks of any size. It is miniaudio's resampler, translated to Go, and its output is bit-identical to the original.

```go
r, err := koloaudio.NewResampler(1, 44100, 48000)
if err != nil {
	log.Fatal(err)
}
defer r.Close()
out := make([]int16, r.OutputLen(len(in)))
written, consumed, err := r.Process(out, in) // call again with in[consumed:] if consumed < len(in)
```

  `Latency()` is the filter delay in output frames. `Flush(dst)` writes the held-back tail into `(Latency()+1)*channels` samples and resets. `Reset()` drops it.
- `ConvertChannels(dst, src, from, to)` copies, duplicates mono, or averages to mono. `Gain(pcm, factor)` scales in place with saturation. `Mix(dst, src)` adds with saturation. `Level(pcm)` returns peak and RMS in dBFS.

## Streams

- `Read` blocks until at least one frame is captured and returns up to `len(p)` samples. If the application reads too slowly the oldest frames are dropped and counted in `Stats().Overruns`; the audio thread never waits for the application.
- `Write` queues all of `p`, blocking while the buffer is full. The device plays silence until the first `Write`; after that, silence played because the buffer ran empty is counted in `Stats().Underruns`.
- `Stats()` returns `Frames`, `Overruns`, `Underruns` (all in frames) and `Buffered` frames.
- `Close()` stops and releases the device. It is idempotent, and blocked and later calls return `ErrClosed`. Always call it: a stream that is not closed keeps its device running until the garbage collector finds it unreachable.
- `Input` and `Output` are safe for concurrent use. Concurrent `Read`s split the captured audio between them and concurrent `Write`s may interleave.
- `Period` is a hint to the backend. `Buffer` is the application's ring, not the device buffer.

## Opus

An `Encoder` or `Decoder` is one translated libopus 1.6.1 instance with its own memory. Both are safe for concurrent use and take a lock per call.

```go
enc, err := opus.NewEncoder(opus.Config{
	SampleRate:        48000,
	Channels:          1,
	Application:       opus.VoIP,
	Bitrate:           24000,
	FEC:               true,
	PacketLossPercent: 10,
})
```

| Setting | Values | Notes |
|---|---|---|
| `SampleRate` | 8000, 12000, 16000, 24000, 48000 | Default 48000 |
| `Channels` | 1 or 2 | Default 1 |
| `Application` | `VoIP`, `Audio`, `LowDelay` | `VoIP` favors speech; `Audio` favors fidelity; `LowDelay` disables the speech modes |
| `Bitrate` | 500 to 512000 bit/s | Zero lets libopus choose from the rate and channels |
| `Complexity` | 1 to 10 | Zero keeps libopus's default, 9 |
| `FEC` | bool | Adds forward error correction when `PacketLossPercent` is above 0 |
| `PacketLossPercent` | 0 to 100 | The loss you expect; it sizes the FEC data |
| `DTX` | bool | During silence the encoder sends almost nothing; a packet of 2 bytes or less carries silence and need not be sent |
| `CBR` | bool | Constant bitrate: every packet of a frame duration has the same size. Default false (variable bitrate). See below |
| `FrameDuration` | 2.5, 5, 10, 20, 40, 60 ms | Default 20 ms |

- `Encode(pcm, out)` takes exactly `FrameSize()` samples (`ErrFrameSize` otherwise) and returns the packet length. An `out` of `PacketBufferSize` (4000 bytes) always holds a packet.
- `SetBitrate`, `SetComplexity`, `SetFEC`, `SetPacketLossPercent`, `SetDTX` and `SetCBR` change settings between frames.
- `NewDecoder(sampleRate, channels)`; `Decode(packet, pcm)` returns the samples written. Size `pcm` for `MaxPacketDuration` (120 ms) to accept any packet. `DecodedLen(packet)` reads the header to tell how many samples a packet holds.
- Loss. `DecodeFEC(next, pcm)` rebuilds a lost packet from the FEC data in the packet after it. `DecodeLost(pcm)` conceals it. In both, `pcm` must cover exactly the lost duration, a multiple of 2.5 ms up to 120 ms; call `DecodeLost` once per lost packet. [examples/opustone](../examples/opustone) shows the recovery.
- Constant bitrate. With variable bitrate the packet size follows the signal, so an observer who sees only packet sizes, for example of encrypted audio, can tell speech from silence (RFC 6562). With `CBR` (or `SetCBR(true)`) every packet of a frame duration has the same length, `(bitrate * frame duration + 4) / 8` bytes (at least 1), for tone, silence, noise and speech alike, at 16, 32 and 64 kbit/s, mono and stereo. Keep `DTX` off with it: DTX still replaces silence with packets of 1 or 2 bytes, which shows when someone is quiet. Pass an `out` of at least that size to `Encode`; a shorter one cuts the packet. Constrained variable bitrate, libopus's default, is not constant and is what `CBR` turns off.
- Opus packets follow RFC 6716 and can be carried as RFC 7587 payloads.
- kolo-audio does not provide echo cancellation, noise suppression or automatic gain control. During a call, use headphones or the platform's voice-processing device.

## Errors

Every error starts with `koloaudio:` and wraps a sentinel, so `errors.Is` works.

| Error | When |
|---|---|
| `ErrInvalidConfig` | A `Config` field is out of range or inconsistent |
| `ErrInvalidArgument` | A buffer or value passed to a call is not valid |
| `ErrNoDevice` | An unknown ID, no default device, no sound server and no ALSA devices, or the Windows audio service is not running |
| `ErrUnsupportedFormat` | The format is outside the range, or the device cannot be reached by conversion |
| `ErrDeviceLost` | The device was removed, or made no callback for `StallTimeout` |
| `ErrClosed` | After `Close` |
| `ErrUnsupported` | This GOOS and GOARCH has no device backend: `koloaudio: not supported on <GOOS>/<GOARCH>` |
| `ErrBackend` | Any other backend failure; the message has the cause. A denied access also wraps `fs.ErrPermission` |
| `ErrPermission` | `RequestAccess` or `OpenInput`: the user or a policy refused microphone access. Errors that wrap it also wrap `fs.ErrPermission`; from `OpenInput` they also wrap `ErrBackend` |

- A lost stream fails every later `Read` and `Write` with `ErrDeviceLost` at once, and buffered capture is discarded. Open a new stream to go on.
- Rerouting to a new default device (macOS, Windows) is not an error.
- The `opus` package has its own sentinels for libopus codes: `ErrInvalidArgument`, `ErrBufferTooSmall`, `ErrInternal`, `ErrInvalidPacket`, `ErrUnimplemented`, `ErrInvalidState`, `ErrAllocFail`, plus `ErrInvalidConfig`, `ErrFrameSize` and `ErrClosed`. A corrupt or empty packet returns `ErrInvalidPacket` and never panics.
- If the translated codec or resampler faults, the call returns `ErrInternal` (opus) or `ErrBackend` (resampler) with the fault text, and the next call continues on a new instance with the same settings and fresh codec state. For a decoder that faces a network, one bad packet is therefore one lost frame, not a dead peer. See [SECURITY.md](../SECURITY.md).

## Examples

- [examples/passthrough](../examples/passthrough) plays a capture device on a playback device. `-list` prints the devices and IDs, `-in` and `-out` pick them, `-seconds` sets the length. Use headphones.
- [examples/opustone](../examples/opustone) encodes a tone, drops packet 25 and recovers it with FEC.

Examples take device IDs as flags and never read or write files.

## References

- [RFC 6716, Definition of the Opus Audio Codec](https://www.rfc-editor.org/info/rfc6716)
  (Proposed Standard, September 2012, updated by RFC 8251).
  Takes: the packet limits (sections 3.2.1 and 3.2.5), the sample rates and the channels.
- [RFC 8251, Updates to the Opus Audio Codec](https://www.rfc-editor.org/info/rfc8251)
  (Proposed Standard, October 2017).
  Takes: the conformance test vectors.
- [RFC 7587, RTP Payload Format for the Opus Speech and Audio Codec](https://www.rfc-editor.org/info/rfc7587)
  (Proposed Standard, June 2015).
  Takes: the 48 kHz RTP clock; Opus packets can be carried as its payload.
  Not implemented: the RTP packetization.
- [RFC 6562, Guidelines for the Use of Variable Bit Rate Audio with Secure RTP](https://www.rfc-editor.org/info/rfc6562)
  (Proposed Standard, March 2012).
  Takes: constant bitrate against size leaks.
