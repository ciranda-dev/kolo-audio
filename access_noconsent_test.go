//go:build (linux || windows) && (amd64 || arm64)

package koloaudio

import (
	"context"
	"testing"
)

func TestNoConsentStep(t *testing.T) {
	nullBackend.Store(false)
	t.Cleanup(func() { nullBackend.Store(true) })
	if a, err := AccessStatus(); a != AccessAuthorized || err != nil {
		t.Errorf("AccessStatus = %v, %v", a, err)
	}
	if err := RequestAccess(context.Background()); err != nil {
		t.Errorf("RequestAccess = %v", err)
	}
	asked := false
	if err := hwAskAccess(func(granted bool) { asked = granted }); err != nil || !asked {
		t.Errorf("hwAskAccess = %v, granted %v", err, asked)
	}
}
