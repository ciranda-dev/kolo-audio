//go:build darwin && (amd64 || arm64)

package koloaudio

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"sync"
	"time"
)

// unitFunc is the data callback of an audio unit. For capture buf holds the
// frames the device recorded; for playback buf is filled with the frames to
// play. Frames are interleaved s16 in the unit's format.
type unitFunc func(buf []int16)

// audioUnit is one AUHAL unit. stop returns only when its callback is idle.
type audioUnit interface {
	// format is the client format: the device's sample rate and channel count.
	format() (rate, channels int)
	start() error
	stop()
	close()
}

// coreAudio is the part of CoreAudio the backend uses. The real one calls the
// system frameworks; tests replace it.
type coreAudio interface {
	devices() ([]uint32, error)
	// defaultDevice returns 0 when there is none.
	defaultDevice(kind Kind) uint32
	uid(dev uint32) (string, error)
	name(dev uint32) (string, error)
	channels(dev uint32, kind Kind) int
	alive(dev uint32) bool
	nominalRate(dev uint32) (int, error)
	// listen calls fn on a foreign thread whenever property sel of obj
	// changes. remove is safe to call more than once. A call that began before
	// remove may still run after it returns, so fn must never block and must
	// tolerate that.
	listen(obj, sel uint32, fn func()) (remove func(), err error)
	newUnit(kind Kind, dev uint32, period time.Duration, fn unitFunc) (audioUnit, error)
}

const caSystemObject = 1

var (
	caSelDefaultIn  = caFourCC("dIn ")
	caSelDefaultOut = caFourCC("dOut")
	caSelAlive      = caFourCC("livn")
	caSelRate       = caFourCC("nsrt")
)

func caFourCC(s string) uint32 {
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

func caDefaultSelector(kind Kind) uint32 {
	if kind == Capture {
		return caSelDefaultIn
	}
	return caSelDefaultOut
}

// caStatusError maps an OSStatus to a package error. The permission and
// format codes are the ones CoreAudio documents for those failures.
func caStatusError(call string, st int32) error {
	desc := fmt.Sprintf("coreaudio %s: status %d", call, st)
	if u := uint32(st); u > 0x20202020 && u < 0x7f7f7f7f && isFourCC(u) {
		desc += fmt.Sprintf(" (%q)", string([]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}))
	}
	switch st {
	case int32(caFourCC("!hog")), -10847: // kAudioDevicePermissionsError, kAudioUnitErr_Unauthorized
		return fmt.Errorf("%w: %w: %s", ErrBackend, fs.ErrPermission, desc)
	case int32(caFourCC("!dat")), -10868: // kAudioDeviceUnsupportedFormatError, kAudioUnitErr_FormatNotSupported
		return fmt.Errorf("%w: %s", ErrUnsupportedFormat, desc)
	case int32(caFourCC("!dev")), int32(caFourCC("!obj")): // kAudioHardwareBadDeviceError, kAudioHardwareBadObjectError
		return fmt.Errorf("%w: %s", ErrNoDevice, desc)
	}
	return fmt.Errorf("%w: %s", ErrBackend, desc)
}

