package libopus_test

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/ciranda-dev/kolo-audio/opus/internal/libopus"
)

const (
	appAudio    = 2049
	appLowDelay = 2051
	setFEC      = 4012
	setLoss     = 4014
	setForce    = 11002
	maxPacket   = 2 + 47*2 + 48*1275 // RFC 6716 section 3.2.5: 48 frames of at most 1275 bytes
	maxFrame    = 5760               // 120 ms at 48 kHz
	allocFail   = -7
	pageSize    = 65536
)

// wantMaxPages is the --max-memory value in build/libopus/build.sh (MAXMEM / 65536).
const wantMaxPages = 16

type rig struct {
	m              *libopus.Module
	errp, out      int32
	pkt, pcm, dpcm int32
}

func newRig(t testing.TB) *rig {
	m := newModule()
	r := &rig{m: m}
	r.errp = m.Xkolo_malloc(4)
	r.pkt = m.Xkolo_malloc(maxPacket)
	r.out = m.Xkolo_malloc(4000)
	r.pcm = m.Xkolo_malloc(maxFrame * 2 * 2)
	r.dpcm = m.Xkolo_malloc(maxFrame * 2 * 2)
	if r.errp == 0 || r.pkt == 0 || r.out == 0 || r.pcm == 0 || r.dpcm == 0 {
		t.Fatal("rig allocation failed")
	}
	return r
}

func (r *rig) setEnc(t testing.TB, enc, req, v int32) {
	t.Helper()
	if c := r.m.Xkolo_enc_set(enc, req, v); c != 0 {
		t.Fatalf("encoder ctl %d = %d: %d", req, v, c)
	}
}

// signal fills n interleaved samples with a mix of tones, noise bursts and silence.
func signal(rng *rand.Rand, n, ch int, loud bool) []int16 {
	s := make([]int16, n*ch)
	f1, f2 := 100+rng.Float64()*3000, 200+rng.Float64()*6000
	amp := 6000.0
	if loud {
		amp = 14000
	}
	for i := range n {
		var v float64
		switch (i / 2400) % 4 {
		case 0:
			v = amp * (math.Sin(2*math.Pi*f1*float64(i)/48000) + 0.5*math.Sin(2*math.Pi*f2*float64(i)/48000))
		case 1:
			v = (rng.Float64()*2 - 1) * amp * 1.5
		case 2:
			v = 0
		default:
			v = amp * math.Sin(2*math.Pi*f1*(1+float64(i%2400)/600)*float64(i)/48000)
		}
		v = max(-32768, min(32767, v))
		for c := range ch {
			s[i*ch+c] = int16(v * (1 - 0.3*float64(c)))
		}
	}
	return s
}

func (r *rig) write(p int32, s []int16) {
	b := mem(r.m)[p:]
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
}

type encCfg struct {
	rate, ch, frame, kbps, cx, app, force int
	fec                                   bool
}

