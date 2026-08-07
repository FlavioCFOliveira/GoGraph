//go:build race || gograph_debug

package lpg

import (
	"fmt"
	"sync"
)

// barrierGuard enforces that no single goroutine re-enters the
// transaction-visibility barrier ([Graph.View] / [Graph.ApplyAtomically]).
//
// # When this implementation is compiled
//
// This is the ENFORCING form, compiled only under `-race` or
// `-tags gograph_debug`. Released binaries link the no-op in
// reentrancy_disabled.go. The split exists because the guard's only way to
// answer "is the CURRENT goroutine already inside the barrier?" is a goroutine
// id, and the only supported way to obtain one is [goID], which calls
// runtime.Stack and takes the Go runtime's process-global debuglock once per
// stack frame. Measured by two independent streams of the round-3 comparative
// audit: that call was 97-99% of the cost of every Graph.View — 1.65 us serial
// and 3.29 us at 10 cores with a 64 B allocation, against 3.6 ns for the bare
// RWMutex pair it guards — and it made aggregate read throughput HALVE from 1
// to 10 cores, because every reader serialised on the runtime's debuglock. The
// guard makes no isolation decision, so paying that on every production read
// bought nothing.
//
// The local gate runs `go test -race ./...`, so the guard is enforced on every
// change; `-tags gograph_debug` turns it on for any other build. The cost of
// the split is that in a released binary a nested acquisition deadlocks
// silently instead of panicking — see the [Graph.View] and
// [Graph.ApplyAtomically] godoc, which state the contract, and
// reentrancy_disabled.go, which documents the trade-off in full.
//
// # Why a guard is needed
//
// visMu is a non-re-entrant [sync.RWMutex]. A goroutine that already holds the
// barrier and nests another acquisition deadlocks the whole engine:
//
//   - writer → writer / writer → reader: the nested call blocks forever waiting
//     for the lock the SAME goroutine already holds, which it can never release.
//   - reader → writer: the nested writer waits for the in-flight reader (itself)
//     to release; classic self-deadlock.
//   - reader → reader: deadlocks as soon as ANY writer is pending, because Go's
//     RWMutex stops admitting new readers once a writer is queued (writer
//     starvation avoidance) — so the nested RLock blocks behind the writer,
//     which blocks behind the outer reader (itself).
//
// Production never nests today, but the invariant was UNENFORCED: a future
// CALL { … } IN TRANSACTIONS, a user-defined procedure, or a nested Engine.Run
// would silently freeze the engine. The guard converts that silent hang into an
// immediate, actionable panic — the CLAUDE.md-sanctioned "programmer error
// surfaces immediately". The guard itself never recovers.
//
// # Mechanism and cost
//
// The barrier is entered once per query ([Graph.View]) or once per write
// transaction ([Graph.ApplyAtomically]) — never per row — so an O(1) bookkeeping
// step per acquisition is acceptable; there is no per-row overhead and no
// allocation on the common (non-nested) path:
//
//   - The serialised writer's identity is a single [sync/atomic] int64,
//     stamped immediately AFTER visMu.Lock succeeds and cleared (a CAS on the
//     goroutine's own id) immediately BEFORE visMu.Unlock, so it is exactly
//     "the goroutine currently HOLDING visMu in write mode" — never a writer
//     merely queued on the lock. Both [Graph.View] and [Graph.ApplyAtomically]
//     check it with one atomic load — no lock, no allocation — which catches
//     every nesting that involves the writer (writer→writer, writer→reader,
//     reader→writer). The entry-side check still runs BEFORE Lock, so the
//     panic fires instead of the lock deadlocking. A writer queued on visMu is
//     deliberately registered nowhere: between its entry check and blocking on
//     Lock it executes no user code, and while blocked it cannot call
//     anything, so no same-goroutine nested acquisition can originate from it.
//     (Task #1355: the previous stamp-before-Lock let a queued writer
//     overwrite the active writer's id, so the active writer's nested
//     View/ApplyAtomically sailed past the guard into the deadlock the guard
//     exists to prevent; the exit-side unconditional Store(0) likewise erased
//     the other writer's stamp.)
//   - Concurrent writers are tracked in a small set keyed by
//     goroutine id, guarded by a dedicated mutex (NOT visMu). The mutex is held
//     only for the O(1) insert/remove at the RLock/RUnlock boundary, never while
//     fn runs, so the read hot path stays exactly as lock-free as before. The
//     map is pre-created in [New] and bounded by the number of concurrently
//     active readers, so steady-state churn reuses buckets and does not allocate.
//
// goroutine ids come from [goID]; if the runtime makes that unparseable the
// helper returns 0, the guard simply stops tripping, and the contract reverts to
// documented-but-unenforced. The guard never produces a false positive against
// legitimate concurrent (different-goroutine) ApplyAtomically writers.
type barrierGuard struct {
	// writers holds the goroutine id of EVERY goroutine currently inside a
	// write bracket. It is stamped by [barrierGuard.stampWriter] once the
	// bracket is entered and cleared by [barrierGuard.clearWriter] on the way
	// out, so a writer merely QUEUED on visMu never appears here and can never
	// clobber an active writer's id (#1355).
	//
	// A SET rather than one id (rmp #2301). It was a single atomic.Int64
	// because visMu serialised writers, so at most one could be live at a time.
	// Sprint 334 retires that exclusion, and with two writers in flight the
	// single field reports FALSE RESULTS in both directions: the second
	// stampWriter overwrites the first's id, so the first's clearing
	// compare-and-swap fails and strands a stale entry, and a genuine
	// writer-to-writer re-entry on the overwritten goroutine goes undetected —
	// the guard's whole purpose.
	//
	// A map under a mutex is the right cost here BECAUSE this file is
	// //go:build race || gograph_debug: it is compiled only into the enforcing
	// build, and the released binary links the no-op form in
	// reentrancy_disabled.go, which allocates and locks nothing.
	writers map[int64]struct{}

	// writerMu guards writers. It is independent of the visibility gate and is
	// held only for the O(1) map mutation at a bracket boundary.
	writerMu sync.Mutex
}

