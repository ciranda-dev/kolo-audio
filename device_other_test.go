//go:build !((darwin || linux || windows) && (amd64 || arm64))

package koloaudio_test

import (
	"context"
	"errors"
	"testing"

	koloaudio "github.com/ciranda-dev/kolo-audio"
)

func TestNoHardwareBackend(t *testing.T) {
	koloaudio.SetNullBackend(false)
	t.Cleanup(func() { koloaudio.SetNullBackend(true) })
	for _, kind := range []koloaudio.Kind{koloaudio.Capture, koloaudio.Playback} {
		if _, err := koloaudio.Devices(kind); !errors.Is(err, koloaudio.ErrUnsupported) {
			t.Errorf("Devices(%v) = %v", kind, err)
		}
	}
	for _, cfg := range []koloaudio.Config{{}, {DeviceID: "00"}} {
		if _, err := koloaudio.OpenInput(cfg); !errors.Is(err, koloaudio.ErrUnsupported) {
			t.Errorf("OpenInput(%+v) = %v", cfg, err)
		}
		if _, err := koloaudio.OpenOutput(cfg); !errors.Is(err, koloaudio.ErrUnsupported) {
			t.Errorf("OpenOutput(%+v) = %v", cfg, err)
		}
	}
	if _, err := koloaudio.AccessStatus(); !errors.Is(err, koloaudio.ErrUnsupported) {
		t.Errorf("AccessStatus = %v", err)
	}
	if err := koloaudio.RequestAccess(context.Background()); !errors.Is(err, koloaudio.ErrUnsupported) {
		t.Errorf("RequestAccess = %v", err)
	}
	bad := koloaudio.Config{Format: koloaudio.Format{SampleRate: 1}}
	if _, err := koloaudio.OpenInput(bad); !errors.Is(err, koloaudio.ErrUnsupportedFormat) {
		t.Errorf("OpenInput with a bad format = %v", err)
	}
}
