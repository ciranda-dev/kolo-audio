//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// alsaHeaderTable is the output of a C program built against
// <sound/asound.h> (linux-libc-dev 6.1.190-1) on linux/arm64 and
// linux/amd64; both printed exactly this.
const alsaHeaderTable = `size snd_pcm_hw_params 608
size snd_pcm_sw_params 136
size snd_pcm_status 152
size snd_xferi 24
size snd_pcm_info 288
size snd_ctl_card_info 376
size snd_interval 12
size snd_mask 32
off snd_pcm_hw_params.masks 4
off snd_pcm_hw_params.intervals 260
off snd_pcm_hw_params.rmask 512
off snd_pcm_hw_params.fifo_size 536
off snd_pcm_sw_params.avail_min 16
off snd_pcm_sw_params.start_threshold 32
off snd_pcm_sw_params.stop_threshold 40
off snd_pcm_sw_params.proto 72
off snd_pcm_sw_params.tstamp_type 76
off snd_pcm_status.trigger_tstamp 8
off snd_pcm_status.tstamp 24
off snd_pcm_status.avail 64
off snd_xferi.result 0
off snd_xferi.buf 8
off snd_xferi.frames 16
off snd_pcm_info.stream 8
off snd_pcm_info.name 80
off snd_ctl_card_info.id 8
off snd_ctl_card_info.name 40
bit openmin 0x1
bit openmax 0x2
bit integer 0x4
bit empty 0x8
ioctl SNDRV_PCM_IOCTL_PVERSION 0x80044100
ioctl SNDRV_PCM_IOCTL_HW_REFINE 0xc2604110
ioctl SNDRV_PCM_IOCTL_HW_PARAMS 0xc2604111
ioctl SNDRV_PCM_IOCTL_SW_PARAMS 0xc0884113
ioctl SNDRV_PCM_IOCTL_STATUS 0x80984120
ioctl SNDRV_PCM_IOCTL_PREPARE 0x00004140
ioctl SNDRV_PCM_IOCTL_START 0x00004142
ioctl SNDRV_PCM_IOCTL_DROP 0x00004143
ioctl SNDRV_PCM_IOCTL_RESUME 0x00004147
ioctl SNDRV_PCM_IOCTL_WRITEI_FRAMES 0x40184150
ioctl SNDRV_PCM_IOCTL_READI_FRAMES 0x80184151
ioctl SNDRV_CTL_IOCTL_CARD_INFO 0x81785501
ioctl SNDRV_CTL_IOCTL_PCM_INFO 0xc1205531`

func TestALSAHeaderTable(t *testing.T) {
	var x alsaXferi
	var iv [12]byte
	p := alsaHWParams{}
	b := make([]byte, alsaHWParamsSize)
	ours := map[string]uint64{
		"size snd_pcm_hw_params":                alsaHWParamsSize,
		"size snd_pcm_sw_params":                alsaSWParamsSize,
		"size snd_pcm_status":                   alsaStatusSize,
		"size snd_xferi":                        uint64(unsafe.Sizeof(x)),
		"size snd_pcm_info":                     alsaPCMInfoSize,
		"size snd_ctl_card_info":                alsaCardInfoSize,
		"size snd_interval":                     uint64(len(iv)),
		"size snd_mask":                         uint64(unsafe.Sizeof(alsaMask{})),
		"off snd_pcm_hw_params.masks":           alsaOffMasks,
		"off snd_pcm_hw_params.intervals":       alsaOffIntervals,
		"off snd_pcm_hw_params.rmask":           alsaOffRmask,
		"off snd_pcm_hw_params.fifo_size":       alsaOffFifo,
		"off snd_pcm_sw_params.avail_min":       alsaOffAvailMin,
		"off snd_pcm_sw_params.start_threshold": alsaOffStartThr,
		"off snd_pcm_sw_params.stop_threshold":  alsaOffStopThr,
		"off snd_pcm_sw_params.proto":           alsaOffProto,
		"off snd_pcm_sw_params.tstamp_type":     alsaOffTstampTyp,
		"off snd_pcm_status.trigger_tstamp":     alsaOffTrigger,
		"off snd_pcm_status.tstamp":             alsaOffTstamp,
		"off snd_pcm_status.avail":              alsaOffAvail,
		"off snd_xferi.result":                  uint64(unsafe.Offsetof(x.result)),
		"off snd_xferi.buf":                     uint64(unsafe.Offsetof(x.buf)),
		"off snd_xferi.frames":                  uint64(unsafe.Offsetof(x.frames)),
		"off snd_pcm_info.stream":               alsaOffInfoStream,
		"off snd_pcm_info.name":                 alsaOffInfoName,
		"off snd_ctl_card_info.id":              alsaOffCardID,
		"off snd_ctl_card_info.name":            alsaOffCardName,
		"ioctl SNDRV_PCM_IOCTL_PVERSION":        uint64(alsaIoctlPCMVersion),
		"ioctl SNDRV_PCM_IOCTL_HW_REFINE":       uint64(alsaIoctlHWRefine),
		"ioctl SNDRV_PCM_IOCTL_HW_PARAMS":       uint64(alsaIoctlHWParams),
		"ioctl SNDRV_PCM_IOCTL_SW_PARAMS":       uint64(alsaIoctlSWParams),
		"ioctl SNDRV_PCM_IOCTL_STATUS":          uint64(alsaIoctlStatus),
		"ioctl SNDRV_PCM_IOCTL_PREPARE":         uint64(alsaIoctlPrepare),
		"ioctl SNDRV_PCM_IOCTL_START":           uint64(alsaIoctlStart),
		"ioctl SNDRV_PCM_IOCTL_DROP":            uint64(alsaIoctlDrop),
		"ioctl SNDRV_PCM_IOCTL_RESUME":          uint64(alsaIoctlResume),
		"ioctl SNDRV_PCM_IOCTL_WRITEI_FRAMES":   uint64(alsaIoctlWriteI),
		"ioctl SNDRV_PCM_IOCTL_READI_FRAMES":    uint64(alsaIoctlReadI),
		"ioctl SNDRV_CTL_IOCTL_CARD_INFO":       uint64(alsaIoctlCardInfo),
		"ioctl SNDRV_CTL_IOCTL_PCM_INFO":        uint64(alsaIoctlPCMInfo),
	}
	for i, name := range []string{"openmin", "openmax", "integer", "empty"} {
		p.intervals[0] = alsaInterval{}
		*[]*bool{&p.intervals[0].openMin, &p.intervals[0].openMax, &p.intervals[0].integer, &p.intervals[0].empty}[i] = true
		p.encode(b)
		ours["bit "+name] = uint64(binary.LittleEndian.Uint32(b[alsaOffIntervals+8:]))
	}
	for _, line := range strings.Split(alsaHeaderTable, "\n") {
		i := strings.LastIndexByte(line, ' ')
		var want uint64
		if _, err := fmt.Sscan(line[i+1:], &want); err != nil {
			if _, err := fmt.Sscanf(line[i+1:], "0x%x", &want); err != nil {
				t.Fatalf("%q: %v", line, err)
			}
		}
		got, ok := ours[line[:i]]
		if !ok || got != want {
			t.Errorf("%s: header %#x, ours %#x (known %v)", line[:i], want, got, ok)
		}
		delete(ours, line[:i])
	}
	if len(ours) != 0 {
		t.Errorf("not checked against the header: %v", ours)
	}
}

func TestALSAParseNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		want alsaNode
		ok   bool
	}{
		{"pcmC0D0p", alsaNode{0, 0, false}, true},
		{"pcmC1D7c", alsaNode{1, 7, true}, true},
		{"pcmC31D1023p", alsaNode{31, 1023, false}, true},
		{"pcmC0D0", alsaNode{}, false},
		{"pcmC0D0x", alsaNode{}, false},
		{"pcmC00D0p", alsaNode{}, false},
		{"pcmC-1D0p", alsaNode{}, false},
		{"pcmC+1D0p", alsaNode{}, false},
		{"pcmC12345D0p", alsaNode{}, false},
		{"pcmCD0p", alsaNode{}, false},
		{"pcmC0D0pp", alsaNode{}, false},
		{"controlC0", alsaNode{}, false},
		{"hwC0D0", alsaNode{}, false},
		{"", alsaNode{}, false},
	} {
		got, ok := alsaParseNode(tc.name)
		if ok != tc.ok || ok && got != tc.want {
			t.Errorf("%q: %+v %v", tc.name, got, ok)
		}
		if ok && got.file() != tc.name {
			t.Errorf("%q: file() = %q", tc.name, got.file())
		}
	}
}

