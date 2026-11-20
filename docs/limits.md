# Limits

Every bound in kolo-audio, with its value and where it comes from. A value outside a range returns `ErrInvalidConfig`, `ErrInvalidArgument` or `ErrUnsupportedFormat`, as noted. This page is reference material: the README does not repeat it.

## Streams

| Field | Default | Range | Why |
|---|---|---|---|
| `Format.SampleRate` | 48000 Hz | 8000 to 384000 Hz (`MinSampleRate`, `MaxSampleRate`) | Opus runs at 48 kHz (RFC 7587). The range is the standard sample rates of miniaudio 0.11.26 (`ma_standard_sample_rate_min` and `max`) |
| `Format.Channels` | 1 | 1 to 254 (`MaxChannels`) | miniaudio 0.11.26 `MA_MAX_CHANNELS` |
| `Config.Period` | 10 ms | at least one frame, at most `Buffer` | The device callback period. A hint: a backend clamps it to what the device allows |
| `Config.Buffer` | 120 ms | `Period` to 2 s (`MaxBuffer`) | 120 ms holds one Opus packet of the longest duration (RFC 6716 section 3.2.5). 2 s is PulseAudio's default playback target length, the longest default buffer among the supported sound servers; at the largest format it still fits a 32-bit frame count |
| `Config.StallTimeout` | 12 s | longer than `Period` | A platform can defer the start by 5 s when resuming from standby; the timeout leaves room for that and a start-up check |

- A buffer too large for the sample count returns `ErrInvalidConfig`.
- `Read` and `Write` take a positive multiple of the channel count, otherwise `ErrInvalidArgument`.
- A capture overrun drops the oldest frames. `Write` blocks while the buffer is full. Nothing grows beyond `Buffer`.
- Memory per stream: the stream buffer is allocated when the stream opens and holds `Buffer` x `SampleRate` x `Channels` samples of 2 bytes. The ranges allow 2 s x 384000 Hz x 254 channels, about 390 MB for one stream, and a backend adds staging buffers of its own. The ranges are independent, so an application that takes a format from a peer or a settings file should bound it before it opens a stream.

## Opus

| Value | Limit | Why |
|---|---|---|
| Sample rate | 8000, 12000, 16000, 24000, 48000 Hz | RFC 6716 |
| Channels | 1 or 2 | RFC 6716 |
| Bitrate | 500 to 512000 bit/s (0 lets libopus choose) | libopus: "rates from 500 to 512000 bits per second are meaningful" |
| Complexity | 1 to 10 (0 keeps the libopus default, 9) | libopus `opus_defines.h` |
| Packet loss | 0 to 100 percent | libopus |
| `CBR` | on or off; the packet is `(bitrate * frame duration + 4) / 8` bytes, at least 1 | libopus fixes the packet length from the bitrate and the frame duration; every valid `Bitrate` and `FrameDuration` works with it, and `TestCBRFrameDurations` checks the size |
| Frame duration | 2.5, 5, 10, 20, 40 or 60 ms | The frame sizes `opus_encode` accepts, up to 60 ms |
| `PacketBufferSize` | 4000 bytes | libopus's recommended output buffer; holds any packet the encoder makes |
| `MaxPacketDuration` | 120 ms | RFC 6716: a packet carries at most 48 frames of 2.5 ms |
| Longest accepted packet | 61296 bytes | RFC 6716 sections 3.2.1 and 3.2.5: a TOC byte, a frame count byte, 47 two-byte lengths and 48 frames of 1275 bytes. A longer packet returns `ErrInvalidPacket` before it reaches the codec |
| `DecodeLost`, `DecodeFEC` | a multiple of 2.5 ms, at most 120 ms | The span of one packet |

## Translated code

The codec and the resampler run as WebAssembly modules translated to Go. Each Encoder, Decoder or Resampler has its own module with its own linear memory.

