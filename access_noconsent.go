//go:build (linux || windows) && (amd64 || arm64)

package koloaudio

// Linux and Windows have no consent step; OpenInput reports a refusal.

func hwAccessStatus() (Access, error) { return AccessAuthorized, nil }

func hwAskAccess(done func(granted bool)) error {
	done(true)
	return nil
}
