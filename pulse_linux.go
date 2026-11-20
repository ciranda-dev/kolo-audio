//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var errPulseNoAnswer = errors.New("koloaudio: PulseAudio server did not answer")

// pulseCallbacks receives what a stream delivers. A *stream implements it.
type pulseCallbacks interface {
	onCapture([]int16)
	onPlayback([]int16)
	onStopped()
}

func pulseList(kind Kind) ([]Device, error) {
	return pulseListEnv(osPulseEnv(), kind, pulseRequestTimeout)
}

func pulseListEnv(env pulseEnv, kind Kind, timeout time.Duration) ([]Device, error) {
	c, err := dialPulse(env, timeout)
	if err != nil {
		return nil, err
	}
	defer c.close()
	info, err := c.serverInfo()
	if err != nil {
		return nil, pulseWrap(err)
	}
	infos, err := c.devices(kind == Playback)
	if err != nil {
		return nil, pulseWrap(err)
	}
	def := info.defaultSource
	if kind == Playback {
		def = info.defaultSink
	}
	devs := make([]Device, 0, len(infos))
	for _, d := range infos {
		if d.name == "" {
			continue
		}
		name := d.desc
		if name == "" {
			name = d.name
		}
		devs = append(devs, Device{ID: hex.EncodeToString([]byte(d.name)), Name: name, Kind: kind, Default: d.name == def})
	}
	return devs, nil
}

func pulseWrap(err error) error {
	return fmt.Errorf("%w: %w", ErrBackend, err)
}

