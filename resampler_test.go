package koloaudio_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	koloaudio "github.com/ciranda-dev/kolo-audio"
)

// power returns the signal power of pcm at freq using the Goertzel algorithm.
func power(pcm []int16, freq float64, rate int) float64 {
	w := 2 * math.Pi * freq / float64(rate)
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range pcm {
		s := float64(v) + c*s1 - s2
		s2, s1 = s1, s
	}
	return (s1*s1 + s2*s2 - c*s1*s2) / (float64(len(pcm)) * float64(len(pcm)))
}

func resampleAll(t *testing.T, r *koloaudio.Resampler, src []int16, chunk int) []int16 {
	t.Helper()
	var out []int16
	buf := make([]int16, 256)
	for len(src) > 0 {
		in := src[:min(chunk, len(src))]
		for len(in) > 0 {
			w, c, err := r.Process(buf, in)
			if err != nil {
				t.Fatal(err)
			}
			if w == 0 && c == 0 {
				t.Fatal("Process made no progress")
			}
			out = append(out, buf[:w]...)
			in = in[c:]
		}
		src = src[min(chunk, len(src)):]
	}
	tail := make([]int16, r.OutputLen(r.Latency()*4+16))
	n, err := r.Flush(tail)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, tail[:n]...)
}

func TestResamplerTone(t *testing.T) {
	cases := []struct{ in, out, channels int }{
		{44100, 48000, 1},
		{48000, 16000, 1},
		{16000, 48000, 2},
		{48000, 48000, 1},
		{8000, 384000, 1},
	}
	for _, c := range cases {
		r, err := koloaudio.NewResampler(c.channels, c.in, c.out)
		if err != nil {
			t.Fatal(err)
		}
		const freq = 1000
		src := tone(freq, c.in, c.channels, c.in, 10000)
		out := resampleAll(t, r, src, 333*c.channels)
		r.Close()

		frames := len(out) / c.channels
		if math.Abs(float64(frames-c.out)) > float64(c.out)/1000+8 {
			t.Errorf("%d to %d: %d frames, want about %d", c.in, c.out, frames, c.out)
		}
		mono := make([]int16, frames)
		for i := range mono {
			mono[i] = out[i*c.channels]
		}
		on := power(mono, freq, c.out)
		off := power(mono, freq*1.25, c.out)
		if on < 100*off {
			t.Errorf("%d to %d: 1 kHz power %.1f, 1.25 kHz %.1f", c.in, c.out, on, off)
		}
		p, _ := koloaudio.Level(mono[c.out/10:])
		t.Logf("%d to %d Hz: %d frames, tone/off-tone power %.0f, peak %.2f dBFS", c.in, c.out, frames, on/off, p)
		if p < -11 || p > -9 {
			t.Errorf("%d to %d: peak %.2f dBFS, want about -10.3", c.in, c.out, p)
		}
	}
}

func TestResamplerChunking(t *testing.T) {
	src := tone(440, 44100, 2, 4410, 12000)
	var results [][]int16
	for _, chunk := range []int{2, 34, 882, len(src)} {
		r, err := koloaudio.NewResampler(2, 44100, 48000)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, resampleAll(t, r, src, chunk))
		r.Close()
	}
	for i := 1; i < len(results); i++ {
		if !slices.Equal(results[0], results[i]) {
			t.Fatalf("chunked output %d differs from the first", i)
		}
	}
}

