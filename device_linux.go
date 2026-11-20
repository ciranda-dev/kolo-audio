//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"errors"
	"fmt"
	"strings"
)

// linuxBackends are the Linux device backends in the order they are tried. A
// backend whose list or open returns *backendAbsent is not available and the
// next one is tried; any other result is final, so a backend never takes over
// from one that is present but failing, and hwList and hwOpen pick the same one.
var linuxBackends = []linuxBackend{
	{list: pulseList, open: pulseOpen},
	{list: alsaList, open: alsaOpen},
}

type linuxBackend struct {
	list func(Kind) ([]Device, error)
	open func(*stream, Config) (hwStream, error)
}

// backendAbsent is returned by a backend that has nothing to talk to. Its
// text says why, for the error that Devices returns when no backend exists.
type backendAbsent struct{ reason string }

func (e *backendAbsent) Error() string { return e.reason }

func noBackend(reasons []string) error {
	return fmt.Errorf("%w: %s", ErrNoDevice, strings.Join(reasons, " and "))
}

func hwList(kind Kind) ([]Device, error) {
	var reasons []string
	for _, b := range linuxBackends {
		devs, err := b.list(kind)
		var absent *backendAbsent
		if errors.As(err, &absent) {
			reasons = append(reasons, absent.reason)
			continue
		}
		return devs, err
	}
	return nil, noBackend(reasons)
}

func hwOpen(s *stream, cfg Config) (hwStream, error) {
	var reasons []string
	for _, b := range linuxBackends {
		hw, err := b.open(s, cfg)
		var absent *backendAbsent
		if errors.As(err, &absent) {
			reasons = append(reasons, absent.reason)
			continue
		}
		return hw, err
	}
	return nil, noBackend(reasons)
}
