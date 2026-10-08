//go:build race

package server

// raceEnabled skips the peak-RSS gate: the race detector's shadow memory
// inflates resident size several times over.
const raceEnabled = true
