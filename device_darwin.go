//go:build darwin && (amd64 || arm64)

package koloaudio

import "fmt"

func hwList(kind Kind) ([]Device, error) {
	api, err := loadHAL()
	if err != nil {
		return nil, err
	}
	return caList(api, kind)
}

func hwOpen(s *stream, cfg Config) (hwStream, error) {
	if s.kind == Capture {
		if err := captureAllowed(hwAccessStatus); err != nil {
			return nil, err
		}
	}
	api, err := loadHAL()
	if err != nil {
		return nil, err
	}
	api.setRunLoop()
	return openCA(api, s, cfg)
}

// captureAllowed refuses a capture open when the user or a policy has refused the microphone, because
// CoreAudio would open and deliver silence. AccessNotDetermined is left to the system.
func captureAllowed(status func() (Access, error)) error {
	a, err := status()
	if err != nil {
		return err
	}
	if a == AccessDenied || a == AccessRestricted {
		return fmt.Errorf("%w: %w", ErrBackend, accessError(a))
	}
	return nil
}
