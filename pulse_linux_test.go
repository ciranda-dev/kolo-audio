//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// spy records what a pulseStream delivers and detects overlapping callbacks
// and callbacks after close.
type spy struct {
	ch       int
	inFlight atomic.Int32
	overlap  atomic.Bool
	closed   atomic.Bool
	late     atomic.Bool
	stopped  atomic.Int32
	asked    atomic.Int64 // samples the stream asked onPlayback for

	mu       sync.Mutex
	captured []int16
	played   int // frames handed to the server
}

func (s *spy) enter() {
	if s.inFlight.Add(1) > 1 {
		s.overlap.Store(true)
	}
	if s.closed.Load() {
		s.late.Store(true)
	}
	runtime.Gosched()
}

func (s *spy) leave() { s.inFlight.Add(-1) }

func (s *spy) onCapture(in []int16) {
	s.enter()
	defer s.leave()
	s.mu.Lock()
	s.captured = append(s.captured, in...)
	s.mu.Unlock()
}

// onPlayback writes the frame number plus one into every channel.
func (s *spy) onPlayback(out []int16) {
	s.asked.Add(int64(len(out)))
	s.enter()
	defer s.leave()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < len(out); i += s.ch {
		s.played++
		for c := 0; c < s.ch; c++ {
			out[i+c] = int16(s.played)
		}
	}
}

func (s *spy) onStopped() {
	s.enter()
	defer s.leave()
	s.stopped.Add(1)
}

func (s *spy) capturedLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captured)
}

func (s *spy) check(t *testing.T) {
	t.Helper()
	if s.overlap.Load() {
		t.Error("callbacks ran concurrently")
	}
	if s.late.Load() {
		t.Error("callback after close returned")
	}
}

func testConfig(t *testing.T, channels int) Config {
	t.Helper()
	cfg, err := Config{Format: Format{SampleRate: 48000, Channels: channels}}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// noLeaks fails the test if goroutines outlive it.
func noLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > before {
			buf := make([]byte, 1<<16)
			t.Errorf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
		}
	})
}

func openSpy(t *testing.T, srv *fakePulse, kind Kind, cfg Config) (*pulseStream, *spy) {
	t.Helper()
	sp := &spy{ch: cfg.Format.Channels}
	p, err := openPulse(sp, kind, cfg, srv.env(t), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p, sp
}

func closeSpy(p *pulseStream, sp *spy) {
	p.close()
	sp.closed.Store(true)
}

func TestPulseList(t *testing.T) {
	for _, version := range []uint32{21, 24, 32} {
		t.Run(fmt.Sprint("v", version), func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			srv.version = version
			env := srv.env(t)
			devs, err := pulseListEnv(env, Playback, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			want := []Device{
				{ID: hex.EncodeToString([]byte("alsa_output.fake")), Name: "Fake Speakers", Kind: Playback, Default: true},
				{ID: hex.EncodeToString([]byte("null")), Name: "Null Output", Kind: Playback},
			}
			if fmt.Sprint(devs) != fmt.Sprint(want) {
				t.Errorf("sinks = %v, want %v", devs, want)
			}
			devs, err = pulseListEnv(env, Capture, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			want = []Device{
				{ID: hex.EncodeToString([]byte("alsa_input.fake")), Name: "Fake Microphone", Kind: Capture, Default: true},
				{ID: hex.EncodeToString([]byte("null.monitor")), Name: "Monitor of Null Output", Kind: Capture},
			}
			if fmt.Sprint(devs) != fmt.Sprint(want) {
				t.Errorf("sources = %v, want %v", devs, want)
			}
		})
	}
}

func TestPulseCookieAuth(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	srv.cookie = bytes.Repeat([]byte{0xA5, 0x5A}, 128)
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, srv.cookie, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, bytes.Repeat([]byte{1}, 300), 0o600); err != nil {
		t.Fatal(err)
	}
	env := srv.env(t)
	env.cookie = good
	if _, err := pulseListEnv(env, Playback, time.Second); err != nil {
		t.Fatalf("with the cookie: %v", err)
	}
	env.cookie = bad
	_, err := pulseListEnv(env, Playback, time.Second)
	if !errors.Is(err, ErrBackend) || !errors.Is(err, pulseErrAccess) || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("wrong cookie: %v", err)
	}
	env.cookie = ""
	if _, err := pulseListEnv(env, Playback, time.Second); !errors.Is(err, ErrBackend) {
		t.Errorf("no cookie: %v", err)
	}
	c, _ := srv.stream0Cookies()
	if len(c) != 3 || !bytes.Equal(c[0], srv.cookie) || !bytes.Equal(c[1], bytes.Repeat([]byte{1}, 256)) || !bytes.Equal(c[2], make([]byte, 256)) {
		t.Errorf("the server saw cookies of lengths %v", cookieLens(c))
	}
}

func (s *fakePulse) stream0Cookies() ([][]byte, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all [][]byte
	for _, c := range s.conns {
		c.mu.Lock()
		all = append(all, c.cookies...)
		c.mu.Unlock()
	}
	return all, len(s.conns)
}

func cookieLens(c [][]byte) []int {
	var n []int
	for _, b := range c {
		n = append(n, len(b))
	}
	return n
}

