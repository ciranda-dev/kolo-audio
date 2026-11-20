package koloaudio

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// Access is the microphone access the system grants this process.
type Access int

const (
	// AccessNotDetermined means the user has not been asked yet.
	AccessNotDetermined Access = iota + 1
	// AccessRestricted means a policy, such as device management, forbids access.
	AccessRestricted
	// AccessDenied means the user refused access.
	AccessDenied
	// AccessAuthorized means capture is allowed. On Linux and Windows it only
	// means that there is nothing to check.
	AccessAuthorized
)

// String returns "not determined", "restricted", "denied" or "authorized".
func (a Access) String() string {
	switch a {
	case AccessNotDetermined:
		return "not determined"
	case AccessRestricted:
		return "restricted"
	case AccessDenied:
		return "denied"
	case AccessAuthorized:
		return "authorized"
	}
	return fmt.Sprintf("Access(%d)", int(a))
}

// AccessStatus reports the microphone access of this process on macOS. It
// never asks the user and never opens a device; an unknown system status is
// ErrBackend. Linux and Windows have no consent step to read, so it returns
// AccessAuthorized, and OpenInput reports the refusals the backend returns as
// ErrPermission. Elsewhere it returns ErrUnsupported.
func AccessStatus() (Access, error) {
	if nullBackend.Load() {
		return AccessAuthorized, nil
	}
	return hwAccessStatus()
}

// RequestAccess asks for microphone access on macOS when the user has not
// been asked yet, and returns once access is decided. It never opens a
// device. A refusal returns ErrPermission. The prompt waits for the person, so
// bound the wait with ctx; the prompt stays until answered. An app bundle
// needs NSMicrophoneUsageDescription in its Info.plist and, with the hardened
// runtime, the com.apple.security.device.audio-input entitlement; without them
// macOS ends the process. Linux and Windows return nil at once, elsewhere
// ErrUnsupported.
func RequestAccess(ctx context.Context) error {
	if nullBackend.Load() {
		return nil
	}
	return requestAccess(ctx, hwAccessStatus, hwAskAccess)
}

// requestAccess asks only when status is AccessNotDetermined. ask calls done
// once, on any thread.
func requestAccess(ctx context.Context, status func() (Access, error), ask func(done func(granted bool)) error) error {
	a, err := status()
	if err != nil {
		return err
	}
	switch a {
	case AccessAuthorized:
		return nil
	case AccessNotDetermined:
	default:
		return accessError(a)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	answer := make(chan bool, 1)
	if err := ask(func(granted bool) {
		select {
		case answer <- granted:
		default:
		}
	}); err != nil {
		return err
	}
	select {
	case granted := <-answer:
		if !granted {
			return accessError(AccessDenied)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func accessError(a Access) error {
	return fmt.Errorf("%w (%v): %w", ErrPermission, a, fs.ErrPermission)
}

// captureDenied adds ErrPermission to a refused capture open, so that one
// check covers RequestAccess and OpenInput. The message stays the backend's.
func captureDenied(kind Kind, err error) error {
	if kind != Capture || !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrPermission) {
		return err
	}
	return deniedError{err}
}

type deniedError struct{ err error }

func (e deniedError) Error() string   { return e.err.Error() }
func (e deniedError) Unwrap() []error { return []error{ErrPermission, e.err} }
