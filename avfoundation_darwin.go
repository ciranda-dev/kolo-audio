//go:build darwin && (amd64 || arm64)

package koloaudio

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// avCapture is the part of AVFoundation that AccessStatus and RequestAccess
// use. The real one calls the system framework; tests replace it.
type avCapture interface {
	// status is the AVAuthorizationStatus for audio. It never prompts.
	status() int64
	// request may show the system prompt and calls done on a system thread.
	request(done func(granted bool))
}

var loadAV = loadAVFoundation

func hwAccessStatus() (Access, error) {
	av, err := loadAV()
	if err != nil {
		return 0, err
	}
	return avAccess(av.status())
}

func hwAskAccess(done func(granted bool)) error {
	av, err := loadAV()
	if err != nil {
		return err
	}
	av.request(done)
	return nil
}

// avAccess maps an AVAuthorizationStatus.
func avAccess(st int64) (Access, error) {
	switch st {
	case 0:
		return AccessNotDetermined, nil
	case 1:
		return AccessRestricted, nil
	case 2:
		return AccessDenied, nil
	case 3:
		return AccessAuthorized, nil
	}
	return 0, fmt.Errorf("%w: avfoundation: authorization status %d", ErrBackend, st)
}

var (
	avOnce           sync.Once
	avErr            error
	avDevice         objc.ID
	avMediaTypeAudio objc.ID
	avPoolPush       func() uintptr
	avPoolPop        func(p uintptr)

	avSelStatus  = objc.RegisterName("authorizationStatusForMediaType:")
	avSelRequest = objc.RegisterName("requestAccessForMediaType:completionHandler:")
)

// loadAVFoundation loads the framework on first use.
func loadAVFoundation() (avCapture, error) {
	avOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				avErr = fmt.Errorf("%w: avfoundation: %v", ErrBackend, r)
			}
		}()
		open := func(path string) uintptr {
			h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				panic(err)
			}
			return h
		}
		av := open("/System/Library/Frameworks/AVFoundation.framework/AVFoundation")
		ob := open("/usr/lib/libobjc.A.dylib")
		purego.RegisterLibFunc(&avPoolPush, ob, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&avPoolPop, ob, "objc_autoreleasePoolPop")
		addr, err := purego.Dlsym(av, "AVMediaTypeAudio")
		if err != nil {
			panic(err)
		}
		avMediaTypeAudio = *(*objc.ID)(*(*unsafe.Pointer)(unsafe.Pointer(&addr)))
		avDevice = objc.ID(objc.GetClass("AVCaptureDevice"))
		if avDevice == 0 || avMediaTypeAudio == 0 {
			panic("AVCaptureDevice or AVMediaTypeAudio missing")
		}
	})
	if avErr != nil {
		return nil, avErr
	}
	return avFoundation{}, nil
}

// avPool runs fn in an autorelease pool, which purego does not provide. The
// pool belongs to the thread that pushed it, so fn runs on a locked thread.
func avPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := avPoolPush()
	defer avPoolPop(p)
	fn()
}

type avFoundation struct{}

func (avFoundation) status() int64 {
	var st int64
	avPool(func() { st = objc.Send[int64](avDevice, avSelStatus, avMediaTypeAudio) })
	return st
}

// request needs no run loop: AVFoundation calls the completion block on a
// dispatch queue of its own.
func (avFoundation) request(done func(granted bool)) {
	avWithBlock(done, func(b objc.Block) { avDevice.Send(avSelRequest, avMediaTypeAudio, b) })
}

// avWithBlock passes send a completion block that calls done. The receiver
// copies the block, so this reference is released when send returns.
func avWithBlock(done func(granted bool), send func(b objc.Block)) {
	avPool(func() {
		b := objc.NewBlock(func(_ objc.Block, granted bool) { done(granted) })
		defer b.Release()
		send(b)
	})
}
