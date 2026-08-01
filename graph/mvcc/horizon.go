package mvcc

// horizon.go — the reclamation watermark (rmp #2282, MVCC P6 foundation).
//
// # What it is
//
// A version superseded at timestamp T is reachable only by a reader whose start
// timestamp is older than T. Once every active reader has started at or after
// T, nothing can reach that version and it may be freed. The oldest start
// timestamp among active readers is therefore the reclamation watermark, and
// both reference implementations are built on exactly this quantity:
// Memgraph's `commit_log_->OldestActive()`, and PostgreSQL's xmin horizon
// computed by `ComputeXidHorizons()` in procarray.c, which scans every active
// session and keeps the oldest.
//
// # Why it is not simply a registry behind a mutex
//
// A shared list of active readers would be written twice per read, and this
// project already measured what a single shared cache line costs a read path:
// rmp #2203 found that a bare sync.RWMutex degrades 17.6× from 1 to 10 cores
// purely because of its one shared reader counter, and that this — not the
// barrier code around it — is what stops reads scaling. Replacing a barrier
// whose contention is the problem with a registry that has the same problem
// would be self-defeating.
//
// So the slots are STRIPED and padded to a cache line each, and a reader
// touches exactly one of them. Two atomic stores per read, on a line it is
// usually alone on.
//
// # Choosing a slot
//
// The design note in docs/design-reader-indicator.md settled this question for
// the reader-indicator work and the same conclusion applies: Go has no stable
// per-CPU identity, and the alternatives it considered — a hashed goroutine id,
// linkname'd procPin, a pool lease — were rejected as needing either a
// runtime.Stack parse or unsafe linkage. What is left is a rotating counter,
// which is what [Horizon.Enter] uses as a STARTING POINT for a linear probe. It
// gives no locality guarantee, only spreading, and that is enough: correctness
// does not depend on which slot a reader takes, only on it taking one
// exclusively and clearing it.
//
// # What it deliberately does not do
//
// A stalled reader holds the watermark back, and versions accumulate behind it.
// That is not a defect to be engineered away — it is the same contract
// PostgreSQL has with a long-running transaction and VACUUM, and the same one
// Memgraph has. What the module owes is that the cost be OBSERVABLE, which is
// why [Horizon.Active] exists.

import (
	"sync/atomic"
)

// horizonSlots is the number of striped slots. A power of two so the rotating
// counter can mask rather than divide. Slots are EXCLUSIVE, so this is also the
// number of readers that can be registered at once; beyond it reclamation
// suspends rather than becoming unsound. See [Horizon.Enter].
const horizonSlots = 64

// cacheLine is the padding unit. 128 rather than 64 because Apple silicon
// prefetches pairs of lines, so 64-byte padding still lets two slots share a
// prefetch group.
const cacheLine = 128

// horizonSlot is one reader's announced start timestamp, alone on its cache
// line.
//
// A zero value means the slot is free. A start timestamp of zero would be
// indistinguishable from free, so [Horizon.Enter] stores startTS+1 and
// [Horizon.Oldest] subtracts it back; the clock starts at 1 for its first
// commit, but a reader may legitimately start at 0 before anything has been
// committed at all.
type horizonSlot struct {
	ts atomic.Uint64
	_  [cacheLine - 8]byte
}

// Horizon tracks the start timestamps of active readers so a reclaimer can
// compute the oldest version any of them can still reach.
//
// The zero value is ready to use. Safe for concurrent use.
type Horizon struct {
	slots [horizonSlots]horizonSlot
	next  atomic.Uint64
	_     [cacheLine - 8]byte
	// unreg counts active readers that found no free slot. While it is
	// non-zero the watermark is zero and nothing is reclaimed.
	unreg atomic.Int64
	_     [cacheLine - 8]byte
}

// unregistered is the slot value returned when no free slot was available. A
// reader holding it is invisible to the slot scan, so [Horizon.Oldest] reports
// a watermark of zero — reclaim NOTHING — for as long as any such reader is
// active.
const unregistered = -1

// Enter announces that a reader with the given start timestamp is active and
// returns the slot it occupies, which must be passed to [Horizon.Leave].
//
// It never blocks and never fails.
//
// # Why a slot is never shared
//
// The first version of this let two readers share a slot, keeping the older
// timestamp. It is UNSOUND, and the failure is silent data loss rather than a
// crash: readers A (start 10) and B (start 20) share a slot holding 10; A
// finishes and clears it; the watermark jumps to the clock; a reclaimer frees
// versions superseded at 15 that B can still reach. The slot is exclusive
// precisely so that "occupied" and "some specific reader is still here" are the
// same statement.
//
// When every slot is taken — more concurrent readers than [horizonSlots] — the
// reader is UNREGISTERED, and the watermark collapses to zero until it leaves,
// so nothing is reclaimed. That is the sound direction: reclamation stops, and
// correctness does not. It is also observable, via [Horizon.Unregistered].
func (h *Horizon) Enter(startTS uint64) int {
	start := int(h.next.Add(1) & (horizonSlots - 1))
	want := startTS + 1
	for i := 0; i < horizonSlots; i++ {
		slot := (start + i) & (horizonSlots - 1)
		if h.slots[slot].ts.CompareAndSwap(0, want) {
			return slot
		}
	}
	h.unreg.Add(1)
	return unregistered
}

