//go:build darwin && (amd64 || arm64)

package koloaudio

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

type caFakeDevice struct {
	uid, name string
	in, out   int
	dead      bool
	rate      int
}

type caFakeListener struct {
	obj, sel uint32
	fn       func()
}

type caFakeAPI struct {
	mu        sync.Mutex
	order     []uint32
	devs      map[uint32]*caFakeDevice
	defIn     uint32
	defOut    uint32
	listeners map[int]caFakeListener
	nextL     int
	units     []*caFakeUnit
	unitErr   error
	startErr  error
}

func newCAFakeAPI(devs ...*caFakeDevice) *caFakeAPI {
	f := &caFakeAPI{devs: map[uint32]*caFakeDevice{}, listeners: map[int]caFakeListener{}}
	for i, d := range devs {
		id := uint32(100 + i)
		f.order = append(f.order, id)
		f.devs[id] = d
	}
	return f
}

func (f *caFakeAPI) dev(id uint32) *caFakeDevice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devs[id]
}

func (f *caFakeAPI) devices() ([]uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.order), nil
}

func (f *caFakeAPI) defaultDevice(kind Kind) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == Capture {
		return f.defIn
	}
	return f.defOut
}

func (f *caFakeAPI) setDefault(kind Kind, id uint32) {
	f.mu.Lock()
	if kind == Capture {
		f.defIn = id
	} else {
		f.defOut = id
	}
	f.mu.Unlock()
	f.fire(caSystemObject, caDefaultSelector(kind))
}

func (f *caFakeAPI) uid(id uint32) (string, error) {
	d := f.dev(id)
	if d == nil || d.uid == "" {
		return "", errors.New("no uid")
	}
	return d.uid, nil
}

func (f *caFakeAPI) name(id uint32) (string, error) {
	if d := f.dev(id); d != nil {
		return d.name, nil
	}
	return "", errors.New("no name")
}

func (f *caFakeAPI) channels(id uint32, kind Kind) int {
	d := f.dev(id)
	switch {
	case d == nil:
		return 0
	case kind == Capture:
		return d.in
	}
	return d.out
}

func (f *caFakeAPI) alive(id uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devs[id]
	return d != nil && !d.dead
}

func (f *caFakeAPI) nominalRate(id uint32) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.devs[id]; d != nil {
		return d.rate, nil
	}
	return 0, errors.New("no device")
}

func (f *caFakeAPI) update(id uint32, sel uint32, fn func(*caFakeDevice)) {
	f.mu.Lock()
	fn(f.devs[id])
	f.mu.Unlock()
	f.fire(id, sel)
}

func (f *caFakeAPI) listen(obj, sel uint32, fn func()) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextL++
	id := f.nextL
	f.listeners[id] = caFakeListener{obj, sel, fn}
	return func() {
		f.mu.Lock()
		delete(f.listeners, id)
		f.mu.Unlock()
	}, nil
}

func (f *caFakeAPI) fire(obj, sel uint32) {
	f.mu.Lock()
	var fns []func()
	for _, l := range f.listeners {
		if l.obj == obj && l.sel == sel {
			fns = append(fns, l.fn)
		}
	}
	f.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (f *caFakeAPI) listenerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listeners)
}

func (f *caFakeAPI) listeningOn(obj uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.listeners {
		if l.obj == obj {
			return true
		}
	}
	return false
}

func (f *caFakeAPI) newUnit(kind Kind, dev uint32, period time.Duration, fn unitFunc) (audioUnit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unitErr != nil {
		return nil, f.unitErr
	}
	d := f.devs[dev]
	if d == nil || d.dead {
		return nil, caStatusError("AudioUnitSetProperty(CurrentDevice)", int32(caFourCC("!dev")))
	}
	ch := d.in
	if kind == Playback {
		ch = d.out
	}
	u := &caFakeUnit{dev: dev, rate: d.rate, ch: ch, fn: fn, startErr: f.startErr, period: period}
	f.units = append(f.units, u)
	return u, nil
}

func (f *caFakeAPI) unit(i int) *caFakeUnit {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.units) {
		return f.units[i]
	}
	return nil
}

func (f *caFakeAPI) unitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.units)
}

func (f *caFakeAPI) openUnits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.units {
		if !u.closed.Load() {
			n++
		}
	}
	return n
}

type caFakeUnit struct {
	dev      uint32
	rate, ch int
	fn       unitFunc
	period   time.Duration
	startErr error
	started  atomic.Bool
	stopped  atomic.Bool
	closed   atomic.Bool
}

func (u *caFakeUnit) format() (int, int) { return u.rate, u.ch }

func (u *caFakeUnit) start() error {
	if u.startErr != nil {
		return u.startErr
	}
	u.started.Store(true)
	return nil
}

func (u *caFakeUnit) stop()  { u.stopped.Store(true) }
func (u *caFakeUnit) close() { u.closed.Store(true) }

func caTwoDevices() (*caFakeAPI, uint32, uint32) {
	f := newCAFakeAPI(
		&caFakeDevice{uid: "A", name: "Device A", in: 2, out: 2, rate: 48000},
		&caFakeDevice{uid: "B", name: "Device B", in: 1, out: 1, rate: 44100},
	)
	f.defIn, f.defOut = 100, 100
	return f, 100, 101
}

