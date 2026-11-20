package koloaudio

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/ciranda-dev/kolo-audio/internal/maresample"
)

// Resampler converts interleaved PCM from one sample rate to another with
// linear interpolation and a 4th-order low-pass filter. It keeps state
// between calls, so a stream can be fed in chunks of any size. Its methods
// are safe for concurrent use. A call that fails inside the resampler returns
// ErrBackend, and the next call starts again from a cleared filter state.
type Resampler struct {
	mu       sync.Mutex
	channels int
	eng      *maresample.Engine
}

// NewResampler returns a Resampler for the given channel count and rates in Hz.
func NewResampler(channels, inRate, outRate int) (*Resampler, error) {
	for _, f := range []Format{{inRate, channels}, {outRate, channels}} {
		if err := f.validate(); err != nil {
			return nil, err
		}
	}
	eng, err := maresample.NewEngine(channels, inRate, outRate)
	if err != nil {
		var res maresample.ResultError
		if errors.As(err, &res) && !res.OutOfMemory() {
			return nil, fmt.Errorf("%w: resampler: %v", ErrUnsupportedFormat, err)
		}
		return nil, fmt.Errorf("%w: resampler: %v", ErrBackend, err)
	}
	return &Resampler{channels: channels, eng: eng}, nil
}

func backendErr(err error) error {
	return fmt.Errorf("%w: resample: %v", ErrBackend, err)
}

func (r *Resampler) frames(p []int16, name string, minFrames int) (int, error) {
	if len(p)%r.channels != 0 || len(p)/r.channels < minFrames {
		return 0, fmt.Errorf("%w: %s holds %d samples, not a whole number of %d-channel frames (at least %d)", ErrInvalidArgument, name, len(p), r.channels, minFrames)
	}
	return len(p) / r.channels, nil
}

// Process resamples src into dst and returns the number of samples written
// and consumed. It stops when dst is full or src is used up; pass the
// unconsumed rest of src again with the next call. dst must hold at least
// one frame.
func (r *Resampler) Process(dst, src []int16) (written, consumed int, err error) {
	if r == nil {
		return 0, 0, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eng == nil {
		return 0, 0, ErrClosed
	}
	out, err := r.frames(dst, "dst", 1)
	if err != nil {
		return 0, 0, err
	}
	in, err := r.frames(src, "src", 0)
	if err != nil {
		return 0, 0, err
	}
	o, i, err := r.eng.Process(dst, src, out, in)
	if err != nil {
		return 0, 0, backendErr(err)
	}
	return o * r.channels, i * r.channels, nil
}

// OutputLen returns the number of samples Process writes when it consumes
// n input samples, given the current state. It saturates at math.MaxInt.
func (r *Resampler) OutputLen(n int) int {
	if r == nil || n <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eng == nil {
		return 0
	}
	frames, err := r.eng.Expected(n / r.channels)
	if err != nil {
		return 0
	}
	if frames > math.MaxInt/r.channels {
		return math.MaxInt / r.channels * r.channels
	}
	return frames * r.channels
}

// Latency returns the delay the filter adds, in output frames.
func (r *Resampler) Latency() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eng == nil {
		return 0
	}
	n, err := r.eng.OutputLatency()
	if err != nil {
		return 0
	}
	return n
}

// Flush writes the output still held back by the filter delay, as if the
// input were followed by silence, then resets the Resampler. A dst of
// (Latency()+1)*channels samples always suffices; Flush returns the samples
// written.
func (r *Resampler) Flush(dst []int16) (int, error) {
	if r == nil {
		return 0, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eng == nil {
		return 0, ErrClosed
	}
	pending, err := r.eng.InputLatency()
	if err != nil {
		return 0, backendErr(err)
	}
	need, err := r.eng.Expected(pending)
	if err != nil {
		return 0, backendErr(err)
	}
	if len(dst) < need*r.channels {
		return 0, fmt.Errorf("%w: dst holds %d samples, need %d", ErrInvalidArgument, len(dst), need*r.channels)
	}
	o := 0
	if need > 0 {
		if o, _, err = r.eng.Process(dst[:need*r.channels], nil, need, pending); err != nil {
			return 0, backendErr(err)
		}
	}
	if err := r.eng.Reset(); err != nil {
		return 0, backendErr(err)
	}
	return o * r.channels, nil
}

// Reset clears the filter state, dropping any held-back output.
func (r *Resampler) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eng != nil {
		_ = r.eng.Reset()
	}
}

// Close releases the Resampler. Later calls return ErrClosed. Close is
// idempotent and always returns nil.
func (r *Resampler) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eng = nil
	return nil
}
