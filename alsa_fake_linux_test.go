//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The fake kernel below reimplements the configuration space of the ALSA
// PCM core (sound/core/pcm_native.c and pcm_lib.c): interval arithmetic,
// the core's rules between parameters, the hardware constraints a driver
// declares, refinement to a fixpoint and the choice HW_PARAMS makes. The
// real-kernel test compares its answers with the kernel's.

func fakeDiv32(a, b uint32) (uint32, uint32) {
	if b == 0 {
		return math.MaxUint32, 0
	}
	return a / b, a % b
}

func fakeMulSat(a, b uint32) uint32 {
	if a == 0 {
		return 0
	}
	if math.MaxUint32/a < b {
		return math.MaxUint32
	}
	return a * b
}

func fakeMulDiv32(a, b, c uint32) (uint32, uint32) {
	if c == 0 {
		return math.MaxUint32, 0
	}
	n := uint64(a) * uint64(b)
	q, r := n/uint64(c), uint32(n%uint64(c))
	if q >= math.MaxUint32 {
		return math.MaxUint32, 0
	}
	return uint32(q), r
}

func fakeCheckEmpty(i alsaInterval) bool {
	return i.min > i.max || i.min == i.max && (i.openMin || i.openMax)
}

// fakeRefine is snd_interval_refine.
func fakeRefine(i *alsaInterval, v alsaInterval) (bool, error) {
	if i.empty {
		return false, unix.EINVAL
	}
	changed := false
	if i.min < v.min {
		i.min, i.openMin, changed = v.min, v.openMin, true
	} else if i.min == v.min && !i.openMin && v.openMin {
		i.openMin, changed = true, true
	}
	if i.max > v.max {
		i.max, i.openMax, changed = v.max, v.openMax, true
	} else if i.max == v.max && !i.openMax && v.openMax {
		i.openMax, changed = true, true
	}
	if !i.integer && v.integer {
		i.integer, changed = true, true
	}
	if i.integer {
		if i.openMin {
			i.min++
			i.openMin = false
		}
		if i.openMax {
			i.max--
			i.openMax = false
		}
	} else if !i.openMin && !i.openMax && i.min == i.max {
		i.integer = true
	}
	if fakeCheckEmpty(*i) {
		*i = alsaInterval{empty: true}
		return false, unix.EINVAL
	}
	return changed, nil
}

func fakeMul(a, b alsaInterval) alsaInterval {
	if a.empty || b.empty {
		return alsaInterval{empty: true}
	}
	return alsaInterval{min: fakeMulSat(a.min, b.min), openMin: a.openMin || b.openMin,
		max: fakeMulSat(a.max, b.max), openMax: a.openMax || b.openMax, integer: a.integer && b.integer}
}

func fakeDiv(a, b alsaInterval) alsaInterval {
	if a.empty || b.empty {
		return alsaInterval{empty: true}
	}
	var c alsaInterval
	var r uint32
	c.min, r = fakeDiv32(a.min, b.max)
	c.openMin = r != 0 || a.openMin || b.openMax
	if b.min > 0 {
		c.max, r = fakeDiv32(a.max, b.min)
		if r != 0 {
			c.max++
			c.openMax = true
		} else {
			c.openMax = a.openMax || b.openMin
		}
	} else {
		c.max = math.MaxUint32
	}
	return c
}

func fakeMulDivK(a, b alsaInterval, k uint32) alsaInterval {
	if a.empty || b.empty {
		return alsaInterval{empty: true}
	}
	var c alsaInterval
	var r uint32
	c.min, r = fakeMulDiv32(a.min, b.min, k)
	c.openMin = r != 0 || a.openMin || b.openMin
	c.max, r = fakeMulDiv32(a.max, b.max, k)
	if r != 0 {
		c.max++
		c.openMax = true
	} else {
		c.openMax = a.openMax || b.openMax
	}
	return c
}