func TestALSAID(t *testing.T) {
	id := alsaID("Loopback", 1)
	if hexText(t, id) != "hw:CARD=Loopback,DEV=1" {
		t.Fatalf("id %s", id)
	}
	card, dev, ok := alsaParseID(id)
	if !ok || card != "Loopback" || dev != 1 {
		t.Errorf("parse: %q %d %v", card, dev, ok)
	}
	for _, bad := range []string{"", "zz", hexEncode("hw:CARD=,DEV=0"), hexEncode("hw:CARD=a,b,DEV=0"), hexEncode("hw:CARD=x,DEV=01"),
		hexEncode("hw:CARD=x,DEV=0,SUBDEV=1"), hexEncode("hw:CARD=x"), hexEncode("plughw:CARD=x,DEV=0"), hexEncode("hw:CARD=../x,DEV=0")} {
		if _, _, ok := alsaParseID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	for id, ok := range map[string]bool{"Loopback": true, "PCH": true, "Card_1": true, "a-b": true, "": false, "a,b": false, "a=b": false, "x/y": false, "sixteen_chars_xx": false, "Gerät": false} {
		if alsaValidCardID(id) != ok {
			t.Errorf("valid(%q) != %v", id, ok)
		}
	}
}

func TestALSACString(t *testing.T) {
	for in, want := range map[string]string{
		"Loopback\x00junk": "Loopback",
		"  name \n":        "name",
		"a\x1bb\x7fc":      "abc",
		"\xff\xfe":         "�",
		"":                 "",
	} {
		if got := alsaCString([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestALSAHWParamsRoundTrip(t *testing.T) {
	p := alsaAny()
	p.masks[alsaParamFormat].single(alsaS24_3.code)
	*p.iv(alsaParamRate) = alsaInterval{min: 44100, max: 48000, openMin: true, integer: true}
	p.msbits, p.rateNum, p.rateDen, p.fifoSize, p.cmask, p.info, p.flags = 24, 48000, 1, 7, 3, 9, 1
	b := make([]byte, alsaHWParamsSize)
	p.encode(b)
	var q alsaHWParams
	q.decode(b)
	p.rmask = math.MaxUint32
	if q != p {
		t.Errorf("round trip\n%+v\n%+v", q, p)
	}
	*q.iv(alsaParamChannels) = alsaInterval{min: 3, max: 2}
	q.encode(b)
	q.decode(b)
	if !q.iv(alsaParamChannels).empty {
		t.Error("crossed bounds must decode as empty")
	}
	for _, tc := range []struct {
		iv          alsaInterval
		first, last uint32
		single      bool
	}{
		{alsaInterval{min: 5, max: 5}, 5, 5, true},
		{alsaInterval{min: 5, max: 6, openMin: true}, 6, 6, true},
		{alsaInterval{min: 5, max: 6, openMax: true}, 5, 5, true},
		{alsaInterval{min: 5, max: 9, openMin: true, openMax: true}, 6, 8, false},
		{alsaInterval{min: math.MaxUint32, max: math.MaxUint32, openMin: true}, math.MaxUint32, math.MaxUint32, true},
		{alsaInterval{min: 0, max: 0, openMax: true}, 0, 0, true},
		{alsaInterval{min: 1, max: 2, empty: true}, 1, 2, false},
	} {
		if tc.iv.first() != tc.first || tc.iv.last() != tc.last || tc.iv.single() != tc.single {
			t.Errorf("%+v: first %d last %d single %v", tc.iv, tc.iv.first(), tc.iv.last(), tc.iv.single())
		}
	}
}

// TestALSAFakeKernelRules checks the fake kernel's refinement against
// values the kernel's rules imply for the loopback driver.
func TestALSAFakeKernelRules(t *testing.T) {
	c := fakeBuild(fakeLoopback())
	p := alsaAny()
	p.masks[alsaParamFormat].single(alsaS16.code)
	*p.iv(alsaParamChannels) = alsaInterval{min: 2, max: 2}
	*p.iv(alsaParamRate) = alsaInterval{min: 48000, max: 48000}
	if err := c.refine(&p); err != nil {
		t.Fatal(err)
	}
	want := map[int][2]uint32{
		alsaParamSampleBits:  {16, 16},
		alsaParamFrameBits:   {32, 32},
		alsaParamPeriodBytes: {64, 1024 * 1024},
		alsaParamPeriodSize:  {16, 262144},
		alsaParamBufferSize:  {16, 524288},
		alsaParamPeriods:     {1, 1024},
	}
	for k, w := range want {
		if iv := *p.iv(k); iv.min != w[0] || iv.max != w[1] {
			t.Errorf("%s = %+v, want %v", alsaParamNames[ivIdx(k)], iv, w)
		}
	}
	if p.msbits != 16 || p.rateNum != 48000 || p.rateDen != 1 {
		t.Errorf("fixup: msbits %d rate %d/%d", p.msbits, p.rateNum, p.rateDen)
	}
	q := p
	*q.iv(alsaParamPeriodSize) = alsaInterval{min: 480, max: 480}
	*q.iv(alsaParamBufferSize) = alsaInterval{min: 4800, max: 4800}
	if err := c.refine(&q); err != nil {
		t.Fatal(err)
	}
	if iv := *q.iv(alsaParamPeriods); !iv.single() || iv.first() != 10 {
		t.Errorf("periods %+v", iv)
	}
	if iv := *q.iv(alsaParamPeriodTime); !iv.single() || iv.first() != 10000 {
		t.Errorf("period time %+v", iv)
	}
	q = p
	*q.iv(alsaParamPeriodSize) = alsaInterval{min: 480, max: 480}
	*q.iv(alsaParamBufferSize) = alsaInterval{min: 1000, max: 1000}
	if err := c.refine(&q); err == nil {
		t.Errorf("a buffer that is not a whole number of periods must be refused: %+v", *q.iv(alsaParamPeriods))
	}
	q = p
	q.masks[alsaParamAccess] = alsaMask{}
	if err := c.refine(&q); err == nil {
		t.Error("an empty mask must be refused")
	}
}

type alsaSpy struct {
	*spy
	xruns  atomic.Uint64
	failed atomic.Pointer[error]
}

func (s *alsaSpy) onXrun(frames uint64) { s.xruns.Add(frames) }
func (s *alsaSpy) fail(err error)       { s.failed.Store(&err) }

func alsaCfg(t *testing.T, rate, ch int) Config {
	t.Helper()
	cfg, err := Config{Format: Format{SampleRate: rate, Channels: ch}}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func openALSASpy(t *testing.T, f *fakeALSA, kind Kind, cfg Config) (*alsaStream, *alsaSpy) {
	t.Helper()
	sp := &alsaSpy{spy: &spy{ch: cfg.Format.Channels}}
	a, err := alsaOpenSys(f, sp, kind, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeALSASpy(a, sp) })
	return a, sp
}

func closeALSASpy(a *alsaStream, sp *alsaSpy) {
	a.close()
	sp.closed.Store(true)
}

func loopbackFake(t *testing.T) *fakeALSA {
	f := newFakeALSA(t)
	f.add(0, "Loopback", "Loopback", fakeLoopback(), 0, 1)
	return f
}

func negotiate(t *testing.T, hw *fakeHW, want Format, period, buffer time.Duration, formats []alsaFormat) (alsaSetup, error) {
	t.Helper()
	f := newFakeALSA(t)
	f.add(0, "Card", "Card", hw, 0)
	d, err := f.open("pcmC0D0p", true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	return alsaNegotiate(d, want, period, buffer, formats)
}

func TestALSANegotiate(t *testing.T) {
	ms := time.Millisecond
	usb := func() *fakeHW { // a USB-style device: S24_3LE only, fixed rates, stereo
		return &fakeHW{formats: []uint32{alsaS24_3.code}, channelsMin: 2, channelsMax: 2, rateMin: 44100, rateMax: 96000,
			rates: []uint32{44100, 48000, 96000}, periodBytesMin: 48, periodBytesMax: 1 << 20, periodsMin: 2, periodsMax: 1024,
			bufferBytesMax: 1 << 20, periodsInteger: true}
	}
	for _, tc := range []struct {
		name   string
		hw     func() *fakeHW
		want   Format
		period time.Duration
		buffer time.Duration
		setup  alsaSetup
	}{
		{"s16 direct", fakeLoopback, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 2, 48000, 480, 5760}},
		{"mono", fakeLoopback, Format{16000, 1}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 1, 16000, 160, 1920}},
		{"usb s24_3le", usb, Format{48000, 1}, 10 * ms, 120 * ms, alsaSetup{alsaS24_3, 2, 48000, 480, 5760}},
		{"usb nearest rate", usb, Format{16000, 2}, 10 * ms, 40 * ms, alsaSetup{alsaS24_3, 2, 44100, 441, 1764}},
		{"usb rate tie goes up", usb, Format{72000, 2}, 10 * ms, 40 * ms, alsaSetup{alsaS24_3, 2, 96000, 960, 3840}},
		{"s32 only", func() *fakeHW { h := fakeLoopback(); h.formats = []uint32{alsaS32.code, alsaFloat.code}; return h }, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaS32, 2, 48000, 480, 5760}},
		{"s24 only", func() *fakeHW { h := fakeLoopback(); h.formats = []uint32{alsaS24.code}; return h }, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaS24, 2, 48000, 480, 5760}},
		{"float only", func() *fakeHW { h := fakeLoopback(); h.formats = []uint32{alsaFloat.code}; return h }, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaFloat, 2, 48000, 480, 5760}},
		{"stereo on 4 to 8", func() *fakeHW { h := fakeLoopback(); h.channelsMin, h.channelsMax = 4, 8; return h }, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 4, 48000, 480, 5760}},
		{"6 on mono and stereo", func() *fakeHW { h := fakeLoopback(); h.channelsMax = 2; return h }, Format{48000, 6}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 2, 48000, 480, 5760}},
		{"channel list", func() *fakeHW { h := fakeLoopback(); h.channelList = []uint32{2, 8}; return h }, Format{48000, 3}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 8, 48000, 480, 5760}},
		{"period step", func() *fakeHW { h := fakeLoopback(); h.periodStep = 64; return h }, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 2, 48000, 512, 5632}},
		{"small buffer gets two periods", fakeLoopback, Format{48000, 2}, 10 * ms, 10 * ms, alsaSetup{alsaS16, 2, 48000, 480, 960}},
		{"buffer limit", func() *fakeHW { h := fakeLoopback(); h.bufferBytesMax = 8192; return h }, Format{48000, 2}, 10 * ms, 120 * ms, alsaSetup{alsaS16, 2, 48000, 480, 1920}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := negotiate(t, tc.hw(), tc.want, tc.period, tc.buffer, alsaFormats)
			if err != nil || got != tc.setup {
				t.Errorf("got %+v, %v\nwant %+v", got, err, tc.setup)
			}
		})
	}
}

func TestALSANegotiateErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		hw   func() *fakeHW
		want string
		is   error
	}{
		{"no interleaved access", func() *fakeHW { h := fakeLoopback(); h.nonInterleavedOnly = true; return h }, "interleaved", ErrUnsupportedFormat},
		{"no usable format", func() *fakeHW { h := fakeLoopback(); h.formats = []uint32{1, 3}; return h }, "device formats U8, S16_BE", ErrUnsupportedFormat},
		{"too many channels", func() *fakeHW { h := fakeLoopback(); h.channelsMin, h.channelsMax = 300, 300; return h }, "more than 254 channels", ErrUnsupportedFormat},
		{"buffer too long", func() *fakeHW {
			h := fakeLoopback()
			h.periodBytesMin, h.bufferBytesMax, h.periodBytesMax = 1<<23, 1<<24, 1<<24
			return h
		}, "buffer longer than 2s", ErrUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := negotiate(t, tc.hw(), Format{48000, 2}, 10*time.Millisecond, 120*time.Millisecond, alsaFormats)
			if !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v", err)
			}
		})
	}
	f := loopbackFake(t)
	f.tune = func(p *fakePCM) { p.errs[alsaIoctlHWRefine] = unix.EIO }
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Playback, alsaCfg(t, 48000, 1)); !errors.Is(err, ErrBackend) {
		t.Errorf("refine failure: %v", err)
	}
	f.tune = func(p *fakePCM) { p.errs[alsaIoctlSWParams] = unix.EINVAL }
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Playback, alsaCfg(t, 48000, 1)); !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "sw_params") {
		t.Errorf("sw_params failure: %v", err)
	}
	f.tune = func(p *fakePCM) { p.errs[alsaIoctlStart] = unix.EIO }
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Capture, alsaCfg(t, 48000, 1)); !errors.Is(err, ErrBackend) {
		t.Errorf("start failure: %v", err)
	}
}

func TestALSASoftwareParams(t *testing.T) {
	f := loopbackFake(t)
	_, _ = openALSASpy(t, f, Playback, alsaCfg(t, 48000, 2))
	_, _ = openALSASpy(t, f, Capture, alsaCfg(t, 48000, 2))
	play, rec := f.pcm(t, 0), f.pcm(t, 1)
	play.do(func(p *fakePCM) {
		if p.availMin != 5760-1440+480 || p.startThr != 1440 || p.stopThr != 5760 || p.proto != 0x2000f || p.tstampType != 1 || p.tstampMode != 1 {
			t.Errorf("playback sw: avail_min %d start %d stop %d proto %#x tstamp %d/%d", p.availMin, p.startThr, p.stopThr, p.proto, p.tstampMode, p.tstampType)
		}
	})
	rec.do(func(p *fakePCM) {
		if p.availMin != 480 || p.stopThr != 5760 || p.state != alsaStateRunning {
			t.Errorf("capture sw: avail_min %d stop %d state %d", p.availMin, p.stopThr, p.state)
		}
	})
	if got := strings.Join(rec.callLog(), " "); got != "hw_params sw_params prepare start" {
		t.Errorf("capture calls: %s", got)
	}
	f.tune = func(p *fakePCM) { p.version = 0x2000b }
	_, _ = openALSASpy(t, f, Playback, alsaCfg(t, 48000, 2))
	f.pcm(t, 2).do(func(p *fakePCM) {
		if p.proto != 0x2000b || p.tstampType != 0 {
			t.Errorf("before protocol 2.0.12 the timestamp type is not sent: proto %#x type %d", p.proto, p.tstampType)
		}
	})
}

func TestALSAEnumerate(t *testing.T) {
	f := loopbackFake(t)
	usb := f.add(2, "Device", "USB Audio Device", fakeLoopback(), 0)
	usb.noCtl = true
	f.add(3, "bad,id", "Bad", fakeLoopback(), 0)
	missing := f.add(4, "Missing", "Missing", fakeLoopback(), 0, 1)
	missing.pcmMissed[alsaNode{4, 1, true}] = true
	f.add(5, "Loopback", "Duplicate", fakeLoopback(), 0)
	devs, err := alsaListSys(f, Capture)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{alsaID("Loopback", 0), "Loopback, Loopback PCM 0", Capture, true},
		{alsaID("Loopback", 1), "Loopback, Loopback PCM 1", Capture, false},
		{alsaID("Device", 0), "Device, device 0", Capture, false},
		{alsaID("Missing", 0), "Missing, Missing PCM 0", Capture, false},
	}
	if fmt.Sprint(devs) != fmt.Sprint(want) {
		t.Errorf("devices\n%v\nwant\n%v", devs, want)
	}
	if devs, err := alsaListSys(f, Playback); err != nil || len(devs) != 5 {
		t.Errorf("playback: %v %v", devs, err)
	}
}

func TestALSAAbsentAndErrors(t *testing.T) {
	f := newFakeALSA(t)
	f.listErr = &fs.PathError{Op: "open", Path: alsaDir, Err: fs.ErrNotExist}
	var absent *backendAbsent
	if _, err := alsaListSys(f, Playback); !errors.As(err, &absent) || absent.reason != "no ALSA devices in /dev/snd" {
		t.Errorf("no /dev/snd: %v", err)
	}
	f.listErr = nil
	f.nodes = []string{"controlC0", "seq", "timer"}
	if _, err := alsaListSys(f, Playback); !errors.As(err, &absent) {
		t.Errorf("no PCM nodes: %v", err)
	}
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Playback, alsaCfg(t, 48000, 1)); !errors.As(err, &absent) {
		t.Errorf("open without PCM nodes: %v", err)
	}
	f.listErr = &fs.PathError{Op: "open", Path: alsaDir, Err: unix.EACCES}
	if _, err := alsaListSys(f, Playback); !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("unreadable /dev/snd: %v", err)
	}
	f.listErr = nil

	f = loopbackFake(t)
	f.openErr["controlC0"] = unix.EACCES
	if _, err := alsaListSys(f, Playback); !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("control permission: %v", err)
	}
	delete(f.openErr, "controlC0")
	f.cards[0].infoErr = unix.EIO
	if _, err := alsaListSys(f, Playback); !errors.Is(err, ErrBackend) {
		t.Errorf("card info failure: %v", err)
	}
	f.cards[0].infoErr = nil
	cfg := alsaCfg(t, 48000, 1)
	for errno, want := range map[unix.Errno]error{unix.EACCES: fs.ErrPermission, unix.EBUSY: ErrBackend, unix.ENOENT: ErrNoDevice, unix.ENODEV: ErrNoDevice} {
		f.openErr["pcmC0D0p"] = errno
		_, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Playback, cfg)
		if !errors.Is(err, want) {
			t.Errorf("%v: %v", errno, err)
		}
		if errno == unix.EBUSY && !strings.Contains(err.Error(), "busy") {
			t.Errorf("busy: %v", err)
		}
	}
	delete(f.openErr, "pcmC0D0p")
	cfg.DeviceID = alsaID("Loopback", 9)
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Playback, cfg); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown device: %v", err)
	}
	cfg.DeviceID = "not hex"
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Playback, cfg); !errors.Is(err, ErrNoDevice) {
		t.Errorf("bad id: %v", err)
	}
	f = newFakeALSA(t)
	f.add(0, "Out", "Out", fakeLoopback(), 0)
	f.nodes = []string{"controlC0", "pcmC0D0p"}
	if devs, err := alsaListSys(f, Capture); err != nil || len(devs) != 0 {
		t.Errorf("capture on a playback-only system: %v %v", devs, err)
	}
	if _, err := alsaOpenSys(f, &alsaSpy{spy: &spy{}}, Capture, alsaCfg(t, 48000, 1)); !errors.Is(err, ErrNoDevice) {
		t.Errorf("default capture on a playback-only system: %v", err)
	}
}

