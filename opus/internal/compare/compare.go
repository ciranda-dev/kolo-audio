// Package compare is a Go port of the libopus 1.6.1 tool src/opus_compare.c,
// used by the tests to judge decoder output against the reference vectors.
//
// Copyright (c) 2011-2012 Xiph.Org Foundation, Mozilla Corporation
// Written by Jean-Marc Valin and Timothy B. Terriberry
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
//
//   - Redistributions of source code must retain the above copyright
//     notice, this list of conditions and the following disclaimer.
//
//   - Redistributions in binary form must reproduce the above copyright
//     notice, this list of conditions and the following disclaimer in the
//     documentation and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// “AS IS” AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER
// OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
// EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
// PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
// PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
// LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
// NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
// SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
package compare

import (
	"errors"
	"fmt"
	"math"
)

const (
	nBands     = 21
	nFreqs     = 240
	testWinLen = 480
	testWinHop = 120
	pi32       = float32(3.14159265)
)

var bands = [nBands + 1]int{0, 2, 4, 6, 8, 10, 12, 14, 16, 20, 24, 28, 32, 40, 48, 56, 68, 80, 96, 120, 156, 200}

// Result is the outcome of one comparison.
type Result struct {
	// Quality is the Opus quality metric in percent; the vector passes when it is not negative.
	Quality float64
	// Error is the internal weighted error.
	Error float64
	// Pass reports Quality >= 0.
	Pass bool
}

// Compare compares test, decoded PCM of channels (1 or 2) at rate Hz, with
// ref, the stereo 48 kHz reference decode, as opus_compare does. A mono test
// is compared with the mean of the reference channels.
func Compare(ref, test []int16, channels, rate int) (Result, error) {
	if channels != 1 && channels != 2 {
		return Result{}, fmt.Errorf("compare: %d channels", channels)
	}
	ybands, downsample := nBands, 1
	switch rate {
	case 48000:
	case 24000:
		ybands, downsample = 19, 2
	case 16000:
		ybands, downsample = 17, 3
	case 12000:
		ybands, downsample = 15, 4
	case 8000:
		ybands, downsample = 13, 6
	default:
		return Result{}, fmt.Errorf("compare: sample rate %d", rate)
	}
	yfreqs := nFreqs / downsample
	if len(ref)%2 != 0 || len(test)%channels != 0 {
		return Result{}, errors.New("compare: partial sample frame")
	}
	xlength, ylength := len(ref)/2, len(test)/channels
	if xlength != ylength*downsample {
		return Result{}, fmt.Errorf("compare: sample counts do not match (%d != %d)", xlength, ylength*downsample)
	}
	if xlength < testWinLen {
		return Result{}, fmt.Errorf("compare: insufficient sample data (%d < %d)", xlength, testWinLen)
	}
	x := make([]float32, xlength*channels)
	if channels == 1 {
		for i := range xlength {
			x[i] = float32(.5 * (float64(ref[2*i]) + float64(ref[2*i+1])))
		}
	} else {
		for i, v := range ref {
			x[i] = float32(v)
		}
	}
	y := make([]float32, len(test))
	for i, v := range test {
		y[i] = float32(v)
	}

	nframes := (xlength - testWinLen + testWinHop) / testWinHop
	xb := make([]float32, nframes*nBands*channels)
	bx := make([]float32, nframes*nFreqs*channels)
	by := make([]float32, nframes*yfreqs*channels)
	bandEnergy(xb, bx, nBands, x, channels, nframes, testWinLen, testWinHop, 1)
	bandEnergy(nil, by, ybands, y, channels, nframes, testWinLen/downsample, testWinHop/downsample, downsample)

	for xi := range nframes {
		for bi := 1; bi < nBands; bi++ {
			for ci := range channels {
				xb[(xi*nBands+bi)*channels+ci] += 0.1 * xb[(xi*nBands+bi-1)*channels+ci]
			}
		}
		for bi := nBands - 2; bi >= 0; bi-- {
			for ci := range channels {
				xb[(xi*nBands+bi)*channels+ci] += 0.03 * xb[(xi*nBands+bi+1)*channels+ci]
			}
		}
		if xi > 0 {
			for bi := range nBands {
				for ci := range channels {
					xb[(xi*nBands+bi)*channels+ci] += 0.5 * xb[((xi-1)*nBands+bi)*channels+ci]
				}
			}
		}
		if channels == 2 {
			for bi := range nBands {
				l, r := xb[(xi*nBands+bi)*channels], xb[(xi*nBands+bi)*channels+1]
				xb[(xi*nBands+bi)*channels] += 0.01 * r
				xb[(xi*nBands+bi)*channels+1] += 0.01 * l
			}
		}
		for bi := range ybands {
			for xj := bands[bi]; xj < bands[bi+1]; xj++ {
				for ci := range channels {
					m := 0.1 * xb[(xi*nBands+bi)*channels+ci]
					bx[(xi*nFreqs+xj)*channels+ci] += m
					by[(xi*yfreqs+xj)*channels+ci] += m
				}
			}
		}
	}

	for bi := range ybands {
		for xj := bands[bi]; xj < bands[bi+1]; xj++ {
			for ci := range channels {
				xtmp, ytmp := bx[xj*channels+ci], by[xj*channels+ci]
				for xi := 1; xi < nframes; xi++ {
					xtmp2, ytmp2 := bx[(xi*nFreqs+xj)*channels+ci], by[(xi*yfreqs+xj)*channels+ci]
					bx[(xi*nFreqs+xj)*channels+ci] += xtmp
					by[(xi*yfreqs+xj)*channels+ci] += ytmp
					xtmp, ytmp = xtmp2, ytmp2
				}
			}
		}
	}

	var maxCompare int
	switch rate {
	case 48000:
		maxCompare = bands[nBands]
	case 12000:
		maxCompare = bands[ybands]
	default:
		maxCompare = bands[ybands] - 3
	}
	var sum float64
	for xi := range nframes {
		var ef float64
		for bi := range ybands {
			var eb float64
			for xj := bands[bi]; xj < bands[bi+1] && xj < maxCompare; xj++ {
				for ci := range channels {
					re := by[(xi*yfreqs+xj)*channels+ci] / bx[(xi*nFreqs+xj)*channels+ci]
					im := float32(float64(re) - math.Log(float64(re)) - 1)
					if xj >= 79 && xj <= 81 {
						im *= 0.1
					}
					if xj == 80 {
						im *= 0.1
					}
					eb += float64(im)
				}
			}
			eb /= float64((bands[bi+1] - bands[bi]) * channels)
			ef += eb * eb
		}
		ef /= nBands
		ef *= ef
		sum += ef * ef
	}
	werr := math.Pow(sum/float64(nframes), 1.0/16)
	q := 100 * (1 - 0.5*math.Log(1+werr)/math.Log(1.13))
	return Result{Quality: float64(float32(q)), Error: werr, Pass: q >= 0}, nil
}