func caTestStream(kind Kind, f Format) *stream {
	return &stream{
		kind: kind, format: f, stall: time.Minute,
		buf:  ring{data: make([]int16, f.SampleRate*f.Channels)},
		done: make(chan struct{}), signal: make(chan struct{}, 1),
		epoch: time.Now(), quit: make(chan struct{}), exited: make(chan struct{}),
	}
}

func caTestConfig(f Format) Config {
	return Config{Format: f, Period: 10 * time.Millisecond, Buffer: time.Second, StallTimeout: time.Minute}
}

func caOpenFake(t *testing.T, api *caFakeAPI, kind Kind, id string, f Format) (*stream, *caStream) {
	t.Helper()
	s := caTestStream(kind, f)
	cfg := caTestConfig(f)
	cfg.DeviceID = id
	hw, err := openCA(api, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := hw.(*caStream)
	t.Cleanup(c.close)
	return s, c
}

func caEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *stream) caFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func caReadAll(t *testing.T, s *stream) []int16 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int16, s.buf.n)
	s.buf.read(out)
	return out
}

func caFill(n int, v int16) []int16 {
	p := make([]int16, n)
	for i := range p {
		p[i] = v
	}
	return p
}

// The list the fake layer below must produce, in its order.
var caGoldenCapture = []Device{
	{ID: "50484f4e452d554944", Name: "Phone Microphone"},
	{ID: "4275696c74496e4d6963726f70686f6e65446576696365", Name: "Built-in Microphone", Default: true},
	{ID: "5649525455414c2d412d554944", Name: "Virtual Device A"},
	{ID: "5649525455414c2d422d554944", Name: "Virtual Device B"},
	{ID: "7669727475616c2d6d69632d31", Name: "Virtual Microphone 1"},
	{ID: "7669727475616c2d6d69632d32", Name: "Virtual Microphone 2"},
	{ID: "6167677265676174652d31", Name: "Aggregate Device"},
}

var caGoldenPlayback = []Device{
	{ID: "4275696c74496e537065616b6572446576696365", Name: "Built-in Speakers", Default: true},
	{ID: "5649525455414c2d412d554944", Name: "Virtual Device A"},
	{ID: "5649525455414c2d422d554944", Name: "Virtual Device B"},
	{ID: "6d756c74692d6f75747075742d31", Name: "Multi-Output Device"},
}

func caWithKind(devs []Device, kind Kind) []Device {
	out := slices.Clone(devs)
	for i := range out {
		out[i].Kind = kind
	}
	return out
}

func caGoldenFake() *caFakeAPI {
	f := newCAFakeAPI(
		&caFakeDevice{uid: "PHONE-UID", name: "Phone Microphone", in: 1},
		&caFakeDevice{uid: "BuiltInMicrophoneDevice", name: "Built-in Microphone", in: 1},
		&caFakeDevice{uid: "BuiltInSpeakerDevice", name: "Built-in Speakers", out: 2},
		&caFakeDevice{uid: "VIRTUAL-A-UID", name: "Virtual Device A", in: 2, out: 2},
		&caFakeDevice{uid: "VIRTUAL-B-UID", name: "Virtual Device B", in: 2, out: 2},
		&caFakeDevice{uid: "virtual-mic-1", name: "Virtual Microphone 1", in: 1},
		&caFakeDevice{uid: "virtual-mic-2", name: "Virtual Microphone 2", in: 1},
		&caFakeDevice{uid: "aggregate-1", name: "Aggregate Device", in: 1},
		&caFakeDevice{uid: "multi-output-1", name: "Multi-Output Device", out: 2},
	)
	f.defIn, f.defOut = 101, 102
	return f
}

func TestCAListGolden(t *testing.T) {
	f := caGoldenFake()
	for kind, want := range map[Kind][]Device{Capture: caGoldenCapture, Playback: caGoldenPlayback} {
		got, err := caList(f, kind)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, caWithKind(want, kind)) {
			t.Errorf("%v devices:\n got %+v\nwant %+v", kind, got, caWithKind(want, kind))
		}
	}
}

func TestCAListSkips(t *testing.T) {
	f := newCAFakeAPI(
		&caFakeDevice{uid: "silent", name: "No channels"},
		&caFakeDevice{uid: "", name: "No uid", in: 1},
		&caFakeDevice{uid: "unnamed", in: 1},
		&caFakeDevice{uid: "ok", name: "Fine", in: 1},
	)
	got, err := caList(f, Capture)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{ID: hex.EncodeToString([]byte("unnamed")), Name: "unnamed", Kind: Capture},
		{ID: hex.EncodeToString([]byte("ok")), Name: "Fine", Kind: Capture},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got, err := caList(newCAFakeAPI(), Playback); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v; want empty non-nil", got, err)
	}
}

