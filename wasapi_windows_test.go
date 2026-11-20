//go:build windows && (amd64 || arm64)

package koloaudio

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// withEnumerator runs f on a locked thread in an apartment with a real
// IMMDeviceEnumerator. It never opens an audio client.
func withEnumerator(t *testing.T, f func(en wasapiEnumerator)) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	uninit, err := winAPI{}.coInit()
	if err != nil {
		t.Fatal(err)
	}
	defer uninit()
	en, err := winAPI{}.enumerator()
	if err != nil {
		t.Fatal(err)
	}
	defer en.release()
	f(en)
}

func TestWasapiEnumerate(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		devs, err := hwList(kind)
		if errors.Is(err, ErrNoDevice) {
			t.Skipf("%v: no audio service on this host: %v", kind, err)
		}
		if err != nil {
			t.Fatalf("%v: %v", kind, err)
		}
		t.Logf("%v: %d endpoints", kind, len(devs))
		defaults := 0
		for _, d := range devs {
			id, err := wasapiEndpointID(d.ID)
			if err != nil {
				t.Errorf("%v: %v", d, err)
				continue
			}
			t.Logf("  default=%v name=%q endpoint=%q id=%s", d.Default, d.Name, id, d.ID)
			if d.Default {
				defaults++
			}
			if d.Name == "" || d.Kind != kind {
				t.Errorf("incomplete device %+v", d)
			}
			withEnumerator(t, func(en wasapiEnumerator) {
				ep, err := en.endpoint(id)
				if err != nil {
					t.Errorf("GetDevice(%q): %v", id, err)
					return
				}
				defer ep.release()
				if got, err := ep.id(); err != nil || got != id {
					t.Errorf("endpoint ID %q, %v, want %q", got, err, id)
				}
			})
		}
		if defaults > 1 {
			t.Errorf("%v: %d default endpoints", kind, defaults)
		}
	}
}

func TestWasapiEnumerateUnknownEndpoint(t *testing.T) {
	withEnumerator(t, func(en wasapiEnumerator) {
		if ep, err := en.endpoint(`{0.0.0.00000000}.{00000000-0000-0000-0000-000000000000}`); err == nil {
			ep.release()
			t.Error("an unknown endpoint was found")
		}
	})
}