// run encodes and decodes about 0.4 s of audio with every packet through a normal
// decode, a decode with FEC after a simulated loss and concealment.
func (r *rig) run(t testing.TB, c encCfg, rng *rand.Rand) {
	t.Helper()
	m := r.m
	frame := c.frame * c.rate / 48000
	enc := m.Xopus_encoder_create(int32(c.rate), int32(c.ch), int32(c.app), r.errp)
	if enc == 0 {
		t.Fatalf("encoder create: %d", i32(m, r.errp))
	}
	dec := m.Xopus_decoder_create(int32(c.rate), int32(c.ch), r.errp)
	if dec == 0 {
		t.Fatalf("decoder create: %d", i32(m, r.errp))
	}
	r.setEnc(t, enc, setBitrate, int32(c.kbps*1000))
	r.setEnc(t, enc, setComplexity, int32(c.cx))
	if c.fec {
		r.setEnc(t, enc, setFEC, 1)
		r.setEnc(t, enc, setLoss, 20)
	}
	if c.force != 0 {
		r.setEnc(t, enc, setForce, int32(c.force))
	}
	frames := max(4, 19200/c.frame)
	sig := signal(rng, frames*frame, c.ch, c.kbps >= 128)
	var prevN int32
	for f := range frames {
		r.write(r.pcm, sig[f*frame*c.ch:(f+1)*frame*c.ch])
		n := m.Xopus_encode(enc, r.pcm, int32(frame), r.out, 4000)
		if n < 0 {
			t.Fatalf("encode %+v: %d", c, n)
		}
		copy(mem(m)[r.pkt:], mem(m)[r.out:r.out+n])
		switch {
		case f%7 == 3:
			if got := m.Xopus_decode(dec, 0, 0, r.dpcm, int32(frame), 0); got != int32(frame) {
				t.Fatalf("conceal %+v: %d", c, got)
			}
		case f%7 == 4 && prevN > 0:
			if got := m.Xopus_decode(dec, r.pkt, n, r.dpcm, int32(frame), 1); got < 0 {
				t.Fatalf("fec decode %+v: %d", c, got)
			}
		}
		if got := m.Xopus_decode(dec, r.pkt, n, r.dpcm, maxFrame, 0); got != int32(frame) {
			t.Fatalf("decode %+v: %d", c, got)
		}
		prevN = n
	}
	m.Xopus_encoder_destroy(enc)
	m.Xopus_decoder_destroy(dec)
}

// matrix is every encoder shape the wrappers can request.
func matrix() []encCfg {
	var cfgs []encCfg
	frames := []int{120, 240, 480, 960, 1920, 2880, 3840, 4800, 5760}
	for _, ch := range []int{1, 2} {
		for _, fr := range frames {
			for _, app := range []int{appVoIP, appAudio, appLowDelay} {
				for _, kbps := range []int{6, 24, 64, 510} {
					cfgs = append(cfgs, encCfg{rate: 48000, ch: ch, frame: fr, kbps: kbps, cx: 10, app: app, fec: kbps != 64})
				}
			}
			for _, force := range []int{1000, 1001, 1002} {
				cfgs = append(cfgs, encCfg{rate: 48000, ch: ch, frame: fr, kbps: 32, cx: 10, app: appVoIP, force: force, fec: true})
			}
			for _, cx := range []int{0, 5} {
				cfgs = append(cfgs, encCfg{rate: 48000, ch: ch, frame: fr, kbps: 64, cx: cx, app: appAudio})
			}
		}
	}
	for _, rate := range []int{8000, 12000, 16000, 24000} {
		for _, ch := range []int{1, 2} {
			for _, fr := range []int{480, 960, 2880, 5760} {
				cfgs = append(cfgs, encCfg{rate: rate, ch: ch, frame: fr, kbps: 24, cx: 10, app: appVoIP, fec: true})
				cfgs = append(cfgs, encCfg{rate: rate, ch: ch, frame: fr, kbps: 64, cx: 10, app: appAudio})
			}
		}
	}
	return cfgs
}

