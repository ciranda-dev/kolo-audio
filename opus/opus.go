// Package opus encodes and decodes Opus (RFC 6716) with libopus 1.6.1
// translated to Go. It needs no cgo and no installed library. Samples are
// interleaved signed 16-bit PCM.
package opus

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrInvalidConfig means a Config field or constructor argument is out of range.
	ErrInvalidConfig = errors.New("koloaudio: opus: invalid config")
	// ErrInvalidArgument means a buffer or value passed to a call is not valid.
	ErrInvalidArgument = errors.New("koloaudio: opus: invalid argument")
	// ErrFrameSize means the PCM length is not an allowed frame duration.
	ErrFrameSize = errors.New("koloaudio: opus: invalid frame size")
	// ErrBufferTooSmall means the output buffer cannot hold the result.
	ErrBufferTooSmall = errors.New("koloaudio: opus: buffer too small")
	// ErrInvalidPacket means the packet is empty or corrupt.
	ErrInvalidPacket = errors.New("koloaudio: opus: invalid packet")
	// ErrInternal means libopus detected an internal error or the codec
	// faulted. After a fault the next call continues on a new codec
	// instance with the same settings and fresh codec state.
	ErrInternal = errors.New("koloaudio: opus: internal error")
	// ErrUnimplemented means libopus does not implement the request.
	ErrUnimplemented = errors.New("koloaudio: opus: not implemented")
	// ErrInvalidState means the libopus state is invalid.
	ErrInvalidState = errors.New("koloaudio: opus: invalid state")
	// ErrAllocFail means libopus could not allocate memory.
	ErrAllocFail = errors.New("koloaudio: opus: allocation failed")
	// ErrClosed means the encoder or decoder was closed.
	ErrClosed = errors.New("koloaudio: opus: closed")
)

// libopus error codes (opus_defines.h).
var codeErrors = map[int]error{
	-1: ErrInvalidArgument,
	-2: ErrBufferTooSmall,
	-3: ErrInternal,
	-4: ErrInvalidPacket,
	-5: ErrUnimplemented,
	-6: ErrInvalidState,
	-7: ErrAllocFail,
}

func codeError(code int) error {
	if err, ok := codeErrors[code]; ok {
		return err
	}
	return fmt.Errorf("%w: libopus error %d", ErrInternal, code)
}

const (
	// PacketBufferSize is an output buffer size for Encode that holds any
	// packet, as libopus recommends.
	PacketBufferSize = 4000
	// MaxPacketDuration is the longest audio one packet can carry. Size
	// Decode buffers for it to accept any packet.
	MaxPacketDuration = 120 * time.Millisecond
)

// maxPacketSize is the longest packet without padding: a TOC byte, a frame
// count byte, a 2-byte length for each of 47 frames and 48 frames of 1275
// bytes (RFC 6716 sections 3.2.1 and 3.2.5).
const maxPacketSize = 2 + 47*2 + 48*1275

// Application tunes the encoder for a kind of signal.
type Application int

const (
	// VoIP favors speech intelligibility. It is the default.
	VoIP Application = iota
	// Audio favors faithfulness to the input, for music and mixed content.
	Audio
	// LowDelay disables the speech modes to save a few milliseconds of delay.
	LowDelay
)

// Config configures an Encoder. Zero fields take the defaults.
type Config struct {
	// SampleRate is 8000, 12000, 16000, 24000 or 48000 Hz. Zero means 48000.
	SampleRate int
	// Channels is 1 or 2. Zero means 1.
	Channels int
	// Application defaults to VoIP.
	Application Application
	// Bitrate in bits per second, 500 to 512000. Zero lets libopus choose
	// from the sample rate and channel count.
	Bitrate int
	// Complexity from 1 to 10 trades CPU for quality. Zero keeps the libopus
	// default, 9. SetComplexity also accepts 0.
	Complexity int
	// FEC adds in-band forward error correction when PacketLossPercent > 0.
	FEC bool
	// PacketLossPercent is the expected loss, 0 to 100.
	PacketLossPercent int
	// DTX sends almost nothing during silence.
	DTX bool
	// CBR makes every packet of a frame duration the same size, so sizes do
	// not reveal speech. Turn DTX off with it: DTX still sends 1 or 2 byte
	// packets in silence. The default is variable bitrate.
	CBR bool
	// FrameDuration is 2.5, 5, 10, 20, 40 or 60 ms. Zero means 20 ms.
	FrameDuration time.Duration
}

