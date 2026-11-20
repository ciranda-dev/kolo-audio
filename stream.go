package koloaudio

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// stream is the state shared by Input and Output. The device callback and
// the application meet at buf, guarded by mu; the callback never waits for
// the application beyond that short lock.
type stream struct {
	kind   Kind
	format Format
	stall  time.Duration

	mu      sync.Mutex
	buf     ring
	stats   Stats
	written bool
	err     error
	done    chan struct{}
	signal  chan struct{}

	epoch   time.Time
	last    atomic.Int64
	closing atomic.Bool

	quit      chan struct{}
	exited    chan struct{}
	closeOnce sync.Once

	hw hwStream
}

func openStream(kind Kind, cfg Config) (*stream, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	s := &stream{
		kind:   kind,
		format: cfg.Format,
		stall:  cfg.StallTimeout,
		buf:    ring{data: make([]int16, cfg.Format.frames(cfg.Buffer)*cfg.Format.Channels)},
		done:   make(chan struct{}),
		signal: make(chan struct{}, 1),
		epoch:  time.Now(),
		quit:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	ready := make(chan error)
	go s.run(cfg, ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return s, nil
}

// run owns the device for its whole life on one locked OS thread, because
// some backends (COM on Windows) bind setup and teardown to the thread.
// It also watches for stalls.
func (s *stream) run(cfg Config, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.exited)
	if err := s.openDevice(cfg); err != nil {
		ready <- captureDenied(s.kind, err)
		return
	}
	s.touch()
	ready <- nil
	s.watch()
	s.closeDevice()
}

func (s *stream) touch() {
	s.last.Store(int64(time.Since(s.epoch)))
}

func (s *stream) poke() {
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// onCapture runs on the audio thread with the frames the device recorded.
func (s *stream) onCapture(in []int16) {
	s.touch()
	ch := s.format.Channels
	s.mu.Lock()
	dropped := s.buf.overwrite(in)
	s.stats.Frames += uint64(len(in) / ch)
	s.stats.Overruns += uint64(dropped / ch)
	s.mu.Unlock()
	s.poke()
}

// onPlayback runs on the audio thread and fills out with queued frames.
func (s *stream) onPlayback(out []int16) {
	s.touch()
	ch := s.format.Channels
	s.mu.Lock()
	n := s.buf.read(out)
	s.stats.Frames += uint64(len(out) / ch)
	if s.written {
		s.stats.Underruns += uint64((len(out) - n) / ch)
	}
	s.mu.Unlock()
	clear(out[n:])
	s.poke()
}

// onStopped runs when the backend reports that the device stopped.
func (s *stream) onStopped() {
	if !s.closing.Load() {
		s.fail(ErrDeviceLost)
	}
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		close(s.done)
	}
	if s.err == nil || err == ErrClosed {
		s.err = err
	}
	s.mu.Unlock()
}

// watch fails the stream when callbacks stop for the stall timeout and
// returns when the stream is closed.
func (s *stream) watch() {
	t := time.NewTicker(max(s.stall/4, time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			if time.Since(s.epoch)-time.Duration(s.last.Load()) > s.stall {
				s.fail(ErrDeviceLost)
			}
		}
	}
}

func (s *stream) checkLen(p []int16) error {
	if len(p) == 0 || len(p)%s.format.Channels != 0 {
		return fmt.Errorf("%w: %d samples is not a whole number of %d-channel frames", ErrInvalidArgument, len(p), s.format.Channels)
	}
	return nil
}

func (s *stream) read(ctx context.Context, p []int16) (int, error) {
	if err := s.checkLen(p); err != nil {
		return 0, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return 0, err
		}
		if s.buf.n > 0 {
			n := s.buf.read(p)
			s.mu.Unlock()
			return n, nil
		}
		s.mu.Unlock()
		select {
		case <-s.signal:
		case <-s.done:
		case <-ctx.Done():
		}
	}
}

