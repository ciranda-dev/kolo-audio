package libopus_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
	"github.com/ciranda-dev/kolo-audio/opus/internal/libopus"
)

var _ libopus.Xwasi_snapshot_preview1 = wasihost.Host{}

const (
	appVoIP       = 2048
	setBitrate    = 4002
	getBitrate    = 4003
	setComplexity = 4010
	getComplexity = 4011
	getFinalRange = 4031
	errBadArg     = -1
)

func newModule() *libopus.Module {
	m := libopus.New(wasihost.Host{})
	m.X_initialize()
	return m
}

func mem(m *libopus.Module) []byte { return *m.Slice() }

func cstring(m *libopus.Module, p int32) string {
	b := mem(m)[p:]
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func i32(m *libopus.Module, p int32) int32 {
	return int32(binary.LittleEndian.Uint32(mem(m)[p:]))
}

func TestVersion(t *testing.T) {
	m := newModule()
	if got, want := cstring(m, m.Xopus_get_version_string()), "libopus 1.6.1"; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	const frame = 960
	m := newModule()
	errp := m.Xkolo_malloc(4)
	enc := m.Xopus_encoder_create(48000, 1, appVoIP, errp)
	if enc == 0 || i32(m, errp) != 0 {
		t.Fatalf("encoder create = %d, error %d", enc, i32(m, errp))
	}
	dec := m.Xopus_decoder_create(48000, 1, errp)
	if dec == 0 || i32(m, errp) != 0 {
		t.Fatalf("decoder create = %d, error %d", dec, i32(m, errp))
	}
	if r := m.Xkolo_enc_set(enc, setBitrate, 32000); r != 0 {
		t.Fatalf("set bitrate = %d", r)
	}
	if r := m.Xkolo_enc_set(enc, setComplexity, 7); r != 0 {
		t.Fatalf("set complexity = %d", r)
	}
	out := m.Xkolo_malloc(4)
	if r := m.Xkolo_enc_get(enc, getBitrate, out); r != 0 || i32(m, out) != 32000 {
		t.Fatalf("get bitrate = %d, status %d", i32(m, out), r)
	}
	if r := m.Xkolo_enc_get(enc, getComplexity, out); r != 0 || i32(m, out) != 7 {
		t.Fatalf("get complexity = %d, status %d", i32(m, out), r)
	}

	pcm := m.Xkolo_malloc(frame * 2)
	pkt := m.Xkolo_malloc(1500)
	dst := m.Xkolo_malloc(frame * 2)
	var energy float64
	for f := range 10 {
		b := mem(m)[pcm:]
		for i := range frame {
			v := 8000 * math.Sin(2*math.Pi*1000*float64(f*frame+i)/48000)
			binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(v)))
		}
		n := m.Xopus_encode(enc, pcm, frame, pkt, 1500)
		if n <= 0 {
			t.Fatalf("encode = %d", n)
		}
		if got := m.Xopus_packet_get_nb_samples(pkt, n, 48000); got != frame {
			t.Fatalf("packet samples = %d, want %d", got, frame)
		}
		if got := m.Xopus_decoder_get_nb_samples(dec, pkt, n); got != frame {
			t.Fatalf("decoder samples = %d, want %d", got, frame)
		}
		if got := m.Xopus_decode(dec, pkt, n, dst, frame, 0); got != frame {
			t.Fatalf("decode = %d", got)
		}
		if f >= 5 {
			b = mem(m)[dst:]
			for i := range frame {
				v := float64(int16(binary.LittleEndian.Uint16(b[2*i:])))
				energy += v * v
			}
		}
	}
	if rms := math.Sqrt(energy / (5 * frame)); rms < 3000 || rms > 8000 {
		t.Fatalf("decoded rms = %.0f, want near 5657", rms)
	}
	if r := m.Xkolo_enc_get_u32(enc, getFinalRange, out); r != 0 {
		t.Fatalf("final range status %d", r)
	}
	if r := m.Xkolo_enc_reset(enc); r != 0 {
		t.Fatalf("encoder reset = %d", r)
	}
	if r := m.Xkolo_dec_reset(dec); r != 0 {
		t.Fatalf("decoder reset = %d", r)
	}
	m.Xopus_encoder_destroy(enc)
	m.Xopus_decoder_destroy(dec)
	m.Xkolo_free(pcm)
	m.Xkolo_free(pkt)
	m.Xkolo_free(dst)
	m.Xkolo_free(out)
	m.Xkolo_free(errp)
}

