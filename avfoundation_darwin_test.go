//go:build darwin && (amd64 || arm64)

package koloaudio

import (
	"errors"
	"io/fs"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// avFake is AVFoundation with a fixed status. Its request answers with
// answer, or never when answer is nil.
type avFake struct {
	st       int64
	loads    *atomic.Int32
	requests atomic.Int32
	answer   *bool
}

func (f *avFake) status() int64 { return f.st }

func (f *avFake) request(done func(bool)) {
	f.requests.Add(1)
	if f.answer != nil {
		go done(*f.answer)
	}
}

// useAVFake replaces AVFoundation and turns the null backend off for one test.
func useAVFake(t *testing.T, f *avFake, err error) {
	old := loadAV
	f.loads = new(atomic.Int32)
	loadAV = func() (avCapture, error) {
		f.loads.Add(1)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	nullBackend.Store(false)
	t.Cleanup(func() {
		loadAV = old
		nullBackend.Store(true)
	})
}

func TestAVAccessMapping(t *testing.T) {
	want := map[int64]Access{0: AccessNotDetermined, 1: AccessRestricted, 2: AccessDenied, 3: AccessAuthorized}
	for st := int64(-1); st <= 4; st++ {
		a, err := avAccess(st)
		w, known := want[st]
		if known && (a != w || err != nil) || !known && (a != 0 || !errors.Is(err, ErrBackend)) {
			t.Errorf("status %d: %v, %v", st, a, err)
		}
	}
}

func TestAVAccessStatus(t *testing.T) {
	for st, want := range map[int64]Access{0: AccessNotDetermined, 1: AccessRestricted, 2: AccessDenied, 3: AccessAuthorized} {
		f := &avFake{st: st}
		useAVFake(t, f, nil)
		if a, err := AccessStatus(); a != want || err != nil {
			t.Errorf("status %d: %v, %v", st, a, err)
		}
		if f.requests.Load() != 0 {
			t.Errorf("status %d: requested access", st)
		}
	}
	useAVFake(t, &avFake{st: 7}, nil)
	if _, err := AccessStatus(); !errors.Is(err, ErrBackend) {
		t.Errorf("unknown status: %v", err)
	}
	boom := errors.New("boom")
	useAVFake(t, &avFake{}, boom)
	if _, err := AccessStatus(); err != boom {
		t.Errorf("load error: %v", err)
	}
	if err := RequestAccess(bounded(t)); err != boom {
		t.Errorf("load error: %v", err)
	}
}

func TestAVRequestAccess(t *testing.T) {
	synctest.Test(t, testAVRequestAccess)
}

func testAVRequestAccess(t *testing.T) {
	yes, no := true, false
	f := &avFake{st: 0, answer: &yes}
	useAVFake(t, f, nil)
	if err := RequestAccess(bounded(t)); err != nil || f.requests.Load() != 1 {
		t.Errorf("granted: %v, %d requests", err, f.requests.Load())
	}
	f.answer = &no
	if err := RequestAccess(bounded(t)); !errors.Is(err, ErrPermission) {
		t.Errorf("refused: %v", err)
	}
	for _, st := range []int64{1, 2, 3} {
		f := &avFake{st: st, answer: &yes}
		useAVFake(t, f, nil)
		err := RequestAccess(bounded(t))
		if (st == 3) != (err == nil) || f.requests.Load() != 0 {
			t.Errorf("status %d: %v, %d requests", st, err, f.requests.Load())
		}
	}
}

func TestAVNullBackendNeverLoads(t *testing.T) {
	synctest.Test(t, testAVNullBackendNeverLoads)
}

func testAVNullBackendNeverLoads(t *testing.T) {
	f := &avFake{st: 0}
	useAVFake(t, f, nil)
	nullBackend.Store(true)
	if a, err := AccessStatus(); a != AccessAuthorized || err != nil {
		t.Errorf("AccessStatus = %v, %v", a, err)
	}
	if err := RequestAccess(bounded(t)); err != nil {
		t.Errorf("RequestAccess = %v", err)
	}
	if f.loads.Load() != 0 || f.requests.Load() != 0 {
		t.Errorf("null backend reached AVFoundation: %d loads, %d requests", f.loads.Load(), f.requests.Load())
	}
}

// TestAVFoundationStatusReal reads the real authorization status. Reading
// never prompts; this test never requests access.
func TestAVFoundationStatusReal(t *testing.T) {
	av, err := loadAVFoundation()
	if err != nil {
		t.Fatal(err)
	}
	again, err := loadAVFoundation()
	if err != nil || again != av {
		t.Fatalf("second load: %v", err)
	}
	st := av.status()
	a, err := avAccess(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("microphone access: %v", a)
}

func TestCoreAudioCaptureDenied(t *testing.T) {
	for _, st := range []int32{int32(caFourCC("!hog")), -10847} {
		backend := caStatusError("AudioOutputUnitStart", st)
		err := captureDenied(Capture, backend)
		if !errors.Is(err, ErrPermission) || !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("status %d: %v", st, err)
		}
		if errors.Is(captureDenied(Playback, backend), ErrPermission) {
			t.Errorf("status %d: playback refusal is ErrPermission", st)
		}
	}
}

// blockProbe returns a done function whose closure holds an object with a
// cleanup, so freed reports when nothing references done any more.
func blockProbe() (done func(bool), got chan bool, freed chan struct{}) {
	held := new([64]byte)
	got, freed = make(chan bool, 1), make(chan struct{})
	runtime.AddCleanup(held, func(ch chan struct{}) { close(ch) }, freed)
	return func(g bool) { held[0] = 1; got <- g }, got, freed
}

// TestAVBlockLifetime runs the completion block as AVFoundation would: copy
// it, invoke it through its C entry point from another thread, release the
// copy. A Go function stands in for the system call, so nothing prompts.
func TestAVBlockLifetime(t *testing.T) {
	if _, err := loadAVFoundation(); err != nil {
		t.Fatal(err)
	}
	done, got, freed := blockProbe()
	avWithBlock(done, func(b objc.Block) {
		c := b.Copy()
		go func() {
			// A block starts with isa, flags, reserved and then invoke.
			invoke := *(*uintptr)(unsafe.Add(*(*unsafe.Pointer)(unsafe.Pointer(&c)), 16))
			purego.SyscallN(invoke, uintptr(c), 1)
			c.Release()
		}()
	})
	deadline := time.After(10 * time.Second)
	select {
	case g := <-got:
		if !g {
			t.Error("block delivered false")
		}
	case <-deadline:
		t.Fatal("block never ran")
	}
	for {
		runtime.GC()
		select {
		case <-freed:
			return
		case <-deadline:
			t.Fatal("block not freed after its last release")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCaptureAllowed(t *testing.T) {
	for st, ok := range map[int64]bool{0: true, 1: false, 2: false, 3: true} {
		useAVFake(t, &avFake{st: st}, nil)
		err := captureAllowed(hwAccessStatus)
		if ok != (err == nil) {
			t.Errorf("status %d: %v", st, err)
		}
		if !ok && (!errors.Is(err, ErrPermission) || !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission)) {
			t.Errorf("status %d: %v", st, err)
		}
	}
	useAVFake(t, &avFake{st: 7}, nil)
	if err := captureAllowed(hwAccessStatus); !errors.Is(err, ErrBackend) {
		t.Errorf("unknown status: %v", err)
	}
	boom := errors.New("boom")
	useAVFake(t, &avFake{}, boom)
	if err := captureAllowed(hwAccessStatus); err != boom {
		t.Errorf("load error: %v", err)
	}
}

// TestOpenInputRefused opens capture with a refused microphone. The refusal comes before CoreAudio is
// loaded, so no device is touched.
func TestOpenInputRefused(t *testing.T) {
	for _, st := range []int64{1, 2} {
		f := &avFake{st: st}
		useAVFake(t, f, nil)
		live := liveDevices.Load()
		in, err := OpenInput(Config{})
		if in != nil || !errors.Is(err, ErrPermission) || !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("status %d: %v, %v", st, in, err)
		}
		if f.requests.Load() != 0 || liveDevices.Load() != live {
			t.Errorf("status %d: %d requests, %d live devices", st, f.requests.Load(), liveDevices.Load()-live)
		}
	}
}
