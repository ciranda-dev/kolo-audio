package koloaudio

import (
	"sync"
	"sync/atomic"
	"time"
)

const nullID = "00"

func nullList(kind Kind) []Device {
	return []Device{{ID: nullID, Name: "NULL " + kind.String() + " Device", Kind: kind, Default: true}}
}

// pacer maps a frame count to the instant that audio ends on the clock.
// Every instant is computed from the start, never from the previous one, so
// timer lateness does not accumulate: the frames delivered by time t are
// always within one period of t times the rate.
type pacer struct {
	start time.Time
	rate  int64
}

// due returns the instant at which frames frames have been played since start.
func (p pacer) due(frames int64) time.Time {
	sec, rem := frames/p.rate, frames%p.rate
	return p.start.Add(time.Duration(sec)*time.Second + time.Duration(rem*int64(time.Second)/p.rate))
}

// schedule is the pacer plus the periods delivered so far.
type schedule struct {
	pacer
	frames    int64
	maxLag    time.Duration
	delivered int64
}

// next advances to the next period and returns how long to wait for it at
// now. A device cannot be behind by more than its buffer without an xrun,
// so after a lag longer than maxLag (the process was suspended) the
// schedule restarts at now instead of replaying the missed periods.
func (c *schedule) next(now time.Time) time.Duration {
	c.delivered += c.frames
	wait := c.due(c.delivered).Sub(now)
	if wait < -c.maxLag {
		c.start = c.start.Add(-wait)
		wait = 0
	}
	return max(wait, 0)
}

// nullStream is a virtual device driven by the Go monotonic clock. Capture
// yields silence and playback discards. One goroutine makes every callback,
// once per period at the instant that period's audio would have been recorded
// or consumed.
type nullStream struct {
	s       *stream
	frames  int
	buf     []int16
	paused  atomic.Bool // callbacks are skipped, as for a stalled device; tests set it
	quit    chan struct{}
	halt    chan struct{}
	done    chan struct{}
	quitOne sync.Once
	haltOne sync.Once
}

func openNull(s *stream, cfg Config) (hwStream, error) {
	n := &nullStream{
		s:      s,
		frames: cfg.Format.frames(cfg.Period),
		quit:   make(chan struct{}),
		halt:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	n.buf = make([]int16, n.frames*cfg.Format.Channels)
	sc := schedule{pacer: pacer{start: time.Now(), rate: int64(cfg.Format.SampleRate)}, frames: int64(n.frames), maxLag: lagLimit(cfg.Period, cfg.Buffer)}
	go n.run(sc)
	return n, nil
}

// lagLimit is the longest stall that is caught up with back to back
// periods. The buffer holds that burst and still has room for the period the
// stall interrupted and the one its reader has not taken yet, so it is the
// buffer minus three periods, and at least one period so that timer lateness
// never moves the schedule.
func lagLimit(period, buffer time.Duration) time.Duration {
	return max(buffer-3*period, period)
}

// run delivers one period at a time. A stall longer than the lag limit
// restarts the schedule. A reader that is slower than the clock overruns
// (capture) or underruns (playback), as on a real device.
func (n *nullStream) run(sc schedule) {
	defer close(n.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		wait := sc.next(time.Now())
		timer.Reset(max(wait, 0))
		select {
		case <-timer.C:
		case <-n.quit:
			return
		case <-n.halt:
			n.s.onStopped()
			return
		}
		if n.paused.Load() {
			continue
		}
		if n.s.kind == Capture {
			clear(n.buf)
			n.s.onCapture(n.buf)
		} else {
			n.s.onPlayback(n.buf)
		}
	}
}

func (n *nullStream) close() {
	n.quitOne.Do(func() { close(n.quit) })
	<-n.done
}

func (n *nullStream) stop() {
	n.haltOne.Do(func() { close(n.halt) })
	<-n.done
}
