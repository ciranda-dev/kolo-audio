package koloaudio

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestPacerDue(t *testing.T) {
	start := time.Now()
	cases := []struct {
		rate, frames int64
		want         time.Duration
	}{
		{48000, 480, 10 * time.Millisecond},
		{48000, 48000, time.Second},
		{44100, 44100 * 3600, time.Hour},
		{44100, 441, 10 * time.Millisecond},
		{8000, 1, 125 * time.Microsecond},
		{384000, 3, 7812 * time.Nanosecond},
		{48000, 48000 * 3600 * 24 * 365, 365 * 24 * time.Hour},
		{44100, 44100*3600*24*365 + 1, 365*24*time.Hour + 22675},
	}
	for _, c := range cases {
		p := pacer{start: start, rate: c.rate}
		if got := p.due(c.frames).Sub(start); got != c.want {
			t.Errorf("rate %d, %d frames: %v, want %v", c.rate, c.frames, got, c.want)
		}
	}
}

// TestPacerNoDrift checks that periods that do not divide the second evenly
// do not accumulate rounding error: the last instant equals the exact one.
func TestPacerNoDrift(t *testing.T) {
	start := time.Now()
	p := pacer{start: start, rate: 44100}
	const period = 441
	var last time.Duration
	for k := int64(1); k <= 100_000; k++ {
		d := p.due(k * period).Sub(start)
		if d <= last {
			t.Fatalf("instant %d went back: %v after %v", k, d, last)
		}
		last = d
	}
	if want := 1000 * time.Second; last != want {
		t.Fatalf("100000 periods of 441 frames at 44.1 kHz end at %v, want %v", last, want)
	}
}

func TestNullPacing(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		synctest.Test(t, func(t *testing.T) {
			s, err := openStream(kind, Config{Period: 5 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			time.Sleep(time.Second + time.Millisecond)
			if f := s.getStats().Frames; f != 48000 {
				t.Errorf("%v: %d frames in 1 s, want 48000", kind, f)
			}
		})
	}
}

func TestNullNoCallbackAfterClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := openStream(Capture, Config{Period: time.Millisecond, Buffer: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20*time.Millisecond + time.Microsecond)
		s.close()
		n := s.getStats().Frames
		time.Sleep(20 * time.Millisecond)
		if got := s.getStats().Frames; got != n || n != 20*48 {
			t.Fatalf("frames %d after close, %d at close, want %d", got, n, 20*48)
		}
	})
}

