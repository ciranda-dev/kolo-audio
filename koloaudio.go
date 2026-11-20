package koloaudio

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"time"
)

var (
	// ErrInvalidConfig means a Config field is out of range.
	ErrInvalidConfig = errors.New("koloaudio: invalid config")
	// ErrInvalidArgument means a buffer or value passed to a call is not valid,
	// for example a sample count that is not a whole number of frames.
	ErrInvalidArgument = errors.New("koloaudio: invalid argument")
	// ErrNoDevice means no audio backend is available or no device matches the ID.
	ErrNoDevice = errors.New("koloaudio: no such device")
	// ErrUnsupportedFormat means the sample rate or channel count is not supported.
	ErrUnsupportedFormat = errors.New("koloaudio: unsupported format")
	// ErrDeviceLost means the device stopped or stopped calling back while in use.
	ErrDeviceLost = errors.New("koloaudio: device lost")
	// ErrClosed means the stream or resampler was closed.
	ErrClosed = errors.New("koloaudio: closed")
	// ErrBackend means the audio backend failed for another reason; the
	// message holds its description.
	ErrBackend = errors.New("koloaudio: audio backend failed")
	// ErrUnsupported means the platform has no audio device backend.
	ErrUnsupported = errors.New("koloaudio: not supported on " + runtime.GOOS + "/" + runtime.GOARCH)
	// ErrPermission means the user or a policy refused microphone access, from
	// RequestAccess or OpenInput. Errors that wrap it also wrap fs.ErrPermission.
	ErrPermission = errors.New("koloaudio: microphone access not granted")
)

// Format limits and defaults.
const (
	// MinSampleRate and MaxSampleRate bound Format.SampleRate in Hz.
	MinSampleRate = 8000
	MaxSampleRate = 384000
	// MaxChannels bounds Format.Channels.
	MaxChannels = 254

	// DefaultSampleRate is used when Format.SampleRate is zero; Opus runs at 48 kHz.
	DefaultSampleRate = 48000
	// DefaultChannels is used when Format.Channels is zero (mono).
	DefaultChannels = 1
	// DefaultPeriod is the device callback period when Config.Period is zero.
	DefaultPeriod = 10 * time.Millisecond
	// DefaultBuffer is the stream buffer when Config.Buffer is zero; it holds
	// one Opus packet of the maximum duration, 120 ms.
	DefaultBuffer = 120 * time.Millisecond
	// MaxBuffer bounds Config.Buffer; it is the longest default buffer
	// among the supported sound servers.
	MaxBuffer = 2 * time.Second
	// DefaultStallTimeout is used when Config.StallTimeout is zero. It leaves
	// room for platforms that delay the first callback after resume from sleep.
	DefaultStallTimeout = 12 * time.Second
)

// Format is a PCM format: interleaved signed 16-bit samples.
type Format struct {
	// SampleRate in Hz, MinSampleRate to MaxSampleRate. Zero means DefaultSampleRate.
	SampleRate int
	// Channels, 1 to MaxChannels. Zero means DefaultChannels.
	Channels int
}

func (f Format) withDefaults() Format {
	if f.SampleRate == 0 {
		f.SampleRate = DefaultSampleRate
	}
	if f.Channels == 0 {
		f.Channels = DefaultChannels
	}
	return f
}

func (f Format) validate() error {
	if f.SampleRate < MinSampleRate || f.SampleRate > MaxSampleRate {
		return fmt.Errorf("%w: sample rate %d Hz", ErrUnsupportedFormat, f.SampleRate)
	}
	if f.Channels < 1 || f.Channels > MaxChannels {
		return fmt.Errorf("%w: %d channels", ErrUnsupportedFormat, f.Channels)
	}
	return nil
}

// frames converts d to whole frames, rounding up, or returns -1 on overflow.
func (f Format) frames(d time.Duration) int {
	if d < 0 || int64(d) > (math.MaxInt64-int64(time.Second))/int64(f.SampleRate) {
		return -1
	}
	n := (int64(d)*int64(f.SampleRate) + int64(time.Second) - 1) / int64(time.Second)
	if n > math.MaxInt32 {
		return -1
	}
	return int(n)
}