func TestPulseCallWithoutParser(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	c, err := dialPulse(srv.env(t), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err := c.call(pulseOpSetDefaultSink, func(w *tagWriter) { w.str("x") }, nil); err != nil {
		t.Errorf("call without a reply parser: %v", err)
	}
	if err := c.call(pulseOpServerInfo, nil, nil); err != nil {
		t.Errorf("call that discards its reply: %v", err)
	}
}

func TestPulseCookiePaths(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, fill byte) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte{fill}, 256), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	explicit := write("explicit", 1)
	write("xdg/pulse/cookie", 2)
	write("home/.config/pulse/cookie", 3)
	write("home/.pulse-cookie", 4)
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	cases := []struct {
		name string
		env  pulseEnv
		want byte
	}{
		{"PULSE_COOKIE first", pulseEnv{cookie: explicit, configDir: filepath.Join(dir, "xdg"), home: home}, 1},
		{"XDG_CONFIG_HOME next", pulseEnv{configDir: filepath.Join(dir, "xdg"), home: home}, 2},
		{"~/.config without XDG_CONFIG_HOME", pulseEnv{home: home}, 3},
		{"legacy when XDG has none", pulseEnv{configDir: filepath.Join(dir, "none"), home: home}, 4},
		{"short file skipped", pulseEnv{cookie: short, configDir: filepath.Join(dir, "xdg"), home: home}, 2},
		{"directory skipped", pulseEnv{cookie: dir, home: home}, 3},
		{"missing file skipped", pulseEnv{cookie: filepath.Join(dir, "absent"), home: home}, 3},
		{"relative home ignored", pulseEnv{home: "home"}, 0},
		{"nothing", pulseEnv{}, 0},
	}
	for _, c := range cases {
		got := pulseCookie(c.env)
		if len(got) != pulseCookieLen || got[0] != c.want || got[255] != c.want {
			t.Errorf("%s: cookie starts with %d (len %d), want %d", c.name, got[0], len(got), c.want)
		}
	}
}

func TestPulseCookieSpecialFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan []byte, 1)
	go func() { done <- pulseCookie(pulseEnv{cookie: fifo}) }()
	select {
	case c := <-done:
		if !bytes.Equal(c, make([]byte, pulseCookieLen)) {
			t.Error("a fifo was read as a cookie")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a fifo as a cookie blocks")
	}
}

func TestPulseCookieSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, bytes.Repeat([]byte{7}, pulseCookieLen), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if c := pulseCookie(pulseEnv{cookie: link}); c[0] != 7 || c[pulseCookieLen-1] != 7 {
		t.Error("a symlink to a cookie file was not followed")
	}
}

func TestPulseNoServer(t *testing.T) {
	dir := t.TempDir()
	env := pulseEnv{server: "unix:" + filepath.Join(dir, "none"), runtimeDir: dir, uid: -1}
	_, err := dialPulse(env, time.Second)
	var absent *backendAbsent
	if !errors.As(err, &absent) {
		t.Fatalf("err = %v, want backendAbsent", err)
	}
	for _, want := range []string{"no PulseAudio protocol server (tried", filepath.Join(dir, "none"), filepath.Join(dir, "pulse/native"), "/run/user/-1/pulse/native"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	if _, err := pulseListEnv(env, Capture, time.Second); !errors.As(err, &absent) {
		t.Errorf("list err = %v", err)
	}
	cfg := testConfig(t, 1)
	if _, err := openPulse(&spy{ch: 1}, Capture, cfg, env, time.Second); !errors.As(err, &absent) {
		t.Errorf("open err = %v", err)
	}
}

func TestPulseIPv6Refused(t *testing.T) {
	forms := []string{"[::1]:4713", "[::1]", "tcp6:localhost", "tcp6:127.0.0.1", "::1", "::ffff:127.0.0.1", "tcp:[::1]:4713",
		"tcp4:[::ffff:127.0.0.1]:4713", "fe80::1%eth0", "tcp:::1", "[", "tcp4:[::1"}
	for _, f := range forms {
		targets, errs := parsePulseServer(f, pulseEnv{})
		if len(targets) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "IPv6 is not supported") {
			t.Errorf("%q: targets %v, errors %v", f, targets, errs)
		}
	}
	dir := t.TempDir()
	env := pulseEnv{server: strings.Join(forms, " "), runtimeDir: dir, uid: -1}
	_, err := dialPulse(env, time.Second)
	var absent *backendAbsent
	if !errors.As(err, &absent) || strings.Count(err.Error(), "IPv6 is not supported") < 8 {
		t.Errorf("err = %v", err)
	}
	targets, _ := pulseTargets(env)
	for _, tg := range targets {
		if tg.network != "unix" {
			t.Errorf("target %v is not a unix socket", tg)
		}
	}
}

