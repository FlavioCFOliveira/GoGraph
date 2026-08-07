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
// serialised against one another by an ordinary mutex, which is correct because
// DDL is rare and its cost is irrelevant next to the scan it performs anyway.

import (
	"context"
	"math/rand/v2"
	"runtime"
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

	// strongMu serialises strong acquirers against one another.
	strongMu sync.Mutex
	// blocked parks weak acquirers that arrive while a strong holder is present,
	// so they wait instead of spinning. A strong holder takes it exclusively for
	// its whole tenure.
	blocked sync.RWMutex
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
	g.blocked.RLock()
	return gateSlow
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
	// Blocked behind a strong holder. Park on the blocking path from a helper
	// goroutine so the caller can abandon the WAIT on ctx — the acquisition itself
	// cannot be abandoned, because sync.RWMutex has no cancellable acquire, so a
	// hold that lands after the caller gave up must still be released.
	if !acquireCtx(ctx, g.blocked.RLock, g.blocked.RUnlock) {
		return 0, ctx.Err()
	}
	return gateSlow, nil
}

// WeakUnlock releases a weak acquisition made with [Gate.WeakLock].
func (g *Gate) WeakUnlock(slot int) {
	if slot == gateSlow {
		g.blocked.RUnlock()
		return
	}
	g.slots[slot].n.Add(-1)
}

// StrongLock acquires the gate exclusively, excluding every weak holder and every
// other strong holder. It returns once no weak holder remains.
func (g *Gate) StrongLock() {
	g.strongMu.Lock()
	// Raise the flag BEFORE draining, so no new fast-path holder can appear after
	// the drain has passed its slot.
	g.strong.Add(1)
	// Shut the blocking path too, and wait for any weak acquirer already parked on
	// it. Taken after the flag is raised so an acquirer that backs out of the fast
	// path always finds this held rather than slipping between the two.
	g.blocked.Lock()
	for i := range g.slots {
		for g.slots[i].n.Load() != 0 {
			runtime.Gosched()
		}
	}
}

// StrongLockCtx is [Gate.StrongLock] with the wait bounded by ctx. It returns ctx's
// error, holding nothing, when ctx finishes before the acquisition completes.
//
// A strong acquirer waits for two things — other strong acquirers, and the drain of
// every weak holder — and both are unbounded in principle, so a caller with a
// deadline needs this for the same reason [Gate.WeakLockCtx] exists.
//
// As there, the acquisition itself cannot be abandoned once started: the underlying
// mutexes have no cancellable acquire, so a hold that lands after the caller gave up
// must still be released, which the helper below does.
func (g *Gate) StrongLockCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !acquireCtx(ctx, g.StrongLock, g.StrongUnlock) {
		return ctx.Err()
	}
	return nil
}

// acquireCtx runs a non-cancellable acquire on a helper goroutine and lets the
// CALLER stop waiting when ctx finishes. It reports whether the caller now owns the
// lock; when it reports false the caller owns nothing and ctx.Err() is the reason.
//
// # ONE helper per abandoned acquire, never two (rmp #2260, re-established by #2348)
//
// The obvious shape — a helper that acquires and closes a channel, plus a SECOND
// goroutine spawned on the ctx.Done branch whose only job is to wait for the first
// and unlock — costs two goroutines for every abandoned attempt. Both Ctx methods on
// this gate had that shape until rmp #2348. The retired internal/ctxlock package had
// already established the fix and measured what the transient costs; its argument
// lives here now, which is the whole reason this helper exists rather than the inline
// form it replaced.
//
// The three-way handoff is load-bearing and a plain boolean RACES: the helper can
// read "the caller gave up" as false at the same instant the caller takes the
// ctx.Done branch, leaving the lock held with no logical owner and nobody to release
// it. With a CAS exactly one side wins and each side's losing branch knows that
// cleaning up is its job.
//
// # What is bounded here, and what is not
//
// The live helper count tracks ARRIVAL RATE × HOLDER TENURE, not the number of
// concurrent callers: each abandoned attempt parks one helper until the holder
// releases. Measured (in ctxlock, against a barrier held for 3 s by acquirers with a
// 2 ms deadline) the two-goroutine form reached 597 819 live goroutines and 1 677 MiB
// from 256 callers. Halving the per-attempt cost does not change that asymptote, and
// removing the transient altogether would mean refusing an acquire past some
// admission limit — turning a blocking call into a failing one and changing every
// caller's contract. That is a deliberate omission recorded here, not an oversight.
func acquireCtx(ctx context.Context, lock, unlock func()) bool {
	const (
		stateWaiting   int32 = 0 // neither side has claimed the acquisition yet
		stateHandedOff int32 = 1 // the helper published it; the caller owns the lock
		stateAbandoned int32 = 2 // the caller gave up first; the helper must unlock
	)
	var state atomic.Int32
	acquired := make(chan struct{})
	go func() {
		lock()
		if state.CompareAndSwap(stateWaiting, stateHandedOff) {
			close(acquired) // the caller is still waiting: hand it the lock
			return
		}
		unlock() // the caller abandoned first; nothing ran under the lock
	}()

	select {
	case <-acquired:
		// Held. Re-check ctx so a deadline that elapsed WHILE QUEUED is reported
		// rather than handing back a lock the caller may no longer use. Both Ctx
		// methods here omitted this until rmp #2348.
		//
		// Its window is ONE SCHEDULING QUANTUM and it is stated that way rather than
		// inflated: this arm is reachable with an expired ctx only when the helper's
		// acquisition and the deadline become ready at the same instant, and the
		// elapsed time is then still within budget. It is not the rmp #2174 defect —
		// that one is being held for the HOLDER'S REMAINING TENURE, and what prevents
		// it is abandoning the wait, not this check. Correct and free, so it stays;
		// no test claims to cover it, because it is not observable from outside.
		//
		// Releasing here is correct: nothing has been done under the lock.
		if ctx.Err() != nil {
			unlock()
			return false
		}
		return true
	case <-ctx.Done():
		if state.CompareAndSwap(stateWaiting, stateAbandoned) {
			// Won the race: the helper has not published and will unlock once it
			// acquires. No second goroutine is needed.
			return false
		}
		// The helper published between ctx firing and this CAS, so the lock IS held
		// and we own it. acquired is already closed, so this cannot block.
		<-acquired
		unlock()
		return false
	}
}

// StrongUnlock releases an acquisition made with [Gate.StrongLock].
func (g *Gate) StrongUnlock() {
	g.strong.Add(-1)
	g.blocked.Unlock()
	g.strongMu.Unlock()
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