func isFourCC(u uint32) bool {
	for _, b := range []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)} {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

func caList(api coreAudio, kind Kind) ([]Device, error) {
	ids, err := api.devices()
	if err != nil {
		return nil, err
	}
	def := api.defaultDevice(kind)
	devs := []Device{}
	for _, id := range ids {
		if api.channels(id, kind) == 0 {
			continue
		}
		uid, err := api.uid(id)
		if err != nil || uid == "" {
			continue
		}
		name, err := api.name(id)
		if err != nil || name == "" {
			name = uid
		}
		devs = append(devs, Device{ID: hex.EncodeToString([]byte(uid)), Name: name, Kind: kind, Default: id == def})
	}
	return devs, nil
}

func caResolve(api coreAudio, kind Kind, id string) (uint32, error) {
	uid, err := hex.DecodeString(id)
	if err != nil {
		return 0, fmt.Errorf("%w: id %q", ErrNoDevice, id)
	}
	ids, err := api.devices()
	if err != nil {
		return 0, err
	}
	for _, d := range ids {
		if api.channels(d, kind) == 0 {
			continue
		}
		if u, err := api.uid(d); err == nil && u == string(uid) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("%w: id %q", ErrNoDevice, id)
}

// caPipe converts between the device format and the stream format.
type caPipe struct {
	s       *stream
	devRate int
	devCh   int
	app     Format
	res     *Resampler
	chBuf   []int16
	rsBuf   []int16
	pendBuf []int16
	pend    []int16
}

func newCAPipe(s *stream, kind Kind, devRate, devCh int, app Format) (*caPipe, error) {
	if devCh != app.Channels && devCh != 1 && app.Channels != 1 {
		return nil, fmt.Errorf("%w: %d device channels to %d", ErrUnsupportedFormat, devCh, app.Channels)
	}
	p := &caPipe{s: s, devRate: devRate, devCh: devCh, app: app}
	if devRate != app.SampleRate {
		from, to := devRate, app.SampleRate
		if kind == Playback {
			from, to = to, from
		}
		res, err := NewResampler(app.Channels, from, to)
		if err != nil {
			return nil, err
		}
		p.res = res
	}
	return p, nil
}

func (p *caPipe) close() {
	if p.res != nil {
		p.res.Close()
	}
}

func caGrow(b *[]int16, n int) []int16 {
	if cap(*b) < n {
		*b = make([]int16, n)
	}
	*b = (*b)[:n]
	return *b
}

func (p *caPipe) fail(err error) {
	p.s.fail(fmt.Errorf("%w: %w", ErrBackend, err))
}

// capture takes frames recorded by the device and delivers them to the stream.
func (p *caPipe) capture(dev []int16) {
	src := dev
	if p.devCh != p.app.Channels {
		dst := caGrow(&p.chBuf, len(dev)/p.devCh*p.app.Channels)
		if _, err := ConvertChannels(dst, dev, p.devCh, p.app.Channels); err != nil {
			p.fail(err)
			return
		}
		src = dst
	}
	if p.res == nil {
		p.s.onCapture(src)
		return
	}
	out := caGrow(&p.rsBuf, p.res.OutputLen(len(src))+p.app.Channels)
	for len(src) > 0 {
		w, c, err := p.res.Process(out, src)
		if err != nil {
			p.fail(err)
			return
		}
		if w > 0 {
			p.s.onCapture(out[:w])
		}
		src = src[c:]
		if w == 0 && c == 0 {
			return
		}
	}
}

// playback fills dev, the frames the device wants, from the stream.
func (p *caPipe) playback(dev []int16) {
	if p.res == nil && p.devCh == p.app.Channels {
		p.s.onPlayback(dev)
		return
	}
	dst := dev
	if p.devCh != p.app.Channels {
		dst = caGrow(&p.chBuf, len(dev)/p.devCh*p.app.Channels)
	}
	if p.res == nil {
		p.s.onPlayback(dst)
	} else {
		p.pull(dst)
	}
	if p.devCh != p.app.Channels {
		if _, err := ConvertChannels(dev, dst, p.app.Channels, p.devCh); err != nil {
			p.fail(err)
			clear(dev)
		}
	}
}

// pull fills dst with stream frames resampled to the device rate.
func (p *caPipe) pull(dst []int16) {
	ch := p.app.Channels
	w := 0
	for w < len(dst) {
		if len(p.pend) == 0 {
			frames := int((int64((len(dst)-w)/ch)*int64(p.app.SampleRate) + int64(p.devRate) - 1) / int64(p.devRate))
			p.pend = caGrow(&p.pendBuf, max(frames, 1)*ch)
			p.s.onPlayback(p.pend)
		}
		o, c, err := p.res.Process(dst[w:], p.pend)
		if err != nil {
			p.fail(err)
			break
		}
		w += o
		p.pend = p.pend[c:]
		if o == 0 && c == 0 {
			break
		}
	}
	clear(dst[w:])
}

// caBinding is a unit bound to one device with its listeners and pipe.
type caBinding struct {
	dev      uint32
	unit     audioUnit
	pipe     *caPipe
	rate     int
	channels int
	nominal  int // the device's nominal rate when bound, to see it change
	removes  []func()
}

// caStream is an open CoreAudio stream: it follows the default device when
// the stream was opened without an ID, and fails when the device dies.
type caStream struct {
	s      *stream
	api    coreAudio
	kind   Kind
	cfg    Config
	follow bool

	// dmu serializes every callback into s, and guards the fields below.
	dmu     sync.Mutex
	live    *caBinding
	closed  bool
	stopped bool

	// ctl serializes rebinding and close; cur is guarded by it.
	ctl       sync.Mutex
	cur       *caBinding
	sys       []func()
	events    chan struct{}
	closeOnce sync.Once
	quit      chan struct{}
	done      chan struct{}
}

func openCA(api coreAudio, s *stream, cfg Config) (hwStream, error) {
	c := &caStream{
		s: s, api: api, kind: s.kind, cfg: cfg, follow: cfg.DeviceID == "",
		events: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{}),
	}
	var dev uint32
	if c.follow {
		if dev = api.defaultDevice(c.kind); dev == 0 {
			return nil, fmt.Errorf("%w: no default %s device", ErrNoDevice, c.kind)
		}
		remove, err := api.listen(caSystemObject, caDefaultSelector(c.kind), c.poke)
		if err != nil {
			return nil, err
		}
		c.sys = append(c.sys, remove)
	} else {
		var err error
		if dev, err = caResolve(api, c.kind, cfg.DeviceID); err != nil {
			return nil, err
		}
	}
	b, err := c.bind(dev)
	if err != nil {
		c.removeSys()
		return nil, err
	}
	c.cur = b
	go c.watch()
	c.poke()
	return c, nil
}

func (c *caStream) poke() {
	select {
	case c.events <- struct{}{}:
	default:
	}
}

func (c *caStream) removeSys() {
	for _, r := range c.sys {
		r()
	}
	c.sys = nil
}

func (c *caStream) deliver(b *caBinding) unitFunc {
	return func(buf []int16) {
		c.dmu.Lock()
		defer c.dmu.Unlock()
		if c.live != b {
			if c.kind == Playback {
				clear(buf)
			}
			return
		}
		if c.kind == Capture {
			b.pipe.capture(buf)
		} else {
			b.pipe.playback(buf)
		}
	}
}

// bind starts a unit on dev. Its callbacks reach s until unbind.
func (c *caStream) bind(dev uint32) (*caBinding, error) {
	b := &caBinding{dev: dev}
	b.nominal, _ = c.api.nominalRate(dev)
	unit, err := c.api.newUnit(c.kind, dev, c.cfg.Period, c.deliver(b))
	if err != nil {
		return nil, err
	}
	b.unit = unit
	b.rate, b.channels = unit.format()
	if b.pipe, err = newCAPipe(c.s, c.kind, b.rate, b.channels, c.cfg.Format); err != nil {
		unit.close()
		return nil, err
	}
	for _, sel := range []uint32{caSelAlive, caSelRate} {
		remove, err := c.api.listen(dev, sel, c.poke)
		if err != nil {
			c.teardown(b)
			return nil, err
		}
		b.removes = append(b.removes, remove)
	}
	c.dmu.Lock()
	if c.closed || c.stopped {
		c.dmu.Unlock()
		c.teardown(b)
		return nil, ErrClosed
	}
	c.live = b
	c.dmu.Unlock()
	if err := unit.start(); err != nil {
		c.unbind(b)
		return nil, err
	}
	return b, nil
}

// unbind ends delivery from b and releases it. No callback of b runs after
// it returns.
func (c *caStream) unbind(b *caBinding) {
	c.dmu.Lock()
	if c.live == b {
		c.live = nil
	}
	c.dmu.Unlock()
	c.teardown(b)
}

func (c *caStream) teardown(b *caBinding) {
	for _, r := range b.removes {
		r()
	}
	b.unit.stop()
	b.unit.close()
	b.pipe.close()
}

func (c *caStream) watch() {
	defer close(c.done)
	for {
		select {
		case <-c.quit:
			return
		case <-c.events:
			c.recheck()
		}
	}
}

// recheck compares the state of the world with the binding: it moves to a
// new default device, rebuilds after a sample rate change and reports a dead
// device.
func (c *caStream) recheck() {
	c.ctl.Lock()
	defer c.ctl.Unlock()
	b := c.cur
	if b == nil || c.ended() {
		return
	}
	target := b.dev
	if c.follow {
		target = c.api.defaultDevice(c.kind)
	}
	if target == 0 || !c.api.alive(target) {
		c.lost()
		return
	}
	if rate, err := c.api.nominalRate(target); target == b.dev && (err != nil || rate == b.nominal) {
		return
	}
	c.unbind(b)
	c.cur = nil
	nb, err := c.bind(target)
	if err != nil {
		c.lost()
		return
	}
	c.cur = nb
}

// lost ends delivery and reports the device as gone. ctl is held.
func (c *caStream) lost() {
	if b := c.cur; b != nil {
		c.unbind(b)
		c.cur = nil
	}
	c.notifyStopped()
}

// ended reports whether the stream is closed or has reported its device lost.
func (c *caStream) ended() bool {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	return c.closed || c.stopped
}

func (c *caStream) notifyStopped() {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	c.live = nil
	c.stopped = true
	if !c.closed {
		c.s.onStopped()
	}
}

func (c *caStream) stop() { c.notifyStopped() }

func (c *caStream) close() { c.closeOnce.Do(c.shutdown) }

func (c *caStream) shutdown() {
	c.dmu.Lock()
	c.closed = true
	c.live = nil
	c.dmu.Unlock()
	close(c.quit)
	<-c.done
	c.ctl.Lock()
	if b := c.cur; b != nil {
		c.unbind(b)
		c.cur = nil
	}
	c.ctl.Unlock()
	c.removeSys()
}