// TestWasapiEnumerateInSTA lists from a thread that already has a
// single-threaded apartment, as a GUI thread has.
func TestWasapiEnumerateInSTA(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil {
			done <- err
			return
		}
		defer windows.CoUninitialize()
		_, err := hwList(Playback)
		done <- err
	}()
	if err := <-done; errors.Is(err, ErrNoDevice) {
		t.Skipf("no audio service on this host: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
}

type countNotifier struct{ n atomic.Int64 }

func (c *countNotifier) deviceState(string, uint32)           { c.n.Add(1) }
func (c *countNotifier) deviceRemoved(string)                 { c.n.Add(1) }
func (c *countNotifier) defaultChanged(_, _ uint32, _ string) { c.n.Add(1) }

func TestWasapiNotifyRegister(t *testing.T) {
	withEnumerator(t, func(en wasapiEnumerator) {
		a, b := &countNotifier{}, &countNotifier{}
		if err := en.register(a); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if err := en.unregister(); err != nil {
			t.Fatal(err)
		}
		if err := en.unregister(); err != nil {
			t.Errorf("second unregister: %v", err)
		}
		if err := en.register(b); err != nil {
			t.Fatal(err)
		}
		if err := en.unregister(); err != nil {
			t.Fatal(err)
		}
		t.Logf("notifications seen: %d and %d", a.n.Load(), b.n.Load())
	})
	notifyLive.Lock()
	n := len(notifyLive.m)
	notifyLive.Unlock()
	if n != 0 {
		t.Errorf("%d notification objects kept after unregister", n)
	}
}

type recordNotifier struct {
	state   []string
	removed []string
	changed []string
}

func (r *recordNotifier) deviceState(id string, st uint32) {
	r.state = append(r.state, id+"/"+string(rune('0'+st)))
}
func (r *recordNotifier) deviceRemoved(id string) { r.removed = append(r.removed, id) }
func (r *recordNotifier) defaultChanged(flow, role uint32, id string) {
	r.changed = append(r.changed, string(rune('0'+flow))+string(rune('0'+role))+id)
}

// TestWasapiNotifyCallbacks calls the registered object's vtable the way
// Windows does, with no device change needed, and checks the arguments
// reach the notifier and the COM identity rules hold.
func TestWasapiNotifyCallbacks(t *testing.T) {
	withEnumerator(t, func(en wasapiEnumerator) {
		rec := &recordNotifier{}
		if err := en.register(rec); err != nil {
			t.Fatal(err)
		}
		o := en.(*winEnumerator).reg
		obj := (*comObj)(unsafe.Pointer(o))
		id, _ := windows.UTF16PtrFromString(`{0.0.0.00000000}.{x}`)
		idp := uintptr(unsafe.Pointer(id))

		var out *comObj
		for _, iid := range []*windows.GUID{&iidUnknown, &iidNotify} {
			out = nil
			if err := obj.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); err != nil || out != obj {
				t.Errorf("QueryInterface: %v, %p", err, out)
			}
		}
		if err := obj.call(0, uintptr(unsafe.Pointer(&iidAudioClient)), uintptr(unsafe.Pointer(&out))); err != hresult(hrNoInterface) || out != nil {
			t.Errorf("QueryInterface of another interface: %v, %p", err, out)
		}
		refs := o.refs.Load()
		obj.call(1)
		obj.call(1)
		obj.call(2)
		if got := o.refs.Load(); got != refs+1 {
			t.Errorf("refs %d, want %d", got, refs+1)
		}

		if err := obj.call(3, idp, 2); err != nil {
			t.Error(err)
		}
		if err := obj.call(4, idp); err != nil {
			t.Error(err)
		}
		if err := obj.call(5, idp); err != nil {
			t.Error(err)
		}
		if err := obj.call(6, 1, 0, idp); err != nil {
			t.Error(err)
		}
		if err := obj.call(6, 0, 2, 0); err != nil { // no default endpoint
			t.Error(err)
		}
		key := propertyKey{pid: 14}
		if err := obj.call(7, idp, uintptr(unsafe.Pointer(&key))); err != nil {
			t.Error(err)
		}
		want := `{0.0.0.00000000}.{x}`
		if len(rec.state) != 1 || rec.state[0] != want+"/2" || len(rec.removed) != 1 || rec.removed[0] != want ||
			len(rec.changed) != 2 || rec.changed[0] != "10"+want || rec.changed[1] != "02" {
			t.Errorf("notifier saw %+v", rec)
		}
		if err := en.unregister(); err != nil {
			t.Fatal(err)
		}
		// After unregistering a late call reaches nothing.
		obj.call(3, idp, 4)
		if len(rec.state) != 1 {
			t.Error("a notification arrived after unregister")
		}
		for o.refs.Load() > 0 { // the references taken above
			obj.call(2)
		}
		if notifyIsLive(o) {
			t.Error("object kept at zero references")
		}
	})
}

func notifyIsLive(o *notifyObj) bool {
	notifyLive.Lock()
	defer notifyLive.Unlock()
	_, ok := notifyLive.m[o]
	return ok
}

// TestWasapiNotifyLifetime checks that the object stays live (and pinned) while a reference is held
// after unregistration, as the enumerator may still call Release on it, and is forgotten at zero.
func TestWasapiNotifyLifetime(t *testing.T) {
	withEnumerator(t, func(en wasapiEnumerator) {
		if err := en.register(&countNotifier{}); err != nil {
			t.Fatal(err)
		}
		o := en.(*winEnumerator).reg
		obj := (*comObj)(unsafe.Pointer(o))
		obj.call(1) // a reference held elsewhere
		if err := en.unregister(); err != nil {
			t.Fatal(err)
		}
		if !notifyIsLive(o) || o.refs.Load() != 1 {
			t.Errorf("object dropped with refs %d, live %v", o.refs.Load(), notifyIsLive(o))
		}
		obj.call(2)
		if notifyIsLive(o) || o.refs.Load() != 0 {
			t.Errorf("object kept with refs %d", o.refs.Load())
		}
	})
}

// TestWasapiNotifyManyRegistrations registers more often than NewCallback
// allows callbacks (2000): the vtable is made once and every registration
// reuses it.
func TestWasapiNotifyManyRegistrations(t *testing.T) {
	withEnumerator(t, func(en wasapiEnumerator) {
		n := &countNotifier{}
		for i := 0; i < 2500; i++ {
			if err := en.register(n); err != nil {
				t.Fatalf("registration %d: %v", i, err)
			}
			if err := en.unregister(); err != nil {
				t.Fatalf("unregistration %d: %v", i, err)
			}
		}
	})
}

