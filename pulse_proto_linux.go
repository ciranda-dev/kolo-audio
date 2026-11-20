//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// Native protocol constants, from the PulseAudio sources (src/pulsecore/
// native-common.h, pstream.c, def.h).
const (
	pulseVersion     = 32         // highest protocol version this client speaks
	pulseMinVersion  = 21         // oldest server it accepts: stream formats and ports are always present
	pulseVersionMask = 0x0000FFFF // the rest of the version word carries shm flags
	pulseCookieLen   = 256
	pulseMaxFrame    = 16 << 20 // FRAME_SIZE_MAX_ALLOW in pstream.c
	pulseReadStep    = 64 << 10 // the frame buffer grows by this much per read
	pulseKeepFrame   = 1 << 20  // a larger frame buffer is dropped before the next packet
	pulseNoIndex     = 0xFFFFFFFF
	pulseMaxChannels = 32 // PA_CHANNELS_MAX
	pulseDescLen     = 20
	pulseShmMask     = 0xFF000000
	pulseSampleS16LE = 3
)

const (
	pulseOpError         = 0
	pulseOpReply         = 2
	pulseOpCreatePlay    = 3
	pulseOpCreateRecord  = 5
	pulseOpAuth          = 8
	pulseOpSetClientName = 9
	pulseOpServerInfo    = 20
	pulseOpSinkList      = 22
	pulseOpSourceList    = 24
	pulseOpRequest       = 61
	pulseOpOverflow      = 62
	pulseOpUnderflow     = 63
	pulseOpPlayKilled    = 64
	pulseOpRecordKilled  = 65
	pulseOpPlaySuspended = 76
	pulseOpRecSuspended  = 77
	pulseOpPlayMoved     = 78
	pulseOpRecMoved      = 79
	pulseOpStarted       = 86
	pulseOpPlayAttr      = 94
	pulseOpRecAttr       = 95
)

var errPulseProto = errors.New("koloaudio: malformed PulseAudio protocol data")

// pulseError is an error code sent by the server.
type pulseError uint32

var pulseErrorText = [...]string{
	"ok", "access denied", "unknown command", "invalid argument", "entity exists",
	"no such entity", "connection refused", "protocol error", "timeout",
	"no authentication key", "internal error", "connection terminated", "entity killed",
	"invalid server", "module initialization failed", "bad state", "no data",
	"incompatible protocol version", "data too large", "operation not supported",
	"unknown error code", "no such extension", "obsolete functionality",
	"missing implementation", "client forked", "input/output error", "device or resource busy",
}

const (
	pulseErrAccess       pulseError = 1
	pulseErrInvalid      pulseError = 3
	pulseErrNoEntity     pulseError = 5
	pulseErrNotSupported pulseError = 19
)

func (e pulseError) Error() string {
	if int(e) < len(pulseErrorText) {
		return "PulseAudio server: " + pulseErrorText[e]
	}
	return fmt.Sprintf("PulseAudio server: error %d", uint32(e))
}

// tagWriter builds a tagstruct.
type tagWriter struct{ b []byte }

func (w *tagWriter) u8(v byte)    { w.b = append(w.b, 'B', v) }
func (w *tagWriter) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(append(w.b, 'L'), v) }
func (w *tagWriter) usec(v uint64) {
	w.b = binary.BigEndian.AppendUint64(append(w.b, 'U'), v)
}

func (w *tagWriter) bool(v bool) {
	if v {
		w.b = append(w.b, '1')
	} else {
		w.b = append(w.b, '0')
	}
}

// str writes a string, or the null string for "".
func (w *tagWriter) str(s string) {
	if s == "" {
		w.b = append(w.b, 'N')
		return
	}
	w.b = append(append(append(w.b, 't'), s...), 0)
}

func (w *tagWriter) arbitrary(p []byte) {
	w.b = binary.BigEndian.AppendUint32(append(w.b, 'x'), uint32(len(p)))
	w.b = append(w.b, p...)
}

func (w *tagWriter) sampleSpec(s pulseSpec) {
	w.b = binary.BigEndian.AppendUint32(append(w.b, 'a', s.format, s.channels), s.rate)
}

func (w *tagWriter) channelMap(m []byte) {
	w.b = append(append(w.b, 'm', byte(len(m))), m...)
}

func (w *tagWriter) cvolume(channels int, v uint32) {
	w.b = append(w.b, 'v', byte(channels))
	for range channels {
		w.b = binary.BigEndian.AppendUint32(w.b, v)
	}
}

// proplist writes string properties in the order given.
func (w *tagWriter) proplist(kv ...string) {
	w.b = append(w.b, 'P')
	for i := 0; i+1 < len(kv); i += 2 {
		v := kv[i+1] + "\x00"
		w.b = append(append(append(w.b, 't'), kv[i]...), 0)
		w.b = binary.BigEndian.AppendUint32(append(w.b, 'L'), uint32(len(v)))
		w.arbitrary([]byte(v))
	}
	w.b = append(w.b, 'N')
}

