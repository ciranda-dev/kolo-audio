package koloaudio

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

const (
	devA = `{0.0.0.00000000}.{aaaaaaaa-0000-0000-0000-000000000001}`
	devB = `{0.0.0.00000000}.{bbbbbbbb-0000-0000-0000-000000000002}`
	devC = `{0.0.1.00000000}.{cccccccc-0000-0000-0000-000000000003}`
)

func TestWasapiIDRoundTrip(t *testing.T) {
	for _, id := range []string{devA, "", "a", "\u00e9\u4e2d", "\U0001F3A4 mic"} {
		got, err := wasapiEndpointID(wasapiID(id))
		if id == "" {
			if !errors.Is(err, ErrNoDevice) {
				t.Errorf("empty id: %v", err)
			}
			continue
		}
		if err != nil || got != id {
			t.Errorf("%q round trip: %q, %v", id, got, err)
		}
	}
	if got := wasapiID("A"); got != "4100" {
		t.Errorf("ID of A = %s, want UTF-16LE hex 4100", got)
	}
	for _, bad := range []string{"zz", "410", "41", "", "4100zz"} {
		if _, err := wasapiEndpointID(bad); !errors.Is(err, ErrNoDevice) {
			t.Errorf("id %q: %v, want ErrNoDevice", bad, err)
		}
	}
}

func TestWaveFormat(t *testing.T) {
	if unsafe.Sizeof(waveFormat{}) != 40 || unsafe.Offsetof(waveFormat{}.cbSize) != 16 || unsafe.Offsetof(waveFormat{}.mask) != 20 || unsafe.Offsetof(waveFormat{}.subFormat) != 24 {
		t.Fatalf("layout: size %d", unsafe.Sizeof(waveFormat{}))
	}
	mono := newWaveFormat(Format{SampleRate: 48000, Channels: 1})
	if mono.tag != 1 || mono.channels != 1 || mono.rate != 48000 || mono.avgBytes != 96000 || mono.blockAlign != 2 || mono.bits != 16 || mono.cbSize != 0 {
		t.Errorf("mono: %+v", mono)
	}
	st := newWaveFormat(Format{SampleRate: 44100, Channels: 2})
	if st.tag != 1 || st.avgBytes != 176400 || st.blockAlign != 4 || st.cbSize != 0 {
		t.Errorf("stereo: %+v", st)
	}
	masks := map[int]uint32{3: 0x7, 4: 0x33, 5: 0x37, 6: 0x3F, 7: 0x70F, 8: 0x63F, 9: 0, 254: 0}
	for ch, mask := range masks {
		f := newWaveFormat(Format{SampleRate: 48000, Channels: ch})
		if f.tag != 0xFFFE || f.cbSize != 22 || f.validBits != 16 || f.mask != mask || f.subFormat != subtypePCM ||
			int(f.channels) != ch || int(f.blockAlign) != 2*ch || f.avgBytes != uint32(48000*2*ch) {
			t.Errorf("%d channels: %+v", ch, f)
		}
	}
	big := newWaveFormat(Format{SampleRate: MaxSampleRate, Channels: MaxChannels})
	if big.avgBytes != MaxSampleRate*2*MaxChannels {
		t.Errorf("largest format: %+v", big)
	}
}

func TestWasapiList(t *testing.T) {
	w := newFakeWorld(
		&fakeDevice{id: devA, name: "Speakers", kind: Playback},
		&fakeDevice{id: devB, name: "", kind: Playback},
		&fakeDevice{id: devC, name: "Mic", kind: Capture},
	)
	w.device(devB).activeFlag = true
	devs, err := listWasapi(w, Playback)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{ID: wasapiID(devA), Name: "Speakers", Kind: Playback, Default: true},
		{ID: wasapiID(devB), Name: devB, Kind: Playback},
	}
	if !slices.Equal(devs, want) {
		t.Errorf("playback: %+v", devs)
	}
	devs, err = listWasapi(w, Capture)
	if err != nil || len(devs) != 1 || devs[0].Name != "Mic" || !devs[0].Default {
		t.Errorf("capture: %+v, %v", devs, err)
	}
	checkWorld(t, w)

	// An inactive endpoint is not listed, and no default is not an error.
	w.setActive(devA, false)
	w.setActive(devB, false)
	w.setDefault(Playback, "")
	devs, err = listWasapi(w, Playback)
	if err != nil || len(devs) != 0 {
		t.Errorf("no endpoints: %+v, %v", devs, err)
	}
	checkWorld(t, w)

	// An endpoint whose ID cannot be read is skipped.
	w.setActive(devA, true)
	w.setActive(devB, true)
	w.device(devA).errs = map[string]error{"id": hresult(0x80004005)}
	devs, err = listWasapi(w, Playback)
	if err != nil || len(devs) != 1 || devs[0].Name != devB {
		t.Errorf("unreadable id: %+v, %v", devs, err)
	}
	checkWorld(t, w)
}

