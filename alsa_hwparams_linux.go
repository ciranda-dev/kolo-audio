//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Parameter indexes of struct snd_pcm_hw_params.
const (
	alsaParamAccess    = 0
	alsaParamFormat    = 1
	alsaParamSubformat = 2

	alsaParamSampleBits  = 8
	alsaParamFrameBits   = 9
	alsaParamChannels    = 10
	alsaParamRate        = 11
	alsaParamPeriodTime  = 12
	alsaParamPeriodSize  = 13
	alsaParamPeriodBytes = 14
	alsaParamPeriods     = 15
	alsaParamBufferTime  = 16
	alsaParamBufferSize  = 17
	alsaParamBufferBytes = 18
	alsaParamTickTime    = 19

	alsaMasks     = 3
	alsaIntervals = 12
)

const (
	alsaAccessRWInterleaved = 3
	alsaSubformatStd        = 0
)

// PCM states.
const (
	alsaStateOpen = iota
	alsaStateSetup
	alsaStatePrepared
	alsaStateRunning
	alsaStateXrun
	alsaStateDraining
	alsaStatePaused
	alsaStateSuspended
	alsaStateDisconnected
)

// alsaFormat is a sample format the backend converts from and to s16.
type alsaFormat struct {
	code  uint32
	bytes int
	name  string
}

var (
	alsaS16   = alsaFormat{2, 2, "S16_LE"}
	alsaS24   = alsaFormat{6, 4, "S24_LE"}
	alsaS32   = alsaFormat{10, 4, "S32_LE"}
	alsaFloat = alsaFormat{14, 4, "FLOAT_LE"}
	alsaS24_3 = alsaFormat{32, 3, "S24_3LE"}
)

// alsaFormats is the order formats are tried in: s16 needs no conversion,
// then the wider integer formats, then float.
var alsaFormats = []alsaFormat{alsaS16, alsaS32, alsaS24, alsaS24_3, alsaFloat}

// alsaMask is struct snd_mask: 256 bits, of which the kernel uses the first
// 64 and leaves the rest as they are.
type alsaMask [8]uint32

func (m *alsaMask) test(v uint32) bool { return v < 64 && m[v/32]&(1<<(v%32)) != 0 }

func (m *alsaMask) single(v uint32) {
	*m = alsaMask{}
	m[v/32] = 1 << (v % 32)
}

// alsaInterval is struct snd_interval. An open bound excludes its value.
type alsaInterval struct {
	min, max                         uint32
	openMin, openMax, integer, empty bool
}

// first and last are the lowest and highest values in the interval.
func (i alsaInterval) first() uint32 {
	if i.openMin && i.min < math.MaxUint32 {
		return i.min + 1
	}
	return i.min
}

func (i alsaInterval) last() uint32 {
	if i.openMax && i.max > 0 {
		return i.max - 1
	}
	return i.max
}

func (i alsaInterval) single() bool {
	return !i.empty && i.first() == i.last()
}

// alsaHWParams is struct snd_pcm_hw_params without its reserved fields.
type alsaHWParams struct {
	flags     uint32
	masks     [alsaMasks]alsaMask
	intervals [alsaIntervals]alsaInterval
	rmask     uint32
	cmask     uint32
	info      uint32
	msbits    uint32
	rateNum   uint32
	rateDen   uint32
	fifoSize  uint64
}

// alsaAny returns the whole configuration space, as alsa-lib's
// snd_pcm_hw_params_any does before the first refine.
func alsaAny() alsaHWParams {
	var p alsaHWParams
	for k := range p.masks {
		for j := range p.masks[k] {
			p.masks[k][j] = math.MaxUint32
		}
	}
	for k := range p.intervals {
		p.intervals[k] = alsaInterval{max: math.MaxUint32}
	}
	p.rmask = math.MaxUint32
	return p
}

func (p *alsaHWParams) iv(k int) *alsaInterval { return &p.intervals[k-alsaParamSampleBits] }

func (p *alsaHWParams) encode(b []byte) {
	clear(b[:alsaHWParamsSize])
	le := binary.LittleEndian
	le.PutUint32(b, p.flags)
	for k, m := range p.masks {
		for j, w := range m {
			le.PutUint32(b[alsaOffMasks+32*k+4*j:], w)
		}
	}
	for k, iv := range p.intervals {
		o := alsaOffIntervals + 12*k
		le.PutUint32(b[o:], iv.min)
		le.PutUint32(b[o+4:], iv.max)
		var f uint32
		for bit, set := range []bool{iv.openMin, iv.openMax, iv.integer, iv.empty} {
			if set {
				f |= 1 << bit
			}
		}
		le.PutUint32(b[o+8:], f)
	}
	for j, v := range []uint32{p.rmask, p.cmask, p.info, p.msbits, p.rateNum, p.rateDen} {
		le.PutUint32(b[alsaOffRmask+4*j:], v)
	}
	le.PutUint64(b[alsaOffFifo:], p.fifoSize)
}

