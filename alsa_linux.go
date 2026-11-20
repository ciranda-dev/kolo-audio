//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// alsaSystem is the kernel interface of the ALSA backend; tests replace it.
var alsaSystem alsaSys = alsaKernel{}

const alsaAbsent = "no ALSA devices in " + alsaDir

// alsaFill is the playback fill level the backend keeps in the device
// buffer, in periods: the three periods of a common ALSA buffer.
const alsaFill = 3

// alsaResumeRetry is the wait between attempts to resume a suspended
// device, as in alsa-lib's recovery example.
var alsaResumeRetry = time.Second

// alsaNode is a PCM device node, /dev/snd/pcmC<card>D<dev><p|c>.
type alsaNode struct {
	card, dev int
	capture   bool
}

func (n alsaNode) file() string {
	d := "p"
	if n.capture {
		d = "c"
	}
	return "pcmC" + strconv.Itoa(n.card) + "D" + strconv.Itoa(n.dev) + d
}

// alsaParseNode parses a PCM node name. Anything else, including numbers
// with leading zeros or signs, is not a PCM node.
func alsaParseNode(name string) (alsaNode, bool) {
	var n alsaNode
	rest, ok := strings.CutPrefix(name, "pcmC")
	if !ok {
		return n, false
	}
	if n.card, rest, ok = alsaNumber(rest); !ok {
		return n, false
	}
	if rest, ok = strings.CutPrefix(rest, "D"); !ok {
		return n, false
	}
	if n.dev, rest, ok = alsaNumber(rest); !ok {
		return n, false
	}
	switch rest {
	case "p":
	case "c":
		n.capture = true
	default:
		return n, false
	}
	return n, true
}

// alsaNumber reads a decimal number of at most 4 digits at the start of s.
func alsaNumber(s string) (int, string, bool) {
	i := 0
	for i < len(s) && i < 5 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i > 4 || i > 1 && s[0] == '0' {
		return 0, s, false
	}
	n, _ := strconv.Atoi(s[:i])
	return n, s[i:], true
}

