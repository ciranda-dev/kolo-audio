package koloaudio

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestRingMatchesModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{1, 2, 7, 64} {
		b := ring{data: make([]int16, size)}
		var model []int16
		next := int16(0)
		for range 5000 {
			n := rng.IntN(2 * size)
			p := make([]int16, n)
			for i := range p {
				p[i] = next
				next++
			}
			switch rng.IntN(3) {
			case 0:
				w := b.write(p)
				if want := min(n, size-len(model)); w != want {
					t.Fatalf("size %d: write %d, want %d", size, w, want)
				}
				model = append(model, p[:w]...)
			case 1:
				d := b.overwrite(p)
				model = append(model, p...)
				if drop := max(0, len(model)-size); d != drop {
					t.Fatalf("size %d: dropped %d, want %d", size, d, drop)
				} else {
					model = model[drop:]
				}
			case 2:
				r := b.read(p)
				if want := min(n, len(model)); r != want || !slices.Equal(p[:r], model[:r]) {
					t.Fatalf("size %d: read %v, want %v", size, p[:r], model[:want])
				}
				model = model[r:]
			}
			if b.n != len(model) {
				t.Fatalf("size %d: n = %d, want %d", size, b.n, len(model))
			}
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	c, err := Config{}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Format: Format{48000, 1}, Period: DefaultPeriod, Buffer: DefaultBuffer, StallTimeout: DefaultStallTimeout}
	if c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
	if n := c.Format.frames(c.Buffer); n != 5760 {
		t.Fatalf("buffer frames = %d, want 5760", n)
	}
	if n := (Format{44100, 1}).frames(c.Period); n != 441 {
		t.Fatalf("period frames at 44.1 kHz = %d, want 441", n)
	}
}
