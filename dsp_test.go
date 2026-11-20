package koloaudio_test

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"strconv"
	"testing"

	koloaudio "github.com/ciranda-dev/kolo-audio"
)

func tone(freq float64, rate, channels, frames int, amp float64) []int16 {
	pcm := make([]int16, frames*channels)
	for i := range frames {
		v := int16(math.Round(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))))
		for c := range channels {
			pcm[i*channels+c] = v
		}
	}
	return pcm
}

func TestGain(t *testing.T) {
	pcm := []int16{100, -100, 20000, -20000, 32767, -32768, 0}
	if err := koloaudio.Gain(pcm, 2); err != nil {
		t.Fatal(err)
	}
	want := []int16{200, -200, 32767, -32768, 32767, -32768, 0}
	if !slices.Equal(pcm, want) {
		t.Fatalf("got %v, want %v", pcm, want)
	}
	pcm = []int16{3, -3, -32768}
	if err := koloaudio.Gain(pcm, -0.5); err != nil {
		t.Fatal(err)
	}
	if want := []int16{-2, 2, 16384}; !slices.Equal(pcm, want) {
		t.Fatalf("got %v, want %v", pcm, want)
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		pcm := []int16{1}
		if err := koloaudio.Gain(pcm, bad); !errors.Is(err, koloaudio.ErrInvalidArgument) {
			t.Fatalf("Gain(%v) = %v, want ErrInvalidArgument", bad, err)
		}
		if pcm[0] != 1 {
			t.Fatalf("Gain(%v) changed the samples", bad)
		}
	}
}

func TestMix(t *testing.T) {
	dst := []int16{1, 30000, -30000, 5, 7}
	n := koloaudio.Mix(dst, []int16{2, 10000, -10000, -5})
	if n != 4 {
		t.Fatalf("n = %d, want 4", n)
	}
	if want := []int16{3, 32767, -32768, 0, 7}; !slices.Equal(dst, want) {
		t.Fatalf("got %v, want %v", dst, want)
	}
}

func TestLevel(t *testing.T) {
	peak, rms := koloaudio.Level(nil)
	if !math.IsInf(peak, -1) || !math.IsInf(rms, -1) {
		t.Fatalf("empty: %v %v", peak, rms)
	}
	peak, rms = koloaudio.Level(make([]int16, 10))
	if !math.IsInf(peak, -1) || !math.IsInf(rms, -1) {
		t.Fatalf("silence: %v %v", peak, rms)
	}
	peak, rms = koloaudio.Level([]int16{-32768, -32768})
	if peak != 0 || rms != 0 {
		t.Fatalf("full-scale square: %v %v", peak, rms)
	}
	peak, rms = koloaudio.Level(tone(1000, 48000, 1, 48000, 16384))
	if math.Abs(peak-(-6.02)) > 0.01 || math.Abs(rms-(-9.03)) > 0.01 {
		t.Fatalf("half-scale sine: peak %.3f rms %.3f, want -6.02 and -9.03", peak, rms)
	}
}

func TestConvertChannels(t *testing.T) {
	dst := make([]int16, 6)
	n, err := koloaudio.ConvertChannels(dst, []int16{1, -2, 3}, 1, 2)
	if err != nil || n != 6 || !slices.Equal(dst, []int16{1, 1, -2, -2, 3, 3}) {
		t.Fatalf("mono to stereo: %v %d %v", err, n, dst)
	}
	n, err = koloaudio.ConvertChannels(dst, []int16{32767, 32767, -32768, -32767, 3, -4}, 2, 1)
	if err != nil || n != 3 || !slices.Equal(dst[:3], []int16{32767, -32767, 0}) {
		t.Fatalf("stereo to mono: %v %d %v", err, n, dst[:3])
	}
	n, err = koloaudio.ConvertChannels(dst, []int16{9, 8, 7}, 3, 3)
	if err != nil || n != 3 || !slices.Equal(dst[:3], []int16{9, 8, 7}) {
		t.Fatalf("copy: %v %d %v", err, n, dst[:3])
	}
	n, err = koloaudio.ConvertChannels(dst, []int16{1, 2, 3, 4, 5, 6}, 6, 1)
	if err != nil || n != 1 || dst[0] != 3 {
		t.Fatalf("5.1 to mono: %v %d %v", err, n, dst[0])
	}

	cases := []struct {
		name     string
		dst, src []int16
		from, to int
		want     error
	}{
		{"stereo to 6", make([]int16, 12), make([]int16, 4), 2, 6, koloaudio.ErrUnsupportedFormat},
		{"zero channels", dst, []int16{1}, 0, 1, koloaudio.ErrUnsupportedFormat},
		{"too many channels", dst, []int16{1}, 1, koloaudio.MaxChannels + 1, koloaudio.ErrUnsupportedFormat},
		{"partial frame", dst, []int16{1, 2, 3}, 2, 1, koloaudio.ErrInvalidArgument},
		{"short dst", make([]int16, 5), []int16{1, 2, 3}, 1, 2, koloaudio.ErrInvalidArgument},
	}
	for _, c := range cases {
		if _, err := koloaudio.ConvertChannels(c.dst, c.src, c.from, c.to); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestConvertChannelsOverflow(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("needs a 32-bit int to overflow with a small buffer")
	}
	src := make([]int16, math.MaxInt/koloaudio.MaxChannels+1)
	if _, err := koloaudio.ConvertChannels(nil, src, 1, koloaudio.MaxChannels); !errors.Is(err, koloaudio.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestS16LE(t *testing.T) {
	pcm := []int16{0, 1, -1, 32767, -32768, 0x1234}
	b := koloaudio.EncodeS16LE([]byte{0xAA}, pcm)
	want := []byte{0xAA, 0, 0, 1, 0, 0xFF, 0xFF, 0xFF, 0x7F, 0x00, 0x80, 0x34, 0x12}
	if !bytes.Equal(b, want) {
		t.Fatalf("encode: %x, want %x", b, want)
	}
	got, err := koloaudio.DecodeS16LE([]int16{7}, b[1:])
	if err != nil || !slices.Equal(got, append([]int16{7}, pcm...)) {
		t.Fatalf("decode: %v %v", got, err)
	}
	prev := []int16{5}
	got, err = koloaudio.DecodeS16LE(prev, []byte{1, 2, 3})
	if !errors.Is(err, koloaudio.ErrInvalidArgument) || !slices.Equal(got, prev) {
		t.Fatalf("odd length: %v %v", got, err)
	}
}

func FuzzDecodeS16LE(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{0, 0x80, 0xff, 0x7f})
	f.Fuzz(func(t *testing.T, b []byte) {
		pcm, err := koloaudio.DecodeS16LE(nil, b)
		if len(b)%2 != 0 {
			if err == nil {
				t.Fatal("odd length accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(koloaudio.EncodeS16LE(nil, pcm), b) {
			t.Fatal("round trip mismatch")
		}
	})
}