func TestCAStatusError(t *testing.T) {
	cases := []struct {
		st      int32
		is      error
		perm    bool
		message string
	}{
		{int32(caFourCC("!hog")), ErrBackend, true, `("!hog")`},
		{-10847, ErrBackend, true, "status -10847"},
		{int32(caFourCC("!dat")), ErrUnsupportedFormat, false, `("!dat")`},
		{-10868, ErrUnsupportedFormat, false, "status -10868"},
		{int32(caFourCC("!dev")), ErrNoDevice, false, `("!dev")`},
		{int32(caFourCC("!obj")), ErrNoDevice, false, `("!obj")`},
		{int32(caFourCC("what")), ErrBackend, false, `("what")`},
		{-50, ErrBackend, false, "status -50"},
	}
	for _, c := range cases {
		err := caStatusError("call", c.st)
		if !errors.Is(err, c.is) || errors.Is(err, fs.ErrPermission) != c.perm || !strings.Contains(err.Error(), c.message) || !strings.HasPrefix(err.Error(), "koloaudio:") {
			t.Errorf("status %d: %v", c.st, err)
		}
	}
}

func TestCAOpenResolve(t *testing.T) {
	f, _, _ := caTwoDevices()
	f.devs[f.order[1]].out = 0
	f.defIn = 0
	f.defOut = 0
	cfg := caTestConfig(Format{SampleRate: 48000, Channels: 1})
	cases := []struct {
		kind Kind
		id   string
		ok   bool
	}{
		{Capture, hex.EncodeToString([]byte("B")), true},
		{Playback, hex.EncodeToString([]byte("B")), false},
		{Capture, hex.EncodeToString([]byte("missing")), false},
		{Capture, "zz", false},
		{Capture, "", false},
	}
	for _, c := range cases {
		s := caTestStream(c.kind, cfg.Format)
		cfg.DeviceID = c.id
		hw, err := openCA(f, s, cfg)
		if c.ok {
			if err != nil {
				t.Errorf("%v %q: %v", c.kind, c.id, err)
				continue
			}
			hw.close()
			continue
		}
		if !errors.Is(err, ErrNoDevice) {
			t.Errorf("%v %q: %v, want ErrNoDevice", c.kind, c.id, err)
		}
	}
	if n := f.unitCount(); n != 1 || f.openUnits() != 0 || f.listenerCount() != 0 {
		t.Errorf("units %d open %d listeners %d after failed opens", n, f.openUnits(), f.listenerCount())
	}
}

func TestCAPeriodPassed(t *testing.T) {
	f, _, _ := caTwoDevices()
	caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 2})
	if got := f.unit(0).period; got != 10*time.Millisecond {
		t.Fatalf("period %v", got)
	}
}

func TestCACaptureChannels(t *testing.T) {
	f, _, _ := caTwoDevices()
	s, _ := caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 1})
	u := f.unit(0)
	if !u.started.Load() {
		t.Fatal("unit not started")
	}
	u.fn([]int16{100, 300, -50, -150})
	if got := caReadAll(t, s); !slices.Equal(got, []int16{200, -100}) {
		t.Fatalf("stereo to mono: %v", got)
	}

	f, _, b := caTwoDevices()
	f.defIn = b
	s, _ = caOpenFake(t, f, Capture, "", Format{SampleRate: 44100, Channels: 2})
	f.unit(0).fn([]int16{7, -8})
	if got := caReadAll(t, s); !slices.Equal(got, []int16{7, 7, -8, -8}) {
		t.Fatalf("mono to stereo: %v", got)
	}
	if st := s.getStats(); st.Frames != 2 {
		t.Fatalf("frames %d", st.Frames)
	}
}

func TestCAUnsupportedChannels(t *testing.T) {
	f := newCAFakeAPI(&caFakeDevice{uid: "many", name: "Many", in: 8, out: 8, rate: 48000})
	f.defIn, f.defOut = 100, 100
	for _, kind := range []Kind{Capture, Playback} {
		s := caTestStream(kind, Format{SampleRate: 48000, Channels: 2})
		_, err := openCA(f, s, caTestConfig(s.format))
		if !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("%v: %v, want ErrUnsupportedFormat", kind, err)
		}
	}
	if f.openUnits() != 0 || f.listenerCount() != 0 {
		t.Fatalf("leaked %d units, %d listeners", f.openUnits(), f.listenerCount())
	}
}

func TestCACaptureResample(t *testing.T) {
	f, _, b := caTwoDevices()
	f.defIn = b
	s, _ := caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 1})
	u := f.unit(0)
	for range 100 {
		u.fn(caFill(441, 1000))
	}
	st := s.getStats()
	if st.Frames < 47000 || st.Frames > 48100 {
		t.Fatalf("44100 frames became %d, want about 48000", st.Frames)
	}
	got := caReadAll(t, s)
	for _, v := range got[len(got)-100:] {
		if math.Abs(float64(v)-1000) > 20 {
			t.Fatalf("settled sample %d, want about 1000", v)
		}
	}
}

func TestCACaptureResampleDown(t *testing.T) {
	f, _, _ := caTwoDevices()
	s, _ := caOpenFake(t, f, Capture, "", Format{SampleRate: 16000, Channels: 2})
	u := f.unit(0)
	for range 100 {
		u.fn(caFill(480*2, 500))
	}
	if st := s.getStats(); st.Frames < 15500 || st.Frames > 16100 {
		t.Fatalf("48000 frames became %d, want about 16000", st.Frames)
	}
}