func TestWasapiListErrors(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *fakeWorld)
		is    error
		text  string
	}{
		{"service not running", func(w *fakeWorld) { w.listErr = hrServiceNotRunning }, ErrNoDevice, "AUDCLNT_E_SERVICE_NOT_RUNNING"},
		{"access denied", func(w *fakeWorld) { w.listErr = hrAccessDenied }, ErrBackend, "0x80070005"},
		{"enumerator", func(w *fakeWorld) { w.enumErr = hresult(0x80040154) }, ErrBackend, "0x80040154"},
		{"coinit", func(w *fakeWorld) { w.coInitErr = hresult(0x80070057) }, ErrBackend, "CoInitializeEx"},
		{"plain error", func(w *fakeWorld) { w.listErr = errFake }, ErrBackend, "fake failure"},
	}
	for _, c := range cases {
		w := newFakeWorld(&fakeDevice{id: devA, kind: Playback})
		c.setup(w)
		_, err := listWasapi(w, Playback)
		if !errors.Is(err, c.is) || !strings.Contains(err.Error(), c.text) || !strings.HasPrefix(err.Error(), "koloaudio:") {
			t.Errorf("%s: %v", c.name, err)
		}
		checkWorld(t, w)
	}
}

func TestWasapiOpenInitialize(t *testing.T) {
	w := newFakeWorld(
		&fakeDevice{id: devA, kind: Capture},
		&fakeDevice{id: devB, kind: Capture},
	)
	hw, sink := mustOpen(t, w, Capture, Config{Format: Format{SampleRate: 16000, Channels: 6}})
	d := w.device(devA)
	w.mu.Lock()
	inits := slices.Clone(d.inits)
	w.mu.Unlock()
	if len(inits) != 1 || inits[0].flags != 0x00040000|0x80000000|0x08000000 {
		t.Fatalf("initialize: %+v", inits)
	}
	if f := inits[0].format; f.channels != 6 || f.rate != 16000 || f.tag != 0xFFFE || f.bits != 16 {
		t.Errorf("format: %+v", f)
	}
	closeAndCheck(t, w, hw, sink)

	// A named device is opened by its endpoint ID, not the default.
	hw, sink = mustOpen(t, w, Capture, Config{DeviceID: wasapiID(devB)})
	w.mu.Lock()
	log := strings.Join(w.log, "\n")
	w.mu.Unlock()
	if !strings.Contains(log, "endpoint "+devB) || len(w.device(devB).inits) != 1 {
		t.Errorf("named device not opened:\n%s", log)
	}
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiCapture(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{Format: Format{SampleRate: 48000, Channels: 2}})
	w.with(devA, func(d *fakeDevice) {
		d.packets = []fakePacket{
			{data: []int16{1, 2, 3, 4}, frames: 2},
			{frames: 3, silent: true},
			{data: []int16{5, 6}, frames: 1},
		}
	})
	w.signal(devA)
	want := []int16{1, 2, 3, 4, 0, 0, 0, 0, 0, 0, 5, 6}
	eventually(t, "capture", func() bool { got, _, _ := sink.snapshot(); return len(got) >= len(want) })
	if got, _, _ := sink.snapshot(); !slices.Equal(got, want) {
		t.Errorf("captured %v, want %v", got, want)
	}
	w.mu.Lock()
	left := len(w.device(devA).packets)
	w.mu.Unlock()
	if left != 0 {
		t.Errorf("%d packets not released", left)
	}
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiCaptureSilentLargerThanBuffer(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture, bufFrames: 4})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.with(devA, func(d *fakeDevice) { d.packets = []fakePacket{{frames: 10, silent: true}} })
	w.signal(devA)
	eventually(t, "silence", func() bool { got, _, _ := sink.snapshot(); return len(got) == 10 })
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiCaptureShortPacketLosesDevice(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.with(devA, func(d *fakeDevice) { d.packets = []fakePacket{{data: []int16{1}, frames: 5}} })
	w.signal(devA)
	eventually(t, "stopped", func() bool { _, _, n := sink.snapshot(); return n == 1 })
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRender(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Playback, bufFrames: 480})
	hw, sink := mustOpen(t, w, Playback, Config{Format: Format{SampleRate: 48000, Channels: 2}})
	var first fakeRender
	w.with(devA, func(d *fakeDevice) {
		if len(d.rendered) != 1 {
			t.Fatalf("%d prefills", len(d.rendered))
		}
		first = d.rendered[0]
	})
	if first.frames != 480 || first.flags != wasapiBufferSilent {
		t.Errorf("prefill: %d frames, flags %d", first.frames, first.flags)
	}
	if _, played, _ := sink.snapshot(); played != 0 {
		t.Errorf("sink played %d samples before the first event", played)
	}
	// 100 frames are free: only those are asked for.
	w.with(devA, func(d *fakeDevice) { d.pad = 380 })
	w.signal(devA)
	eventually(t, "render", func() bool { _, p, _ := sink.snapshot(); return p == 200 })
	w.with(devA, func(d *fakeDevice) {
		r := d.rendered[len(d.rendered)-1]
		if r.frames != 100 || r.flags != 0 || len(r.data) != 200 || r.data[0] != 7 || r.data[199] != 7 {
			t.Errorf("committed %d frames, flags %d, %d samples", r.frames, r.flags, len(r.data))
		}
		d.pad = 480 // full: nothing to do
	})
	calls := 0
	w.with(devA, func(d *fakeDevice) { calls = d.padCalls })
	w.signal(devA)
	eventually(t, "full buffer event", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.device(devA).padCalls > calls
	})
	if _, p, _ := sink.snapshot(); p != 200 {
		t.Errorf("a full buffer was written to: %d samples", p)
	}
	w.with(devA, func(d *fakeDevice) { d.pad = 0 })
	w.signal(devA)
	eventually(t, "second render", func() bool { _, p, _ := sink.snapshot(); return p == 200+960 })
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiOpenErrors(t *testing.T) {
	cases := []struct {
		name  string
		kind  Kind
		setup func(w *fakeWorld)
		id    string
		is    error
		perm  bool
		text  string
	}{
		{"coinit", Capture, func(w *fakeWorld) { w.coInitErr = hresult(0x8007000E) }, "", ErrBackend, false, "CoInitializeEx"},
		{"enumerator", Capture, func(w *fakeWorld) { w.enumErr = hresult(0x80040154) }, "", ErrBackend, false, "MMDeviceEnumerator"},
		{"no default", Capture, func(w *fakeWorld) { w.defaults[Capture] = "" }, "", ErrNoDevice, false, "no capture endpoint"},
		{"service", Playback, func(w *fakeWorld) { w.device(devA).activateErr = hrServiceNotRunning }, "", ErrNoDevice, false, "SERVICE_NOT_RUNNING"},
		{"denied", Capture, func(w *fakeWorld) { w.device(devA).activateErr = hrAccessDenied }, "", ErrBackend, true, "Activate"},
		{"denied at initialize", Capture, func(w *fakeWorld) { w.device(devA).initErr = hrAccessDenied }, "", ErrBackend, true, "Initialize"},
		{"format", Playback, func(w *fakeWorld) { w.device(devA).initErr = hrUnsupportedFormat }, "", ErrUnsupportedFormat, false, "UNSUPPORTED_FORMAT"},
		{"in use", Capture, func(w *fakeWorld) { w.device(devA).initErr = hresult(0x8889000A) }, "", ErrBackend, false, "0x8889000a"},
		{"event", Capture, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"setEvent": hresult(0x80070057)} }, "", ErrBackend, false, "SetEventHandle"},
		{"buffer", Capture, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"bufferFrames": errFake} }, "", ErrBackend, false, "GetBufferSize"},
		{"service object", Capture, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"service": errFake} }, "", ErrBackend, false, "IAudioCaptureClient"},
		{"render object", Playback, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"service": errFake} }, "", ErrBackend, false, "IAudioRenderClient"},
		{"start", Capture, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"start": hresult(0x80004005)} }, "", ErrBackend, false, "Start"},
		{"prefill", Playback, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"getBuffer": errFake} }, "", ErrBackend, false, "GetBuffer"},
		{"commit prefill", Playback, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"releaseBuffer": errFake} }, "", ErrBackend, false, "ReleaseBuffer"},
		{"device id", Capture, func(w *fakeWorld) { w.device(devA).errs = map[string]error{"id": errFake} }, "", ErrBackend, false, "GetId"},
		{"unknown id", Capture, func(w *fakeWorld) {}, wasapiID("{nope}"), ErrNoDevice, false, "no capture endpoint"},
		{"malformed id", Capture, func(w *fakeWorld) {}, "xyz", ErrNoDevice, false, `"xyz"`},
	}
	for _, c := range cases {
		w := newFakeWorld(&fakeDevice{id: devA, kind: c.kind})
		c.setup(w)
		hw, sink, err := openFake(t, w, c.kind, Config{DeviceID: c.id})
		if err == nil {
			hw.close()
			t.Errorf("%s: opened", c.name)
			continue
		}
		if !errors.Is(err, c.is) || errors.Is(err, fs.ErrPermission) != c.perm || !strings.Contains(err.Error(), c.text) || !strings.HasPrefix(err.Error(), "koloaudio:") {
			t.Errorf("%s: %v", c.name, err)
		}
		if _, _, n := sink.snapshot(); n != 0 {
			t.Errorf("%s: onStopped called for a failed open", c.name)
		}
		checkWorld(t, w)
	}
}