// pulseSpec is a sample specification.
type pulseSpec struct {
	format, channels byte
	rate             uint32
}

// tagReader decodes a tagstruct. It never reads past its buffer; the first
// malformed field sets err and every later read returns zero values.
type tagReader struct {
	b   []byte
	err error
}

func (r *tagReader) fail() {
	if r.err == nil {
		r.err = errPulseProto
	}
}

func (r *tagReader) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.fail()
		return nil
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}

// tag consumes the type byte t and the n payload bytes behind it.
func (r *tagReader) field(t byte, n int) []byte {
	p := r.take(1 + n)
	if p == nil || p[0] != t {
		r.fail()
		return nil
	}
	return p[1:]
}

func (r *tagReader) u8() byte {
	if p := r.field('B', 1); p != nil {
		return p[0]
	}
	return 0
}

func (r *tagReader) u32() uint32 {
	if p := r.field('L', 4); p != nil {
		return binary.BigEndian.Uint32(p)
	}
	return 0
}

func (r *tagReader) u64() uint64 {
	if p := r.field('R', 8); p != nil {
		return binary.BigEndian.Uint64(p)
	}
	return 0
}

func (r *tagReader) i64() int64 {
	if p := r.field('r', 8); p != nil {
		return int64(binary.BigEndian.Uint64(p))
	}
	return 0
}

func (r *tagReader) usec() uint64 {
	if p := r.field('U', 8); p != nil {
		return binary.BigEndian.Uint64(p)
	}
	return 0
}

func (r *tagReader) volume() uint32 {
	if p := r.field('V', 4); p != nil {
		return binary.BigEndian.Uint32(p)
	}
	return 0
}

func (r *tagReader) bool() bool {
	p := r.take(1)
	if p == nil || (p[0] != '1' && p[0] != '0') {
		r.fail()
		return false
	}
	return p[0] == '1'
}

// str reads a string or the null string, which is "".
func (r *tagReader) str() string {
	p := r.take(1)
	if p == nil {
		return ""
	}
	switch p[0] {
	case 'N':
		return ""
	case 't':
		i := bytes.IndexByte(r.b, 0)
		if i < 0 {
			r.fail()
			return ""
		}
		return string(r.take(i + 1)[:i])
	}
	r.fail()
	return ""
}

func (r *tagReader) arbitrary() []byte {
	p := r.field('x', 4)
	if p == nil {
		return nil
	}
	return r.take(int(binary.BigEndian.Uint32(p)))
}

func (r *tagReader) sampleSpec() pulseSpec {
	if p := r.field('a', 6); p != nil {
		return pulseSpec{p[0], p[1], binary.BigEndian.Uint32(p[2:])}
	}
	return pulseSpec{}
}

func (r *tagReader) channelMap() []byte {
	p := r.field('m', 1)
	if p == nil || int(p[0]) > pulseMaxChannels {
		r.fail()
		return nil
	}
	return r.take(int(p[0]))
}

func (r *tagReader) cvolume() {
	p := r.field('v', 1)
	if p == nil || int(p[0]) > pulseMaxChannels {
		r.fail()
		return
	}
	r.take(4 * int(p[0]))
}

func (r *tagReader) timeval() {
	r.field('T', 8)
}

func (r *tagReader) proplist() {
	r.field('P', 0)
	for r.err == nil {
		if len(r.b) > 0 && r.b[0] == 'N' {
			r.b = r.b[1:]
			return
		}
		if r.str() == "" {
			r.fail()
			return
		}
		n := r.u32()
		if v := r.arbitrary(); r.err == nil && uint32(len(v)) != n {
			r.fail()
		}
	}
}

func (r *tagReader) formatInfo() {
	r.field('f', 0)
	r.u8()
	r.proplist()
}

func (r *tagReader) formats() {
	for n := r.u8(); n > 0 && r.err == nil; n-- {
		r.formatInfo()
	}
}

func (r *tagReader) eof() bool { return r.err == nil && len(r.b) == 0 }

// pulseDevInfo is the part of a sink or source description this client uses.
type pulseDevInfo struct {
	index, module uint32
	name, desc    string
}

// deviceInfo reads one sink or source record. They have the same layout, except that a source
// carries its formats only from protocol version 22 and a sink from 21.
func (r *tagReader) deviceInfo(version uint32, sink bool) pulseDevInfo {
	var d pulseDevInfo
	d.index = r.u32()
	d.name = r.str()
	d.desc = r.str()
	r.sampleSpec()
	r.channelMap()
	d.module = r.u32()
	r.cvolume()
	r.bool()
	r.u32() // monitor source index, or the sink a source monitors
	r.str()
	r.usec()
	r.str() // driver
	r.u32() // flags
	r.proplist()
	r.usec()
	r.volume()
	r.u32() // state
	r.u32() // volume steps
	r.u32() // card
	for n := r.u32(); n > 0 && r.err == nil; n-- {
		r.str()
		r.str()
		r.u32()
		if version >= 24 {
			r.u32()
		}
	}
	r.str() // active port
	if sink || version >= 22 {
		r.formats()
	}
	return d
}

