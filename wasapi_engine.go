package koloaudio

// The WASAPI backend logic. Every call into Windows goes through the wasapi*
// interfaces below (wasapi_windows.go, or a fake in tests), so this file builds
// and is tested on every platform.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// hresult is a failure HRESULT returned by a COM call.
type hresult uint32

func (h hresult) Error() string { return fmt.Sprintf("HRESULT 0x%08x", uint32(h)) }

const (
	hrAccessDenied        hresult = 0x80070005 // E_ACCESSDENIED
	hrNotFound            hresult = 0x80070490 // E_NOTFOUND
	hrDeviceInvalidated   hresult = 0x88890004 // AUDCLNT_E_DEVICE_INVALIDATED
	hrUnsupportedFormat   hresult = 0x88890008 // AUDCLNT_E_UNSUPPORTED_FORMAT
	hrServiceNotRunning   hresult = 0x88890010 // AUDCLNT_E_SERVICE_NOT_RUNNING
	hrResourcesInvalidate hresult = 0x88890026 // AUDCLNT_E_RESOURCES_INVALIDATED
)

// Values from audioclient.h, mmdeviceapi.h and mmreg.h.
const (
	wasapiStreamFlags  = 0x00040000 | 0x80000000 | 0x08000000 // EVENTCALLBACK | AUTOCONVERTPCM | SRC_DEFAULT_QUALITY
	wasapiBufferSilent = 0x2                                  // AUDCLNT_BUFFERFLAGS_SILENT
	wasapiStateActive  = 0x1                                  // DEVICE_STATE_ACTIVE
	wasapiRoleConsole  = 0                                    // eConsole
	wasapiFlowRender   = 0                                    // eRender
	wasapiFlowCapture  = 1                                    // eCapture
)

func wasapiFlow(kind Kind) uint32 {
	if kind == Capture {
		return wasapiFlowCapture
	}
	return wasapiFlowRender
}

// waveFormat is WAVEFORMATEXTENSIBLE with the layout Windows reads. A
// format of one or two channels is sent as plain WAVEFORMATEX (cbSize 0),
// where Windows reads only the first 18 bytes.
type waveFormat struct {
	tag        uint16
	channels   uint16
	rate       uint32
	avgBytes   uint32
	blockAlign uint16
	bits       uint16
	cbSize     uint16
	validBits  uint16
	mask       uint32
	subFormat  [16]byte
}

// subtypePCM is KSDATAFORMAT_SUBTYPE_PCM.
var subtypePCM = [16]byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71}

// speakerMasks holds the dwChannelMask of 3 to 8 channels (SPEAKER_*
// positions of ksmedia.h: 3.0, quad, 5.0, 5.1, 6.1, 7.1). More channels
// have no position, so their mask is 0.
var speakerMasks = [...]uint32{3: 0x7, 4: 0x33, 5: 0x37, 6: 0x3F, 7: 0x70F, 8: 0x63F}

func newWaveFormat(f Format) waveFormat {
	w := waveFormat{
		tag:        1, // WAVE_FORMAT_PCM
		channels:   uint16(f.Channels),
		rate:       uint32(f.SampleRate),
		blockAlign: uint16(2 * f.Channels),
		bits:       16,
	}
	w.avgBytes = w.rate * uint32(w.blockAlign)
	if f.Channels > 2 {
		w.tag = 0xFFFE // WAVE_FORMAT_EXTENSIBLE
		w.cbSize = 22
		w.validBits = 16
		w.subFormat = subtypePCM
		if f.Channels < len(speakerMasks) {
			w.mask = speakerMasks[f.Channels]
		}
	}
	return w
}

// wasapiID is the Device.ID of an endpoint: the hex of its UTF-16LE
// endpoint ID string, without the terminator (what miniaudio stores).
func wasapiID(endpoint string) string {
	u := utf16.Encode([]rune(endpoint))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return hex.EncodeToString(b)
}

