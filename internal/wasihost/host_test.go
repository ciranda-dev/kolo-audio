package wasihost_test

import (
	"errors"
	"go/parser"
	"go/token"
	"testing"

	"github.com/ciranda-dev/kolo-audio/internal/wasihost"
)

// Import interfaces as emitted by wasm2go for the libopus and resampler modules.
type opusImports = interface {
	Xclock_time_get(v0 int32, v1 int64, v2 int32) int32
	Xfd_close(v0 int32) int32
	Xfd_seek(v0 int32, v1 int64, v2, v3 int32) int32
	Xfd_write(v0, v1, v2, v3 int32) int32
}

type resamplerImports = interface {
	Xclock_time_get(v0 int32, v1 int64, v2 int32) int32
	Xfd_close(v0 int32) int32
	Xfd_prestat_dir_name(v0, v1, v2 int32) int32
	Xfd_prestat_get(v0, v1 int32) int32
	Xfd_seek(v0 int32, v1 int64, v2, v3 int32) int32
	Xfd_write(v0, v1, v2, v3 int32) int32
	Xproc_exit(v0 int32)
}

var (
	_ opusImports      = wasihost.Host{}
	_ resamplerImports = wasihost.Host{}
)

func TestErrno(t *testing.T) {
	var h wasihost.Host
	const badf, nosys = 8, 52
	for name, got := range map[string]int32{
		"fd_close":            h.Xfd_close(1),
		"fd_seek":             h.Xfd_seek(1, 0, 0, 0),
		"fd_write":            h.Xfd_write(1, 0, 0, 0),
		"fd_prestat_get":      h.Xfd_prestat_get(3, 0),
		"fd_prestat_dir_name": h.Xfd_prestat_dir_name(3, 0, 0),
	} {
		if got != badf {
			t.Errorf("%s = %d, want EBADF (%d)", name, got, badf)
		}
	}
	if got := h.Xclock_time_get(0, 0, 0); got != nosys {
		t.Errorf("clock_time_get = %d, want ENOSYS (%d)", got, nosys)
	}
}

func TestProcExit(t *testing.T) {
	defer func() {
		err, ok := recover().(error)
		if !ok {
			t.Fatal("proc_exit did not panic with an error")
		}
		var e wasihost.Exit
		if !errors.As(err, &e) || e != 3 {
			t.Fatalf("panic = %v, want Exit(3)", err)
		}
		if want := "koloaudio: wasm module exited with status 3"; err.Error() != want {
			t.Fatalf("message = %q, want %q", err.Error(), want)
		}
	}()
	wasihost.Host{}.Xproc_exit(3)
}

func TestHostDoesNotImportOS(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "host.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if imp.Path.Value == `"os"` {
			t.Fatal("host.go imports os: proc_exit must panic, never exit the process")
		}
	}
}