func fakeMulKDiv(a alsaInterval, k uint32, b alsaInterval) alsaInterval {
	if a.empty || b.empty {
		return alsaInterval{empty: true}
	}
	var c alsaInterval
	var r uint32
	c.min, r = fakeMulDiv32(a.min, k, b.max)
	c.openMin = r != 0 || a.openMin || b.openMax
	if b.min > 0 {
		c.max, r = fakeMulDiv32(a.max, k, b.min)
		if r != 0 {
			c.max++
			c.openMax = true
		} else {
			c.openMax = a.openMax || b.openMin
		}
	} else {
		c.max = math.MaxUint32
	}
	return c
}

func fakeTest(i alsaInterval, v uint32) bool {
	return !(i.min > v || i.min == v && i.openMin || i.max < v || i.max == v && i.openMax)
}

// fakeList is snd_interval_list.
func fakeList(i *alsaInterval, list []uint32) (bool, error) {
	r := alsaInterval{min: math.MaxUint32}
	for _, v := range list {
		if fakeTest(*i, v) {
			r.min, r.max = min(r.min, v), max(r.max, v)
		}
	}
	return fakeRefine(i, r)
}

// fakeStep is snd_interval_step.
func fakeStep(i *alsaInterval, step uint32) (bool, error) {
	changed := false
	if n := i.min % step; n != 0 || i.openMin {
		i.min += step - n
		i.openMin, changed = false, true
	}
	if n := i.max % step; n != 0 || i.openMax {
		i.max -= n
		i.openMax, changed = false, true
	}
	if fakeCheckEmpty(*i) {
		i.empty = true
		return false, unix.EINVAL
	}
	return changed, nil
}

func fakeMaskRefine(m *alsaMask, v alsaMask) (bool, error) {
	old := *m
	m[0] &= v[0]
	m[1] &= v[1]
	if m[0] == 0 && m[1] == 0 {
		return false, unix.EINVAL
	}
	return *m != old, nil
}

// fakeWidth is snd_pcm_format_physical_width for the formats the fake knows.
var fakeWidth = map[uint32]uint32{0: 8, 1: 8, 2: 16, 3: 16, 4: 16, 5: 16, 6: 32, 7: 32, 10: 32, 11: 32, 14: 32, 15: 32, 16: 64, 32: 24, 33: 24, 48: 8, 49: 16, 50: 32, 51: 16, 52: 32}

// fakeHW is what a driver declares: struct snd_pcm_hardware plus the extra
// constraints some drivers add.
type fakeHW struct {
	formats                        []uint32
	nonInterleavedOnly             bool
	channelsMin, channelsMax       uint32
	rateMin, rateMax               uint32
	rates                          []uint32 // a rate list instead of a continuous range
	periodBytesMin, periodBytesMax uint32
	periodsMin, periodsMax         uint32
	bufferBytesMax                 uint32
	periodsInteger                 bool
	channelList                    []uint32
	periodStep                     uint32
}

// fakeLoopback is the loopback driver's snd_pcm_hardware
// (sound/drivers/aloop.c) with its integer period count.
func fakeLoopback() *fakeHW {
	return &fakeHW{
		formats:     []uint32{2, 3, 6, 7, 32, 33, 10, 11, 14, 15, 48, 49, 50, 51, 52},
		channelsMin: 1, channelsMax: 32, rateMin: 8000, rateMax: 768000,
		periodBytesMin: 64, periodBytesMax: 1024 * 1024, periodsMin: 1, periodsMax: 1024,
		bufferBytesMax: 2 * 1024 * 1024, periodsInteger: true,
	}
}

type fakeRule func(p *alsaHWParams) (bool, error)

type fakeConstraints struct {
	masks [alsaMasks]alsaMask
	ivs   [alsaIntervals]alsaInterval
	rules []fakeRule
}

func ivIdx(k int) int { return k - alsaParamSampleBits }