func waitALSA(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// decodePlayed returns what the fake device played as s16 frames.
func decodePlayed(p *fakePCM) []int16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int16, len(p.played)/p.set.format.bytes)
	alsaDecode(p.set.format, p.played, out)
	return out
}

func TestALSAPlaybackFrames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		formats []uint32
		devCh   uint32
		appCh   int
	}{
		{"s16 stereo", nil, 0, 2},
		{"s16 mono", nil, 0, 1},
		{"s24_3le", []uint32{alsaS24_3.code}, 0, 2},
		{"s32", []uint32{alsaS32.code}, 0, 2},
		{"s24", []uint32{alsaS24.code}, 0, 2},
		{"float", []uint32{alsaFloat.code}, 0, 2},
		{"mono to stereo device", nil, 2, 1},
		{"stereo to 4 channel device", nil, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeaks(t)
			f := newFakeALSA(t)
			hw := fakeLoopback()
			if tc.formats != nil {
				hw.formats = tc.formats
			}
			if tc.devCh != 0 {
				hw.channelsMin, hw.channelsMax = tc.devCh, tc.devCh
			}
			f.add(0, "Loopback", "Loopback", hw, 0)
			a, sp := openALSASpy(t, f, Playback, alsaCfg(t, 48000, tc.appCh))
			p := f.pcm(t, 0)
			waitALSA(t, "prefill", func() bool { return p.playedFrames() >= 1440 })
			for range 20 {
				p.tick(480)
				time.Sleep(time.Millisecond)
				waitALSA(t, "refill", func() bool {
					p.mu.Lock()
					defer p.mu.Unlock()
					return p.appl-p.hwp == 1440 || p.state != alsaStateRunning
				})
			}
			closeALSASpy(a, sp)
			sp.check(t)
			got := decodePlayed(p)
			devCh := p.set.channels
			if len(got)/devCh < 1440+20*480 {
				t.Fatalf("played %d frames", len(got)/devCh)
			}
			for i := 0; i < len(got)/devCh; i++ {
				for c := 0; c < devCh; c++ {
					want := int16(i + 1)
					if c >= tc.appCh && tc.appCh != 1 {
						want = 0
					}
					if got[i*devCh+c] != want {
						t.Fatalf("frame %d channel %d = %d, want %d", i, c, got[i*devCh+c], want)
					}
				}
			}
			if x := sp.xruns.Load(); x != 0 {
				t.Errorf("xruns %d", x)
			}
		})
	}
}

func TestALSACaptureFrames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		formats []uint32
		devCh   uint32
		appCh   int
	}{
		{"s16 stereo", nil, 0, 2},
		{"s24_3le mono", []uint32{alsaS24_3.code}, 0, 1},
		{"s32", []uint32{alsaS32.code}, 0, 2},
		{"s24", []uint32{alsaS24.code}, 0, 2},
		{"float", []uint32{alsaFloat.code}, 0, 2},
		{"stereo device to mono", nil, 2, 1},
		{"6 channel device to stereo", nil, 6, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeaks(t)
			f := newFakeALSA(t)
			hw := fakeLoopback()
			if tc.formats != nil {
				hw.formats = tc.formats
			}
			if tc.devCh != 0 {
				hw.channelsMin, hw.channelsMax = tc.devCh, tc.devCh
			}
			f.add(0, "Loopback", "Loopback", hw, 0)
			f.gen = func(frame uint64, ch int) int16 { return int16(frame%20000) + int16(ch)*2 }
			a, sp := openALSASpy(t, f, Capture, alsaCfg(t, 48000, tc.appCh))
			p := f.pcm(t, 0)
			for i := range 30 {
				p.tick(480)
				waitALSA(t, "capture", func() bool { return sp.capturedLen() >= (i+1)*480*tc.appCh })
			}
			closeALSASpy(a, sp)
			sp.check(t)
			sp.mu.Lock()
			got := sp.captured
			sp.mu.Unlock()
			devCh := int(p.set.channels)
			for i := 0; i < len(got)/tc.appCh; i++ {
				for c := 0; c < tc.appCh; c++ {
					want := int16(i%20000) + int16(c)*2
					if tc.appCh == 1 && devCh > 1 {
						sum := 0
						for d := range devCh {
							sum += int(int16(i%20000) + int16(d)*2)
						}
						want = int16(sum / devCh)
					}
					if got[i*tc.appCh+c] != want {
						t.Fatalf("frame %d channel %d = %d, want %d", i, c, got[i*tc.appCh+c], want)
					}
				}
			}
		})
	}
}

func TestALSAResample(t *testing.T) {
	noLeaks(t)
	f := newFakeALSA(t)
	hw := fakeLoopback()
	hw.rates = []uint32{44100}
	f.add(0, "Loopback", "Loopback", hw, 0)
	f.gen = func(frame uint64, ch int) int16 { return int16(8000 * math.Sin(2*math.Pi*1000*float64(frame)/44100)) }
	a, sp := openALSASpy(t, f, Capture, alsaCfg(t, 48000, 1))
	p := f.pcm(t, 0)
	if p.set.rate != 44100 {
		t.Fatalf("device rate %d", p.set.rate)
	}
	for i := range 100 {
		p.tick(441)
		waitALSA(t, "capture", func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.appl == uint64(441*(i+1))
		})
	}
	waitALSA(t, "capture", func() bool { return sp.capturedLen() >= 47000 })
	out, sq := openALSASpy(t, f, Playback, alsaCfg(t, 48000, 1))
	q := f.pcm(t, 1)
	for range 100 {
		waitALSA(t, "refill", func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return q.appl-q.hwp == uint64(3*q.set.period)
		})
		q.tick(441)
	}
	closeALSASpy(a, sp)
	closeALSASpy(out, sq)
	if n := sp.capturedLen(); n < 47000 || n > 48100 {
		t.Errorf("1 s at 44.1 kHz gave %d frames at 48 kHz", n)
	}
	if n := q.playedFrames(); n < 44100 || n > 44100+3*441+1 {
		t.Errorf("played %d device frames", n)
	}
	sp.mu.Lock()
	freq := toneFreq(sp.captured[4800:], 48000)
	sp.mu.Unlock()
	if math.Abs(freq-1000) > 5 {
		t.Errorf("tone %.1f Hz after resampling", freq)
	}
}

// toneFreq estimates the frequency of a tone from its zero crossings.
func toneFreq(pcm []int16, rate int) float64 {
	first, last, n := -1, -1, 0
	for i := 1; i < len(pcm); i++ {
		if pcm[i-1] < 0 && pcm[i] >= 0 {
			if first < 0 {
				first = i
			}
			last = i
			n++
		}
	}
	if n < 2 {
		return 0
	}
	return float64(n-1) * float64(rate) / float64(last-first)
}

