//go:build darwin && (amd64 || arm64)

package koloaudio

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The real CoreAudio layer: AudioObject properties for the device list and
// AUHAL units for streams, called through purego. Every callback into Go is
// created once per process (purego never frees them and allows 2000) and
// finds its owner by the reference it was registered with.

type caPropAddr struct{ selector, scope, element uint32 }

type caCompDesc struct {
	typ, subType, manufacturer uint32
	_, _                       uint32 // flags and flagsMask
}

type caASBD struct {
	rate                                                                                    float64
	format, flags, bytesPerPacket, framesPerPacket, bytesPerFrame, channels, bits, reserved uint32
}

type caBuffer struct {
	channels, size uint32
	data           unsafe.Pointer
}

type caBufferList struct {
	n      uint32
	_      uint32
	buffer caBuffer
}

type caCallbackStruct struct{ proc, refCon uintptr }

const (
	caUTF8 = 0x08000100

	caPropStreamFormat   = 8
	caPropMaxFrames      = 14
	caPropSetRender      = 23
	caPropCurrentDevice  = 2000
	caPropEnableIO       = 2003
	caPropSetInputCB     = 2005
	caAUScopeGlobal      = 0
	caAUScopeInput       = 1
	caAUScopeOutput      = 2
	caFormatSignedPacked = 4 | 8
)

var (
	caSelDevices    = caFourCC("dev#")
	caSelUID        = caFourCC("uid ")
	caSelName       = caFourCC("lnam")
	caSelStreamConf = caFourCC("slay")
	caSelBufFrames  = caFourCC("fsiz")
	caSelBufRange   = caFourCC("fsz#")
	caSelRunLoop    = caFourCC("rnlp")
	caScopeGlobal   = caFourCC("glob")
	caScopeInput    = caFourCC("inpt")
	caScopeOutput   = caFourCC("outp")
)

var (
	caObjGetSize func(id uint32, a *caPropAddr, qsize uint32, q unsafe.Pointer, size *uint32) int32
	caObjGetData func(id uint32, a *caPropAddr, qsize uint32, q unsafe.Pointer, size *uint32, out unsafe.Pointer) int32
	caObjSetData func(id uint32, a *caPropAddr, qsize uint32, q unsafe.Pointer, size uint32, data unsafe.Pointer) int32
	caObjAddLis  func(id uint32, a *caPropAddr, proc uintptr, client uintptr) int32
	caObjDelLis  func(id uint32, a *caPropAddr, proc uintptr, client uintptr) int32

	caCFLength func(s uintptr) int
	caCFMax    func(length int, enc uint32) int
	caCFCStr   func(s uintptr, buf *byte, n int, enc uint32) bool
	caCFFree   func(p uintptr)

	caFindNext func(after uintptr, d *caCompDesc) uintptr
	caNewInst  func(c uintptr, out *uintptr) int32
	caDispose  func(u uintptr) int32
	caSetProp  func(u uintptr, prop, scope, elem uint32, data unsafe.Pointer, size uint32) int32
	caGetProp  func(u uintptr, prop, scope, elem uint32, data unsafe.Pointer, size *uint32) int32
	caInit     func(u uintptr) int32
	caUninit   func(u uintptr) int32
	caStart    func(u uintptr) int32
	caStop     func(u uintptr) int32
	caRender   func(u uintptr, flags, ts unsafe.Pointer, bus, frames uint32, list unsafe.Pointer) int32

	caListenerProc, caInputProc, caRenderProc uintptr
)

// caRefs maps the reference passed to CoreAudio as client data to its owner.
var (
	caRefs    sync.Map
	caNextRef atomic.Uintptr
)

func caRegister(v any) uintptr {
	ref := caNextRef.Add(1)
	caRefs.Store(ref, v)
	return ref
}

type halAPI struct{}

var (
	halOnce sync.Once
	halInst *halAPI
	halErr  error
)

