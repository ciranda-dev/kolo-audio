package koloaudio

// A fake of the COM layer for the WASAPI engine tests. It keeps a log of
// every call and counts what is still open, so tests can check teardown.

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeEvent struct {
	w      *fakeWorld
	ch     chan struct{}
	closed bool
}

func (e *fakeEvent) set() {
	select {
	case e.ch <- struct{}{}:
	default:
	}
}

func (e *fakeEvent) close() {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	if e.closed {
		e.w.fail("event closed twice")
	}
	e.closed = true
	e.w.live--
	e.w.logf("event.close")
}

type fakeDevice struct {
	id, name   string
	kind       Kind
	activeFlag bool

	activateErr error
	initErr     error
	bufFrames   uint32
	// device period in 100 ns units; zero means 10 ms.
	periodDef, periodMin int64
	// errs maps an operation to the error its next call returns.
	errs map[string]error

	// capture packets, oldest first; a nil data is a silent packet.
	packets []fakePacket
	// render: padding reported, and what each ReleaseBuffer committed.
	pad      uint32
	padCalls int
	rendered []fakeRender

	client *fakeClient
	inits  []fakeInit
}

type fakePacket struct {
	data   []int16
	frames uint32
	silent bool
}

type fakeRender struct {
	frames, flags uint32
	data          []int16
}

type fakeInit struct {
	format   waveFormat
	flags    uint32
	duration int64
}

type fakeWorld struct {
	mu       sync.Mutex
	devices  []*fakeDevice
	defaults map[Kind]string
	log      []string
	live     int // open COM objects, events and apartments
	notifier wasapiNotifier
	failures []string

	// defaultFails makes that many defaultEndpoint calls fail, after the
	// first connect; prioErr fails prioritize.
	defaultFails int
	prioErr      error
	waits        []time.Duration

	coInitErr error
	enumErr   error
	listErr   error
	// calls counts the COM calls of the engine, to show that notifications
	// make none.
	calls int
}

func newFakeWorld(devs ...*fakeDevice) *fakeWorld {
	w := &fakeWorld{defaults: map[Kind]string{}}
	for _, d := range devs {
		if d.bufFrames == 0 {
			d.bufFrames = 480
		}
		d.activeFlag = true
		w.devices = append(w.devices, d)
		if _, ok := w.defaults[d.kind]; !ok {
			w.defaults[d.kind] = d.id
		}
	}
	return w
}

func (w *fakeWorld) logf(f string, a ...any) { w.log = append(w.log, fmt.Sprintf(f, a...)) }

func (w *fakeWorld) fail(f string, a ...any) { w.failures = append(w.failures, fmt.Sprintf(f, a...)) }

// call counts a COM call made by the engine; it must hold w.mu.
func (w *fakeWorld) call(f string, a ...any) { w.calls++; w.logf(f, a...) }

func (w *fakeWorld) device(id string) *fakeDevice {
	for _, d := range w.devices {
		if d.id == id {
			return d
		}
	}
	return nil
}

func (w *fakeWorld) logged() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.log, "\n")
}

func (w *fakeWorld) setDefault(kind Kind, id string) {
	w.mu.Lock()
	w.defaults[kind] = id
	w.mu.Unlock()
}

func (w *fakeWorld) setActive(id string, on bool) {
	w.mu.Lock()
	w.device(id).activeFlag = on
	w.mu.Unlock()
}

func (w *fakeWorld) notify() wasapiNotifier {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.notifier
}

// with runs f on a device under the world's lock.
func (w *fakeWorld) with(id string, f func(d *fakeDevice)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w.device(id))
}

func (w *fakeWorld) signal(id string) {
	w.mu.Lock()
	var e *fakeEvent
	if c := w.device(id).client; c != nil {
		e = c.event
	}
	w.mu.Unlock()
	if e != nil {
		e.set()
	}
}

func (w *fakeWorld) coInit() (func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.coInitErr != nil {
		return nil, w.coInitErr
	}
	w.live++
	w.logf("coInit")
	return func() {
		w.mu.Lock()
		w.live--
		w.logf("coUninit")
		w.mu.Unlock()
	}, nil
}

func (w *fakeWorld) enumerator() (wasapiEnumerator, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enumErr != nil {
		return nil, w.enumErr
	}
	w.live++
	w.logf("enumerator")
	return &fakeEnum{w: w}, nil
}

func (w *fakeWorld) newEvent() (wasapiEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.live++
	w.logf("event.new")
	return &fakeEvent{w: w, ch: make(chan struct{}, 1)}, nil
}

func (w *fakeWorld) prioritize() (func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.prioErr != nil {
		return nil, w.prioErr
	}
	w.live++
	w.logf("prioritize")
	return func() {
		w.mu.Lock()
		w.live--
		w.logf("revert")
		w.mu.Unlock()
	}, nil
}