func wasapiEndpointID(id string) (string, error) {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) == 0 || len(b)%2 != 0 {
		return "", fmt.Errorf("%w: id %q", ErrNoDevice, id)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u)), nil
}

// The interfaces below are the COM objects the backend uses. Every method
// runs on a thread that called coInit.

type wasapiAPI interface {
	// coInit enters a multithreaded apartment on the calling thread, which
	// must be locked to it. The returned function leaves it.
	coInit() (func(), error)
	enumerator() (wasapiEnumerator, error)
	// newEvent makes an auto-reset event, initially not signalled.
	newEvent() (wasapiEvent, error)
	// wait blocks until a or b is signalled and returns 0 or 1, or until
	// timeout passes and returns 2; a wins when both are signalled. A
	// negative timeout waits without limit.
	wait(a, b wasapiEvent, timeout time.Duration) (int, error)
	// prioritize registers the calling thread with the multimedia class
	// scheduler. The returned function reverts it on the same thread.
	prioritize() (func(), error)
}

type wasapiEvent interface {
	set()
	close()
}

type wasapiEnumerator interface {
	// endpoints lists the active endpoints of kind.
	endpoints(kind Kind) ([]wasapiEndpoint, error)
	defaultEndpoint(kind Kind) (wasapiEndpoint, error)
	endpoint(id string) (wasapiEndpoint, error)
	register(n wasapiNotifier) error
	unregister() error
	release()
}

// wasapiNotifier receives IMMNotificationClient calls. Implementations
// return at once and never call back into the enumerator: Windows makes
// the calls on its own threads.
type wasapiNotifier interface {
	deviceState(id string, state uint32)
	deviceRemoved(id string)
	defaultChanged(flow, role uint32, id string)
}

type wasapiEndpoint interface {
	id() (string, error)
	// name is the friendly name, or empty.
	name() string
	activate() (wasapiClient, error)
	release()
}

type wasapiClient interface {
	// devicePeriod returns the default and minimum device period in 100 ns
	// units.
	devicePeriod() (def, min int64, err error)
	// initialize starts a shared-mode stream of f with the given stream
	// flags and buffer duration (100 ns units) and a periodicity of 0, which
	// shared mode requires.
	initialize(f *waveFormat, flags uint32, duration int64) error
	setEvent(e wasapiEvent) error
	bufferFrames() (uint32, error)
	padding() (uint32, error)
	start() error
	stop() error
	capture() (wasapiCapture, error)
	render() (wasapiRender, error)
	release()
}

type wasapiCapture interface {
	nextPacket() (uint32, error)
	// getBuffer returns the next packet. data is nil when the packet is
	// flagged silent.
	getBuffer() (data []int16, frames, flags uint32, err error)
	releaseBuffer(frames uint32) error
	release()
}

type wasapiRender interface {
	getBuffer(frames uint32) ([]int16, error)
	releaseBuffer(frames, flags uint32) error
	release()
}

// wasapiSink is the stream the backend delivers to.
type wasapiSink interface {
	onCapture([]int16)
	onPlayback([]int16)
	onStopped()
}

// wasapiError maps a failed COM call to the package's errors.
func wasapiError(op string, kind Kind, err error) error {
	var h hresult
	if !errors.As(err, &h) {
		return fmt.Errorf("%w: wasapi %s: %w", ErrBackend, op, err)
	}
	switch h {
	case hrAccessDenied:
		return fmt.Errorf("%w: %w: wasapi %s: %w", ErrBackend, fs.ErrPermission, op, h)
	case hrUnsupportedFormat:
		return fmt.Errorf("%w: wasapi %s: AUDCLNT_E_UNSUPPORTED_FORMAT: %w", ErrUnsupportedFormat, op, h)
	case hrServiceNotRunning:
		return fmt.Errorf("%w: wasapi %s: AUDCLNT_E_SERVICE_NOT_RUNNING (the Windows Audio service is not running): %w", ErrNoDevice, op, h)
	case hrNotFound:
		return fmt.Errorf("%w: wasapi %s: no %v endpoint: %w", ErrNoDevice, op, kind, h)
	}
	return fmt.Errorf("%w: wasapi %s: %w", ErrBackend, op, h)
}

