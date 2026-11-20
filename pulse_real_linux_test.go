//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"testing"
	"time"
)

// The tests below run against a real PulseAudio or PipeWire server with a
// null sink, in the Docker job (build/validate/linux/run.sh). They need
// KOLO_AUDIO_PULSE_TEST=1 and an explicit PULSE_SERVER, and they open only
// the sink named by KOLO_AUDIO_PULSE_SINK (default kolo_null) and its
// monitor source, never the default device.

const (
	pulseOpSetDefaultSink = 44
	pulseOpLoadModule     = 51
	pulseOpUnloadModule   = 52
)

func realPulse(t *testing.T) (sink string) {
	t.Helper()
	if os.Getenv("KOLO_AUDIO_PULSE_TEST") != "1" {
		t.Skip("set KOLO_AUDIO_PULSE_TEST=1 and PULSE_SERVER to test against a PulseAudio server")
	}
	if os.Getenv("PULSE_SERVER") == "" {
		t.Fatal("PULSE_SERVER must name the test server")
	}
	nullBackend.Store(false)
	t.Cleanup(func() { nullBackend.Store(true) })
	if sink = os.Getenv("KOLO_AUDIO_PULSE_SINK"); sink == "" {
		sink = "kolo_null"
	}
	return sink
}

func hexID(name string) string { return hex.EncodeToString([]byte(name)) }

func TestPulseEnumerate(t *testing.T) {
	sink := realPulse(t)
	for _, kind := range []Kind{Capture, Playback} {
		devs, err := Devices(kind)
		if err != nil {
			t.Fatal(err)
		}
		defaults, found := 0, false
		want := sink
		if kind == Capture {
			want = sink + ".monitor"
		}
		for _, d := range devs {
			t.Logf("%v: %q default=%v id=%s", kind, d.Name, d.Default, d.ID)
			if d.Default {
				defaults++
			}
			found = found || d.ID == hexID(want)
		}
		if !found {
			t.Errorf("%v: no device %q", kind, want)
		}
		if defaults > 1 {
			t.Errorf("%v: %d default devices", kind, defaults)
		}
	}
}