func TestCAPlaybackChannels(t *testing.T) {
	f, _, _ := caTwoDevices()
	s, _ := caOpenFake(t, f, Playback, "", Format{SampleRate: 48000, Channels: 1})
	if _, err := s.write(context.Background(), []int16{10, -20, 30}); err != nil {
		t.Fatal(err)
	}
	buf := caFill(10, 99)
	f.unit(0).fn(buf)
	if want := []int16{10, 10, -20, -20, 30, 30, 0, 0, 0, 0}; !slices.Equal(buf, want) {
		t.Fatalf("got %v, want %v", buf, want)
	}
	if st := s.getStats(); st.Frames != 5 || st.Underruns != 2 {
		t.Fatalf("stats %+v", st)
	}

	f, _, b := caTwoDevices()
	f.defOut = b
	s, _ = caOpenFake(t, f, Playback, "", Format{SampleRate: 44100, Channels: 2})
	s.write(context.Background(), []int16{10, 30, -4, -8})
	buf = caFill(3, 99)
	f.unit(0).fn(buf)
	if want := []int16{20, -6, 0}; !slices.Equal(buf, want) {
		t.Fatalf("got %v, want %v", buf, want)
	}
}

func TestCAPlaybackResample(t *testing.T) {
	for _, c := range []struct {
		appRate, devRate int
		dev              uint32
	}{{48000, 44100, 101}, {16000, 48000, 100}, {44100, 48000, 100}} {
		f, _, _ := caTwoDevices()
		f.defOut = c.dev
		s, _ := caOpenFake(t, f, Playback, "", Format{SampleRate: c.appRate, Channels: 1})
		s.write(context.Background(), caFill(c.appRate, 1000))
		u := f.unit(0)
		buf := make([]int16, c.devRate/100*u.ch)
		for range 100 {
			clear(buf)
			u.fn(buf)
		}
		st := s.getStats()
		if d := math.Abs(float64(st.Frames) - float64(c.appRate)); d > float64(c.appRate)/50 {
			t.Errorf("%d to %d: pulled %d frames, want about %d", c.appRate, c.devRate, st.Frames, c.appRate)
		}
		if st.Underruns > uint64(c.appRate)/50 {
			t.Errorf("%d to %d: %d underruns", c.appRate, c.devRate, st.Underruns)
		}
		for _, v := range buf[len(buf)-u.ch*20:] {
			if math.Abs(float64(v)-1000) > 20 {
				t.Errorf("%d to %d: tail sample %d, want about 1000", c.appRate, c.devRate, v)
				break
			}
		}
	}
}

func TestCAReroute(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		f, a, b := caTwoDevices()
		f.defIn, f.defOut = a, a
		s, c := caOpenFake(t, f, kind, "", Format{SampleRate: 48000, Channels: 1})
		first := f.unit(0)
		if !f.listeningOn(caSystemObject) {
			t.Fatal("not listening for default changes")
		}
		f.setDefault(kind, b)
		caEventually(t, "second unit", func() bool { u := f.unit(1); return u != nil && u.started.Load() })
		caEventually(t, "first unit closed", func() bool { return first.closed.Load() })
		if !first.stopped.Load() {
			t.Fatal("first unit closed without stop")
		}
		if f.unit(1).dev != b || f.unit(1).rate != 44100 {
			t.Fatalf("second unit %+v", f.unit(1))
		}
		if err := s.caFailure(); err != nil {
			t.Fatalf("reroute failed the stream: %v", err)
		}
		frames := s.getStats().Frames
		buf := caFill(4, 5)
		first.fn(buf)
		if kind == Playback && !slices.Equal(buf, make([]int16, 4)) {
			t.Fatalf("old unit was not silenced: %v", buf)
		}
		if s.getStats().Frames != frames {
			t.Fatal("old unit still delivers")
		}
		f.unit(1).fn(caFill(441, 3))
		if s.getStats().Frames == frames {
			t.Fatal("new unit does not deliver")
		}
		f.setDefault(kind, b)
		time.Sleep(10 * time.Millisecond)
		if f.unitCount() != 2 {
			t.Fatal("an unchanged default rebuilt the unit")
		}
		if f.listeningOn(a) || !f.listeningOn(b) {
			t.Fatal("device listeners were not moved to the new device")
		}
		c.close()
		if f.listenerCount() != 0 || f.openUnits() != 0 {
			t.Fatalf("after close: %d listeners, %d open units", f.listenerCount(), f.openUnits())
		}
	}
}

func TestCAExplicitDeviceIgnoresDefault(t *testing.T) {
	f, a, b := caTwoDevices()
	s, _ := caOpenFake(t, f, Capture, hex.EncodeToString([]byte("A")), Format{SampleRate: 48000, Channels: 1})
	if f.listeningOn(caSystemObject) {
		t.Fatal("an explicit device listens for default changes")
	}
	f.setDefault(Capture, b)
	f.fire(a, caSelAlive)
	time.Sleep(10 * time.Millisecond)
	if f.unitCount() != 1 || s.caFailure() != nil {
		t.Fatalf("units %d, err %v", f.unitCount(), s.caFailure())
	}
}