// decode reads the structure back. An interval whose bounds cross is empty.
func (p *alsaHWParams) decode(b []byte) {
	le := binary.LittleEndian
	p.flags = le.Uint32(b)
	for k := range p.masks {
		for j := range p.masks[k] {
			p.masks[k][j] = le.Uint32(b[alsaOffMasks+32*k+4*j:])
		}
	}
	for k := range p.intervals {
		o := alsaOffIntervals + 12*k
		f := le.Uint32(b[o+8:])
		iv := alsaInterval{min: le.Uint32(b[o:]), max: le.Uint32(b[o+4:]),
			openMin: f&1 != 0, openMax: f&2 != 0, integer: f&4 != 0, empty: f&8 != 0}
		if iv.min > iv.max || iv.min == iv.max && (iv.openMin || iv.openMax) {
			iv.empty = true
		}
		p.intervals[k] = iv
	}
	for j, v := range []*uint32{&p.rmask, &p.cmask, &p.info, &p.msbits, &p.rateNum, &p.rateDen} {
		*v = le.Uint32(b[alsaOffRmask+4*j:])
	}
	p.fifoSize = le.Uint64(b[alsaOffFifo:])
}

// call runs HW_REFINE or HW_PARAMS on p and reads the kernel's answer
// into it.
func (p *alsaHWParams) call(d alsaDev, req uint32) error {
	p.rmask = math.MaxUint32
	b := make([]byte, alsaHWParamsSize)
	p.encode(b)
	if err := d.ioctl(req, b); err != nil {
		return err
	}
	p.decode(b)
	return nil
}

// alsaSetup is the configuration the kernel accepted.
type alsaSetup struct {
	format   alsaFormat
	channels int
	rate     int
	period   int // frames
	buffer   int // frames
}

func (s alsaSetup) frameBytes() int { return s.format.bytes * s.channels }

// alsaNegotiate narrows the device's configuration space one parameter at a
// time with HW_REFINE, in the kernel's order of choice (access, format,
// subformat, channels, rate, period, buffer), and installs it with
// HW_PARAMS. Each value is the one nearest to the request that the kernel
// accepts. Channels prefer the smallest count at or above the request.
// Rate, period and buffer take the nearest value, the higher one on a tie.
func alsaNegotiate(d alsaDev, want Format, period, buffer time.Duration, formats []alsaFormat) (alsaSetup, error) {
	var set alsaSetup
	p := alsaAny()
	if err := p.call(d, alsaIoctlHWRefine); err != nil {
		return set, alsaErr("hw_refine", err)
	}
	q := p
	q.masks[alsaParamAccess].single(alsaAccessRWInterleaved)
	if q.call(d, alsaIoctlHWRefine) != nil {
		return set, fmt.Errorf("%w: device has no interleaved read and write access", ErrUnsupportedFormat)
	}
	p = q
	for _, f := range formats {
		q = p
		q.masks[alsaParamFormat].single(f.code)
		if q.call(d, alsaIoctlHWRefine) == nil {
			p, set.format = q, f
			break
		}
	}
	if set.format.bytes == 0 {
		return set, fmt.Errorf("%w: device formats %s, none of %s", ErrUnsupportedFormat, alsaFormatNames(p.masks[alsaParamFormat], nil), alsaFormatNames(alsaMask{}, formats))
	}
	q = p
	q.masks[alsaParamSubformat].single(alsaSubformatStd)
	if q.call(d, alsaIoctlHWRefine) == nil {
		p = q
	}
	q = p
	q.iv(alsaParamChannels).max = min(q.iv(alsaParamChannels).max, MaxChannels)
	if q.call(d, alsaIoctlHWRefine) != nil {
		return set, fmt.Errorf("%w: device needs more than %d channels", ErrUnsupportedFormat, MaxChannels)
	}
	p = q
	var v uint32
	var err error
	if p, v, err = alsaNear(d, p, alsaParamChannels, uint32(want.Channels), false); err != nil {
		return set, err
	}
	set.channels = int(v)
	if p, v, err = alsaNear(d, p, alsaParamRate, uint32(want.SampleRate), true); err != nil {
		return set, err
	}
	set.rate = int(v)
	dev := Format{SampleRate: set.rate, Channels: set.channels}
	if set.rate < 1 || dev.frames(MaxBuffer) < 1 {
		return set, fmt.Errorf("%w: device rate %d Hz", ErrUnsupportedFormat, set.rate)
	}
	q = p
	q.iv(alsaParamBufferSize).max = min(q.iv(alsaParamBufferSize).max, uint32(dev.frames(MaxBuffer)))
	if q.call(d, alsaIoctlHWRefine) != nil {
		return set, fmt.Errorf("%w: device buffer longer than %v at %d Hz", ErrUnsupportedFormat, MaxBuffer, set.rate)
	}
	p = q
	if p, v, err = alsaNear(d, p, alsaParamPeriodSize, uint32(max(dev.frames(period), 1)), true); err != nil {
		return set, err
	}
	q = p
	q.iv(alsaParamPeriods).min = max(q.iv(alsaParamPeriods).min, 2)
	if q.call(d, alsaIoctlHWRefine) == nil {
		p = q
	}
	if p, _, err = alsaNear(d, p, alsaParamBufferSize, uint32(max(dev.frames(buffer), 2*int(v))), true); err != nil {
		return set, err
	}
	if err := p.call(d, alsaIoctlHWParams); err != nil {
		return set, alsaErr("hw_params", err)
	}
	return alsaInstalled(p, set.format)
}

