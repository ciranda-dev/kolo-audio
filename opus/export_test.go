package opus

import (
	"encoding/binary"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
	"github.com/ciranda-dev/kolo-audio/opus/internal/libopus"
)

const MaxPacketSize = maxPacketSize

const badState = 0x7ffffff0

// BreakEncoder points the codec state outside the module's memory, so the
// next call into the generated code traps.
func BreakEncoder(e *Encoder) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.c != nil {
		e.c.st = badState
	}
}

// BreakDecoder is BreakEncoder for a Decoder.
func BreakDecoder(d *Decoder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.c != nil {
		d.c.st = badState
	}
}

// Version returns the version string of the translated libopus.
func Version() string {
	m := libopus.New(wasihost.Host{})
	m.X_initialize()
	b := (*m.Slice())[m.Xopus_get_version_string():]
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// FinalRange returns the range coder state after the last Decode.
func (d *Decoder) FinalRange() (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.live()
	if err != nil {
		return 0, err
	}
	const getFinalRange = 4031
	if code := c.m.Xkolo_dec_get_u32(c.st, getFinalRange, c.word); code != 0 {
		return 0, codeError(int(code))
	}
	return binary.LittleEndian.Uint32(c.mem()[c.word:]), nil
}
