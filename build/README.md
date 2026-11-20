# build

Maintainer tooling. Consumers never run it: the generated Go is committed.

## Layout

| Path | Role |
|---|---|
| `libopus/build.sh` | Builds `libopus.wasm` from the libopus 1.6.1 source lists in a Docker container with no network. Writes `SHA256SUMS`. |
| `libopus/exports.txt`, `libopus/shim.c` | Exported functions and the non-variadic control wrappers. |
| `regen.sh` | Runs wasm2go on every committed wasm and writes the Go packages. `regen.sh [rootdir [target...]]`. |
| `verify.sh` | Checks hashes, regenerated Go, notices. `--rebuild` also rebuilds every wasm in Docker. |
| `validate/` | Scripts that build every platform's test binaries and run them on macOS, Linux and Windows. See `validate/README.md`. |
| `tools/go.mod` | Pins wasm2go v0.4.22 with a `tool` directive. |

A second pipeline (a directory with `build.sh` and `SHA256SUMS`, plus one `gen` line in `regen.sh`)
is picked up by `verify.sh` and selectable by name in `regen.sh` without other changes.

## Regeneration

```sh
build/regen.sh                 # all targets
build/regen.sh "$PWD" libopus  # one target, same tree
```

`regen.sh` runs `go tool wasm2go` inside `build/tools` with `GOWORK=off` and `GOFLAGS=`. The tool and
its one dependency (`golang.org/x/tools`) come from the module cache or the network, verified by
`build/tools/go.sum`. No other setting is needed.

- Warm cache: works with `GOPROXY=off`.
- Cold cache and `GOPROXY=off`: fails with `module lookup disabled by GOPROXY=off`. Run once with the
  network (or `go mod download` in `build/tools`) to fill the cache.
- `go run github.com/ncruces/wasm2go@v0.4.22` is not used: it fails with `GOPROXY=off` even on a warm
  cache (`loading deprecation ... module lookup disabled`), and `GOFLAGS=-mod=mod` does not change that.

Each output is written next to its destination and renamed into place, so a reader never sees a
partial file, and an unchanged output is not touched. A generated file must start with the
`// Code generated ... DO NOT EDIT.` line (the generator writes it; `regen.sh` fails otherwise), which
is what tells staticcheck and gofmt tooling to skip it.

## Static analysis

staticcheck skips generated files for most checks, but SA4000 and SA4006 report on the translated
code anyway. Each package that holds generated code carries a `staticcheck.conf` with
`checks = ["inherit", "-SA4000", "-SA4006"]` (`opus/internal/libopus/staticcheck.conf` and
`internal/maresample/staticcheck.conf`). Every other check still runs on them. Checked with
staticcheck 2026.2.2: `staticcheck ./...` is clean for the module. `verify.sh` checks that every
generated file starts with the `// Code generated ... DO NOT EDIT.` line, that the notice files next to
the two generated packages equal the ones in `third_party/licenses`, and, with `--rebuild`, that `LICENSE-miniaudio` equals the
licence block of the pinned miniaudio header.

## Notices

`LICENSE-wasi-libc` lists the wasi-libc objects that are linked, read from the linker maps of both
builds (`-Wl,-Map` added to the link line in a scratch copy of each `build.sh`; the output kept the
committed hash). After a change to a build script, to the sources or to the SDK, repeat that and
update the list.

## Generated file imports

`opus/internal/libopus/libopus.go` imports `encoding/binary`, `math`, `math/bits`, `runtime` and
`unsafe`. `runtime` is used for constants only.

## libopus limits

| Setting | Value | Why |
|---|---|---|
| Stack | 262144 B (256 KiB) | `TestStackHighWater` paints the stack, runs the matrix and measures the deepest use. Policy: the stack is at least twice the high-water mark. |
| `--max-memory` | 1048576 B (16 pages) | Twice the measured peak of a module that holds the largest buffers a wrapper allocates. |

Sources for the memory bound:

- A packet has at most 48 frames of at most 1275 bytes (RFC 6716 sections 3.2.1 and 3.2.5), so at
  most 2 + 47 * 2 + 48 * 1275 = 61296 bytes. A packet carries at most 120 ms.
- 120 ms of stereo 48 kHz 16-bit PCM is 5760 * 2 * 2 = 23040 bytes.
- The test module holds one packet buffer of 61296 bytes, one 4000-byte output buffer and two PCM
  buffers of 23040 bytes, a stereo encoder and a stereo decoder.

The generated `maxMem` constant carries the bound. `TestMemoryPeak` fails if it differs from the
value in the test, or if the peak exceeds half of it. `TestMemoryLimit` exhausts a module: it stops at
the limit, `malloc` returns NULL, and `opus_encoder_create` returns the allocation error instead of
growing to 4 GiB. Measured: the peak of that module is 8 pages (it starts at 5), so the cap is 16; `malloc(1 GiB)` returns 0 and `opus_encoder_create` returns -7 (allocation failure); the deepest stack use is 108608 B (41.4 percent of 256 KiB).
