//go:build !race

package snapshot

// secStoreRaceEnabled is false in a non-race build, so the
// assertBoundedAlloc-based defense lock-ins run. See the //go:build race
// variant for the rationale behind gating them on the race detector.
const secStoreRaceEnabled = false
