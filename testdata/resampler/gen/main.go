//go:build cgo

// Command gen writes the golden outputs of miniaudio's linear resampler to
// the directory given by -out. It drives the native ma_resampler through cgo,
// compiled from the pinned miniaudio.h, and needs KOLO_AUDIO_GEN_GOLDEN=1.
// Run it through ../gen.sh.
package main

/*
#define MA_NO_DEVICE_IO
#define MA_NO_THREADING
#define MA_NO_DECODING
#define MA_NO_ENCODING
#define MA_NO_WAV
#define MA_NO_FLAC
#define MA_NO_MP3
#define MA_NO_RESOURCE_MANAGER
#define MA_NO_NODE_GRAPH
#define MA_NO_ENGINE
#define MA_NO_GENERATION
#define MINIAUDIO_IMPLEMENTATION
#include <stdlib.h>
#include "miniaudio.h"

static ma_result gen_init(ma_resampler* r, ma_uint32 channels, ma_uint32 in, ma_uint32 out) {
	ma_resampler_config c = ma_resampler_config_init(ma_format_s16, channels, in, out, ma_resample_algorithm_linear);
	return ma_resampler_init(&c, NULL, r);
}
*/
import "C"

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"unsafe"
)

type resampler struct {
	p  *C.ma_resampler
	ch int
}

func newResampler(ch, in, out int) *resampler {
	p := (*C.ma_resampler)(C.calloc(1, C.sizeof_ma_resampler))
	if res := C.gen_init(p, C.ma_uint32(ch), C.ma_uint32(in), C.ma_uint32(out)); res != C.MA_SUCCESS {
		panic(fmt.Sprintf("ma_resampler_init: %d", int(res)))
	}
	return &resampler{p, ch}
}

func (r *resampler) process(dst, src []int16, outFrames, inFrames int) (int, int) {
	var in unsafe.Pointer
	if len(src) > 0 {
		in = unsafe.Pointer(&src[0])
	}
	ci, co := C.ma_uint64(inFrames), C.ma_uint64(outFrames)
	if res := C.ma_resampler_process_pcm_frames(r.p, in, &ci, unsafe.Pointer(&dst[0]), &co); res != C.MA_SUCCESS {
		panic(fmt.Sprintf("ma_resampler_process_pcm_frames: %d", int(res)))
	}
	return int(co), int(ci)
}

func (r *resampler) expected(inFrames int) int {
	var out C.ma_uint64
	if C.ma_resampler_get_expected_output_frame_count(r.p, C.ma_uint64(inFrames), &out) != C.MA_SUCCESS {
		return 0
	}
	return int(min(uint64(out), math.MaxInt))
}

func (r *resampler) latency() int { return int(C.ma_resampler_get_output_latency(r.p)) }

func (r *resampler) flush() []int16 {
	pending := int(C.ma_resampler_get_input_latency(r.p))
	need := r.expected(pending)
	dst := make([]int16, need*r.ch)
	if need > 0 {
		r.process(dst, nil, need, pending)
	}
	C.ma_resampler_reset(r.p)
	return dst
}

func (r *resampler) free() {
	C.ma_resampler_uninit(r.p, nil)
	C.free(unsafe.Pointer(r.p))
}

type caseJSON struct {
	Name           string `json:"name"`
	Channels       int    `json:"channels"`
	InRate         int    `json:"in_rate"`
	OutRate        int    `json:"out_rate"`
	Seed           int    `json:"seed"`
	InFrames       int    `json:"in_frames"`
	Latency        int    `json:"latency"`
	OutputLen      int    `json:"output_len"`
	Samples        int    `json:"samples"`
	SHA256         string `json:"sha256"`
	ChunkedSamples int    `json:"chunked_samples"`
	ChunkedSHA256  string `json:"chunked_sha256"`
	ReusedSamples  int    `json:"reused_samples"`
	ReusedSHA256   string `json:"reused_sha256"`
}

var cases = [][3]int{{1, 44100, 48000}, {2, 44100, 48000}, {1, 48000, 16000}, {1, 16000, 48000}, {2, 96000, 48000}, {1, 8000, 48000}, {2, 48000, 44100}}

func input(idx, ch, rate int) []int16 {
	rng := rand.New(rand.NewPCG(1, uint64(idx)))
	src := make([]int16, rate*ch)
	for i := range src {
		src[i] = int16(rng.IntN(65536) - 32768)
	}
	return src
}

func whole(r *resampler, src []int16) []int16 {
	var out []int16
	dst := make([]int16, 4096*r.ch)
	for len(src) > 0 {
		w, c := r.process(dst, src, 4096, len(src)/r.ch)
		if w == 0 && c == 0 {
			panic("no progress")
		}
		out = append(out, dst[:w*r.ch]...)
		src = src[c*r.ch:]
	}
	return append(out, r.flush()...)
}

func chunked(r *resampler, idx int, src []int16) []int16 {
	rng := rand.New(rand.NewPCG(2, uint64(idx)))
	var out []int16
	for len(src) > 0 {
		in := src[:min(len(src), (1+rng.IntN(1000))*r.ch)]
		src = src[len(in):]
		for len(in) > 0 {
			of := 1 + rng.IntN(2000)
			dst := make([]int16, of*r.ch)
			w, c := r.process(dst, in, of, len(in)/r.ch)
			if w == 0 && c == 0 {
				panic("no progress")
			}
			out = append(out, dst[:w*r.ch]...)
			in = in[c*r.ch:]
		}
	}
	return append(out, r.flush()...)
}

func digest(pcm []int16) ([]byte, string) {
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:])
}

func main() {
	out := flag.String("out", "", "directory to write the golden files to")
	flag.Parse()
	if os.Getenv("KOLO_AUDIO_GEN_GOLDEN") != "1" || *out == "" {
		fmt.Fprintln(os.Stderr, "gen: set KOLO_AUDIO_GEN_GOLDEN=1 and pass -out")
		os.Exit(2)
	}
	var list []caseJSON
	for i, c := range cases {
		ch, in, o := c[0], c[1], c[2]
		name := fmt.Sprintf("c%d_%dhz_to_%dhz", ch, in, o)
		src := input(i, ch, in)
		r := newResampler(ch, in, o)
		cj := caseJSON{Name: name, Channels: ch, InRate: in, OutRate: o, Seed: i, InFrames: in, Latency: r.latency(), OutputLen: r.expected(in)}
		w := whole(r, src)
		k := chunked(r, i, src)
		r.free()
		_, sum := digest(k)
		cj.ReusedSamples, cj.ReusedSHA256 = len(k), sum
		raw, sum := digest(w)
		cj.Samples, cj.SHA256 = len(w), sum
		if err := os.WriteFile(filepath.Join(*out, name+".s16le"), raw, 0o644); err != nil {
			panic(err)
		}
		r = newResampler(ch, in, o)
		k = chunked(r, i, src)
		r.free()
		_, sum = digest(k)
		cj.ChunkedSamples, cj.ChunkedSHA256 = len(k), sum
		list = append(list, cj)
	}
	b, _ := json.MarshalIndent(list, "", "\t")
	if err := os.WriteFile(filepath.Join(*out, "golden.json"), append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
}
