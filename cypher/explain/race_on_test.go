//go:build race

package explain

// raceEnabled reports whether the race detector is active. Under the
// race build tag it is always true.
const raceEnabled = true