// fakeBuild sets up the constraints as snd_pcm_hw_constraints_init and
// snd_pcm_hw_constraints_complete do.
func fakeBuild(hw *fakeHW) *fakeConstraints {
	c := &fakeConstraints{}
	p := alsaAny()
	c.masks, c.ivs = p.masks, p.intervals
	for _, k := range []int{alsaParamChannels, alsaParamBufferSize, alsaParamBufferBytes, alsaParamSampleBits, alsaParamFrameBits} {
		c.ivs[ivIdx(k)].integer = true
	}
	iv := func(p *alsaHWParams, k int) *alsaInterval { return p.iv(k) }
	ref := func(v int, f func(p *alsaHWParams) alsaInterval) fakeRule {
		return func(p *alsaHWParams) (bool, error) { return fakeRefine(iv(p, v), f(p)) }
	}
	mul := func(v, a, b int) fakeRule {
		return ref(v, func(p *alsaHWParams) alsaInterval { return fakeMul(*iv(p, a), *iv(p, b)) })
	}
	div := func(v, a, b int) fakeRule {
		return ref(v, func(p *alsaHWParams) alsaInterval { return fakeDiv(*iv(p, a), *iv(p, b)) })
	}
	muldivk := func(v, a, b int, k uint32) fakeRule {
		return ref(v, func(p *alsaHWParams) alsaInterval { return fakeMulDivK(*iv(p, a), *iv(p, b), k) })
	}
	mulkdiv := func(v, a int, k uint32, b int) fakeRule {
		return ref(v, func(p *alsaHWParams) alsaInterval { return fakeMulKDiv(*iv(p, a), k, *iv(p, b)) })
	}
	c.rules = []fakeRule{
		func(p *alsaHWParams) (bool, error) { // snd_pcm_hw_rule_format
			sb := *iv(p, alsaParamSampleBits)
			m := p.masks[alsaParamFormat]
			keep := m
			for f, w := range fakeWidth {
				if m.test(f) && (w < sb.min || w > sb.max) {
					keep[f/32] &^= 1 << (f % 32)
				}
			}
			return fakeMaskRefine(&p.masks[alsaParamFormat], keep)
		},
		func(p *alsaHWParams) (bool, error) { // snd_pcm_hw_rule_sample_bits
			t := alsaInterval{min: math.MaxUint32, integer: true}
			for f, w := range fakeWidth {
				if p.masks[alsaParamFormat].test(f) {
					t.min, t.max = min(t.min, w), max(t.max, w)
				}
			}
			return fakeRefine(iv(p, alsaParamSampleBits), t)
		},
		div(alsaParamSampleBits, alsaParamFrameBits, alsaParamChannels),
		mul(alsaParamFrameBits, alsaParamSampleBits, alsaParamChannels),
		mulkdiv(alsaParamFrameBits, alsaParamPeriodBytes, 8, alsaParamPeriodSize),
		mulkdiv(alsaParamFrameBits, alsaParamBufferBytes, 8, alsaParamBufferSize),
		div(alsaParamChannels, alsaParamFrameBits, alsaParamSampleBits),
		mulkdiv(alsaParamRate, alsaParamPeriodSize, 1000000, alsaParamPeriodTime),
		mulkdiv(alsaParamRate, alsaParamBufferSize, 1000000, alsaParamBufferTime),
		div(alsaParamPeriods, alsaParamBufferSize, alsaParamPeriodSize),
		div(alsaParamPeriodSize, alsaParamBufferSize, alsaParamPeriods),
		mulkdiv(alsaParamPeriodSize, alsaParamPeriodBytes, 8, alsaParamFrameBits),
		muldivk(alsaParamPeriodSize, alsaParamPeriodTime, alsaParamRate, 1000000),
		mul(alsaParamBufferSize, alsaParamPeriodSize, alsaParamPeriods),
		mulkdiv(alsaParamBufferSize, alsaParamBufferBytes, 8, alsaParamFrameBits),
		muldivk(alsaParamBufferSize, alsaParamBufferTime, alsaParamRate, 1000000),
		muldivk(alsaParamPeriodBytes, alsaParamPeriodSize, alsaParamFrameBits, 8),
		muldivk(alsaParamBufferBytes, alsaParamBufferSize, alsaParamFrameBits, 8),
		mulkdiv(alsaParamPeriodTime, alsaParamPeriodSize, 1000000, alsaParamRate),
		mulkdiv(alsaParamBufferTime, alsaParamBufferSize, 1000000, alsaParamRate),
		func(p *alsaHWParams) (bool, error) { // snd_pcm_hw_rule_subformats: STD, and MSBITS_MAX for linear formats
			m := alsaMask{1}
			for f := range fakeWidth {
				if p.masks[alsaParamFormat].test(f) && (f < 14 || f >= 32 && f < 44) {
					m[0] |= 2
				}
			}
			return fakeMaskRefine(&p.masks[alsaParamSubformat], m)
		},
	}
	var access alsaMask
	if !hw.nonInterleavedOnly {
		access[0] |= 1<<0 | 1<<3
	}
	access[0] |= 1<<1 | 1<<4
	c.masks[alsaParamAccess] = access
	var formats alsaMask
	for _, f := range hw.formats {
		formats[f/32] |= 1 << (f % 32)
	}
	c.masks[alsaParamFormat] = formats
	minmax := func(k int, lo, hi uint32) { fakeRefine(&c.ivs[ivIdx(k)], alsaInterval{min: lo, max: hi}) }
	minmax(alsaParamChannels, hw.channelsMin, hw.channelsMax)
	minmax(alsaParamRate, hw.rateMin, hw.rateMax)
	minmax(alsaParamPeriodBytes, hw.periodBytesMin, hw.periodBytesMax)
	minmax(alsaParamPeriods, hw.periodsMin, hw.periodsMax)
	minmax(alsaParamBufferBytes, hw.periodBytesMin, hw.bufferBytesMax)
	if hw.rates != nil {
		rates := hw.rates
		c.rules = append(c.rules, func(p *alsaHWParams) (bool, error) { return fakeList(iv(p, alsaParamRate), rates) })
	}
	if hw.channelList != nil {
		list := hw.channelList
		c.rules = append(c.rules, func(p *alsaHWParams) (bool, error) { return fakeList(iv(p, alsaParamChannels), list) })
	}
	if hw.periodStep > 0 {
		step := hw.periodStep
		c.rules = append(c.rules, func(p *alsaHWParams) (bool, error) { return fakeStep(iv(p, alsaParamPeriodSize), step) })
	}
	c.ivs[ivIdx(alsaParamPeriodSize)].integer = true
	if hw.periodsInteger {
		c.ivs[ivIdx(alsaParamPeriods)].integer = true
	}
	return c
}

