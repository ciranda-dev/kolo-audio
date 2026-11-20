//go:build linux && (amd64 || arm64)

package koloaudio

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Sizes of the structures in include/uapi/sound/asound.h on 64-bit Linux
// (amd64 and arm64 share them); a test checks them against the headers.
const (
	alsaHWParamsSize = 608
	alsaSWParamsSize = 136
	alsaStatusSize   = 152
	alsaXferiSize    = 24
	alsaPCMInfoSize  = 288
	alsaCardInfoSize = 376

	alsaOffMasks     = 4   // snd_pcm_hw_params.masks
	alsaOffIntervals = 260 // snd_pcm_hw_params.intervals
	alsaOffRmask     = 512 // snd_pcm_hw_params.rmask, then cmask, info, msbits, rate_num, rate_den
	alsaOffFifo      = 536 // snd_pcm_hw_params.fifo_size

	alsaOffAvailMin  = 16 // snd_pcm_sw_params.avail_min
	alsaOffStartThr  = 32 // snd_pcm_sw_params.start_threshold
	alsaOffStopThr   = 40 // snd_pcm_sw_params.stop_threshold
	alsaOffProto     = 72 // snd_pcm_sw_params.proto
	alsaOffTstampTyp = 76 // snd_pcm_sw_params.tstamp_type

	alsaOffTrigger = 8  // snd_pcm_status.trigger_tstamp
	alsaOffTstamp  = 24 // snd_pcm_status.tstamp
	alsaOffAvail   = 64 // snd_pcm_status.avail

	alsaOffInfoStream = 8  // snd_pcm_info.stream
	alsaOffInfoName   = 80 // snd_pcm_info.name, 80 bytes
	alsaOffCardID     = 8  // snd_ctl_card_info.id, 16 bytes
	alsaOffCardName   = 40 // snd_ctl_card_info.name, 32 bytes
)

// alsaIOC encodes an ioctl request number as asm-generic/ioctl.h does.
func alsaIOC(dir, typ, nr, size uint32) uint32 {
	return dir<<30 | size<<16 | typ<<8 | nr
}

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

var (
	alsaIoctlPCMVersion = alsaIOC(iocRead, 'A', 0x00, 4)
	alsaIoctlHWRefine   = alsaIOC(iocRead|iocWrite, 'A', 0x10, alsaHWParamsSize)
	alsaIoctlHWParams   = alsaIOC(iocRead|iocWrite, 'A', 0x11, alsaHWParamsSize)
	alsaIoctlSWParams   = alsaIOC(iocRead|iocWrite, 'A', 0x13, alsaSWParamsSize)
	alsaIoctlStatus     = alsaIOC(iocRead, 'A', 0x20, alsaStatusSize)
	alsaIoctlPrepare    = alsaIOC(iocNone, 'A', 0x40, 0)
	alsaIoctlStart      = alsaIOC(iocNone, 'A', 0x42, 0)
	alsaIoctlDrop       = alsaIOC(iocNone, 'A', 0x43, 0)
	alsaIoctlResume     = alsaIOC(iocNone, 'A', 0x47, 0)
	alsaIoctlWriteI     = alsaIOC(iocWrite, 'A', 0x50, alsaXferiSize)
	alsaIoctlReadI      = alsaIOC(iocRead, 'A', 0x51, alsaXferiSize)
	alsaIoctlCardInfo   = alsaIOC(iocRead, 'U', 0x01, alsaCardInfoSize)
	alsaIoctlPCMInfo    = alsaIOC(iocRead|iocWrite, 'U', 0x31, alsaPCMInfoSize)
)

// alsaSys is the part of the kernel the backend uses; tests replace it.
type alsaSys interface {
	// names lists /dev/snd.
	names() ([]string, error)
	// sysfsID reads the card ID from sysfs, for cards without a control device.
	sysfsID(card int) (string, error)
	// open opens /dev/snd/<name> without blocking.
	open(name string, pcm bool) (alsaDev, error)
}

