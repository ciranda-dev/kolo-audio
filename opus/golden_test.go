package opus_test

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ciranda-dev/kolo-audio/opus"
)

const goldenFile = "testdata/golden.txt"

// goldenPCM is built with integer arithmetic only, so it is the same on every
// target: triangle waves, noise and a stretch of silence.
func goldenPCM(rate, channels, frames int) []int16 {
	pcm := make([]int16, frames*channels)
	seed := uint32(12345)
	for i := range frames {
		var v int
		if i < frames*3/4 {
			seed = seed*1664525 + 1013904223
			tri := func(period, amp int) int {
				p := (i * period) % rate
				if p >= rate/2 {
					p = rate - p
				}
				return (p*2*amp)/(rate/2) - amp
			}
			v = tri(220, 4000) + tri(1330, 2000) + int(int16(seed>>16))/32
		}
		for c := range channels {
			pcm[i*channels+c] = int16(v - c*v/8)
		}
	}
	return pcm
}

type goldenCase struct {
	name string
	cfg  opus.Config
}

var goldenCases = []goldenCase{
	{"voip-mono-48k", opus.Config{}},
	{"voip-mono-16k-fec", opus.Config{SampleRate: 16000, Bitrate: 24000, FEC: true, PacketLossPercent: 10}},
	{"audio-stereo-48k-64k", opus.Config{Channels: 2, Application: opus.Audio, Bitrate: 64000, Complexity: 10}},
	{"lowdelay-stereo-48k-10ms", opus.Config{Channels: 2, Application: opus.LowDelay, Bitrate: 96000, FrameDuration: 10 * time.Millisecond}},
	{"voip-stereo-24k-40ms", opus.Config{SampleRate: 24000, Channels: 2, FrameDuration: 40 * time.Millisecond}},
	{"audio-mono-8k-60ms", opus.Config{SampleRate: 8000, Application: opus.Audio, FrameDuration: 60 * time.Millisecond}},
	{"audio-stereo-12k-5ms", opus.Config{SampleRate: 12000, Channels: 2, Application: opus.Audio, FrameDuration: 5 * time.Millisecond}},
	{"voip-mono-48k-2.5ms", opus.Config{FrameDuration: 2500 * time.Microsecond}},
	{"dtx-mono-48k", opus.Config{DTX: true, Bitrate: 20000}},
	{"fec-stereo-48k", opus.Config{Channels: 2, Bitrate: 32000, Complexity: 5, FEC: true, PacketLossPercent: 20}},
	{"voip-mono-48k-complexity1", opus.Config{Complexity: 1, Bitrate: 12000}},
}

func goldenLine(t *testing.T, c goldenCase) string {
	rate, ch := c.cfg.SampleRate, c.cfg.Channels
	if rate == 0 {
		rate = 48000
	}
	if ch == 0 {
		ch = 1
	}
	e := newEncoder(t, c.cfg)
	packets := encodeAll(t, e, goldenPCM(rate, ch, rate))
	enc := sha256.New()
	for _, p := range packets {
		enc.Write(binary.BigEndian.AppendUint32(nil, uint32(len(p))))
		enc.Write(p)
	}
	d := newDecoder(t, rate, ch)
	frame := e.FrameSize()
	out := make([]int16, rate*120/1000*ch)
	dec := sha256.New()
	for i, p := range packets {
		var n int
		var err error
		switch {
		case i%7 == 3 && i+1 < len(packets) && c.cfg.FEC:
			n, err = d.DecodeFEC(packets[i+1], out[:frame])
		case i%7 == 3:
			n, err = d.DecodeLost(out[:frame])
		default:
			n, err = d.Decode(p, out)
		}
		if err != nil {
			t.Fatalf("%s packet %d: %v", c.name, i, err)
		}
		for _, v := range out[:n] {
			dec.Write([]byte{byte(v), byte(v >> 8)})
		}
	}
	return fmt.Sprintf("%s packets=%d encoded=%x decoded=%x", c.name, len(packets), enc.Sum(nil), dec.Sum(nil))
}

// TestGolden pins the packets and the decoded PCM of a fixed matrix. The
// output is the same on every target. Run with KOLO_OPUS_UPDATE=1 to rewrite
// testdata/golden.txt.
func TestGolden(t *testing.T) {
	var lines []string
	for _, c := range goldenCases {
		lines = append(lines, goldenLine(t, c))
	}
	got := strings.Join(lines, "\n") + "\n"
	if os.Getenv("KOLO_OPUS_UPDATE") != "" {
		if err := os.WriteFile(goldenFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("golden mismatch\ngot:\n%swant:\n%s", got, want)
	}
}
