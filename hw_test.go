package koloaudio_test

import (
	"context"
	"os"
	"testing"
	"time"

	koloaudio "github.com/ciranda-dev/kolo-audio"
)

// realHardware switches to the real backends for one test. These tests open
// the default microphone and speakers, so they run only when
// KOLO_AUDIO_HW_TEST=1.
func realHardware(t *testing.T) {
	if os.Getenv("KOLO_AUDIO_HW_TEST") != "1" {
		t.Skip("set KOLO_AUDIO_HW_TEST=1 to use real audio devices")
	}
	koloaudio.SetNullBackend(false)
	t.Cleanup(func() { koloaudio.SetNullBackend(true) })
}

func TestHardwareDevices(t *testing.T) {
	realHardware(t)
	for _, kind := range []koloaudio.Kind{koloaudio.Capture, koloaudio.Playback} {
		devs, err := koloaudio.Devices(kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range devs {
			t.Logf("%v: %q default=%v id=%s", kind, d.Name, d.Default, d.ID)
		}
	}
}

func TestHardwareCapture(t *testing.T) {
	realHardware(t)
	checkLeaks(t)
	in := openInput(t, koloaudio.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	buf := make([]int16, 960)
	got := 0
	for got < 48000 {
		n, err := in.Read(ctx, buf)
		if err != nil {
			t.Fatal(err)
		}
		got += n
	}
	peak, rms := koloaudio.Level(buf)
	t.Logf("stats %+v, last chunk peak %.1f dBFS rms %.1f dBFS", in.Stats(), peak, rms)
}

func TestHardwarePlayback(t *testing.T) {
	realHardware(t)
	checkLeaks(t)
	out := openOutput(t, koloaudio.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := out.Write(ctx, tone(440, 48000, 1, 48000, 1000)); err != nil {
		t.Fatal(err)
	}
	t.Logf("stats %+v", out.Stats())
}

// TestHardwareRequestAccess may show the macOS microphone prompt; the person
// running it has two minutes to answer. It opens no device.
func TestHardwareRequestAccess(t *testing.T) {
	realHardware(t)
	before, err := koloaudio.AccessStatus()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = koloaudio.RequestAccess(ctx)
	after, statusErr := koloaudio.AccessStatus()
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	t.Logf("access %v, then %v; RequestAccess: %v", before, after, err)
	if (err == nil) != (after == koloaudio.AccessAuthorized) {
		t.Errorf("RequestAccess = %v with access %v", err, after)
	}
}