func TestALSAPlaybackXrun(t *testing.T) {
	noLeaks(t)
	f := loopbackFake(t)
	a, sp := openALSASpy(t, f, Playback, alsaCfg(t, 48000, 1))
	p := f.pcm(t, 0)
	waitALSA(t, "prefill", func() bool { return p.playedFrames() >= 1440 })
	p.tick(1440 + 2400) // the buffer runs dry, then 50 ms pass in XRUN
	waitALSA(t, "recovery", func() bool { return sp.xruns.Load() > 0 })
	waitALSA(t, "restart", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.state == alsaStateRunning
	})
	if x := sp.xruns.Load(); x != 2400 {
		t.Errorf("xrun frames %d, want 2400 (50 ms)", x)
	}
	if got := strings.Join(p.callLog(), " "); !strings.Contains(got, "prepare prepare") && !strings.HasSuffix(got, "prepare") {
		t.Logf("calls %s", got)
	}
	closeALSASpy(a, sp)
	sp.check(t)
	if sp.stopped.Load() != 0 {
		t.Error("an xrun is not a lost device")
	}
}

func TestALSACaptureXrun(t *testing.T) {
	noLeaks(t)
	f := loopbackFake(t)
	a, sp := openALSASpy(t, f, Capture, alsaCfg(t, 48000, 1))
	p := f.pcm(t, 0)
	p.do(func(p *fakePCM) { p.availMin = math.MaxUint32 }) // the reader stops waking up
	p.tick(5760 + 960)                                     // full buffer, then 20 ms in XRUN
	p.do(func(p *fakePCM) { p.availMin = 480 })
	waitALSA(t, "recovery", func() bool { return sp.xruns.Load() > 0 })
	if x := sp.xruns.Load(); x != 5760+960 {
		t.Errorf("xrun frames %d, want buffer 5760 plus 960", x)
	}
	waitALSA(t, "restart", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.state == alsaStateRunning
	})
	p.tick(480)
	waitALSA(t, "capture after xrun", func() bool { return sp.capturedLen() >= 480 })
	closeALSASpy(a, sp)
	sp.check(t)
}

func TestALSAXrunFromTransfer(t *testing.T) {
	noLeaks(t)
	f := loopbackFake(t)
	a, sp := openALSASpy(t, f, Playback, alsaCfg(t, 48000, 1))
	p := f.pcm(t, 0)
	waitALSA(t, "prefill", func() bool { return p.playedFrames() >= 1440 })
	p.do(func(p *fakePCM) { p.errs[alsaIoctlWriteI] = unix.EPIPE; p.state = alsaStateXrun; p.trigger = p.clock })
	p.do(func(p *fakePCM) { p.state = alsaStateRunning; p.hwp += 960 })
	waitALSA(t, "prepare after EPIPE", func() bool { return strings.Count(strings.Join(p.callLog(), " "), "prepare") >= 2 })
	p.do(func(p *fakePCM) { delete(p.errs, alsaIoctlWriteI) })
	closeALSASpy(a, sp)
	sp.check(t)
	if sp.stopped.Load() != 0 {
		t.Error("EPIPE is not a lost device")
	}
}

func TestALSASuspend(t *testing.T) {
	saved := alsaResumeRetry
	alsaResumeRetry = time.Millisecond
	t.Cleanup(func() { alsaResumeRetry = saved })
	for _, noResume := range []bool{false, true} {
		t.Run(fmt.Sprint("noResume=", noResume), func(t *testing.T) {
			noLeaks(t)
			f := loopbackFake(t)
			a, sp := openALSASpy(t, f, Capture, alsaCfg(t, 48000, 1))
			p := f.pcm(t, 0)
			p.tick(480)
			waitALSA(t, "capture", func() bool { return sp.capturedLen() >= 480 })
			p.suspend(3, noResume)
			waitALSA(t, "resume", func() bool {
				p.mu.Lock()
				defer p.mu.Unlock()
				return p.state == alsaStateRunning
			})
			log := strings.Join(p.callLog(), " ")
			if noResume != strings.HasSuffix(log, "prepare start") || !noResume && !strings.HasSuffix(log, "resume") {
				t.Errorf("calls %s", log)
			}
			p.tick(480)
			waitALSA(t, "capture after resume", func() bool { return sp.capturedLen() >= 960 })
			closeALSASpy(a, sp)
			sp.check(t)
			if sp.stopped.Load() != 0 {
				t.Error("a suspend is not a lost device")
			}
		})
	}
}

func TestALSASuspendThenClose(t *testing.T) {
	noLeaks(t)
	f := loopbackFake(t)
	a, sp := openALSASpy(t, f, Playback, alsaCfg(t, 48000, 1))
	p := f.pcm(t, 0)
	waitALSA(t, "prefill", func() bool { return p.playedFrames() >= 1440 })
	p.suspend(1<<30, false)
	waitALSA(t, "resume attempts", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.resumeWait < 1<<30
	})
	start := time.Now()
	closeALSASpy(a, sp)
	if d := time.Since(start); d > time.Second/2 {
		t.Errorf("close took %v while waiting for resume", d)
	}
}

func TestALSALoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(p *fakePCM)
	}{
		{"disconnect", (*fakePCM).disconnect},
		{"POLLHUP", func(p *fakePCM) { p.do(func(p *fakePCM) { p.pollBits = unix.POLLHUP }) }},
		{"POLLNVAL", func(p *fakePCM) { p.do(func(p *fakePCM) { p.pollBits = unix.POLLNVAL }) }},
		{"poll error", func(p *fakePCM) { p.do(func(p *fakePCM) { p.pollErr = unix.EBADF }) }},
		{"ENODEV on transfer", func(p *fakePCM) {
			p.do(func(p *fakePCM) { p.errs[alsaIoctlReadI], p.errs[alsaIoctlWriteI] = unix.ENODEV, unix.ENODEV })
			p.tick(480)
		}},
		{"EIO on status", func(p *fakePCM) { p.do(func(p *fakePCM) { p.errs[alsaIoctlStatus] = unix.EIO }); p.tick(480) }},
		{"unexpected state", func(p *fakePCM) { p.do(func(p *fakePCM) { p.state = alsaStateSetup }) }},
		{"prepare fails in xrun", func(p *fakePCM) {
			p.do(func(p *fakePCM) { p.errs[alsaIoctlPrepare] = unix.ENODEV })
			p.xrun()
		}},
	} {
		for _, kind := range []Kind{Capture, Playback} {
			t.Run(tc.name+" "+kind.String(), func(t *testing.T) {
				noLeaks(t)
				f := loopbackFake(t)
				a, sp := openALSASpy(t, f, kind, alsaCfg(t, 48000, 1))
				p := f.pcm(t, 0)
				p.tick(480)
				tc.lose(p)
				waitALSA(t, "onStopped", func() bool { return sp.stopped.Load() > 0 })
				p.tick(480)
				time.Sleep(5 * time.Millisecond)
				closeALSASpy(a, sp)
				sp.check(t)
				if n := sp.stopped.Load(); n != 1 {
					t.Errorf("onStopped %d times", n)
				}
				log := p.callLog()
				if log[len(log)-1] != "close" {
					t.Errorf("calls %v", log)
				}
			})
		}
	}
}

func TestALSAStop(t *testing.T) {
	noLeaks(t)
	f := loopbackFake(t)
	a, sp := openALSASpy(t, f, Playback, alsaCfg(t, 48000, 1))
	a.stop()
	if sp.stopped.Load() != 1 {
		t.Errorf("onStopped %d", sp.stopped.Load())
	}
	a.stop()
	closeALSASpy(a, sp)
	a.stop()
	a.close()
	sp.check(t)
	if n := sp.stopped.Load(); n != 1 {
		t.Errorf("onStopped %d times", n)
	}
}

func TestALSACloseSilencesCallbacks(t *testing.T) {
	noLeaks(t)
	f := newFakeALSA(t)
	f.add(0, "Loopback", "Loopback", fakeLoopback(), 0)
	for i := range 50 {
		kind := []Kind{Capture, Playback}[i%2]
		cfg := alsaCfg(t, 48000, 2)
		cfg.Period = time.Millisecond
		sp := &alsaSpy{spy: &spy{ch: 2}}
		a, err := alsaOpenSys(f, sp, kind, cfg)
		if err != nil {
			t.Fatal(err)
		}
		p := f.pcm(t, i)
		p.autoTick()
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		if i%3 == 0 {
			p.disconnect()
		}
		closeALSASpy(a, sp)
		p.stopTicks()
		time.Sleep(time.Millisecond)
		sp.check(t)
		log := p.callLog()
		if n := len(log); n < 2 || log[n-1] != "close" || log[n-2] != "drop" && !p.gone {
			t.Fatalf("round %d: calls %v", i, log)
		}
	}
}

