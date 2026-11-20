package maresample

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
)

const pageSize = 65536

func noise(n int) []int16 {
	rng := rand.New(rand.NewPCG(7, 7))
	p := make([]int16, n)
	for i := range p {
		p[i] = int16(rng.IntN(65536) - 32768)
	}
	return p
}

// extremes are the configurations that use the most stack and heap: the
// largest channel count with the lowest, the highest and equal rates.
var extremes = [][3]int{{1, 8000, 8000}, {1, 8000, 384000}, {1, 384000, 8000}, {2, 44100, 48000}, {254, 8000, 384000}, {254, 384000, 8000}, {254, 48000, 44100}, {128, 22050, 384000}}

func TestStackHighWater(t *testing.T) {
	worst, stack := 0, 0
	for _, c := range extremes {
		m := New(wasihost.Host{})
		stack = int(m.g0)
		copy(*m.Slice(), bytes.Repeat([]byte{0xa5}, stack))
		m.X_initialize()
		r := m.Xkolo_rs_new(int32(c[0]), int32(c[1]), int32(c[2]))
		if r <= 0 {
			t.Fatalf("%v: Xkolo_rs_new = %d", c, r)
		}
		frames := stageSamples / c[0]
		in, out, counts := m.Xkolo_malloc(int32(2*frames*c[0])), m.Xkolo_malloc(int32(2*frames*c[0])), m.Xkolo_malloc(8)
		if in == 0 || out == 0 || counts == 0 {
			t.Fatalf("%v: malloc failed", c)
		}
		for range 5 {
			if res := m.Xkolo_rs_process(r, in, int32(frames), out, int32(frames), counts); res != 0 {
				t.Fatalf("%v: process = %d", c, res)
			}
		}
		m.Xkolo_rs_expected(r, 1<<40)
		m.Xkolo_rs_input_latency(r)
		m.Xkolo_rs_output_latency(r)
		m.Xkolo_rs_reset(r)
		m.Xkolo_rs_free(r)
		mem := *m.Slice()
		low := 0
		for low < stack && mem[low] == 0xa5 {
			low++
		}
		worst = max(worst, stack-low)
		if 2*len(mem) > maxMem*pageSize {
			t.Errorf("%v: memory is %d of %d pages, want at most half", c, len(mem)/pageSize, maxMem)
		}
		t.Logf("%v: stack used %d of %d bytes, memory %d pages", c, stack-low, stack, len(mem)/pageSize)
	}
	if 2*worst > stack {
		t.Errorf("stack high water %d of %d bytes leaves less than a factor 2", worst, stack)
	}
}

func TestMemoryBound(t *testing.T) {
	e, err := NewEngine(1, 48000, 44100)
	if err != nil {
		t.Fatal(err)
	}
	if p := e.m.Xkolo_malloc(1 << 30); p != 0 {
		t.Errorf("malloc of 1 GiB returned %d, want 0", p)
	}
	if got := len(*e.m.Slice()) / pageSize; got > maxMem {
		t.Errorf("memory is %d pages, cap is %d", got, maxMem)
	}
	if p := e.m.Xkolo_malloc(maxMem * pageSize); p != 0 {
		t.Errorf("malloc of the whole cap returned %d, want 0", p)
	}
	if p := e.m.Xkolo_malloc(maxMem * pageSize / 2); p == 0 {
		t.Error("malloc of half the cap failed")
	}
}

func process(t *testing.T, e *Engine, src []int16) ([]int16, error) {
	t.Helper()
	dst := make([]int16, len(src)*2)
	w, c, err := e.Process(dst, src, len(dst), len(src))
	if err == nil && c != len(src) {
		t.Fatalf("consumed %d of %d frames", c, len(src))
	}
	return dst[:w], err
}

func TestTrapRebuilds(t *testing.T) {
	src := noise(5000)
	fresh, err := NewEngine(1, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	want, err := process(t, fresh, src)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(1, 44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		e.r = 0x7ffffff0
		w, c, err := e.Process(make([]int16, 100), src, 100, len(src))
		var trap *Trap
		if !errors.As(err, &trap) || w != 0 || c != 0 {
			t.Fatalf("Process on a corrupt resampler = %d, %d, %v; want a Trap", w, c, err)
		}
		if e.m != nil {
			t.Fatal("module kept after a trap")
		}
		got, err := process(t, e, src)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("after a trap: %v, output equal %v", err, slices.Equal(got, want))
		}
	}
}

func TestFault(t *testing.T) {
	e, err := NewEngine(2, 16000, 48000)
	if err != nil {
		t.Fatal(err)
	}
	e.Fault(wasihost.Exit(3))
	var trap *Trap
	if _, err := e.Expected(10); !errors.As(err, &trap) || trap.Value != wasihost.Exit(3) {
		t.Fatalf("Expected after Fault = %v", err)
	}
	if n, err := e.Expected(10); err != nil || n != 30 {
		t.Fatalf("Expected after the trap = %d, %v", n, err)
	}
}

func TestNewEngineRefused(t *testing.T) {
	for _, c := range [][3]int{{0, 48000, 44100}, {1, 0, 44100}, {1, 48000, 0}} {
		_, err := NewEngine(c[0], c[1], c[2])
		var res ResultError
		if !errors.As(err, &res) || res.OutOfMemory() {
			t.Errorf("NewEngine%v = %v, want a miniaudio result other than out of memory", c, err)
		}
	}
}
