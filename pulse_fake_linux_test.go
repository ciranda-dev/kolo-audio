//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The fake server speaks enough of the native protocol for the client:
// authentication, client name, server and device info, playback and record
// streams paced by a 5 ms clock, and the notifications for killing,
// suspending and moving a stream. It uses the client's own codec, so the
// Docker job against a real server is what checks the wire format.

const fakeTick = 5 * time.Millisecond

type fakeDev struct {
	name, desc string
	module     uint32
}

type fakeStream struct {
	index       uint32
	capture     bool
	spec        pulseSpec
	chmap       []byte
	devName     string
	attr        pulseStreamAttr
	noMove      bool
	adjust      bool
	stop        chan struct{}
	received    []byte // playback bytes, guarded by the conn's mu
	outstanding int    // playback bytes requested and not yet received
}

type fakePulse struct {
	t        *testing.T
	ln       net.Listener
	addr     string
	version  uint32
	cookie   []byte
	sinks    []fakeDev
	sources  []fakeDev
	defSink  string
	defSrc   string
	hang     atomic.Bool
	createEr atomic.Uint32
	oddSplit atomic.Bool
	quiet    atomic.Bool // the clock makes no playback requests
	override func(c *fakeConn, op, tag uint32, r *tagReader) bool

	mu    sync.Mutex
	conns []*fakeConn
	wg    sync.WaitGroup
}

type fakeConn struct {
	srv     *fakePulse
	nc      net.Conn
	version uint32
	wmu     sync.Mutex

	mu      sync.Mutex
	streams []*fakeStream
	authed  bool
	cookies [][]byte
}

// startFake serves on a unix socket in a temp dir.
func startFake(t *testing.T) *fakePulse {
	t.Helper()
	s := &fakePulse{
		t:       t,
		version: pulseVersion,
		sinks:   []fakeDev{{"alsa_output.fake", "Fake Speakers", 7}, {"null", "Null Output", 8}},
		sources: []fakeDev{{"alsa_input.fake", "Fake Microphone", 9}, {"null.monitor", "Monitor of Null Output", 0}},
		defSink: "alsa_output.fake",
		defSrc:  "alsa_input.fake",
	}
	dir, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s.addr = filepath.Join(dir, "native")
	ln, err := net.Listen("unix", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			c := &fakeConn{srv: s, nc: nc}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				c.run()
			}()
		}
	}()
	t.Cleanup(s.close)
	return s
}

func (s *fakePulse) close() {
	s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.nc.Close()
		c.stopClocks()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// env points the client at the server and at nothing else.
func (s *fakePulse) env(t *testing.T) pulseEnv {
	dir := t.TempDir()
	return pulseEnv{server: "unix:" + s.addr, home: dir, uid: -1, hostname: "fake-host"}
}

// stream waits for the nth stream and returns it with its connection.
func (s *fakePulse) stream(t *testing.T, n int) (*fakeConn, *fakeStream) {
	t.Helper()
	var c *fakeConn
	var st *fakeStream
	waitFor(t, "stream", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		k := 0
		for _, cc := range s.conns {
			cc.mu.Lock()
			for _, ss := range cc.streams {
				if k == n {
					c, st = cc, ss
				}
				k++
			}
			cc.mu.Unlock()
		}
		return st != nil
	})
	return c, st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *fakeConn) received(st *fakeStream) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), st.received...)
}

func (c *fakeConn) run() {
	defer c.nc.Close()
	defer c.stopClocks()
	fr := frameReader{r: bufio.NewReader(c.nc)}
	for {
		channel, p, err := fr.next()
		if err != nil {
			return
		}
		if channel != pulseNoIndex {
			c.data(channel, p)
			continue
		}
		op, tag, r := controlHeader(p)
		if r.err != nil {
			return
		}
		if c.srv.override != nil && c.srv.override(c, op, tag, &r) {
			continue
		}
		c.handle(op, tag, &r)
	}
}

func (c *fakeConn) send(channel uint32, build func(w *tagWriter)) {
	w := tagWriter{b: make([]byte, pulseDescLen)}
	build(&w)
	putFrameHeader(w.b, len(w.b)-pulseDescLen, channel)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.nc.Write(w.b)
}

func (c *fakeConn) reply(tag uint32, build func(w *tagWriter)) {
	c.send(pulseNoIndex, func(w *tagWriter) {
		w.u32(pulseOpReply)
		w.u32(tag)
		if build != nil {
			build(w)
		}
	})
}

func (c *fakeConn) fail(tag uint32, code pulseError) {
	c.send(pulseNoIndex, func(w *tagWriter) {
		w.u32(pulseOpError)
		w.u32(tag)
		w.u32(uint32(code))
	})
}