func wasapiInvalidated(err error) bool {
	var h hresult
	return errors.As(err, &h) && (h == hrDeviceInvalidated || h == hrResourcesInvalidate)
}

// listWasapi lists the active endpoints of kind.
func listWasapi(api wasapiAPI, kind Kind) ([]Device, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	uninit, err := api.coInit()
	if err != nil {
		return nil, wasapiError("CoInitializeEx", kind, err)
	}
	defer uninit()
	en, err := api.enumerator()
	if err != nil {
		return nil, wasapiError("MMDeviceEnumerator", kind, err)
	}
	defer en.release()
	var defID string
	if d, err := en.defaultEndpoint(kind); err == nil {
		defID, _ = d.id()
		d.release()
	}
	eps, err := en.endpoints(kind)
	if err != nil {
		return nil, wasapiError("EnumAudioEndpoints", kind, err)
	}
	devs := make([]Device, 0, len(eps))
	for _, ep := range eps {
		id, err := ep.id()
		name := ep.name()
		ep.release()
		if err != nil {
			continue
		}
		if name == "" {
			name = id
		}
		devs = append(devs, Device{ID: wasapiID(id), Name: name, Kind: kind, Default: id == defID})
	}
	return devs, nil
}

// wasapiDuration is the buffer duration to request, in 100 ns units: the
// period, but never below the device's minimum. It is not rounded to a
// multiple of the device period (only exclusive mode needs that): a minimum
// of 2.67 ms would turn a 10 ms request into 10.67 ms.
func wasapiDuration(period time.Duration, minPeriod int64) int64 {
	return max(int64(period/100), minPeriod)
}

// wasapiRetry is the wait before each further attempt to open the new
// default device after a reroute fails: five retries over 3.1 s, doubling.
var wasapiRetry = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// Control bits the notifier and the owner set for the audio thread.
const (
	wasapiQuit    = 1 << iota // close
	wasapiStop                // stop: report the device as lost
	wasapiGone                // the endpoint in use left
	wasapiDefault             // the default endpoint changed
)

// wasapiStream is an open stream. One goroutine, locked to an OS thread in
// a multithreaded apartment, makes every COM call and every delivery, so
// close waits for it and nothing is delivered afterwards.
type wasapiStream struct {
	api    wasapiAPI
	sink   wasapiSink
	kind   Kind
	format waveFormat
	chans  int
	period time.Duration
	retry  []time.Duration
	follow bool
	fixed  string // endpoint ID when not following the default

	ctl atomic.Uint32
	cur atomic.Pointer[string] // endpoint ID in use

	// priorityErr is why the thread has no multimedia scheduling, and
	// cause is why a reroute gave up; both are set by the audio thread.
	priorityErr error
	cause       error

	mu   sync.Mutex
	wake wasapiEvent // nil once the stream is torn down
	done chan struct{}
}

// openWasapi opens and starts a shared-mode stream at cfg.Format on the
// device cfg.DeviceID, or on the default device when it is empty. The
// device converts to s16 at the requested rate and channel count.
func openWasapi(api wasapiAPI, sink wasapiSink, kind Kind, cfg Config) (hwStream, error) {
	w := &wasapiStream{
		api:    api,
		sink:   sink,
		kind:   kind,
		format: newWaveFormat(cfg.Format),
		chans:  cfg.Format.Channels,
		period: cfg.Period,
		retry:  wasapiRetry,
		follow: cfg.DeviceID == "",
		done:   make(chan struct{}),
	}
	if !w.follow {
		id, err := wasapiEndpointID(cfg.DeviceID)
		if err != nil {
			return nil, err
		}
		w.fixed = id
	}
	ready := make(chan error, 1)
	go w.run(ready)
	if err := <-ready; err != nil {
		<-w.done
		return nil, err
	}
	return w, nil
}