// loadHAL loads the frameworks on first use.
func loadHAL() (*halAPI, error) {
	halOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				halErr = fmt.Errorf("%w: coreaudio: %v", ErrBackend, r)
			}
		}()
		open := func(name string) uintptr {
			h, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				panic(err)
			}
			return h
		}
		ca, cf, at := open("CoreAudio"), open("CoreFoundation"), open("AudioToolbox")
		purego.RegisterLibFunc(&caObjGetSize, ca, "AudioObjectGetPropertyDataSize")
		purego.RegisterLibFunc(&caObjGetData, ca, "AudioObjectGetPropertyData")
		purego.RegisterLibFunc(&caObjSetData, ca, "AudioObjectSetPropertyData")
		purego.RegisterLibFunc(&caObjAddLis, ca, "AudioObjectAddPropertyListener")
		purego.RegisterLibFunc(&caObjDelLis, ca, "AudioObjectRemovePropertyListener")
		purego.RegisterLibFunc(&caCFLength, cf, "CFStringGetLength")
		purego.RegisterLibFunc(&caCFMax, cf, "CFStringGetMaximumSizeForEncoding")
		purego.RegisterLibFunc(&caCFCStr, cf, "CFStringGetCString")
		purego.RegisterLibFunc(&caCFFree, cf, "CFRelease")
		purego.RegisterLibFunc(&caFindNext, at, "AudioComponentFindNext")
		purego.RegisterLibFunc(&caNewInst, at, "AudioComponentInstanceNew")
		purego.RegisterLibFunc(&caDispose, at, "AudioComponentInstanceDispose")
		purego.RegisterLibFunc(&caSetProp, at, "AudioUnitSetProperty")
		purego.RegisterLibFunc(&caGetProp, at, "AudioUnitGetProperty")
		purego.RegisterLibFunc(&caInit, at, "AudioUnitInitialize")
		purego.RegisterLibFunc(&caUninit, at, "AudioUnitUninitialize")
		purego.RegisterLibFunc(&caStart, at, "AudioOutputUnitStart")
		purego.RegisterLibFunc(&caStop, at, "AudioOutputUnitStop")
		purego.RegisterLibFunc(&caRender, at, "AudioUnitRender")

		caListenerProc = purego.NewCallback(func(obj, n uint32, addrs unsafe.Pointer, client uintptr) int32 {
			if fn, ok := caRefs.Load(client); ok {
				fn.(func())()
			}
			return 0
		})
		caInputProc = purego.NewCallback(func(ref uintptr, flags, ts unsafe.Pointer, bus, frames uint32, list unsafe.Pointer) int32 {
			if u, ok := caRefs.Load(ref); ok {
				return u.(*halUnit).input(flags, ts, bus, frames)
			}
			return 0
		})
		caRenderProc = purego.NewCallback(func(ref uintptr, flags, ts unsafe.Pointer, bus, frames uint32, list unsafe.Pointer) int32 {
			if u, ok := caRefs.Load(ref); ok {
				return u.(*halUnit).render(list)
			}
			return 0
		})

		halInst = &halAPI{}
	})
	return halInst, halErr
}

var (
	halRunLoopOnce sync.Once
	halRunLoopSet  atomic.Bool // the process-wide property has been changed
)

// setRunLoop moves the HAL's notifications off the main run loop, which a Go
// program does not run. It changes the whole process, so it waits for the
// first stream open: listing devices leaves it alone.
func (*halAPI) setRunLoop() {
	halRunLoopOnce.Do(func() {
		var none uintptr
		a := caPropAddr{caSelRunLoop, caScopeGlobal, 0}
		caObjSetData(caSystemObject, &a, 0, nil, uint32(unsafe.Sizeof(none)), unsafe.Pointer(&none))
		halRunLoopSet.Store(true)
	})
}

func caScope(kind Kind) uint32 {
	if kind == Capture {
		return caScopeInput
	}
	return caScopeOutput
}

func (*halAPI) getU32(obj, sel, scope uint32) (uint32, error) {
	var v uint32
	size := uint32(4)
	a := caPropAddr{sel, scope, 0}
	if st := caObjGetData(obj, &a, 0, nil, &size, unsafe.Pointer(&v)); st != 0 {
		return 0, caStatusError("AudioObjectGetPropertyData", st)
	}
	return v, nil
}

