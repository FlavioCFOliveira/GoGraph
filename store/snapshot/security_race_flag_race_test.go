//go:build race

package snapshot

// secStoreRaceEnabled is true when the test binary is built with -race. The
// security battery uses it to skip the assertBoundedAlloc-based defense
// lock-ins, which read process-global runtime.MemStats: that read is both
// unreliable under concurrent allocation and flagged by the race detector
// against shared runtime state. The error-classification assertions those
// lock-ins complement remain covered, race-safe, by the sibling component
// tests.
const secStoreRaceEnabled = true