func (c *fakeConn) notify(op uint32, build func(w *tagWriter)) {
	c.send(pulseNoIndex, func(w *tagWriter) {
		w.u32(op)
		w.u32(0xFFFFFFFF)
		build(w)
	})
}

func (c *fakeConn) handle(op, tag uint32, r *tagReader) {
	s := c.srv
	switch op {
	case pulseOpAuth:
		ver := r.u32()
		cookie := r.arbitrary()
		if r.err != nil || !r.eof() {
			s.t.Errorf("fake: malformed auth")
			return
		}
		c.mu.Lock()
		c.cookies = append(c.cookies, append([]byte(nil), cookie...))
		c.mu.Unlock()
		if s.cookie != nil && !bytes.Equal(cookie, s.cookie) {
			c.fail(tag, pulseErrAccess)
			return
		}
		c.version = min(ver&pulseVersionMask, s.version)
		c.authed = true
		c.reply(tag, func(w *tagWriter) { w.u32(s.version) })
	case pulseOpSetClientName:
		r.proplist()
		if r.err != nil || !r.eof() {
			s.t.Errorf("fake: malformed client name")
		}
		c.reply(tag, func(w *tagWriter) { w.u32(1) })
	case pulseOpServerInfo:
		c.reply(tag, func(w *tagWriter) {
			w.str("fake")
			w.str("1.0")
			w.str("user")
			w.str("fake-host")
			w.sampleSpec(pulseSpec{pulseSampleS16LE, 2, 44100})
			w.str(s.defSink)
			w.str(s.defSrc)
			w.u32(1)
			w.channelMap([]byte{1, 2})
		})
	case pulseOpSinkList, pulseOpSourceList:
		devs := s.sinks
		if op == pulseOpSourceList {
			devs = s.sources
		}
		c.reply(tag, func(w *tagWriter) {
			for i, d := range devs {
				writeFakeDeviceInfo(w, c.version, uint32(i), d, op == pulseOpSinkList)
			}
		})
	case pulseOpCreatePlay, pulseOpCreateRecord:
		c.create(op, tag, r)
	default:
		c.reply(tag, nil)
	}
}

func writeFakeDeviceInfo(w *tagWriter, version, index uint32, d fakeDev, sink bool) {
	w.u32(index)
	w.str(d.name)
	w.str(d.desc)
	w.sampleSpec(pulseSpec{pulseSampleS16LE, 2, 48000})
	w.channelMap([]byte{1, 2})
	w.u32(d.module)
	w.cvolume(2, 0x10000)
	w.bool(false)
	w.u32(pulseNoIndex)
	w.str("")
	w.usec(0)
	w.str("fake driver")
	w.u32(0)
	w.proplist("device.description", d.desc)
	w.usec(0)
	w.volume(0x10000)
	w.u32(0)
	w.u32(256)
	w.u32(pulseNoIndex)
	w.u32(2) // ports
	for _, p := range []string{"analog-output", "headphones"} {
		w.str(p)
		w.str("Port " + p)
		w.u32(100)
		if version >= 24 {
			w.u32(0)
		}
	}
	w.str("analog-output")
	if sink || version >= 22 {
		w.u8(1)
		w.b = append(w.b, 'f')
		w.u8(1)
		w.proplist()
	}
}

func (w *tagWriter) volume(v uint32) {
	w.b = binary.BigEndian.AppendUint32(append(w.b, 'V'), v)
}