func (*halAPI) devices() ([]uint32, error) {
	a := caPropAddr{caSelDevices, caScopeGlobal, 0}
	var size uint32
	if st := caObjGetSize(caSystemObject, &a, 0, nil, &size); st != 0 {
		return nil, caStatusError("AudioObjectGetPropertyDataSize", st)
	}
	ids := make([]uint32, size/4)
	if len(ids) == 0 {
		return ids, nil
	}
	if st := caObjGetData(caSystemObject, &a, 0, nil, &size, unsafe.Pointer(&ids[0])); st != 0 {
		return nil, caStatusError("AudioObjectGetPropertyData", st)
	}
	return ids[:size/4], nil
}

func (h *halAPI) defaultDevice(kind Kind) uint32 {
	id, err := h.getU32(caSystemObject, caDefaultSelector(kind), caScopeGlobal)
	if err != nil {
		return 0
	}
	return id
}

func (*halAPI) stringProp(obj, sel uint32) (string, error) {
	var ref uintptr
	size := uint32(unsafe.Sizeof(ref))
	a := caPropAddr{sel, caScopeGlobal, 0}
	if st := caObjGetData(obj, &a, 0, nil, &size, unsafe.Pointer(&ref)); st != 0 {
		return "", caStatusError("AudioObjectGetPropertyData", st)
	}
	if ref == 0 {
		return "", nil
	}
	defer caCFFree(ref)
	n := caCFMax(caCFLength(ref), caUTF8)
	if n < 0 {
		return "", fmt.Errorf("%w: coreaudio: string too long", ErrBackend)
	}
	buf := make([]byte, n+1)
	if !caCFCStr(ref, &buf[0], len(buf), caUTF8) {
		return "", fmt.Errorf("%w: coreaudio: string conversion", ErrBackend)
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf), nil
}

func (h *halAPI) uid(dev uint32) (string, error)  { return h.stringProp(dev, caSelUID) }
func (h *halAPI) name(dev uint32) (string, error) { return h.stringProp(dev, caSelName) }

// channels sums the channels of every stream of dev in the scope of kind.
func (*halAPI) channels(dev uint32, kind Kind) int {
	a := caPropAddr{caSelStreamConf, caScope(kind), 0}
	var size uint32
	if caObjGetSize(dev, &a, 0, nil, &size) != 0 || size < 8 {
		return 0
	}
	buf := make([]byte, size)
	if caObjGetData(dev, &a, 0, nil, &size, unsafe.Pointer(&buf[0])) != 0 || int(size) > len(buf) {
		return 0
	}
	buf = buf[:size]
	n := int(binary.NativeEndian.Uint32(buf))
	total := 0
	// AudioBufferList: a UInt32 count, padding to 8, then 16-byte buffers
	// that start with their channel count.
	for i := 0; i < n && 8+16*i+4 <= len(buf); i++ {
		total += int(binary.NativeEndian.Uint32(buf[8+16*i:]))
	}
	return total
}

func (h *halAPI) alive(dev uint32) bool {
	v, err := h.getU32(dev, caSelAlive, caScopeGlobal)
	return err == nil && v != 0
}

func (*halAPI) nominalRate(dev uint32) (int, error) {
	var rate float64
	size := uint32(8)
	a := caPropAddr{caSelRate, caScopeGlobal, 0}
	if st := caObjGetData(dev, &a, 0, nil, &size, unsafe.Pointer(&rate)); st != 0 {
		return 0, caStatusError("AudioObjectGetPropertyData", st)
	}
	return int(math.Round(rate)), nil
}

func (*halAPI) listen(obj, sel uint32, fn func()) (func(), error) {
	ref := caRegister(fn)
	a := caPropAddr{sel, caScopeGlobal, 0}
	if st := caObjAddLis(obj, &a, caListenerProc, ref); st != 0 {
		caRefs.Delete(ref)
		return nil, caStatusError("AudioObjectAddPropertyListener", st)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			a := caPropAddr{sel, caScopeGlobal, 0}
			caObjDelLis(obj, &a, caListenerProc, ref)
			caRefs.Delete(ref)
		})
	}, nil
}