func TestResamplerErrors(t *testing.T) {
	for _, c := range [][3]int{{0, 48000, 48000}, {1, 7999, 48000}, {1, 48000, 384001}, {255, 48000, 44100}} {
		if _, err := koloaudio.NewResampler(c[0], c[1], c[2]); !errors.Is(err, koloaudio.ErrUnsupportedFormat) {
			t.Errorf("NewResampler%v = %v", c, err)
		}
	}
	r, err := koloaudio.NewResampler(2, 48000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Process(make([]int16, 1), make([]int16, 2)); !errors.Is(err, koloaudio.ErrInvalidArgument) {
		t.Errorf("dst smaller than a frame: %v", err)
	}
	if _, _, err := r.Process(make([]int16, 2), make([]int16, 3)); !errors.Is(err, koloaudio.ErrInvalidArgument) {
		t.Errorf("partial src frame: %v", err)
	}
	if w, c, err := r.Process(make([]int16, 2), nil); err != nil || w != 0 || c != 0 {
		t.Errorf("empty src: %d %d %v", w, c, err)
	}
	r.Process(make([]int16, 100), make([]int16, 300))
	if _, err := r.Flush(nil); !errors.Is(err, koloaudio.ErrInvalidArgument) {
		t.Errorf("Flush into nil: %v", err)
	}
	if r.OutputLen(-2) != 0 || r.OutputLen(0) != 0 {
		t.Error("OutputLen of a non-positive count is not 0")
	}
	r.Reset()
	for range 2 {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.Process(make([]int16, 2), make([]int16, 2)); !errors.Is(err, koloaudio.ErrClosed) {
		t.Errorf("Process after Close: %v", err)
	}
	if _, err := r.Flush(make([]int16, 100)); !errors.Is(err, koloaudio.ErrClosed) {
		t.Errorf("Flush after Close: %v", err)
	}
	r.Reset()
	if r.Latency() != 0 || r.OutputLen(100) != 0 {
		t.Error("closed Resampler reports state")
	}
	var zero koloaudio.Resampler
	if _, _, err := zero.Process(make([]int16, 2), nil); !errors.Is(err, koloaudio.ErrClosed) {
		t.Errorf("zero Resampler: %v", err)
	}
}

func TestResamplerSizes(t *testing.T) {
	rates := []int{8000, 11025, 16000, 22050, 44100, 48000, 96000, 384000}
	for _, in := range rates {
		for _, out := range rates {
			r, err := koloaudio.NewResampler(2, in, out)
			if err != nil {
				t.Fatal(err)
			}
			src := tone(440, in, 2, 1000, 5000)
			want := r.OutputLen(len(src))
			dst := make([]int16, want+64)
			w, c, err := r.Process(dst, src)
			if err != nil || w != want || c != len(src) {
				t.Errorf("%d to %d: Process = %d, %d, %v; OutputLen %d", in, out, w, c, err, want)
			}
			if _, err := r.Flush(make([]int16, (r.Latency()+1)*2)); err != nil {
				t.Errorf("%d to %d: Flush into Latency()+1 frames: %v", in, out, err)
			}
			r.Close()
		}
	}
}

func TestResamplerConcurrentClose(t *testing.T) {
	r, err := koloaudio.NewResampler(1, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src, dst := make([]int16, 441), make([]int16, 480)
			for {
				if _, _, err := r.Process(dst, src); err != nil {
					if !errors.Is(err, koloaudio.ErrClosed) {
						t.Errorf("Process = %v", err)
					}
					return
				}
				r.OutputLen(441)
				r.Latency()
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	r.Close()
	wg.Wait()
}

type goldenCase struct {
	Name           string `json:"name"`
	Channels       int    `json:"channels"`
	InRate         int    `json:"in_rate"`
	OutRate        int    `json:"out_rate"`
	Seed           int    `json:"seed"`
	Latency        int    `json:"latency"`
	OutputLen      int    `json:"output_len"`
	Samples        int    `json:"samples"`
	SHA256         string `json:"sha256"`
	ChunkedSamples int    `json:"chunked_samples"`
	ChunkedSHA256  string `json:"chunked_sha256"`
	ReusedSamples  int    `json:"reused_samples"`
	ReusedSHA256   string `json:"reused_sha256"`
}

func goldenDigest(pcm []int16) string {
	b := koloaudio.EncodeS16LE(nil, pcm)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func goldenFlush(t *testing.T, r *koloaudio.Resampler, channels int) []int16 {
	t.Helper()
	tail := make([]int16, (r.Latency()+1)*channels)
	n, err := r.Flush(tail)
	if err != nil {
		t.Fatal(err)
	}
	return tail[:n]
}

// goldenWhole feeds src at once into a 4096 frame output buffer.
func goldenWhole(t *testing.T, r *koloaudio.Resampler, src []int16, channels int) []int16 {
	t.Helper()
	var got []int16
	dst := make([]int16, 4096*channels)
	for rest := src; len(rest) > 0; {
		w, n, err := r.Process(dst, rest)
		if err != nil || w+n == 0 {
			t.Fatalf("Process = %d, %d, %v", w, n, err)
		}
		got = append(got, dst[:w]...)
		rest = rest[n:]
	}
	return append(got, goldenFlush(t, r, channels)...)
}

// goldenChunked feeds src in random chunks into random output buffers.
func goldenChunked(t *testing.T, r *koloaudio.Resampler, src []int16, channels, seed int) []int16 {
	t.Helper()
	rng := rand.New(rand.NewPCG(2, uint64(seed)))
	var got []int16
	for rest := src; len(rest) > 0; {
		in := rest[:min(len(rest), (1+rng.IntN(1000))*channels)]
		rest = rest[len(in):]
		for len(in) > 0 {
			dst := make([]int16, (1+rng.IntN(2000))*channels)
			w, n, err := r.Process(dst, in)
			if err != nil || w+n == 0 {
				t.Fatalf("Process = %d, %d, %v", w, n, err)
			}
			got = append(got, dst[:w]...)
			in = in[n:]
		}
	}
	return append(got, goldenFlush(t, r, channels)...)
}

// TestResamplerGolden compares the output with files made by the native
// miniaudio resampler (testdata/resampler/gen.sh): fed whole, in chunks, and
// in chunks again on the Resampler that was just flushed.
func TestResamplerGolden(t *testing.T) {
	dir := filepath.Join("testdata", "resampler")
	b, err := os.ReadFile(filepath.Join(dir, "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 7 {
		t.Fatalf("%d golden cases, want 7", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(1, uint64(c.Seed)))
			src := make([]int16, c.InRate*c.Channels)
			for i := range src {
				src[i] = int16(rng.IntN(65536) - 32768)
			}
			want, err := os.ReadFile(filepath.Join(dir, c.Name+".s16le"))
			if err != nil {
				t.Fatal(err)
			}
			if len(want) != 2*c.Samples {
				t.Fatalf("golden file holds %d bytes, manifest says %d samples", len(want), c.Samples)
			}

			r, err := koloaudio.NewResampler(c.Channels, c.InRate, c.OutRate)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.Latency() != c.Latency || r.OutputLen(len(src)) != c.OutputLen*c.Channels {
				t.Errorf("Latency %d, OutputLen %d; want %d, %d", r.Latency(), r.OutputLen(len(src)), c.Latency, c.OutputLen*c.Channels)
			}
			got := goldenWhole(t, r, src, c.Channels)
			if !slices.Equal(koloaudio.EncodeS16LE(nil, got), want) {
				t.Errorf("whole: output differs from %s.s16le (%d samples, want %d)", c.Name, len(got), c.Samples)
			}
			o, err := koloaudio.NewResampler(c.Channels, c.InRate, c.OutRate)
			if err != nil {
				t.Fatal(err)
			}
			defer o.Close()
			once := make([]int16, o.OutputLen(len(src))+c.Channels)
			w, n, err := o.Process(once, src)
			if err != nil || n != len(src) {
				t.Fatalf("one call: Process = %d, %d, %v", w, n, err)
			}
			got = append(once[:w], goldenFlush(t, o, c.Channels)...)
			if !slices.Equal(koloaudio.EncodeS16LE(nil, got), want) {
				t.Errorf("one call: output differs from %s.s16le (%d samples, want %d)", c.Name, len(got), c.Samples)
			}
			got = goldenChunked(t, r, src, c.Channels, c.Seed)
			if len(got) != c.ReusedSamples || goldenDigest(got) != c.ReusedSHA256 {
				t.Errorf("reused: %d samples, sha256 %s; want %d, %s", len(got), goldenDigest(got), c.ReusedSamples, c.ReusedSHA256)
			}

			f, err := koloaudio.NewResampler(c.Channels, c.InRate, c.OutRate)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got = goldenChunked(t, f, src, c.Channels, c.Seed)
			if len(got) != c.ChunkedSamples || goldenDigest(got) != c.ChunkedSHA256 {
				t.Errorf("chunked: %d samples, sha256 %s; want %d, %s", len(got), goldenDigest(got), c.ChunkedSamples, c.ChunkedSHA256)
			}
		})
	}
}

func FuzzResampler(f *testing.F) {
	f.Add(uint8(1), uint16(44100), uint16(48000), []byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add(uint8(2), uint16(48000), uint16(8000), []byte{0xff, 0x7f, 0, 0x80})
	f.Fuzz(func(t *testing.T, channels uint8, in, out uint16, data []byte) {
		ch := int(channels%4) + 1
		r, err := koloaudio.NewResampler(ch, int(in)+8000, int(out)+8000)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		src, _ := koloaudio.DecodeS16LE(nil, data[:len(data)/2*2])
		src = src[:len(src)/ch*ch]
		size := ch * (1 + len(data)%37)
		guard := make([]int16, size+64)
		for i := range guard {
			guard[i] = 0x5a5a
		}
		dst := guard[:size]
		for len(src) > 0 {
			w, c, err := r.Process(dst, src)
			if slices.ContainsFunc(guard[size:], func(v int16) bool { return v != 0x5a5a }) {
				t.Fatal("Process wrote past dst")
			}
			if err != nil || w > len(dst) || c > len(src) || w%ch != 0 || c%ch != 0 || w+c == 0 {
				t.Fatalf("Process = %d, %d, %v", w, c, err)
			}
			src = src[c:]
		}
	})
}
