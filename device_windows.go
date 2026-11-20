//go:build windows && (amd64 || arm64)

package koloaudio

func hwList(kind Kind) ([]Device, error) { return listWasapi(winAPI{}, kind) }

func hwOpen(s *stream, cfg Config) (hwStream, error) { return openWasapi(winAPI{}, s, s.kind, cfg) }