// refine is snd_pcm_hw_refine with every parameter requested.
func (c *fakeConstraints) refine(p *alsaHWParams) error {
	for k := range p.masks {
		if alsaMaskCount(p.masks[k]) == 0 {
			return unix.EINVAL
		}
		if _, err := fakeMaskRefine(&p.masks[k], c.masks[k]); err != nil {
			return err
		}
	}
	for k := range p.intervals {
		if _, err := fakeRefine(&p.intervals[k], c.ivs[k]); err != nil {
			return err
		}
	}
	for range 10000 {
		again := false
		for _, r := range c.rules {
			changed, err := r(p)
			if err != nil {
				return err
			}
			again = again || changed
		}
		if !again {
			p.msbits, p.rateNum, p.rateDen = 0, 0, 0
			if m := p.masks[alsaParamFormat]; alsaMaskCount(m) == 1 {
				for f, w := range fakeWidth {
					if m.test(f) {
						p.msbits = w
					}
				}
			}
			if r := *p.iv(alsaParamRate); r.single() {
				p.rateNum, p.rateDen = r.first(), 1
			}
			p.rmask = 0
			return nil
		}
	}
	panic("fake refine did not converge")
}

// choose is snd_pcm_hw_params_choose.
func (c *fakeConstraints) choose(p *alsaHWParams) error {
	for _, k := range []int{alsaParamAccess, alsaParamFormat, alsaParamSubformat, alsaParamChannels, alsaParamRate, alsaParamPeriodTime, alsaParamBufferSize, alsaParamTickTime} {
		changed := false
		if k < alsaMasks {
			m := &p.masks[k]
			for v := uint32(0); v < 64; v++ {
				if m.test(v) {
					if alsaMaskCount(*m) > 1 {
						m.single(v)
						changed = true
					}
					break
				}
			}
		} else {
			i := p.iv(k)
			if !i.single() {
				changed = true
				if k == alsaParamBufferSize {
					last := i.min
					i.min = i.max
					if i.openMax {
						i.min--
					}
					i.openMin = i.openMin && i.min <= last
				} else {
					last := i.max
					i.max = i.min
					if i.openMin {
						i.max++
					}
					i.openMax = i.openMax && i.max >= last
				}
			}
		}
		if changed {
			if err := c.refine(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// fakeALSA is a fake /dev/snd with cards, control devices and PCMs.
type fakeALSA struct {
	t       *testing.T
	mu      sync.Mutex
	nodes   []string
	listErr error
	cards   map[int]*fakeCard
	hw      map[alsaNode]*fakeHW
	openErr map[string]error
	opened  []*fakePCM
	live    int
	gen     func(frame uint64, ch int) int16
	tune    func(p *fakePCM)
}

type fakeCard struct {
	id, name  string
	noCtl     bool
	sysfsID   string
	infoErr   error
	pcmNames  map[alsaNode]string
	pcmMissed map[alsaNode]bool
}

func newFakeALSA(t *testing.T) *fakeALSA {
	f := &fakeALSA{t: t, cards: map[int]*fakeCard{}, hw: map[alsaNode]*fakeHW{}, openErr: map[string]error{}}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.live != 0 {
			t.Errorf("fake ALSA: %d devices left open", f.live)
		}
		for _, p := range f.opened {
			p.stopTicks()
		}
	})
	return f
}

// add adds a card with PCM devices; each device has both directions unless
// only is set.
func (f *fakeALSA) add(card int, id, name string, hw *fakeHW, devs ...int) *fakeCard {
	c := &fakeCard{id: id, name: name, sysfsID: id, pcmNames: map[alsaNode]string{}, pcmMissed: map[alsaNode]bool{}}
	f.cards[card] = c
	f.nodes = append(f.nodes, "controlC"+strconv.Itoa(card))
	for _, d := range devs {
		for _, capture := range []bool{false, true} {
			n := alsaNode{card, d, capture}
			f.nodes = append(f.nodes, n.file())
			f.hw[n] = hw
			c.pcmNames[n] = fmt.Sprintf("%s PCM %d", name, d)
		}
	}
	return c
}

func (f *fakeALSA) names() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string{"timer", "seq", "by-path"}, f.nodes...), nil
}