func (c *fakeConn) create(op, tag uint32, r *tagReader) {
	s := c.srv
	st := &fakeStream{capture: op == pulseOpCreateRecord, stop: make(chan struct{})}
	st.spec = r.sampleSpec()
	st.chmap = append([]byte(nil), r.channelMap()...)
	idx := r.u32()
	st.devName = r.str()
	st.attr.maxLength = r.u32()
	r.bool() // corked
	if st.capture {
		st.attr.fragSize = r.u32()
	} else {
		st.attr.tLength = r.u32()
		st.attr.preBuf = r.u32()
		st.attr.minReq = r.u32()
		r.u32() // sync id
		r.cvolume()
	}
	for i := 0; i < 7; i++ {
		b := r.bool()
		if i == 5 {
			st.noMove = b
		}
	}
	if st.capture {
		r.bool()
		st.adjust = r.bool()
		r.proplist()
		r.u32()
		r.bool()
		r.bool()
		r.bool()
		if c.version >= 22 {
			r.u8()
			r.cvolume()
			for i := 0; i < 5; i++ {
				r.bool()
			}
		}
	} else {
		r.bool()
		st.adjust = r.bool()
		r.proplist()
		for i := 0; i < 7; i++ {
			r.bool()
		}
		r.u8()
	}
	if r.err != nil || !r.eof() || idx != pulseNoIndex {
		s.t.Errorf("fake: malformed create stream: err %v, %d bytes left", r.err, len(r.b))
		c.fail(tag, pulseErrInvalid)
		return
	}
	if s.hang.Load() {
		return
	}
	if code := s.createEr.Load(); code != 0 {
		c.fail(tag, pulseError(code))
		return
	}
	c.mu.Lock()
	st.index = uint32(len(c.streams)) + 10
	c.streams = append(c.streams, st)
	c.mu.Unlock()
	frame := int(st.spec.channels) * 2
	st.outstanding = int(st.attr.tLength)
	c.reply(tag, func(w *tagWriter) {
		w.u32(st.index)
		w.u32(st.index + 100)
		if st.capture {
			w.u32(st.attr.maxLength)
			w.u32(st.attr.fragSize)
		} else {
			w.u32(uint32(st.outstanding))
			w.u32(st.attr.maxLength)
			w.u32(st.attr.tLength)
			w.u32(st.attr.preBuf)
			w.u32(st.attr.minReq)
		}
		w.sampleSpec(st.spec)
		w.channelMap(st.chmap)
		w.u32(3)
		w.str("alsa_output.fake")
		w.bool(false)
		w.usec(1000)
		if !st.capture || c.version >= 22 {
			w.b = append(w.b, 'f')
			w.u8(1)
			w.proplist()
		}
	})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		c.clock(st, frame)
	}()
}

// clock plays the part of the device: it requests playback data and produces
// capture data in real time.
func (c *fakeConn) clock(st *fakeStream, frame int) {
	t := time.NewTicker(fakeTick)
	defer t.Stop()
	perTick := int(st.spec.rate) * int(fakeTick) / int(time.Second) * frame
	var counter int64
	for {
		select {
		case <-st.stop:
			return
		case <-t.C:
		}
		if st.capture {
			b := make([]byte, perTick)
			for i := 0; i < perTick/frame; i++ {
				for ch := 0; ch < frame/2; ch++ {
					binary.LittleEndian.PutUint16(b[i*frame+2*ch:], uint16(int16(counter)))
				}
				counter++
			}
			if c.srv.oddSplit.Load() {
				c.send(st.index, func(w *tagWriter) { w.b = append(w.b, b[:3]...) })
				b = b[3:]
			}
			c.send(st.index, func(w *tagWriter) { w.b = append(w.b, b...) })
			continue
		}
		c.mu.Lock()
		want := min(perTick, int(st.attr.tLength)-st.outstanding)
		if want > 0 {
			st.outstanding += want
		}
		c.mu.Unlock()
		if want > 0 && !c.srv.quiet.Load() {
			c.notify(pulseOpRequest, func(w *tagWriter) {
				w.u32(st.index)
				w.u32(uint32(want))
			})
		}
	}
}

func (c *fakeConn) data(channel uint32, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.streams {
		if st.index == channel && !st.capture {
			st.received = append(st.received, p...)
			st.outstanding -= len(p)
		}
	}
}

func (c *fakeConn) kill(st *fakeStream) {
	op := uint32(pulseOpPlayKilled)
	if st.capture {
		op = pulseOpRecordKilled
	}
	c.notify(op, func(w *tagWriter) { w.u32(st.index) })
}

func (c *fakeConn) suspend(st *fakeStream, on bool) {
	op := uint32(pulseOpPlaySuspended)
	if st.capture {
		op = pulseOpRecSuspended
	}
	c.notify(op, func(w *tagWriter) {
		w.u32(st.index)
		w.bool(on)
	})
}

func (c *fakeConn) move(st *fakeStream) {
	op := uint32(pulseOpPlayMoved)
	if st.capture {
		op = pulseOpRecMoved
	}
	c.notify(op, func(w *tagWriter) {
		w.u32(st.index)
		w.u32(4)
		w.str("null")
		w.bool(false)
		if st.capture {
			w.u32(st.attr.maxLength)
			w.u32(st.attr.fragSize)
			w.usec(500)
			return
		}
		w.u32(st.attr.maxLength)
		w.u32(st.attr.tLength)
		w.u32(st.attr.preBuf)
		w.u32(st.attr.minReq)
		w.usec(500)
	})
}

func (c *fakeConn) stopClocks() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.streams {
		select {
		case <-st.stop:
		default:
			close(st.stop)
		}
	}
}
