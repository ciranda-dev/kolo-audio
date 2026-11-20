package koloaudio_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	koloaudio "github.com/ciranda-dev/kolo-audio"
)

func TestMain(m *testing.M) {
	koloaudio.SetNullBackend(true)
	os.Exit(m.Run())
}

// checkLeaks fails the test if devices or goroutines outlive it.
func checkLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	devices := koloaudio.LiveDevices()
	t.Cleanup(func() {
		if n := koloaudio.LiveDevices(); n != devices {
			t.Errorf("live devices = %d, want %d", n, devices)
		}
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > before {
			t.Errorf("goroutines = %d, want at most %d", n, before)
		}
	})
}

// inClock runs f on a fake clock: time.Now, Sleep, timers and the null
// backend's schedule advance only when every goroutine is blocked, so the
// result does not depend on the machine.
func inClock(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	checkLeaks(t)
	synctest.Test(t, f)
}

func openInput(t *testing.T, cfg koloaudio.Config) *koloaudio.Input {
	t.Helper()
	in, err := koloaudio.OpenInput(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close() })
	return in
}

func openOutput(t *testing.T, cfg koloaudio.Config) *koloaudio.Output {
	t.Helper()
	out, err := koloaudio.OpenOutput(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNilStreams(t *testing.T) {
	var in *koloaudio.Input
	var out koloaudio.Output
	if _, err := in.Read(context.Background(), make([]int16, 2)); !errors.Is(err, koloaudio.ErrClosed) {
		t.Errorf("nil Input Read = %v", err)
	}
	if _, err := out.Write(context.Background(), make([]int16, 2)); !errors.Is(err, koloaudio.ErrClosed) {
		t.Errorf("zero Output Write = %v", err)
	}
	if in.Close() != nil || out.Close() != nil || in.Stats() != (koloaudio.Stats{}) || out.Format() != (koloaudio.Format{}) {
		t.Error("nil or zero stream misbehaves")
	}
}

func TestErrUnsupportedMessage(t *testing.T) {
	want := "koloaudio: not supported on " + runtime.GOOS + "/" + runtime.GOARCH
	if got := koloaudio.ErrUnsupported.Error(); got != want {
		t.Fatalf("ErrUnsupported = %q, want %q", got, want)
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[koloaudio.Kind]string{koloaudio.Capture: "capture", koloaudio.Playback: "playback", 7: "Kind(7)"} {
		if got := k.String(); got != want {
			t.Errorf("%d: %q, want %q", int(k), got, want)
		}
	}
}

func TestUnclosedStreamIsReleased(t *testing.T) {
	before := koloaudio.LiveDevices()
	func() {
		if _, err := koloaudio.OpenInput(koloaudio.Config{}); err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, "the garbage collector to close the stream", func() bool {
		runtime.GC()
		return koloaudio.LiveDevices() == before
	})
}

func TestDevicesNull(t *testing.T) {
	for _, kind := range []koloaudio.Kind{koloaudio.Capture, koloaudio.Playback} {
		devs, err := koloaudio.Devices(kind)
		if err != nil {
			t.Fatal(err)
		}
		if len(devs) != 1 || devs[0].ID != "00" || !devs[0].Default || devs[0].Kind != kind || devs[0].Name == "" {
			t.Fatalf("%v devices: %+v", kind, devs)
		}
	}
	if _, err := koloaudio.Devices(0); !errors.Is(err, koloaudio.ErrInvalidArgument) {
		t.Fatalf("Devices(0) = %v, want ErrInvalidArgument", err)
	}
}

func TestCapture(t *testing.T) {
	inClock(t, func(t *testing.T) {
		in := openInput(t, koloaudio.Config{Format: koloaudio.Format{SampleRate: 16000, Channels: 2}})
		if f := in.Format(); f != (koloaudio.Format{SampleRate: 16000, Channels: 2}) {
			t.Fatalf("format %+v", f)
		}
		buf := make([]int16, 640)
		start := time.Now()
		frames := 0
		for frames < 8000 {
			n, err := in.Read(context.Background(), buf)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 || n%2 != 0 {
				t.Fatalf("Read returned %d samples", n)
			}
			for _, v := range buf[:n] {
				if v != 0 {
					t.Fatalf("null capture returned %d, want silence", v)
				}
			}
			frames += n / 2
		}
		// 8000 frames at 16 kHz are 500 ms of audio.
		if d := time.Since(start); d != 500*time.Millisecond {
			t.Fatalf("500 ms of capture took %v", d)
		}
		st := in.Stats()
		if st.Frames < 8000 || st.Underruns != 0 {
			t.Fatalf("stats %+v", st)
		}
	})
}

func TestPlayback(t *testing.T) {
	inClock(t, func(t *testing.T) {
		out := openOutput(t, koloaudio.Config{Buffer: 100 * time.Millisecond})
		pcm := tone(440, 48000, 1, 24000, 8000)
		start := time.Now()
		n, err := out.Write(context.Background(), pcm)
		if err != nil || n != len(pcm) {
			t.Fatalf("Write = %d, %v", n, err)
		}
		// 500 ms of audio through a 100 ms buffer: Write returns once 400 ms are played.
		if d := time.Since(start); d != 400*time.Millisecond {
			t.Fatalf("Write of 500 ms took %v", d)
		}
		waitFor(t, "buffer to drain", func() bool { return out.Stats().Buffered == 0 })
		if st := out.Stats(); st.Frames < 24000 {
			t.Fatalf("stats %+v", st)
		}
	})
}

func TestInvalidDeviceID(t *testing.T) {
	checkLeaks(t)
	for _, id := range []string{"zz", "01", "ff00", string(make([]byte, 1024)), strings.Repeat("00", 257)} {
		if _, err := koloaudio.OpenInput(koloaudio.Config{DeviceID: id}); !errors.Is(err, koloaudio.ErrNoDevice) {
			t.Errorf("OpenInput(%q) = %v, want ErrNoDevice", id, err)
		}
		if _, err := koloaudio.OpenOutput(koloaudio.Config{DeviceID: id}); !errors.Is(err, koloaudio.ErrNoDevice) {
			t.Errorf("OpenOutput(%q) = %v, want ErrNoDevice", id, err)
		}
	}
	in := openInput(t, koloaudio.Config{DeviceID: "00"})
	in.Close()
}

func TestInvalidConfig(t *testing.T) {
	checkLeaks(t)
	cases := []struct {
		cfg  koloaudio.Config
		want error
	}{
		{koloaudio.Config{Format: koloaudio.Format{SampleRate: 7999}}, koloaudio.ErrUnsupportedFormat},
		{koloaudio.Config{Format: koloaudio.Format{SampleRate: 384001}}, koloaudio.ErrUnsupportedFormat},
		{koloaudio.Config{Format: koloaudio.Format{SampleRate: -1}}, koloaudio.ErrUnsupportedFormat},
		{koloaudio.Config{Format: koloaudio.Format{Channels: 255}}, koloaudio.ErrUnsupportedFormat},
		{koloaudio.Config{Format: koloaudio.Format{Channels: -2}}, koloaudio.ErrUnsupportedFormat},
		{koloaudio.Config{Period: -time.Millisecond}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{Period: time.Duration(1 << 62), Buffer: time.Duration(1 << 62)}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{Buffer: 5 * time.Millisecond}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{Buffer: 24 * time.Hour}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{Buffer: time.Duration(1 << 62)}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{StallTimeout: -1}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{StallTimeout: koloaudio.DefaultPeriod}, koloaudio.ErrInvalidConfig},
		{koloaudio.Config{Buffer: koloaudio.MaxBuffer + 1}, koloaudio.ErrInvalidConfig},
	}
	for _, c := range cases {
		if _, err := koloaudio.OpenInput(c.cfg); !errors.Is(err, c.want) {
			t.Errorf("OpenInput(%+v) = %v, want %v", c.cfg, err, c.want)
		}
		if _, err := koloaudio.OpenOutput(c.cfg); !errors.Is(err, c.want) {
			t.Errorf("OpenOutput(%+v) = %v, want %v", c.cfg, err, c.want)
		}
	}
}

func TestPartialFrames(t *testing.T) {
	checkLeaks(t)
	cfg := koloaudio.Config{Format: koloaudio.Format{Channels: 2}}
	in := openInput(t, cfg)
	out := openOutput(t, cfg)
	for _, p := range [][]int16{nil, make([]int16, 1), make([]int16, 3)} {
		if _, err := in.Read(context.Background(), p); !errors.Is(err, koloaudio.ErrInvalidArgument) {
			t.Errorf("Read(%d) = %v", len(p), err)
		}
		if n, err := out.Write(context.Background(), p); n != 0 || !errors.Is(err, koloaudio.ErrInvalidArgument) {
			t.Errorf("Write(%d) = %d, %v", len(p), n, err)
		}
	}
}

func TestOverrun(t *testing.T) {
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{Buffer: 50 * time.Millisecond})
	waitFor(t, "overrun", func() bool { return in.Stats().Overruns > 0 })
	st := in.Stats()
	if st.Buffered != 2400 {
		t.Fatalf("buffered %d frames, want 2400", st.Buffered)
	}
	if st.Frames-st.Overruns != uint64(st.Buffered) {
		t.Fatalf("stats %+v: frames minus overruns must equal buffered", st)
	}
	buf := make([]int16, 4800)
	n, err := in.Read(context.Background(), buf)
	if err != nil || n < 2400 {
		t.Fatalf("Read = %d, %v", n, err)
	}
}

func TestUnderrun(t *testing.T) {
	checkLeaks(t)
	out := openOutput(t, koloaudio.Config{})
	waitFor(t, "playback callbacks", func() bool { return out.Stats().Frames > 4800 })
	if st := out.Stats(); st.Underruns != 0 {
		t.Fatalf("before the first Write: %+v", st)
	}
	if _, err := out.Write(context.Background(), make([]int16, 480)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "underrun", func() bool { return out.Stats().Underruns > 0 })
}

func TestWriteBackpressure(t *testing.T) {
	checkLeaks(t)
	out := openOutput(t, koloaudio.Config{Buffer: 100 * time.Millisecond})
	koloaudio.PauseCallbacks(out, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	n, err := out.Write(ctx, make([]int16, 48000))
	if !errors.Is(err, context.DeadlineExceeded) || n != 4800 {
		t.Fatalf("Write to a stalled full buffer = %d, %v; want 4800, deadline exceeded", n, err)
	}
	if st := out.Stats(); st.Buffered != 4800 {
		t.Fatalf("stats %+v", st)
	}
	koloaudio.PauseCallbacks(out, false)
	n, err = out.Write(context.Background(), make([]int16, 9600))
	if err != nil || n != 9600 {
		t.Fatalf("Write after resume = %d, %v", n, err)
	}
}

func TestReadCancel(t *testing.T) {
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{})
	koloaudio.PauseCallbacks(in, true)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	buf := make([]int16, 480)
	for {
		n, err := in.Read(ctx, buf)
		if errors.Is(err, context.Canceled) {
			break
		}
		if err != nil || n == 0 {
			t.Fatalf("Read = %d, %v", n, err)
		}
	}
	if _, err := in.Read(ctx, buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read with a cancelled context = %v", err)
	}
}

func TestCloseWhileBlocked(t *testing.T) {
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{})
	out := openOutput(t, koloaudio.Config{})
	koloaudio.PauseCallbacks(in, true)
	koloaudio.PauseCallbacks(out, true)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]int16, 480)
		for {
			if _, err := in.Read(context.Background(), buf); err != nil {
				if !errors.Is(err, koloaudio.ErrClosed) {
					t.Errorf("Read = %v, want ErrClosed", err)
				}
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		n, err := out.Write(context.Background(), make([]int16, 48000))
		if !errors.Is(err, koloaudio.ErrClosed) || n != 5760 {
			t.Errorf("Write = %d, %v; want 5760, ErrClosed", n, err)
		}
	}()
	waitFor(t, "a full output buffer", func() bool { return out.Stats().Buffered == 5760 })
	time.Sleep(20 * time.Millisecond)
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func TestDoubleClose(t *testing.T) {
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{})
	out := openOutput(t, koloaudio.Config{})
	for range 2 {
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := in.Read(context.Background(), make([]int16, 2)); !errors.Is(err, koloaudio.ErrClosed) {
		t.Fatalf("Read after Close = %v", err)
	}
	if _, err := out.Write(context.Background(), make([]int16, 2)); !errors.Is(err, koloaudio.ErrClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
	_ = in.Stats()
	_ = out.Stats()
}

func TestDeviceStopped(t *testing.T) {
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{})
	out := openOutput(t, koloaudio.Config{})
	if _, err := in.Read(context.Background(), make([]int16, 480)); err != nil {
		t.Fatal(err)
	}
	koloaudio.StopDevice(in)
	koloaudio.StopDevice(out)
	buf := make([]int16, 480)
	waitFor(t, "ErrDeviceLost from Read", func() bool {
		_, err := in.Read(context.Background(), buf)
		return errors.Is(err, koloaudio.ErrDeviceLost)
	})
	if _, err := out.Write(context.Background(), make([]int16, 48000)); !errors.Is(err, koloaudio.ErrDeviceLost) {
		t.Fatalf("Write = %v, want ErrDeviceLost", err)
	}
	in.Close()
	if _, err := in.Read(context.Background(), buf); !errors.Is(err, koloaudio.ErrClosed) {
		t.Fatalf("Read after Close = %v, want ErrClosed", err)
	}
}

func TestStallTimeout(t *testing.T) {
	inClock(t, func(t *testing.T) {
		cfg := koloaudio.Config{StallTimeout: 100 * time.Millisecond}
		in := openInput(t, cfg)
		out := openOutput(t, cfg)
		koloaudio.PauseCallbacks(in, true)
		koloaudio.PauseCallbacks(out, true)
		start := time.Now()
		buf := make([]int16, 480)
		for {
			_, err := in.Read(context.Background(), buf)
			if errors.Is(err, koloaudio.ErrDeviceLost) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		// The watchdog ticks every quarter of the timeout and fails the stream at
		// the first tick more than the timeout after the last callback.
		if d := time.Since(start); d != 125*time.Millisecond {
			t.Fatalf("stall detected after %v", d)
		}
		if _, err := out.Write(context.Background(), make([]int16, 48000)); !errors.Is(err, koloaudio.ErrDeviceLost) {
			t.Fatalf("Write = %v, want ErrDeviceLost", err)
		}
	})
}

func TestConcurrentUse(t *testing.T) {
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{Period: time.Millisecond, Buffer: 5 * time.Millisecond})
	out := openOutput(t, koloaudio.Config{Period: time.Millisecond, Buffer: 5 * time.Millisecond})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			buf := make([]int16, 96)
			for {
				if _, err := in.Read(context.Background(), buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]int16, 96)
			for {
				if _, err := out.Write(context.Background(), buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				in.Stats()
				out.Stats()
				runtime.Gosched()
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	in.Close()
	out.Close()
	wg.Wait()
}

// TestLongSession runs a 10 s capture to playback loop on the fake clock. A
// reader that keeps up sees every frame, with no overrun and no underrun.
func TestLongSession(t *testing.T) {
	inClock(t, func(t *testing.T) {
		in := openInput(t, koloaudio.Config{})
		out := openOutput(t, koloaudio.Config{})
		if _, err := out.Write(context.Background(), make([]int16, 1920)); err != nil {
			t.Fatal(err)
		}
		// The 1000th period ends at 10 s; the deadline follows it.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second+time.Millisecond)
		defer cancel()
		buf := make([]int16, 960)
		captured := 0
		for ctx.Err() == nil {
			n, err := in.Read(ctx, buf)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				t.Fatal(err)
			}
			captured += n
			if _, err := out.Write(ctx, buf[:n]); err != nil && ctx.Err() == nil {
				t.Fatal(err)
			}
		}
		ist, ost := in.Stats(), out.Stats()
		if captured != 10*48000 || ist.Frames != 10*48000 || ist.Overruns != 0 || ist.Underruns != 0 {
			t.Fatalf("captured %d samples in 10 s, input %+v", captured, ist)
		}
		if ost.Frames != 10*48000 || ost.Overruns != 0 || ost.Underruns != 0 {
			t.Fatalf("output %+v", ost)
		}
	})
}

// TestRealClockSession runs the same loop for 2 s on the real clock. A loaded
// machine can stall past the lag limit, so amounts are not bounded; it checks
// what holds under any load: the clock never runs ahead, audio arrives, and
// every captured frame is read, still buffered or counted as an overrun.
func TestRealClockSession(t *testing.T) {
	checkLeaks(t)
	start := time.Now()
	in := openInput(t, koloaudio.Config{})
	out := openOutput(t, koloaudio.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	buf := make([]int16, 960)
	read := 0
	for ctx.Err() == nil {
		n, err := in.Read(ctx, buf)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			t.Fatal(err)
		}
		read += n
		if _, err := out.Write(ctx, buf[:n]); err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	ist, ost := in.Stats(), out.Stats()
	t.Logf("input %+v, output %+v", ist, ost)
	if ist.Frames != uint64(read)+ist.Overruns+uint64(ist.Buffered) {
		t.Errorf("input %+v: %d frames read", ist, read)
	}
	for _, st := range []koloaudio.Stats{ist, ost} {
		if audio := time.Duration(st.Frames) * time.Second / 48000; audio > elapsed || st.Frames == 0 {
			t.Errorf("%v of audio in %v: %+v", audio, elapsed, st)
		}
	}
}