func (w *fakeWorld) wait(a, b wasapiEvent, timeout time.Duration) (int, error) {
	ea, eb := a.(*fakeEvent), b.(*fakeEvent)
	select {
	case <-ea.ch:
		return 0, nil
	default:
	}
	var expire <-chan time.Time
	if timeout >= 0 {
		w.mu.Lock()
		w.waits = append(w.waits, timeout)
		w.mu.Unlock()
		t := time.NewTimer(timeout)
		defer t.Stop()
		expire = t.C
	}
	select {
	case <-ea.ch:
		return 0, nil
	case <-eb.ch:
		return 1, nil
	case <-expire:
		return 2, nil
	}
}

type fakeEnum struct {
	w *fakeWorld
}

func (e *fakeEnum) release() {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.live--
	e.w.logf("enumerator.release")
}

func (e *fakeEnum) endpoints(kind Kind) ([]wasapiEndpoint, error) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.call("endpoints %v", kind)
	if e.w.listErr != nil {
		return nil, e.w.listErr
	}
	var eps []wasapiEndpoint
	for _, d := range e.w.devices {
		if d.kind == kind && d.activeFlag {
			e.w.live++
			eps = append(eps, &fakeEndpoint{w: e.w, d: d})
		}
	}
	return eps, nil
}

func (e *fakeEnum) defaultEndpoint(kind Kind) (wasapiEndpoint, error) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.call("defaultEndpoint %v", kind)
	d := e.w.device(e.w.defaults[kind])
	if e.w.defaultFails > 0 {
		e.w.defaultFails--
		return nil, hrNotFound
	}
	if d == nil || !d.activeFlag {
		return nil, hrNotFound
	}
	e.w.live++
	return &fakeEndpoint{w: e.w, d: d}, nil
}

func (e *fakeEnum) endpoint(id string) (wasapiEndpoint, error) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.call("endpoint %s", id)
	d := e.w.device(id)
	if d == nil {
		return nil, hrNotFound
	}
	e.w.live++
	return &fakeEndpoint{w: e.w, d: d}, nil
}

func (e *fakeEnum) register(n wasapiNotifier) error {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.call("register")
	if e.w.notifier != nil {
		e.w.fail("registered twice")
	}
	e.w.notifier = n
	return nil
}

func (e *fakeEnum) unregister() error {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.call("unregister")
	e.w.notifier = nil
	return nil
}

type fakeEndpoint struct {
	w *fakeWorld
	d *fakeDevice
}

func (e *fakeEndpoint) release() {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.live--
	e.w.logf("endpoint.release %s", e.d.id)
}

func (e *fakeEndpoint) id() (string, error) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	if err := e.d.errs["id"]; err != nil {
		return "", err
	}
	return e.d.id, nil
}

func (e *fakeEndpoint) name() string { return e.d.name }

func (e *fakeEndpoint) activate() (wasapiClient, error) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.call("activate %s", e.d.id)
	if e.d.activateErr != nil {
		return nil, e.d.activateErr
	}
	e.w.live++
	c := &fakeClient{w: e.w, d: e.d}
	e.d.client = c
	return c, nil
}

type fakeClient struct {
	w       *fakeWorld
	d       *fakeDevice
	event   *fakeEvent
	started bool
	chans   int
}

func (c *fakeClient) err(op string) error {
	if err := c.d.errs[op]; err != nil {
		delete(c.d.errs, op)
		return err
	}
	return nil
}

func (c *fakeClient) release() {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.w.live--
	c.w.logf("client.release %s", c.d.id)
}

func (c *fakeClient) devicePeriod() (int64, int64, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if err := c.err("devicePeriod"); err != nil {
		return 0, 0, err
	}
	def, min := c.d.periodDef, c.d.periodMin
	if def == 0 {
		def = 100000
	}
	if min == 0 {
		min = 100000
	}
	return def, min, nil
}

func (c *fakeClient) initialize(f *waveFormat, flags uint32, duration int64) error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.w.call("initialize %s", c.d.id)
	c.d.inits = append(c.d.inits, fakeInit{*f, flags, duration})
	c.chans = int(f.channels)
	return c.d.initErr
}

func (c *fakeClient) setEvent(e wasapiEvent) error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.event = e.(*fakeEvent)
	return c.err("setEvent")
}

func (c *fakeClient) bufferFrames() (uint32, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	return c.d.bufFrames, c.err("bufferFrames")
}

func (c *fakeClient) padding() (uint32, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.d.padCalls++
	return c.d.pad, c.err("padding")
}

func (c *fakeClient) start() error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.w.call("start %s", c.d.id)
	c.started = true
	return c.err("start")
}

func (c *fakeClient) stop() error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.w.call("stop %s", c.d.id)
	c.started = false
	return nil
}

func (c *fakeClient) capture() (wasapiCapture, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if err := c.err("service"); err != nil {
		return nil, err
	}
	c.w.live++
	return &fakeCapture{c: c}, nil
}

func (c *fakeClient) render() (wasapiRender, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if err := c.err("service"); err != nil {
		return nil, err
	}
	c.w.live++
	return &fakeRenderClient{c: c, buf: make([]int16, int(c.d.bufFrames)*c.chans)}, nil
}

type fakeCapture struct{ c *fakeClient }

func (f *fakeCapture) release() {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	f.c.w.live--
	f.c.w.logf("capture.release %s", f.c.d.id)
}