| Bound | Value | Where it comes from |
|---|---|---|
| libopus memory cap | 16 pages (1 MiB), set at link time (`--max-memory`) | Twice the measured peak, 8 pages, of a module that holds the largest buffers the wrapper allocates: one 61296 byte packet, one 4000 byte output, two 23040 byte PCM buffers (120 ms of stereo 48 kHz), a stereo encoder and a stereo decoder |
| libopus stack | 256 KiB | The measured deepest use is 108608 bytes (41.4 percent); the rule is a stack of at least twice that |
| Memory growth past the cap | `malloc` returns NULL, `opus_encoder_create` returns the allocation error | A failed growth is an error, never a 4 GiB module |
| Resampler memory cap | 8 pages (512 KiB) | Twice the measured peak of 2 pages, rounded up |
| Resampler stack | 4096 bytes | The measured use is 304 bytes in every extreme case |
| Resampler staging | two buffers of 16384 samples in linear memory | Memory does not depend on the size of the caller's slices; `Process` works in blocks |
| Memory per codec instance | about 0.8 to 0.9 MB of heap per stereo encoder and decoder pair | Measured with `runtime.MemStats` after a GC on 100 pairs: 816 KiB with a 20 ms frame, complexity 10 and FEC, and 920 KiB when each pair has also handled one 61296 byte packet. Most of it is the stack |

The figures are checked by tests. `TestMemoryPeak` fails if the memory cap differs from the value in the test or the peak is above half of it. `TestMemoryLimit` exhausts a module and checks that it stops at 16 pages, that `malloc(1 GiB)` returns NULL and that `opus_encoder_create` returns the allocation error. `TestStackHighWater` paints the stack and measures the deepest use. The derivation is in `build/README.md`.

## Backends

| Backend | Bound | Value | Why |
|---|---|---|---|
| PulseAudio | Protocol version | offers 32, refuses servers older than 21 | 21 is the first version with stream formats and ports always present |
| PulseAudio | Request, dial and write timeout | 30 s | libpulse's reply timeout |
| PulseAudio | Playback data write timeout | the smaller of 30 s and `StallTimeout` | The write runs on the reader goroutine; a stream that makes no progress for `StallTimeout` is lost anyway |
| PulseAudio | Frame size from the server | at most 16 MiB; the buffer grows in 64 KiB steps with the bytes that arrive and is dropped before the next packet when it is over 1 MiB | The protocol's largest allowed frame; a header alone does not allocate it |
| PulseAudio | Channels | at most 32 | `PA_CHANNELS_MAX` |
| PulseAudio | Cookie | 256 bytes | The protocol's cookie length; the file must hold at least 256 and be a regular file (a symlink to one works). It is sent to the server as is, so over `tcp4` it travels in clear text: name only servers and networks you trust |
| PulseAudio | Playback attributes | `tlength` = min(2 x `Period`, `Buffer`), `minreq` = `Period`, `maxlength` = `Buffer` | A server cannot make the client do more than `Buffer` of work per request |
| ALSA | Buffer fill | 3 periods | Playback keeps the device filled to three periods |
| ALSA | Suspend retry | every 1 s | Resume is retried until the driver answers |
| ALSA | Channels, buffer | `MaxChannels`, `MaxBuffer` | Same as streams |
| WASAPI | Reroute retries | 100, 200, 400, 800 and 1600 ms (5 retries, 3.1 s), then the stream stops | Gives a device that is being switched time to appear |
| CoreAudio | Device buffer | `Period`, clamped to the device's range | Best effort |

IDs and device names are not bounded by the library: they come from the operating system.

## Blocking calls

`Devices`, `OpenInput` and `OpenOutput` take no context. None of them can block without bound:

- PulseAudio: every dial, write and reply wait has a 30 s timeout. Connecting to one server takes at most a dial plus two requests (about 90 s if the server accepts and then stays silent). Listing devices adds two requests (server info and the list, 60 s, about 150 s in all per server) and opening a stream adds one (30 s). `$PULSE_SERVER` may name several servers; each is tried in turn, so the worst case is that bound times the number of entries. A unix socket that nobody listens on fails at once; a black-holed `tcp4` entry costs its 30 s dial timeout. Name only the servers you need.
- ALSA: local `ioctl` and file calls; a device that does not answer blocks inside the kernel, as for any ALSA program.
- CoreAudio and WASAPI: system calls bounded by the system.
- `Read` and `Write` take a context. `Close` never waits for the application.

## Not bounded

- The number of streams and of codec instances is the application's choice. Each instance costs its own memory (see above).
- Host sound servers set their own limits on streams and latency.