func TestParsePulseServer(t *testing.T) {
	env := pulseEnv{hostname: "box", machineID: "0123abcd"}
	cases := []struct {
		in   string
		want []pulseTarget
		errs int
	}{
		{"", nil, 0},
		{"   ", nil, 0},
		{"/run/user/1/pulse/native", []pulseTarget{{"unix", "/run/user/1/pulse/native"}}, 0},
		{"unix:/tmp/x", []pulseTarget{{"unix", "/tmp/x"}}, 0},
		{"unix:relative", nil, 1},
		{"unix:", nil, 1},
		{"tcp4:127.0.0.1", []pulseTarget{{"tcp4", "127.0.0.1:4713"}}, 0},
		{"tcp4:127.0.0.1:5000", []pulseTarget{{"tcp4", "127.0.0.1:5000"}}, 0},
		{"tcp:example.org", []pulseTarget{{"tcp4", "example.org:4713"}}, 0},
		{"gurki", []pulseTarget{{"tcp4", "gurki:4713"}}, 0},
		{"gurki:1", []pulseTarget{{"tcp4", "gurki:1"}}, 0},
		{"gurki:0", nil, 1},
		{"gurki:65536", nil, 1},
		{"gurki:x", nil, 1},
		{"gurki:", nil, 1},
		{":4713", nil, 1},
		{"bad host", []pulseTarget{{"tcp4", "bad:4713"}, {"tcp4", "host:4713"}}, 0},
		{"bad/host", nil, 1},
		{"{box}unix:/a {other}unix:/b {0123abcd}/c", []pulseTarget{{"unix", "/a"}, {"unix", "/c"}}, 0},
		{"{box", nil, 1},
		{"{}/a", nil, 1},
		{"{box}", nil, 1},
		{"/a\x00b", nil, 1},
		{"/a tcp6:x /b", []pulseTarget{{"unix", "/a"}, {"unix", "/b"}}, 1},
	}
	for _, c := range cases {
		got, errs := parsePulseServer(c.in, env)
		if fmt.Sprint(got) != fmt.Sprint(c.want) || len(errs) != c.errs {
			t.Errorf("%q: targets %v, %d errors %v; want %v, %d errors", c.in, got, len(errs), errs, c.want, c.errs)
		}
	}
}

func TestPulseTargetsOrder(t *testing.T) {
	env := pulseEnv{server: "unix:/a /b", runtimeDir: "/xdg", uid: 1000}
	got, _ := pulseTargets(env)
	want := "[unix:/a unix:/b unix:/xdg/pulse/native unix:/run/user/1000/pulse/native]"
	if fmt.Sprint(got) != want {
		t.Errorf("targets = %v, want %s", got, want)
	}
	env = pulseEnv{server: "/run/user/1000/pulse/native", runtimeDir: "relative", uid: 1000}
	got, _ = pulseTargets(env)
	if fmt.Sprint(got) != "[unix:/run/user/1000/pulse/native]" {
		t.Errorf("duplicates or relative runtime dir kept: %v", got)
	}
}

func TestPulseTCP(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			c := &fakeConn{srv: srv, nc: nc}
			srv.mu.Lock()
			srv.conns = append(srv.conns, c)
			srv.mu.Unlock()
			srv.wg.Add(1)
			go func() {
				defer srv.wg.Done()
				c.run()
			}()
		}
	}()
	for _, form := range []string{"tcp4:", "tcp:", ""} {
		env := pulseEnv{server: form + ln.Addr().String(), uid: -1}
		if devs, err := pulseListEnv(env, Playback, time.Second); err != nil || len(devs) != 2 {
			t.Errorf("%s: %v %v", form, devs, err)
		}
	}
}

func TestBackendOrder(t *testing.T) {
	saved := linuxBackends
	t.Cleanup(func() { linuxBackends = saved })
	absent := func(reason string) linuxBackend {
		e := &backendAbsent{reason}
		return linuxBackend{
			list: func(Kind) ([]Device, error) { return nil, e },
			open: func(*stream, Config) (hwStream, error) { return nil, e },
		}
	}
	answers := func(name string) linuxBackend {
		return linuxBackend{
			list: func(Kind) ([]Device, error) { return []Device{{ID: name}}, nil },
			open: func(*stream, Config) (hwStream, error) { return nil, errors.New(name) },
		}
	}
	failing := linuxBackend{
		list: func(Kind) ([]Device, error) { return nil, ErrBackend },
		open: func(*stream, Config) (hwStream, error) { return nil, ErrBackend },
	}
	linuxBackends = []linuxBackend{absent("no pulse"), absent("no alsa")}
	_, err := hwList(Capture)
	if !errors.Is(err, ErrNoDevice) || err.Error() != "koloaudio: no such device: no pulse and no alsa" {
		t.Errorf("all absent: %v", err)
	}
	if _, err := hwOpen(nil, Config{}); !errors.Is(err, ErrNoDevice) {
		t.Errorf("all absent open: %v", err)
	}
	linuxBackends = []linuxBackend{absent("no pulse"), answers("second")}
	if devs, err := hwList(Capture); err != nil || devs[0].ID != "second" {
		t.Errorf("fallback: %v %v", devs, err)
	}
	if _, err := hwOpen(nil, Config{}); err == nil || err.Error() != "second" {
		t.Errorf("fallback open: %v", err)
	}
	linuxBackends = []linuxBackend{failing, answers("second")}
	if _, err := hwList(Capture); !errors.Is(err, ErrBackend) {
		t.Errorf("a failing backend must not fall through: %v", err)
	}
	if _, err := hwOpen(nil, Config{}); !errors.Is(err, ErrBackend) {
		t.Errorf("a failing backend must not fall through on open: %v", err)
	}
}