// goertzel returns the power of x at freq as a fraction of its total power.
func goertzel(x []int16, freq, rate float64) float64 {
	w := 2 * math.Pi * freq / rate
	coeff := 2 * math.Cos(w)
	var s1, s2, total float64
	for _, v := range x {
		s0 := float64(v) + coeff*s1 - s2
		s2, s1 = s1, s0
		total += float64(v) * float64(v)
	}
	if total == 0 {
		return 0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	return 2 * power / (float64(len(x)) * total)
}

func TestPulsePlayRecord(t *testing.T) {
	sink := realPulse(t)
	const rate, freq = 48000, 440.0
	base := Config{Format: Format{SampleRate: rate, Channels: 1}, Period: 20 * time.Millisecond, Buffer: 200 * time.Millisecond}
	inCfg, outCfg := base, base
	inCfg.DeviceID, outCfg.DeviceID = hexID(sink+".monitor"), hexID(sink)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	in, err := OpenInput(inCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := OpenOutput(outCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	recorded := make(chan []int16, 1)
	go func() {
		var all []int16
		buf := make([]int16, 960)
		for len(all) < 3*rate {
			n, err := in.Read(ctx, buf)
			if err != nil {
				break
			}
			all = append(all, buf[:n]...)
		}
		recorded <- all
	}()

	tone := make([]int16, rate)
	for i := range tone {
		tone[i] = int16(16384 * math.Sin(2*math.Pi*freq*float64(i)/rate))
	}
	for off := 0; off < len(tone); off += 480 {
		if _, err := out.Write(ctx, tone[off:off+480]); err != nil {
			t.Fatal(err)
		}
	}
	st := out.Stats()
	t.Logf("output stats after the last write: %+v", st)
	if st.Frames == 0 || st.Underruns != 0 {
		t.Errorf("output stats = %+v, want Frames > 0 and Underruns 0", st)
	}

	all := <-recorded
	t.Logf("recorded %d samples, input stats %+v", len(all), in.Stats())
	const window = rate / 10
	tonal := 0
	for off := 0; off+window <= len(all); off += window {
		if goertzel(all[off:off+window], freq, rate) > 0.8 {
			tonal++
		}
	}
	if tonal < 8 {
		t.Errorf("%d of %d windows of 100 ms carry the %v Hz tone, want at least 8", tonal, len(all)/window, freq)
	}
}

// TestPulseFollowDefault checks that a stream opened on the default device
// moves with the default when the server changes it. It first makes the null
// sink the default, so the default device is never real hardware.
func TestPulseFollowDefault(t *testing.T) {
	sink := realPulse(t)
	const rate, freq, second = 48000, 440.0, "kolo_null2"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := dialPulse(osPulseEnv(), pulseRequestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	setDefault := func(name string) {
		t.Helper()
		if err := c.call(pulseOpSetDefaultSink, func(w *tagWriter) { w.str(name) }, nil); err != nil {
			t.Fatal(err)
		}
	}
	setDefault(sink)
	var module uint32
	err = c.call(pulseOpLoadModule, func(w *tagWriter) {
		w.str("module-null-sink")
		w.str("sink_name=" + second + " rate=48000 channels=1")
	}, func(r *tagReader) error { module = r.u32(); return r.err })
	if err != nil {
		t.Fatal(err)
	}
	defer c.call(pulseOpUnloadModule, func(w *tagWriter) { w.u32(module) }, nil)

	base := Config{Format: Format{SampleRate: rate, Channels: 1}, Period: 20 * time.Millisecond, Buffer: 200 * time.Millisecond}
	inCfg := base
	inCfg.DeviceID = hexID(second + ".monitor")
	in, err := OpenInput(inCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := OpenOutput(base)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	recorded := make(chan []int16, 1)
	go func() {
		var all []int16
		buf := make([]int16, 960)
		for len(all) < 4*rate {
			n, err := in.Read(ctx, buf)
			if err != nil {
				break
			}
			all = append(all, buf[:n]...)
		}
		recorded <- all
	}()
	switched := time.Now().Add(500 * time.Millisecond)
	chunk := make([]int16, 480)
	for i := 0; i < 3*rate; i += len(chunk) {
		for j := range chunk {
			chunk[j] = int16(16384 * math.Sin(2*math.Pi*freq*float64(i+j)/rate))
		}
		if _, err := out.Write(ctx, chunk); err != nil {
			t.Fatalf("Write at %d = %v", i, err)
		}
		if !switched.IsZero() && time.Now().After(switched) {
			setDefault(second)
			switched = time.Time{}
		}
	}
	all := <-recorded
	const window = rate / 10
	tonal := 0
	for off := 0; off+window <= len(all); off += window {
		if goertzel(all[off:off+window], freq, rate) > 0.8 {
			tonal++
		}
	}
	t.Logf("%d of %d windows of the second sink's monitor carry the tone", tonal, len(all)/window)
	if tonal < 10 {
		t.Errorf("only %d windows carry the tone: the stream did not follow the default", tonal)
	}
}

// TestPulseSinkRemoved runs last: it removes the null sink.
func TestPulseSinkRemoved(t *testing.T) {
	sink := realPulse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := OpenOutput(Config{DeviceID: hexID(sink)})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	silence := make([]int16, 480)
	if _, err := out.Write(ctx, silence); err != nil {
		t.Fatal(err)
	}

	c, err := dialPulse(osPulseEnv(), pulseRequestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	sinks, err := c.devices(true)
	if err != nil {
		t.Fatal(err)
	}
	module := uint32(pulseNoIndex)
	for _, s := range sinks {
		if s.name == sink {
			module = s.module
		}
	}
	if module == pulseNoIndex {
		t.Fatalf("sink %q has no owner module", sink)
	}
	if err := c.call(pulseOpUnloadModule, func(w *tagWriter) { w.u32(module) }, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for {
		_, err := out.Write(ctx, silence)
		if errors.Is(err, ErrDeviceLost) {
			if d := time.Since(start); d > DefaultStallTimeout/2 {
				t.Errorf("the loss took %v, so it came from the stall watchdog and not the server", d)
			}
			return
		}
		if err != nil {
			t.Fatalf("Write after the sink was removed = %v, want ErrDeviceLost", err)
		}
	}
}
