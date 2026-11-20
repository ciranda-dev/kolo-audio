// A hardware backend is a device_<os>.go file that implements hwStream and
//
//	func hwList(kind Kind) ([]Device, error)
//	func hwOpen(s *stream, cfg Config) (hwStream, error)
//
// hwOpen runs on the locked OS thread of s.run and has started delivery when
// it returns; cfg.DeviceID is empty (default device) or one hwList returned.
// A backend delivers s16 samples at cfg.Format through s.onCapture,
// s.onPlayback and s.onStopped, converting from the device format itself.
// Calls never run concurrently for one stream and none starts after
// hwStream.close returns.

package koloaudio

import (
	"fmt"
	"sync/atomic"
)

// hwStream is an open device. close and stop run on the thread that called
// hwOpen.
type hwStream interface {
	// close stops delivery and releases the device.
	close()
	// stop ends delivery as a vanished device would: s.onStopped is called.
	// Tests use it.
	stop()
}

// nullBackend makes every context use the null backend, a timer-driven
// virtual device. Tests set it; it never touches real hardware.
var nullBackend atomic.Bool

// liveDevices counts devices that are open and not yet closed.
var liveDevices atomic.Int64

func listDevices(kind Kind) ([]Device, error) {
	if nullBackend.Load() {
		return nullList(kind), nil
	}
	return hwList(kind)
}

func (s *stream) openDevice(cfg Config) error {
	if cfg.DeviceID != "" {
		devs, err := listDevices(s.kind)
		if err != nil {
			return err
		}
		if !hasDevice(devs, cfg.DeviceID) {
			return fmt.Errorf("%w: id %q", ErrNoDevice, cfg.DeviceID)
		}
	}
	var hw hwStream
	var err error
	if nullBackend.Load() {
		hw, err = openNull(s, cfg)
	} else {
		hw, err = hwOpen(s, cfg)
	}
	if err != nil {
		return err
	}
	s.hw = hw
	liveDevices.Add(1)
	return nil
}

func (s *stream) closeDevice() {
	s.hw.close()
	liveDevices.Add(-1)
}

func hasDevice(devs []Device, id string) bool {
	for _, d := range devs {
		if d.ID == id {
			return true
		}
	}
	return false
}
