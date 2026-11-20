//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pulseEnv is what server and cookie discovery read from the process.
type pulseEnv struct {
	server     string // $PULSE_SERVER
	cookie     string // $PULSE_COOKIE
	runtimeDir string // $XDG_RUNTIME_DIR
	configDir  string // $XDG_CONFIG_HOME
	home       string // $HOME
	uid        int
	hostname   string
	machineID  string
}

func osPulseEnv() pulseEnv {
	host, _ := os.Hostname()
	return pulseEnv{
		server:     os.Getenv("PULSE_SERVER"),
		cookie:     os.Getenv("PULSE_COOKIE"),
		runtimeDir: os.Getenv("XDG_RUNTIME_DIR"),
		configDir:  os.Getenv("XDG_CONFIG_HOME"),
		home:       os.Getenv("HOME"),
		uid:        os.Getuid(),
		hostname:   host,
		machineID:  readMachineID(),
	}
}

func readMachineID() string {
	for _, f := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(f); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// pulseTarget is a server address: a unix socket path or an IPv4 host:port.
type pulseTarget struct{ network, addr string }

func (t pulseTarget) String() string {
	if t.network == "unix" {
		return "unix:" + t.addr
	}
	return "tcp4:" + t.addr
}

const pulseDefaultPort = "4713"

var errPulseIPv6 = errors.New("IPv6 is not supported")

// parsePulseServer parses a PulseAudio server string, a space separated list
// of entries (freedesktop.org ServerStrings). It returns the usable
// targets and one error per refused or malformed entry.
func parsePulseServer(s string, env pulseEnv) ([]pulseTarget, []error) {
	var targets []pulseTarget
	var errs []error
	for _, e := range strings.Fields(s) {
		t, ok, err := parsePulseEntry(e, env)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("server %q: %w", e, err))
		case ok:
			targets = append(targets, t)
		}
	}
	return targets, errs
}

// parsePulseEntry parses one entry. ok is false for an entry that names
// another machine.
func parsePulseEntry(e string, env pulseEnv) (t pulseTarget, ok bool, err error) {
	if strings.ContainsRune(e, 0) {
		return t, false, errors.New("malformed")
	}
	if e[0] == '{' {
		end := strings.IndexByte(e, '}')
		if end < 2 {
			return t, false, errors.New("malformed machine prefix")
		}
		id := e[1:end]
		e = e[end+1:]
		if id != env.hostname && id != env.machineID {
			return t, false, nil
		}
	}
	switch {
	case e == "":
		return t, false, errors.New("malformed")
	case e[0] == '/':
		return unixTarget(e)
	case strings.HasPrefix(e, "unix:"):
		return unixTarget(e[len("unix:"):])
	case strings.HasPrefix(e, "tcp6:"):
		return t, false, errPulseIPv6
	case strings.HasPrefix(e, "tcp4:"):
		return tcpTarget(e[len("tcp4:"):])
	case strings.HasPrefix(e, "tcp:"):
		return tcpTarget(e[len("tcp:"):])
	}
	return tcpTarget(e)
}

func unixTarget(p string) (pulseTarget, bool, error) {
	if !path.IsAbs(p) {
		return pulseTarget{}, false, errors.New("unix socket path is not absolute")
	}
	return pulseTarget{"unix", p}, true, nil
}

// tcpTarget accepts a host name or IPv4 address with an optional port. Any
// form of IPv6 address (bracketed, bare, with a zone, IPv4-mapped) is refused.
func tcpTarget(hp string) (pulseTarget, bool, error) {
	if hp == "" {
		return pulseTarget{}, false, errors.New("malformed")
	}
	if hp[0] == '[' || strings.Count(hp, ":") > 1 || strings.ContainsRune(hp, '%') {
		return pulseTarget{}, false, errPulseIPv6
	}
	host, port := hp, pulseDefaultPort
	if i := strings.IndexByte(hp, ':'); i >= 0 {
		host, port = hp[:i], hp[i+1:]
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return pulseTarget{}, false, errors.New("malformed port")
		}
	}
	if !validPulseHost(host) {
		return pulseTarget{}, false, errors.New("malformed host")
	}
	if a, err := netip.ParseAddr(host); err == nil && !a.Is4() {
		return pulseTarget{}, false, errPulseIPv6
	}
	return pulseTarget{"tcp4", net.JoinHostPort(host, port)}, true, nil
}

func validPulseHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, c := range []byte(h) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// pulseTargets lists the servers to try, in order: the entries of
// $PULSE_SERVER, $XDG_RUNTIME_DIR/pulse/native, /run/user/<uid>/pulse/native.
// errs holds one error per refused or malformed $PULSE_SERVER entry.
func pulseTargets(env pulseEnv) (targets []pulseTarget, errs []error) {
	targets, errs = parsePulseServer(env.server, env)
	if env.runtimeDir != "" && path.IsAbs(env.runtimeDir) {
		targets = append(targets, pulseTarget{"unix", path.Join(env.runtimeDir, "pulse/native")})
	}
	targets = append(targets, pulseTarget{"unix", "/run/user/" + strconv.Itoa(env.uid) + "/pulse/native"})
	seen := make(map[pulseTarget]bool, len(targets))
	uniq := targets[:0]
	for _, t := range targets {
		if !seen[t] {
			seen[t] = true
			uniq = append(uniq, t)
		}
	}
	return uniq, errs
}

// pulseCookiePaths lists the cookie files to try: $PULSE_COOKIE,
// $XDG_CONFIG_HOME/pulse/cookie (~/.config/pulse/cookie when unset), then
// ~/.pulse-cookie.
func pulseCookiePaths(env pulseEnv) []string {
	var paths []string
	if env.cookie != "" {
		paths = append(paths, env.cookie)
	}
	switch {
	case env.configDir != "" && filepath.IsAbs(env.configDir):
		paths = append(paths, filepath.Join(env.configDir, "pulse", "cookie"))
	case env.home != "" && filepath.IsAbs(env.home):
		paths = append(paths, filepath.Join(env.home, ".config", "pulse", "cookie"))
	}
	if env.home != "" && filepath.IsAbs(env.home) {
		paths = append(paths, filepath.Join(env.home, ".pulse-cookie"))
	}
	return paths
}

// pulseCookie returns the authentication cookie, the first 256 bytes of the
// first usable file in pulseCookiePaths. Without one it returns zeros, which
// a server that checks the peer's credentials or allows anonymous access
// accepts.
func pulseCookie(env pulseEnv) []byte {
	for _, p := range pulseCookiePaths(env) {
		if c, err := readCookie(p); err == nil {
			return c
		}
	}
	return make([]byte, pulseCookieLen)
}

// readCookie opens without blocking and checks the opened file, so a file
// swapped for a FIFO between a check and the open cannot stall the caller.
func readCookie(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	c := make([]byte, pulseCookieLen)
	if _, err := io.ReadFull(f, c); err != nil {
		return nil, err
	}
	return c, nil
}
