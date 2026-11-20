package opus_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ciranda-dev/kolo-audio/opus/internal/compare"
)

func readPCM(t *testing.T, path string) []int16 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, len(b)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return pcm
}

// TestConformance decodes the RFC 8251 vectors in $KOLO_OPUS_VECTORS, as mono
// and as stereo, checks the range coder state of every packet and judges the
// output with the opus_compare port. With $KOLO_OPUS_DUMP set it also writes
// the decoded PCM there.
func TestConformance(t *testing.T) {
	dir := os.Getenv("KOLO_OPUS_VECTORS")
	if dir == "" {
		t.Skip("KOLO_OPUS_VECTORS is not set")
	}
	dump := os.Getenv("KOLO_OPUS_DUMP")
	vectors, rangeBad, compareBad := 0, 0, 0
	for v := 1; v <= 12; v++ {
		bit, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("testvector%02d.bit", v)))
		if err != nil {
			t.Fatal(err)
		}
		refs := [][]int16{
			readPCM(t, filepath.Join(dir, fmt.Sprintf("testvector%02d.dec", v))),
			readPCM(t, filepath.Join(dir, fmt.Sprintf("testvector%02dm.dec", v))),
		}
		for _, ch := range []int{1, 2} {
			d := newDecoder(t, 48000, ch)
			pcm := make([]int16, 5760*ch)
			var out []int16
			for data := bit; len(data) >= 8; {
				n := int(binary.BigEndian.Uint32(data))
				want := binary.BigEndian.Uint32(data[4:])
				if len(data) < 8+n {
					t.Fatalf("vector %d: truncated packet", v)
				}
				packet := data[8 : 8+n]
				data = data[8+n:]
				got, err := d.Decode(packet, pcm)
				if err != nil {
					t.Fatalf("vector %d, %d ch: %v", v, ch, err)
				}
				out = append(out, pcm[:got]...)
				if r, err := d.FinalRange(); err != nil || r != want {
					rangeBad++
				}
			}
			if dump != "" {
				b := make([]byte, 2*len(out))
				for i, s := range out {
					binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
				}
				name := filepath.Join(dump, fmt.Sprintf("v%02d-ch%d.pcm", v, ch))
				if err := os.WriteFile(name, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var best compare.Result
			ok := false
			for _, ref := range refs {
				r, err := compare.Compare(ref, out, ch, 48000)
				if err == nil && r.Pass && (!ok || r.Quality > best.Quality) {
					best, ok = r, true
				}
			}
			vectors++
			if !ok {
				compareBad++
			}
			t.Logf("vector %02d ch%d: quality %.1f", v, ch, best.Quality)
		}
	}
	t.Logf("vectors=%d range_mismatch=%d compare_fail=%d", vectors, rangeBad, compareBad)
	if vectors != 24 || rangeBad != 0 || compareBad != 0 {
		t.Fatalf("vectors=%d range_mismatch=%d compare_fail=%d", vectors, rangeBad, compareBad)
	}
}