func TestALSAPartialWrite(t *testing.T) {
	noLeaks(t)
	f := loopbackFake(t)
	a, sp := openALSASpy(t, f, Playback, alsaCfg(t, 48000, 1))
	p := f.pcm(t, 0)
	waitALSA(t, "prefill", func() bool { return p.playedFrames() >= 1440 })
	p.do(func(p *fakePCM) { p.writeCap = 100 }) // the device takes less than asked
	p.tick(480)
	waitALSA(t, "partial", func() bool { return p.playedFrames() >= 1540 })
	p.tick(480)
	waitALSA(t, "partial", func() bool { return p.playedFrames() >= 1640 })
	p.do(func(p *fakePCM) { p.writeCap = 0 })
	for i := range 4 {
		p.tick(480)
		waitALSA(t, "rest", func() bool { return p.playedFrames() >= 1440+480*(i+3) })
	}
	closeALSASpy(a, sp)
	got := decodePlayed(p)
	for i, v := range got {
		if v != int16(i+1) {
			t.Fatalf("frame %d = %d: a partial write lost or repeated frames", i, v)
		}
	}
}

func TestALSAConvert(t *testing.T) {
	cases := []struct {
		f    alsaFormat
		dev  []byte
		want []int16
	}{
		{alsaS16, []byte{0x34, 0x12, 0x00, 0x80}, []int16{0x1234, -32768}},
		{alsaS24_3, []byte{0x80, 0x34, 0x12, 0x7f, 0xff, 0x7f, 0x00, 0x00, 0x80, 0x7f, 0xff, 0xff}, []int16{0x1235, 32767, -32768, -1}},
		{alsaS24, []byte{0x80, 0x34, 0x12, 0x00, 0x00, 0x00, 0x80, 0xff, 0xff, 0xff, 0xff, 0xab}, []int16{0x1235, -32768, 0}},
		{alsaS32, []byte{0x00, 0x80, 0x34, 0x12, 0xff, 0xff, 0xff, 0x7f, 0x00, 0x00, 0x00, 0x80}, []int16{0x1235, 32767, -32768}},
	}
	for _, tc := range cases {
		got := make([]int16, len(tc.want))
		alsaDecode(tc.f, tc.dev, got)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s decode %v, want %v", tc.f.name, got, tc.want)
		}
	}
	fl := make([]byte, 24)
	for i, v := range []float32{0.5, -1, 2, float32(math.NaN()), float32(math.Inf(-1)), 1.0 / 65536} {
		binary.LittleEndian.PutUint32(fl[4*i:], math.Float32bits(v))
	}
	got := make([]int16, 6)
	alsaDecode(alsaFloat, fl, got)
	if fmt.Sprint(got) != fmt.Sprint([]int16{16384, -32768, 32767, 0, -32768, 1}) {
		t.Errorf("float decode %v", got)
	}
	for _, f := range alsaFormats {
		src := []int16{0, 1, -1, 12345, -32768, 32767}
		b := make([]byte, len(src)*f.bytes)
		alsaEncode(f, src, b)
		back := make([]int16, len(src))
		alsaDecode(f, b, back)
		if fmt.Sprint(back) != fmt.Sprint(src) {
			t.Errorf("%s round trip %v", f.name, back)
		}
	}
	b := make([]byte, 6)
	alsaEncode(alsaS24_3, []int16{0x1234, -2}, b)
	if !bytes.Equal(b, []byte{0x00, 0x34, 0x12, 0x00, 0xfe, 0xff}) {
		t.Errorf("s24_3le encode % x", b)
	}
	b = make([]byte, 8)
	alsaEncode(alsaS24, []int16{-2, 1}, b)
	if !bytes.Equal(b, []byte{0x00, 0xfe, 0xff, 0xff, 0x00, 0x01, 0x00, 0x00}) {
		t.Errorf("s24_le encode % x", b)
	}
	for _, tc := range []struct {
		from, to int
		src      []int16
		want     []int16
	}{
		{2, 2, []int16{1, 2}, []int16{1, 2}},
		{1, 3, []int16{5}, []int16{5, 5, 5}},
		{3, 1, []int16{3, 6, 9}, []int16{6}},
		{2, 4, []int16{1, 2, 3, 4}, []int16{1, 2, 0, 0, 3, 4, 0, 0}},
		{6, 2, []int16{1, 2, 3, 4, 5, 6}, []int16{1, 2}},
	} {
		dst := make([]int16, len(tc.src)/tc.from*tc.to)
		alsaMap(dst, tc.src, tc.from, tc.to)
		if fmt.Sprint(dst) != fmt.Sprint(tc.want) {
			t.Errorf("%d to %d: %v, want %v", tc.from, tc.to, dst, tc.want)
		}
	}
}

// TestALSABackendOrder checks that a working PulseAudio server is always
// used before ALSA, ALSA is used without one, and a server that fails is
// reported instead of falling back.
func TestALSABackendOrder(t *testing.T) {
	saved := alsaSystem
	t.Cleanup(func() { alsaSystem = saved })
	f := loopbackFake(t)
	alsaSystem = f
	dir := t.TempDir()
	t.Setenv("PULSE_COOKIE", "")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	srv := startFake(t)
	t.Setenv("PULSE_SERVER", "unix:"+srv.addr)
	devs, err := hwList(Playback)
	if err != nil || len(devs) != 2 || strings.HasPrefix(hexText(t, devs[0].ID), "hw:") {
		t.Errorf("with a server: %v %v", devs, err)
	}
	st := &stream{kind: Playback, format: Format{48000, 1}, buf: ring{data: make([]int16, 4800)}, signal: make(chan struct{}, 1), done: make(chan struct{})}
	hw, err := hwOpen(st, alsaCfg(t, 48000, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hw.(*pulseStream); !ok {
		t.Errorf("opened %T with a server running", hw)
	}
	hw.close()

	refusing := startFake(t)
	refusing.cookie = bytes.Repeat([]byte{1}, 256)
	t.Setenv("PULSE_SERVER", "unix:"+refusing.addr)
	if _, err := hwList(Playback); err == nil || !errors.Is(err, ErrBackend) {
		t.Errorf("a refusing server must be reported, not skipped: %v", err)
	}
	if _, err := hwOpen(st, alsaCfg(t, 48000, 1)); err == nil || !errors.Is(err, ErrBackend) {
		t.Errorf("a refusing server must be reported on open: %v", err)
	}

	t.Setenv("PULSE_SERVER", "unix:"+dir+"/none")
	devs, err = hwList(Playback)
	if err != nil || len(devs) != 2 || hexText(t, devs[0].ID) != "hw:CARD=Loopback,DEV=0" {
		t.Errorf("without a server: %v %v", devs, err)
	}
	f.listErr = &fs.PathError{Op: "open", Path: alsaDir, Err: fs.ErrNotExist}
	_, err = hwList(Capture)
	if !errors.Is(err, ErrNoDevice) || !strings.Contains(err.Error(), "no PulseAudio protocol server") || !strings.HasSuffix(err.Error(), " and no ALSA devices in /dev/snd") {
		t.Errorf("neither: %v", err)
	}
}

// TestALSAEndToEnd runs the public API on the fake ALSA devices with no
// sound server.
func TestALSAEndToEnd(t *testing.T) {
	noLeaks(t)
	saved := alsaSystem
	t.Cleanup(func() { alsaSystem = saved })
	f := loopbackFake(t)
	f.tune = func(p *fakePCM) { go func() { waitInstalled(p); p.autoTick() }() }
	alsaSystem = f
	dir := t.TempDir()
	t.Setenv("PULSE_SERVER", "unix:"+dir+"/none")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	nullBackend.Store(false)
	t.Cleanup(func() { nullBackend.Store(true) })

	devs, err := Devices(Playback)
	if err != nil || len(devs) != 2 || !devs[0].Default {
		t.Fatalf("Devices = %v, %v", devs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := OpenOutput(Config{DeviceID: devs[1].ID, Format: Format{48000, 2}})
	if err != nil {
		t.Fatal(err)
	}
	tone := make([]int16, 9600)
	if _, err := out.Write(ctx, tone); err != nil {
		t.Fatal(err)
	}
	waitALSA(t, "frames", func() bool { return out.Stats().Frames > 4800 })
	p := f.pcm(t, 0)
	if p.node.dev != 1 {
		t.Errorf("opened device %d", p.node.dev)
	}
	p.stopTicks()
	time.Sleep(20 * time.Millisecond)
	u0 := out.Stats().Underruns
	p.do(func(p *fakePCM) { p.state, p.trigger, p.clock = alsaStateXrun, p.clock, p.clock+100*time.Millisecond })
	waitALSA(t, "underrun counted", func() bool { return out.Stats().Underruns-u0 >= 4800 })
	if u := out.Stats().Underruns - u0; u > 4800+1440 {
		t.Errorf("a 100 ms xrun counted %d frames", u)
	}
	StopDevice(out)
	if _, err := out.Write(ctx, tone); !errors.Is(err, ErrDeviceLost) {
		t.Errorf("Write after stop = %v", err)
	}
	out.Close()

	in, err := OpenInput(Config{})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int16, 480)
	if n, err := in.Read(ctx, buf); err != nil || n == 0 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	f.pcm(t, 1).disconnect()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := in.Read(ctx, buf)
		if errors.Is(err, ErrDeviceLost) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("Read after removal = %v", err)
		}
	}
	in.Close()
	if n := LiveDevices(); n != 0 {
		t.Errorf("%d devices live", n)
	}
}

func waitInstalled(p *fakePCM) {
	for {
		p.mu.Lock()
		ok := p.state == alsaStateRunning || p.state == alsaStatePrepared && p.installed
		p.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func FuzzALSAHWParams(f *testing.F) {
	b := make([]byte, alsaHWParamsSize)
	p := alsaAny()
	p.encode(b)
	f.Add(b)
	f.Add(make([]byte, alsaHWParamsSize))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < alsaHWParamsSize {
			b = append(b, make([]byte, alsaHWParamsSize-len(b))...)
		}
		var p, q alsaHWParams
		p.decode(b)
		out := make([]byte, alsaHWParamsSize)
		p.encode(out)
		q.decode(out)
		if q != p {
			t.Fatalf("decode is not stable:\n%+v\n%+v", p, q)
		}
		for k := range p.intervals {
			iv := p.intervals[k]
			if !iv.empty && (iv.first() < iv.min || iv.last() > iv.max) {
				t.Fatalf("interval %+v: first %d last %d", iv, iv.first(), iv.last())
			}
		}
		for _, f := range alsaFormatList {
			if s, err := alsaInstalled(p, f); err == nil && (s.channels < 1 || s.channels > MaxChannels || s.period < 1 || s.buffer < s.period) {
				t.Fatalf("accepted %+v", s)
			}
		}
		alsaFormatNames(p.masks[alsaParamFormat], nil)
		c := fakeBuild(fakeLoopback())
		c.refine(&p)
	})
}

func FuzzALSANames(f *testing.F) {
	for _, s := range []string{"pcmC0D0p", "pcmC12D3c", "controlC0", "pcmC00D0p", alsaID("Loopback", 0), "Loopback\x00x", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if n, ok := alsaParseNode(s); ok {
			if n.file() != s {
				t.Fatalf("%q parsed to %+v, prints %q", s, n, n.file())
			}
		}
		if card, dev, ok := alsaParseID(s); ok {
			if alsaID(card, dev) != strings.ToLower(s) {
				t.Fatalf("%q parsed to %q %d", s, card, dev)
			}
		}
		if card, dev, ok := alsaParseID(hexEncode(s)); ok && alsaID(card, dev) != hexEncode(s) {
			t.Fatalf("%q parsed to %q %d", s, card, dev)
		}
		if alsaValidCardID(s) {
			if _, _, ok := alsaParseID(alsaID(s, 7)); !ok {
				t.Fatalf("valid card id %q does not round trip", s)
			}
		}
		name := alsaCString([]byte(s))
		if strings.ContainsAny(name, "\x00\n\r\t") || !strings.EqualFold(name, strings.ToValidUTF8(name, "")) && !strings.Contains(name, "�") {
			t.Fatalf("name %q from %q", name, s)
		}
	})
}