func TestPulsePlayback(t *testing.T) {
	for _, tc := range []struct {
		version  uint32
		channels int
		device   string
	}{{32, 1, ""}, {32, 2, "alsa_output.fake"}, {21, 2, ""}, {24, 6, ""}} {
		t.Run(fmt.Sprint("v", tc.version, "c", tc.channels, tc.device), func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			srv.version = tc.version
			cfg := testConfig(t, tc.channels)
			if tc.device != "" {
				cfg.DeviceID = hex.EncodeToString([]byte(tc.device))
			}
			p, sp := openSpy(t, srv, Playback, cfg)
			c, st := srv.stream(t, 0)
			frame := 2 * tc.channels
			period := cfg.Format.frames(cfg.Period) * frame
			buffer := cfg.Format.frames(cfg.Buffer) * frame
			if st.capture || st.spec != (pulseSpec{3, byte(tc.channels), 48000}) || len(st.chmap) != tc.channels {
				t.Errorf("stream %+v", st)
			}
			wantAttr := pulseStreamAttr{maxLength: uint32(buffer), tLength: uint32(2 * period), preBuf: uint32(2 * period), minReq: uint32(period)}
			if st.attr.maxLength != wantAttr.maxLength || st.attr.tLength != wantAttr.tLength || st.attr.preBuf != wantAttr.preBuf || st.attr.minReq != wantAttr.minReq {
				t.Errorf("attr = %+v, want %+v", st.attr, wantAttr)
			}
			if st.devName != tc.device || st.noMove != (tc.device != "") || !st.adjust {
				t.Errorf("device %q noMove %v adjust %v", st.devName, st.noMove, st.adjust)
			}
			waitFor(t, "5000 frames", func() bool { return len(c.received(st)) >= 5000*frame })
			closeSpy(p, sp)
			got := c.received(st)
			if len(got)%frame != 0 {
				t.Errorf("received %d bytes, not whole frames", len(got))
			}
			for i := 0; i+frame <= len(got); i += frame {
				for ch := 0; ch < tc.channels; ch++ {
					if v := binary.LittleEndian.Uint16(got[i+2*ch:]); v != uint16(i/frame+1) {
						t.Fatalf("frame %d channel %d = %d, want %d", i/frame, ch, v, i/frame+1)
					}
				}
			}
			sp.check(t)
			if sp.stopped.Load() != 0 {
				t.Error("close reported the device as stopped")
			}
		})
	}
}

func TestPulseCapture(t *testing.T) {
	for _, tc := range []struct {
		version  uint32
		channels int
		odd      bool
	}{{32, 1, false}, {32, 2, true}, {21, 1, false}, {22, 2, true}} {
		t.Run(fmt.Sprint("v", tc.version, "c", tc.channels, "odd", tc.odd), func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			srv.version = tc.version
			srv.oddSplit.Store(tc.odd)
			cfg := testConfig(t, tc.channels)
			cfg.DeviceID = hex.EncodeToString([]byte("null.monitor"))
			p, sp := openSpy(t, srv, Capture, cfg)
			_, st := srv.stream(t, 0)
			frame := 2 * tc.channels
			if !st.capture || st.devName != "null.monitor" || !st.noMove || st.attr.fragSize != uint32(cfg.Format.frames(cfg.Period)*frame) ||
				st.attr.maxLength != uint32(cfg.Format.frames(cfg.Buffer)*frame) {
				t.Errorf("stream %+v", st)
			}
			waitFor(t, "5000 frames", func() bool { return sp.capturedLen() >= 5000*tc.channels })
			closeSpy(p, sp)
			sp.mu.Lock()
			defer sp.mu.Unlock()
			for i, v := range sp.captured {
				if want := int16(i / tc.channels); v != want {
					t.Fatalf("sample %d = %d, want %d", i, v, want)
				}
			}
			sp.check(t)
			if sp.stopped.Load() != 0 {
				t.Error("close reported the device as stopped")
			}
		})
	}
}

func TestPulseDefaultDeviceFollowed(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	for _, kind := range []Kind{Capture, Playback} {
		p, sp := openSpy(t, srv, kind, testConfig(t, 1))
		_, st := srv.stream(t, map[Kind]int{Capture: 0, Playback: 1}[kind])
		if st.devName != "" || st.noMove {
			t.Errorf("%v: default stream has device %q, noMove %v", kind, st.devName, st.noMove)
		}
		closeSpy(p, sp)
	}
}

func TestPulseKilled(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		t.Run(kind.String(), func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			p, sp := openSpy(t, srv, kind, testConfig(t, 1))
			c, st := srv.stream(t, 0)
			c.kill(st)
			waitFor(t, "onStopped", func() bool { return sp.stopped.Load() == 1 })
			time.Sleep(50 * time.Millisecond)
			n := sp.capturedLen()
			time.Sleep(50 * time.Millisecond)
			if sp.stopped.Load() != 1 || sp.capturedLen() != n {
				t.Error("delivery continued after the stream was killed")
			}
			closeSpy(p, sp)
			sp.check(t)
			if sp.stopped.Load() != 1 {
				t.Errorf("onStopped called %d times", sp.stopped.Load())
			}
		})
	}
}