// alsaValidCardID reports whether id can be a card ID: the kernel keeps
// only letters, digits, underscores and hyphens, at most 15 of them.
func alsaValidCardID(id string) bool {
	if id == "" || len(id) > 15 {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// alsaID is the device ID: the hex of hw:CARD=<card id>,DEV=<device>.
func alsaID(cardID string, dev int) string {
	return hex.EncodeToString([]byte("hw:CARD=" + cardID + ",DEV=" + strconv.Itoa(dev)))
}

func alsaParseID(id string) (string, int, bool) {
	b, err := hex.DecodeString(id)
	if err != nil {
		return "", 0, false
	}
	rest, ok := strings.CutPrefix(string(b), "hw:CARD=")
	if !ok {
		return "", 0, false
	}
	card, dev, ok := strings.Cut(rest, ",DEV=")
	if !ok || !alsaValidCardID(card) {
		return "", 0, false
	}
	n, tail, ok := alsaNumber(dev)
	if !ok || tail != "" {
		return "", 0, false
	}
	return card, n, true
}

// alsaPCM is a listed PCM stream.
type alsaPCM struct {
	alsaNode
	cardID string
	name   string
}

// alsaScan lists the PCM streams of one kind, by card and device number.
// Card IDs and names come from the card's control device, or the ID from
// sysfs when there is no control device. A system without PCM nodes is
// absent.
func alsaScan(sys alsaSys, kind Kind) ([]alsaPCM, error) {
	names, err := sys.names()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &backendAbsent{alsaAbsent}
	}
	if err != nil {
		return nil, alsaErr("list "+alsaDir, err)
	}
	var nodes []alsaNode
	found := false
	for _, name := range names {
		if n, ok := alsaParseNode(name); ok {
			found = true
			if n.capture == (kind == Capture) {
				nodes = append(nodes, n)
			}
		}
	}
	if !found {
		return nil, &backendAbsent{alsaAbsent}
	}
	slices.SortFunc(nodes, func(a, b alsaNode) int {
		if a.card != b.card {
			return a.card - b.card
		}
		return a.dev - b.dev
	})
	var pcms []alsaPCM
	seen := map[string]int{}
	for i := 0; i < len(nodes); {
		j := i
		for j < len(nodes) && nodes[j].card == nodes[i].card {
			j++
		}
		found, err := alsaScanCard(sys, nodes[i:j])
		if err != nil {
			return nil, err
		}
		for _, p := range found {
			if c, ok := seen[p.cardID]; ok && c != p.card {
				continue
			}
			seen[p.cardID] = p.card
			pcms = append(pcms, p)
		}
		i = j
	}
	return pcms, nil
}

func alsaScanCard(sys alsaSys, nodes []alsaNode) ([]alsaPCM, error) {
	card := nodes[0].card
	ctlName := "controlC" + strconv.Itoa(card)
	var info alsaCard
	ctl, err := sys.open(ctlName, false)
	switch {
	case err == nil:
		defer ctl.close()
		if info, err = alsaCardInfo(ctl); err != nil {
			return nil, alsaErr("card info "+ctlName, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		if info.id, err = sys.sysfsID(card); err != nil {
			return nil, nil
		}
	default:
		return nil, alsaErr("open "+ctlName, err)
	}
	if !alsaValidCardID(info.id) {
		return nil, nil
	}
	if info.name == "" {
		info.name = info.id
	}
	var pcms []alsaPCM
	for _, n := range nodes {
		p := alsaPCM{alsaNode: n, cardID: info.id, name: info.name + ", device " + strconv.Itoa(n.dev)}
		if ctl != nil {
			name, err := alsaPCMName(ctl, n.dev, n.capture)
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENXIO) {
				continue
			}
			if err == nil && name != "" {
				p.name = info.name + ", " + name
			}
		}
		pcms = append(pcms, p)
	}
	return pcms, nil
}

func alsaList(kind Kind) ([]Device, error) { return alsaListSys(alsaSystem, kind) }

// alsaListSys lists devices. The default is the first device of the lowest
// numbered card, which is ALSA's default card and device.
func alsaListSys(sys alsaSys, kind Kind) ([]Device, error) {
	pcms, err := alsaScan(sys, kind)
	if err != nil {
		return nil, err
	}
	devs := make([]Device, 0, len(pcms))
	for i, p := range pcms {
		devs = append(devs, Device{ID: alsaID(p.cardID, p.dev), Name: p.name, Kind: kind, Default: i == 0})
	}
	return devs, nil
}

func alsaOpen(s *stream, cfg Config) (hwStream, error) {
	return alsaOpenSys(alsaSystem, s, s.kind, cfg)
}

// alsaCallbacks receives what a stream delivers. A *stream implements it.
type alsaCallbacks interface {
	onCapture([]int16)
	onPlayback([]int16)
	onStopped()
	onXrun(frames uint64)
	fail(error)
}

// onXrun counts frames the device lost in an xrun: captured frames dropped,
// or silence played after the first Write.
func (s *stream) onXrun(frames uint64) {
	s.mu.Lock()
	if s.kind == Capture {
		s.stats.Overruns += frames
	} else if s.written {
		s.stats.Underruns += frames
	}
	s.mu.Unlock()
}

func alsaOpenSys(sys alsaSys, cb alsaCallbacks, kind Kind, cfg Config) (*alsaStream, error) {
	pcms, err := alsaScan(sys, kind)
	if err != nil {
		return nil, err
	}
	var pick *alsaPCM
	if cfg.DeviceID == "" {
		if len(pcms) == 0 {
			return nil, fmt.Errorf("%w: no ALSA %v device", ErrNoDevice, kind)
		}
		pick = &pcms[0]
	} else if card, dev, ok := alsaParseID(cfg.DeviceID); ok {
		for i := range pcms {
			if pcms[i].cardID == card && pcms[i].dev == dev {
				pick = &pcms[i]
			}
		}
	}
	if pick == nil {
		return nil, fmt.Errorf("%w: id %q", ErrNoDevice, cfg.DeviceID)
	}
	name := pick.file()
	d, err := sys.open(name, true)
	if err != nil {
		return nil, alsaErr("open "+alsaDir+"/"+name, err)
	}
	a, err := startALSA(d, cb, kind == Capture, cfg, alsaFormats)
	if err != nil {
		d.close()
		return nil, err
	}
	return a, nil
}

// alsaStream is an open PCM. One goroutine waits on the device and makes
// every callback, so callbacks never overlap.
type alsaStream struct {
	cb      alsaCallbacks
	dev     alsaDev
	capture bool
	set     alsaSetup
	app     Format
	pipe    *alsaPipe
	target  int // playback fill level in frames
	chunk   int // frames per transfer
	buf     []byte
	pend    []byte // playback bytes the device has not taken yet
	pendBuf []byte

	quit      atomic.Bool
	halt      atomic.Bool
	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
}

// startALSA configures an open PCM and starts delivery. Capture starts at once
// with avail_min = period. Playback keeps the device buffer filled to alsaFill
// periods (or the whole buffer if smaller): avail_min = buffer - fill + period,
// start_threshold = fill. An xrun stops either; it recovers by preparing the
// device again.
func startALSA(d alsaDev, cb alsaCallbacks, capture bool, cfg Config, formats []alsaFormat) (*alsaStream, error) {
	set, err := alsaNegotiate(d, cfg.Format, cfg.Period, cfg.Buffer, formats)
	if err != nil {
		return nil, err
	}
	a := &alsaStream{cb: cb, dev: d, capture: capture, set: set, app: cfg.Format, done: make(chan struct{})}
	sw := alsaSW{stopThreshold: uint64(set.buffer), proto: alsaPCMVersion(d)}
	if sw.proto >= 0x2000c {
		sw.tstampType = 1 // monotonic, so xrun durations ignore wall clock steps
	}
	if capture {
		a.chunk = set.period
		sw.availMin, sw.startThreshold = uint64(set.period), uint64(set.buffer)
	} else {
		a.target = min(alsaFill*set.period, set.buffer)
		a.chunk = a.target
		sw.availMin = uint64(set.buffer - a.target + set.period)
		sw.startThreshold = uint64(a.target)
	}
	if err := sw.call(d); err != nil {
		return nil, alsaErr("sw_params", err)
	}
	if a.pipe, err = newALSAPipe(cb, set, cfg.Format, capture); err != nil {
		return nil, err
	}
	a.buf = make([]byte, a.chunk*set.frameBytes())
	if err := a.restart(); err != nil {
		a.pipe.close()
		return nil, alsaErr("prepare", err)
	}
	go a.run()
	return a, nil
}

// restart prepares the device and starts a capture stream. A playback
// stream starts when the fill level is reached.
func (a *alsaStream) restart() error {
	a.pend = nil
	if err := a.dev.ioctl(alsaIoctlPrepare, nil); err != nil {
		return err
	}
	if a.capture {
		return a.dev.ioctl(alsaIoctlStart, nil)
	}
	return nil
}

func (a *alsaStream) run() {
	defer close(a.done)
	idle := false
	for {
		if a.quit.Load() {
			return
		}
		if a.halt.Load() {
			a.cb.onStopped()
			return
		}
		timeout, pcm := time.Duration(-1), true
		if idle {
			// The device woke us and had nothing to move: wait a period
			// instead of polling it again at once.
			timeout, pcm = time.Duration(a.set.period)*time.Second/time.Duration(a.set.rate), false
		}
		rev, woken, err := a.dev.wait(timeout, pcm)
		if woken {
			continue
		}
		if err != nil || rev&(unix.POLLNVAL|unix.POLLHUP) != 0 {
			a.lose()
			return
		}
		moved, ok := a.service()
		if !ok {
			return
		}
		idle = !moved
	}
}

// lose reports the device as gone and ends delivery.
func (a *alsaStream) lose() {
	if !a.quit.Load() {
		a.cb.onStopped()
	}
}

// service moves the frames the device has room for or holds. It returns
// false once the stream has ended.
func (a *alsaStream) service() (moved, ok bool) {
	st, err := alsaGetStatus(a.dev)
	if err != nil {
		return false, a.ioErr(err)
	}
	switch st.state {
	case alsaStateXrun:
		return true, a.xrun(st)
	case alsaStateSuspended:
		return true, a.resume()
	case alsaStatePrepared:
		if a.capture {
			if err := a.dev.ioctl(alsaIoctlStart, nil); err != nil {
				return false, a.ioErr(err)
			}
			return true, true
		}
	case alsaStateRunning:
	default:
		a.lose()
		return false, false
	}
	avail := int(min(st.avail, uint64(a.set.buffer)))
	if a.capture {
		return a.read(avail)
	}
	return a.write(avail)
}

func (a *alsaStream) read(avail int) (moved, ok bool) {
	fb := a.set.frameBytes()
	for avail > 0 {
		n := min(avail, a.chunk)
		got, err := a.dev.transfer(alsaIoctlReadI, a.buf[:n*fb], fb)
		if errors.Is(err, unix.EAGAIN) || err == nil && got == 0 {
			break
		}
		if err != nil {
			return moved, a.ioErr(err)
		}
		moved = true
		if err := a.pipe.capture(a.buf[:got*fb]); err != nil {
			a.cb.fail(err)
			return moved, false
		}
		avail -= got
	}
	return moved, true
}

func (a *alsaStream) write(avail int) (moved, ok bool) {
	fb := a.set.frameBytes()
	n := min(a.target-(a.set.buffer-avail), avail)
	if len(a.pend) > 0 {
		w, err := a.dev.transfer(alsaIoctlWriteI, a.pend, fb)
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			return false, a.ioErr(err)
		}
		a.pend = a.pend[w*fb:]
		n -= w
		moved = w > 0
		if len(a.pend) > 0 {
			return moved, true
		}
	}
	if n <= 0 {
		return moved, true
	}
	b := a.buf[:n*fb]
	if err := a.pipe.playback(b); err != nil {
		a.cb.fail(err)
		return moved, false
	}
	w, err := a.dev.transfer(alsaIoctlWriteI, b, fb)
	if err != nil && !errors.Is(err, unix.EAGAIN) {
		return true, a.ioErr(err)
	}
	if w < n {
		a.pendBuf = append(a.pendBuf[:0], b[w*fb:]...)
		a.pend = a.pendBuf
	}
	return true, true
}

// ioErr handles an error from the device: an xrun or suspend is recovered,
// anything else, such as ENODEV for a removed card, ends the stream.
func (a *alsaStream) ioErr(err error) bool {
	switch {
	case errors.Is(err, unix.EAGAIN):
		return true
	case errors.Is(err, unix.EPIPE):
		st, err := alsaGetStatus(a.dev)
		if err != nil {
			a.lose()
			return false
		}
		return a.xrun(st)
	case errors.Is(err, unix.ESTRPIPE):
		return a.resume()
	}
	a.lose()
	return false
}

// xrun counts the frames lost since the device stopped, plus a full
// capture buffer that prepare discards, and restarts the device.
func (a *alsaStream) xrun(st alsaStatus) bool {
	var lost float64
	if st.trigger > 0 {
		lost = max(st.now-st.trigger, 0).Seconds() * float64(a.app.SampleRate)
	}
	if a.capture {
		lost += float64(min(st.avail, uint64(a.set.buffer))) * float64(a.app.SampleRate) / float64(a.set.rate)
	}
	a.cb.onXrun(uint64(math.Round(lost)))
	if err := a.restart(); err != nil {
		a.lose()
		return false
	}
	return true
}

// resume waits for a suspended device to come back and restarts it if it
// cannot resume where it stopped.
func (a *alsaStream) resume() bool {
	for {
		err := a.dev.ioctl(alsaIoctlResume, nil)
		if err == nil {
			return true
		}
		if !errors.Is(err, unix.EAGAIN) {
			break
		}
		if _, woken, err := a.dev.wait(alsaResumeRetry, false); woken || err != nil {
			return true
		}
	}
	if err := a.restart(); err != nil {
		a.lose()
		return false
	}
	return true
}

// close stops delivery and releases the device. No callback runs after it
// returns.
func (a *alsaStream) close() {
	a.closeOnce.Do(func() {
		a.quit.Store(true)
		a.dev.wake()
		<-a.done
		a.closed.Store(true)
		a.dev.ioctl(alsaIoctlDrop, nil)
		a.dev.close()
		a.pipe.close()
	})
}

// stop ends delivery as a vanished device would.
func (a *alsaStream) stop() {
	if a.closed.Load() {
		return
	}
	a.halt.Store(true)
	a.dev.wake()
	<-a.done
}

// alsaPipe converts between the device's format, channels and rate and the
// stream's s16 frames.
type alsaPipe struct {
	cb               alsaCallbacks
	f                alsaFormat
	devCh, appCh     int
	devRate, appRate int
	res              *Resampler
	dev, app, rs     []int16
	pendBuf, pend    []int16
}

func newALSAPipe(cb alsaCallbacks, set alsaSetup, app Format, capture bool) (*alsaPipe, error) {
	p := &alsaPipe{cb: cb, f: set.format, devCh: set.channels, appCh: app.Channels, devRate: set.rate, appRate: app.SampleRate}
	if p.devRate != p.appRate {
		from, to := p.appRate, p.devRate
		if capture {
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

func (p *alsaPipe) close() {
	if p.res != nil {
		p.res.Close()
	}
}

func alsaGrow(b *[]int16, n int) []int16 {
	if cap(*b) < n {
		*b = make([]int16, n)
	}
	*b = (*b)[:n]
	return *b
}

// capture converts device bytes and delivers them.
func (p *alsaPipe) capture(b []byte) error {
	dev := alsaGrow(&p.dev, len(b)/p.f.bytes)
	alsaDecode(p.f, b, dev)
	src := dev
	if p.devCh != p.appCh {
		src = alsaGrow(&p.app, len(dev)/p.devCh*p.appCh)
		alsaMap(src, dev, p.devCh, p.appCh)
	}
	if p.res == nil {
		p.cb.onCapture(src)
		return nil
	}
	out := alsaGrow(&p.rs, p.res.OutputLen(len(src))+p.appCh)
	for len(src) > 0 {
		w, c, err := p.res.Process(out, src)
		if err != nil {
			return err
		}
		if w > 0 {
			p.cb.onCapture(out[:w])
		}
		src = src[c:]
		if w == 0 && c == 0 {
			break
		}
	}
	return nil
}

// playback fills b with device frames taken from the stream.
func (p *alsaPipe) playback(b []byte) error {
	frames := len(b) / (p.f.bytes * p.devCh)
	app := alsaGrow(&p.app, frames*p.appCh)
	if p.res == nil {
		p.cb.onPlayback(app)
	} else if err := p.pull(app); err != nil {
		return err
	}
	dev := app
	if p.devCh != p.appCh {
		dev = alsaGrow(&p.dev, frames*p.devCh)
		alsaMap(dev, app, p.appCh, p.devCh)
	}
	alsaEncode(p.f, dev, b)
	return nil
}

// pull fills dst with stream frames resampled to the device rate.
func (p *alsaPipe) pull(dst []int16) error {
	ch := p.appCh
	w := 0
	for w < len(dst) {
		if len(p.pend) == 0 {
			frames := int((int64((len(dst)-w)/ch)*int64(p.appRate) + int64(p.devRate) - 1) / int64(p.devRate))
			p.pend = alsaGrow(&p.pendBuf, max(frames, 1)*ch)
			p.cb.onPlayback(p.pend)
		}
		o, c, err := p.res.Process(dst[w:], p.pend)
		if err != nil {
			clear(dst[w:])
			return err
		}
		w += o
		p.pend = p.pend[c:]
		if o == 0 && c == 0 {
			break
		}
	}
	clear(dst[w:])
	return nil
}

// alsaMap converts frames between channel counts: copy, mono duplicated,
// all channels averaged to mono, and otherwise the first channels kept
// with any extra ones silent.
func alsaMap(dst, src []int16, from, to int) {
	if from == to || from == 1 || to == 1 {
		ConvertChannels(dst, src, from, to)
		return
	}
	n := min(from, to)
	for i, j := 0, 0; i+from <= len(src) && j+to <= len(dst); i, j = i+from, j+to {
		copy(dst[j:j+n], src[i:i+n])
		clear(dst[j+n : j+to])
	}
}

func alsaRound16(v, shift int64) int16 {
	return int16(max(math.MinInt16, min(math.MaxInt16, (v+1<<(shift-1))>>shift)))
}

// alsaDecode converts device samples to s16, rounding to nearest.
func alsaDecode(f alsaFormat, src []byte, dst []int16) {
	le := binary.LittleEndian
	for i := range dst {
		b := src[i*f.bytes:]
		switch f.code {
		case alsaS16.code:
			dst[i] = int16(le.Uint16(b))
		case alsaS24_3.code:
			dst[i] = alsaRound16(int64(int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24)>>8), 8)
		case alsaS24.code:
			dst[i] = alsaRound16(int64(int32(le.Uint32(b)<<8)>>8), 8)
		case alsaS32.code:
			dst[i] = alsaRound16(int64(int32(le.Uint32(b))), 16)
		case alsaFloat.code:
			v := float64(math.Float32frombits(le.Uint32(b))) * 32768
			if math.IsNaN(v) {
				v = 0
			}
			dst[i] = int16(math.Round(max(math.MinInt16, min(math.MaxInt16, v))))
		}
	}
}

// alsaEncode converts s16 samples to the device format.
func alsaEncode(f alsaFormat, src []int16, dst []byte) {
	le := binary.LittleEndian
	for i, v := range src {
		b := dst[i*f.bytes:]
		switch f.code {
		case alsaS16.code:
			le.PutUint16(b, uint16(v))
		case alsaS24_3.code:
			u := uint32(int32(v) << 8)
			b[0], b[1], b[2] = byte(u), byte(u>>8), byte(u>>16)
		case alsaS24.code:
			le.PutUint32(b, uint32(int32(v)<<8))
		case alsaS32.code:
			le.PutUint32(b, uint32(int32(v)<<16))
		case alsaFloat.code:
			le.PutUint32(b, math.Float32bits(float32(v)/32768))
		}
	}
}
