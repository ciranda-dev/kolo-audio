package opus_test

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ciranda-dev/kolo-audio/opus"
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

func newEncoder(t testing.TB, cfg opus.Config) *opus.Encoder {
	t.Helper()
	e, err := opus.NewEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func newDecoder(t testing.TB, rate, channels int) *opus.Decoder {
	t.Helper()
	d, err := opus.NewDecoder(rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// encodeAll encodes pcm frame by frame and returns the packets.
func encodeAll(t *testing.T, e *opus.Encoder, pcm []int16) [][]byte {
	t.Helper()
	var packets [][]byte
	buf := make([]byte, opus.PacketBufferSize)
	for len(pcm) >= e.FrameSize() {
		n, err := e.Encode(pcm[:e.FrameSize()], buf)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), buf[:n]...))
		pcm = pcm[e.FrameSize():]
	}
	return packets
}

// snr returns the signal-to-noise ratio in dB of got against want, with got
// delayed by the codec delay, searched up to maxDelay samples.
func snr(want, got []int16, maxDelay int) (float64, int) {
	best, bestDelay := math.Inf(-1), 0
	for delay := 0; delay <= maxDelay; delay++ {
		var sig, noise float64
		for i := 0; i+delay < len(got) && i < len(want); i++ {
			s, g := float64(want[i]), float64(got[i+delay])
			sig += s * s
			noise += (s - g) * (s - g)
		}
		if v := 10 * math.Log10(sig/noise); v > best {
			best, bestDelay = v, delay
		}
	}
	return best, bestDelay
}

func TestRoundTripSNR(t *testing.T) {
	for _, app := range []opus.Application{opus.VoIP, opus.Audio, opus.LowDelay} {
		e := newEncoder(t, opus.Config{Application: app, Bitrate: 64000})
		d := newDecoder(t, 48000, 1)
		src := tone(1000, 48000, 1, 48000, 10000)
		var out []int16
		pcm := make([]int16, 5760)
		for _, p := range encodeAll(t, e, src) {
			n, err := d.Decode(p, pcm)
			if err != nil {
				t.Fatal(err)
			}
			if n != 960 {
				t.Fatalf("decoded %d samples, want 960", n)
			}
			out = append(out, pcm[:n]...)
		}
		// Skip the first 100 ms while the codec settles.
		v, delay := snr(src[4800:47000], out[4800:], 1000)
		t.Logf("application %d: SNR %.1f dB at delay %d samples", app, v, delay)
		if v < 15 {
			t.Errorf("application %d: SNR %.1f dB, want at least 15", app, v)
		}
	}
}

func TestFormats(t *testing.T) {
	durations := []time.Duration{2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond}
	for _, rate := range []int{8000, 12000, 16000, 24000, 48000} {
		for _, ch := range []int{1, 2} {
			for _, dur := range durations {
				e := newEncoder(t, opus.Config{SampleRate: rate, Channels: ch, FrameDuration: dur})
				frames := int(int64(rate) * int64(dur) / int64(time.Second))
				if e.FrameSize() != frames*ch {
					t.Fatalf("%d Hz %d ch %v: FrameSize %d, want %d", rate, ch, dur, e.FrameSize(), frames*ch)
				}
				packets := encodeAll(t, e, tone(440, rate, ch, frames*3, 5000))
				d := newDecoder(t, rate, ch)
				pcm := make([]int16, rate*120/1000*ch)
				for _, p := range packets {
					if n, err := d.DecodedLen(p); err != nil || n != frames*ch {
						t.Fatalf("%d Hz %d ch %v: DecodedLen = %d, %v", rate, ch, dur, n, err)
					}
					if n, err := d.Decode(p, pcm); err != nil || n != frames*ch {
						t.Fatalf("%d Hz %d ch %v: Decode = %d, %v", rate, ch, dur, n, err)
					}
				}
			}
		}
	}
}

func TestEncoderConfigErrors(t *testing.T) {
	bad := []opus.Config{
		{SampleRate: 44100},
		{SampleRate: -1},
		{Channels: 3},
		{Channels: -1},
		{Application: 3},
		{Application: -1},
		{Bitrate: 499},
		{Bitrate: 512001},
		{Bitrate: -1},
		{Complexity: 11},
		{Complexity: -1},
		{PacketLossPercent: 101},
		{PacketLossPercent: -1},
		{FrameDuration: 30 * time.Millisecond},
		{FrameDuration: 120 * time.Millisecond},
		{FrameDuration: -1},
	}
	for _, cfg := range bad {
		if _, err := opus.NewEncoder(cfg); !errors.Is(err, opus.ErrInvalidConfig) {
			t.Errorf("NewEncoder(%+v) = %v, want ErrInvalidConfig", cfg, err)
		}
	}
	for _, c := range [][2]int{{44100, 1}, {48000, 0}, {48000, 3}, {0, 1}} {
		if _, err := opus.NewDecoder(c[0], c[1]); !errors.Is(err, opus.ErrInvalidConfig) {
			t.Errorf("NewDecoder%v = %v, want ErrInvalidConfig", c, err)
		}
	}
	e := newEncoder(t, opus.Config{Bitrate: 500, Complexity: 10, FEC: true, PacketLossPercent: 100, DTX: true})
	checks := []struct {
		err  error
		want error
	}{
		{e.SetBitrate(0), nil},
		{e.SetBitrate(512000), nil},
		{e.SetBitrate(100), opus.ErrInvalidArgument},
		{e.SetComplexity(0), nil},
		{e.SetComplexity(11), opus.ErrInvalidArgument},
		{e.SetPacketLossPercent(0), nil},
		{e.SetPacketLossPercent(-5), opus.ErrInvalidArgument},
		{e.SetFEC(false), nil},
		{e.SetDTX(false), nil},
		{e.SetCBR(true), nil},
		{e.SetCBR(false), nil},
	}
	for i, c := range checks {
		if !errors.Is(c.err, c.want) {
			t.Errorf("setter %d: %v, want %v", i, c.err, c.want)
		}
	}
}