func TestCADeviceAlive(t *testing.T) {
	for _, follow := range []bool{false, true} {
		for _, kind := range []Kind{Capture, Playback} {
			f, a, _ := caTwoDevices()
			id := ""
			if !follow {
				id = hex.EncodeToString([]byte("A"))
			}
			s, _ := caOpenFake(t, f, kind, id, Format{SampleRate: 48000, Channels: 1})
			u := f.unit(0)
			f.update(a, caSelAlive, func(d *caFakeDevice) { d.dead = true })
			caEventually(t, "device lost", func() bool { return errors.Is(s.caFailure(), ErrDeviceLost) })
			caEventually(t, "unit released", func() bool { return u.closed.Load() })
			frames := s.getStats().Frames
			u.fn(caFill(10, 1))
			if s.getStats().Frames != frames {
				t.Fatal("delivery after the device died")
			}
			if f.unitCount() != 1 {
				t.Fatal("a dead device was reopened")
			}
		}
	}
}

func TestCADefaultGone(t *testing.T) {
	f, _, _ := caTwoDevices()
	s, _ := caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 1})
	f.setDefault(Capture, 0)
	caEventually(t, "device lost", func() bool { return errors.Is(s.caFailure(), ErrDeviceLost) })
}

func TestCADefaultDiesAndMoves(t *testing.T) {
	f, a, b := caTwoDevices()
	s, _ := caOpenFake(t, f, Playback, "", Format{SampleRate: 48000, Channels: 1})
	f.mu.Lock()
	f.devs[a].dead = true
	f.mu.Unlock()
	f.setDefault(Playback, b)
	caEventually(t, "second unit", func() bool { u := f.unit(1); return u != nil && u.started.Load() })
	if err := s.caFailure(); err != nil {
		t.Fatal(err)
	}
}

func TestCARerouteFails(t *testing.T) {
	f, _, b := caTwoDevices()
	s, _ := caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 1})
	f.mu.Lock()
	f.unitErr = errors.New("refused")
	f.mu.Unlock()
	f.setDefault(Capture, b)
	caEventually(t, "device lost", func() bool { return errors.Is(s.caFailure(), ErrDeviceLost) })
	if f.openUnits() != 0 {
		t.Fatal("old unit left open")
	}
}

func TestCARateChange(t *testing.T) {
	f, a, _ := caTwoDevices()
	s, _ := caOpenFake(t, f, Capture, hex.EncodeToString([]byte("A")), Format{SampleRate: 48000, Channels: 1})
	first := f.unit(0)
	f.update(a, caSelRate, func(d *caFakeDevice) { d.rate = 44100 })
	caEventually(t, "rebuilt unit", func() bool { u := f.unit(1); return u != nil && u.started.Load() })
	caEventually(t, "first unit closed", func() bool { return first.closed.Load() })
	if f.unit(1).rate != 44100 || s.caFailure() != nil {
		t.Fatalf("rate %d err %v", f.unit(1).rate, s.caFailure())
	}
	f.unit(1).fn(caFill(441*2, 100))
	if s.getStats().Frames == 0 {
		t.Fatal("rebuilt unit does not deliver")
	}
}

func TestCAStartError(t *testing.T) {
	f, _, _ := caTwoDevices()
	f.startErr = caStatusError("AudioOutputUnitStart", int32(caFourCC("!hog")))
	s := caTestStream(Capture, Format{SampleRate: 48000, Channels: 1})
	_, err := openCA(f, s, caTestConfig(s.format))
	if !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("got %v, want ErrBackend wrapping fs.ErrPermission", err)
	}
	if f.openUnits() != 0 || f.listenerCount() != 0 {
		t.Fatalf("leaked %d units, %d listeners", f.openUnits(), f.listenerCount())
	}
}

func TestCANoDefault(t *testing.T) {
	f, _, _ := caTwoDevices()
	f.defOut = 0
	s := caTestStream(Playback, Format{SampleRate: 48000, Channels: 1})
	if _, err := openCA(f, s, caTestConfig(s.format)); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("got %v, want ErrNoDevice", err)
	}
	if f.listenerCount() != 0 {
		t.Fatal("listeners leaked")
	}
}

func TestCAStop(t *testing.T) {
	f, a, _ := caTwoDevices()
	s, c := caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 1})
	u := f.unit(0)
	c.stop()
	if !errors.Is(s.caFailure(), ErrDeviceLost) {
		t.Fatalf("stream error %v", s.caFailure())
	}
	frames := s.getStats().Frames
	u.fn(caFill(10, 1))
	if s.getStats().Frames != frames {
		t.Fatal("delivery after stop")
	}
	c.stop()
	f.setDefault(Capture, 101)
	f.fire(a, caSelAlive)
	time.Sleep(10 * time.Millisecond)
	u.fn(caFill(10, 1))
	if s.getStats().Frames != frames || f.unitCount() != 1 {
		t.Fatal("stopped stream resumed")
	}
	c.close()
	c.close()
}

