package lpg

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// barrierGuard enforces that no single goroutine re-enters the
// transaction-visibility barrier ([Graph.View] / [Graph.ApplyAtomically]).
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
//   - Concurrent [Graph.View] readers are tracked in a small map keyed by
//     goroutine id, guarded by a dedicated mutex (NOT visMu). The mutex is held
//     only for the O(1) insert/remove at the RLock/RUnlock boundary, never while
//     fn runs, so the read hot path stays exactly as lock-free as before. The
//     map is pre-created in [New] and bounded by the number of concurrently
//     active readers, so steady-state churn reuses buckets and does not allocate.
//
// goroutine ids come from [goID]; if the runtime makes that unparseable the
// helper returns 0, the guard simply stops tripping, and the contract reverts to
// documented-but-unenforced. The guard never produces a false positive against
// legitimate concurrent (different-goroutine) View readers and an
// ApplyAtomically writer.
type barrierGuard struct {
	// writerGID is the goroutine id of the goroutine currently HOLDING visMu
	// in write mode, or 0 when no writer holds it. It is stamped by
	// [barrierGuard.stampWriter] only after visMu.Lock succeeds and cleared by
	// [barrierGuard.clearWriter] before visMu.Unlock, so it is exclusively
	// owned by the lock holder: a writer queued on visMu never appears here
	// and can never clobber the active writer's id (#1355). visMu serialises
	// writers, so at most one id is live at a time and a plain atomic
	// suffices.
	writerGID atomic.Int64

	// readerMu guards readers. It is independent of visMu and is held only for
	// the O(1) map mutation at the View entry/exit boundary.
	readerMu sync.Mutex
	// readers maps an in-View goroutine id to its current View nesting depth.
	// Depth is always 1 in correct code (the guard panics before it can reach
	// 2); the counter exists purely so the entry check can recognise "this
	// goroutine is already a reader" without the entry itself being mistaken
	// for a fresh reader. Pre-created in New so common-path inserts do not grow
	// a nil/empty map into existence.
	readers map[int64]int
}

// initBarrierGuard pre-creates the reader map so the common path never
// allocates the map into existence under the boundary mutex.
func (bg *barrierGuard) init() {
	bg.readers = make(map[int64]int)
}

// reentrancyMessage builds the panic message for a detected nested acquisition.
// nested is the method the goroutine tried to re-enter ("View" or
// "ApplyAtomically"); held is the role it already holds ("View" or
// "ApplyAtomically").
func reentrancyMessage(nested, held string) string {
	return fmt.Sprintf(
		"lpg: Graph.%s is not re-entrant; this goroutine is already inside Graph.%s, "+
			"and a nested barrier acquisition from the same goroutine would deadlock the engine "+
			"(visMu is a non-re-entrant sync.RWMutex). Restructure the call so the inner work runs "+
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
	// reader → writer: this goroutine is inside View and is now trying to take
	// the write lock — always a self-deadlock.
	bg.readerMu.Lock()
	_, isReader := bg.readers[gid]
	bg.readerMu.Unlock()
	if isReader {
		panic(reentrancyMessage("ApplyAtomically", "View"))
	}
	// writer → writer: this goroutine already holds the write lock.
	if bg.writerGID.Load() == gid {
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
	bg.writerGID.Store(gid)
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
	bg.writerGID.CompareAndSwap(gid, 0)
}

// enterReader marks the calling goroutine as an in-barrier reader, panicking if
// the goroutine already holds the barrier in any role. It is called by
// [Graph.View] BEFORE acquiring visMu.RLock. The returned gid must be passed to
// exitReader (via defer) to clear the mark even if fn panics.
func (bg *barrierGuard) enterReader() int64 {
	gid := goID()
	if gid == 0 {
		return 0
	}
	// writer → reader: this goroutine holds the write lock and is now trying to
	// read — always a self-deadlock. Checked with a lock-free atomic load.
	if bg.writerGID.Load() == gid {
		panic(reentrancyMessage("View", "ApplyAtomically"))
	}
	bg.readerMu.Lock()
	if bg.readers == nil {
		// Defensive: New always pre-creates the map, but a future Graph built by
		// another path must not nil-panic here. One-time, never on the common
		// path for a New-constructed graph.
		bg.readers = make(map[int64]int)
	}
	if _, isReader := bg.readers[gid]; isReader {
		// reader → reader: deadlocks the instant any writer queues behind the
		// outer RLock. Reject unconditionally rather than only when a writer is
		// pending, so the contract is enforced deterministically.
		bg.readerMu.Unlock()
		panic(reentrancyMessage("View", "View"))
	}
	bg.readers[gid] = 1
	bg.readerMu.Unlock()
	return gid
}

// exitReader clears the reader mark set by enterReader. gid==0 means enterReader
// failed open. It runs from a defer in [Graph.View], so it executes even when fn
// panics and never strands a stale reader id.
func (bg *barrierGuard) exitReader(gid int64) {
	if gid == 0 {
		return
	}
	bg.readerMu.Lock()
	delete(bg.readers, gid)
	bg.readerMu.Unlock()
}
