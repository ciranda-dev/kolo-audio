package koloaudio

import (
	"fmt"
	"math"
)

func clamp16(v float64) int16 {
	switch {
	case v >= math.MaxInt16:
		return math.MaxInt16
	case v <= math.MinInt16:
		return math.MinInt16
	}
	return int16(math.Round(v))
}

// Gain multiplies every sample by factor in place, saturating at the int16
// limits. factor must be finite; a negative factor also inverts the phase.
func Gain(pcm []int16, factor float64) error {
	if math.IsNaN(factor) || math.IsInf(factor, 0) {
		return fmt.Errorf("%w: gain %v", ErrInvalidArgument, factor)
	}
	for i, v := range pcm {
		pcm[i] = clamp16(float64(v) * factor)
	}
	return nil
}

// Mix adds src into dst sample by sample, saturating at the int16 limits,
// and returns the number of samples mixed, min(len(dst), len(src)).
func Mix(dst, src []int16) int {
	n := min(len(dst), len(src))
	for i := range n {
		dst[i] = int16(max(math.MinInt16, min(math.MaxInt16, int32(dst[i])+int32(src[i]))))
	}
	return n
}

// Level returns the peak and RMS level of pcm in dBFS, where 0 dBFS is a
// sample magnitude of 32768. A full-scale sine has an RMS of -3.01 dBFS.
// Silence and an empty slice return -Inf for both.
func Level(pcm []int16) (peak, rms float64) {
	var maxAbs, sum float64
	for _, v := range pcm {
		a := math.Abs(float64(v))
		maxAbs = max(maxAbs, a)
		sum += a * a
	}
	if maxAbs == 0 {
		return math.Inf(-1), math.Inf(-1)
	}
	return 20 * math.Log10(maxAbs/32768), 20 * math.Log10(math.Sqrt(sum/float64(len(pcm)))/32768)
}

// ConvertChannels converts interleaved frames in src from one channel count
// to another into dst and returns the number of samples written. It copies
// when the counts match, duplicates a mono channel, and averages all
// channels down to mono; other conversions return ErrUnsupportedFormat.
// dst must hold len(src)/from*to samples and must not overlap src.
func ConvertChannels(dst, src []int16, from, to int) (int, error) {
	if from < 1 || from > MaxChannels || to < 1 || to > MaxChannels {
		return 0, fmt.Errorf("%w: %d to %d channels", ErrUnsupportedFormat, from, to)
	}
	if from != to && from != 1 && to != 1 {
		return 0, fmt.Errorf("%w: %d to %d channels", ErrUnsupportedFormat, from, to)
	}
	if len(src)%from != 0 {
		return 0, fmt.Errorf("%w: %d samples is not a whole number of %d-channel frames", ErrInvalidArgument, len(src), from)
	}
	frames := len(src) / from
	if frames > math.MaxInt/to {
		return 0, fmt.Errorf("%w: %d frames of %d channels overflow", ErrInvalidArgument, frames, to)
	}
	if len(dst) < frames*to {
		return 0, fmt.Errorf("%w: dst holds %d samples, need %d", ErrInvalidArgument, len(dst), frames*to)
	}
	switch {
	case from == to:
		copy(dst, src)
	case from == 1:
		for i, v := range src {
			for c := range to {
				dst[i*to+c] = v
			}
		}
	default:
		for i := range frames {
			var sum int
			for _, v := range src[i*from : (i+1)*from] {
				sum += int(v)
			}
			dst[i] = int16(sum / from)
		}
	}
	return frames * to, nil
}
