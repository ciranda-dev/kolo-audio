# kolo-audio

Audio capture, playback, PCM processing and Opus for Kolo.

[![status: experimental](https://img.shields.io/badge/status-experimental-orange)](#status)
[![license: MIT OR Apache-2.0](https://img.shields.io/badge/license-MIT%20OR%20Apache--2.0-blue)](#license)
[![go: 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8)](go.mod)

## What it is

kolo-audio captures and plays sound and encodes it with the [Opus codec (RFC 6716)](https://www.rfc-editor.org/info/rfc6716), checked against the test vectors of [RFC 8251](https://www.rfc-editor.org/info/rfc8251). It lists audio devices, records from a microphone and plays to speakers as interleaved signed 16-bit PCM. It also resamples, converts channels, applies gain, mixes, measures levels, and decodes Opus. It knows nothing about the network or about timing across tracks.

It is Go only: no cgo, no C compiler, no libopus, no sound library to install. The Opus codec is libopus 1.6.1 and the resampler is miniaudio's, both translated to Go source that is committed in this repository. Device access uses each operating system's own interface (CoreAudio, WASAPI, the PulseAudio protocol, the ALSA kernel interface).

## Install

```sh
go get github.com/ciranda-dev/kolo-audio
```

The first build of the Opus package compiles about 2 MB of generated Go and peaks at about 660 MB of memory (measured with the go command and its children); later builds use the build cache. On a small build machine, `-gcflags=github.com/ciranda-dev/kolo-audio/opus/internal/libopus=-c=1` lowers the peak: on a Mac arm64 with an empty build cache, 636 MB without it and 433 MB with it, and 11 s instead of 9 s.

## Quick start

List the devices, then encode 20 ms of a tone with Opus and decode it again. Save it outside the repository and run it with `go run .` in a module that requires kolo-audio.

```go
package main

import (
	"errors"
	"fmt"
	"log"
	"math"

	koloaudio "github.com/ciranda-dev/kolo-audio"
	"github.com/ciranda-dev/kolo-audio/opus"
)

func main() {
	for _, kind := range []koloaudio.Kind{koloaudio.Capture, koloaudio.Playback} {
		devs, err := koloaudio.Devices(kind)
		if errors.Is(err, koloaudio.ErrNoDevice) || errors.Is(err, koloaudio.ErrUnsupported) {
			fmt.Printf("%s: %v\n", kind, err)
			continue
		}
		if err != nil {
			log.Fatal(err)
		}
		for _, d := range devs {
			fmt.Printf("%s %q default=%v\n", kind, d.Name, d.Default)
		}
	}

	enc, err := opus.NewEncoder(opus.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer enc.Close()
	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		log.Fatal(err)
	}
	defer dec.Close()

	frame := make([]int16, enc.FrameSize())
	for i := range frame {
		frame[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	packet := make([]byte, opus.PacketBufferSize)
	n, err := enc.Encode(frame, packet)
	if err != nil {
		log.Fatal(err)
	}
	pcm := make([]int16, 48000*opus.MaxPacketDuration.Milliseconds()/1000)
	m, err := dec.Decode(packet[:n], pcm)
	if err != nil {
		log.Fatal(err)
	}
	peak, rms := koloaudio.Level(pcm[:m])
	fmt.Printf("20 ms in %d bytes, decoded peak %.1f dBFS, rms %.1f dBFS\n", n, peak, rms)
}
```

The last line of the output does not depend on the machine:

```text
$ CGO_ENABLED=0 go run .
...
20 ms in 207 bytes, decoded peak -11.3 dBFS, rms -17.0 dBFS
```

To record, open an `Input` with `OpenInput` and call `Read` in a loop; to play, open an `Output` with `OpenOutput` and call `Write`. [examples/passthrough](examples/passthrough) plays a capture device on a playback device, and [examples/opustone](examples/opustone) encodes a tone, drops a packet and recovers it with forward error correction. Details are in the [guide](docs/guide.md).

## Packages and API

| Symbol | What it does |
|---|---|
| `Devices(kind)` | List capture or playback devices without opening them |
| `OpenInput`, `OpenOutput` | Open and start a stream; `Read`, `Write`, `Format`, `Stats`, `Close` |
| `AccessStatus`, `RequestAccess` | Read or ask for macOS microphone access without opening a device |
| `NewResampler` | Convert sample rates in chunks of any size |
| `ConvertChannels`, `Gain`, `Mix`, `Level` | Channels, gain, mixing and level in dBFS |
| `EncodeS16LE`, `DecodeS16LE` | Samples to and from little-endian bytes |
| `opus.NewEncoder` | Encoder with bitrate, complexity, FEC, packet loss, DTX, constant bitrate (`CBR`) and frame duration |
| `opus.NewDecoder` | Decoder with `Decode`, `DecodeFEC` and `DecodeLost` |

Errors are sentinels that work with `errors.Is`: `ErrNoDevice`, `ErrUnsupported`, `ErrDeviceLost`, `ErrUnsupportedFormat`, `ErrBackend`, `ErrPermission` and others. Access denied wraps `fs.ErrPermission`.

## Platforms

| Platform | Device backend | What was run |
|---|---|---|
| macOS arm64 | CoreAudio through purego | Tests, real device enumeration, listener registration and a hardened-runtime build on a Mac. A playback stream with silence and a capture stream opened on the Mac's built-in devices. |
| macOS amd64 | same | Test binary under Rosetta. No native Intel Mac. |
| Windows arm64 | WASAPI through COM | Tests and real endpoint enumeration natively in a Windows 11 VM (virtual sound devices). No stream opened. |
| Windows amd64 | same | The same tests under the VM's x64 emulation. |
| Linux, sound server | PulseAudio protocol (PulseAudio, PipeWire) | Play, record and device removal against real servers in Docker (arm64 native, amd64 emulated), null sink only. |
| Linux, no server | ALSA kernel interface | Enumeration, negotiation and playback into recording on a real kernel (Debian 13 arm64, 6.12.116) with the `snd-aloop` loopback card. No real sound card. amd64 not run on a real kernel. |
| Any | Null device | The tests use a timer-driven virtual device on every platform. |

On real sound hardware, streams were opened only on one Mac (arm64): playback of silence and capture on the built-in devices. Windows and Linux opened no stream on real sound hardware. Every other platform builds, and `Devices`, `OpenInput` and `OpenOutput` return `ErrUnsupported`; the resampler, the PCM helpers and Opus work. The Opus decoder passes the 12 RFC 8251 vectors, mono and stereo, on macOS arm64 and on Windows (arm64 natively, amd64 under emulation). Tests that open real devices run only with `KOLO_AUDIO_HW_TEST=1`. How each platform was validated, and how to repeat it, is in [platforms](docs/platforms.md).

`CGO_ENABLED=0` always works and gives static Linux binaries. Go turns cgo off by itself when no C compiler is on `PATH`. With cgo on, a working C compiler is needed on macOS (purego imports `runtime/cgo`) and on Linux (Go's `net` package does), and a Linux binary then links libc dynamically.

More: [guide](docs/guide.md), [limits](docs/limits.md), [platforms](docs/platforms.md), [security policy](SECURITY.md).

## License

Licensed under either of the [Apache License, Version 2.0](LICENSE) or the [MIT license](LICENSE-MIT), at your option. The generated Go in `opus/internal/libopus` and `internal/maresample` is a translation of two WebAssembly modules, so it carries third-party code. Applications that ship binaries are responsible for showing these notices; copies sit next to each generated package.

| Component | Version | Licence | Notice | Ships in |
|---|---|---|---|---|
| libopus (Xiph.Org) | 1.6.1 | BSD-3-Clause | [LICENSE-libopus](third_party/licenses/LICENSE-libopus) | `opus` |
| miniaudio resampler (David Reid) | 0.11.26 | public domain or MIT-0, your choice | [LICENSE-miniaudio](third_party/licenses/LICENSE-miniaudio) | `Resampler` |
| wasi-libc: musl, dlmalloc, cloudlibc | commit 2e6fb9d8ee0c, wasi-sdk 34 | MIT, CC0, BSD-2-Clause, Apache-2.0 with LLVM exception | [LICENSE-wasi-libc](third_party/licenses/LICENSE-wasi-libc) | `opus` and `Resampler` |
| wasm2go runtime code (Nuno Cruces) | v0.4.22 | MIT | [LICENSE-wasm2go](third_party/licenses/LICENSE-wasm2go) | `opus` and `Resampler` |

The notice for wasi-libc lists the objects that are linked into the two modules, read from the linker maps. The LLVM compiler-rt builtins are not linked into either module.

Dependencies in `go.mod`:

| Module | Licence | Built into |
|---|---|---|
| `github.com/ebitengine/purego` | Apache-2.0 | macOS |
| `golang.org/x/sys` | BSD-3-Clause | Linux and Windows |

## Status

Experimental. The API may change before v1.0.0.