func TestWrongFrameSize(t *testing.T) {
	e := newEncoder(t, opus.Config{Channels: 2})
	out := make([]byte, opus.PacketBufferSize)
	for _, n := range []int{0, 1, 959, 960, 1921, 3840} {
		if _, err := e.Encode(make([]int16, n), out); !errors.Is(err, opus.ErrFrameSize) {
			t.Errorf("Encode(%d samples) = %v, want ErrFrameSize", n, err)
		}
	}
	if _, err := e.Encode(make([]int16, 1920), nil); !errors.Is(err, opus.ErrBufferTooSmall) {
		t.Errorf("Encode into nil = %v", err)
	}
	if n, err := e.Encode(make([]int16, 1920), out); err != nil || n == 0 {
		t.Errorf("Encode = %d, %v", n, err)
	}
}

func TestMalformedPackets(t *testing.T) {
	d := newDecoder(t, 48000, 2)
	pcm := make([]int16, 5760*2)
	badHeader := [][]byte{
		nil,
		{},
		{0x03},       // code 3 without a frame count byte
		{0x03, 0x00}, // code 3 with zero frames
		{0x03, 0x31}, // code 3, 49 frames: over 120 ms
	}
	badBody := [][]byte{
		{0x01, 0x00},       // code 1 with an odd payload length
		{0x02, 0xfc},       // code 2 with a length larger than the packet
		{0x02, 0xff, 0xff}, // code 2 with a truncated two-byte length
	}
	for _, p := range append(badHeader, badBody...) {
		if _, err := d.Decode(p, pcm); !errors.Is(err, opus.ErrInvalidPacket) {
			t.Errorf("Decode(%x) = %v, want ErrInvalidPacket", p, err)
		}
	}
	for _, p := range badHeader {
		if _, err := d.DecodedLen(p); !errors.Is(err, opus.ErrInvalidPacket) {
			t.Errorf("DecodedLen(%x) = %v, want ErrInvalidPacket", p, err)
		}
	}
	if _, err := d.Decode([]byte{0x00}, make([]int16, 3)); !errors.Is(err, opus.ErrInvalidArgument) {
		t.Errorf("Decode into a partial frame: %v", err)
	}
	e := newEncoder(t, opus.Config{Channels: 2})
	p := encodeAll(t, e, tone(440, 48000, 2, 960, 5000))[0]
	if _, err := d.Decode(p, make([]int16, 960)); !errors.Is(err, opus.ErrBufferTooSmall) {
		t.Errorf("Decode 20 ms into 10 ms = %v, want ErrBufferTooSmall", err)
	}
}

func TestLossConcealment(t *testing.T) {
	e := newEncoder(t, opus.Config{Bitrate: 24000, FEC: true, PacketLossPercent: 20})
	packets := encodeAll(t, e, tone(300, 48000, 1, 48000, 8000))
	d := newDecoder(t, 48000, 1)
	pcm := make([]int16, 960)
	for _, p := range packets[:10] {
		if _, err := d.Decode(p, pcm); err != nil {
			t.Fatal(err)
		}
	}
	// Packet 10 is lost; recover it from the FEC data in packet 11.
	n, err := d.DecodeFEC(packets[11], pcm)
	if err != nil || n != 960 {
		t.Fatalf("DecodeFEC = %d, %v", n, err)
	}
	peak, _ := levelDB(pcm)
	t.Logf("FEC frame peak %.1f dBFS", peak)
	if peak < -30 {
		t.Errorf("FEC frame peak %.1f dBFS, want the tone back", peak)
	}
	if n, err := d.Decode(packets[11], pcm); err != nil || n != 960 {
		t.Fatalf("Decode after FEC = %d, %v", n, err)
	}
	// Packet 12 is lost with nothing to recover it from.
	if n, err := d.DecodeLost(pcm); err != nil || n != 960 {
		t.Fatalf("DecodeLost = %d, %v", n, err)
	}
	for _, size := range []int{0, 100, 961, 5760 + 120} {
		if _, err := d.DecodeLost(make([]int16, size)); err == nil {
			t.Errorf("DecodeLost(%d samples) accepted", size)
		}
		if _, err := d.DecodeFEC(packets[13], make([]int16, size)); err == nil {
			t.Errorf("DecodeFEC(%d samples) accepted", size)
		}
	}
	if _, err := d.DecodeFEC(nil, pcm); !errors.Is(err, opus.ErrInvalidPacket) {
		t.Errorf("DecodeFEC(nil) = %v", err)
	}
	if n, err := d.DecodeLost(make([]int16, 120)); err != nil || n != 120 {
		t.Errorf("DecodeLost(2.5 ms) = %d, %v", n, err)
	}
	if n, err := d.DecodeLost(make([]int16, 5760)); err != nil || n != 5760 {
		t.Errorf("DecodeLost(120 ms) = %d, %v", n, err)
	}
}