// post sets a control bit and wakes the audio thread. It never blocks.
func (w *wasapiStream) post(bit uint32) {
	w.mu.Lock()
	w.ctl.Or(bit)
	if w.wake != nil {
		w.wake.set()
	}
	w.mu.Unlock()
}

func (w *wasapiStream) close() {
	w.post(wasapiQuit)
	<-w.done
}

func (w *wasapiStream) stop() {
	w.post(wasapiStop)
	<-w.done
}

func (w *wasapiStream) current(id string) bool {
	c := w.cur.Load()
	return c != nil && *c == id
}

func (w *wasapiStream) deviceState(id string, state uint32) {
	if state != wasapiStateActive && w.current(id) {
		w.post(wasapiGone)
	}
}

func (w *wasapiStream) deviceRemoved(id string) {
	if w.current(id) {
		w.post(wasapiGone)
	}
}

func (w *wasapiStream) defaultChanged(flow, role uint32, id string) {
	if w.follow && role == wasapiRoleConsole && flow == wasapiFlow(w.kind) && !w.current(id) {
		w.post(wasapiDefault)
	}
}

// wasapiSession is the device the stream is bound to.
type wasapiSession struct {
	id      string
	ep      wasapiEndpoint
	client  wasapiClient
	capture wasapiCapture
	render  wasapiRender
	frames  uint32
	silence []int16
}

func (s *wasapiSession) close() {
	if s.client != nil {
		s.client.stop()
	}
	if s.capture != nil {
		s.capture.release()
	}
	if s.render != nil {
		s.render.release()
	}
	if s.client != nil {
		s.client.release()
	}
	s.ep.release()
}

// openSession initializes ep and starts it, releasing ep on failure.
func (w *wasapiStream) openSession(ep wasapiEndpoint, audio wasapiEvent) (*wasapiSession, error) {
	s := &wasapiSession{ep: ep}
	fail := func(op string, err error) (*wasapiSession, error) {
		s.close()
		return nil, wasapiError(op, w.kind, err)
	}
	id, err := ep.id()
	if err != nil {
		return fail("IMMDevice::GetId", err)
	}
	s.id = id
	if s.client, err = ep.activate(); err != nil {
		return fail("IMMDevice::Activate", err)
	}
	_, minPeriod, err := s.client.devicePeriod()
	if err != nil {
		return fail("IAudioClient::GetDevicePeriod", err)
	}
	if err = s.client.initialize(&w.format, wasapiStreamFlags, wasapiDuration(w.period, minPeriod)); err != nil {
		return fail("IAudioClient::Initialize", err)
	}
	if err = s.client.setEvent(audio); err != nil {
		return fail("IAudioClient::SetEventHandle", err)
	}
	if s.frames, err = s.client.bufferFrames(); err != nil {
		return fail("IAudioClient::GetBufferSize", err)
	}
	if w.kind == Capture {
		s.silence = make([]int16, int(s.frames)*w.chans)
		if s.capture, err = s.client.capture(); err != nil {
			return fail("IAudioClient::GetService(IAudioCaptureClient)", err)
		}
	} else {
		if s.render, err = s.client.render(); err != nil {
			return fail("IAudioClient::GetService(IAudioRenderClient)", err)
		}
		// Start with a buffer of silence, as the event-driven render
		// pattern requires, without calling the sink.
		if _, err = s.render.getBuffer(s.frames); err != nil {
			return fail("IAudioRenderClient::GetBuffer", err)
		}
		if err = s.render.releaseBuffer(s.frames, wasapiBufferSilent); err != nil {
			return fail("IAudioRenderClient::ReleaseBuffer", err)
		}
	}
	if err = s.client.start(); err != nil {
		return fail("IAudioClient::Start", err)
	}
	w.cur.Store(&id)
	return s, nil
}