func bandEnergy(out, ps []float32, nb int, in []float32, channels, nframes, window, step, downsample int) {
	win := make([]float32, window)
	c := make([]float32, window)
	s := make([]float32, window)
	x := make([]float32, channels*window)
	psSize := window / 2
	for j := range window {
		win[j] = 0.5 - 0.5*float32(math.Cos(float64((2*pi32/float32(window-1))*float32(j))))
		c[j] = float32(math.Cos(float64((2 * pi32 / float32(window)) * float32(j))))
		s[j] = float32(math.Sin(float64((2 * pi32 / float32(window)) * float32(j))))
	}
	ds := float32(downsample)
	for xi := range nframes {
		for ci := range channels {
			for k := range window {
				x[ci*window+k] = win[k] * in[(xi*step+k)*channels+ci]
			}
		}
		xj := 0
		for bi := range nb {
			var p [2]float32
			for ; xj < bands[bi+1]; xj++ {
				for ci := range channels {
					var re, im float32
					ti := 0
					xc := x[ci*window : (ci+1)*window]
					for k := range window {
						re += c[ti] * xc[k]
						im -= s[ti] * xc[k]
						ti += xj
						if ti >= window {
							ti -= window
						}
					}
					re *= ds
					im *= ds
					v := re*re + im*im + 100000
					ps[(xi*psSize+xj)*channels+ci] = v
					p[ci] += v
				}
			}
			if out != nil {
				n := float32(bands[bi+1] - bands[bi])
				out[(xi*nb+bi)*channels] = p[0] / n
				if channels == 2 {
					out[(xi*nb+bi)*channels+1] = p[1] / n
				}
			}
		}
	}
}
