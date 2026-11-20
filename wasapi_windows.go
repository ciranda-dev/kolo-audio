//go:build windows && (amd64 || arm64)

package koloaudio

// The COM objects of wasapiAPI, called through their vtables.

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32         = windows.NewLazySystemDLL("ole32")
	procCoCreate  = ole32.NewProc("CoCreateInstance")
	procPropClear = ole32.NewProc("PropVariantClear")
	avrt          = windows.NewLazySystemDLL("avrt")
	procMmSet     = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	procMmRevert  = avrt.NewProc("AvRevertMmThreadCharacteristics")

	clsidEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidEnumerator   = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidNotify       = windows.GUID{Data1: 0x7991EEC9, Data2: 0x7E89, Data3: 0x4D85, Data4: [8]byte{0x83, 0x90, 0x6C, 0x70, 0x3C, 0xEC, 0x60, 0xC0}}
	iidUnknown      = windows.GUID{Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidAudioClient  = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidCapture      = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidRender       = windows.GUID{Data1: 0xF294ACFC, Data2: 0x3146, Data3: 0x4483, Data4: [8]byte{0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2}}
	pkeyFriendly    = propertyKey{windows.GUID{Data1: 0xA45C254E, Data2: 0xDF1C, Data3: 0x4EFD, Data4: [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}}, 14}
)

const (
	clsctxAll       = 0x17       // CLSCTX_ALL
	deviceStateOK   = 1          // DEVICE_STATE_ACTIVE
	vtLPWSTR        = 31         // VT_LPWSTR
	hrChangedMode   = 0x80010106 // RPC_E_CHANGED_MODE
	hrFalse         = 1          // S_FALSE
	hrNoInterface   = 0x80004002 // E_NOINTERFACE
	shareModeShared = 0          // AUDCLNT_SHAREMODE_SHARED
)

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// comObj is a COM interface pointer: an object whose first word points to
// its vtable.
type comObj struct{ vtbl *[16]uintptr }

// call invokes vtable entry i with the object as first argument. A failure
// HRESULT is returned as an hresult; success codes are nil.
//
//go:uintptrescapes
func (o *comObj) call(i int, args ...uintptr) error {
	a := make([]uintptr, 1, len(args)+1)
	a[0] = uintptr(unsafe.Pointer(o))
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[i], a...)
	if int32(r) < 0 {
		return hresult(r)
	}
	return nil
}

func (o *comObj) release() {
	if o != nil {
		o.call(2)
	}
}

type winAPI struct{}

func (winAPI) coInit() (func(), error) {
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err {
	case nil, syscall.Errno(hrFalse):
		return windows.CoUninitialize, nil
	case syscall.Errno(hrChangedMode):
		// The thread already has an apartment of another kind, which
		// also serves these calls and is not ours to leave.
		return func() {}, nil
	default:
		return nil, hresult(err.(syscall.Errno))
	}
}

func (winAPI) enumerator() (wasapiEnumerator, error) {
	var o *comObj
	r, _, _ := procCoCreate.Call(uintptr(unsafe.Pointer(&clsidEnumerator)), 0, clsctxAll, uintptr(unsafe.Pointer(&iidEnumerator)), uintptr(unsafe.Pointer(&o)))
	if int32(r) < 0 {
		return nil, hresult(r)
	}
	return &winEnumerator{o: o}, nil
}

type winEvent struct{ h windows.Handle }

func (winAPI) newEvent() (wasapiEvent, error) {
	h, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	return &winEvent{h}, nil
}

func (e *winEvent) set()   { windows.SetEvent(e.h) }
func (e *winEvent) close() { windows.CloseHandle(e.h) }

func (winAPI) wait(a, b wasapiEvent, timeout time.Duration) (int, error) {
	ms := uint32(windows.INFINITE)
	if timeout >= 0 {
		// Round up so a short wait never becomes a poll.
		ms = uint32(min((timeout+time.Millisecond-1)/time.Millisecond, windows.INFINITE-1))
	}
	r, err := windows.WaitForMultipleObjects([]windows.Handle{a.(*winEvent).h, b.(*winEvent).h}, false, ms)
	if r == uint32(windows.WAIT_TIMEOUT) {
		return 2, nil
	}
	if err != nil {
		return 0, err
	}
	if r > windows.WAIT_OBJECT_0+1 {
		return 0, fmt.Errorf("wait returned 0x%x", r)
	}
	return int(r - windows.WAIT_OBJECT_0), nil
}

// prioritize registers the thread for the "Audio" task of the multimedia
// class scheduler: scheduling category Medium, like the Playback and
// Capture tasks. "Pro Audio" is the High category meant for dedicated
// low-latency applications, which a library should not claim for its host.
func (winAPI) prioritize() (func(), error) {
	name, _ := windows.UTF16PtrFromString("Audio")
	var index uint32
	h, _, err := procMmSet.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&index)))
	if h == 0 {
		return nil, fmt.Errorf("AvSetMmThreadCharacteristics: %w", err)
	}
	return func() { procMmRevert.Call(h) }, nil
}

