package maresample

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
)

// stageSamples is the size of each staging buffer, in samples. It bounds the
// linear memory the module needs, whatever the size of the caller's slices.
const stageSamples = 16384

// Miniaudio's MA_INVALID_ARGS and MA_OUT_OF_MEMORY result codes.
const (
	invalidArgs = -2
	outOfMemory = -4
)

// ResultError is a miniaudio result code other than success.
type ResultError int32

// Error implements error.
func (e ResultError) Error() string { return fmt.Sprintf("miniaudio result %d", int32(e)) }

// OutOfMemory reports whether the module ran out of its bounded memory.
func (e ResultError) OutOfMemory() bool { return e == outOfMemory }

// Trap is the panic of the module (bounds violation, unreachable, exit).
type Trap struct{ Value any }

// Error implements error.
func (t *Trap) Error() string { return fmt.Sprint("trap: ", t.Value) }

// Engine is one miniaudio linear resampler in its own wasm module. It is not
// safe for concurrent use. After a trap the next call builds a new module
// with the same configuration, so the filter state starts again.
type Engine struct {
	channels, in, out int
	frames            int
	m                 *Module
	r, inAddr         int32
	outAddr, counts   int32
	fault             any
}

// Fault makes the next call into the module panic with v, as a trap would.
// Tests use it.
func (e *Engine) Fault(v any) { e.fault = v }

// NewEngine returns an Engine, or a ResultError when miniaudio refuses the
// configuration.
func NewEngine(channels, inRate, outRate int) (*Engine, error) {
	if channels < 1 {
		return nil, ResultError(invalidArgs)
	}
	e := &Engine{channels: channels, in: inRate, out: outRate, frames: max(1, stageSamples/channels)}
	if err := e.build(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) build() error {
	var res error
	if err := e.guard(func() {
		m := New(wasihost.Host{})
		m.X_initialize()
		r := m.Xkolo_rs_new(int32(e.channels), int32(e.in), int32(e.out))
		if r < 0 {
			res = ResultError(r)
			return
		}
		inAddr := m.Xkolo_malloc(int32(2 * e.frames * e.channels))
		outAddr := m.Xkolo_malloc(int32(2 * e.frames * e.channels))
		counts := m.Xkolo_malloc(8)
		if inAddr == 0 || outAddr == 0 || counts == 0 {
			res = ResultError(outOfMemory)
			return
		}
		e.m, e.r, e.inAddr, e.outAddr, e.counts = m, r, inAddr, outAddr, counts
	}); err != nil {
		return err
	}
	return res
}

// guard runs f and turns a panic into a Trap that discards the module.
func (e *Engine) guard(f func()) (err error) {
	defer func() {
		if v := recover(); v != nil {
			e.m = nil
			err = &Trap{v}
		}
	}()
	if v := e.fault; v != nil {
		e.fault = nil
		panic(v)
	}
	f()
	return nil
}

// use runs f on a live module, rebuilding it first after a trap.
func (e *Engine) use(f func()) error {
	if e.m == nil {
		if err := e.build(); err != nil {
			return err
		}
	}
	return e.guard(f)
}

// Process resamples at most inFrames frames of src into at most outFrames
// frames of dst and returns the frames written and consumed. It stops when dst
// is full or the input is used up. A nil src feeds silence.
func (e *Engine) Process(dst, src []int16, outFrames, inFrames int) (written, consumed int, err error) {
	ch := e.channels
	var perr error
	err = e.use(func() {
		for written < outFrames {
			nout := min(outFrames-written, e.frames)
			// Feeding about what nout frames need keeps many small calls cheap.
			nin := min(inFrames-consumed, e.frames, int(int64(nout)*int64(e.in)/int64(e.out))+4)
			mem := *e.m.Slice()
			in := mem[e.inAddr : int(e.inAddr)+2*nin*ch]
			if src == nil {
				clear(in)
			}
			for i := 0; src != nil && i < nin*ch; i++ {
				binary.LittleEndian.PutUint16(in[2*i:], uint16(src[consumed*ch+i]))
			}
			if res := e.m.Xkolo_rs_process(e.r, e.inAddr, int32(nin), e.outAddr, int32(nout), e.counts); res != 0 {
				perr = ResultError(res)
				return
			}
			mem = *e.m.Slice()
			c := int(binary.LittleEndian.Uint32(mem[e.counts:]))
			w := int(binary.LittleEndian.Uint32(mem[e.counts+4:]))
			out := mem[e.outAddr : int(e.outAddr)+2*w*ch]
			for i := 0; i < w*ch; i++ {
				dst[written*ch+i] = int16(binary.LittleEndian.Uint16(out[2*i:]))
			}
			written += w
			consumed += c
			if w+c == 0 || consumed == inFrames && w < nout {
				return
			}
		}
	})
	if err == nil {
		err = perr
	}
	if err != nil {
		return 0, 0, err
	}
	return written, consumed, nil
}

// Expected returns the output frames for inFrames input frames, saturating at
// math.MaxInt.
func (e *Engine) Expected(inFrames int) (n int, err error) {
	err = e.use(func() {
		n = int(min(uint64(e.m.Xkolo_rs_expected(e.r, int64(inFrames))), math.MaxInt))
	})
	return n, err
}

// InputLatency returns the delay in input frames.
func (e *Engine) InputLatency() (n int, err error) {
	err = e.use(func() { n = int(e.m.Xkolo_rs_input_latency(e.r)) })
	return n, err
}

// OutputLatency returns the delay in output frames.
func (e *Engine) OutputLatency() (n int, err error) {
	err = e.use(func() { n = int(e.m.Xkolo_rs_output_latency(e.r)) })
	return n, err
}

// Reset clears the filter state.
func (e *Engine) Reset() error {
	return e.use(func() { e.m.Xkolo_rs_reset(e.r) })
}