func TestWasapiDeviceInvalidated(t *testing.T) {
	for _, h := range []hresult{hrDeviceInvalidated, hrResourcesInvalidate} {
		for _, kind := range []Kind{Capture, Playback} {
			w := newFakeWorld(&fakeDevice{id: devA, kind: kind}, &fakeDevice{id: devB, kind: kind})
			hw, sink := mustOpen(t, w, kind, Config{DeviceID: wasapiID(devA)})
			op := "nextPacket"
			if kind == Playback {
				op = "padding"
			}
			w.with(devA, func(d *fakeDevice) { d.errs = map[string]error{op: h} })
			w.signal(devA)
			eventually(t, "stopped", func() bool { _, _, n := sink.snapshot(); return n == 1 })
			// The thread has released the device by itself.
			w.signal(devA)
			time.Sleep(5 * time.Millisecond)
			if _, _, n := sink.snapshot(); n != 1 {
				t.Errorf("%v %v: onStopped %d times", h, kind, n)
			}
			closeAndCheck(t, w, hw, sink)
			if strings.Contains(w.logged(), "activate "+devB) {
				t.Errorf("%v %v: a named device must not move to another", h, kind)
			}
		}
	}
}

func TestWasapiOtherStreamErrorLosesDevice(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.with(devA, func(d *fakeDevice) { d.errs = map[string]error{"nextPacket": errFake} })
	w.signal(devA)
	eventually(t, "stopped", func() bool { _, _, n := sink.snapshot(); return n == 1 })
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRerouteOnInvalidated(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		w := newFakeWorld(&fakeDevice{id: devA, kind: kind}, &fakeDevice{id: devB, kind: kind})
		hw, sink := mustOpen(t, w, kind, Config{})
		w.setDefault(kind, devB)
		op := "nextPacket"
		if kind == Playback {
			op = "padding"
		}
		w.with(devA, func(d *fakeDevice) { d.errs = map[string]error{op: hrDeviceInvalidated} })
		w.signal(devA)
		eventually(t, "reroute", func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.device(devB).client != nil && w.device(devB).client.started
		})
		w.with(devA, func(d *fakeDevice) {
			if d.client.started {
				t.Errorf("%v: old client still started", kind)
			}
		})
		deliver(t, w, sink, kind, devB, 11)
		if _, _, n := sink.snapshot(); n != 0 {
			t.Errorf("%v: reroute reported a loss", kind)
		}
		closeAndCheck(t, w, hw, sink)
		log := w.logged()
		if strings.Index(log, "client.release "+devA) > strings.Index(log, "initialize "+devB) {
			t.Errorf("%v: new device opened before the old one was released\n%s", kind, log)
		}
	}
}