func TestCACloseSilencesCallbacks(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		f, _, _ := caTwoDevices()
		s, c := caOpenFake(t, f, kind, "", Format{SampleRate: 44100, Channels: 1})
		u := f.unit(0)
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for range 4 {
			wg.Go(func() {
				buf := make([]int16, 960)
				for {
					select {
					case <-stop:
						return
					default:
						u.fn(buf)
					}
				}
			})
		}
		time.Sleep(10 * time.Millisecond)
		c.close()
		frames := s.getStats().Frames
		if frames == 0 {
			t.Fatal("no callback ran before close")
		}
		time.Sleep(10 * time.Millisecond)
		close(stop)
		wg.Wait()
		if got := s.getStats().Frames; got != frames {
			t.Fatalf("%v: %d frames after close returned, %d at return", kind, got, frames)
		}
		if err := s.caFailure(); err != nil {
			t.Fatalf("close reported %v", err)
		}
		if !u.closed.Load() || f.listenerCount() != 0 {
			t.Fatal("close left the unit or listeners")
		}
	}
}

func TestCAConcurrentEvents(t *testing.T) {
	f, a, b := caTwoDevices()
	s, c := caOpenFake(t, f, Capture, "", Format{SampleRate: 48000, Channels: 1})
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 50 {
				f.setDefault(Capture, []uint32{a, b}[(i+j)%2])
				if cur := f.unit(f.unitCount() - 1); cur != nil {
					cur.fn(caFill(480, 1))
				}
			}
		})
	}
	wg.Wait()
	time.Sleep(20 * time.Millisecond)
	c.close()
	if f.openUnits() != 0 || f.listenerCount() != 0 {
		t.Fatalf("leaked %d units, %d listeners", f.openUnits(), f.listenerCount())
	}
	if err := s.caFailure(); err != nil {
		t.Fatal(err)
	}
}

// The tests below call CoreAudio. They list devices and register property
// listeners but never open a stream.

func caRealHAL(t *testing.T) *halAPI {
	t.Helper()
	api, err := loadHAL()
	if err != nil {
		t.Fatal(err)
	}
	return api
}

// TestCoreAudioEnumeration checks the real device list. With KOLO_AUDIO_DEVICE_GOLDEN set to a JSON file
// {"Capture": [{"ID","Name","Default"}], "Playback": [...]} it also compares the list with it.
// Keep that file outside the repository: device names and IDs identify this machine.
func TestCoreAudioEnumeration(t *testing.T) {
	api := caRealHAL(t)
	got := map[Kind][]Device{}
	for _, kind := range []Kind{Capture, Playback} {
		devs, err := hwList(kind)
		if err != nil {
			t.Fatal(err)
		}
		got[kind] = devs
		defaults, seen := 0, map[string]bool{}
		for _, d := range devs {
			uid, err := hex.DecodeString(d.ID)
			if err != nil || len(uid) == 0 || d.Name == "" || d.Kind != kind || seen[d.ID] {
				t.Errorf("bad device %+v", d)
			}
			seen[d.ID] = true
			if d.Default {
				defaults++
			}
		}
		if defaults > 1 {
			t.Errorf("%v: %d defaults", kind, defaults)
		}
		if def := api.defaultDevice(kind); def != 0 && defaults != 1 {
			t.Errorf("%v: default device %d not listed", kind, def)
		}
		t.Logf("%v: %d devices", kind, len(devs))
	}
	path := os.Getenv("KOLO_AUDIO_DEVICE_GOLDEN")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want struct{ Capture, Playback []Device }
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	for kind, list := range map[Kind][]Device{Capture: want.Capture, Playback: want.Playback} {
		if !slices.Equal(got[kind], caWithKind(list, kind)) {
			t.Errorf("%v devices differ from %s: got %d, want %d", kind, path, len(got[kind]), len(list))
		}
	}
}

func caRefCount() int {
	n := 0
	caRefs.Range(func(any, any) bool { n++; return true })
	return n
}

func TestCoreAudioListeners(t *testing.T) {
	api := caRealHAL(t)
	callbacks := [3]uintptr{caListenerProc, caInputProc, caRenderProc}
	before := caRefCount()
	var removes []func()
	add := func(obj, sel uint32) {
		t.Helper()
		remove, err := api.listen(obj, sel, func() {})
		if err != nil {
			t.Fatalf("listen(%d, %#x): %v", obj, sel, err)
		}
		removes = append(removes, remove)
	}
	add(caSystemObject, caSelDevices)
	add(caSystemObject, caSelDefaultIn)
	add(caSystemObject, caSelDefaultOut)
	ids, err := api.devices()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		add(id, caSelAlive)
		add(id, caSelRate)
	}
	if got, want := caRefCount(), before+3+2*len(ids); got != want {
		t.Fatalf("%d references registered, want %d", got, want)
	}
	time.Sleep(100 * time.Millisecond)
	for _, remove := range removes {
		remove()
		remove()
	}
	if got := caRefCount(); got != before {
		t.Fatalf("%d references left, want %d", got, before)
	}
	if _, err := loadHAL(); err != nil || callbacks != [3]uintptr{caListenerProc, caInputProc, caRenderProc} {
		t.Fatal("callbacks were created again")
	}
}