func (w *wasapiStream) endpointFor(en wasapiEnumerator) (wasapiEndpoint, error) {
	if w.follow {
		ep, err := en.defaultEndpoint(w.kind)
		if err != nil {
			return nil, wasapiError("GetDefaultAudioEndpoint", w.kind, err)
		}
		return ep, nil
	}
	ep, err := en.endpoint(w.fixed)
	if err != nil {
		return nil, wasapiError("IMMDeviceEnumerator::GetDevice", w.kind, err)
	}
	return ep, nil
}

func (w *wasapiStream) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(w.done)
	su, teardown, err := w.setup()
	if err != nil {
		ready <- err
		return
	}
	defer teardown()
	ready <- nil
	w.loop(su)
}

// wasapiSetup holds what run releases.
type wasapiSetup struct {
	en      wasapiEnumerator
	wake    wasapiEvent
	audio   wasapiEvent
	session *wasapiSession
}

// setup enters COM, makes the events, registers for notifications and opens
// the device. teardown undoes it in reverse order: notifications end before
// the wake event is closed.
func (w *wasapiStream) setup() (*wasapiSetup, func(), error) {
	var steps []func()
	teardown := func() {
		for i := len(steps) - 1; i >= 0; i-- {
			steps[i]()
		}
	}
	su := &wasapiSetup{}
	fail := func(op string, err error) (*wasapiSetup, func(), error) {
		teardown()
		return nil, nil, wasapiError(op, w.kind, err)
	}
	uninit, err := w.api.coInit()
	if err != nil {
		return fail("CoInitializeEx", err)
	}
	steps = append(steps, uninit)
	// Without it the stream still runs, at normal priority.
	if revert, err := w.api.prioritize(); err != nil {
		w.priorityErr = err
	} else {
		steps = append(steps, revert)
	}
	if su.en, err = w.api.enumerator(); err != nil {
		return fail("MMDeviceEnumerator", err)
	}
	steps = append(steps, su.en.release)
	if su.wake, err = w.api.newEvent(); err != nil {
		return fail("CreateEvent", err)
	}
	steps = append(steps, func() {
		w.mu.Lock()
		w.wake = nil
		w.mu.Unlock()
		su.wake.close()
	})
	if su.audio, err = w.api.newEvent(); err != nil {
		return fail("CreateEvent", err)
	}
	steps = append(steps, su.audio.close)
	w.mu.Lock()
	w.wake = su.wake
	w.mu.Unlock()
	if err = su.en.register(w); err != nil {
		return fail("RegisterEndpointNotificationCallback", err)
	}
	steps = append(steps, func() { su.en.unregister() })
	ep, err := w.endpointFor(su.en)
	if err != nil {
		teardown()
		return nil, nil, err
	}
	if su.session, err = w.openSession(ep, su.audio); err != nil {
		teardown()
		return nil, nil, err
	}
	steps = append(steps, func() {
		if su.session != nil {
			su.session.close()
		}
	})
	return su, teardown, nil
}

// loop delivers audio until the stream is closed, stopped or lost. A lost
// endpoint is replaced by the new default when the stream follows it.
func (w *wasapiStream) loop(su *wasapiSetup) {
	for {
		i, err := w.api.wait(su.wake, su.audio, -1)
		if err != nil {
			w.sink.onStopped()
			return
		}
		if i == 0 {
			c := w.ctl.Swap(0)
			switch {
			case c&wasapiQuit != 0:
				return
			case c&wasapiStop != 0:
				w.sink.onStopped()
				return
			case c&(wasapiGone|wasapiDefault) != 0:
				if !w.rerouteOrStop(su, c&wasapiGone != 0) {
					return
				}
			}
			continue
		}
		if err := w.pump(su.session); err != nil {
			if !wasapiInvalidated(err) {
				w.sink.onStopped()
				return
			}
			if !w.rerouteOrStop(su, true) {
				return
			}
		}
	}
}

