package koloaudio

// SetNullBackend makes devices come from the null backend when on: a
// timer-driven virtual device whose capture yields silence and whose
// playback discards. Off uses the real backends.
func SetNullBackend(on bool) { nullBackend.Store(on) }

// LiveDevices returns the number of devices not yet released.
func LiveDevices() int64 { return liveDevices.Load() }

// StopDevice stops the device under in or out as a vanished device would.
func StopDevice(s interface{ stream() *stream }) { s.stream().hw.stop() }

// PauseCallbacks stops the null device under in or out from calling back, as a
// stalled device would.
func PauseCallbacks(s interface{ stream() *stream }, on bool) {
	s.stream().hw.(*nullStream).paused.Store(on)
}

func (in *Input) stream() *stream   { return in.s }
func (out *Output) stream() *stream { return out.s }