func TestNullStop(t *testing.T) {
	s, err := openStream(Playback, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.hw.stop()
	n := s.getStats().Frames
	time.Sleep(30 * time.Millisecond)
	if got := s.getStats().Frames; got != n {
		t.Fatalf("frames grew from %d to %d after stop", n, got)
	}
	if _, err := s.write(t.Context(), make([]int16, 2)); !errors.Is(err, ErrDeviceLost) {
		t.Fatalf("Write after stop = %v, want ErrDeviceLost", err)
	}
	s.hw.stop()
}

// lagRun drives a schedule on a virtual clock through a capture stream. A
// stall of lag follows period stallAt. The reader takes what is buffered
// whenever the device waits, never in the middle of a burst of late
// periods, which is the worst case for the buffer. It returns the stream
// stats and the longest run of late periods.
func lagRun(rate int, period, buffer, lag time.Duration) (Stats, int) {
	f := Format{SampleRate: rate, Channels: 1}
	s := &stream{kind: Capture, format: f, buf: ring{data: make([]int16, f.frames(buffer))}, signal: make(chan struct{}, 1)}
	frames := f.frames(period)
	t0 := time.Now()
	sc := schedule{pacer: pacer{start: t0, rate: int64(rate)}, frames: int64(frames), maxLag: lagLimit(period, buffer)}
	chunk, rd := make([]int16, frames), make([]int16, len(s.buf.data))
	now := t0
	late, longest := 0, 0
	for k := range 100 {
		wait := sc.next(now)
		if wait > 0 {
			s.mu.Lock()
			s.buf.read(rd)
			s.mu.Unlock()
			late = 0
		} else {
			late++
			longest = max(longest, late)
		}
		now = now.Add(wait)
		if k == 40 {
			now = now.Add(lag)
		}
		s.onCapture(chunk)
	}
	return s.getStats(), longest
}

// TestNullLagBurst checks that no stall, however long, makes a reader that
// is only late by the stall overrun: a stall up to the lag limit is caught
// up in a burst that fits in the buffer, a longer one restarts the schedule.
func TestNullLagBurst(t *testing.T) {
	for _, c := range []struct{ period, buffer time.Duration }{
		{10 * time.Millisecond, 120 * time.Millisecond},
		{5 * time.Millisecond, 50 * time.Millisecond},
		{20 * time.Millisecond, 2 * time.Second},
		{10 * time.Millisecond, 30 * time.Millisecond},
		{10 * time.Millisecond, 20 * time.Millisecond},
		{10 * time.Millisecond, 10 * time.Millisecond},
	} {
		maxLag := lagLimit(c.period, c.buffer)
		for _, lag := range []time.Duration{0, c.period / 2, c.period, maxLag, maxLag + time.Nanosecond, c.buffer, 10 * c.buffer, time.Hour} {
			st, longest := lagRun(48000, c.period, c.buffer, lag)
			if want := uint64(100 * 48000 * c.period / time.Second); st.Frames != want {
				t.Errorf("period %v buffer %v lag %v: %d frames, want %d", c.period, c.buffer, lag, st.Frames, want)
			}
			if longest > int(maxLag/c.period)+1 {
				t.Errorf("period %v buffer %v lag %v: %d late periods in a row, limit %d", c.period, c.buffer, lag, longest, maxLag/c.period+1)
			}
			if c.buffer >= 4*c.period && st.Overruns != 0 {
				t.Errorf("period %v buffer %v lag %v: %d overruns", c.period, c.buffer, lag, st.Overruns)
			}
		}
	}
}

func TestNullLagResync(t *testing.T) {
	now := time.Now()
	// A schedule one hour in the past would replay 360000 periods if the
	// lag were not bounded.
	sc := schedule{pacer: pacer{start: now.Add(-time.Hour), rate: 48000}, frames: 480, maxLag: 20 * time.Millisecond}
	if w := sc.next(now); w != 0 {
		t.Fatalf("first wait %v, want 0", w)
	}
	if w := sc.next(now); w != 10*time.Millisecond {
		t.Fatalf("second wait %v, want one period", w)
	}
	if got := sc.due(sc.delivered).Sub(now); got != 10*time.Millisecond {
		t.Fatalf("next period due in %v, want 10ms", got)
	}
}

// TestNullLagNotReplayed runs a device whose schedule starts an hour in the
// past through the real goroutine: it delivers on the clock, not 360000
// periods at once.
func TestNullLagNotReplayed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := Format{SampleRate: 48000, Channels: 1}
		s := &stream{kind: Capture, format: f, buf: ring{data: make([]int16, 4800)}, signal: make(chan struct{}, 1)}
		n := &nullStream{s: s, frames: 480, buf: make([]int16, 480),
			quit: make(chan struct{}), halt: make(chan struct{}), done: make(chan struct{})}
		go n.run(schedule{pacer: pacer{start: time.Now().Add(-time.Hour), rate: 48000}, frames: 480, maxLag: 20 * time.Millisecond})
		time.Sleep(100*time.Millisecond + time.Microsecond)
		n.close()
		if got := s.getStats().Frames; got != 11*480 {
			t.Fatalf("%d frames, want %d", got, 11*480)
		}
	})
}

// TestPlaybackUnderrunIsSilence checks that frames the application did not queue play as zeros:
// the system's render buffer holds stale audio that must never reach the speakers.
func TestPlaybackUnderrunIsSilence(t *testing.T) {
	f := Format{SampleRate: 48000, Channels: 2}
	s := &stream{kind: Playback, format: f, buf: ring{data: make([]int16, 4800)}, signal: make(chan struct{}, 1), written: true}
	s.buf.write([]int16{1, 2, 3, 4, 5, 6})
	out := make([]int16, 20)
	for i := range out {
		out[i] = 0x5a5a
	}
	s.onPlayback(out)
	want := []int16{1, 2, 3, 4, 5, 6}
	for i, v := range out {
		if i < len(want) && v != want[i] || i >= len(want) && v != 0 {
			t.Fatalf("out[%d] = %#x, want %v then zeros: %v", i, v, want, out)
		}
	}
	if s.stats.Underruns != 7 {
		t.Errorf("underruns %d, want 7 frames", s.stats.Underruns)
	}
}

// TestCleanupDoesNotWaitForBackend checks that closing an abandoned stream leaves the cleanup goroutine
// at once, even when the backend is slow to stop.
func TestCleanupDoesNotWaitForBackend(t *testing.T) {
	s := &stream{quit: make(chan struct{}), exited: make(chan struct{}), done: make(chan struct{})}
	returned := make(chan struct{})
	go func() {
		closeStream(s)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("closeStream waited for the backend")
	}
	close(s.exited)
	waitClosed(t, s.quit)
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed")
	}
}