func (f *fakeALSA) sysfsID(card int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cards[card]; ok && c.sysfsID != "" {
		return c.sysfsID, nil
	}
	return "", fs.ErrNotExist
}

func (f *fakeALSA) open(name string, pcm bool) (alsaDev, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.openErr[name]; err != nil {
		return nil, &fs.PathError{Op: "open", Path: alsaDir + "/" + name, Err: err}
	}
	if !pcm {
		n, err := strconv.Atoi(strings.TrimPrefix(name, "controlC"))
		c, ok := f.cards[n]
		if err != nil || !ok || c.noCtl {
			return nil, &fs.PathError{Op: "open", Path: name, Err: unix.ENOENT}
		}
		f.live++
		return &fakeCtl{f: f, card: c}, nil
	}
	n, ok := alsaParseNode(name)
	hw := f.hw[n]
	if !ok || hw == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: unix.ENOENT}
	}
	p := &fakePCM{f: f, node: n, c: fakeBuild(hw), changed: make(chan struct{}), version: 0x2000f, gen: f.gen, errs: map[uint32]error{}}
	if p.gen == nil {
		p.gen = func(frame uint64, ch int) int16 { return int16(frame*7 + uint64(ch)) }
	}
	if f.tune != nil {
		f.tune(p)
	}
	f.opened = append(f.opened, p)
	f.live++
	return p, nil
}

func (f *fakeALSA) pcm(t *testing.T, i int) *fakePCM {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.opened) {
		t.Fatalf("fake ALSA: PCM %d not opened (%d)", i, len(f.opened))
	}
	return f.opened[i]
}

