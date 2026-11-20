//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// pulseRequestTimeout bounds every request, dial and write to the server. It
// is libpulse's reply timeout (DEFAULT_TIMEOUT in src/pulse/internal.h).
var pulseRequestTimeout = 30 * time.Second

var errPulseClosed = errors.New("koloaudio: PulseAudio connection closed")

// pulseHandler receives what the server sends about one stream. All three
// methods run on the connection's reader goroutine, or on the caller of
// close for pulseLost.
type pulseHandler interface {
	pulseData(p []byte)
	pulseNotify(op uint32, r *tagReader)
	pulseLost()
}

type pulseWait struct {
	done  chan<- error
	parse func(*tagReader) error
}

// pulseConn is one authenticated connection to a PulseAudio protocol server.
type pulseConn struct {
	nc      net.Conn
	timeout time.Duration
	version atomic.Uint32
	exited  chan struct{}
	wmu     sync.Mutex

	mu      sync.Mutex
	nextTag uint32
	waits   map[uint32]pulseWait
	streams map[uint32]pulseHandler
	err     error
}

func newPulseConn(nc net.Conn, timeout time.Duration) *pulseConn {
	c := &pulseConn{
		nc:      nc,
		timeout: timeout,
		exited:  make(chan struct{}),
		waits:   make(map[uint32]pulseWait),
		streams: make(map[uint32]pulseHandler),
	}
	c.version.Store(pulseMinVersion)
	go c.readLoop()
	return c
}

func (c *pulseConn) readLoop() {
	defer close(c.exited)
	fr := frameReader{r: bufio.NewReaderSize(c.nc, 64<<10)}
	for {
		channel, p, err := fr.next()
		if err != nil {
			c.fail(err)
			return
		}
		if channel != pulseNoIndex {
			if h := c.stream(channel); h != nil {
				h.pulseData(p)
			}
			continue
		}
		op, tag, body := controlHeader(p)
		if body.err != nil {
			c.fail(body.err)
			return
		}
		switch op {
		case pulseOpReply:
			c.finish(tag, &body, nil)
		case pulseOpError:
			code := body.u32()
			if body.err != nil {
				c.fail(body.err)
				return
			}
			c.finish(tag, nil, pulseError(code))
		default:
			c.notify(op, &body)
		}
	}
}

func (c *pulseConn) stream(index uint32) pulseHandler {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[index]
}

// addStream registers h for the packets of a stream. It fails when the
// connection has ended.
func (c *pulseConn) addStream(index uint32, h pulseHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.streams[index] = h
	return nil
}

func (c *pulseConn) finish(tag uint32, body *tagReader, err error) {
	c.mu.Lock()
	w, ok := c.waits[tag]
	delete(c.waits, tag)
	c.mu.Unlock()
	if !ok {
		return
	}
	if err == nil && w.parse != nil {
		err = w.parse(body)
	}
	w.done <- err
}

// notify passes a server message about a stream to its handler. Every such
// message starts with the stream index.
func (c *pulseConn) notify(op uint32, body *tagReader) {
	switch op {
	case pulseOpRequest, pulseOpOverflow, pulseOpUnderflow, pulseOpPlayKilled, pulseOpRecordKilled,
		pulseOpPlaySuspended, pulseOpRecSuspended, pulseOpPlayMoved, pulseOpRecMoved,
		pulseOpStarted, pulseOpPlayAttr, pulseOpRecAttr:
	default:
		return
	}
	index := body.u32()
	if body.err != nil {
		return
	}
	if h := c.stream(index); h != nil {
		h.pulseNotify(op, body)
	}
}

// fail ends the connection: pending requests get err and every stream is
// told it is lost. Only the first call has an effect.
func (c *pulseConn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	waits := c.waits
	streams := c.streams
	c.waits, c.streams = nil, nil
	c.mu.Unlock()
	c.nc.Close()
	for _, w := range waits {
		w.done <- err
	}
	for _, h := range streams {
		h.pulseLost()
	}
}

// close ends the connection and waits for its reader to stop.
func (c *pulseConn) close() {
	c.fail(errPulseClosed)
	<-c.exited
}

// write sends one packet. On failure it closes the socket, which makes the
// reader end the connection.
func (c *pulseConn) write(p []byte) error { return c.writeWithin(p, c.timeout) }