func levelDB(pcm []int16) (peak, rms float64) {
	var m, sum float64
	for _, v := range pcm {
		a := math.Abs(float64(v))
		m = max(m, a)
		sum += a * a
	}
	return 20 * math.Log10(m/32768), 20 * math.Log10(math.Sqrt(sum/float64(len(pcm)))/32768)
}

func TestDTX(t *testing.T) {
	e := newEncoder(t, opus.Config{DTX: true})
	packets := encodeAll(t, e, make([]int16, 48000))
	small := 0
	for _, p := range packets {
		if len(p) <= 2 {
			small++
		}
	}
	if small == 0 {
		t.Fatalf("no DTX packets in 1 s of silence")
	}
}

func TestClose(t *testing.T) {
	e := newEncoder(t, opus.Config{})
	d := newDecoder(t, 48000, 1)
	for range 2 {
		if e.Close() != nil || d.Close() != nil {
			t.Fatal("Close failed")
		}
	}
	if _, err := e.Encode(make([]int16, 960), make([]byte, 100)); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("Encode after Close = %v", err)
	}
	if err := e.SetBitrate(32000); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("SetBitrate after Close = %v", err)
	}
	if _, err := d.Decode([]byte{0}, make([]int16, 960)); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("Decode after Close = %v", err)
	}
	if _, err := d.DecodedLen([]byte{0}); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("DecodedLen after Close = %v", err)
	}
	var ze opus.Encoder
	var zd opus.Decoder
	if _, err := ze.Encode(nil, nil); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("zero Encoder: %v", err)
	}
	if _, err := zd.DecodeLost(make([]int16, 960)); !errors.Is(err, opus.ErrClosed) {
		t.Errorf("zero Decoder: %v", err)
	}
}

func TestConcurrentClose(t *testing.T) {
	e := newEncoder(t, opus.Config{})
	d := newDecoder(t, 48000, 1)
	pcm := tone(440, 48000, 1, 960, 5000)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf, out := make([]byte, opus.PacketBufferSize), make([]int16, 5760)
			for {
				n, err := e.Encode(pcm, buf)
				if err == nil {
					_, err = d.Decode(buf[:n], out)
				}
				if err != nil {
					if !errors.Is(err, opus.ErrClosed) {
						t.Errorf("got %v, want ErrClosed", err)
					}
					return
				}
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	e.Close()
	d.Close()
	wg.Wait()
}

func FuzzDecode(f *testing.F) {
	e, err := opus.NewEncoder(opus.Config{Channels: 2, FEC: true, PacketLossPercent: 10})
	if err != nil {
		f.Fatal(err)
	}
	buf := make([]byte, opus.PacketBufferSize)
	n, err := e.Encode(tone(440, 48000, 2, 960, 5000), buf)
	if err != nil {
		f.Fatal(err)
	}
	e.Close()
	f.Add(buf[:n], true)
	f.Add([]byte{0x03, 0x05, 0x01, 0x02}, false)
	f.Add([]byte{0xfc, 0xff, 0xff}, true)
	f.Fuzz(func(t *testing.T, packet []byte, fec bool) {
		for _, ch := range []int{1, 2} {
			d, err := opus.NewDecoder(48000, ch)
			if err != nil {
				t.Fatal(err)
			}
			guard := make([]int16, 5760*ch+64)
			for i := range guard {
				guard[i] = 0x5a5a
			}
			pcm := guard[:5760*ch]
			size, lenErr := d.DecodedLen(packet)
			n, err := d.Decode(packet, pcm)
			if err == nil && (lenErr != nil || n != size || n > len(pcm)) {
				t.Fatalf("Decode = %d, DecodedLen = %d, %v", n, size, lenErr)
			}
			for _, v := range guard[len(pcm):] {
				if v != 0x5a5a {
					t.Fatal("Decode wrote past pcm")
				}
			}
			if fec && len(packet) > 0 {
				if _, err := d.DecodeFEC(packet, pcm[:960*ch]); err != nil && !errors.Is(err, opus.ErrInvalidPacket) {
					t.Fatalf("DecodeFEC: %v", err)
				}
			}
			d.Close()
		}
	})
}
