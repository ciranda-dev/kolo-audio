package koloaudio

// ring is a fixed-capacity FIFO of samples.
type ring struct {
	data []int16
	r, n int
}

func (b *ring) free() int { return len(b.data) - b.n }

// write appends as much of p as fits and returns the count.
func (b *ring) write(p []int16) int {
	total := min(len(p), b.free())
	p = p[:total]
	for len(p) > 0 {
		w := (b.r + b.n) % len(b.data)
		c := copy(b.data[w:min(len(b.data), w+b.free())], p)
		b.n += c
		p = p[c:]
	}
	return total
}

// overwrite appends p, dropping the oldest samples to make room, and
// returns the number of samples dropped.
func (b *ring) overwrite(p []int16) int {
	dropped := 0
	if len(p) > len(b.data) {
		dropped = len(p) - len(b.data)
		p = p[dropped:]
	}
	if need := len(p) - b.free(); need > 0 {
		b.discard(need)
		dropped += need
	}
	b.write(p)
	return dropped
}

// read moves up to len(p) samples into p and returns the count.
func (b *ring) read(p []int16) int {
	total := min(len(p), b.n)
	p = p[:total]
	for len(p) > 0 {
		c := copy(p, b.data[b.r:min(len(b.data), b.r+b.n)])
		b.discard(c)
		p = p[c:]
	}
	return total
}

func (b *ring) discard(n int) {
	b.r = (b.r + n) % len(b.data)
	b.n -= n
	if b.n == 0 {
		b.r = 0
	}
}