// TestCoreAudioRunLoopOnOpen checks that listing devices leaves the process's HAL run loop alone and
// that setRunLoop, which a stream open calls, changes it. It opens no stream.
func TestCoreAudioRunLoopOnOpen(t *testing.T) {
	api := caRealHAL(t)
	if halRunLoopSet.Load() {
		t.Skip("an earlier test opened a stream")
	}
	for _, kind := range []Kind{Capture, Playback} {
		if _, err := hwList(kind); err != nil {
			t.Fatal(err)
		}
	}
	if halRunLoopSet.Load() {
		t.Fatal("listing devices set the run loop")
	}
	api.setRunLoop()
	if !halRunLoopSet.Load() {
		t.Fatal("setRunLoop did not run")
	}
}

func TestCoreAudioListenerError(t *testing.T) {
	api := caRealHAL(t)
	before := caRefCount()
	if _, err := api.listen(0xfffffff0, caSelAlive, func() {}); !errors.Is(err, ErrBackend) && !errors.Is(err, ErrNoDevice) {
		t.Fatalf("listen on a bad object: %v", err)
	}
	if caRefCount() != before {
		t.Fatal("failed listen left a reference")
	}
}

// TestCoreAudioStringTooLong makes CFStringGetMaximumSizeForEncoding report kCFNotFound, as it does when
// the size overflows, and checks for an error instead of a panic.
func TestCoreAudioStringTooLong(t *testing.T) {
	api := caRealHAL(t)
	ids, err := api.devices()
	if err != nil || len(ids) == 0 {
		t.Skip("no device to read a name from")
	}
	if _, err := api.name(ids[0]); err != nil {
		t.Fatal(err)
	}
	old := caCFMax
	caCFMax = func(int, uint32) int { return -1 }
	defer func() { caCFMax = old }()
	if _, err := api.name(ids[0]); !errors.Is(err, ErrBackend) {
		t.Fatalf("name with an overflowing size = %v, want ErrBackend", err)
	}
}

func TestCoreAudioDeviceProperties(t *testing.T) {
	api := caRealHAL(t)
	ids, err := api.devices()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if api.channels(id, Capture)+api.channels(id, Playback) == 0 {
			continue
		}
		rate, err := api.nominalRate(id)
		if err != nil || rate < MinSampleRate {
			t.Errorf("device %d: rate %d, %v", id, rate, err)
		}
	}
	if api.alive(0xfffffff0) {
		t.Fatal("a missing device is alive")
	}
}

func TestCoreAudioCallbackRefs(t *testing.T) {
	caRealHAL(t)
	var hits atomic.Int64
	ref := caRegister(func() { hits.Add(1) })
	defer caRefs.Delete(ref)
	call := func(ref uintptr) uintptr {
		r, _, _ := purego.SyscallN(caListenerProc, 1, 1, 0, ref)
		return r
	}
	if call(ref) != 0 || hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
	if call(ref+1000) != 0 || hits.Load() != 1 {
		t.Fatal("an unknown reference reached a handler")
	}
}

