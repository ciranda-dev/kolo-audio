//go:build !((darwin || linux || windows) && (amd64 || arm64))

package koloaudio

func hwList(Kind) ([]Device, error) { return nil, ErrUnsupported }

func hwOpen(*stream, Config) (hwStream, error) { return nil, ErrUnsupported }

func hwAccessStatus() (Access, error) { return 0, ErrUnsupported }

func hwAskAccess(func(bool)) error { return ErrUnsupported }
