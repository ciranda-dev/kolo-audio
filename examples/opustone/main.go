// Command opustone encodes a generated tone with Opus, drops one packet,
// decodes the rest with loss recovery and prints what happened.
package main

import (
	"fmt"
	"log"
	"math"

	koloaudio "github.com/ciranda-dev/kolo-audio"
	"github.com/ciranda-dev/kolo-audio/opus"
)

func main() {
	const rate = 48000
	pcm := make([]int16, rate)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/rate))
	}

	enc, err := opus.NewEncoder(opus.Config{Bitrate: 24000, FEC: true, PacketLossPercent: 10})
	if err != nil {
		log.Fatal(err)
	}
	defer enc.Close()
	dec, err := opus.NewDecoder(rate, 1)
	if err != nil {
		log.Fatal(err)
	}
	defer dec.Close()

	var packets [][]byte
	buf := make([]byte, opus.PacketBufferSize)
	for frame := range len(pcm) / enc.FrameSize() {
		n, err := enc.Encode(pcm[frame*enc.FrameSize():(frame+1)*enc.FrameSize()], buf)
		if err != nil {
			log.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), buf[:n]...))
	}

	const lost = 25
	var bytes int
	out := make([]int16, rate*opus.MaxPacketDuration.Milliseconds()/1000)
	for i, p := range packets {
		bytes += len(p)
		var n int
		if i == lost {
			n, err = dec.DecodeFEC(packets[i+1], out[:enc.FrameSize()])
		} else {
			n, err = dec.Decode(p, out)
		}
		if err != nil {
			log.Fatal(err)
		}
		if i == lost {
			peak, _ := koloaudio.Level(out[:n])
			fmt.Printf("packet %d lost, recovered %d samples from the next packet, peak %.1f dBFS\n", i, n, peak)
		}
	}
	fmt.Printf("%d packets of 20 ms, %d bytes, %.1f kbit/s\n", len(packets), bytes, float64(bytes*8)/1000)
}