type fakeCtl struct {
	f    *fakeALSA
	card *fakeCard
	once sync.Once
}

func fakePut(b []byte, s string) { copy(b, s) }

func (c *fakeCtl) ioctl(req uint32, arg []byte) error {
	if len(arg) != int(req>>16&0x3fff) {
		return unix.EINVAL
	}
	switch req {
	case alsaIoctlCardInfo:
		if c.card.infoErr != nil {
			return c.card.infoErr
		}
		fakePut(arg[8:24], c.card.id)
		fakePut(arg[40:72], c.card.name)
		return nil
	case alsaIoctlPCMInfo:
		dev := int(binary.LittleEndian.Uint32(arg))
		capture := binary.LittleEndian.Uint32(arg[8:]) == 1
		for n, name := range c.card.pcmNames {
			if n.dev == dev && n.capture == capture {
				if c.card.pcmMissed[n] {
					return unix.ENOENT
				}
				fakePut(arg[80:160], name)
				return nil
			}
		}
		return unix.ENXIO
	}
	return unix.ENOTTY
}

func (c *fakeCtl) transfer(uint32, []byte, int) (int, error) { return 0, unix.ENOTTY }
func (c *fakeCtl) wait(time.Duration, bool) (int16, bool, error) {
	return 0, false, unix.ENOTTY
}
func (c *fakeCtl) wake() {}
func (c *fakeCtl) close() {
	c.once.Do(func() {
		c.f.mu.Lock()
		c.f.live--
		c.f.mu.Unlock()
	})
}

// fakePCM is an open PCM with a runtime: state, pointers, thresholds and a
// clock that the test advances.
type fakePCM struct {
	f    *fakeALSA
	node alsaNode
	c    *fakeConstraints

	mu       sync.Mutex
	changed  chan struct{}
	version  uint32
	gen      func(frame uint64, ch int) int16
	errs     map[uint32]error // injected ioctl errors
	pollErr  error
	pollBits int16 // injected poll events

	state, prevState    uint32
	set                 alsaSetup
	availMin, startThr  uint64
	stopThr             uint64
	tstampType, proto   uint32
	tstampMode          uint32
	appl, hwp           uint64
	clock, trigger      time.Duration
	woken, gone, closed bool
	resumeWait          int
	noResume            bool
	played              []byte
	calls               []string
	installed           bool
	waits               int
	writeCap            uint64

	tickStop chan struct{}
	tickDone chan struct{}
}

func (p *fakePCM) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *fakePCM) logLocked(s string) { p.calls = append(p.calls, s) }

func (p *fakePCM) availLocked() uint64 {
	if p.node.capture {
		return p.hwp - p.appl
	}
	return uint64(p.set.buffer) - (p.appl - p.hwp)
}