// Kind selects capture (input) or playback (output) devices.
type Kind int

const (
	// Capture devices record audio.
	Capture Kind = iota + 1
	// Playback devices play audio.
	Playback
)

// String returns "capture" or "playback".
func (k Kind) String() string {
	switch k {
	case Capture:
		return "capture"
	case Playback:
		return "playback"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Device describes an audio device. ID and Name identify hardware on this
// machine (a Bluetooth ID can hold its address), so do not send them to peers.
type Device struct {
	// ID is opaque and stable while the system keeps the device's identifier.
	ID string
	// Name is the name the system shows for the device.
	Name string
	// Kind is Capture or Playback.
	Kind Kind
	// Default reports whether the device is the system default of its kind.
	Default bool
}

// Config configures OpenInput and OpenOutput. Zero fields take the defaults.
type Config struct {
	// DeviceID selects a device from Devices. Empty means the system default
	// device; on macOS and Windows the stream follows it when it changes.
	DeviceID string
	// Format of the samples passed to Read or Write. The device converts to
	// and from its native format.
	Format Format
	// Period is the requested device callback period. Zero means DefaultPeriod.
	// The backend may round it.
	Period time.Duration
	// Buffer is the capacity of the stream buffer between the device and the
	// application. Zero means DefaultBuffer. It must be at least Period and
	// at most MaxBuffer. The buffer holds Buffer times the sample rate times
	// Channels samples of 2 bytes, allocated when the stream opens.
	Buffer time.Duration
	// StallTimeout fails the stream with ErrDeviceLost when the device makes
	// no callback for this long. Zero means DefaultStallTimeout. It must be
	// longer than Period.
	StallTimeout time.Duration
}

func (c Config) withDefaults() (Config, error) {
	c.Format = c.Format.withDefaults()
	if err := c.Format.validate(); err != nil {
		return c, err
	}
	if c.Period == 0 {
		c.Period = DefaultPeriod
	}
	if c.Buffer == 0 {
		c.Buffer = DefaultBuffer
	}
	if c.StallTimeout == 0 {
		c.StallTimeout = DefaultStallTimeout
	}
	switch {
	case c.Period < 0 || c.Buffer < 0 || c.StallTimeout < 0:
		return c, fmt.Errorf("%w: negative duration", ErrInvalidConfig)
	case c.Format.frames(c.Period) < 1:
		return c, fmt.Errorf("%w: period %v", ErrInvalidConfig, c.Period)
	case c.Buffer < c.Period:
		return c, fmt.Errorf("%w: buffer %v shorter than period %v", ErrInvalidConfig, c.Buffer, c.Period)
	case c.Buffer > MaxBuffer:
		return c, fmt.Errorf("%w: buffer %v longer than %v", ErrInvalidConfig, c.Buffer, MaxBuffer)
	case c.StallTimeout <= c.Period:
		return c, fmt.Errorf("%w: stall timeout %v not longer than period %v", ErrInvalidConfig, c.StallTimeout, c.Period)
	}
	return c, nil
}

// Stats counts what happened on a stream since it was opened. Counts are in frames.
type Stats struct {
	// Frames moved through the device callback.
	Frames uint64
	// Overruns counts captured frames dropped, oldest first, because the
	// application did not read fast enough. Always zero on an Output.
	Overruns uint64
	// Underruns counts frames of silence played because the buffer was empty
	// after the first Write. Always zero on an Input.
	Underruns uint64
	// Buffered is the number of frames waiting in the stream buffer.
	Buffered int
}

// Devices lists the devices of the given kind. It never opens a device, asks
// for consent or changes process-wide state.
func Devices(kind Kind) ([]Device, error) {
	if kind != Capture && kind != Playback {
		return nil, fmt.Errorf("%w: kind %v", ErrInvalidArgument, kind)
	}
	return listDevices(kind)
}