// halUnit is an AUHAL unit with a client format of interleaved s16 at the
// device's rate and channel count.
type halUnit struct {
	kind Kind
	unit uintptr
	ref  uintptr
	fn   unitFunc
	rate int
	ch   int

	// mu is held for reading by every callback; close takes it for writing,
	// so the unit is never used after it is disposed.
	mu      sync.RWMutex
	dead    bool
	scratch []int16
	list    caBufferList
	pin     runtime.Pinner // holds scratch in place while the unit reads through list
}

// setScratch replaces the capture buffer and pins it, because list passes its address to the unit.
func (u *halUnit) setScratch(n int) {
	u.pin.Unpin()
	u.scratch = make([]int16, n)
	if n > 0 {
		u.pin.Pin(&u.scratch[0])
	}
}

func caSetU32(u uintptr, prop, scope, elem, v uint32) int32 {
	return caSetProp(u, prop, scope, elem, unsafe.Pointer(&v), 4)
}

func (h *halAPI) newUnit(kind Kind, dev uint32, period time.Duration, fn unitFunc) (audioUnit, error) {
	desc := caCompDesc{typ: caFourCC("auou"), subType: caFourCC("ahal"), manufacturer: caFourCC("appl")}
	comp := caFindNext(0, &desc)
	if comp == 0 {
		return nil, fmt.Errorf("%w: coreaudio: no AUHAL component", ErrBackend)
	}
	u := &halUnit{kind: kind, fn: fn}
	if st := caNewInst(comp, &u.unit); st != 0 {
		return nil, caStatusError("AudioComponentInstanceNew", st)
	}
	if err := u.configure(h, dev, period); err != nil {
		caDispose(u.unit)
		u.pin.Unpin()
		return nil, err
	}
	return u, nil
}

func (u *halUnit) configure(h *halAPI, dev uint32, period time.Duration) error {
	hwScope, hwElem := uint32(caAUScopeOutput), uint32(0)
	clientScope, clientElem := uint32(caAUScopeInput), uint32(0)
	if u.kind == Capture {
		hwScope, hwElem = caAUScopeInput, 1
		clientScope, clientElem = caAUScopeOutput, 1
		if st := caSetU32(u.unit, caPropEnableIO, caAUScopeInput, 1, 1); st != 0 {
			return caStatusError("AudioUnitSetProperty(EnableIO input)", st)
		}
		if st := caSetU32(u.unit, caPropEnableIO, caAUScopeOutput, 0, 0); st != 0 {
			return caStatusError("AudioUnitSetProperty(EnableIO output)", st)
		}
	}
	if st := caSetU32(u.unit, caPropCurrentDevice, caAUScopeGlobal, 0, dev); st != 0 {
		return caStatusError("AudioUnitSetProperty(CurrentDevice)", st)
	}
	var hw caASBD
	size := uint32(unsafe.Sizeof(hw))
	if st := caGetProp(u.unit, caPropStreamFormat, hwScope, hwElem, unsafe.Pointer(&hw), &size); st != 0 {
		return caStatusError("AudioUnitGetProperty(StreamFormat)", st)
	}
	u.rate, u.ch = int(math.Round(hw.rate)), int(hw.channels)
	if u.rate < 1 || u.ch < 1 || u.ch > 1<<10 {
		return fmt.Errorf("%w: coreaudio: device format %v Hz, %d channels", ErrUnsupportedFormat, hw.rate, hw.channels)
	}
	client := caASBD{
		rate: hw.rate, format: caFourCC("lpcm"), flags: caFormatSignedPacked,
		bytesPerPacket: 2 * hw.channels, framesPerPacket: 1, bytesPerFrame: 2 * hw.channels,
		channels: hw.channels, bits: 16,
	}
	if st := caSetProp(u.unit, caPropStreamFormat, clientScope, clientElem, unsafe.Pointer(&client), uint32(unsafe.Sizeof(client))); st != 0 {
		return caStatusError("AudioUnitSetProperty(StreamFormat)", st)
	}
	u.ref = caRegister(u)
	cb := caCallbackStruct{proc: caRenderProc, refCon: u.ref}
	prop, scope := uint32(caPropSetRender), uint32(caAUScopeInput)
	if u.kind == Capture {
		cb.proc = caInputProc
		prop, scope = caPropSetInputCB, caAUScopeGlobal
	}
	if st := caSetProp(u.unit, prop, scope, 0, unsafe.Pointer(&cb), uint32(unsafe.Sizeof(cb))); st != 0 {
		caRefs.Delete(u.ref)
		return caStatusError("AudioUnitSetProperty(callback)", st)
	}
	h.setBufferFrames(dev, u.kind, period, u.rate)
	if st := caInit(u.unit); st != 0 {
		caRefs.Delete(u.ref)
		return caStatusError("AudioUnitInitialize", st)
	}
	var maxFrames uint32
	size = 4
	if caGetProp(u.unit, caPropMaxFrames, caAUScopeGlobal, 0, unsafe.Pointer(&maxFrames), &size) == 0 {
		u.setScratch(int(maxFrames) * u.ch)
	}
	return nil
}