func TestHALUnitRender(t *testing.T) {
	caRealHAL(t)
	var got []int16
	u := &halUnit{kind: Playback, ch: 2, fn: func(buf []int16) {
		got = slices.Clone(buf)
		for i := range buf {
			buf[i] = int16(i + 1)
		}
	}}
	ref := caRegister(u)
	defer caRefs.Delete(ref)
	data := caFill(8, -1)
	list := caBufferList{n: 1, buffer: caBuffer{channels: 2, size: 16, data: unsafe.Pointer(&data[0])}}
	if r, _, _ := purego.SyscallN(caRenderProc, ref, 0, 0, 0, 4, uintptr(unsafe.Pointer(&list))); r != 0 {
		t.Fatalf("status %d", r)
	}
	if len(got) != 8 || !slices.Equal(data, []int16{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("callback saw %d samples, buffer %v", len(got), data)
	}
	u.dead = true
	purego.SyscallN(caRenderProc, ref, 0, 0, 0, 4, uintptr(unsafe.Pointer(&list)))
	if !slices.Equal(data, make([]int16, 8)) {
		t.Fatalf("a closed unit did not play silence: %v", data)
	}
	if u.render(nil) != 0 {
		t.Fatal("nil list")
	}
	list.buffer.data = nil
	if u.render(unsafe.Pointer(&list)) != 0 {
		t.Fatal("nil data")
	}
}

// caStubUnitCalls replaces the calls that tear a unit down, for tests that close a unit that is not real.
func caStubUnitCalls(t *testing.T) {
	t.Helper()
	uninit, dispose := caUninit, caDispose
	caUninit = func(uintptr) int32 { return 0 }
	caDispose = func(uintptr) int32 { return 0 }
	t.Cleanup(func() { caUninit, caDispose = uninit, dispose })
}

// TestHALUnitCloseUnpins checks that close releases the pin on the capture buffer: a Pinner that is
// collected while it still pins something panics in the runtime's finalizer, so the test ends the
// process if close forgets.
func TestHALUnitCloseUnpins(t *testing.T) {
	caRealHAL(t)
	saved := caRender
	defer func() { caRender = saved }()
	caRender = func(uintptr, unsafe.Pointer, unsafe.Pointer, uint32, uint32, unsafe.Pointer) int32 { return 0 }
	caStubUnitCalls(t)
	func() {
		u := &halUnit{kind: Capture, unit: 9, ch: 2, scratch: make([]int16, 2), fn: func([]int16) {}}
		if u.input(nil, nil, 1, 6) != 0 {
			t.Fatal("input failed")
		}
		u.close()
	}()
	for range 5 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHALUnitInput(t *testing.T) {
	caRealHAL(t)
	saved := caRender
	defer func() { caRender = saved }()
	var rendered atomic.Int64
	caRender = func(unit uintptr, flags, ts unsafe.Pointer, bus, frames uint32, list unsafe.Pointer) int32 {
		l := (*caBufferList)(list)
		if unit != 7 || bus != 1 || l.n != 1 || l.buffer.channels != 2 || int(l.buffer.size) != int(frames)*4 {
			return -1
		}
		buf := unsafe.Slice((*int16)(l.buffer.data), frames*2)
		for i := range buf {
			buf[i] = int16(i)
		}
		rendered.Add(1)
		return 0
	}
	var got []int16
	u := &halUnit{kind: Capture, unit: 7, ch: 2, scratch: make([]int16, 4), fn: func(buf []int16) { got = slices.Clone(buf) }}
	caStubUnitCalls(t)
	defer u.close() // input pins the buffer it grows; close unpins it
	ref := caRegister(u)
	if r, _, _ := purego.SyscallN(caInputProc, ref, 0, 0, 1, 6, 0); r != 0 {
		t.Fatalf("status %d", r)
	}
	if !slices.Equal(got, []int16{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}) {
		t.Fatalf("got %v", got)
	}
	got = nil
	u.dead = true
	purego.SyscallN(caInputProc, ref, 0, 0, 1, 6, 0)
	if got != nil || rendered.Load() != 1 {
		t.Fatal("a closed unit rendered")
	}
	u.dead = false
	if u.input(nil, nil, 1, 0) != 0 || rendered.Load() != 1 {
		t.Fatal("zero frames rendered")
	}
}

// The tests below open real devices. They run only with KOLO_AUDIO_HW_TEST=1.

func caHardware(t *testing.T) {
	t.Helper()
	if os.Getenv("KOLO_AUDIO_HW_TEST") != "1" {
		t.Skip("set KOLO_AUDIO_HW_TEST=1 to use real audio devices")
	}
	was := nullBackend.Swap(false)
	t.Cleanup(func() { nullBackend.Store(was) })
}

func TestCoreAudioHardwareCapture(t *testing.T) {
	caHardware(t)
	for _, f := range []Format{{SampleRate: 16000, Channels: 1}, {SampleRate: 44100, Channels: 2}, {SampleRate: 48000, Channels: 1}} {
		s, err := openStream(Capture, Config{Format: f})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		buf := make([]int16, f.SampleRate/10*f.Channels)
		got := 0
		for got < f.SampleRate*f.Channels/2 {
			n, err := s.read(ctx, buf)
			if err != nil {
				t.Fatalf("%+v: %v", f, err)
			}
			got += n
		}
		cancel()
		t.Logf("%+v: stats %+v", f, s.getStats())
		s.close()
	}
}

func TestCoreAudioHardwarePlayback(t *testing.T) {
	caHardware(t)
	for _, f := range []Format{{SampleRate: 16000, Channels: 1}, {SampleRate: 44100, Channels: 2}, {SampleRate: 48000, Channels: 1}} {
		s, err := openStream(Playback, Config{Format: f})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		silence := make([]int16, f.SampleRate/10*f.Channels)
		for range 5 {
			if _, err := s.write(ctx, silence); err != nil {
				t.Fatalf("%+v: %v", f, err)
			}
		}
		cancel()
		t.Logf("%+v: stats %+v", f, s.getStats())
		s.close()
	}
}

func TestCALayouts(t *testing.T) {
	var b caBufferList
	for name, got := range map[string][2]uintptr{
		"AudioComponentDescription":   {unsafe.Sizeof(caCompDesc{}), 20},
		"AudioStreamBasicDescription": {unsafe.Sizeof(caASBD{}), 40},
		"AudioBufferList(1)":          {unsafe.Sizeof(caBufferList{}), 24},
		"AudioBuffer offset":          {unsafe.Offsetof(b.buffer), 8},
		"AudioBuffer data offset":     {unsafe.Offsetof(b.buffer.data), 8},
		"AURenderCallbackStruct":      {unsafe.Sizeof(caCallbackStruct{}), 16},
		"AudioObjectPropertyAddress":  {unsafe.Sizeof(caPropAddr{}), 12},
	} {
		if got[0] != got[1] {
			t.Errorf("%s: %d bytes, want %d", name, got[0], got[1])
		}
	}
}
