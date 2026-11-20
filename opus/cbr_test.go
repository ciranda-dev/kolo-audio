package opus_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ciranda-dev/kolo-audio/opus"
)

const cbrRate = 48000

// cbrSignals returns 1 s each of a 1 kHz tone, digital silence, noise and
// speech-like bursts (a decaying 150 Hz buzz with 2 kHz formant, gated at 4 Hz).
func cbrSignals(channels int) map[string][]int16 {
	frames := cbrRate
	noise := make([]int16, frames*channels)
	seed := uint32(7)
	for i := range noise {
		seed = seed*1664525 + 1013904223
		noise[i] = int16(seed>>16) / 4
	}
	speech := make([]int16, frames*channels)
	for i := range frames {
		x := float64(i) / cbrRate
		v := 0.0
		if math.Mod(x*4, 1) < 0.5 {
			v = 6000*math.Sin(2*math.Pi*150*x) + 3000*math.Sin(2*math.Pi*2000*x) + 1500*math.Sin(2*math.Pi*3300*x)
		}
		for c := range channels {
			speech[i*channels+c] = int16(v)
		}
	}
	return map[string][]int16{
		"tone":    tone(1000, cbrRate, channels, frames, 10000),
		"silence": make([]int16, frames*channels),
		"noise":   noise,
		"speech":  speech,
	}
}

func TestCBREqualPacketSizes(t *testing.T) {
	for _, channels := range []int{1, 2} {
		for _, bitrate := range []int{16000, 32000, 64000} {
			e := newEncoder(t, opus.Config{Channels: channels, Bitrate: bitrate, CBR: true})
			var all []int16
			signals := cbrSignals(channels)
			for _, name := range []string{"tone", "silence", "noise", "speech", "silence", "tone"} {
				all = append(all, signals[name]...)
			}
			packets := encodeAll(t, e, all)
			if len(packets) != 6*50 {
				t.Fatalf("%d packets, want 300", len(packets))
			}
			for i, p := range packets {
				if len(p) != len(packets[0]) {
					t.Fatalf("%d ch %d bit/s: packet %d is %d bytes, packet 0 is %d", channels, bitrate, i, len(p), len(packets[0]))
				}
			}
			// 20 ms at bitrate bit/s, rounded as libopus does.
			if want := (bitrate/50 + 4) / 8; len(packets[0]) != want {
				t.Errorf("%d ch %d bit/s: %d bytes per packet, want %d", channels, bitrate, len(packets[0]), want)
			}
		}
	}
}

func TestCBREqualSizesPerSignal(t *testing.T) {
	for name, pcm := range cbrSignals(1) {
		e := newEncoder(t, opus.Config{Bitrate: 32000, CBR: true})
		sizes := map[int]bool{}
		for _, p := range encodeAll(t, e, pcm) {
			sizes[len(p)] = true
		}
		if len(sizes) != 1 {
			t.Errorf("%s: %d different packet sizes, want 1", name, len(sizes))
		}
	}
}

func TestCBRRoundTrip(t *testing.T) {
	for _, channels := range []int{1, 2} {
		for _, bitrate := range []int{16000, 32000, 64000} {
			e := newEncoder(t, opus.Config{Channels: channels, Bitrate: bitrate, CBR: true})
			d := newDecoder(t, cbrRate, channels)
			src := tone(1000, cbrRate, channels, cbrRate, 10000)
			var out []int16
			pcm := make([]int16, 5760*channels)
			for _, p := range encodeAll(t, e, src) {
				n, err := d.Decode(p, pcm)
				if err != nil || n != 960*channels {
					t.Fatalf("Decode = %d, %v", n, err)
				}
				out = append(out, pcm[:n]...)
			}
			mono := func(x []int16) []int16 {
				m := make([]int16, len(x)/channels)
				for i := range m {
					m[i] = x[i*channels]
				}
				return m
			}
			v, _ := snr(mono(src)[4800:47000], mono(out)[4800:], 1000)
			t.Logf("%d ch %d bit/s: SNR %.1f dB", channels, bitrate, v)
			if v < 6 {
				t.Errorf("%d ch %d bit/s: SNR %.1f dB, want at least 6", channels, bitrate, v)
			}
		}
	}
}