// writeWithin is write with its own deadline.
func (c *pulseConn) writeWithin(p []byte, d time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.nc.SetWriteDeadline(time.Now().Add(d))
	if _, err := c.nc.Write(p); err != nil {
		c.nc.Close()
		return err
	}
	return nil
}

// call sends a command and waits for its reply. body writes the arguments;
// parse decodes the reply on the reader goroutine and may be nil.
func (c *pulseConn) call(op uint32, body func(*tagWriter), parse func(*tagReader) error) error {
	done := make(chan error, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	tag := c.nextTag
	c.nextTag++
	c.waits[tag] = pulseWait{done, parse}
	c.mu.Unlock()

	w := tagWriter{b: make([]byte, pulseDescLen, 128)}
	w.u32(op)
	w.u32(tag)
	if body != nil {
		body(&w)
	}
	putFrameHeader(w.b, len(w.b)-pulseDescLen, pulseNoIndex)
	if err := c.write(w.b); err != nil {
		if owned, perr := c.abandon(tag, done); owned {
			return perr
		}
		return err
	}
	t := time.NewTimer(c.timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		if owned, err := c.abandon(tag, done); owned {
			return err
		}
		return fmt.Errorf("%w: command %d within %v", errPulseNoAnswer, op, c.timeout)
	}
}

// abandon stops waiting for tag. If the reader already took the wait it is running the reply parser,
// which uses the caller's variables, so abandon waits for it and returns its result with owned set.
func (c *pulseConn) abandon(tag uint32, done <-chan error) (owned bool, err error) {
	c.mu.Lock()
	_, pending := c.waits[tag]
	delete(c.waits, tag)
	c.mu.Unlock()
	if pending {
		return false, nil
	}
	return true, <-done
}

// dialPulse connects to the first server that completes the handshake. It
// returns a *backendAbsent error when no server could be reached and an
// ErrBackend error when one answered but refused us.
func dialPulse(env pulseEnv, timeout time.Duration) (*pulseConn, error) {
	targets, errs := pulseTargets(env)
	cookie := pulseCookie(env)
	var failures []string
	for _, err := range errs {
		failures = append(failures, err.Error())
	}
	var refused error
	for _, t := range targets {
		d := net.Dialer{Timeout: timeout}
		nc, err := d.Dial(t.network, t.addr)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", t, err))
			continue
		}
		c := newPulseConn(nc, timeout)
		if err := c.handshake(cookie); err != nil {
			c.close()
			if refused == nil {
				refused = fmt.Errorf("%w: PulseAudio server %s: %w", ErrBackend, t, err)
			}
			continue
		}
		return c, nil
	}
	if refused != nil {
		return nil, refused
	}
	return nil, &backendAbsent{"no PulseAudio protocol server (tried " + strings.Join(failures, "; ") + ")"}
}

func (c *pulseConn) handshake(cookie []byte) error {
	var server uint32
	err := c.call(pulseOpAuth, func(w *tagWriter) {
		w.u32(pulseVersion)
		w.arbitrary(cookie)
	}, func(r *tagReader) error {
		server = r.u32() & pulseVersionMask
		return r.err
	})
	if err != nil {
		return err
	}
	if server < pulseMinVersion {
		return fmt.Errorf("protocol version %d is older than %d", server, pulseMinVersion)
	}
	c.version.Store(min(server, pulseVersion))
	return c.call(pulseOpSetClientName, func(w *tagWriter) {
		w.proplist("application.name", "kolo-audio")
	}, func(r *tagReader) error {
		r.u32()
		return r.err
	})
}

// devices lists the sinks or sources of the server.
func (c *pulseConn) devices(sinks bool) ([]pulseDevInfo, error) {
	op := uint32(pulseOpSourceList)
	if sinks {
		op = pulseOpSinkList
	}
	var devs []pulseDevInfo
	err := c.call(op, nil, func(r *tagReader) error {
		for r.err == nil && len(r.b) > 0 {
			devs = append(devs, r.deviceInfo(c.version.Load(), sinks))
		}
		return r.err
	})
	return devs, err
}

func (c *pulseConn) serverInfo() (pulseServerInfo, error) {
	var info pulseServerInfo
	err := c.call(pulseOpServerInfo, nil, func(r *tagReader) error {
		info = r.serverInfo(c.version.Load())
		return r.err
	})
	return info, err
}
