//go:build !race && !gograph_debug

package lpg

// barrierGuard is the production (non-enforcing) form of the barrier
// re-entrancy guard. It is compiled into every binary built without `-race` and
// without `-tags gograph_debug` — which is every released binary. Every method
// has an empty body over a zero-sized struct, so the compiler elides the calls
// entirely and [Graph.View] and [Graph.ApplyAtomically] pay nothing beyond the
// visMu acquisition itself.
//
// # Why the guard is not always on
//
// The enforcing implementation (reentrancy_enabled.go) must answer "is the
// CURRENT goroutine already inside the barrier?", and the only supported way to
// identify a goroutine is [goID], which parses the first line of
// runtime.Stack. runtime.Stack takes the Go runtime's process-global debuglock
// once per stack frame, so it does not just cost time — it serialises callers
// against each other.
//
// Measured independently by two streams of the round-3 comparative audit,
// agreeing to within noise:
//
//   - Graph.View cost 1.65 us serially and 3.29 us at 10 cores, with a 64 B
//     allocation per call, against 3.6 ns for the bare RWMutex pair it guards.
//     The guard was 97-99% of the whole operation.
//   - Aggregate read throughput HALVED from 1 to 10 cores, because every reader
//     contended on the runtime's debuglock rather than on the graph.
//
// The guard makes no isolation decision and protects no data path: it exists
// purely to convert a would-be deadlock into an actionable panic. Paying an
// anti-scaling tax on every production read for a development-time diagnostic
// is the wrong trade, so the diagnostic moves to the builds that want it.
//
// # What is given up, and why that is acceptable
//
// In a released binary a same-goroutine nested barrier acquisition deadlocks
// silently instead of panicking with an explanation. That is a real loss of
// diagnosability, mitigated three ways:
//
//   - The invariant is documented on both public methods, which state plainly
//     that neither may be called from a goroutine already inside the barrier.
//   - The local gate runs `go test -race ./...`, so the guard is enforced
//     against the whole test suite on every change — including the TCK, the
//     crash-injection battery and the concurrency stress tests. A nesting bug
//     introduced by new code is caught before it can be released.
//   - Any build can re-enable enforcement with `-tags gograph_debug`, which is
//     the first thing to reach for when diagnosing a suspected engine freeze.
//
// Production has never nested; the guard was added (task #1286) to enforce an
// invariant that future work — a `CALL { … } IN TRANSACTIONS`, a user-defined
// procedure, a nested Engine.Run — could otherwise violate silently. Catching
// that in the gate rather than in production is sufficient for its purpose.
//
// This type deliberately carries no fields: the enforcing form's reader map and
// boundary mutex are absent from a released binary, so a Graph is smaller as
// well as faster.
type barrierGuard struct{}

// init is a no-op; there is no reader map to pre-create.
func (bg *barrierGuard) init() {}

// checkWriter performs no check and records nothing. It returns 0, the "no
// goroutine recorded" sentinel that [barrierGuard.stampWriter] and
// [barrierGuard.clearWriter] both treat as "nothing to do", so the call sites in
// [Graph.ApplyAtomically] and [Graph.LockBarrier] need no build-tag awareness.
func (bg *barrierGuard) checkWriter() int64 { return 0 }

// currentGID returns the sentinel 0 rather than paying for a goroutine id.
func (bg *barrierGuard) currentGID() int64 { return 0 }

// stampWriter records nothing.
func (bg *barrierGuard) stampWriter(int64) {}

// clearWriter clears nothing.
func (bg *barrierGuard) clearWriter(int64) {}

// enterReader performs no check and records nothing, returning the same 0
// sentinel as [barrierGuard.checkWriter].
func (bg *barrierGuard) enterReader() int64 { return 0 }

// exitReader clears nothing.
func (bg *barrierGuard) exitReader(int64) {}