// deliver makes one device event with data worth marker and waits for it.
func deliver(t *testing.T, w *fakeWorld, sink *fakeSink, kind Kind, id string, marker int16) {
	t.Helper()
	if kind == Capture {
		w.with(id, func(d *fakeDevice) { d.packets = []fakePacket{{data: []int16{marker}, frames: 1}} })
		w.signal(id)
		eventually(t, "capture from "+id, func() bool { got, _, _ := sink.snapshot(); return slices.Contains(got, marker) })
		return
	}
	_, before, _ := sink.snapshot()
	w.with(id, func(d *fakeDevice) { d.pad = d.bufFrames - 10 })
	w.signal(id)
	eventually(t, "render on "+id, func() bool { _, p, _ := sink.snapshot(); return p > before })
}

func TestWasapiRerouteOnDefaultChange(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	deliver(t, w, sink, Capture, devA, 1)

	w.setDefault(Capture, devB)
	w.notify().defaultChanged(wasapiFlowCapture, wasapiRoleConsole, devB)
	eventually(t, "reroute", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.device(devB).client != nil && w.device(devB).client.started
	})
	deliver(t, w, sink, Capture, devB, 2)

	// The default moves back: a second reroute works too.
	w.setDefault(Capture, devA)
	w.notify().defaultChanged(wasapiFlowCapture, wasapiRoleConsole, devA)
	eventually(t, "second reroute", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.device(devA).inits) == 2
	})
	deliver(t, w, sink, Capture, devA, 3)
	if _, _, n := sink.snapshot(); n != 0 {
		t.Error("reroute reported a loss")
	}
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRerouteWhenCurrentLeaves(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Playback}, &fakeDevice{id: devB, kind: Playback})
	hw, sink := mustOpen(t, w, Playback, Config{})
	w.setActive(devA, false)
	w.setDefault(Playback, devB)
	w.notify().deviceState(devA, 4) // DEVICE_STATE_NOTPRESENT
	eventually(t, "reroute", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.device(devB).client != nil && w.device(devB).client.started
	})
	deliver(t, w, sink, Playback, devB, 0)
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRerouteFailureLosesDevice(t *testing.T) {
	cases := map[string]func(w *fakeWorld){
		"no default":   func(w *fakeWorld) { w.setActive(devA, false); w.setActive(devB, false) },
		"open fails":   func(w *fakeWorld) { w.device(devB).initErr = hresult(0x80004005) },
		"activate":     func(w *fakeWorld) { w.device(devB).activateErr = hrAccessDenied },
		"still absent": func(w *fakeWorld) { w.setDefault(Capture, "") },
	}
	for name, setup := range cases {
		w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
		hw, sink := mustOpen(t, w, Capture, Config{})
		w.setDefault(Capture, devB)
		setup(w)
		w.notify().defaultChanged(wasapiFlowCapture, wasapiRoleConsole, devB)
		eventually(t, name, func() bool { _, _, n := sink.snapshot(); return n == 1 })
		closeAndCheck(t, w, hw, sink)
	}
}