func TestPulseConnectionClosed(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		t.Run(kind.String(), func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			p, sp := openSpy(t, srv, kind, testConfig(t, 1))
			c, _ := srv.stream(t, 0)
			c.nc.Close()
			waitFor(t, "onStopped", func() bool { return sp.stopped.Load() == 1 })
			closeSpy(p, sp)
			sp.check(t)
			if sp.stopped.Load() != 1 {
				t.Errorf("onStopped called %d times", sp.stopped.Load())
			}
		})
	}
}

func TestPulseSuspendAndMoveAreNotLoss(t *testing.T) {
	for _, kind := range []Kind{Capture, Playback} {
		t.Run(kind.String(), func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			p, sp := openSpy(t, srv, kind, testConfig(t, 1))
			c, st := srv.stream(t, 0)
			progress := func() int {
				if kind == Capture {
					return sp.capturedLen()
				}
				return len(c.received(st))
			}
			waitFor(t, "data", func() bool { return progress() > 0 })
			c.suspend(st, true)
			c.suspend(st, false)
			c.move(st)
			before := progress()
			waitFor(t, "data after suspend and move", func() bool { return progress() > before+2000 })
			if sp.stopped.Load() != 0 {
				t.Error("a suspend or move was reported as a loss")
			}
			closeSpy(p, sp)
			sp.check(t)
		})
	}
}

func TestPulseStop(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	p, sp := openSpy(t, srv, Capture, testConfig(t, 1))
	waitFor(t, "data", func() bool { return sp.capturedLen() > 0 })
	p.stop()
	p.stop()
	n := sp.capturedLen()
	time.Sleep(50 * time.Millisecond)
	if sp.stopped.Load() != 1 || sp.capturedLen() != n {
		t.Errorf("stopped %d times, delivery continued: %v", sp.stopped.Load(), sp.capturedLen() != n)
	}
	closeSpy(p, sp)
	sp.check(t)
}

func TestPulseCloseSilencesCallbacks(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	for i := 0; i < 20; i++ {
		kind := Capture
		if i%2 == 1 {
			kind = Playback
		}
		p, sp := openSpy(t, srv, kind, testConfig(t, 2))
		time.Sleep(time.Duration(i%4) * time.Millisecond)
		closeSpy(p, sp)
		time.Sleep(2 * fakeTick)
		sp.check(t)
		if sp.stopped.Load() != 0 {
			t.Fatal("close called onStopped")
		}
	}
}

func TestPulseCreateErrors(t *testing.T) {
	cases := []struct {
		code pulseError
		want error
	}{
		{pulseErrInvalid, ErrUnsupportedFormat},
		{pulseErrNotSupported, ErrUnsupportedFormat},
		{pulseErrNoEntity, ErrNoDevice},
		{pulseErrAccess, ErrBackend},
		{pulseError(10), ErrBackend},
		{pulseError(200), ErrBackend},
	}
	for _, c := range cases {
		noLeaks(t)
		srv := startFake(t)
		srv.createEr.Store(uint32(c.code))
		for _, kind := range []Kind{Capture, Playback} {
			_, err := openPulse(&spy{ch: 1}, kind, testConfig(t, 1), srv.env(t), time.Second)
			if !errors.Is(err, c.want) {
				t.Errorf("code %d %v: err = %v, want %v", c.code, kind, err, c.want)
			}
		}
	}
}

func TestPulseCreateTimeout(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	srv.hang.Store(true)
	start := time.Now()
	_, err := openPulse(&spy{ch: 1}, Playback, testConfig(t, 1), srv.env(t), 200*time.Millisecond)
	if !errors.Is(err, ErrBackend) || !errors.Is(err, errPulseNoAnswer) || !strings.Contains(err.Error(), "session manager") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v", d)
	}
}

// TestPulseSlowParser makes a reply parser run past the request timeout. call must wait for it,
// because the parser writes the caller's variables: returning early is a data race (run with -race).
func TestPulseSlowParser(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	srv.override = func(c *fakeConn, op, tag uint32, r *tagReader) bool { return op == pulseOpSinkList }
	c, err := dialPulse(srv.env(t), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	c.timeout = 30 * time.Millisecond
	parsed := false
	err = c.call(pulseOpServerInfo, nil, func(r *tagReader) error {
		time.Sleep(150 * time.Millisecond)
		parsed = true
		return nil
	})
	if err != nil || !parsed {
		t.Errorf("call returned %v with parsed = %v", err, parsed)
	}
	// A reply that never comes still times out.
	if err := c.call(pulseOpSinkList, nil, nil); !errors.Is(err, errPulseNoAnswer) {
		t.Errorf("unanswered call: %v", err)
	}
}

func TestPulseRequestTimeoutOnList(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	srv.override = func(c *fakeConn, op, tag uint32, r *tagReader) bool { return op == pulseOpSinkList }
	start := time.Now()
	_, err := pulseListEnv(srv.env(t), Playback, 200*time.Millisecond)
	if !errors.Is(err, errPulseNoAnswer) {
		t.Fatalf("err = %v", err)
	}
	// The documented bound: one timeout per step (docs/limits.md), not unbounded.
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("took %v with a 200 ms request timeout", d)
	}
}

