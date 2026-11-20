// Package wasihost provides the WASI imports of the generated wasm2go packages.
// The host has no clock, no files and no output; every call fails.
package wasihost

import "strconv"

const (
	errnoBadf  = 8
	errnoNosys = 52
)

// Host implements the import interface of every generated package.
type Host struct{}

// Exit is the panic value of a module that calls proc_exit.
type Exit int32

// Error implements error.
func (e Exit) Error() string {
	return "koloaudio: wasm module exited with status " + strconv.Itoa(int(e))
}

// Xclock_time_get reports that there is no clock.
func (Host) Xclock_time_get(int32, int64, int32) int32 { return errnoNosys }

// Xfd_close reports that no descriptor is open.
func (Host) Xfd_close(int32) int32 { return errnoBadf }

// Xfd_seek reports that no descriptor is open.
func (Host) Xfd_seek(int32, int64, int32, int32) int32 { return errnoBadf }

// Xfd_write reports that no descriptor is open.
func (Host) Xfd_write(int32, int32, int32, int32) int32 { return errnoBadf }

// Xfd_prestat_get reports that no directory is preopened.
func (Host) Xfd_prestat_get(int32, int32) int32 { return errnoBadf }

// Xfd_prestat_dir_name reports that no directory is preopened.
func (Host) Xfd_prestat_dir_name(int32, int32, int32) int32 { return errnoBadf }

// Xproc_exit panics with Exit.
func (Host) Xproc_exit(code int32) { panic(Exit(code)) }