func TestWasapiNoDefaultWhenFollowingLosesDevice(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.setDefault(Capture, "")
	w.notify().defaultChanged(wasapiFlowCapture, wasapiRoleConsole, "")
	eventually(t, "stopped", func() bool { _, _, n := sink.snapshot(); return n == 1 })
	closeAndCheck(t, w, hw, sink)
}

// TestWasapiNotifications checks which notifications end or move a stream.
func TestWasapiNotifications(t *testing.T) {
	type notice func(n wasapiNotifier)
	state := func(id string, st uint32) notice { return func(n wasapiNotifier) { n.deviceState(id, st) } }
	removed := func(id string) notice { return func(n wasapiNotifier) { n.deviceRemoved(id) } }
	changed := func(flow, role uint32, id string) notice {
		return func(n wasapiNotifier) { n.defaultChanged(flow, role, id) }
	}
	cases := []struct {
		name   string
		follow bool
		n      notice
		lost   bool
		rerout bool
	}{
		{"fixed: current disabled", false, state(devA, 2), true, false},
		{"fixed: current not present", false, state(devA, 4), true, false},
		{"fixed: current unplugged", false, state(devA, 8), true, false},
		{"fixed: current removed", false, removed(devA), true, false},
		{"fixed: current becomes active", false, state(devA, 1), false, false},
		{"fixed: other disabled", false, state(devB, 2), false, false},
		{"fixed: other removed", false, removed(devB), false, false},
		{"fixed: default changed", false, changed(wasapiFlowCapture, wasapiRoleConsole, devB), false, false},
		{"follow: other disabled", true, state(devB, 2), false, false},
		{"follow: other removed", true, removed(devB), false, false},
		{"follow: default is already current", true, changed(wasapiFlowCapture, wasapiRoleConsole, devA), false, false},
		{"follow: other flow", true, changed(wasapiFlowRender, wasapiRoleConsole, devB), false, false},
		{"follow: communications role", true, changed(wasapiFlowCapture, 2, devB), false, false},
		{"follow: multimedia role", true, changed(wasapiFlowCapture, 1, devB), false, false},
		{"follow: default changed", true, changed(wasapiFlowCapture, wasapiRoleConsole, devB), false, true},
		{"follow: current removed", true, removed(devA), false, true},
		{"follow: current unplugged", true, state(devA, 8), false, true},
	}
	for _, c := range cases {
		w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
		cfg := Config{}
		if !c.follow {
			cfg.DeviceID = wasapiID(devA)
		}
		hw, sink := mustOpen(t, w, Capture, cfg)
		if c.rerout {
			w.setDefault(Capture, devB)
		}
		c.n(w.notify())
		switch {
		case c.lost:
			eventually(t, c.name, func() bool { _, _, n := sink.snapshot(); return n == 1 })
		case c.rerout:
			eventually(t, c.name, func() bool {
				w.mu.Lock()
				defer w.mu.Unlock()
				return w.device(devB).client != nil
			})
		default:
			// Prove the thread handled the notification by delivering.
			deliver(t, w, sink, Capture, devA, 9)
			w.mu.Lock()
			other := w.device(devB).client != nil
			opens := len(w.device(devA).inits)
			w.mu.Unlock()
			if other || opens != 1 {
				t.Errorf("%s: the stream moved", c.name)
			}
			if _, _, n := sink.snapshot(); n != 0 {
				t.Errorf("%s: the stream was reported lost", c.name)
			}
		}
		closeAndCheck(t, w, hw, sink)
	}
}

func TestWasapiStop(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Playback})
	hw, sink := mustOpen(t, w, Playback, Config{})
	hw.stop()
	if _, _, n := sink.snapshot(); n != 1 {
		t.Fatalf("onStopped %d times", n)
	}
	hw.stop()
	closeAndCheck(t, w, hw, sink)
	if _, _, n := sink.snapshot(); n != 1 {
		t.Errorf("onStopped %d times after close", n)
	}
	// stop after close and close twice are safe.
	hw.stop()
	hw.close()
}