func validRate(rate int) bool {
	switch rate {
	case 8000, 12000, 16000, 24000, 48000:
		return true
	}
	return false
}

func validFrameDuration(d time.Duration) bool {
	switch d {
	case 2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond,
		20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond:
		return true
	}
	return false
}

// Encoder encodes PCM frames to Opus packets. Its methods are safe for
// concurrent use. Close releases its codec but is not required to avoid a leak.
type Encoder struct {
	mu        sync.Mutex
	c         *core
	rate      int
	app       Application
	channels  int
	frameSize int
	ctls      map[int32]int32
}

// NewEncoder returns an Encoder for cfg.
func NewEncoder(cfg Config) (*Encoder, error) {
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 48000
	}
	if cfg.Channels == 0 {
		cfg.Channels = 1
	}
	if cfg.FrameDuration == 0 {
		cfg.FrameDuration = 20 * time.Millisecond
	}
	switch {
	case !validRate(cfg.SampleRate):
		return nil, fmt.Errorf("%w: sample rate %d", ErrInvalidConfig, cfg.SampleRate)
	case cfg.Channels != 1 && cfg.Channels != 2:
		return nil, fmt.Errorf("%w: %d channels", ErrInvalidConfig, cfg.Channels)
	case cfg.Application < VoIP || cfg.Application > LowDelay:
		return nil, fmt.Errorf("%w: application %d", ErrInvalidConfig, cfg.Application)
	case cfg.Bitrate != 0 && (cfg.Bitrate < 500 || cfg.Bitrate > 512000):
		return nil, fmt.Errorf("%w: bitrate %d", ErrInvalidConfig, cfg.Bitrate)
	case cfg.Complexity < 0 || cfg.Complexity > 10:
		return nil, fmt.Errorf("%w: complexity %d", ErrInvalidConfig, cfg.Complexity)
	case cfg.PacketLossPercent < 0 || cfg.PacketLossPercent > 100:
		return nil, fmt.Errorf("%w: packet loss %d%%", ErrInvalidConfig, cfg.PacketLossPercent)
	case !validFrameDuration(cfg.FrameDuration):
		return nil, fmt.Errorf("%w: frame duration %v", ErrInvalidConfig, cfg.FrameDuration)
	}
	e := &Encoder{
		rate:      cfg.SampleRate,
		app:       cfg.Application,
		channels:  cfg.Channels,
		frameSize: int(int64(cfg.SampleRate)*int64(cfg.FrameDuration)/int64(time.Second)) * cfg.Channels,
		ctls:      make(map[int32]int32),
	}
	c, err := e.newCore()
	if err != nil {
		return nil, err
	}
	e.c = c
	settings := []struct {
		req, v int32
		set    bool
	}{
		{reqSetBitrate, int32(cfg.Bitrate), cfg.Bitrate != 0},
		{reqSetComplexity, int32(cfg.Complexity), cfg.Complexity != 0},
		{reqSetFEC, int32(boolInt(cfg.FEC)), cfg.FEC},
		{reqSetPacketLoss, int32(cfg.PacketLossPercent), cfg.PacketLossPercent != 0},
		{reqSetDTX, int32(boolInt(cfg.DTX)), cfg.DTX},
		{reqSetVBR, 0, cfg.CBR},
	}
	for _, s := range settings {
		if s.set {
			if err := e.set(s.req, s.v); err != nil {
				return nil, err
			}
		}
	}
	return e, nil
}

