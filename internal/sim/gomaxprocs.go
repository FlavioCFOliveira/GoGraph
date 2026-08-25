package sim

import "sync"

// gomaxprocsMu serialises every scenario that mutates process-global
// `GOMAXPROCS` against every scenario running beside it.
//
// # Why this exists
//
// `GOMAXPROCS` is PROCESS-GLOBAL. A scenario that clamps it does not merely slow
// itself down: it removes the parallelism that every co-resident scenario's
// claims are built on. [Swarm] runs Workers scenarios at once inside one
// process, so without this lock a clamping scenario silently decides the regime
// of whatever its neighbours happened to draw.
//
// That is not hypothetical. A DST exercise on 2026-08-25 (11141 runs at
// `-workers=3`) produced 38 failures and every one of them traced here: 37
// `bolt-decode-swarm` runs whose co-residence clauses cannot be satisfied at
// `GOMAXPROCS=1`, and one `pagerank-ranker` run that caught the clamp directly
// through [prWithClamp]'s read-back. Holding the seeds fixed and varying ONLY
// whether `cpu-starvation` ran alongside them moved the result from 0/37 to
// 37/37 (rmp #2613).
//
// # The contract
//
//   - A scenario that WRITES `GOMAXPROCS` declares `ClampsGOMAXPROCS` on its
//     [Scenario] and takes the write side via [holdGOMAXPROCSExclusive]. It
//     therefore runs alone.
//   - Every other scenario the swarm dispatches takes the read side via
//     [holdGOMAXPROCSShared]. Many run together, but none can overlap a clamp.
//
// Serialising the two clampers against each other also closes the interleaved
// save/restore that could otherwise leave the process permanently clamped — A
// saves 8 and sets 1, B saves 1 and sets 4, A restores 8, B restores 1 (rmp
// #2606).
//
// # Why a package-level variable
//
// `global_state_guard_test.go` polices package-level state in this package. This
// is not the shape it forbids: that guard reports state a TEST file writes to,
// and nothing writes to this one. It is a lock over a resource the RUNTIME owns,
// not shared state of ours, so there is no parameter to thread it through — the
// resource being serialised is not ours to pass.
var gomaxprocsMu sync.RWMutex

// holdGOMAXPROCSExclusive blocks until no scenario holds the shared side, then
// takes the process's `GOMAXPROCS` exclusively and returns the release.
//
// The caller may clamp `GOMAXPROCS` freely for as long as it holds the returned
// release, and MUST restore the previous value before calling it.
func holdGOMAXPROCSExclusive() (release func()) {
	gomaxprocsMu.Lock()
	return gomaxprocsMu.Unlock
}

// holdGOMAXPROCSShared blocks while a clamping scenario holds the process, then
// registers this caller as one of many that require the process's normal
// parallelism, and returns the release.
//
// It grants no exclusivity against other shared holders: they are precisely the
// scenarios that are safe to run together.
func holdGOMAXPROCSShared() (release func()) {
	gomaxprocsMu.RLock()
	return gomaxprocsMu.RUnlock
}