// winEnumerator is IMMDeviceEnumerator.
type winEnumerator struct {
	o   *comObj
	reg *notifyObj
}

func (e *winEnumerator) release() { e.o.release() }

func (e *winEnumerator) endpoints(kind Kind) ([]wasapiEndpoint, error) {
	var coll *comObj
	if err := e.o.call(3, uintptr(wasapiFlow(kind)), deviceStateOK, uintptr(unsafe.Pointer(&coll))); err != nil {
		return nil, err
	}
	defer coll.release()
	var n uint32
	if err := coll.call(3, uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, err
	}
	var eps []wasapiEndpoint
	for i := uint32(0); i < n; i++ {
		var d *comObj
		if err := coll.call(4, uintptr(i), uintptr(unsafe.Pointer(&d))); err != nil {
			continue
		}
		eps = append(eps, &winEndpoint{o: d})
	}
	return eps, nil
}

func (e *winEnumerator) defaultEndpoint(kind Kind) (wasapiEndpoint, error) {
	var d *comObj
	if err := e.o.call(4, uintptr(wasapiFlow(kind)), wasapiRoleConsole, uintptr(unsafe.Pointer(&d))); err != nil {
		return nil, err
	}
	return &winEndpoint{o: d}, nil
}

func (e *winEnumerator) endpoint(id string) (wasapiEndpoint, error) {
	u, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, hrNotFound
	}
	var d *comObj
	if err := e.o.call(5, uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&d))); err != nil {
		return nil, err
	}
	return &winEndpoint{o: d}, nil
}

// IMMNotificationClient. Its vtable is built once for the process:
// NewCallback has a process-wide limit and never frees. Each registration
// has its own small object that points to that vtable and names its
// target. The object is pinned and kept in notifyLive until its reference
// count reaches zero: the registration owns one reference, the enumerator
// takes its own, and the object may be called until both are released.
type notifyObj struct {
	vtbl   *[8]uintptr
	refs   atomic.Int32
	target atomic.Pointer[notifyTarget]
}

type notifyTarget struct{ n wasapiNotifier }

var (
	notifyOnce sync.Once
	notifyVtbl [8]uintptr
	notifyLive = struct {
		sync.Mutex
		m map[*notifyObj]*runtime.Pinner
	}{m: map[*notifyObj]*runtime.Pinner{}}
)

// notifyRelease drops one reference and forgets the object at zero.
func notifyRelease(o *notifyObj) int32 {
	n := o.refs.Add(-1)
	if n == 0 {
		notifyLive.Lock()
		if p, ok := notifyLive.m[o]; ok {
			p.Unpin()
			delete(notifyLive.m, o)
		}
		notifyLive.Unlock()
	}
	return n
}

func initNotifyVtbl() {
	notifyVtbl = [8]uintptr{
		windows.NewCallback(func(this *notifyObj, riid *windows.GUID, out *uintptr) uintptr {
			if *riid == iidUnknown || *riid == iidNotify {
				*out = uintptr(unsafe.Pointer(this))
				this.refs.Add(1)
				return 0
			}
			*out = 0
			return hrNoInterface
		}),
		windows.NewCallback(func(this *notifyObj) uintptr { return uintptr(this.refs.Add(1)) }),
		windows.NewCallback(func(this *notifyObj) uintptr { return uintptr(notifyRelease(this)) }),
		windows.NewCallback(func(this *notifyObj, id *uint16, state uint32) uintptr {
			if t := this.target.Load(); t != nil {
				t.n.deviceState(windows.UTF16PtrToString(id), state)
			}
			return 0
		}),
		windows.NewCallback(func(this *notifyObj, id *uint16) uintptr { return 0 }),
		windows.NewCallback(func(this *notifyObj, id *uint16) uintptr {
			if t := this.target.Load(); t != nil {
				t.n.deviceRemoved(windows.UTF16PtrToString(id))
			}
			return 0
		}),
		windows.NewCallback(func(this *notifyObj, flow, role uint32, id *uint16) uintptr {
			if t := this.target.Load(); t != nil {
				name := ""
				if id != nil {
					name = windows.UTF16PtrToString(id)
				}
				t.n.defaultChanged(flow, role, name)
			}
			return 0
		}),
		windows.NewCallback(func(this *notifyObj, id *uint16, key *propertyKey) uintptr { return 0 }),
	}
}

func (e *winEnumerator) register(n wasapiNotifier) error {
	notifyOnce.Do(initNotifyVtbl)
	o := &notifyObj{vtbl: &notifyVtbl}
	o.refs.Store(1)
	o.target.Store(&notifyTarget{n})
	pin := new(runtime.Pinner)
	pin.Pin(o)
	notifyLive.Lock()
	notifyLive.m[o] = pin
	notifyLive.Unlock()
	if err := e.o.call(6, uintptr(unsafe.Pointer(o))); err != nil {
		notifyRelease(o)
		return err
	}
	e.reg = o
	return nil
}

