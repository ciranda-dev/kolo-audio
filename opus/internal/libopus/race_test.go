//go:build race

package libopus_test

// raceEnabled shrinks the matrix: the race detector slows translated code about 60 times.
const raceEnabled = true