func TestWasapiCloseWithoutStop(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	hw.close()
	hw.close()
	sink.markClosed()
	if _, _, n := sink.snapshot(); n != 0 {
		t.Errorf("close reported a loss")
	}
	checkWorld(t, w)
}

// TestWasapiTeardownOrder checks that the device is released before the
// enumerator, the notifications end before their wake event closes, and
// the apartment is left last.
func TestWasapiTeardownOrder(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	closeAndCheck(t, w, hw, sink)
	log := strings.Split(w.logged(), "\n")
	pos := func(s string) int {
		for i, l := range log {
			if l == s {
				return i
			}
		}
		t.Fatalf("%q not in log:\n%s", s, strings.Join(log, "\n"))
		return -1
	}
	order := []string{"stop " + devA, "capture.release " + devA, "client.release " + devA, "endpoint.release " + devA, "unregister", "enumerator.release", "coUninit"}
	for i := 1; i < len(order); i++ {
		if pos(order[i-1]) >= pos(order[i]) {
			t.Errorf("%q must come before %q:\n%s", order[i-1], order[i], strings.Join(log, "\n"))
		}
	}
	if pos("unregister") > pos("event.close") {
		t.Errorf("notifications still registered when an event closed:\n%s", strings.Join(log, "\n"))
	}
	if log[len(log)-1] != "coUninit" {
		t.Errorf("the apartment is not left last:\n%s", strings.Join(log, "\n"))
	}
}

// TestWasapiNoCallbackAfterClose runs close against a device that is
// delivering and against notifications, with the race detector.
func TestWasapiNoCallbackAfterClose(t *testing.T) {
	for round := 0; round < 50; round++ {
		w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
		hw, sink := mustOpen(t, w, Capture, Config{})
		stopFeeding := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopFeeding:
					return
				default:
				}
				w.with(devA, func(d *fakeDevice) {
					if len(d.packets) < 4 {
						d.packets = append(d.packets, fakePacket{data: []int16{1}, frames: 1})
					}
				})
				w.signal(devA)
			}
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopFeeding:
					return
				default:
				}
				if n := w.notify(); n != nil {
					n.deviceState(devA, 2)
					n.defaultChanged(wasapiFlowCapture, wasapiRoleConsole, devB)
				}
			}
		}()
		time.Sleep(time.Duration(round%5) * time.Millisecond)
		hw.close()
		sink.markClosed()
		// Notifications and events that arrive after close are harmless.
		close(stopFeeding)
		wg.Wait()
		time.Sleep(time.Millisecond)
		checkWorld(t, w)
	}
}

// TestWasapiNotifierNeverBlocks calls every notification while the audio
// thread is stuck inside the sink, and checks that none of them waits or
// makes a COM call.
func TestWasapiNotifierNeverBlocks(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	sink.mu.Lock()
	sink.block = make(chan struct{})
	sink.entered = make(chan struct{}, 1)
	sink.mu.Unlock()
	w.with(devA, func(d *fakeDevice) { d.packets = []fakePacket{{data: []int16{1}, frames: 1}} })
	w.signal(devA)
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sink not entered")
	}
	w.mu.Lock()
	calls := w.calls
	n := w.notifier
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			n.deviceState(devA, 2)
			n.deviceState(devB, 4)
			n.deviceRemoved(devA)
			n.defaultChanged(wasapiFlowCapture, wasapiRoleConsole, devB)
			n.defaultChanged(wasapiFlowCapture, wasapiRoleConsole, "")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a notification blocked while the audio thread was busy")
	}
	w.mu.Lock()
	if w.calls != calls {
		t.Errorf("notifications made %d COM calls", w.calls-calls)
	}
	w.mu.Unlock()
	close(sink.block)
	eventually(t, "reroute after unblock", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.device(devA).inits) == 2
	})
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRerouteLoop(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Playback}, &fakeDevice{id: devB, kind: Playback})
	hw, sink := mustOpen(t, w, Playback, Config{})
	cur := devA
	for i := 0; i < 50; i++ {
		next := devB
		if cur == devB {
			next = devA
		}
		w.setDefault(Playback, next)
		w.notify().defaultChanged(wasapiFlowRender, wasapiRoleConsole, next)
		cur = next
		eventually(t, "reroute", func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			c := w.device(cur).client
			return c != nil && c.started
		})
		deliver(t, w, sink, Playback, cur, 0)
	}
	if _, _, n := sink.snapshot(); n != 0 {
		t.Error("lost")
	}
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiErrorMapping(t *testing.T) {
	if wasapiInvalidated(nil) || wasapiInvalidated(errFake) || wasapiInvalidated(hrAccessDenied) {
		t.Error("wrong errors reported as invalidated")
	}
	if !wasapiInvalidated(hrDeviceInvalidated) || !wasapiInvalidated(hrResourcesInvalidate) {
		t.Error("invalidation not recognized")
	}
	wrapped := wasapiError("x", Capture, hresult(0x88890004))
	if !wasapiInvalidated(wrapped) || !errors.Is(wrapped, ErrBackend) {
		t.Errorf("wrapped invalidation: %v", wrapped)
	}
	if got := hresult(0x88890004).Error(); got != "HRESULT 0x88890004" {
		t.Error(got)
	}
}