// rerouteOrStop reroutes, and reports false after calling onStopped when
// the stream cannot continue. A close or stop that arrives while it waits
// is left for the loop to handle.
func (w *wasapiStream) rerouteOrStop(su *wasapiSetup, force bool) bool {
	if !w.follow {
		w.sink.onStopped()
		return false
	}
	var err error
	for attempt := 0; ; attempt++ {
		if err = w.reroute(su, force); err == nil {
			return true
		}
		if attempt == len(w.retry) {
			break
		}
		if c := w.sleep(su, w.retry[attempt]); c != 0 {
			w.post(c)
			return true
		}
	}
	w.cause = err
	w.sink.onStopped()
	return false
}

// sleep waits d and returns the close or stop bits that arrive meanwhile.
// Other notifications are dropped: the next attempt reads the default anew.
func (w *wasapiStream) sleep(su *wasapiSetup, d time.Duration) uint32 {
	deadline := time.Now().Add(d)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return 0
		}
		i, err := w.api.wait(su.wake, su.audio, rem)
		if err != nil || i == 2 {
			return 0
		}
		if i == 0 {
			if c := w.ctl.Swap(0) & (wasapiQuit | wasapiStop); c != 0 {
				return c
			}
		}
	}
}

// reroute moves a stream that follows the default to the current default
// endpoint. force reopens even when the default has the ID in use, which is
// how a lost endpoint is replaced. A failed attempt leaves no session.
func (w *wasapiStream) reroute(su *wasapiSetup, force bool) error {
	ep, err := su.en.defaultEndpoint(w.kind)
	if err != nil {
		if su.session != nil {
			su.session.close()
			su.session = nil
		}
		return wasapiError("GetDefaultAudioEndpoint", w.kind, err)
	}
	if id, err := ep.id(); !force && err == nil && w.current(id) && su.session != nil {
		ep.release()
		return nil
	}
	if su.session != nil {
		su.session.close()
		su.session = nil
	}
	su.session, err = w.openSession(ep, su.audio)
	return err
}

// pump moves the frames the device has ready.
func (w *wasapiStream) pump(s *wasapiSession) error {
	if w.kind == Capture {
		return w.pumpCapture(s)
	}
	return w.pumpRender(s)
}

func (w *wasapiStream) pumpCapture(s *wasapiSession) error {
	for {
		n, err := s.capture.nextPacket()
		if err != nil || n == 0 {
			return err
		}
		data, frames, flags, err := s.capture.getBuffer()
		if err != nil {
			return err
		}
		samples := int(frames) * w.chans
		if flags&wasapiBufferSilent != 0 || data == nil {
			if samples > len(s.silence) {
				s.silence = make([]int16, samples)
			}
			data = s.silence[:samples]
			clear(data)
		} else if len(data) < samples {
			s.capture.releaseBuffer(frames)
			return fmt.Errorf("%w: wasapi capture packet of %d samples, want %d", ErrBackend, len(data), samples)
		}
		w.sink.onCapture(data[:samples])
		if err := s.capture.releaseBuffer(frames); err != nil {
			return err
		}
	}
}

func (w *wasapiStream) pumpRender(s *wasapiSession) error {
	pad, err := s.client.padding()
	if err != nil || pad >= s.frames {
		return err
	}
	n := s.frames - pad
	data, err := s.render.getBuffer(n)
	if err != nil {
		return err
	}
	samples := int(n) * w.chans
	if len(data) < samples {
		s.render.releaseBuffer(n, wasapiBufferSilent)
		return fmt.Errorf("%w: wasapi render buffer of %d samples, want %d", ErrBackend, len(data), samples)
	}
	w.sink.onPlayback(data[:samples])
	return s.render.releaseBuffer(n, 0)
}