// initBarrierGuard pre-creates the writer set so the common path never allocates
// the map into existence under the boundary mutex.
func (bg *barrierGuard) init() {
	bg.writers = make(map[int64]struct{})
}

// currentGID returns the calling goroutine's id for callers that need to pair a
// stamp with a clear without having gone through [barrierGuard.checkWriter] —
// specifically [Graph.UnlockBarrier], which clears the stamp taken by
// [Graph.LockBarrier] on the same goroutine. It exists so that no call site
// outside this file references [goID], which is compiled only in the enforcing
// build; the no-op form returns 0, and every method that consumes a gid treats
// 0 as "nothing recorded".
func (bg *barrierGuard) currentGID() int64 {
	return goID()
}

// reentrancyMessage builds the panic message for a detected nested acquisition.
// nested is the method the goroutine tried to re-enter ("View" or
// "ApplyAtomically"); held is the role it already holds ("View" or
// "ApplyAtomically").
func reentrancyMessage(nested, held string) string {
	return fmt.Sprintf(
		"lpg: Graph.%s is not re-entrant; this goroutine is already inside Graph.%s, "+
			"and a nested barrier acquisition from the same goroutine would deadlock the engine "+
			"(the visibility barrier is a non-re-entrant mvcc.Gate). Restructure the call so the inner work runs "+
			"outside the enclosing View/ApplyAtomically.",
		nested, held)
}

// checkWriter verifies that the calling goroutine does not already hold the
// barrier in any role, panicking on re-entry. It is called by
// [Graph.ApplyAtomically] BEFORE acquiring visMu.Lock, so the panic fires
// instead of the lock deadlocking. It does NOT mark the goroutine: the writer
// stamp is taken by [barrierGuard.stampWriter] only once visMu.Lock has been
// acquired, so a writer that merely QUEUES on visMu can never overwrite the
// active writer's identity (#1355). The window between this check and the
// stamp needs no registration: the goroutine executes no user code there, and
// while blocked on Lock it cannot call anything, so no same-goroutine nested
// acquisition can originate from it. The returned gid must be passed to
// stampWriter after Lock and to clearWriter (via defer) before Unlock.
func (bg *barrierGuard) checkWriter() int64 {
	gid := goID()
	if gid == 0 {
		// Runtime line unparseable: fail open (no enforcement), never crash.
		return 0
	}
	// writer → writer: this goroutine is already inside a write bracket.
	bg.writerMu.Lock()
	_, isWriter := bg.writers[gid]
	bg.writerMu.Unlock()
	if isWriter {
		panic(reentrancyMessage("ApplyAtomically", "ApplyAtomically"))
	}
	return gid
}

// stampWriter records gid as the goroutine holding visMu in write mode. It
// must be called by [Graph.ApplyAtomically] immediately AFTER visMu.Lock
// succeeds, so the stamp is exclusively owned by the lock holder for its whole
// tenure. gid==0 means checkWriter failed open and nothing is recorded.
func (bg *barrierGuard) stampWriter(gid int64) {
	if gid == 0 {
		return
	}
	bg.writerMu.Lock()
	bg.writers[gid] = struct{}{}
	bg.writerMu.Unlock()
}

// clearWriter clears the writer stamp set by stampWriter. gid==0 means
// checkWriter failed open and there is nothing to clear. It runs from a defer
// in [Graph.ApplyAtomically] registered AFTER the deferred visMu.Unlock, so it
// executes first on the unwind (LIFO) — the stamp is cleared while the lock is
// still held, even when fn panics, and therefore never strands a stale writer
// id. The CAS guarantees the call only ever clears this goroutine's OWN stamp,
// never another writer's (#1355); in correct code it always succeeds, because
// the stamp is exclusively owned by the lock holder between Lock and Unlock.
func (bg *barrierGuard) clearWriter(gid int64) {
	if gid == 0 {
		return
	}
	// Deletes only this goroutine's OWN entry, which is what the
	// compare-and-swap on the single field used to provide (#1355) and what a
	// set provides by construction, for any number of concurrent writers.
	bg.writerMu.Lock()
	delete(bg.writers, gid)
	bg.writerMu.Unlock()
}

// The READER half of this guard was removed with [Graph.View] in rmp #2344. It had
// exactly one caller, and once reads take no barrier at all there is no reader to
// mark: a snapshot read acquires nothing, so it cannot nest fatally with anything.
// What remains is the WRITER half, because [Graph.ApplyAtomically] still deadlocks
// when nested inside itself.