func TestWasapiConstants(t *testing.T) {
	if wasapiStreamFlags != 0x88040000 {
		t.Errorf("stream flags %#x", uint32(wasapiStreamFlags))
	}
	if wasapiFlow(Capture) != 1 || wasapiFlow(Playback) != 0 {
		t.Error("flow")
	}
}

func TestWasapiDuration(t *testing.T) {
	ms := func(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }
	cases := []struct {
		period time.Duration
		min    int64
		want   int64
	}{
		{DefaultPeriod, 100000, 100000},
		{DefaultPeriod, 26667, 100000},
		{ms(25), 100000, 250000},
		{ms(7.3), 26667, 73000},
		{ms(1), 100000, 100000},
		{ms(1), 26667, 26667},
		{125 * time.Microsecond, 100000, 100000},
		{100, 100000, 100000},
		{ms(10), 0, 100000},
		{ms(7.5), 0, 75000},
		{MaxBuffer, 100000, 20000000},
		{ms(10), 1, 100000},
	}
	for _, c := range cases {
		if got := wasapiDuration(c.period, c.min); got != c.want {
			t.Errorf("period %v, minimum %d: %d, want %d", c.period, c.min, got, c.want)
		}
	}
}

func TestWasapiPeriodApplied(t *testing.T) {
	cases := []struct {
		name   string
		period time.Duration
		min    int64
		want   int64
	}{
		{"default", 0, 0, 100000},
		{"odd", 25 * time.Millisecond, 0, 250000},
		{"below minimum", time.Millisecond, 0, 100000},
		{"odd with a fine device", 7300 * time.Microsecond, 26667, 73000},
		{"default with a fine device", 0, 26667, 100000},
		{"below a fine device minimum", time.Millisecond, 26667, 26667},
	}
	for _, c := range cases {
		for _, kind := range []Kind{Capture, Playback} {
			w := newFakeWorld(&fakeDevice{id: devA, kind: kind, periodMin: c.min})
			hw, sink := mustOpen(t, w, kind, Config{Period: c.period})
			w.mu.Lock()
			inits := slices.Clone(w.device(devA).inits)
			w.mu.Unlock()
			if len(inits) != 1 || inits[0].duration != c.want {
				t.Errorf("%s %v: %+v, want duration %d", c.name, kind, inits, c.want)
			}
			closeAndCheck(t, w, hw, sink)
		}
	}
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture, errs: map[string]error{"devicePeriod": hresult(0x80004005)}})
	if _, _, err := openFake(t, w, Capture, Config{}); !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "GetDevicePeriod") {
		t.Errorf("device period failure: %v", err)
	}
	checkWorld(t, w)
}

func TestWasapiPriority(t *testing.T) {
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	if err := hw.(*wasapiStream).priorityErr; err != nil {
		t.Errorf("priority: %v", err)
	}
	closeAndCheck(t, w, hw, sink)
	log := strings.Split(w.logged(), "\n")
	pos := func(s string) int { return slices.Index(log, s) }
	if pos("prioritize") < 0 || pos("revert") < pos("prioritize") || pos("revert") > pos("coUninit") {
		t.Errorf("registration must be reverted before the apartment is left:\n%s", strings.Join(log, "\n"))
	}

	// Failing to register is not fatal: the stream runs at normal priority.
	w = newFakeWorld(&fakeDevice{id: devA, kind: Capture})
	w.prioErr = errFake
	hw, sink = mustOpen(t, w, Capture, Config{})
	if err := hw.(*wasapiStream).priorityErr; !errors.Is(err, errFake) {
		t.Errorf("priority error %v", err)
	}
	deliver(t, w, sink, Capture, devA, 4)
	closeAndCheck(t, w, hw, sink)
	if strings.Contains(w.logged(), "revert") {
		t.Error("reverted a registration that failed")
	}
}

func TestWasapiRetrySchedule(t *testing.T) {
	var total time.Duration
	for i, d := range wasapiRetry {
		if i > 0 && d != 2*wasapiRetry[i-1] {
			t.Errorf("retry %d: %v does not double %v", i, d, wasapiRetry[i-1])
		}
		total += d
	}
	if len(wasapiRetry) != 5 || total < 2*time.Second || total > 5*time.Second {
		t.Errorf("%d retries over %v", len(wasapiRetry), total)
	}
}

func startReroute(t *testing.T, w *fakeWorld, kind Kind) {
	t.Helper()
	w.setDefault(kind, devB)
	w.notify().defaultChanged(wasapiFlow(kind), wasapiRoleConsole, devB)
}