func TestCBRDTX(t *testing.T) {
	e := newEncoder(t, opus.Config{Bitrate: 32000, CBR: true, DTX: true})
	full := len(encodeAll(t, e, tone(1000, cbrRate, 1, 960*3, 10000))[2])
	small := 0
	for _, p := range encodeAll(t, e, make([]int16, cbrRate*2)) {
		if len(p) <= 2 {
			small++
		}
	}
	if full != (32000/50+4)/8 || small == 0 {
		t.Fatalf("CBR with DTX: voice packet %d bytes, %d short packets in silence; want %d bytes and some short packets", full, small, (32000/50+4)/8)
	}
}

func TestSetCBR(t *testing.T) {
	e := newEncoder(t, opus.Config{Bitrate: 32000})
	speech := cbrSignals(1)["speech"]
	sizes := func(pcm []int16) map[int]bool {
		m := map[int]bool{}
		for _, p := range encodeAll(t, e, pcm) {
			m[len(p)] = true
		}
		return m
	}
	if n := len(sizes(speech)); n < 2 {
		t.Fatalf("VBR gave %d distinct sizes, want several", n)
	}
	if err := e.SetCBR(true); err != nil {
		t.Fatal(err)
	}
	sizes(speech[:960*5])
	if n := len(sizes(speech)); n != 1 {
		t.Errorf("after SetCBR(true): %d distinct sizes, want 1", n)
	}
	if err := e.SetCBR(false); err != nil {
		t.Fatal(err)
	}
	sizes(speech[:960*5])
	if n := len(sizes(speech)); n < 2 {
		t.Errorf("after SetCBR(false): %d distinct sizes, want several", n)
	}
}

func TestCBRDefaultIsVBR(t *testing.T) {
	e := newEncoder(t, opus.Config{Bitrate: 32000})
	sizes := map[int]bool{}
	for _, p := range encodeAll(t, e, cbrSignals(1)["speech"]) {
		sizes[len(p)] = true
	}
	if len(sizes) < 2 {
		t.Errorf("default encoder gave %d distinct sizes, want variable bitrate", len(sizes))
	}
}

func TestCBRSurvivesFault(t *testing.T) {
	e := newEncoder(t, opus.Config{Bitrate: 32000})
	if err := e.SetCBR(true); err != nil {
		t.Fatal(err)
	}
	opus.BreakEncoder(e)
	buf := make([]byte, opus.PacketBufferSize)
	if _, err := e.Encode(tone(1000, cbrRate, 1, 960, 10000), buf); !errors.Is(err, opus.ErrInternal) {
		t.Fatalf("Encode on broken state = %v, want ErrInternal", err)
	}
	signals := cbrSignals(1)
	sizes := map[int]bool{}
	for _, p := range encodeAll(t, e, append(signals["speech"], signals["silence"]...)) {
		sizes[len(p)] = true
	}
	if len(sizes) != 1 {
		t.Errorf("after a fault: %d distinct sizes, want 1", len(sizes))
	}
}

func TestCBRFrameDurations(t *testing.T) {
	durations := []time.Duration{2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond}
	for _, dur := range durations {
		for _, bitrate := range []int{500, 6000, 512000} {
			e := newEncoder(t, opus.Config{Bitrate: bitrate, CBR: true, FrameDuration: dur})
			signals := cbrSignals(1)
			sizes := map[int]bool{}
			for _, p := range encodeAll(t, e, append(signals["speech"], signals["silence"]...)) {
				sizes[len(p)] = true
			}
			want := max(1, (int(float64(bitrate)*dur.Seconds())+4)/8)
			if len(sizes) != 1 || !sizes[want] {
				t.Errorf("%v at %d bit/s: sizes %v, want only %d", dur, bitrate, sizes, want)
			}
		}
	}
}