func pulseOpen(s *stream, cfg Config) (hwStream, error) {
	p, err := openPulse(s, s.kind, cfg, osPulseEnv(), pulseRequestTimeout)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// pulseStream is one capture or playback stream on its own connection.
// The server converts between the stream's s16le format and the device's.
type pulseStream struct {
	cb      pulseCallbacks
	c       *pulseConn
	capture bool
	ch      int
	frame   int // bytes per frame
	limit   int // bytes: the most one request may ask for, the requested maxlength
	// wtimeout bounds a playback write, which runs on the reader goroutine
	// with mu held. A stream that is stalled for StallTimeout is lost anyway.
	wtimeout time.Duration
	closing  atomic.Bool

	// mu is held while a callback runs, so callbacks never overlap and none
	// runs once dead is set.
	mu    sync.Mutex
	dead  bool
	index uint32
	owed  int // playback bytes the server asked for and has not got
	pcm   []int16
	out   []byte
	carry []byte // capture bytes that do not make a whole frame yet
}

// openPulse opens a stream on the device named by cfg.DeviceID, or on the
// server's default device. Buffer attributes, in bytes with one frame being
// 2*channels bytes:
//
//	playback: maxlength = Buffer, tlength = min(2*Period, Buffer),
//	          minreq = Period, prebuf = tlength
//	capture:  maxlength = Buffer, fragsize = Period
//
// tlength is the fill level the server aims for, so playback latency in the
// server is two periods; the server asks for data in requests of at least
// one period. ADJUST_LATENCY makes the device buffer follow these values.
func openPulse(cb pulseCallbacks, kind Kind, cfg Config, env pulseEnv, timeout time.Duration) (*pulseStream, error) {
	f := cfg.Format
	if f.Channels > pulseMaxChannels {
		return nil, fmt.Errorf("%w: PulseAudio supports at most %d channels", ErrUnsupportedFormat, pulseMaxChannels)
	}
	var devName string
	if cfg.DeviceID != "" {
		b, err := hex.DecodeString(cfg.DeviceID)
		if err != nil {
			return nil, fmt.Errorf("%w: id %q", ErrNoDevice, cfg.DeviceID)
		}
		devName = string(b)
	}
	frame := 2 * f.Channels
	period := f.frames(cfg.Period) * frame
	buffer := f.frames(cfg.Buffer) * frame
	attr := pulseStreamAttr{maxLength: uint32(buffer), tLength: uint32(min(2*period, buffer)), minReq: uint32(period), fragSize: uint32(period)}
	attr.preBuf = attr.tLength

	c, err := dialPulse(env, timeout)
	if err != nil {
		return nil, err
	}
	p := &pulseStream{cb: cb, c: c, capture: kind == Capture, ch: f.Channels, frame: frame, limit: buffer, wtimeout: timeout}
	if cfg.StallTimeout > 0 {
		p.wtimeout = min(timeout, cfg.StallTimeout)
	}
	version := c.version.Load()
	spec := pulseSpec{pulseSampleS16LE, byte(f.Channels), uint32(f.SampleRate)}
	op := uint32(pulseOpCreatePlay)
	if p.capture {
		op = pulseOpCreateRecord
	}
	err = c.call(op, func(w *tagWriter) {
		p.writeCreate(w, version, spec, devName, attr)
	}, func(r *tagReader) error {
		var created pulseCreated
		if p.capture {
			created = r.createdRecord(version)
		} else {
			created = r.createdPlayback()
		}
		if r.err != nil {
			return r.err
		}
		p.mu.Lock()
		p.index = created.index
		p.owed = min(int(created.missing), p.limit)
		p.mu.Unlock()
		return c.addStream(created.index, p)
	})
	if err != nil {
		c.close()
		return nil, pulseOpenErr(err)
	}
	p.mu.Lock()
	p.fillLocked()
	p.mu.Unlock()
	return p, nil
}

func pulseOpenErr(err error) error {
	var pe pulseError
	switch {
	case errors.As(err, &pe) && pe == pulseErrNoEntity:
		return fmt.Errorf("%w: %v", ErrNoDevice, pe)
	case errors.As(err, &pe) && (pe == pulseErrInvalid || pe == pulseErrNotSupported):
		return fmt.Errorf("%w: %v", ErrUnsupportedFormat, pe)
	case errors.Is(err, errPulseNoAnswer):
		return fmt.Errorf("%w: %w (a PipeWire server needs a running session manager to create streams)", ErrBackend, err)
	}
	return pulseWrap(err)
}

func (p *pulseStream) writeCreate(w *tagWriter, version uint32, spec pulseSpec, devName string, a pulseStreamAttr) {
	w.sampleSpec(spec)
	w.channelMap(pulseDefaultMap(p.ch))
	w.u32(pulseNoIndex)
	w.str(devName)
	w.u32(a.maxLength)
	w.bool(false) // corked
	if p.capture {
		w.u32(a.fragSize)
	} else {
		w.u32(a.tLength)
		w.u32(a.preBuf)
		w.u32(a.minReq)
		w.u32(0) // sync id
		w.cvolume(p.ch, 0x10000)
	}
	// An explicit device pins the stream; the default device is followed.
	for _, b := range []bool{false, false, false, false, false, devName != "", false} {
		w.bool(b) // no remap, no remix, fix format, fix rate, fix channels, no move, variable rate
	}
	if p.capture {
		w.bool(false) // peak detect
		w.bool(true)  // adjust latency
		w.proplist("media.name", "kolo-audio capture")
		w.u32(pulseNoIndex) // direct on input
		w.bool(false)       // early requests
		w.bool(false)       // dont inhibit auto suspend
		w.bool(false)       // fail on suspend
		if version >= 22 {
			w.u8(0) // formats
			w.cvolume(p.ch, 0x10000)
			for range 5 {
				w.bool(false) // muted, volume set, muted set, relative volume, passthrough
			}
		}
		return
	}
	w.bool(false) // muted
	w.bool(true)  // adjust latency
	w.proplist("media.name", "kolo-audio playback")
	w.bool(false) // volume set
	w.bool(false) // early requests
	w.bool(false) // muted set
	w.bool(false) // dont inhibit auto suspend
	w.bool(false) // fail on suspend
	w.bool(false) // relative volume
	w.bool(false) // passthrough
	w.u8(0)       // formats
}

// pulseData receives captured bytes.
func (p *pulseStream) pulseData(b []byte) {
	if !p.capture {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		return
	}
	if len(p.carry) > 0 {
		p.carry = append(p.carry, b...)
		b = p.carry
	}
	n := len(b) / p.frame * p.frame
	if cap(p.pcm) < n/2 {
		p.pcm = make([]int16, n/2)
	}
	pcm := p.pcm[:n/2]
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	p.carry = append(p.carry[:0], b[n:]...)
	if n > 0 {
		p.cb.onCapture(pcm)
	}
}

func (p *pulseStream) pulseNotify(op uint32, r *tagReader) {
	switch op {
	case pulseOpRequest:
		n := r.u32()
		if r.err != nil || p.capture {
			return
		}
		p.mu.Lock()
		p.owed = int(min(uint64(p.owed)+uint64(n), uint64(p.limit)))
		p.fillLocked()
		p.mu.Unlock()
	case pulseOpPlayKilled, pulseOpRecordKilled:
		p.pulseLost()
	}
	// Suspended streams resume by themselves, so a suspend is not a loss; the
	// stall watchdog covers one that never ends. A move to another device is
	// handled by the server, which converts the format.
}

func (p *pulseStream) pulseLost() {
	p.mu.Lock()
	p.loseLocked()
	p.mu.Unlock()
}

func (p *pulseStream) loseLocked() {
	if p.dead {
		return
	}
	p.dead = true
	if !p.closing.Load() {
		p.cb.onStopped()
	}
	p.c.nc.Close()
}

// fillLocked answers the server's requests with frames from onPlayback.
func (p *pulseStream) fillLocked() {
	for !p.dead && p.owed >= p.frame {
		n := p.owed / p.frame * p.frame
		samples := n / 2
		if cap(p.pcm) < samples {
			p.pcm = make([]int16, samples)
		}
		pcm := p.pcm[:samples]
		p.cb.onPlayback(pcm)
		if cap(p.out) < pulseDescLen+n {
			p.out = make([]byte, pulseDescLen+n)
		}
		out := p.out[:pulseDescLen+n]
		putFrameHeader(out, n, p.index)
		for i, v := range pcm {
			binary.LittleEndian.PutUint16(out[pulseDescLen+2*i:], uint16(v))
		}
		if err := p.c.writeWithin(out, p.wtimeout); err != nil {
			p.loseLocked()
			return
		}
		p.owed -= n
	}
}

// close ends the stream. No callback runs once it returns.
func (p *pulseStream) close() {
	p.closing.Store(true)
	p.c.close()
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()
}

// stop makes the stream behave as if its device vanished.
func (p *pulseStream) stop() { p.pulseLost() }
