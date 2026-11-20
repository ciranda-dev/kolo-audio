package opus_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ciranda-dev/kolo-audio/opus"
)

func TestVersion(t *testing.T) {
	if got, want := opus.Version(), "libopus 1.6.1"; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func TestEncoderTrapRecovers(t *testing.T) {
	cfg := opus.Config{Bitrate: 24000, Complexity: 5, FEC: true, PacketLossPercent: 10}
	ref, e := newEncoder(t, cfg), newEncoder(t, cfg)
	if err := e.SetDTX(true); err != nil {
		t.Fatal(err)
	}
	if err := ref.SetDTX(true); err != nil {
		t.Fatal(err)
	}
	pcm := tone(440, 48000, 1, 960, 6000)
	want, got := make([]byte, opus.PacketBufferSize), make([]byte, opus.PacketBufferSize)
	wn, err := ref.Encode(pcm, want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Encode(pcm, got); err != nil {
		t.Fatal(err)
	}
	opus.BreakEncoder(e)
	if _, err := e.Encode(pcm, got); !errors.Is(err, opus.ErrInternal) {
		t.Fatalf("Encode on a trapped codec = %v, want ErrInternal", err)
	}
	n, err := e.Encode(pcm, got)
	if err != nil {
		t.Fatalf("Encode after the trap: %v", err)
	}
	if !bytes.Equal(got[:n], want[:wn]) {
		t.Errorf("packet after the trap differs from a fresh encoder with the same settings")
	}
	opus.BreakEncoder(e)
	if err := e.SetBitrate(32000); !errors.Is(err, opus.ErrInternal) {
		t.Errorf("SetBitrate on a trapped codec = %v, want ErrInternal", err)
	}
	if err := e.SetBitrate(32000); err != nil {
		t.Errorf("SetBitrate after the trap: %v", err)
	}
	if _, err := e.Encode(pcm, got); err != nil {
		t.Errorf("Encode after SetBitrate: %v", err)
	}
	e.Close()
	if _, err := e.Encode(pcm, got); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("Encode after Close = %v", err)
	}
}

func TestDecoderTrapRecovers(t *testing.T) {
	e := newEncoder(t, opus.Config{})
	packet := encodeAll(t, e, tone(440, 48000, 1, 960, 6000))[0]
	ref, d := newDecoder(t, 48000, 1), newDecoder(t, 48000, 1)
	want, got := make([]int16, 960), make([]int16, 960)
	if _, err := ref.Decode(packet, want); err != nil {
		t.Fatal(err)
	}
	opus.BreakDecoder(d)
	if _, err := d.Decode(packet, got); !errors.Is(err, opus.ErrInternal) {
		t.Fatalf("Decode on a trapped codec = %v, want ErrInternal", err)
	}
	if n, err := d.Decode(packet, got); err != nil || n != 960 {
		t.Fatalf("Decode after the trap = %d, %v", n, err)
	}
	if !bytes.Equal(int16Bytes(got), int16Bytes(want)) {
		t.Errorf("output after the trap differs from a fresh decoder")
	}
	opus.BreakDecoder(d)
	if _, err := d.DecodedLen(packet); !errors.Is(err, opus.ErrInternal) {
		t.Errorf("DecodedLen on a trapped codec = %v", err)
	}
	if n, err := d.DecodedLen(packet); err != nil || n != 960 {
		t.Errorf("DecodedLen after the trap = %d, %v", n, err)
	}
}

func int16Bytes(p []int16) []byte {
	b := make([]byte, 0, 2*len(p))
	for _, v := range p {
		b = append(b, byte(v), byte(v>>8))
	}
	return b
}

func TestConcurrentTrap(t *testing.T) {
	e := newEncoder(t, opus.Config{FEC: true, PacketLossPercent: 5})
	d := newDecoder(t, 48000, 1)
	pcm := tone(440, 48000, 1, 960, 5000)
	stop := make(chan struct{})
	var wg, breaker sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf, out := make([]byte, opus.PacketBufferSize), make([]int16, 5760)
			for range 20 {
				n, err := e.Encode(pcm, buf)
				if err != nil && !errors.Is(err, opus.ErrInternal) {
					t.Errorf("Encode: %v", err)
					return
				}
				if err == nil {
					if _, err := d.Decode(buf[:n], out); err != nil && !errors.Is(err, opus.ErrInternal) {
						t.Errorf("Decode: %v", err)
						return
					}
				}
			}
		}()
	}
	breaker.Add(1)
	go func() {
		defer breaker.Done()
		for {
			select {
			case <-stop:
				return
			default:
				opus.BreakEncoder(e)
				opus.BreakDecoder(d)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	wg.Wait()
	close(stop)
	breaker.Wait()
	buf, out := make([]byte, opus.PacketBufferSize), make([]int16, 5760)
	var n int
	var err error
	for range 2 {
		if n, err = e.Encode(pcm, buf); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("Encode after the last trap: %v", err)
	}
	for range 2 {
		if _, err = d.Decode(buf[:n], out); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("Decode after the last trap: %v", err)
	}
}

func TestPacketSizeLimit(t *testing.T) {
	d := newDecoder(t, 48000, 1)
	pcm := make([]int16, 960)
	for _, size := range []int{opus.MaxPacketSize + 1, 1 << 20, 1 << 24} {
		p := make([]byte, size)
		if _, err := d.Decode(p, pcm); !errors.Is(err, opus.ErrInvalidPacket) {
			t.Errorf("Decode(%d bytes) = %v, want ErrInvalidPacket", size, err)
		}
		if _, err := d.DecodeFEC(p, pcm); !errors.Is(err, opus.ErrInvalidPacket) {
			t.Errorf("DecodeFEC(%d bytes) = %v, want ErrInvalidPacket", size, err)
		}
		if _, err := d.DecodedLen(p); !errors.Is(err, opus.ErrInvalidPacket) {
			t.Errorf("DecodedLen(%d bytes) = %v, want ErrInvalidPacket", size, err)
		}
	}
	// The longest packet without padding is accepted by the wrapper; libopus
	// judges its content.
	if _, err := d.Decode(make([]byte, opus.MaxPacketSize), pcm); !errors.Is(err, opus.ErrInvalidPacket) {
		t.Errorf("Decode(MaxPacketSize bytes of zero) = %v", err)
	}
	// 48 CELT frames of 2.5 ms with 1275 bytes each, the longest CBR packet.
	p := append([]byte{0x83, 48}, make([]byte, 48*1275)...)
	if n, err := d.DecodedLen(p); err != nil || n != 5760 {
		t.Errorf("DecodedLen(%d bytes) = %d, %v, want 5760", len(p), n, err)
	}
}

// padded wraps a valid code 0 packet as a code 3 packet with padding, total bytes long (RFC 6716 section 3.2.5).
func padded(t *testing.T, packet []byte, total int) []byte {
	t.Helper()
	frame := packet[1:]
	rest := total - 3 - len(frame) // 255*k + b, with k bytes of 255 and a last byte b
	k, b := rest/255, rest%255
	p := []byte{packet[0]&0xFC | 3, 0x41} // code 3, one frame, padding present
	for range k {
		p = append(p, 255)
	}
	p = append(p, byte(b))
	p = append(p, frame...)
	p = append(p, make([]byte, k*254+b)...)
	if len(p) != total {
		t.Fatalf("built %d bytes, want %d", len(p), total)
	}
	return p
}

// TestPacketSizeLimitPadded checks the bound with packets libopus itself decodes: a valid packet
// with padding up to MaxPacketSize is accepted, one byte more is refused before the codec sees it.
func TestPacketSizeLimitPadded(t *testing.T) {
	e := newEncoder(t, opus.Config{})
	packet := encodeAll(t, e, tone(440, 48000, 1, 960, 6000))[0]
	d := newDecoder(t, 48000, 1)
	pcm := make([]int16, 5760)
	ok := padded(t, packet, opus.MaxPacketSize)
	if n, err := d.Decode(ok, pcm); err != nil || n != 960 {
		t.Fatalf("Decode(%d bytes) = %d, %v", len(ok), n, err)
	}
	if n, err := d.DecodedLen(ok); err != nil || n != 960 {
		t.Fatalf("DecodedLen(%d bytes) = %d, %v", len(ok), n, err)
	}
	big := padded(t, packet, opus.MaxPacketSize+1)
	if _, err := d.Decode(big, pcm); !errors.Is(err, opus.ErrInvalidPacket) {
		t.Errorf("Decode(%d bytes) = %v, want ErrInvalidPacket", len(big), err)
	}
	if _, err := d.DecodeFEC(big, pcm[:960]); !errors.Is(err, opus.ErrInvalidPacket) {
		t.Errorf("DecodeFEC(%d bytes) = %v, want ErrInvalidPacket", len(big), err)
	}
	if _, err := d.DecodedLen(big); !errors.Is(err, opus.ErrInvalidPacket) {
		t.Errorf("DecodedLen(%d bytes) = %v, want ErrInvalidPacket", len(big), err)
	}
}

// TestEncodeShortOut documents that an out shorter than PacketBufferSize limits the packet to len(out) bytes.
func TestEncodeShortOut(t *testing.T) {
	e := newEncoder(t, opus.Config{})
	pcm := tone(440, 48000, 1, 960, 6000)
	for _, size := range []int{1, 20, 100} {
		n, err := e.Encode(pcm, make([]byte, size))
		if err != nil || n < 1 || n > size {
			t.Errorf("Encode into %d bytes = %d, %v", size, n, err)
		}
	}
}

func TestOversizedPCMBuffer(t *testing.T) {
	e := newEncoder(t, opus.Config{})
	packet := encodeAll(t, e, tone(440, 48000, 1, 960, 6000))[0]
	d := newDecoder(t, 48000, 1)
	pcm := make([]int16, 48000*10)
	for i := range pcm {
		pcm[i] = 0x5a5a
	}
	n, err := d.Decode(packet, pcm)
	if err != nil || n != 960 {
		t.Fatalf("Decode into 10 s = %d, %v", n, err)
	}
	for i, v := range pcm[n:] {
		if v != 0x5a5a {
			t.Fatalf("Decode wrote past the packet duration at %d", n+i)
		}
	}
	if _, err := d.DecodeLost(pcm); !errors.Is(err, opus.ErrFrameSize) {
		t.Errorf("DecodeLost(10 s) = %v, want ErrFrameSize", err)
	}
	if _, err := e.Encode(make([]int16, 960*50), make([]byte, opus.PacketBufferSize)); !errors.Is(err, opus.ErrFrameSize) {
		t.Errorf("Encode(50 frames) = %v, want ErrFrameSize", err)
	}
	big := make([]byte, 1<<20)
	if n, err := e.Encode(tone(440, 48000, 1, 960, 6000), big); err != nil || n > opus.PacketBufferSize {
		t.Errorf("Encode into 1 MiB = %d, %v", n, err)
	}
}

func TestZeroAllocs(t *testing.T) {
	e := newEncoder(t, opus.Config{Channels: 2, FEC: true, PacketLossPercent: 10})
	d := newDecoder(t, 48000, 2)
	pcm := tone(440, 48000, 2, 960, 6000)
	buf, out := make([]byte, opus.PacketBufferSize), make([]int16, 5760*2)
	n, err := e.Encode(pcm, buf)
	if err != nil {
		t.Fatal(err)
	}
	packet := append([]byte(nil), buf[:n]...)
	check := func(name string, f func() error) {
		t.Helper()
		var ferr error
		if a := testing.AllocsPerRun(50, func() { ferr = f() }); a != 0 || ferr != nil {
			t.Errorf("%s: %v allocations per call, error %v", name, a, ferr)
		}
	}
	check("Encode", func() error { _, err := e.Encode(pcm, buf); return err })
	cbr := newEncoder(t, opus.Config{Channels: 2, CBR: true})
	check("Encode with CBR", func() error { _, err := cbr.Encode(pcm, buf); return err })
	check("Decode", func() error { _, err := d.Decode(packet, out); return err })
	check("DecodeFEC", func() error { _, err := d.DecodeFEC(packet, out[:1920]); return err })
	check("DecodeLost", func() error { _, err := d.DecodeLost(out[:1920]); return err })
	check("DecodedLen", func() error { _, err := d.DecodedLen(packet); return err })
}

// TestWorstCaseMemory checks that the largest configurations fit in the
// module's memory limit.
func TestWorstCaseMemory(t *testing.T) {
	e := newEncoder(t, opus.Config{
		Channels: 2, Application: opus.Audio, Bitrate: 512000, Complexity: 10,
		FEC: true, PacketLossPercent: 100, DTX: true, FrameDuration: 60 * time.Millisecond,
	})
	d := newDecoder(t, 48000, 2)
	packets := encodeAll(t, e, goldenPCM(48000, 2, 48000))
	out := make([]int16, 5760*2)
	for _, p := range packets {
		if _, err := d.Decode(p, out); err != nil {
			t.Fatal(err)
		}
	}
	long := append([]byte{0x83, 48}, make([]byte, 48*1275)...)
	if _, err := d.DecodedLen(long); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Decode(packets[0], out); err != nil {
		t.Fatalf("Decode after the longest packet: %v", err)
	}
	if _, err := d.DecodeLost(out); err != nil {
		t.Fatal(err)
	}
}
