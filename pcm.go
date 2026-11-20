package koloaudio

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// EncodeS16LE appends pcm to dst as little-endian signed 16-bit samples and
// returns the extended slice.
func EncodeS16LE(dst []byte, pcm []int16) []byte {
	for _, v := range pcm {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(v))
	}
	return dst
}

// DecodeS16LE appends the little-endian signed 16-bit samples in b to dst
// and returns the extended slice. An odd len(b) returns ErrInvalidArgument
// and dst unchanged.
func DecodeS16LE(dst []int16, b []byte) ([]int16, error) {
	if len(b)%2 != 0 {
		return dst, fmt.Errorf("%w: odd byte count %d", ErrInvalidArgument, len(b))
	}
	n := len(b) / 2
	dst = slices.Grow(dst, n)[:len(dst)+n]
	out := dst[len(dst)-n:]
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return dst, nil
}
