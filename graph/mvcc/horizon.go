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
	"math/bits"
	"sync/atomic"
)

// horizonSlots is the number of striped slots. A power of two so the rotating
// counter can mask rather than divide. Slots are EXCLUSIVE, so this is also the
// number of readers that can be registered at once; beyond it reclamation
// suspends rather than becoming unsound. See [Horizon.Enter].
//
// # Why 1024, from measurement (rmp #2315)
//
// It was 64, and 64 was chosen when a slot was held for the duration of ONE
// STATEMENT. rmp #2307 gave an explicit read transaction a snapshot pinned for its
// whole lifetime, so a slot is now held for a whole transaction INCLUDING WHILE IT
// SITS IDLE. The exhaustion cliff was measured at exactly 65 concurrent read
// transactions, and past it reclamation does not slow down — it SUSPENDS, which by
// this file's own account is the one state in which version memory has no bound.
//
// The number comes from docs/benchmarks/mvcc-horizon-sizing-2026-08-05.md, which
// measures 64/256/1024/4096 in both the near-empty and near-full regimes. Three
// quantities move differently, so no single "bigger is worse" argument holds:
//
//   - [Horizon.Oldest] WAS O(slots) always, and the sizing decision was taken on the
//     belief that it is not on the query path. THAT BELIEF WAS WRONG (rmp #2292):
//     [Graph.EndRead] calls it on every read whenever versions exist, and it cost a
//     read 200 ns — 29% of a measured 13.44% read regression under a writer. It is now
//     O(ACTIVE READERS) via the occupancy summary; see [Horizon.Oldest];
//   - [Horizon.Enter] near-empty — the per-read-transaction cost, and the only one
//     on a hot path — is FLAT, because the rotating start index probes one slot
//     whatever the capacity: 2.135ns at 64 against 2.186ns at 1024, +2.4%;
//   - [Horizon.Enter] near-full is O(slots), and that is the honest price of the
//     capacity: a system running AT capacity pays the probe.
//
// 4096 was rejected on the measurement, not on an argument about cache sizes: it is
// where Oldest turns super-linear (5.3x for 4x slots, against 4.1x below it) and it
// costs 512 KiB per Graph. 256 was rejected because this module's own load-test grid
// goes to 1024 goroutines, so it would leave the cliff reachable in normal operation.
//
// The capacity survives rmp #2292 unchanged: the defect there was the SCAN, not the
// size, and once the scan is O(active readers) the capacity no longer prices it.
//
// The cost is 128 KiB per [Horizon], one heap allocation per Graph.
// TestHorizon_ExhaustionCliffAtCapacity pins the capacity so it cannot regress
// silently.
const horizonSlots = 1024

// horizonWords is the number of 64-bit occupancy words that summarise the slots.
const horizonWords = horizonSlots / 64

// HorizonCapacity is the number of readers that can be registered at once, exported
// so an operator can compare it against a live reader count without reading the
// source. Past it, reclamation SUSPENDS: see [Horizon.Enter] and the rationale on
// horizonSlots.
const HorizonCapacity = horizonSlots

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

// horizonOcc is one 64-slot occupancy word, alone on its cache line.
//
// # Why an occupancy summary exists at all
//
// [Horizon.Oldest] used to scan every slot unconditionally, which is 1024 distinct
// cache lines — 128 KiB — for a graph with one active reader. That was acceptable
// only while the scan was off the query path, and rmp #2292 measured that it is not:
// [Graph.EndRead] calls it on every read once versions exist, at 200 ns a read.
//
// The summary makes the scan proportional to the number of ACTIVE readers instead of
// to the capacity, which is the same shape PostgreSQL uses: `ComputeXidHorizons`
// walks `procArray->pgprocnos`, a dense list of live backends, not the whole
// `MaxBackends` array. PostgreSQL maintains that list under `ProcArrayLock`; a bitmap
// is the lock-free analogue, and the reason it is a bitmap rather than a dense list
// is that a dense list cannot be compacted without a lock.
//
// Padding per word matters as much as padding per slot: 16 words unpadded would share
// one cache line and turn every registration into a coherence miss for every other,
// which is precisely the shared-counter cost the file header rejects.
type horizonOcc struct {
	bits atomic.Uint64
	_    [cacheLine - 8]byte
}