func hexText(t *testing.T, id string) string {
	t.Helper()
	b, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hexEncode(s string) string { return hex.EncodeToString([]byte(s)) }

// The tests below use the real kernel: the snd-aloop loopback card, never
// any other card. They run only with KOLO_AUDIO_ALSA_TEST=1 (see
// build/validate/linux-alsa).
func realLoopback(t *testing.T) (play, rec string) {
	t.Helper()
	if os.Getenv("KOLO_AUDIO_ALSA_TEST") != "1" {
		t.Skip("set KOLO_AUDIO_ALSA_TEST=1 on a machine with snd-aloop loaded")
	}
	t.Setenv("PULSE_SERVER", "unix:"+t.TempDir()+"/none")
	nullBackend.Store(false)
	t.Cleanup(func() { nullBackend.Store(true) })
	pcms, err := alsaScan(alsaKernel{}, Playback)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pcms {
		if p.cardID == "Loopback" {
			return alsaID("Loopback", 0), alsaID("Loopback", 1)
		}
	}
	t.Fatalf("no Loopback card in %v", pcms)
	return "", ""
}

func TestALSARealEnumerate(t *testing.T) {
	play, rec := realLoopback(t)
	for _, kind := range []Kind{Playback, Capture} {
		devs, err := Devices(kind)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, d := range devs {
			t.Logf("%v: %q default=%v id=%s (%s)", kind, d.Name, d.Default, d.ID, hexText(t, d.ID))
			if d.ID == play || d.ID == rec {
				found++
			}
		}
		if found != 2 {
			t.Errorf("%v: loopback devices not listed", kind)
		}
	}
}

// TestALSARealNegotiateMatchesFake runs the negotiation on the loopback
// card and on the fake kernel's model of it, and compares every answer,
// including the whole refined configuration space.
func TestALSARealNegotiateMatchesFake(t *testing.T) {
	realLoopback(t)
	ms := time.Millisecond
	for _, capture := range []bool{false, true} {
		node := alsaNode{card: -1, dev: 0, capture: capture}
		pcms, err := alsaScan(alsaKernel{}, map[bool]Kind{false: Playback, true: Capture}[capture])
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pcms {
			if p.cardID == "Loopback" && p.dev == 0 {
				node = p.alsaNode
			}
		}
		d, err := alsaKernel{}.open(node.file(), true)
		if err != nil {
			t.Fatal(err)
		}
		f := newFakeALSA(t)
		f.add(0, "Loopback", "Loopback", fakeLoopback(), 0)
		fd, _ := f.open(alsaNode{0, 0, capture}.file(), true)
		for _, req := range []alsaHWParams{alsaAny(), func() alsaHWParams {
			p := alsaAny()
			p.masks[alsaParamFormat].single(alsaS24_3.code)
			*p.iv(alsaParamChannels) = alsaInterval{min: 3, max: 3}
			*p.iv(alsaParamRate) = alsaInterval{min: 44100, max: 44100}
			return p
		}(), func() alsaHWParams {
			p := alsaAny()
			*p.iv(alsaParamPeriodTime) = alsaInterval{min: 10000, max: 10000}
			*p.iv(alsaParamBufferTime) = alsaInterval{min: 0, max: 120000}
			return p
		}()} {
			kp, fp := req, req
			kerr, ferr := kp.call(d, alsaIoctlHWRefine), fp.call(fd, alsaIoctlHWRefine)
			if (kerr == nil) != (ferr == nil) || kp.masks != fp.masks || kp.intervals != fp.intervals {
				t.Errorf("refine differs (capture %v):\nkernel %v %+v\nfake   %v %+v", capture, kerr, kp, ferr, fp)
			}
		}
		for _, formats := range [][]alsaFormat{alsaFormats, {alsaS24_3}, {alsaS24}, {alsaS32}, {alsaFloat}} {
			for _, fm := range []Format{{48000, 2}, {44100, 1}, {8000, 1}, {96000, 6}, {22050, 2}, {384000, 2}} {
				for _, pb := range [][2]time.Duration{{10 * ms, 120 * ms}, {5 * ms, 20 * ms}, {20 * ms, 2 * time.Second}, {ms, ms}} {
					ks, kerr := alsaNegotiate(d, fm, pb[0], pb[1], formats)
					fset, ferr := alsaNegotiate(fd, fm, pb[0], pb[1], formats)
					if ks != fset || (kerr == nil) != (ferr == nil) {
						t.Errorf("%s %+v %v: kernel %+v %v, fake %+v %v", formats[0].name, fm, pb, ks, kerr, fset, ferr)
					}
				}
			}
		}
		t.Logf("capture=%v: %d negotiations agree", capture, 5*6*4)
		d.close()
		fd.close()
	}
}

type loopCase struct {
	name      string
	format    alsaFormat
	play, rec Format
}

func TestALSARealLoopback(t *testing.T) {
	play, rec := realLoopback(t)
	for _, tc := range []loopCase{
		{name: "s16 stereo 48k", format: alsaS16, play: Format{48000, 2}, rec: Format{48000, 2}},
		{name: "s16 mono 48k", format: alsaS16, play: Format{48000, 1}, rec: Format{48000, 1}},
		{name: "s24_3le stereo to mono", format: alsaS24_3, play: Format{48000, 2}, rec: Format{48000, 1}},
		{name: "s32 44.1k resampled to 48k", format: alsaS32, play: Format{44100, 2}, rec: Format{48000, 2}},
		{name: "s24_le mono 16k", format: alsaS24, play: Format{16000, 1}, rec: Format{16000, 1}},
		{name: "float 96k to 48k mono", format: alsaFloat, play: Format{96000, 2}, rec: Format{48000, 1}},
		{name: "s16 mono 8k", format: alsaS16, play: Format{8000, 1}, rec: Format{8000, 1}},
		{name: "s16 22.05k mono to stereo", format: alsaS16, play: Format{22050, 1}, rec: Format{22050, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) { realLoop(t, tc, play, rec) })
	}
}

func realLoop(t *testing.T, tc loopCase, playID, recID string) {
	saved := alsaFormats
	alsaFormats = []alsaFormat{tc.format}
	out, err := OpenOutput(Config{DeviceID: playID, Format: tc.play})
	alsaFormats = saved
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, err := OpenInput(Config{DeviceID: recID, Format: tc.rec})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	ps, rs := out.s.hw.(*alsaStream).set, in.s.hw.(*alsaStream).set
	t.Logf("playback device %s %d ch %d Hz period %d buffer %d; capture device %s %d ch %d Hz period %d buffer %d",
		ps.format.name, ps.channels, ps.rate, ps.period, ps.buffer, rs.format.name, rs.channels, rs.rate, rs.period, rs.buffer)
	if ps.format != tc.format || rs.format != tc.format || rs.rate != ps.rate || rs.channels != ps.channels {
		t.Errorf("the capture side must follow the loopback cable's format")
	}
	const freq, seconds = 997.0, 2
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// A stall of the whole machine (a busy VM host) shows as late timers.
	var stall atomic.Int64
	probeDone := make(chan struct{})
	defer close(probeDone)
	go func() {
		for {
			t0 := time.Now()
			select {
			case <-probeDone:
				return
			case <-time.After(time.Millisecond):
			}
			if d := int64(time.Since(t0)); d > stall.Load() {
				stall.Store(d)
			}
		}
	}()
	var got []int16
	readDone := make(chan error, 1)
	go func() {
		buf := make([]int16, 480*tc.rec.Channels)
		for len(got) < (seconds+1)*tc.rec.SampleRate*tc.rec.Channels {
			n, err := in.Read(ctx, buf)
			if err != nil {
				readDone <- err
				return
			}
			got = append(got, buf[:n]...)
		}
		readDone <- nil
	}()
	chunk := tc.play.SampleRate / 100
	pcm := make([]int16, chunk*tc.play.Channels)
	for i := 0; i < seconds*100; i++ {
		for j := range chunk {
			v := int16(12000 * math.Sin(2*math.Pi*freq*float64(i*chunk+j)/float64(tc.play.SampleRate)))
			for c := range tc.play.Channels {
				pcm[j*tc.play.Channels+c] = v
			}
		}
		if _, err := out.Write(ctx, pcm); err != nil {
			t.Fatal(err)
		}
	}
	playStats := out.Stats()
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	recStats := in.Stats()
	t.Logf("output %+v, input %+v, longest 1 ms timer %v", playStats, recStats, time.Duration(stall.Load()))
	if playStats.Underruns != 0 {
		t.Errorf("output underruns %d before the last write", playStats.Underruns)
	}
	if recStats.Overruns != 0 {
		t.Errorf("input overruns %d", recStats.Overruns)
	}
	ch := tc.rec.Channels
	mono := make([]int16, len(got)/ch)
	for i := range mono {
		mono[i] = got[i*ch]
	}
	start := 0
	for start < len(mono) && math.Abs(float64(mono[start])) < 2000 {
		start++
	}
	window := mono[start:min(len(mono), start+tc.rec.SampleRate)]
	if len(window) < tc.rec.SampleRate*9/10 {
		t.Fatalf("tone found in %d of %d frames", len(window), len(mono))
	}
	f := toneFreq(window[tc.rec.SampleRate/20:], tc.rec.SampleRate)
	peak, _ := Level(window[tc.rec.SampleRate/20:])
	t.Logf("recorded %d frames, tone at frame %d: %.2f Hz, peak %.2f dBFS", len(mono), start, f, peak)
	if math.Abs(f-freq) > freq*0.005 {
		t.Errorf("tone %.2f Hz, want %.0f", f, freq)
	}
	if want := 20 * math.Log10(12000.0/32768); math.Abs(peak-want) > 1 {
		t.Errorf("peak %.2f dBFS, want %.2f", peak, want)
	}
	if ch == 2 {
		for i := 0; i+1 < len(got); i += 2 {
			if got[i] != got[i+1] {
				t.Fatalf("frame %d: channels differ (%d, %d)", i/2, got[i], got[i+1])
			}
		}
	}
}

// TestALSARealRemoval keeps a playback and a capture stream running on the
// loopback card until the card is removed (the validation script unbinds
// the driver once this test prints that it runs). Both streams must fail
// with ErrDeviceLost.
func TestALSARealRemoval(t *testing.T) {
	play, rec := realLoopback(t)
	if os.Getenv("KOLO_AUDIO_ALSA_REMOVAL") != "1" {
		t.Skip("set KOLO_AUDIO_ALSA_REMOVAL=1 and remove the loopback card while it runs")
	}
	out, err := OpenOutput(Config{DeviceID: play, Format: Format{48000, 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, err := OpenInput(Config{DeviceID: rec, Format: Format{48000, 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	readErr := make(chan error, 1)
	go func() {
		buf := make([]int16, 960)
		for {
			if _, err := in.Read(ctx, buf); err != nil {
				readErr <- err
				return
			}
		}
	}()
	fmt.Println("kolo-alsa: streams running")
	pcm := make([]int16, 960)
	var writeErr error
	for writeErr == nil {
		_, writeErr = out.Write(ctx, pcm)
	}
	start := time.Now()
	rerr := <-readErr
	t.Logf("write: %v; read: %v (%v after the write failed)", writeErr, rerr, time.Since(start))
	if !errors.Is(writeErr, ErrDeviceLost) || !errors.Is(rerr, ErrDeviceLost) {
		t.Errorf("want ErrDeviceLost from both")
	}
}

// installed builds the configuration a kernel reports after HW_PARAMS.
func installed(f alsaFormat, channels, rate, period, buffer uint32) alsaHWParams {
	p := alsaAny()
	p.masks[alsaParamFormat].single(f.code)
	for k, v := range map[int]uint32{alsaParamChannels: channels, alsaParamRate: rate, alsaParamPeriodSize: period, alsaParamBufferSize: buffer} {
		*p.iv(k) = alsaInterval{min: v, max: v, integer: true}
	}
	return p
}

// TestALSAInstalledBounds checks that a configuration the kernel installs beyond the library's
// bounds is refused, whatever the negotiation was.
func TestALSAInstalledBounds(t *testing.T) {
	f := alsaFormatList[0]
	maxBuffer := uint32((Format{SampleRate: 48000}).frames(MaxBuffer))
	for name, c := range map[string]struct {
		p  alsaHWParams
		ok bool
	}{
		"largest buffer":      {installed(f, 2, 48000, 480, maxBuffer), true},
		"buffer over":         {installed(f, 2, 48000, 480, maxBuffer+1), false},
		"buffer far over":     {installed(f, 2, 48000, 480, 1<<30), false},
		"buffer below period": {installed(f, 2, 48000, 480, 479), false},
		"no channels":         {installed(f, 0, 48000, 480, 4800), false},
		"too many channels":   {installed(f, MaxChannels+1, 48000, 480, 4800), false},
		"no period":           {installed(f, 2, 48000, 0, 4800), false},
		"no rate":             {installed(f, 2, 0, 480, 4800), false},
	} {
		_, err := alsaInstalled(c.p, f)
		if c.ok && err != nil || !c.ok && !errors.Is(err, ErrBackend) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestALSACaptureDenied(t *testing.T) {
	saved := linuxBackends
	t.Cleanup(func() { linuxBackends = saved; nullBackend.Store(true) })
	linuxBackends = []linuxBackend{{
		list: func(Kind) ([]Device, error) { return nil, nil },
		open: func(*stream, Config) (hwStream, error) { return nil, alsaErr("open pcmC0D0c", unix.EACCES) },
	}}
	nullBackend.Store(false)
	_, err := OpenInput(Config{})
	if !errors.Is(err, ErrPermission) || !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("OpenInput: %v", err)
	}
	_, err = OpenOutput(Config{})
	if errors.Is(err, ErrPermission) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("OpenOutput: %v", err)
	}
}
