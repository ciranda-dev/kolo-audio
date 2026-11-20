package koloaudio

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAccessString(t *testing.T) {
	for a, want := range map[Access]string{
		AccessNotDetermined: "not determined",
		AccessRestricted:    "restricted",
		AccessDenied:        "denied",
		AccessAuthorized:    "authorized",
		0:                   "Access(0)",
		5:                   "Access(5)",
	} {
		if got := a.String(); got != want {
			t.Errorf("Access(%d).String() = %q, want %q", int(a), got, want)
		}
	}
}

// fakeAsk is a consent step that records its calls and answers with answer,
// or never when answer is nil.
type fakeAsk struct {
	calls  atomic.Int32
	answer func(done func(bool))
	err    error
}

func (f *fakeAsk) ask(done func(bool)) error {
	f.calls.Add(1)
	if f.err != nil {
		return f.err
	}
	if f.answer != nil {
		go f.answer(done)
	}
	return nil
}

func fixed(a Access) func() (Access, error) { return func() (Access, error) { return a, nil } }

func isRefusal(err error, a Access) bool {
	return errors.Is(err, ErrPermission) && errors.Is(err, fs.ErrPermission) && strings.Contains(err.Error(), "("+a.String()+")")
}

// bounded is a context for a synctest bubble: a regression that waits for an
// answer fails at once with context.DeadlineExceeded instead of hanging.
func bounded(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestRequestAccessDecided(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, a := range []Access{AccessAuthorized, AccessRestricted, AccessDenied} {
			f := &fakeAsk{}
			err := requestAccess(bounded(t), fixed(a), f.ask)
			if a == AccessAuthorized && err != nil || a != AccessAuthorized && !isRefusal(err, a) {
				t.Errorf("%v: %v", a, err)
			}
			if f.calls.Load() != 0 {
				t.Errorf("%v: asked", a)
			}
		}
		f := &fakeAsk{}
		boom := errors.New("boom")
		if err := requestAccess(bounded(t), func() (Access, error) { return 0, boom }, f.ask); err != boom || f.calls.Load() != 0 {
			t.Errorf("status error: %v, %d asks", err, f.calls.Load())
		}
	})
}

func TestRequestAccessAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeAsk{answer: func(done func(bool)) { done(true) }}
		if err := requestAccess(bounded(t), fixed(AccessNotDetermined), f.ask); err != nil || f.calls.Load() != 1 {
			t.Errorf("granted: %v, %d asks", err, f.calls.Load())
		}
		f.answer = func(done func(bool)) { done(false); done(true) }
		if err := requestAccess(bounded(t), fixed(AccessNotDetermined), f.ask); !isRefusal(err, AccessDenied) {
			t.Errorf("refused: %v", err)
		}
		f.err = errors.New("boom")
		if err := requestAccess(bounded(t), fixed(AccessNotDetermined), f.ask); err != f.err {
			t.Errorf("ask error: %v", err)
		}
		// An answer given before ask returns must not be lost.
		at := func(done func(bool)) error { done(true); return nil }
		if err := requestAccess(bounded(t), fixed(AccessNotDetermined), at); err != nil {
			t.Errorf("answer during ask: %v", err)
		}
	})
}

func TestRequestAccessContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeAsk{}
	if err := requestAccess(ctx, fixed(AccessNotDetermined), f.ask); !errors.Is(err, context.Canceled) || f.calls.Load() != 0 {
		t.Errorf("canceled before: %v, %d asks", err, f.calls.Load())
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		if err := requestAccess(ctx, fixed(AccessNotDetermined), (&fakeAsk{}).ask); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("deadline: %v", err)
		}
		if d := time.Since(start); d != time.Second {
			t.Errorf("returned after %v", d)
		}
	})
}

// TestRequestAccessNeverAnswered: a prompt nobody answers blocks only until
// the caller cancels.
func TestRequestAccessNeverAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var late func(bool)
		ask := func(done func(bool)) error { late = done; return nil }
		result := make(chan error, 1)
		go func() { result <- requestAccess(ctx, fixed(AccessNotDetermined), ask) }()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("returned before cancel: %v", err)
		default:
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Errorf("canceled while waiting: %v", err)
		}
		// An answer after the caller left must not block the system thread.
		late(true)
		late(false)
	})
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := &fakeAsk{answer: func(done func(bool)) {
			time.Sleep(time.Second - time.Nanosecond)
			done(true)
		}}
		if err := requestAccess(ctx, fixed(AccessNotDetermined), f.ask); err != nil {
			t.Errorf("answer just before the deadline: %v", err)
		}
	})
}

func TestNullBackendAccess(t *testing.T) {
	if !nullBackend.Load() {
		t.Fatal("tests run on the null backend")
	}
	if a, err := AccessStatus(); a != AccessAuthorized || err != nil {
		t.Errorf("AccessStatus = %v, %v", a, err)
	}
	synctest.Test(t, func(t *testing.T) {
		if err := RequestAccess(bounded(t)); err != nil {
			t.Errorf("RequestAccess = %v", err)
		}
	})
}

func TestCaptureDenied(t *testing.T) {
	denied := fmt.Errorf("%w: %w: backend says no", ErrBackend, fs.ErrPermission)
	err := captureDenied(Capture, denied)
	if !errors.Is(err, ErrPermission) || !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) || err.Error() != denied.Error() {
		t.Errorf("capture denial: %v", err)
	}
	if again := captureDenied(Capture, err); again != err {
		t.Errorf("wrapped twice: %v", again)
	}
	for _, c := range []struct {
		kind Kind
		err  error
	}{{Playback, denied}, {Capture, ErrNoDevice}, {Capture, nil}} {
		if got := captureDenied(c.kind, c.err); got != c.err {
			t.Errorf("%v %v: %v", c.kind, c.err, got)
		}
	}
}

func TestWasapiCaptureDenied(t *testing.T) {
	for _, op := range []string{"Activate", "Initialize"} {
		backend := wasapiError(op, Capture, hrAccessDenied)
		if err := captureDenied(Capture, backend); !errors.Is(err, ErrPermission) || !errors.Is(err, ErrBackend) || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: %v", op, err)
		}
		if errors.Is(captureDenied(Playback, wasapiError(op, Playback, hrAccessDenied)), ErrPermission) {
			t.Errorf("%s: playback refusal is ErrPermission", op)
		}
	}
}