func (f *fakeCapture) nextPacket() (uint32, error) {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	if err := f.c.err("nextPacket"); err != nil {
		return 0, err
	}
	if len(f.c.d.packets) == 0 {
		return 0, nil
	}
	return f.c.d.packets[0].frames, nil
}

func (f *fakeCapture) getBuffer() ([]int16, uint32, uint32, error) {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	if err := f.c.err("getBuffer"); err != nil {
		return nil, 0, 0, err
	}
	p := f.c.d.packets[0]
	if p.silent {
		return nil, p.frames, wasapiBufferSilent, nil
	}
	return p.data, p.frames, 0, nil
}

func (f *fakeCapture) releaseBuffer(frames uint32) error {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	f.c.d.packets = f.c.d.packets[1:]
	return f.c.err("releaseBuffer")
}

type fakeRenderClient struct {
	c   *fakeClient
	buf []int16
}

func (f *fakeRenderClient) release() {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	f.c.w.live--
	f.c.w.logf("render.release %s", f.c.d.id)
}

func (f *fakeRenderClient) getBuffer(frames uint32) ([]int16, error) {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	if err := f.c.err("getBuffer"); err != nil {
		return nil, err
	}
	if int(frames)*f.c.chans > len(f.buf) {
		return nil, hresult(0x88890006) // AUDCLNT_E_BUFFER_TOO_LARGE
	}
	for i := range f.buf {
		f.buf[i] = 0x5555 // what a driver would leave behind
	}
	return f.buf[:int(frames)*f.c.chans], nil
}

func (f *fakeRenderClient) releaseBuffer(frames, flags uint32) error {
	f.c.w.mu.Lock()
	defer f.c.w.mu.Unlock()
	f.c.d.rendered = append(f.c.d.rendered, fakeRender{frames, flags, slices.Clone(f.buf[:int(frames)*f.c.chans])})
	return f.c.err("releaseBuffer")
}

// fakeSink records what the backend delivers. Every call must come while
// no other call runs and, once closed is set, never.
type fakeSink struct {
	t       *testing.T
	mu      sync.Mutex
	got     []int16
	played  int
	stopped int
	busy    bool
	closed  bool
	block   chan struct{} // when set, onCapture waits for it
	entered chan struct{}
	fill    int16
}

func (s *fakeSink) enter(what string) {
	s.mu.Lock()
	if s.busy {
		s.t.Errorf("%s ran concurrently with another callback", what)
	}
	if s.closed {
		s.t.Errorf("%s after close returned", what)
	}
	s.busy = true
	s.mu.Unlock()
}

func (s *fakeSink) leave() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

func (s *fakeSink) onCapture(p []int16) {
	s.enter("onCapture")
	defer s.leave()
	s.mu.Lock()
	s.got = append(s.got, p...)
	block, entered := s.block, s.entered
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
}

func (s *fakeSink) onPlayback(p []int16) {
	s.enter("onPlayback")
	defer s.leave()
	s.mu.Lock()
	s.played += len(p)
	fill := s.fill
	s.mu.Unlock()
	for i := range p {
		p[i] = fill
	}
}

func (s *fakeSink) onStopped() {
	s.enter("onStopped")
	defer s.leave()
	s.mu.Lock()
	s.stopped++
	s.mu.Unlock()
}

func (s *fakeSink) snapshot() (got []int16, played, stopped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got), s.played, s.stopped
}

func (s *fakeSink) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func openFake(t *testing.T, w *fakeWorld, kind Kind, cfg Config) (hwStream, *fakeSink, error) {
	t.Helper()
	cfg, err := cfg.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{t: t, fill: 7}
	if len(wasapiRetry) == 5 && wasapiRetry[0] == 100*time.Millisecond {
		withRetry(t, 1, 1, 1)
	}
	hw, err := openWasapi(w, sink, kind, cfg)
	return hw, sink, err
}

func mustOpen(t *testing.T, w *fakeWorld, kind Kind, cfg Config) (hwStream, *fakeSink) {
	t.Helper()
	hw, sink, err := openFake(t, w, kind, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return hw, sink
}

// closeAndCheck closes hw, then requires that nothing is left open and the
// fake saw no misuse.
func closeAndCheck(t *testing.T, w *fakeWorld, hw hwStream, sink *fakeSink) {
	t.Helper()
	hw.close()
	sink.markClosed()
	checkWorld(t, w)
}

func checkWorld(t *testing.T, w *fakeWorld) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live != 0 {
		t.Errorf("%d COM objects, events or apartments left open\n%s", w.live, strings.Join(w.log, "\n"))
	}
	if w.notifier != nil {
		t.Error("notifier still registered")
	}
	for _, f := range w.failures {
		t.Error(f)
	}
}

var errFake = errors.New("fake failure")

// withRetry sets the reroute backoff for the test, in milliseconds.
func withRetry(t *testing.T, ms ...int) {
	t.Helper()
	old := wasapiRetry
	wasapiRetry = nil
	for _, m := range ms {
		wasapiRetry = append(wasapiRetry, time.Duration(m)*time.Millisecond)
	}
	t.Cleanup(func() { wasapiRetry = old })
}