func TestPulseUnsupportedChannels(t *testing.T) {
	cfg := testConfig(t, pulseMaxChannels+1)
	_, err := openPulse(&spy{ch: 1}, Playback, cfg, pulseEnv{uid: -1}, time.Second)
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("err = %v", err)
	}
	if _, err := openPulse(&spy{ch: 1}, Playback, Config{DeviceID: "zz", Format: Format{48000, 1}, Period: time.Millisecond, Buffer: time.Millisecond}, pulseEnv{uid: -1}, time.Second); !errors.Is(err, ErrNoDevice) {
		t.Errorf("bad id: %v", err)
	}
}

func TestPulseServerTooOld(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	srv.version = pulseMinVersion - 1
	_, err := pulseListEnv(srv.env(t), Playback, time.Second)
	if !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "older than") {
		t.Errorf("err = %v", err)
	}
}

// TestPulseHostileServer feeds the client malformed replies.
func TestPulseHostileServer(t *testing.T) {
	garbage := map[string]func(c *fakeConn, tag uint32){
		"truncated sink record": func(c *fakeConn, tag uint32) {
			c.reply(tag, func(w *tagWriter) { w.u32(1); w.str("x") })
		},
		"wrong tag": func(c *fakeConn, tag uint32) {
			c.reply(tag, func(w *tagWriter) { w.str("x"); w.u32(1) })
		},
		"huge count": func(c *fakeConn, tag uint32) {
			c.reply(tag, func(w *tagWriter) { w.b = append(w.b, 'x', 0xFF, 0xFF, 0xFF, 0xFF) })
		},
		"unterminated string": func(c *fakeConn, tag uint32) {
			c.reply(tag, func(w *tagWriter) { w.b = append(w.b, 't', 'a', 'b') })
		},
		"huge frame": func(c *fakeConn, tag uint32) {
			c.nc.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		},
		"zero frame": func(c *fakeConn, tag uint32) {
			c.nc.Write(make([]byte, pulseDescLen))
		},
		"shm frame": func(c *fakeConn, tag uint32) {
			c.nc.Write([]byte{0, 0, 0, 1, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0x80, 0, 0, 0, 0})
		},
		"empty control packet": func(c *fakeConn, tag uint32) {
			c.nc.Write([]byte{0, 0, 0, 1, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		},
	}
	for name, g := range garbage {
		t.Run(name, func(t *testing.T) {
			noLeaks(t)
			srv := startFake(t)
			srv.override = func(c *fakeConn, op, tag uint32, r *tagReader) bool {
				if op != pulseOpSinkList {
					return false
				}
				g(c, tag)
				return true
			}
			if _, err := pulseListEnv(srv.env(t), Playback, time.Second); !errors.Is(err, ErrBackend) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestPulseHostileData(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	srv.quiet.Store(true)
	p, sp := openSpy(t, srv, Playback, testConfig(t, 1))
	c, st := srv.stream(t, 0)
	// sent waits until everything the stream asked for has reached the server.
	sent := func() int64 {
		deadline := time.Now().Add(3 * time.Second)
		for len(c.received(st)) != 2*int(sp.asked.Load()) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return sp.asked.Load()
	}
	before := sent()
	// A request far beyond the buffer is clamped, and junk notifications are ignored.
	c.notify(pulseOpRequest, func(w *tagWriter) { w.u32(st.index); w.u32(0xFFFFFFFF) })
	c.notify(pulseOpRequest, func(w *tagWriter) { w.u32(st.index) })
	c.notify(pulseOpPlayMoved, func(w *tagWriter) { w.u32(st.index) })
	c.notify(pulseOpPlayKilled, func(w *tagWriter) { w.u32(st.index + 1) })
	c.notify(200, func(w *tagWriter) { w.u32(st.index) })
	c.send(st.index+50, func(w *tagWriter) { w.b = append(w.b, 1, 2, 3) })
	c.send(st.index, func(w *tagWriter) { w.b = append(w.b, 1, 2, 3) })
	time.Sleep(100 * time.Millisecond)
	if sp.stopped.Load() != 0 {
		t.Error("ignorable messages stopped the stream")
	}
	// The stream filled at most its buffer for the request, and all of it arrived.
	if asked := sent() - before; asked*2 > int64(p.limit) {
		t.Errorf("one request made the stream produce %d bytes, want at most %d", asked*2, p.limit)
	}
	closeSpy(p, sp)
	sp.check(t)
	if got, max := len(c.received(st)), 16000; got > max {
		t.Errorf("received %d bytes after one clamped request, want at most %d", got, max)
	}
}

// TestPulseFrameBounds checks the frame header limits before anything is allocated or read.
func TestPulseFrameBounds(t *testing.T) {
	header := func(n uint32, flags uint32) []byte {
		h := make([]byte, pulseDescLen)
		binary.BigEndian.PutUint32(h[0:], n)
		binary.BigEndian.PutUint32(h[4:], pulseNoIndex)
		binary.BigEndian.PutUint32(h[16:], flags)
		return h
	}
	for name, h := range map[string][]byte{
		"zero":       header(0, 0),
		"one over":   header(pulseMaxFrame+1, 0),
		"four GiB":   header(0xFFFFFFFF, 0),
		"shm flags":  header(8, 0x01000000),
		"shm, large": header(pulseMaxFrame, 0xFF000000),
	} {
		f := &frameReader{r: bytes.NewReader(h)}
		if _, _, err := f.next(); !errors.Is(err, errPulseProto) || cap(f.data) != 0 {
			t.Errorf("%s: err %v, buffer %d", name, err, cap(f.data))
		}
	}
	// The largest frame is accepted by the header check: a short body ends in EOF, not a protocol error,
	// and the buffer holds what arrived, not what the header promised.
	f := &frameReader{r: bytes.NewReader(append(header(pulseMaxFrame, 0), make([]byte, 100)...))}
	if _, _, err := f.next(); errors.Is(err, errPulseProto) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("largest frame: %v", err)
	}
	if cap(f.data) > 2*pulseReadStep {
		t.Errorf("buffer %d bytes after a 100 byte body", cap(f.data))
	}
}

// TestPulseFrameBufferShrinks checks that a large frame is read whole and its buffer is dropped before
// the next packet.
func TestPulseFrameBufferShrinks(t *testing.T) {
	frame := func(n int) []byte {
		b := make([]byte, pulseDescLen+n)
		putFrameHeader(b, n, pulseNoIndex)
		for i := pulseDescLen; i < len(b); i++ {
			b[i] = byte(i)
		}
		return b
	}
	big, small := 3*pulseKeepFrame+17, 30
	f := &frameReader{r: bytes.NewReader(append(frame(big), frame(small)...))}
	_, p, err := f.next()
	if err != nil || len(p) != big || p[big-1] != byte(pulseDescLen+big-1) {
		t.Fatalf("large frame: %d bytes, %v", len(p), err)
	}
	_, p, err = f.next()
	if err != nil || len(p) != small || cap(f.data) > pulseKeepFrame {
		t.Fatalf("small frame: %d bytes, buffer %d, %v", len(p), cap(f.data), err)
	}
}

// TestPulseChannelBounds checks that channel maps and volumes of more than 32 channels are protocol errors.
func TestPulseChannelBounds(t *testing.T) {
	for _, n := range []int{pulseMaxChannels, pulseMaxChannels + 1, 255} {
		var w tagWriter
		w.channelMap(make([]byte, n))
		r := tagReader{b: w.b}
		got := r.channelMap()
		if bad := n > pulseMaxChannels; bad != (r.err != nil) || (!bad && len(got) != n) {
			t.Errorf("channel map of %d: %d entries, err %v", n, len(got), r.err)
		}
		w = tagWriter{}
		w.cvolume(n, 1)
		r = tagReader{b: w.b}
		r.cvolume()
		if bad := n > pulseMaxChannels; bad != (r.err != nil) || (!bad && !r.eof()) {
			t.Errorf("volume of %d channels: err %v", n, r.err)
		}
	}
}

func TestPulseDefaultMap(t *testing.T) {
	for n := 1; n <= pulseMaxChannels; n++ {
		m := pulseDefaultMap(n)
		if len(m) != n {
			t.Fatalf("%d channels: map of %d", n, len(m))
		}
		seen := map[byte]bool{}
		for _, p := range m {
			if seen[p] && n > 1 {
				t.Errorf("%d channels: position %d twice", n, p)
			}
			seen[p] = true
			if p > 50 {
				t.Errorf("%d channels: position %d out of range", n, p)
			}
		}
	}
}

// TestPulseEndToEnd drives the public API against the fake server.
func TestPulseEndToEnd(t *testing.T) {
	noLeaks(t)
	srv := startFake(t)
	dir := t.TempDir()
	t.Setenv("PULSE_SERVER", "unix:"+srv.addr)
	t.Setenv("PULSE_COOKIE", "")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	nullBackend.Store(false)
	t.Cleanup(func() { nullBackend.Store(true) })

	devs, err := Devices(Playback)
	if err != nil || len(devs) != 2 || !devs[0].Default {
		t.Fatalf("Devices = %v, %v", devs, err)
	}
	if _, err := OpenOutput(Config{DeviceID: hex.EncodeToString([]byte("gone"))}); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown id: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := OpenOutput(Config{DeviceID: devs[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	tone := make([]int16, 4800)
	for i := range tone {
		tone[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	for i := 0; i < 3; i++ {
		if _, err := out.Write(ctx, tone); err != nil {
			t.Fatal(err)
		}
	}
	c, st := srv.stream(t, 0)
	waitFor(t, "tone", func() bool { return len(c.received(st)) >= 2*len(tone) })
	if got := out.Stats(); got.Frames == 0 {
		t.Errorf("stats = %+v", got)
	}
	c.kill(st)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := out.Write(ctx, tone)
		if errors.Is(err, ErrDeviceLost) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("Write after kill = %v", err)
		}
	}
	out.Close()

	in, err := OpenInput(Config{})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int16, 480)
	if n, err := in.Read(ctx, buf); err != nil || n == 0 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	c, _ = srv.stream(t, 1)
	c.nc.Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err := in.Read(ctx, buf)
		if errors.Is(err, ErrDeviceLost) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("Read after disconnect = %v", err)
		}
	}
	in.Close()

	out, err = OpenOutput(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	StopDevice(out)
	if _, err := out.Write(ctx, tone); !errors.Is(err, ErrDeviceLost) {
		t.Errorf("Write after stop = %v, want ErrDeviceLost", err)
	}
}

func FuzzPulseDecode(f *testing.F) {
	for _, d := range []func(w *tagWriter){
		func(w *tagWriter) { writeFakeDeviceInfo(w, 32, 1, fakeDev{"a", "b", 1}, true) },
		func(w *tagWriter) { writeFakeDeviceInfo(w, 21, 1, fakeDev{"a", "b", 1}, false) },
		func(w *tagWriter) { w.str("x"); w.str(""); w.u32(5); w.proplist("k", "v"); w.arbitrary([]byte{1, 2}) },
		func(w *tagWriter) { w.sampleSpec(pulseSpec{3, 2, 48000}); w.channelMap([]byte{1, 2}); w.cvolume(2, 1) },
	} {
		var w tagWriter
		d(&w)
		f.Add(w.b, uint32(32))
		f.Add(append([]byte{0, 0, 0, byte(len(w.b)), 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, w.b...), uint32(21))
	}
	f.Fuzz(func(t *testing.T, b []byte, version uint32) {
		version %= 64
		for _, decode := range []func(r *tagReader){
			func(r *tagReader) {
				for r.err == nil && len(r.b) > 0 {
					r.deviceInfo(version, version%2 == 0)
				}
			},
			func(r *tagReader) { r.serverInfo(version) },
			func(r *tagReader) { r.createdPlayback() },
			func(r *tagReader) { r.createdRecord(version) },
			func(r *tagReader) { r.proplist(); r.formats(); r.formatInfo() },
			func(r *tagReader) { r.str(); r.arbitrary(); r.channelMap(); r.cvolume(); r.timeval() },
			func(r *tagReader) {
				r.u8()
				r.u32()
				r.u64()
				r.i64()
				r.usec()
				r.volume()
				r.bool()
				r.sampleSpec()
			},
			func(r *tagReader) { controlHeader(r.b) },
		} {
			r := tagReader{b: b}
			decode(&r)
			if len(r.b) > len(b) {
				t.Fatal("reader grew its buffer")
			}
		}
		fr := frameReader{r: bytes.NewReader(b)}
		for {
			_, p, err := fr.next()
			if err != nil {
				break
			}
			if len(p) == 0 || len(p) > pulseMaxFrame {
				t.Fatalf("frame of %d bytes", len(p))
			}
			op, _, body := controlHeader(p)
			_ = op
			body.u32()
		}
	})
}

func FuzzPulseServerString(f *testing.F) {
	for _, s := range []string{"", "unix:/a", "{box}/x tcp4:1.2.3.4:5", "[::1]:1", "{", "tcp:::1", "a:b:c", "{}", "tcp6:x", "unix:", "::ffff:1.2.3.4"} {
		f.Add(s)
	}
	env := pulseEnv{hostname: "box", machineID: "id"}
	f.Fuzz(func(t *testing.T, s string) {
		targets, errs := parsePulseServer(s, env)
		if len(targets)+len(errs) > len(strings.Fields(s)) {
			t.Fatalf("%d results for %d entries", len(targets)+len(errs), len(strings.Fields(s)))
		}
		for _, tg := range targets {
			switch tg.network {
			case "unix":
				if !filepath.IsAbs(tg.addr) || strings.ContainsRune(tg.addr, 0) {
					t.Fatalf("bad unix target %q", tg.addr)
				}
			case "tcp4":
				host, port, err := net.SplitHostPort(tg.addr)
				if err != nil || host == "" || port == "" || strings.ContainsAny(tg.addr, "[]%") || strings.Count(tg.addr, ":") != 1 {
					t.Fatalf("bad tcp4 target %q (%v)", tg.addr, err)
				}
			default:
				t.Fatalf("network %q", tg.network)
			}
		}
		pulseTargets(pulseEnv{server: s, hostname: "box", machineID: "id", runtimeDir: s, home: s, cookie: s})
	})
}

// TestPulsePlaybackWriteDeadline checks that a server which stops reading costs a playback stream
// its own write timeout, not the connection's 30 s, and that the stream is then lost.
func TestPulsePlaybackWriteDeadline(t *testing.T) {
	noLeaks(t)
	client, server := net.Pipe()
	defer server.Close()
	c := newPulseConn(client, 30*time.Second)
	defer c.close()
	sp := &spy{ch: 1}
	p := &pulseStream{cb: sp, c: c, ch: 1, frame: 2, limit: 4000, wtimeout: 50 * time.Millisecond, owed: 4000}
	start := time.Now()
	p.mu.Lock()
	p.fillLocked()
	dead := p.dead
	p.mu.Unlock()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("blocked write took %v", d)
	}
	if !dead || sp.stopped.Load() != 1 {
		t.Errorf("dead = %v, stopped = %d", dead, sp.stopped.Load())
	}
}
