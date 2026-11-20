// Package koloaudio captures and plays interleaved 16-bit PCM through the
// system's audio devices and provides small PCM processing helpers.
//
// It uses no cgo and needs nothing installed. Devices exist on macOS
// (CoreAudio), Windows (WASAPI) and Linux (a PulseAudio protocol server, or
// the ALSA kernel interface), on amd64 and arm64; elsewhere they return
// ErrUnsupported. The Resampler and the opus subpackage work everywhere.
//
// Experimental: the API may change before v1.0.0.
package koloaudio