// setBufferFrames asks the device for callbacks every period, within the
// range it supports. A device that refuses keeps its own period.
func (h *halAPI) setBufferFrames(dev uint32, kind Kind, period time.Duration, rate int) {
	var rng struct{ min, max float64 }
	size := uint32(unsafe.Sizeof(rng))
	a := caPropAddr{caSelBufRange, caScopeGlobal, 0}
	if caObjGetData(dev, &a, 0, nil, &size, unsafe.Pointer(&rng)) != 0 {
		return
	}
	frames := math.Ceil(period.Seconds() * float64(rate))
	frames = math.Min(math.Max(frames, rng.min), rng.max)
	v := uint32(frames)
	a.selector = caSelBufFrames
	caObjSetData(dev, &a, 0, nil, 4, unsafe.Pointer(&v))
}

func (u *halUnit) format() (int, int) { return u.rate, u.ch }

func (u *halUnit) start() error {
	if st := caStart(u.unit); st != 0 {
		return caStatusError("AudioOutputUnitStart", st)
	}
	return nil
}

func (u *halUnit) stop() { caStop(u.unit) }

func (u *halUnit) close() {
	u.mu.Lock()
	u.dead = true
	u.mu.Unlock()
	caRefs.Delete(u.ref)
	caUninit(u.unit)
	caDispose(u.unit)
	u.mu.Lock()
	u.pin.Unpin()
	u.mu.Unlock()
}

// input is the capture callback: it pulls the recorded frames out of the
// unit and passes them on.
func (u *halUnit) input(flags, ts unsafe.Pointer, bus, frames uint32) int32 {
	u.mu.RLock()
	defer u.mu.RUnlock()
	n := int(frames) * u.ch
	if u.dead || n == 0 {
		return 0
	}
	if n > len(u.scratch) {
		u.setScratch(n)
	}
	u.list = caBufferList{n: 1, buffer: caBuffer{channels: uint32(u.ch), size: uint32(n * 2), data: unsafe.Pointer(&u.scratch[0])}}
	if st := caRender(u.unit, flags, ts, bus, frames, unsafe.Pointer(&u.list)); st != 0 {
		return st
	}
	u.fn(u.scratch[:n])
	return 0
}

// render is the playback callback: it fills the buffer the unit hands over.
func (u *halUnit) render(list unsafe.Pointer) int32 {
	l := (*caBufferList)(list)
	if l == nil || l.n < 1 || l.buffer.data == nil {
		return 0
	}
	buf := unsafe.Slice((*int16)(l.buffer.data), int(l.buffer.size)/2/u.ch*u.ch)
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.dead {
		clear(buf)
		return 0
	}
	u.fn(buf)
	return 0
}
