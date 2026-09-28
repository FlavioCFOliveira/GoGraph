//go:build race

package exec_test

// raceEnabled reports whether the race detector is active. The race runtime
// changes allocation counts (it disables, for example, the append-of-make
// optimisation), so allocation-pinning tests skip under it.
const raceEnabled = true
