package lpg

import (
	"runtime"
	"strconv"
)

// goID returns the numeric identifier of the calling goroutine.
//
// The Go runtime deliberately does not export goroutine identifiers, because
// goroutine-local state is an anti-pattern in almost all code. This package
// uses the id for exactly one narrow, internal purpose: the re-entrancy guard
// in [Graph.View] / [Graph.ApplyAtomically] must answer the question "is the
// CURRENT goroutine already inside the visibility barrier?", and the only
// stable way to identify "the current goroutine" without goroutine-local
// storage is its id. The id is used solely to detect a would-be deadlock and
// fail fast with a clear panic; it is never persisted, exposed, or relied on
// for correctness of any data path.
//
// Caveats of this technique:
//
//   - goroutine ids are NOT stable across a goroutine's lifetime in the sense
//     that an id is reused once a goroutine exits. That is harmless here: the
//     guard records an id only while a goroutine is actively inside the barrier
//     and removes it on exit, so a reused id can never be mistaken for a still
//     in-barrier goroutine.
//   - the id is parsed from the first line of runtime.Stack, whose format
//     ("goroutine <id> [<state>]:") has been stable across every Go release but
//     is not part of the language spec. If a future runtime changes it, parsing
//     fails closed: goID returns 0 (see below), the guard simply never trips,
//     and the long-standing "callers must not nest" contract reverts to being
//     documented-but-unenforced — never a false positive, never a crash.
//
// goID returns 0 if the runtime line cannot be parsed. 0 is therefore reserved
// as a "no goroutine" sentinel by the guard and is never treated as a real id.
func goID() int64 {
	// "goroutine 18446744073709551615 [running]:" is the longest plausible
	// prefix (max uint64); 64 bytes is comfortably enough and is stack-allocated.
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// Expected prefix: "goroutine <id> ["
	const prefix = "goroutine "
	line := buf[:n]
	if len(line) < len(prefix) {
		return 0
	}
	line = line[len(prefix):]
	// The id is the run of decimal digits up to the first space.
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0
	}
	id, err := strconv.ParseInt(string(line[:i]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}