// TestWasapiNotifyTwoEnumerators registers two objects at once: each keeps
// its own target.
func TestWasapiNotifyTwoEnumerators(t *testing.T) {
	withEnumerator(t, func(a wasapiEnumerator) {
		withEnumerator(t, func(b wasapiEnumerator) {
			if err := a.register(&countNotifier{}); err != nil {
				t.Fatal(err)
			}
			if err := b.register(&countNotifier{}); err != nil {
				t.Fatal(err)
			}
			notifyLive.Lock()
			n := len(notifyLive.m)
			notifyLive.Unlock()
			if n != 2 {
				t.Errorf("%d live notification objects, want 2", n)
			}
			if err := a.unregister(); err != nil {
				t.Error(err)
			}
			if err := b.unregister(); err != nil {
				t.Error(err)
			}
		})
	})
}

// TestWasapiRealPriority registers a locked thread with the multimedia class
// scheduler and reverts it. A host without the service reports an error,
// which the stream tolerates; the test records which happened.
func TestWasapiRealPriority(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	revert, err := winAPI{}.prioritize()
	if err != nil {
		t.Logf("not registered (a stream runs at normal priority): %v", err)
		return
	}
	t.Log("registered with the Audio task")
	revert()
}

func TestWasapiWait(t *testing.T) {
	a, _ := winAPI{}.newEvent()
	b, _ := winAPI{}.newEvent()
	defer a.close()
	defer b.close()
	api := winAPI{}
	start := time.Now()
	if i, err := api.wait(a, b, 50*time.Millisecond); i != 2 || err != nil || time.Since(start) < 40*time.Millisecond {
		t.Errorf("timeout: %d, %v after %v", i, err, time.Since(start))
	}
	b.set()
	if i, err := api.wait(a, b, -1); i != 1 || err != nil {
		t.Errorf("b: %d, %v", i, err)
	}
	a.set()
	b.set()
	if i, err := api.wait(a, b, 0); i != 0 || err != nil {
		t.Errorf("both: %d, %v", i, err)
	}
	if i, err := api.wait(a, b, time.Hour); i != 1 || err != nil {
		t.Errorf("b after a was consumed: %d, %v", i, err)
	}
	if i, err := api.wait(a, b, time.Nanosecond); i != 2 || err != nil {
		t.Errorf("tiny timeout: %d, %v", i, err)
	}
}

// TestWasapiDevicePeriod activates the default playback endpoint and reads
// its period. Nothing is initialized or started, so no sound is made.
func TestWasapiDevicePeriod(t *testing.T) {
	withEnumerator(t, func(en wasapiEnumerator) {
		ep, err := en.defaultEndpoint(Playback)
		if err != nil {
			t.Skipf("no default playback endpoint: %v", err)
		}
		defer ep.release()
		c, err := ep.activate()
		if err != nil {
			t.Fatal(err)
		}
		defer c.release()
		def, min, err := c.devicePeriod()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("default period %d, minimum %d (100 ns units); a 10 ms request becomes %d", def, min, wasapiDuration(DefaultPeriod, min))
		if def <= 0 || min <= 0 {
			t.Errorf("period %d %d", def, min)
		}
	})
}

// TestWasapiHardwareReopen opens and closes the default capture and playback
// streams, which uses the real microphone and speakers (silence is played).
// It runs only with KOLO_AUDIO_HW_TEST=1.
func TestWasapiHardwareReopen(t *testing.T) {
	if os.Getenv("KOLO_AUDIO_HW_TEST") != "1" {
		t.Skip("set KOLO_AUDIO_HW_TEST=1 to use real audio devices")
	}
	SetNullBackend(false)
	t.Cleanup(func() { SetNullBackend(true) })
	for round := 0; round < 3; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		in, err := OpenInput(Config{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		out, err := OpenOutput(Config{})
		if err != nil {
			in.Close()
			cancel()
			t.Fatal(err)
		}
		buf := make([]int16, 480)
		if _, err := in.Read(ctx, buf); err != nil {
			t.Error(err)
		}
		if _, err := out.Write(ctx, make([]int16, 4800)); err != nil {
			t.Error(err)
		}
		t.Logf("round %d: input %+v output %+v", round, in.Stats(), out.Stats())
		in.Close()
		out.Close()
		cancel()
		if n := LiveDevices(); n != 0 {
			t.Errorf("%d devices left open", n)
		}
	}
}