// unregister drops the registration's reference. The object stays pinned
// and live while the enumerator still holds one.
func (e *winEnumerator) unregister() error {
	o := e.reg
	if o == nil {
		return nil
	}
	e.reg = nil
	if err := e.o.call(7, uintptr(unsafe.Pointer(o))); err != nil {
		return err
	}
	o.target.Store(nil)
	notifyRelease(o)
	return nil
}

// winEndpoint is IMMDevice.
type winEndpoint struct{ o *comObj }

func (d *winEndpoint) release() { d.o.release() }

func (d *winEndpoint) id() (string, error) {
	var p *uint16
	if err := d.o.call(5, uintptr(unsafe.Pointer(&p))); err != nil {
		return "", err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), nil
}

func (d *winEndpoint) name() string {
	var store *comObj
	if d.o.call(4, 0 /* STGM_READ */, uintptr(unsafe.Pointer(&store))) != nil {
		return ""
	}
	defer store.release()
	var pv [24]byte // PROPVARIANT on 64-bit
	if store.call(5, uintptr(unsafe.Pointer(&pkeyFriendly)), uintptr(unsafe.Pointer(&pv))) != nil {
		return ""
	}
	defer procPropClear.Call(uintptr(unsafe.Pointer(&pv)))
	if *(*uint16)(unsafe.Pointer(&pv[0])) != vtLPWSTR {
		return ""
	}
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&pv[8])))
}

func (d *winEndpoint) activate() (wasapiClient, error) {
	var c *comObj
	if err := d.o.call(3, uintptr(unsafe.Pointer(&iidAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&c))); err != nil {
		return nil, err
	}
	return &winClient{o: c}, nil
}

// winClient is IAudioClient.
type winClient struct {
	o     *comObj
	chans int
}

func (c *winClient) release() { c.o.release() }

func (c *winClient) devicePeriod() (def, min int64, err error) {
	err = c.o.call(9, uintptr(unsafe.Pointer(&def)), uintptr(unsafe.Pointer(&min)))
	return
}

func (c *winClient) initialize(f *waveFormat, flags uint32, duration int64) error {
	c.chans = int(f.channels)
	return c.o.call(3, shareModeShared, uintptr(flags), uintptr(duration), 0, uintptr(unsafe.Pointer(f)), 0)
}

func (c *winClient) setEvent(e wasapiEvent) error { return c.o.call(13, uintptr(e.(*winEvent).h)) }

func (c *winClient) bufferFrames() (uint32, error) {
	var n uint32
	err := c.o.call(4, uintptr(unsafe.Pointer(&n)))
	return n, err
}

func (c *winClient) padding() (uint32, error) {
	var n uint32
	err := c.o.call(6, uintptr(unsafe.Pointer(&n)))
	return n, err
}

func (c *winClient) start() error { return c.o.call(10) }
func (c *winClient) stop() error  { return c.o.call(11) }

func (c *winClient) service(iid *windows.GUID) (*comObj, error) {
	var o *comObj
	if err := c.o.call(14, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	return o, nil
}

func (c *winClient) capture() (wasapiCapture, error) {
	o, err := c.service(&iidCapture)
	if err != nil {
		return nil, err
	}
	return &winCapture{o: o, chans: c.chans}, nil
}

func (c *winClient) render() (wasapiRender, error) {
	o, err := c.service(&iidRender)
	if err != nil {
		return nil, err
	}
	return &winRender{o: o, chans: c.chans}, nil
}

// samples views n frames at p as interleaved s16.
func samples(p *byte, frames uint32, chans int) []int16 {
	if p == nil || frames == 0 {
		return nil
	}
	return unsafe.Slice((*int16)(unsafe.Pointer(p)), int(frames)*chans)
}

// winCapture is IAudioCaptureClient.
type winCapture struct {
	o     *comObj
	chans int
}

func (c *winCapture) release() { c.o.release() }

func (c *winCapture) nextPacket() (uint32, error) {
	var n uint32
	err := c.o.call(5, uintptr(unsafe.Pointer(&n)))
	return n, err
}

func (c *winCapture) getBuffer() ([]int16, uint32, uint32, error) {
	var p *byte
	var frames, flags uint32
	if err := c.o.call(3, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0); err != nil {
		return nil, 0, 0, err
	}
	if flags&wasapiBufferSilent != 0 {
		return nil, frames, flags, nil
	}
	return samples(p, frames, c.chans), frames, flags, nil
}

func (c *winCapture) releaseBuffer(frames uint32) error { return c.o.call(4, uintptr(frames)) }

// winRender is IAudioRenderClient.
type winRender struct {
	o     *comObj
	chans int
}

func (r *winRender) release() { r.o.release() }

func (r *winRender) getBuffer(frames uint32) ([]int16, error) {
	var p *byte
	if err := r.o.call(3, uintptr(frames), uintptr(unsafe.Pointer(&p))); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("IAudioRenderClient::GetBuffer returned no buffer")
	}
	return samples(p, frames, r.chans), nil
}

func (r *winRender) releaseBuffer(frames, flags uint32) error {
	return r.o.call(4, uintptr(frames), uintptr(flags))
}