// alsaInstalled reads the installed configuration and checks it.
func alsaInstalled(p alsaHWParams, f alsaFormat) (alsaSetup, error) {
	get := func(k int) (int, bool) {
		iv := p.intervals[k-alsaParamSampleBits]
		return int(iv.first()), iv.single()
	}
	set := alsaSetup{format: f}
	var ok [4]bool
	set.channels, ok[0] = get(alsaParamChannels)
	set.rate, ok[1] = get(alsaParamRate)
	set.period, ok[2] = get(alsaParamPeriodSize)
	set.buffer, ok[3] = get(alsaParamBufferSize)
	m := p.masks[alsaParamFormat]
	if ok != [4]bool{true, true, true, true} || !m.test(f.code) || alsaMaskCount(m) != 1 ||
		set.channels < 1 || set.channels > MaxChannels || set.rate < 1 ||
		set.period < 1 || set.buffer < set.period || set.buffer > (Format{SampleRate: set.rate}).frames(MaxBuffer) {
		return set, fmt.Errorf("%w: kernel installed an unusable configuration: %d channels, %d Hz, period %d, buffer %d", ErrBackend, set.channels, set.rate, set.period, set.buffer)
	}
	return set, nil
}

func alsaMaskCount(m alsaMask) int {
	return bits.OnesCount32(m[0]) + bits.OnesCount32(m[1])
}

// alsaNear narrows interval k to the accepted value nearest to want and
// returns it. With nearest unset, it takes the lowest value at or above
// want, else the highest below.
func alsaNear(d alsaDev, p alsaHWParams, k int, want uint32, nearest bool) (alsaHWParams, uint32, error) {
	var cands []uint32
	hi, lo := p, p
	hi.iv(k).min, hi.iv(k).openMin = max(hi.iv(k).min, want), hi.iv(k).openMin && hi.iv(k).min >= want
	if hi.call(d, alsaIoctlHWRefine) == nil && !hi.iv(k).empty {
		cands = append(cands, hi.iv(k).first())
	}
	lo.iv(k).max, lo.iv(k).openMax = min(lo.iv(k).max, want), lo.iv(k).openMax && lo.iv(k).max <= want
	if lo.call(d, alsaIoctlHWRefine) == nil && !lo.iv(k).empty {
		v := lo.iv(k).last()
		if len(cands) == 1 && (!nearest || cands[0]-want <= want-v) {
			cands = append(cands, v)
		} else {
			cands = append([]uint32{v}, cands...)
		}
	}
	for _, v := range cands {
		q := p
		*q.iv(k) = alsaInterval{min: v, max: v, integer: p.iv(k).integer}
		if q.call(d, alsaIoctlHWRefine) == nil {
			return q, v, nil
		}
	}
	return p, 0, fmt.Errorf("%w: device accepts no %s near %d", ErrUnsupportedFormat, alsaParamNames[k-alsaParamSampleBits], want)
}

var alsaParamNames = [alsaIntervals]string{"sample bits", "frame bits", "channel count", "rate", "period time", "period size", "period bytes", "period count", "buffer time", "buffer size", "buffer bytes", "tick time"}

var alsaFormatList = []alsaFormat{alsaS16, alsaS24, alsaS32, alsaFloat, alsaS24_3,
	{0, 1, "S8"}, {1, 1, "U8"}, {3, 2, "S16_BE"}, {4, 2, "U16_LE"}, {5, 2, "U16_BE"}, {7, 4, "S24_BE"},
	{11, 4, "S32_BE"}, {15, 4, "FLOAT_BE"}, {16, 8, "FLOAT64_LE"}, {33, 3, "S24_3BE"}}