func (p *fakePCM) ioctl(req uint32, arg []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(arg) != int(req>>16&0x3fff) {
		return unix.EINVAL
	}
	if p.closed {
		return unix.EBADF
	}
	if p.gone {
		return unix.ENODEV
	}
	if err := p.errs[req]; err != nil {
		return err
	}
	switch req {
	case alsaIoctlPCMVersion:
		binary.LittleEndian.PutUint32(arg, p.version)
		return nil
	case alsaIoctlHWRefine, alsaIoctlHWParams:
		var hp alsaHWParams
		hp.decode(arg)
		if err := p.c.refine(&hp); err != nil {
			return err
		}
		if req == alsaIoctlHWParams {
			if p.state > alsaStatePrepared {
				return unix.EBADFD
			}
			if err := p.c.choose(&hp); err != nil {
				return err
			}
			f := alsaFormat{}
			for _, k := range alsaFormatList {
				if hp.masks[alsaParamFormat].test(k.code) {
					f = k
				}
			}
			set, err := alsaInstalled(hp, f)
			if err != nil {
				return unix.EINVAL
			}
			p.set, p.installed, p.state = set, true, alsaStateSetup
			p.availMin, p.startThr, p.stopThr = uint64(set.period), 1, uint64(set.buffer)
			p.logLocked("hw_params")
		}
		hp.encode(arg)
		return nil
	case alsaIoctlSWParams:
		if p.state == alsaStateOpen {
			return unix.EBADFD
		}
		le := binary.LittleEndian
		if le.Uint64(arg[16:]) == 0 {
			return unix.EINVAL
		}
		p.availMin, p.startThr, p.stopThr = le.Uint64(arg[16:]), le.Uint64(arg[32:]), le.Uint64(arg[40:])
		p.proto, p.tstampType, p.tstampMode = le.Uint32(arg[72:]), le.Uint32(arg[76:]), le.Uint32(arg)
		p.logLocked("sw_params")
		return nil
	case alsaIoctlStatus:
		le := binary.LittleEndian
		le.PutUint32(arg, p.state)
		le.PutUint64(arg[8:], uint64(p.trigger/time.Second))
		le.PutUint64(arg[16:], uint64(p.trigger%time.Second))
		if p.tstampMode == 1 || p.state == alsaStateRunning {
			le.PutUint64(arg[24:], uint64(p.clock/time.Second))
			le.PutUint64(arg[32:], uint64(p.clock%time.Second))
		}
		if p.installed {
			le.PutUint64(arg[64:], p.availLocked())
		}
		return nil
	case alsaIoctlPrepare:
		if p.state == alsaStateOpen {
			return unix.EBADFD
		}
		p.state, p.appl, p.hwp = alsaStatePrepared, 0, 0
		p.logLocked("prepare")
		p.notifyLocked()
		return nil
	case alsaIoctlStart:
		if p.state != alsaStatePrepared {
			return unix.EBADFD
		}
		if !p.node.capture && p.appl == p.hwp {
			return unix.EPIPE
		}
		p.state, p.trigger = alsaStateRunning, p.clock
		p.logLocked("start")
		p.notifyLocked()
		return nil
	case alsaIoctlDrop:
		p.state = alsaStateSetup
		p.logLocked("drop")
		p.notifyLocked()
		return nil
	case alsaIoctlResume:
		if p.state != alsaStateSuspended {
			return unix.EBADFD
		}
		if p.noResume {
			return unix.ENOSYS
		}
		if p.resumeWait > 0 {
			p.resumeWait--
			return unix.EAGAIN
		}
		p.state = p.prevState
		p.logLocked("resume")
		p.notifyLocked()
		return nil
	}
	return unix.ENOTTY
}

func (p *fakePCM) xferErrLocked() error {
	switch {
	case p.closed:
		return unix.EBADF
	case p.gone:
		return unix.ENODEV
	case p.state == alsaStateXrun:
		return unix.EPIPE
	case p.state == alsaStateSuspended:
		return unix.ESTRPIPE
	case p.state != alsaStatePrepared && p.state != alsaStateRunning:
		return unix.EBADFD
	}
	return nil
}

func (p *fakePCM) transfer(req uint32, buf []byte, frameBytes int) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.errs[req]; err != nil {
		return 0, err
	}
	if err := p.xferErrLocked(); err != nil {
		return 0, err
	}
	if frameBytes != p.set.frameBytes() || len(buf)%frameBytes != 0 {
		p.f.t.Errorf("transfer: %d bytes with frame size %d, device frame %d", len(buf), frameBytes, p.set.frameBytes())
		return 0, unix.EINVAL
	}
	n := min(uint64(len(buf)/frameBytes), p.availLocked())
	if p.writeCap > 0 {
		n = min(n, p.writeCap)
	}
	if n == 0 {
		return 0, unix.EAGAIN
	}
	if req == alsaIoctlWriteI {
		if p.node.capture {
			return 0, unix.EBADFD
		}
		p.played = append(p.played, buf[:int(n)*frameBytes]...)
		p.appl += n
		if p.state == alsaStatePrepared && p.appl-p.hwp >= p.startThr {
			p.state, p.trigger = alsaStateRunning, p.clock
		}
	} else {
		if !p.node.capture || p.state != alsaStateRunning {
			return 0, unix.EBADFD
		}
		dev := make([]int16, int(n)*p.set.channels)
		for i := range dev {
			dev[i] = p.gen(p.appl+uint64(i/p.set.channels), i%p.set.channels)
		}
		alsaEncode(p.set.format, dev, buf)
		p.appl += n
	}
	p.notifyLocked()
	return int(n), nil
}