// alsaDev is an open control or PCM device.
type alsaDev interface {
	// ioctl runs req with arg as its argument structure; arg is nil for
	// requests without one and must have the size req encodes.
	ioctl(req uint32, arg []byte) error
	// transfer runs READI or WRITEI on the whole frames in buf.
	transfer(req uint32, buf []byte, frameBytes int) (int, error)
	// wait polls the device (when pcm is set) and the wake event for up to
	// timeout (negative: no limit). woken reports a wake call.
	wait(timeout time.Duration, pcm bool) (revents int16, woken bool, err error)
	// wake makes the current or next wait return.
	wake()
	close()
}

const alsaDir = "/dev/snd"

type alsaKernel struct{}

func (alsaKernel) names() ([]string, error) {
	f, err := os.Open(alsaDir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

func (alsaKernel) sysfsID(card int) (string, error) {
	f, err := os.Open("/sys/class/sound/card" + strconv.Itoa(card) + "/id")
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 64)
	n, err := f.Read(b)
	if n == 0 {
		return "", err
	}
	return strings.TrimRight(string(b[:n]), "\n"), nil
}

func (alsaKernel) open(name string, pcm bool) (alsaDev, error) {
	flags := unix.O_RDONLY
	if pcm {
		flags = unix.O_RDWR
	}
	fd, err := unix.Open(alsaDir+"/"+name, flags|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: alsaDir + "/" + name, Err: err}
	}
	d := &alsaFD{fd: fd, ev: -1}
	if pcm {
		ev, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
		if err != nil {
			unix.Close(fd)
			return nil, os.NewSyscallError("eventfd", err)
		}
		d.ev = ev
	}
	return d, nil
}

type alsaFD struct {
	fd, ev int
}

func alsaSyscall(fd int, req uint32, p unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(p))
		switch e {
		case 0:
			return nil
		case unix.EINTR:
			continue
		}
		return e
	}
}

func (d *alsaFD) ioctl(req uint32, arg []byte) error {
	if len(arg) != int(req>>16&0x3fff) {
		return unix.EINVAL
	}
	var p unsafe.Pointer
	if len(arg) > 0 {
		p = unsafe.Pointer(&arg[0])
	}
	err := alsaSyscall(d.fd, req, p)
	runtime.KeepAlive(arg)
	return err
}

// alsaXferi is struct snd_xferi.
type alsaXferi struct {
	result int64
	buf    unsafe.Pointer
	frames uint64
}

func (d *alsaFD) transfer(req uint32, buf []byte, frameBytes int) (int, error) {
	frames := len(buf) / max(frameBytes, 1)
	if frames == 0 {
		return 0, nil
	}
	x := alsaXferi{buf: unsafe.Pointer(&buf[0]), frames: uint64(frames)}
	err := alsaSyscall(d.fd, req, unsafe.Pointer(&x))
	runtime.KeepAlive(buf)
	if err != nil {
		return 0, err
	}
	if x.result < 0 || x.result > int64(frames) {
		return 0, unix.EIO
	}
	return int(x.result), nil
}

func (d *alsaFD) wait(timeout time.Duration, pcm bool) (int16, bool, error) {
	fds := []unix.PollFd{{Fd: int32(d.ev), Events: unix.POLLIN}, {Fd: int32(d.fd), Events: unix.POLLIN | unix.POLLOUT}}
	if !pcm {
		fds = fds[:1]
	}
	ms := -1
	if timeout >= 0 {
		ms = int(min(timeout.Milliseconds(), 1<<30))
	}
	for {
		_, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, false, os.NewSyscallError("poll", err)
		}
		break
	}
	if fds[0].Revents != 0 {
		var b [8]byte
		unix.Read(d.ev, b[:])
		return 0, true, nil
	}
	if !pcm {
		return 0, false, nil
	}
	return fds[1].Revents, false, nil
}

func (d *alsaFD) wake() {
	if d.ev < 0 {
		return
	}
	b := [8]byte{1}
	unix.Write(d.ev, b[:])
}

func (d *alsaFD) close() {
	unix.Close(d.fd)
	if d.ev >= 0 {
		unix.Close(d.ev)
	}
}