// holdEverything is the slot value a reader takes BEFORE it knows its own start
// timestamp. It decodes to a start timestamp of zero, which [Horizon.Oldest]
// reports as a watermark of zero — reclaim nothing.
const holdEverything = 1

// EnterHolding claims a slot for a reader that has NOT yet read the clock, and
// returns it for [Horizon.Publish].
//
// # The race it closes
//
// The obvious sequence is: read the clock, then register. A reclaimer landing
// between the two computes its watermark from a clock that has already moved
// past the reader's start timestamp, and frees versions the reader is about to
// need. The window is nanoseconds and the failure is a wrong answer.
//
// Registering FIRST removes it. Between EnterHolding and Publish the slot holds
// back EVERYTHING, so a reclaimer in that window frees nothing at all; once the
// timestamp is published the ordinary rule applies. The cost is that a
// reclaimer racing a starting reader does no work — the right direction, and
// the window is a single clock read wide.
//
// The barrier used to make this unnecessary: a reader under visMu.RLock
// excluded every writer, and only writers reclaimed. Once reads stop taking the
// barrier, neither half of that is true.
func (h *Horizon) EnterHolding() int {
	start := int(h.next.Add(1) & (horizonSlots - 1))
	for i := 0; i < horizonSlots; i++ {
		slot := (start + i) & (horizonSlots - 1)
		if h.slots[slot].ts.CompareAndSwap(0, holdEverything) {
			return slot
		}
	}
	h.unreg.Add(1)
	return unregistered
}

// Publish announces the start timestamp of a reader that claimed its slot with
// [Horizon.EnterHolding]. It is a no-op for a reader that got no slot, which is
// already holding reclamation back through [Horizon.Unregistered].
func (h *Horizon) Publish(slot int, startTS uint64) {
	if slot == unregistered {
		return
	}
	h.slots[slot].ts.Store(startTS + 1)
}

// Leave releases the slot a reader took from [Horizon.Enter].
//
// Because a slot is exclusive, clearing it cannot release the watermark on
// another reader's behalf.
func (h *Horizon) Leave(slot int) {
	if slot == unregistered {
		h.unreg.Add(-1)
		return
	}
	h.slots[slot].ts.Store(0)
}

// Oldest returns the reclamation watermark: the oldest start timestamp any
// active reader announced, or fallback when none is active.
//
// It returns ZERO — reclaim nothing — while any reader failed to get a slot,
// because such a reader's start timestamp is unknown and assuming anything
// about it would be assuming in the unsafe direction.
//
// The caller supplies fallback, normally the clock's current value, because
// only the caller knows a timestamp that is newer than every version yet not
// newer than any reader that has begun. Nothing superseded AFTER the returned
// value may be reclaimed.
//
// A scan of 64 cache lines, no locks and no writes, so a reclaimer may call it
// as often as it likes without disturbing readers.
func (h *Horizon) Oldest(fallback uint64) uint64 {
	if h.unreg.Load() != 0 {
		return 0
	}
	oldest := fallback
	found := false
	for i := range h.slots {
		v := h.slots[i].ts.Load()
		if v == 0 {
			continue
		}
		ts := v - 1
		if !found || ts < oldest {
			oldest, found = ts, true
		}
	}
	if h.unreg.Load() != 0 {
		// A reader ran out of slots while the scan was in progress; its start
		// timestamp is not represented above, so the scan's result cannot be
		// trusted. Re-checked AFTER the scan as well as before it because the
		// window between the two is exactly where such a reader appears.
		return 0
	}
	return oldest
}

// Unregistered reports how many active readers failed to get a slot.
//
// Non-zero means reclamation is suspended. It is exported so that state is
// diagnosable rather than presenting as an unexplained memory growth.
func (h *Horizon) Unregistered() int64 { return h.unreg.Load() }

// Active reports how many slots currently hold a reader.
//
// It exists so the cost of a stalled reader is observable rather than merely
// suffered: versions accumulate behind the oldest active reader, and a
// deployment needs to be able to see that happening.
//
// Readers that found no slot are NOT counted here; [Horizon.Unregistered]
// reports those separately, because they have a different consequence — they
// suspend reclamation altogether rather than merely holding it back.
func (h *Horizon) Active() int {
	n := 0
	for i := range h.slots {
		if h.slots[i].ts.Load() != 0 {
			n++
		}
	}
	return n
}