// workload runs the conformance vectors when available, the encoder matrix and
// hostile packets on m, which must not have run anything else.
func workload(t testing.TB, r *rig) {
	t.Helper()
	m := r.m
	if dir := os.Getenv("KOLO_OPUS_VECTORS"); dir != "" && !raceEnabled {
		for _, ch := range []int{1, 2} {
			if packets, bad := decodeVectors(t, m, dir, int32(ch)); packets == 0 || bad != 0 {
				t.Fatalf("channels=%d packets=%d range_mismatch=%d", ch, packets, bad)
			}
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	cfgs := matrix()
	if testing.Short() || raceEnabled {
		cfgs = cfgs[:len(cfgs)/8]
	}
	for _, c := range cfgs {
		r.run(t, c, rng)
	}
	for _, ch := range []int32{1, 2} {
		dec := m.Xopus_decoder_create(48000, ch, r.errp)
		for range 3000 {
			n := 1 + rng.IntN(1500)
			b := mem(m)[r.pkt : r.pkt+int32(n)]
			for i := range b {
				b[i] = byte(rng.Uint32())
			}
			m.Xopus_decode(dec, r.pkt, int32(n), r.dpcm, maxFrame, int32(rng.IntN(2)))
		}
		m.Xopus_decoder_destroy(dec)
	}
}

// stackFloor returns the lowest stack address that holds something other than the fill.
func stackFloor(m *libopus.Module, top int32, fill byte) int32 {
	b := mem(m)[:top]
	for i := 0; i+8 <= len(b); i += 8 {
		for _, c := range b[i : i+8] {
			if c != fill {
				return int32(i)
			}
		}
	}
	return top
}

// TestStackHighWater paints the whole stack, runs the matrix and reports how deep it went.
func TestStackHighWater(t *testing.T) {
	r := newRig(t)
	top := libopus.StackTop(r.m)
	b := mem(r.m)[:top]
	for i := range b {
		b[i] = 0xA5
	}
	workload(t, r)
	floor := stackFloor(r.m, top, 0xA5)
	used := top - floor
	t.Logf("stack size=%d used=%d free=%d (%.1f%% used)", top, used, floor, 100*float64(used)/float64(top))
	if used > top/2 {
		t.Fatalf("stack high-water mark %d is more than half of %d", used, top)
	}
}

// TestMemoryPeak runs the matrix in one module that holds the largest buffers a
// wrapper allocates and checks that the peak stays at most half of the memory limit.
func TestMemoryPeak(t *testing.T) {
	r := newRig(t)
	workload(t, r)
	pages := len(mem(r.m)) / pageSize
	t.Logf("peak memory %d pages (%d KiB), limit %d pages", pages, pages*pageSize/1024, libopus.MaxMem)
	if libopus.MaxMem != wantMaxPages {
		t.Fatalf("generated maxMem = %d pages, want %d (build/libopus/build.sh MAXMEM)", libopus.MaxMem, wantMaxPages)
	}
	if pages*2 > libopus.MaxMem {
		t.Fatalf("peak %d pages is more than half of the limit %d", pages, libopus.MaxMem)
	}
}

// TestMemoryLimit exhausts the module and checks that it stops at the limit and
// that libopus reports the failure instead of growing without bound.
func TestMemoryLimit(t *testing.T) {
	m := newModule()
	for m.Xkolo_malloc(pageSize/2) != 0 {
	}
	if got := len(mem(m)); got != libopus.MaxMem*pageSize {
		t.Fatalf("memory = %d bytes after exhaustion, want the limit %d", got, libopus.MaxMem*pageSize)
	}
	if p := m.Xkolo_malloc(1 << 30); p != 0 {
		t.Fatalf("malloc(1 GiB) = %d, want 0", p)
	}
	if got := len(mem(m)); got != libopus.MaxMem*pageSize {
		t.Fatalf("memory grew to %d bytes", got)
	}
	errp := m.Xkolo_malloc(4)
	if errp == 0 {
		t.Skip("no room left for the error slot")
	}
	if enc := m.Xopus_encoder_create(48000, 2, appVoIP, errp); enc != 0 || i32(m, errp) != allocFail {
		t.Fatalf("encoder create = %d, error %d, want 0 and %d", enc, i32(m, errp), allocFail)
	}
}

// TestNoAllocsPerCall checks that encoding and decoding allocate nothing on the Go heap.
func TestNoAllocsPerCall(t *testing.T) {
	r := newRig(t)
	m := r.m
	enc := m.Xopus_encoder_create(48000, 2, appVoIP, r.errp)
	dec := m.Xopus_decoder_create(48000, 2, r.errp)
	r.setEnc(t, enc, setFEC, 1)
	r.setEnc(t, enc, setLoss, 10)
	r.write(r.pcm, signal(rand.New(rand.NewPCG(3, 4)), 960, 2, false))
	call := func() {
		n := m.Xopus_encode(enc, r.pcm, 960, r.out, 4000)
		if n <= 0 || m.Xopus_decode(dec, r.out, n, r.dpcm, maxFrame, 0) != 960 {
			t.Fatal("encode or decode failed")
		}
	}
	call()
	if got := testing.AllocsPerRun(100, call); got != 0 {
		t.Fatalf("allocations per encode and decode = %v, want 0", got)
	}
}
