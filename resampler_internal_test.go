package koloaudio

import (
	"errors"
	"slices"
	"testing"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
)

func TestResamplerTrap(t *testing.T) {
	src := make([]int16, 2*2000)
	for i := range src {
		src[i] = int16(i * 37)
	}
	run := func(r *Resampler) []int16 {
		dst := make([]int16, 2*4000)
		w, c, err := r.Process(dst, src)
		if err != nil || c != len(src) {
			t.Fatalf("Process = %d, %d, %v", w, c, err)
		}
		return dst[:w]
	}
	fresh, err := NewResampler(2, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	want := run(fresh)

	r, err := NewResampler(2, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		r.eng.Fault(wasihost.Exit(1))
		w, c, err := r.Process(make([]int16, 100), src)
		if !errors.Is(err, ErrBackend) || w != 0 || c != 0 {
			t.Fatalf("Process after a trap = %d, %d, %v; want ErrBackend", w, c, err)
		}
		if got := run(r); !slices.Equal(got, want) {
			t.Fatal("output after a trap differs from a new Resampler")
		}
	}

	r.eng.Fault("boom")
	if _, err := r.Flush(make([]int16, 100)); !errors.Is(err, ErrBackend) {
		t.Errorf("Flush after a trap = %v; want ErrBackend", err)
	}
	r.eng.Fault("boom")
	if r.Latency() != 0 {
		t.Error("Latency of a trapped Resampler is not 0")
	}
	if r.Latency() == 0 || r.OutputLen(4000) == 0 {
		t.Error("Resampler did not recover")
	}
	r.eng.Fault("boom")
	r.Reset()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