func (s *stream) write(ctx context.Context, p []int16) (int, error) {
	if err := s.checkLen(p); err != nil {
		return 0, err
	}
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return total, err
		}
		if n := s.buf.write(p[total:]); n > 0 {
			s.written = true
			total += n
		}
		s.mu.Unlock()
		if total == len(p) {
			return total, nil
		}
		select {
		case <-s.signal:
		case <-s.done:
		case <-ctx.Done():
		}
	}
}

func (s *stream) getStats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Buffered = s.buf.n / s.format.Channels
	return st
}

func (s *stream) close() error {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		s.fail(ErrClosed)
		close(s.quit)
		<-s.exited
	})
	return nil
}

// Input captures audio from a device. Its methods are safe for concurrent
// use; concurrent Reads split the captured stream between them.
type Input struct{ s *stream }

// OpenInput opens and starts a capture device. A refusal of microphone access
// returns ErrPermission, which also matches ErrBackend and fs.ErrPermission.
// On macOS it reads AccessStatus first; call RequestAccess before it.
func OpenInput(cfg Config) (*Input, error) {
	s, err := openStream(Capture, cfg)
	if err != nil {
		return nil, err
	}
	in := &Input{s}
	runtime.AddCleanup(in, closeStream, s)
	return in, nil
}

// Read blocks until at least one frame is captured, then copies up to
// len(p) samples and returns the count. len(p) must be a positive multiple
// of the channel count. It returns ctx.Err(), ErrClosed or ErrDeviceLost.
func (in *Input) Read(ctx context.Context, p []int16) (int, error) {
	if in == nil || in.s == nil {
		return 0, ErrClosed
	}
	return in.s.read(ctx, p)
}

// Format returns the format of the samples Read returns.
func (in *Input) Format() Format {
	if in == nil || in.s == nil {
		return Format{}
	}
	return in.s.format
}

// Stats returns the stream counters.
func (in *Input) Stats() Stats {
	if in == nil || in.s == nil {
		return Stats{}
	}
	return in.s.getStats()
}

// Close stops the device and releases it. Pending and later calls return
// ErrClosed. Close is idempotent and always returns nil. An Input that
// becomes unreachable without Close is closed by the garbage collector,
// which may take long; call Close.
func (in *Input) Close() error {
	if in == nil || in.s == nil {
		return nil
	}
	return in.s.close()
}

// Output plays audio on a device. Its methods are safe for concurrent use;
// concurrent Writes may interleave their samples.
type Output struct{ s *stream }

// OpenOutput opens and starts a playback device. It plays silence until the
// first Write.
func OpenOutput(cfg Config) (*Output, error) {
	s, err := openStream(Playback, cfg)
	if err != nil {
		return nil, err
	}
	out := &Output{s}
	runtime.AddCleanup(out, closeStream, s)
	return out, nil
}

// Write queues p for playback, blocking while the buffer is full, and
// returns the number of samples queued. len(p) must be a positive multiple
// of the channel count. On ctx.Err(), ErrClosed or ErrDeviceLost the count
// tells how much was queued.
func (out *Output) Write(ctx context.Context, p []int16) (int, error) {
	if out == nil || out.s == nil {
		return 0, ErrClosed
	}
	return out.s.write(ctx, p)
}

// Format returns the format Write expects.
func (out *Output) Format() Format {
	if out == nil || out.s == nil {
		return Format{}
	}
	return out.s.format
}

// Stats returns the stream counters.
func (out *Output) Stats() Stats {
	if out == nil || out.s == nil {
		return Stats{}
	}
	return out.s.getStats()
}

// Close stops the device and releases it, discarding queued audio. Pending
// and later calls return ErrClosed. Close is idempotent and always returns
// nil. An Output that becomes unreachable without Close is closed by the
// garbage collector, which may take long; call Close.
func (out *Output) Close() error {
	if out == nil || out.s == nil {
		return nil
	}
	return out.s.close()
}

// closeStream runs on the runtime's one cleanup goroutine, so a backend that
// is slow to stop must not hold it.
func closeStream(s *stream) { go s.close() }