func (e *Encoder) newCore() (*core, error) {
	return newEncoderCore(e.rate, e.channels, e.app, e.frameSize)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// live returns the codec instance, replacing one that faulted with a new one
// that has the same settings. The caller holds e.mu.
func (e *Encoder) live() (*core, error) {
	if e.c == nil {
		return nil, ErrClosed
	}
	if e.c.failed == nil {
		return e.c, nil
	}
	c, err := e.newCore()
	if err != nil {
		return nil, err
	}
	for req, v := range e.ctls {
		if err := c.encoderSet(req, v); err != nil {
			return nil, err
		}
	}
	e.c = c
	return c, nil
}

// FrameSize returns the number of samples, all channels together, that
// Encode takes per call.
func (e *Encoder) FrameSize() int {
	if e == nil {
		return 0
	}
	return e.frameSize
}

// Encode encodes one frame of exactly FrameSize samples into out and returns
// the packet length. PacketBufferSize bytes always suffice; a shorter out is
// not an error, libopus limits the packet to len(out) bytes at the cost of
// quality. With DTX, a packet of 2 bytes or less carries silence and need not
// be sent. A codec fault returns ErrInternal.
func (e *Encoder) Encode(pcm []int16, out []byte) (int, error) {
	if e == nil {
		return 0, ErrClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.live()
	if err != nil {
		return 0, err
	}
	if len(pcm) != e.frameSize {
		return 0, fmt.Errorf("%w: %d samples, want %d", ErrFrameSize, len(pcm), e.frameSize)
	}
	if len(out) == 0 {
		return 0, ErrBufferTooSmall
	}
	return c.encode(pcm, len(pcm)/e.channels, out)
}

func (e *Encoder) set(req, v int32) error {
	if e == nil {
		return ErrClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.live()
	if err != nil {
		return err
	}
	if err := c.encoderSet(req, v); err != nil {
		return err
	}
	e.ctls[req] = v
	return nil
}

// SetBitrate sets the target bitrate, 500 to 512000 bits per second, or
// 0 to let libopus choose.
func (e *Encoder) SetBitrate(bps int) error {
	if bps != 0 && (bps < 500 || bps > 512000) {
		return fmt.Errorf("%w: bitrate %d", ErrInvalidArgument, bps)
	}
	if bps == 0 {
		bps = opusAuto
	}
	return e.set(reqSetBitrate, int32(bps))
}

// SetComplexity sets the complexity, 0 to 10.
func (e *Encoder) SetComplexity(c int) error {
	if c < 0 || c > 10 {
		return fmt.Errorf("%w: complexity %d", ErrInvalidArgument, c)
	}
	return e.set(reqSetComplexity, int32(c))
}

// SetFEC turns in-band forward error correction on or off.
func (e *Encoder) SetFEC(on bool) error { return e.set(reqSetFEC, int32(boolInt(on))) }

// SetPacketLossPercent sets the expected packet loss, 0 to 100.
func (e *Encoder) SetPacketLossPercent(p int) error {
	if p < 0 || p > 100 {
		return fmt.Errorf("%w: packet loss %d%%", ErrInvalidArgument, p)
	}
	return e.set(reqSetPacketLoss, int32(p))
}

// SetDTX turns discontinuous transmission on or off.
func (e *Encoder) SetDTX(on bool) error { return e.set(reqSetDTX, int32(boolInt(on))) }

// SetCBR turns constant bitrate on or off. With it on, every packet of a
// frame duration has the same size, as long as out in Encode holds that
// size, and DTX still shortens packets in silence. Off is variable bitrate.
func (e *Encoder) SetCBR(on bool) error { return e.set(reqSetVBR, int32(boolInt(!on))) }

// Close releases the Encoder. Later calls return ErrClosed. Close is
// idempotent and always returns nil.
func (e *Encoder) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c = nil
	return nil
}

// Decoder decodes Opus packets to PCM. Its methods are safe for concurrent
// use. Close releases its codec but is not required to avoid a leak.
type Decoder struct {
	mu       sync.Mutex
	c        *core
	rate     int
	channels int
}

// NewDecoder returns a Decoder that outputs sampleRate Hz (8000, 12000,
// 16000, 24000 or 48000) with 1 or 2 channels, whatever the packets hold.
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	if !validRate(sampleRate) || (channels != 1 && channels != 2) {
		return nil, fmt.Errorf("%w: %d Hz, %d channels", ErrInvalidConfig, sampleRate, channels)
	}
	d := &Decoder{rate: sampleRate, channels: channels}
	c, err := d.newCore()
	if err != nil {
		return nil, err
	}
	d.c = c
	return d, nil
}

func (d *Decoder) newCore() (*core, error) { return newDecoderCore(d.rate, d.channels) }

// live returns the codec instance, replacing one that faulted with a new
// one. The caller holds d.mu.
func (d *Decoder) live() (*core, error) {
	if d.c == nil {
		return nil, ErrClosed
	}
	if d.c.failed != nil {
		c, err := d.newCore()
		if err != nil {
			return nil, err
		}
		d.c = c
	}
	return d.c, nil
}

func (d *Decoder) frames(pcm []int16, lost bool) (int, error) {
	if len(pcm) == 0 || len(pcm)%d.channels != 0 {
		return 0, fmt.Errorf("%w: pcm holds %d samples, not a whole number of %d-channel frames", ErrInvalidArgument, len(pcm), d.channels)
	}
	maxFrames := d.rate * 120 / 1000
	n := len(pcm) / d.channels
	if lost && (n%(d.rate/400) != 0 || n > maxFrames) {
		return 0, fmt.Errorf("%w: %d frames is not a multiple of 2.5 ms up to 120 ms", ErrFrameSize, n)
	}
	return min(n, maxFrames), nil
}

func (d *Decoder) decode(packet []byte, pcm []int16, fec bool) (int, error) {
	if d == nil {
		return 0, ErrClosed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.live()
	if err != nil {
		return 0, err
	}
	if packet != nil && (len(packet) == 0 || len(packet) > maxPacketSize) {
		return 0, fmt.Errorf("%w: length %d", ErrInvalidPacket, len(packet))
	}
	frames, err := d.frames(pcm, packet == nil || fec)
	if err != nil {
		return 0, err
	}
	return c.decode(packet, pcm, frames, d.channels, fec)
}

// Decode decodes packet into pcm and returns the number of samples written,
// all channels together. pcm must hold the packet's duration;
// MaxPacketDuration always suffices. A corrupt packet, or one longer than an
// Opus packet can be without padding (1275 bytes per frame, 48 frames),
// returns ErrInvalidPacket. A codec fault returns ErrInternal.
func (d *Decoder) Decode(packet []byte, pcm []int16) (int, error) {
	if packet == nil {
		packet = []byte{}
	}
	return d.decode(packet, pcm, false)
}

// DecodeFEC reconstructs a lost packet from the forward error correction data
// in the packet that followed it, and fills all of pcm, whose duration must be
// that of the lost packet: a multiple of 2.5 ms up to MaxPacketDuration.
// Without FEC data it conceals the loss like DecodeLost. Decode packet itself
// afterwards.
func (d *Decoder) DecodeFEC(packet []byte, pcm []int16) (int, error) {
	if packet == nil {
		packet = []byte{}
	}
	return d.decode(packet, pcm, true)
}

// DecodeLost conceals a lost packet by filling all of pcm, whose duration
// must be that of the lost packet: a multiple of 2.5 ms up to
// MaxPacketDuration. Call it once per lost packet.
func (d *Decoder) DecodeLost(pcm []int16) (int, error) {
	return d.decode(nil, pcm, false)
}

// DecodedLen returns the number of samples, all channels together, that
// Decode writes for packet. It reads only the packet header, so Decode can
// still reject the packet.
func (d *Decoder) DecodedLen(packet []byte) (int, error) {
	if d == nil {
		return 0, ErrClosed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.live()
	if err != nil {
		return 0, err
	}
	if len(packet) == 0 || len(packet) > maxPacketSize {
		return 0, fmt.Errorf("%w: length %d", ErrInvalidPacket, len(packet))
	}
	n, err := c.samples(packet)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: no frames", ErrInvalidPacket)
	}
	return n * d.channels, nil
}

// Close releases the Decoder. Later calls return ErrClosed. Close is
// idempotent and always returns nil.
func (d *Decoder) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.c = nil
	return nil
}