func TestInvalidArguments(t *testing.T) {
	m := newModule()
	errp := m.Xkolo_malloc(4)
	if enc := m.Xopus_encoder_create(44100, 1, appVoIP, errp); enc != 0 || i32(m, errp) != errBadArg {
		t.Fatalf("encoder at 44100 Hz = %d, error %d, want 0 and %d", enc, i32(m, errp), errBadArg)
	}
	if dec := m.Xopus_decoder_create(48000, 3, errp); dec != 0 || i32(m, errp) != errBadArg {
		t.Fatalf("decoder with 3 channels = %d, error %d, want 0 and %d", dec, i32(m, errp), errBadArg)
	}
}

func TestOutOfBoundsPanics(t *testing.T) {
	m := newModule()
	errp := m.Xkolo_malloc(4)
	dec := m.Xopus_decoder_create(48000, 1, errp)
	pkt := m.Xkolo_malloc(4)
	defer func() {
		if recover() == nil {
			t.Fatal("decode into an address past the end of memory did not panic")
		}
	}()
	m.Xopus_decode(dec, pkt, 0, int32(len(mem(m)))+1<<20, 960, 0)
}

func TestInstancesAreIndependent(t *testing.T) {
	a, b := newModule(), newModule()
	errp := a.Xkolo_malloc(4)
	enc := a.Xopus_encoder_create(48000, 1, appVoIP, errp)
	a.Xkolo_enc_set(enc, setBitrate, 24000)
	out := b.Xkolo_malloc(4)
	if enc2 := b.Xopus_encoder_create(48000, 1, appVoIP, b.Xkolo_malloc(4)); b.Xkolo_enc_get(enc2, getBitrate, out) != 0 || i32(b, out) == 24000 {
		t.Fatalf("second instance sees the first one's bitrate")
	}
}

// decodeVectors decodes the twelve RFC 8251 vectors in dir with a stereo or mono
// decoder and counts the packets whose range coder final state differs.
func decodeVectors(t testing.TB, m *libopus.Module, dir string, channels int32) (packets, bad int) {
	const maxFrame = 5760
	errp := m.Xkolo_malloc(4)
	pkt := m.Xkolo_malloc(4000)
	pcm := m.Xkolo_malloc(maxFrame * 2 * 2)
	rng := m.Xkolo_malloc(4)
	defer func() {
		m.Xkolo_free(errp)
		m.Xkolo_free(pkt)
		m.Xkolo_free(pcm)
		m.Xkolo_free(rng)
	}()
	for v := 1; v <= 12; v++ {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("testvector%02d.bit", v)))
		if err != nil {
			t.Fatal(err)
		}
		dec := m.Xopus_decoder_create(48000, channels, errp)
		if dec == 0 {
			t.Fatalf("decoder create: %d", i32(m, errp))
		}
		for len(data) >= 8 {
			n := int(binary.BigEndian.Uint32(data))
			want := binary.BigEndian.Uint32(data[4:])
			if n > 4000 || len(data) < 8+n {
				t.Fatalf("vector %d: bad packet length %d", v, n)
			}
			copy(mem(m)[pkt:], data[8:8+n])
			data = data[8+n:]
			if got := m.Xopus_decode(dec, pkt, int32(n), pcm, maxFrame, 0); got <= 0 {
				t.Fatalf("vector %d: decode = %d", v, got)
			}
			if r := m.Xkolo_dec_get_u32(dec, getFinalRange, rng); r != 0 {
				t.Fatalf("final range status %d", r)
			}
			packets++
			if uint32(i32(m, rng)) != want {
				bad++
			}
		}
		m.Xopus_decoder_destroy(dec)
	}
	return packets, bad
}

// TestVectorRanges decodes the RFC 8251 vectors in $KOLO_OPUS_VECTORS as mono and
// stereo (24 runs) and checks the range coder final state of every packet.
func TestVectorRanges(t *testing.T) {
	dir := os.Getenv("KOLO_OPUS_VECTORS")
	if dir == "" {
		t.Skip("KOLO_OPUS_VECTORS is not set")
	}
	m := newModule()
	for _, ch := range []int32{1, 2} {
		packets, bad := decodeVectors(t, m, dir, ch)
		t.Logf("channels=%d packets=%d range_mismatch=%d", ch, packets, bad)
		if packets == 0 || bad != 0 {
			t.Fatalf("channels=%d packets=%d range_mismatch=%d", ch, packets, bad)
		}
	}
}