// pulseServerInfo is the part of the server description this client uses.
type pulseServerInfo struct{ defaultSink, defaultSource string }

func (r *tagReader) serverInfo(version uint32) pulseServerInfo {
	var s pulseServerInfo
	r.str()
	r.str()
	r.str()
	r.str()
	r.sampleSpec()
	s.defaultSink = r.str()
	s.defaultSource = r.str()
	r.u32()
	if version >= 15 {
		r.channelMap()
	}
	return s
}

// pulseStreamAttr is the buffer metrics of a stream, in bytes.
type pulseStreamAttr struct{ maxLength, tLength, preBuf, minReq, fragSize uint32 }

// pulseCreated is a server reply to a create-stream request.
type pulseCreated struct {
	index, missing uint32
	attr           pulseStreamAttr
	spec           pulseSpec
	suspended      bool
}

func (r *tagReader) createdPlayback() pulseCreated {
	var c pulseCreated
	c.index = r.u32()
	r.u32() // sink input index
	c.missing = r.u32()
	c.attr.maxLength = r.u32()
	c.attr.tLength = r.u32()
	c.attr.preBuf = r.u32()
	c.attr.minReq = r.u32()
	c.spec = r.sampleSpec()
	r.channelMap()
	r.u32()
	r.str()
	c.suspended = r.bool()
	r.usec()
	r.formatInfo()
	return c
}

func (r *tagReader) createdRecord(version uint32) pulseCreated {
	var c pulseCreated
	c.index = r.u32()
	r.u32() // source output index
	c.attr.maxLength = r.u32()
	c.attr.fragSize = r.u32()
	c.spec = r.sampleSpec()
	r.channelMap()
	r.u32()
	r.str()
	c.suspended = r.bool()
	r.usec()
	if version >= 22 {
		r.formatInfo()
	}
	return c
}

// pulseDefaultMap is PulseAudio's default channel map for n channels
// (pa_channel_map_init_extend with PA_CHANNEL_MAP_DEFAULT): the AIFF layout
// up to six channels, then auxiliary channels.
func pulseDefaultMap(n int) []byte {
	const (
		mono, left, right, center, rearCenter, rearLeft, rearRight = 0, 1, 2, 3, 4, 5, 6
		leftOfCenter, rightOfCenter, aux0                          = 8, 9, 12
	)
	var m []byte
	switch {
	case n == 1:
		m = []byte{mono}
	case n == 2:
		m = []byte{left, right}
	case n == 3:
		m = []byte{left, right, center}
	case n == 4:
		m = []byte{left, center, right, rearCenter}
	case n == 5:
		m = []byte{left, right, center, rearLeft, rearRight}
	default:
		m = []byte{left, leftOfCenter, center, right, rightOfCenter, rearCenter}
	}
	for i, base := len(m), len(m); i < n; i++ {
		m = append(m, byte(aux0+i-base))
	}
	return m
}

// frameReader reads packets from a PulseAudio connection.
type frameReader struct {
	r    io.Reader
	hdr  [pulseDescLen]byte
	data []byte
}

// next returns the channel and payload of the next packet. The payload is
// valid until the next call. A control packet has channel pulseNoIndex.
func (f *frameReader) next() (uint32, []byte, error) {
	if _, err := io.ReadFull(f.r, f.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(f.hdr[0:])
	channel := binary.BigEndian.Uint32(f.hdr[4:])
	flags := binary.BigEndian.Uint32(f.hdr[16:])
	if n == 0 || n > pulseMaxFrame || flags&pulseShmMask != 0 {
		return 0, nil, errPulseProto
	}
	// The length comes from the server, so the buffer grows with the bytes
	// that arrive, and a large one is dropped before the next packet.
	if cap(f.data) > pulseKeepFrame {
		f.data = nil
	}
	f.data = f.data[:0]
	for left := int(n); left > 0; {
		k := min(left, pulseReadStep)
		f.data = slices.Grow(f.data, k)
		got, err := io.ReadFull(f.r, f.data[len(f.data):len(f.data)+k])
		f.data = f.data[:len(f.data)+got]
		if err != nil {
			return 0, nil, err
		}
		left -= k
	}
	return channel, f.data, nil
}

// putFrameHeader fills the first pulseDescLen bytes of dst for a packet of
// n payload bytes on channel.
func putFrameHeader(dst []byte, n int, channel uint32) {
	clear(dst[:pulseDescLen])
	binary.BigEndian.PutUint32(dst[0:], uint32(n))
	binary.BigEndian.PutUint32(dst[4:], channel)
}

// controlHeader reads the command and tag at the start of a control packet.
func controlHeader(p []byte) (op, tag uint32, body tagReader) {
	body = tagReader{b: p}
	op = body.u32()
	tag = body.u32()
	return
}
