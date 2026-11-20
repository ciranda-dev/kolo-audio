package opus

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
	"github.com/ciranda-dev/kolo-audio/opus/internal/libopus"
)

// Request numbers from opus_defines.h.
const (
	reqSetBitrate    = 4002
	reqSetVBR        = 4006
	reqSetComplexity = 4010
	reqSetFEC        = 4012
	reqSetPacketLoss = 4014
	reqSetDTX        = 4016
	opusAuto         = -1000
)

var applications = [...]int32{VoIP: 2048, Audio: 2049, LowDelay: 2051}

// core is one libopus instance: a generated module with one encoder or
// decoder state and its scratch buffers in the module's memory.
type core struct {
	m      *libopus.Module
	failed error
	st     int32
	word   int32
	pkt    int32
	pktCap int32
	pcm    int32
	pcmCap int32
}

// guard turns a panic of the generated code into ErrInternal and poisons c.
func (c *core) guard(err *error) {
	if r := recover(); r != nil {
		c.failed = fmt.Errorf("%w: %v", ErrInternal, r)
		*err = c.failed
	}
}

func (c *core) mem() []byte { return *c.m.Slice() }

func (c *core) word32() int32 {
	return int32(binary.LittleEndian.Uint32(c.mem()[c.word:]))
}

func (c *core) malloc(n int) (int32, error) {
	if n > math.MaxInt32 {
		return 0, ErrAllocFail
	}
	p := c.m.Xkolo_malloc(int32(n))
	if p == 0 {
		return 0, ErrAllocFail
	}
	return p, nil
}

// growPacket makes the packet buffer hold n bytes.
func (c *core) growPacket(n int) error {
	if n <= int(c.pktCap) {
		return nil
	}
	p, err := c.malloc(n)
	if err != nil {
		return err
	}
	if c.pkt != 0 {
		c.m.Xkolo_free(c.pkt)
	}
	c.pkt, c.pktCap = p, int32(n)
	return nil
}

func newCore(packetCap, pcmBytes int) (c *core, err error) {
	c = &core{m: libopus.New(wasihost.Host{})}
	defer c.guard(&err)
	c.m.X_initialize()
	if c.word, err = c.malloc(4); err != nil {
		return nil, err
	}
	if err = c.growPacket(packetCap); err != nil {
		return nil, err
	}
	if c.pcm, err = c.malloc(pcmBytes); err != nil {
		return nil, err
	}
	c.pcmCap = int32(pcmBytes)
	return c, nil
}

// newEncoderCore returns a core holding an encoder; frameSamples is the
// number of samples per Encode call, all channels together.
func newEncoderCore(rate, channels int, app Application, frameSamples int) (c *core, err error) {
	c, err = newCore(PacketBufferSize, frameSamples*2)
	if err != nil {
		return nil, err
	}
	defer c.guard(&err)
	c.st = c.m.Xopus_encoder_create(int32(rate), int32(channels), applications[app], c.word)
	if code := c.word32(); c.st == 0 || code != 0 {
		return nil, codeError(int(code))
	}
	return c, nil
}

// newDecoderCore returns a core holding a decoder.
func newDecoderCore(rate, channels int) (c *core, err error) {
	c, err = newCore(PacketBufferSize, rate*120/1000*channels*2)
	if err != nil {
		return nil, err
	}
	defer c.guard(&err)
	c.st = c.m.Xopus_decoder_create(int32(rate), int32(channels), c.word)
	if code := c.word32(); c.st == 0 || code != 0 {
		return nil, codeError(int(code))
	}
	return c, nil
}

func (c *core) encode(pcm []int16, frames int, out []byte) (n int, err error) {
	defer c.guard(&err)
	b := c.mem()[c.pcm : c.pcm+int32(2*len(pcm))]
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	r := c.m.Xopus_encode(c.st, c.pcm, int32(frames), c.pkt, int32(min(len(out), PacketBufferSize)))
	if r < 0 {
		return 0, codeError(int(r))
	}
	return copy(out, c.mem()[c.pkt:c.pkt+r]), nil
}

func (c *core) encoderSet(req, v int32) (err error) {
	defer c.guard(&err)
	if code := c.m.Xkolo_enc_set(c.st, req, v); code != 0 {
		return codeError(int(code))
	}
	return nil
}

// decode returns the samples written to pcm, all channels together. A nil
// packet conceals a loss.
func (c *core) decode(packet []byte, pcm []int16, frames, channels int, fec bool) (n int, err error) {
	defer c.guard(&err)
	var p int32
	if packet != nil {
		if err := c.growPacket(len(packet)); err != nil {
			return 0, err
		}
		p = c.pkt
		copy(c.mem()[p:], packet)
	}
	f := int32(0)
	if fec {
		f = 1
	}
	r := c.m.Xopus_decode(c.st, p, int32(len(packet)), c.pcm, int32(frames), f)
	if r < 0 {
		return 0, codeError(int(r))
	}
	n = int(r) * channels
	b := c.mem()[c.pcm : c.pcm+int32(2*n)]
	for i := range n {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return n, nil
}

func (c *core) samples(packet []byte) (n int, err error) {
	defer c.guard(&err)
	if err := c.growPacket(len(packet)); err != nil {
		return 0, err
	}
	copy(c.mem()[c.pkt:], packet)
	r := c.m.Xopus_decoder_get_nb_samples(c.st, c.pkt, int32(len(packet)))
	if r < 0 {
		return 0, codeError(int(r))
	}
	return int(r), nil
}
