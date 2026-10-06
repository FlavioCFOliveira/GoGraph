package mvcc

// gate.go — the weak/strong exclusion gate that replaces a shared-counter
// RWMutex on the write path (rmp #2337).
//
// # The problem
//
// Two barriers in this module are acquired SHARED by every ordinary write and
// EXCLUSIVELY by DDL: the Cypher engine's schema lock and lpg's visibility
// barrier. Neither exists to exclude writers from one another — two writes
// holding either one shared do not conflict — yet Go's [sync.RWMutex]
// implements the shared acquisition as an atomic add on ONE readerCount word.
// So a write pays a coherence miss on a shared cache line purely to announce a
// NON-conflict, twice, and the cost grows with core count. rmp #2203 measured a
// bare sync.RWMutex degrading 17.6x from 1 to 10 cores for exactly this reason.
//
// MVCC cannot subsume these barriers: they guard the CATALOG, which is not
// versioned, so there is no snapshot a DDL could be made visible through. Both
// reference engines keep the same weak/strong split — Memgraph takes
// `main_lock_` with a `std::shared_lock` for an ordinary write and uniquely for
// index/constraint transitions (memgraph/memgraph, master, src/storage/v2/
// inmemory/storage.cpp), and PostgreSQL expresses it in its conflict matrix,
// where DML's RowExclusiveLock does not conflict with itself and CREATE INDEX's
// ShareLock does (src/backend/storage/lmgr/lock.c, LockConflicts). What is
// improvable is the IMPLEMENTATION, not the mechanism.
//
// # The shape, and the constraint it must satisfy
//
// PostgreSQL solved this with fast-path locking (src/backend/storage/lmgr/README,
// read at commit 0ec3f048bfc15c8eb9933e8228b847593389da1b, 2026-08-07). Its
// statement of the problem is this one almost verbatim — many short operations on
// one object turn a partition lock into a bottleneck, "measurable even on 2-core
// servers, and becomes very pronounced as core count increases" — and it states
// the constraint that makes the fix non-trivial:
//
//	"it must be possible to verify the absence of possibly conflicting locks
//	 without fighting over a shared LWLock or spinlock. Otherwise, this effort
//	 would simply move the contention bottleneck from one place to another."
//
// PostgreSQL keeps a per-backend fast-path array plus a partitioned array of
// strong-lock counters; a weak locker reads its partition's counter — normally
// zero, read-mostly, so the line stays SHARED in every core's cache and costs no
// coherence traffic — and records the lock locally. A strong locker bumps the
// counter and then drains every per-backend array.
//
// [Gate] is that structure with Go's constraints substituted. Go has no stable
// per-CPU identity (see the slot-choice note on [Horizon]), so "per-backend"
// becomes a striped array of padded counters chosen by a rotating index, the same
// compromise [Horizon] already makes and for the same reason.
//
// # Why the fast path is correct — Dekker, not luck
//
// The weak and strong sides are a store-then-load pair on opposite locations:
//
//	weak:   slot.Add(1)      ; then load strong
//	strong: strong.Add(1)    ; then load every slot
//
// Go's sync/atomic operations are sequentially consistent, so the four accesses
// share one total order. If the weak side's load of `strong` is ordered before the
// strong side's store, then the weak side's increment is ordered before the strong
// side's loads, so the strong side SEES it and waits. Otherwise the weak side sees
// the strong flag and backs out. At least one of the two always happens, which is
// exactly Dekker's argument, and it is why neither side may use a relaxed load.
//
// # Why the drain terminates
//
// Once `strong` is non-zero, any weak acquirer whose slot increment is ordered
// after that store must, by the total order, see a non-zero `strong` in its own
// subsequent load and back out. So the set of goroutines that can still hold a
// fast-path slot is limited to those already in flight when the flag was raised —
// a finite set that cannot be replenished. A writer looping on WeakLock/WeakUnlock
// therefore cannot starve a DDL.
//
// # What it deliberately does NOT do
//
// It is not a general RWMutex and must not be used as one. It provides no
// upgrade, no re-entrancy, and no fairness between weak acquirers, because none
// of those is needed by the DDL-exclusion job it exists for. Strong acquirers are
// serialised against one another in arrival order by a small mutex-guarded queue,
// which is correct because DDL is rare and its cost is irrelevant next to the scan
// it performs anyway.
//
// # Every wait is cancellable, and a withdrawn request leaves no trace (rmp #2983)
//
// Every blocking wait — a strong acquirer queued behind another, a strong acquirer
// draining weak holders, a weak acquirer parked behind a strong one — selects on a
// broadcast channel and on the caller's ctx. A strong acquirer that gives up
// withdraws its request in place: it leaves the queue, or lowers the strong flag
// and wakes every parked weak acquirer, under the same mutex that admits them. No
// goroutine is left behind to finish an acquisition the caller abandoned.
//
// The previous shape could not do that. It built the strong side from a
// sync.Mutex, a sync.RWMutex and the flag, none cancellable, so a Ctx acquire ran
// the whole of StrongLock on a helper goroutine and only the CALLER stopped
// waiting. The helper kept the flag raised and the RWMutex write-pending until the
// in-flight weak holder left, so every new weak acquirer blocked behind a strong
// request whose caller had already returned its deadline error.