func (p *fakePCM) reventsLocked() int16 {
	ok := int16(unix.POLLOUT)
	if p.node.capture {
		ok = unix.POLLIN
	}
	switch {
	case p.pollBits != 0:
		return p.pollBits
	case p.gone:
		return unix.POLLERR
	case p.state == alsaStateRunning || p.state == alsaStatePrepared:
		if p.availLocked() >= p.availMin {
			return ok
		}
		return 0
	}
	return ok | unix.POLLERR
}

func (p *fakePCM) wait(timeout time.Duration, pcm bool) (int16, bool, error) {
	var deadline <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		p.mu.Lock()
		p.waits++
		if p.woken {
			p.woken = false
			p.mu.Unlock()
			return 0, true, nil
		}
		if pcm && p.pollErr != nil {
			err := p.pollErr
			p.mu.Unlock()
			return 0, false, err
		}
		if rev := p.reventsLocked(); pcm && rev != 0 {
			p.mu.Unlock()
			return rev, false, nil
		}
		ch := p.changed
		p.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			return 0, false, nil
		}
	}
}

func (p *fakePCM) wake() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.f.t.Error("wake after close")
	}
	p.woken = true
	p.notifyLocked()
}

func (p *fakePCM) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.f.t.Error("PCM closed twice")
		return
	}
	p.closed = true
	p.logLocked("close")
	p.notifyLocked()
	p.mu.Unlock()
	p.f.mu.Lock()
	p.f.live--
	p.f.mu.Unlock()
}

// tick runs the device for frames: playback consumes, capture produces.
// An empty playback buffer or a full capture buffer stops it in XRUN.
func (p *fakePCM) tick(frames int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.installed {
		return
	}
	start := p.clock
	p.clock += time.Duration(frames) * time.Second / time.Duration(p.set.rate)
	if p.state != alsaStateRunning {
		return
	}
	if p.node.capture {
		p.hwp += uint64(frames)
		if p.hwp-p.appl >= p.stopThr {
			over := p.hwp - p.appl - p.stopThr
			p.state, p.trigger = alsaStateXrun, p.clock-time.Duration(over)*time.Second/time.Duration(p.set.rate)
			p.hwp = p.appl + uint64(p.set.buffer)
		}
	} else {
		if fill := p.appl - p.hwp; uint64(frames) >= fill {
			p.hwp = p.appl
			p.state, p.trigger = alsaStateXrun, start+time.Duration(fill)*time.Second/time.Duration(p.set.rate)
		} else {
			p.hwp += uint64(frames)
		}
	}
	p.notifyLocked()
}

// autoTick runs the device one period per period of real time.
func (p *fakePCM) autoTick() {
	p.mu.Lock()
	period, rate := p.set.period, p.set.rate
	p.tickStop, p.tickDone = make(chan struct{}), make(chan struct{})
	stop, done := p.tickStop, p.tickDone
	p.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(time.Duration(period) * time.Second / time.Duration(rate))
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.tick(period)
			}
		}
	}()
}

func (p *fakePCM) stopTicks() {
	p.mu.Lock()
	stop, done := p.tickStop, p.tickDone
	p.tickStop = nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (p *fakePCM) do(f func(p *fakePCM)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
	p.notifyLocked()
}

func (p *fakePCM) xrun() {
	p.do(func(p *fakePCM) { p.state, p.trigger = alsaStateXrun, p.clock })
}

func (p *fakePCM) suspend(wait int, noResume bool) {
	p.do(func(p *fakePCM) {
		p.prevState, p.state = p.state, alsaStateSuspended
		p.resumeWait, p.noResume = wait, noResume
	})
}

func (p *fakePCM) disconnect() { p.do(func(p *fakePCM) { p.gone = true }) }

func (p *fakePCM) callLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *fakePCM) playedFrames() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.played) / max(p.set.frameBytes(), 1)
}