// alsaFormatNames names the formats in m, or in list when m is empty.
func alsaFormatNames(m alsaMask, list []alsaFormat) string {
	var names []string
	for _, f := range list {
		names = append(names, f.name)
	}
	if list == nil {
		for _, f := range alsaFormatList {
			if m.test(f.code) {
				names = append(names, f.name)
			}
		}
		if len(names) == 0 {
			names = append(names, "none known")
		}
	}
	return strings.Join(names, ", ")
}

// alsaSW is struct snd_pcm_sw_params as the backend sets it.
type alsaSW struct {
	availMin, startThreshold, stopThreshold uint64
	proto, tstampType                       uint32
}

func (s alsaSW) call(d alsaDev) error {
	b := make([]byte, alsaSWParamsSize)
	le := binary.LittleEndian
	le.PutUint32(b, 1)     // tstamp_mode ENABLE: status then has the time of an xrun
	le.PutUint32(b[4:], 1) // period_step
	le.PutUint64(b[alsaOffAvailMin:], s.availMin)
	le.PutUint64(b[alsaOffStartThr:], s.startThreshold)
	le.PutUint64(b[alsaOffStopThr:], s.stopThreshold)
	le.PutUint32(b[alsaOffProto:], s.proto)
	le.PutUint32(b[alsaOffTstampTyp:], s.tstampType)
	return d.ioctl(alsaIoctlSWParams, b)
}

// alsaStatus is the part of struct snd_pcm_status the backend reads.
type alsaStatus struct {
	state   uint32
	trigger time.Duration // trigger_tstamp
	now     time.Duration // tstamp
	avail   uint64
}

func alsaTimespec(b []byte) time.Duration {
	sec := int64(binary.LittleEndian.Uint64(b))
	nsec := int64(binary.LittleEndian.Uint64(b[8:]))
	if sec < 0 || sec > math.MaxInt64/int64(time.Second)-1 || nsec < 0 || nsec >= int64(time.Second) {
		return 0
	}
	return time.Duration(sec)*time.Second + time.Duration(nsec)
}

func alsaGetStatus(d alsaDev) (alsaStatus, error) {
	b := make([]byte, alsaStatusSize)
	if err := d.ioctl(alsaIoctlStatus, b); err != nil {
		return alsaStatus{}, err
	}
	return alsaStatus{
		state:   binary.LittleEndian.Uint32(b),
		trigger: alsaTimespec(b[alsaOffTrigger:]),
		now:     alsaTimespec(b[alsaOffTstamp:]),
		avail:   binary.LittleEndian.Uint64(b[alsaOffAvail:]),
	}, nil
}

func alsaPCMVersion(d alsaDev) uint32 {
	b := make([]byte, 4)
	if d.ioctl(alsaIoctlPCMVersion, b) != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// alsaCString returns the NUL-terminated text in b as printable UTF-8.
func alsaCString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	s := strings.ToValidUTF8(string(b), "�")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
}

type alsaCard struct {
	id, name string
}

func alsaCardInfo(d alsaDev) (alsaCard, error) {
	b := make([]byte, alsaCardInfoSize)
	if err := d.ioctl(alsaIoctlCardInfo, b); err != nil {
		return alsaCard{}, err
	}
	return alsaCard{id: alsaCString(b[alsaOffCardID : alsaOffCardID+16]), name: alsaCString(b[alsaOffCardName : alsaOffCardName+32])}, nil
}

// alsaPCMName returns the name of a PCM device's stream, through the
// control device.
func alsaPCMName(d alsaDev, dev int, capture bool) (string, error) {
	b := make([]byte, alsaPCMInfoSize)
	binary.LittleEndian.PutUint32(b, uint32(dev))
	if capture {
		binary.LittleEndian.PutUint32(b[alsaOffInfoStream:], 1)
	}
	if err := d.ioctl(alsaIoctlPCMInfo, b); err != nil {
		return "", err
	}
	return alsaCString(b[alsaOffInfoName : alsaOffInfoName+80]), nil
}

// alsaErr wraps a kernel error from op: permission errors keep
// fs.ErrPermission, a vanished device is ErrNoDevice.
func alsaErr(op string, err error) error {
	var errno unix.Errno
	if errors.As(err, &errno) && (errno == unix.ENODEV || errno == unix.ENOENT || errno == unix.ENXIO) {
		return fmt.Errorf("%w: %s: %w", ErrNoDevice, op, err)
	}
	if errors.As(err, &errno) && errno == unix.EBUSY {
		return fmt.Errorf("%w: %s: device busy (another program uses it; direct ALSA access is exclusive)", ErrBackend, op)
	}
	return fmt.Errorf("%w: %s: %w", ErrBackend, op, err)
}