import (
	"context"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// gateSlots is the number of striped weak-holder counters. A power of two so the
// rotating index can mask rather than divide.
//
// 64 matches the shard count the label and property maps already use, and the
// cost is bounded and small: one counter per cache line, so 64*128 = 8 KiB per
// Gate. Unlike [Horizon]'s slots these are COUNTERS rather than exclusive
// reservations, so the capacity does not bound how many writers may hold the gate
// — collisions cost sharing, never correctness or admission.
const gateSlots = 64

// gateSlot is one striped weak-holder counter, alone on its cache line.
//
// The padding is what the whole design is for: without it the 64 counters share
// lines and the structure reproduces the single-shared-word cost it replaces.
type gateSlot struct {
	n atomic.Int32
	_ [cacheLine - 4]byte
}

// gateSlow is the slot value returned to a weak acquirer that took the blocking
// path because a strong holder was present.
const gateSlow = -1

// Gate is a weak/strong exclusion gate: any number of WEAK holders may proceed
// together, a STRONG holder excludes every weak holder and every other strong
// holder, and an uncontended weak acquisition touches only one striped cache line
// plus a read-mostly flag.
//
// The zero value is ready to use. Safe for concurrent use.
//
// It is NOT re-entrant in either mode.
type Gate struct {
	slots [gateSlots]gateSlot

	// THERE IS NO ROTATING SLOT COUNTER HERE, AND THAT IS THE WHOLE POINT.
	//
	// The first version of this type chose its slot with `next.Add(1)` on a shared
	// atomic, the way [Horizon.claim] does. Measured on an Apple M4, 10 cores, that
	// design cost 2.98 ns/op at GOMAXPROCS=1 but 30.9 ns at 2 and 30.5 ns at 4 —
	// against sync.RWMutex's 3.71/8.8/16.9 — i.e. it was three times WORSE than the
	// lock it replaces at 2 cores, because every acquisition wrote one shared word
	// and that word, not the RWMutex, became the bottleneck. PostgreSQL's README
	// names this exact failure: an approach that cannot verify the absence of a
	// strong lock without fighting over a shared word "would simply move the
	// contention bottleneck from one place to another". It did.
	//
	// The slot therefore comes from the CALLER, which already holds a stable,
	// well-distributed per-transaction value and needs no shared state to produce
	// one. See [Gate.WeakLock].

	// strong is non-zero while a strong holder is present or arriving. Weak
	// acquirers only ever LOAD it, so in steady state the line stays shared in
	// every core's cache and costs no coherence traffic — which is the property
	// PostgreSQL's README requires and the reason this is not simply another
	// shared counter.
	strong atomic.Int32
	_      [cacheLine - 4]byte

	// mu guards the cold-path state below. Only strong acquirers and weak acquirers
	// that found the strong flag raised ever take it.
	mu sync.Mutex
	// owned is true from the moment a strong acquirer raises the flag until it
	// releases or withdraws. It is the condition a slow-path weak acquirer waits
	// on, and it is set and cleared together with the flag, under mu.
	owned bool
	// slowHolders counts weak holders admitted on the slow path. It can grow only
	// while owned is false, so once a strong acquirer has raised the flag the set
	// it must drain is finite and cannot be replenished.
	slowHolders int
	// weakParked counts weak acquirers waiting for owned to clear. The head of the
	// strong queue does not take the gate while it is non-zero, so weak acquirers
	// parked behind one strong tenure are admitted before the next one begins and
	// a run of strong acquirers cannot starve them. It can grow only while owned is
	// set, so after a release it only shrinks and the wait is bounded.
	weakParked int
	// queue holds the tickets of strong acquirers waiting for owned to clear, in
	// arrival order. The head is the next to proceed; a withdrawn ticket is removed.
	queue []uint64
	// nextTicket mints the tickets in queue.
	nextTicket uint64
	// wake is closed, and cleared, on every change of the state above that a
	// waiter may be waiting for. A waiter creates it on demand, so the zero value
	// of Gate needs no constructor.
	wake chan struct{}
}

// waitChLocked returns the channel the next state change will close. The caller
// holds g.mu.
func (g *Gate) waitChLocked() chan struct{} {
	if g.wake == nil {
		g.wake = make(chan struct{})
	}
	return g.wake
}

// broadcastLocked wakes every waiter so each re-checks its condition. The caller
// holds g.mu.
func (g *Gate) broadcastLocked() {
	if g.wake != nil {
		close(g.wake)
		g.wake = nil
	}
}

// waitLocked releases g.mu, waits for the next state change or for done, and
// re-acquires g.mu in both cases. It reports false when done fired. A nil done
// never fires, which is how the context-free methods use the same code.
//
// When both are ready the select may report either; a caller that is told false
// treats it as cancellation even though its condition may now hold, which is
// correct because it then holds nothing and withdraws.
func (g *Gate) waitLocked(done <-chan struct{}) bool {
	w := g.waitChLocked()
	g.mu.Unlock()
	select {
	case <-w:
		g.mu.Lock()
		return true
	case <-done:
		g.mu.Lock()
		return false
	}
}

// WeakLock acquires the gate in weak mode and returns the token that must be
// passed to [Gate.WeakUnlock].
//
// It blocks only when a strong holder is present or arriving.
//
// hint selects the striped slot. It must be cheap for the caller to produce WITHOUT
// touching shared state — that requirement is the whole design, for the measured
// reason recorded on the [Gate] struct — and it should be well spread across
// concurrent callers. A transaction id is the intended source: [Clock.NextTxID]
// mints them sequentially, so concurrent transactions land on distinct slots, and
// the caller already has one in hand. Correctness does not depend on hint at all:
// two callers sharing a slot merely share a cache line, and any value is safe.
func (g *Gate) WeakLock(hint uint64) int {
	slot := int(hint & (gateSlots - 1))
	g.slots[slot].n.Add(1)
	// Sequentially consistent load, paired with the strong side's store. See the
	// Dekker argument in the file header: this must not be relaxed.
	if g.strong.Load() == 0 {
		return slot
	}
	// A strong holder is arriving or present. Give up the fast-path claim before
	// blocking, or the drain below would wait on a goroutine that is itself
	// waiting for the drain to finish.
	g.slots[slot].n.Add(-1)
	g.weakSlow(nil)
	return gateSlow
}

// weakSlow admits a weak acquirer on the slow path once no strong acquirer owns
// the gate, or reports false, holding nothing, when done fires first.
//
// A weak acquirer comes here only after its fast-path claim found the flag
// raised and was withdrawn. It is admitted as soon as owned is false, even when
// strong acquirers are queued: a queued strong acquirer has not raised the flag,
// so the fast path would admit the same caller anyway.
func (g *Gate) weakSlow(done <-chan struct{}) bool {
	g.mu.Lock()
	for g.owned {
		g.weakParked++
		ok := g.waitLocked(done)
		g.weakParked--
		if g.weakParked == 0 {
			// The head of the strong queue may be waiting for exactly this.
			g.broadcastLocked()
		}
		if !ok {
			g.mu.Unlock()
			return false
		}
	}
	g.slowHolders++
	g.mu.Unlock()
	return true
}

// WeakLockAuto is [Gate.WeakLock] for a caller that has no natural hint in hand.
//
// It draws the stripe from [math/rand/v2.Uint64], whose generator is per-P inside
// the runtime and therefore touches NO shared cache line — which is the entire
// requirement. An ordinary shared counter would reintroduce the bottleneck this
// type exists to remove, as the struct comment records having measured.
//
// Prefer [Gate.WeakLock] with a real per-transaction value where one exists: it is
// marginally cheaper and gives a caller's repeated acquisitions stripe affinity.
func (g *Gate) WeakLockAuto() int {
	// #nosec G404 -- this picks a cache-line stripe, not a secret. Unpredictability
	// is irrelevant here and correctness does not depend on the value at all: a
	// collision costs two callers a shared line and nothing else. crypto/rand would
	// be orders of magnitude slower on a path whose entire purpose is to be cheap.
	return g.WeakLock(rand.Uint64())
}

// WeakLockCtxAuto is [Gate.WeakLockCtx] for a caller with no natural hint, drawing
// the stripe the same way [Gate.WeakLockAuto] does.
//
// Use this rather than passing a constant. A constant hint sends every caller to the
// SAME stripe, which reinstates the single shared cache line this type exists to
// remove — and on the autocommit write path that is the hottest line in the engine.
func (g *Gate) WeakLockCtxAuto(ctx context.Context) (int, error) {
	return g.WeakLockCtx(ctx, rand.Uint64()) // #nosec G404 -- stripe choice, not a secret
}

// TryWeakLock attempts a weak acquisition without ever blocking. It reports false
// when a strong holder is present or arriving, in which case nothing is held.
//
// It exists so a caller can bound its wait with a context; see [Gate.WeakLockCtx].
func (g *Gate) TryWeakLock(hint uint64) (int, bool) {
	slot := int(hint & (gateSlots - 1))
	g.slots[slot].n.Add(1)
	if g.strong.Load() == 0 {
		return slot, true
	}
	g.slots[slot].n.Add(-1)
	return 0, false
}

// WeakLockCtx is [Gate.WeakLock] with the wait bounded by ctx. It returns ctx's
// error, holding nothing, when ctx finishes before the acquisition succeeds.
//
// # Why a weak acquirer needs a deadline at all
//
// Weak acquirers do not wait for each other, so the only thing that can block one
// is a DDL. That wait is legitimate but unbounded — a DDL runs a full backfill scan
// — and a caller carrying a deadline is entitled to hear about it rather than be
// held past it. Losing that bound is not hypothetical: before rmp #2306 an
// autocommit write carrying a 200 ms deadline blocked for TEN MINUTES behind an open
// transaction and returned only when the harness killed it.
//
// The fast path is unchanged and costs nothing extra: ctx is consulted only once the
// try has already failed, so an uncontended acquisition never touches it.
func (g *Gate) WeakLockCtx(ctx context.Context, hint uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if slot, ok := g.TryWeakLock(hint); ok {
		return slot, nil
	}
	// Blocked behind a strong holder. Wait on the slow path with ctx in the same
	// select, so abandoning the wait abandons the acquisition: nothing is left
	// queued and nothing is held.
	if !g.weakSlow(ctx.Done()) {
		return 0, ctx.Err()
	}
	// Admitted. Re-check ctx so a deadline that elapsed while parked is reported
	// rather than handing back a hold the caller may no longer use; nothing has
	// run under the hold, so releasing it here is correct.
	if err := ctx.Err(); err != nil {
		g.WeakUnlock(gateSlow)
		return 0, err
	}
	return gateSlow, nil
}

// WeakUnlock releases a weak acquisition made with [Gate.WeakLock].
//
// It panics when the token names a slow-path hold and none is outstanding: that
// is a double release, a programmer error the gate cannot repair.
func (g *Gate) WeakUnlock(slot int) {
	if slot == gateSlow {
		g.mu.Lock()
		if g.slowHolders == 0 {
			g.mu.Unlock()
			panic("mvcc: Gate.WeakUnlock of a slow-path hold that is not held")
		}
		g.slowHolders--
		if g.slowHolders == 0 {
			// Only a draining strong acquirer waits for this count.
			g.broadcastLocked()
		}
		g.mu.Unlock()
		return
	}
	g.slots[slot].n.Add(-1)
}

// StrongLock acquires the gate exclusively, excluding every weak holder and every
// other strong holder. It returns once no weak holder remains.
func (g *Gate) StrongLock() {
	g.strongLock(nil)
}

// strongLock is the shared body of [Gate.StrongLock] and [Gate.StrongLockCtx]. It
// reports false, holding nothing and leaving no request behind, when done fires
// before the acquisition completes; a nil done never fires.
//
// It has three phases, each of which may be withdrawn from:
//
//  1. queue behind earlier strong acquirers, in arrival order, and behind the weak
//     acquirers parked on the previous strong tenure;
//  2. raise the flag and set owned, so no new weak holder is admitted on either
//     path; then drain the fast-path slots;
//  3. wait for the slow-path holders admitted before phase 2 to leave.
//
// Withdrawing in phase 1 removes the ticket. Withdrawing in phases 2 or 3 is
// exactly [Gate.StrongUnlock]: lower the flag, clear owned, wake every waiter.
func (g *Gate) strongLock(done <-chan struct{}) bool {
	g.mu.Lock()
	t := g.nextTicket
	g.nextTicket++
	g.queue = append(g.queue, t)
	for g.owned || g.queue[0] != t || g.weakParked != 0 {
		if g.waitLocked(done) {
			continue
		}
		i := slices.Index(g.queue, t)
		g.queue = slices.Delete(g.queue, i, i+1)
		// The ticket behind this one may now be the head with the gate free; it is
		// waiting for a state change, so make one.
		g.broadcastLocked()
		g.mu.Unlock()
		return false
	}
	g.queue = slices.Delete(g.queue, 0, 1)
	g.owned = true
	// Raise the flag BEFORE draining, so no new fast-path holder can appear after
	// the drain has passed its slot. Sequentially consistent store, paired with the
	// weak side's load: see the Dekker argument in the file header.
	g.strong.Store(1)
	g.mu.Unlock()

	// Drain the fast path. Only acquirers already in flight when the flag rose can
	// still hold a slot, and each either leaves or backs out to the slow path,
	// where owned now refuses it.
	for i := range g.slots {
		for g.slots[i].n.Load() != 0 {
			select {
			case <-done:
				g.releaseStrong()
				return false
			default:
			}
			runtime.Gosched()
		}
	}

	// Drain the slow path. slowHolders cannot grow while owned is set.
	g.mu.Lock()
	for g.slowHolders != 0 {
		if !g.waitLocked(done) {
			g.mu.Unlock()
			g.releaseStrong()
			return false
		}
	}
	g.mu.Unlock()
	return true
}

// releaseStrong ends a strong tenure or withdraws a strong request that has raised
// the flag. It panics when no strong acquirer owns the gate: a double release is a
// programmer error the gate cannot repair.
func (g *Gate) releaseStrong() {
	g.mu.Lock()
	if !g.owned {
		g.mu.Unlock()
		panic("mvcc: Gate.StrongUnlock of a gate that is not strongly held")
	}
	g.owned = false
	g.strong.Store(0)
	// Wakes the parked weak acquirers and the head of the strong queue together.
	// The head waits for weakParked to reach zero, so the parked weak acquirers
	// are admitted first — the order the sync.RWMutex this replaced gave readers
	// queued behind a writer.
	g.broadcastLocked()
	g.mu.Unlock()
}

// StrongLockCtx is [Gate.StrongLock] with the wait bounded by ctx. It returns ctx's
// error, holding nothing, when ctx finishes before the acquisition completes.
//
// A strong acquirer waits for two things — other strong acquirers, and the drain of
// every weak holder — and both are unbounded in principle, so a caller with a
// deadline needs this for the same reason [Gate.WeakLockCtx] exists.
//
// Giving up WITHDRAWS the request rather than leaving it queued (rmp #2983): the
// flag is lowered and every parked weak acquirer is woken before this returns, so
// once the caller has its error the gate behaves as if the request had never been
// made. No goroutine outlives the call.
func (g *Gate) StrongLockCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !g.strongLock(ctx.Done()) {
		return ctx.Err()
	}
	// Re-check ctx so a deadline that elapsed while queued is reported rather than
	// handing back a hold the caller may no longer use. Nothing has run under the
	// hold, so releasing it here is correct.
	if err := ctx.Err(); err != nil {
		g.releaseStrong()
		return err
	}
	return nil
}

// StrongUnlock releases an acquisition made with [Gate.StrongLock] or a
// successful [Gate.StrongLockCtx]. It panics when the gate is not strongly held.
func (g *Gate) StrongUnlock() {
	g.releaseStrong()
}

// WeakHolders reports how many fast-path slot claims are outstanding.
//
// It exists so the gate's occupancy is observable rather than merely suffered,
// matching the observability mandate every other bounded structure here follows.
//
// IT IS A GAUGE, NOT AN EXACT COUNT OF CRITICAL-SECTION OCCUPANCY, and must not be
// used as an exclusion oracle. [Gate.WeakLock] claims its slot BEFORE it learns
// whether a strong holder is present, so a claim counted here may belong to an
// acquirer that is about to back out and block — one that never enters its critical
// section at all. The count is therefore an upper bound. It also excludes holders
// parked on the blocking path, which are not on the fast path by definition.
func (g *Gate) WeakHolders() int {
	n := 0
	for i := range g.slots {
		if v := g.slots[i].n.Load(); v > 0 {
			n += int(v)
		}
	}
	return n
}