// Horizon tracks the start timestamps of active readers so a reclaimer can
// compute the oldest version any of them can still reach.
//
// The zero value is ready to use. Safe for concurrent use.
type Horizon struct {
	slots [horizonSlots]horizonSlot
	// occ marks which slots are occupied, so [Horizon.Oldest] visits only those.
	//
	// THE BIT IS THE OCCUPANCY, NOT THE TIMESTAMP. A slot's ts is meaningful only
	// while its bit is set, which is what lets [Horizon.Leave] be a single bit clear
	// and lets the scan skip a free slot without touching its cache line at all.
	occ  [horizonWords]horizonOcc
	next atomic.Uint64
	_    [cacheLine - 8]byte
	// unreg counts active readers that found no free slot. While it is
	// non-zero the watermark is zero and nothing is reclaimed.
	unreg atomic.Int64
	_     [cacheLine - 8]byte
	// staleLeave counts releases of a slot whose occupancy bit was ALREADY CLEAR.
	//
	// It is the detector for the one corruption this structure cannot survive, and
	// it costs a branch on a value [Horizon.Leave] already has in hand. A release
	// that clears a bit nobody held means either a slot was returned twice or a
	// slot number was invented — and the damage is not to the counter, it is that
	// SOME OTHER READER'S bit is the one that gets cleared next time round: that
	// reader becomes invisible to [Horizon.Oldest], the watermark advances past its
	// start instant, and versions it can still reach are freed underneath it. The
	// symptom is a snapshot silently reading a value from after its own instant,
	// which is an Isolation violation with nothing anywhere reporting it.
	//
	// Zero is the only correct value. See [Horizon.StaleLeaves].
	staleLeave atomic.Int64
	_          [cacheLine - 8]byte
}