func TestWasapiRerouteRetrySucceeds(t *testing.T) {
	withRetry(t, 3, 6, 12, 24, 48)
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.mu.Lock()
	w.defaultFails = 3
	w.mu.Unlock()
	startReroute(t, w, Capture)
	eventually(t, "reroute", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.device(devB).client != nil && w.device(devB).client.started
	})
	deliver(t, w, sink, Capture, devB, 5)
	if _, _, n := sink.snapshot(); n != 0 {
		t.Error("lost despite a successful retry")
	}
	w.mu.Lock()
	waits := slices.Clone(w.waits)
	w.mu.Unlock()
	if len(waits) < 3 {
		t.Fatalf("waited %d times, want 3", len(waits))
	}
	for i, want := range []time.Duration{3, 6, 12} {
		if waits[i] > want*time.Millisecond || waits[i] < want*time.Millisecond/2 {
			t.Errorf("wait %d: %v, want about %v", i, waits[i], want*time.Millisecond)
		}
	}
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRerouteRetryOpenFailure(t *testing.T) {
	withRetry(t, 20, 20, 20, 20, 20)
	w := newFakeWorld(&fakeDevice{id: devA, kind: Playback}, &fakeDevice{id: devB, kind: Playback, initErr: hresult(0x88890002)})
	hw, sink := mustOpen(t, w, Playback, Config{})
	startReroute(t, w, Playback)
	eventually(t, "second attempt", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.device(devB).inits) >= 2
	})
	w.with(devB, func(d *fakeDevice) { d.initErr = nil })
	eventually(t, "reroute", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		c := w.device(devB).client
		return c != nil && c.started
	})
	if _, _, n := sink.snapshot(); n != 0 {
		t.Error("lost despite a successful retry")
	}
	closeAndCheck(t, w, hw, sink)
}

func TestWasapiRerouteRetryGivesUp(t *testing.T) {
	withRetry(t, 1, 1, 1, 1)
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.mu.Lock()
	w.defaultFails = 1000
	w.mu.Unlock()
	startReroute(t, w, Capture)
	eventually(t, "stopped", func() bool { _, _, n := sink.snapshot(); return n == 1 })
	closeAndCheck(t, w, hw, sink)
	if n := strings.Count(w.logged(), "defaultEndpoint "); n != 1+5 {
		t.Errorf("%d attempts, want 1 at open and 5 for the reroute", n)
	}
	cause := hw.(*wasapiStream).cause
	if !errors.Is(cause, ErrNoDevice) || !strings.Contains(cause.Error(), "GetDefaultAudioEndpoint") {
		t.Errorf("cause %v", cause)
	}
	if _, _, n := sink.snapshot(); n != 1 {
		t.Errorf("onStopped %d times", n)
	}
}

func TestWasapiRerouteRetryOpenFailureGivesUp(t *testing.T) {
	withRetry(t, 1, 1)
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture, initErr: hrUnsupportedFormat})
	hw, sink := mustOpen(t, w, Capture, Config{})
	startReroute(t, w, Capture)
	eventually(t, "stopped", func() bool { _, _, n := sink.snapshot(); return n == 1 })
	closeAndCheck(t, w, hw, sink)
	if cause := hw.(*wasapiStream).cause; !errors.Is(cause, ErrUnsupportedFormat) {
		t.Errorf("cause %v", cause)
	}
	if n := len(w.device(devB).inits); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
}

func TestWasapiCloseDuringBackoff(t *testing.T) {
	withRetry(t, 60000)
	w := newFakeWorld(&fakeDevice{id: devA, kind: Capture}, &fakeDevice{id: devB, kind: Capture})
	hw, sink := mustOpen(t, w, Capture, Config{})
	w.mu.Lock()
	w.defaultFails = 1000
	w.mu.Unlock()
	startReroute(t, w, Capture)
	eventually(t, "backoff", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.waits) > 0
	})
	start := time.Now()
	hw.close()
	sink.markClosed()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("close took %v during a backoff", d)
	}
	if _, _, n := sink.snapshot(); n != 0 {
		t.Error("close reported a loss")
	}
	checkWorld(t, w)
}

func TestWasapiStopDuringBackoff(t *testing.T) {
	withRetry(t, 60000)
	w := newFakeWorld(&fakeDevice{id: devA, kind: Playback}, &fakeDevice{id: devB, kind: Playback})
	hw, sink := mustOpen(t, w, Playback, Config{})
	w.mu.Lock()
	w.defaultFails = 1000
	w.mu.Unlock()
	startReroute(t, w, Playback)
	eventually(t, "backoff", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.waits) > 0
	})
	hw.stop()
	if _, _, n := sink.snapshot(); n != 1 {
		t.Errorf("onStopped %d times", n)
	}
	closeAndCheck(t, w, hw, sink)
}