// claim reserves a slot exclusively and returns it, or [unregistered] when every
// slot is taken.
//
// THE BIT IS CLAIMED BEFORE THE TIMESTAMP IS STORED, and that order is load-bearing
// in the only direction that matters. The reverse — stamp the slot, then advertise it
// — would let [Horizon.Oldest] run between the two and compute a watermark that does
// not account for this reader at all, freeing versions it is about to need. Claiming
// first can only make the scan see a slot whose timestamp is not yet stored, which
// reads as zero — [Horizon.Leave] invalidated it — and [Horizon.Oldest] answers zero
// by holding everything back.
func (h *Horizon) claim() int {
	// Rotate over WORDS rather than over slots: the word's cache line is the unit
	// two registering readers can contend on, so spreading across words is what
	// spreading is for. Within a word the lowest free bit is taken, which costs
	// nothing extra because each slot has its own line regardless.
	start := int(h.next.Add(1) & (horizonWords - 1))
	for i := 0; i < horizonWords; i++ {
		w := (start + i) & (horizonWords - 1)
		for {
			cur := h.occ[w].bits.Load()
			if cur == ^uint64(0) {
				break // full; try the next word
			}
			b := bits.TrailingZeros64(^cur)
			if h.occ[w].bits.CompareAndSwap(cur, cur|1<<uint(b)) {
				return w<<6 + b
			}
		}
	}
	h.unreg.Add(1)
	return unregistered
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
//
// # A slot between its claim and its stamp carries NO timestamp
//
// [Horizon.Leave] invalidates the timestamp before it clears the occupancy bit, so a
// slot being re-claimed reads as zero — "claimed, instant not yet known" — until this
// method stores its new occupant's instant. [Horizon.Oldest] answers zero by holding
// everything back, so the window is conservative.
//
// It used to hold the PREVIOUS occupant's instant instead, which is also conservative
// — an older instant holds back more, given the clock is monotonic — but which makes
// the watermark UNDER-REPORT rather than suspend, and an under-reporting watermark
// cannot be told apart from one that has passed a live reader. See [Horizon.Leave] for
// the measurement that decided it and for what the invariant buys.
func (h *Horizon) Enter(startTS uint64) int {
	slot := h.claim()
	if slot == unregistered {
		return unregistered
	}
	h.slots[slot].ts.Store(startTS + 1)
	return slot
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
	slot := h.claim()
	if slot == unregistered {
		return unregistered
	}
	h.slots[slot].ts.Store(holdEverything)
	return slot
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
//
// It INVALIDATES THE TIMESTAMP and then clears the OCCUPANCY BIT, in that order.
//
// It also DETECTS the release of a slot that was not held: the atomic And returns
// the word as it stood, so testing the bit costs nothing beyond a branch. See
// [Horizon.StaleLeaves] for why that particular corruption is the one worth a
// permanent guard.
//
// # Why the timestamp is invalidated rather than left behind
//
// This method used to leave the timestamp behind, so a slot between its claim and
// its [Horizon.Publish] read as its PREVIOUS occupant's instant. That is safe —
// an older instant holds back more than the arriving reader needs — but it makes
// the watermark UNDER-REPORT for that window, and an under-reporting watermark is
// indistinguishable from the one corruption that matters. Measured: with the
// residue in place the watermark was seen to move BACKWARDS 1 734 to 4 165 times
// per 30-second run of TestIsolation_ApplyAtomically_View_NoPartialReads, all of
// it benign, which left no way to assert the invariant that a live reader is never
// passed (rmp #2420).
//
// Zeroing it instead makes an occupied slot's timestamp exactly one of two things:
// zero, meaning "claimed, instant not yet known", which [Horizon.Oldest] answers
// by holding EVERYTHING back; or this occupant's own published instant. Never a
// third reader's stale one. The watermark is then monotone, and
// [Horizon.StaleLeaves] and its lpg counterpart become assertable rather than
// merely informative.
//
// The order is load-bearing: the timestamp is invalidated BEFORE the occupancy bit
// is cleared, because until the bit is cleared this slot is still ours. Zeroing
// after would race a re-claimer's Publish and clobber a LIVE reader's instant,
// which is the unsafe direction.
//
// The cost is one store to a cache line this goroutine already owns — it published
// its own instant into that same line when it entered — against the previous
// version's zero stores. What it buys back is that a pass landing in the claim
// window reclaims nothing instead of reclaiming conservatively, which is a window
// one store wide.
func (h *Horizon) Leave(slot int) {
	if slot == unregistered {
		h.unreg.Add(-1)
		return
	}
	h.slots[slot].ts.Store(0)
	mask := uint64(1) << uint(slot&63)
	if old := h.occ[slot>>6].bits.And(^mask); old&mask == 0 {
		h.staleLeave.Add(1)
	}
}

// StaleLeaves reports how many times a slot was released whose occupancy bit was
// already clear.
//
// It must be ZERO. A non-zero value means a horizon slot was returned twice, or a
// slot number was released by something that never claimed it, and the consequence
// is an Isolation violation rather than a leak: the next release lands on ANOTHER
// reader's bit, that reader stops being counted by [Horizon.Oldest], and the
// reclamation watermark advances past an instant it can still reach.
//
// Exported so the invariant is observable from outside the package — a test or an
// operator can assert it directly instead of inferring it from a torn read.
//
// Safe for concurrent use.
func (h *Horizon) StaleLeaves() int64 { return h.staleLeave.Load() }

// SlotState reports the start instant a slot currently announces and whether it is
// still occupied, for a caller that must verify a reader is still represented in
// the watermark.
//
// The instant is decoded: it is what [Horizon.Oldest] would contribute for this
// slot, so a reader can compare it against its OWN start timestamp. A slot claimed
// but not yet published reads as (0, true), which is the hold-everything state.
// An unregistered slot reads as (0, false).
//
// Two atomic loads, no locks and no writes. It exists because the invariant "my
// slot still holds MY instant for as long as I am reading" is the one the whole
// reclamation design rests on, and until this accessor existed it could only be
// checked from inside this package.
//
// Safe for concurrent use.
func (h *Horizon) SlotState(slot int) (startTS uint64, occupied bool) {
	if slot == unregistered || slot < 0 || slot >= horizonSlots {
		return 0, false
	}
	occupied = h.occ[slot>>6].bits.Load()&(uint64(1)<<uint(slot&63)) != 0
	v := h.slots[slot].ts.Load()
	if v == 0 {
		return 0, occupied
	}
	return v - 1, occupied
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
// No locks and no writes, so a reclaimer may call it as often as it likes without
// disturbing readers.
//
// # Cost: O(active readers), not O(capacity)
//
// It reads the [horizonWords] occupancy words and then only the slots whose bit is
// set, so an idle graph costs 16 loads and a graph with k readers costs 16+k. It used
// to touch all 1024 slot cache lines unconditionally, which cost 448 ns and — because
// [Graph.EndRead] calls this on every read once versions exist — put 200 ns on every
// single read under a writer (rmp #2292).
//
// # A slot claimed but not yet stamped
//
// A reader that has taken its bit but not yet stored its timestamp leaves the slot
// reading zero, which is not a timestamp. The only sound response is to hold
// everything back, exactly as [Horizon.EnterHolding] does for the same reason: the
// arriving reader's start timestamp is not yet known, and assuming anything about it
// would be assuming in the unsafe direction. The window is one store wide, and since
// [Horizon.Leave] invalidates the timestamp it is the ONLY state in which an occupied
// slot reads zero — so this branch is what makes the returned watermark either exact
// or "reclaim nothing", never a stale value from a previous occupant. That is the
// property [MVCCStats.WatermarkRegressions] rests on.
func (h *Horizon) Oldest(fallback uint64) uint64 {
	if h.unreg.Load() != 0 {
		return 0
	}
	// THE FALLBACK IS A CEILING, NOT A DEFAULT, and that is a correctness fix
	// (rmp #2420).
	//
	// This loop used to carry a `found` flag and take the first occupied slot's
	// timestamp UNCONDITIONALLY — `if !found || ts < oldest` — so the fallback was
	// discarded the moment any reader was seen, and the result could come out ABOVE
	// it. That is unsound, and it is the one hole in this structure's protection of
	// a reader that arrives while a scan is in progress:
	//
	//	reclaimer   samples fallback = 100 (the published frontier)
	//	reader X    claims a slot in a word the scan has ALREADY PASSED, then
	//	            publishes startTS = 104 — invisible to this scan
	//	reader Y    is seen, at startTS = 105
	//	reclaimer   returns 105, having thrown 100 away
	//	sweep       frees every version stamped <= 105, including the one stamped
	//	            105 that X must undo to resolve back to 104
	//	reader X    reads the value committed at 105 through a snapshot pinned at
	//	            104 — over-visible by exactly one transaction
	//
	// Claiming the bit before reading the clock ([Horizon.EnterHolding]) protects a
	// reader the scan SEES — it reads zero and the scan suspends. It cannot protect
	// one the scan never looks at, because the word was read before the bit was set.
	// The fallback is what covers that case, and only because the caller sampled it
	// BEFORE this scan: every reader that appears afterwards begins at or after the
	// frontier of that moment, so a watermark capped at the fallback is below every
	// such reader's start instant by construction.
	//
	// Capping is also cheap and self-correcting: a pass whose readers are all newer
	// than its fallback simply frees less, and the next pass samples a newer
	// fallback. Nothing is retained for longer than one pass.
	oldest := fallback
	for w := 0; w < horizonWords; w++ {
		m := h.occ[w].bits.Load()
		for m != 0 {
			b := bits.TrailingZeros64(m)
			m &= m - 1
			v := h.slots[w<<6+b].ts.Load()
			if v == 0 {
				return 0
			}
			if ts := v - 1; ts < oldest {
				oldest = ts
			}
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
// It counts OCCUPANCY BITS, not non-zero timestamps: a released slot keeps its
// previous occupant's timestamp (see [Horizon.Enter]), so counting timestamps would
// report every slot the graph has ever used as active.
func (h *Horizon) Active() int {
	n := 0
	for w := 0; w < horizonWords; w++ {
		n += bits.OnesCount64(h.occ[w].bits.Load())
	}
	return n
}
