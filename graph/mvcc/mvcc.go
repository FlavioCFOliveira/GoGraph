// Package mvcc holds the timestamp and visibility primitives the versioned
// stores share.
//
// It exists because versioning spans two packages that cannot import each
// other. Node labels and node properties live in [lpg]; the adjacency — which
// edges exist — lives in [adjlist], which lpg imports. Both must answer the
// same question about the same transaction, so the answer cannot live in
// either. Everything here is deliberately small, dependency-free and
// concurrency-safe.
//
// # The timestamp space
//
// One uint64 carries three states, split at [TxIDBase], which is what makes the
// visibility test a single comparison rather than a registry lookup:
//
//	ts <  TxIDBase        committed, and ts is the commit timestamp
//	ts >= TxIDBase        in flight, and ts is the transaction id
//	ts == AbortedTS       aborted
//
// Commit timestamps and transaction ids are both monotonic and neither is ever
// reused, so a reader never has to ask whether a writer is still alive.
//
// The encoding is Memgraph's, read from `src/storage/v2/mvcc.hpp` at master on
// 2026-07-31, where uncommitted deltas carry the writer's transaction id and
// committed ones its commit timestamp, separated by kTransactionInitialId.
package mvcc

import (
	"fmt"
	"sync/atomic"
)

// TxIDBase separates commit timestamps from transaction ids.
const TxIDBase uint64 = 1 << 63

// AbortedTS marks a transaction whose changes must never become visible.
//
// It sits above [TxIDBase] and equals no transaction id a [Clock] can mint, so
// the ordinary rule in [Visible] already classifies it as another transaction's
// uncommitted work — no dedicated branch on the read path. It is
// distinguishable only so garbage collection can recognise a chain it may
// reclaim eagerly.
const AbortedTS = ^uint64(0)

// CommitInfo is the commit record SHARED by every version one transaction
// writes, in every store.
//
// Publishing a transaction is a single atomic store into it, so all of its
// changes — labels, properties and topology alike — become visible at one
// instant however many there are and however many stores they span. That is the
// whole reason it is a pointer rather than a timestamp copied into each record,
// and it is why the same type has to be reachable from both packages.
//
// Memgraph heap-allocates the equivalent for the same reason, stated in
// `src/storage/v2/transaction.hpp`: "`Delta`s have a pointer to it, and that
// pointer must stay valid after the `Transaction` is moved".
//
// Safe for concurrent use.
type CommitInfo struct {
	// ts is the transaction id while in flight, the commit timestamp once
	// committed, and [AbortedTS] once aborted.
	ts atomic.Uint64
	// claim is the commit timestamp this record was registered for by
	// [Clock.AllocateFor], or 0. A record is never recycled, so a helper that
	// finds it in the clock's registry and reads claim == t knows it is looking
	// at the record that owns t (rmp #2932).
	claim atomic.Uint64
	// ready says the owner has decided to commit and every precondition of
	// publication holds — its versions are complete, its WAL record is durable,
	// its index changes are applied — so another publication may stamp and
	// publish the record on the owner's behalf. It is set once and never cleared.
	//
	// The one exception to "durable" is a WAL fsync that failed: the engine
	// undoes every write inside the record and then publishes it, ready, so what
	// becomes visible is a set of versions that net to zero — the state before
	// the transaction.
	ready atomic.Bool
	// finished says the owner has finished claim — published it, having stamped
	// the record first, or abandoned it — while an earlier timestamp was still in
	// flight, so whoever closes that gap may carry the frontier over it:
	// finishedMarked. finishedCounted records that the record has been taken back
	// out of [commitRegistry.pending], by exactly one party. See
	// [Clock.finishCommitTS].
	finished atomic.Uint32
	// older is the record of the same registry slot one lap earlier, kept linked
	// while its timestamp is still pending. It is written before the record is
	// installed, and cleared by whoever carries the frontier past this record's
	// own claim, which is after the frontier has passed older's: a record the
	// frontier has passed no longer needs to be findable, and a record that
	// kept the link would keep the whole chain behind it alive for as long as
	// any version still referenced it. See [commitRegistry.claim] and
	// [commitRegistry.unlink].
	older atomic.Pointer[CommitInfo]
}

// NewCommitInfo returns a record stamped with an in-flight transaction id.
func NewCommitInfo(txID uint64) *CommitInfo {
	c := &CommitInfo{}
	c.ts.Store(txID)
	return c
}

// NewCommittedInfo returns a record already committed at ts. It is the
// autocommit form: a single-statement write is committed the instant it is
// made and its record is never mutated again.
func NewCommittedInfo(ts uint64) *CommitInfo {
	c := &CommitInfo{}
	c.ts.Store(ts)
	return c
}

// Commit publishes every change stamped with this record, atomically.
//
// It stamps by compare-and-swap from the in-flight transaction id, so that the
// owner and a helper publishing on its behalf ([Clock.AllocateFor]) cannot both
// win: whichever stamps first stamps, and the other's call is a no-op. Both stamp
// the same timestamp, so the outcome does not depend on who won. A record that is
// already committed or aborted is left as it is.
func (c *CommitInfo) Commit(commitTS uint64) { c.stamp(commitTS) }

// stamp moves the record from in flight to committed at commitTS and reports
// whether this call did it.
func (c *CommitInfo) stamp(commitTS uint64) bool {
	for {
		cur := c.ts.Load()
		if cur < TxIDBase || cur == AbortedTS {
			return false // already committed, or aborted: never re-stamped
		}
		if c.ts.CompareAndSwap(cur, commitTS) {
			return true
		}
	}
}

// MarkReady declares that every precondition of publishing this record holds, so
// a helper may stamp and publish it on the owner's behalf. See [CommitInfo.ready]
// and [Clock.AllocateFor].
func (c *CommitInfo) MarkReady() { c.ready.Store(true) }

// Abort makes every change stamped with this record permanently invisible.
func (c *CommitInfo) Abort() { c.ts.Store(AbortedTS) }

// TS returns the record's current timestamp.
func (c *CommitInfo) TS() uint64 { return c.ts.Load() }

// Visible reports whether a change stamped ts is visible to a reader that
// started at startTS running as transaction txID.
//
// The three cases, in Memgraph's order:
//
//   - the change is the reader's OWN uncommitted work, so it is visible;
//   - the change is committed, so it is visible when it committed at or before
//     the reader started;
//   - the change belongs to another transaction that has not committed (or has
//     aborted), so it is never visible.
//
// Callers hold versions as UNDO records, so most of them want the negation:
// "must I undo this to see my version?" is `!Visible(...)`.
func Visible(ts, startTS, txID uint64) bool {
	switch {
	case ts == txID:
		return true
	case ts < TxIDBase:
		return ts <= startTS
	default:
		return false
	}
}

// Clock mints commit timestamps and transaction ids from the two disjoint
// ranges either side of [TxIDBase].
//
// Safe for concurrent use.
type Clock struct {
	commit atomic.Uint64
	// visible is the highest timestamp every commit at or below which has
	// FINISHED. It is what a reader starts at, and it is a separate counter
	// from the allocation one for a reason that is a correctness bug, not an
	// optimisation — see [Clock.ReadTS].
	//
	// "Every commit at or below which" is the load-bearing clause, and it is
	// what [commitRegistry] supplies: while commits were serialised, publication
	// happened in allocation order and the highest published timestamp was also
	// the highest contiguous one, so a monotone maximum was enough. It is not
	// enough once two writers may publish out of order (rmp #2298).
	visible atomic.Uint64
	txSeq   atomic.Uint64

	// reg maps each allocated, not-yet-passed commit timestamp to the record that
	// owns it. It takes no lock, on the publish path or on a read. See
	// [Clock.AllocateFor].
	reg commitRegistry

	// afterAllocate, when set, runs in the owner right after [Clock.AllocateFor]
	// has registered and allocated a timestamp. It exists for tests, which use it
	// to deschedule an owner exactly inside the window helping must cover. Set it
	// only before the clock is shared.
	afterAllocate func(ts uint64)
	// afterInOrderPublish, when set, runs right after an in-order publication has
	// moved the frontier to ts and before it clears the record's lap link. It
	// exists for tests, which use it to let a later lap displace the record from
	// its slot inside that window. Set it only before the clock is shared.
	afterInOrderPublish func(ts uint64)

	// waiters is how many callers are inside [Clock.AwaitVisible], and wait is the
	// broadcast channel generation they block on (rmp #2328). The publish path reads
	// waiters to decide whether a broadcast is owed, so the common case — a commit
	// with no session blocked on it — costs one atomic load and no channel work.
	// See await.go.
	waiters atomic.Int64
	wait    atomic.Pointer[waitGate]
}

// OutOfOrderPublications reports how many commit timestamps have finished —
// published or abandoned — while an earlier one was still in flight, and so
// were left for the publication that closes the gap to carry the frontier over.
// Against the number of commits it is the share of publications that took the
// slow path. That path takes no lock (rmp #2932), so a high share costs a few
// atomic operations each and never a convoy.
//
// Safe for concurrent use.
func (c *Clock) OutOfOrderPublications() uint64 { return c.reg.outOfOrder.Load() }

// HelpedPublications reports how many times a publication found the frontier
// stuck on another commit that was ready, and stamped and published it on its
// owner's behalf (see [Clock.AllocateFor]). A non-zero value is not an error:
// each count is an owner descheduled inside its commit window that did not hold
// the frontier for everyone else.
//
// Safe for concurrent use.
func (c *Clock) HelpedPublications() uint64 { return c.reg.helped.Load() }

// NextCommitTS allocates the next commit timestamp. Monotonic, never reused.
//
// Allocating is NOT publishing: the caller must call [Clock.PublishCommitTS]
// once the timestamp is stored in the transaction's commit record and its
// changes are therefore visible.
//
// The timestamp is registered with an anonymous record that is never ready, so
// no helper will ever publish it: its owner must. Callers that hold the record
// their timestamp will stamp use [Clock.AllocateFor] instead.
func (c *Clock) NextCommitTS() uint64 { return c.AllocateFor(nil, false) }

// AllocateFor allocates the next commit timestamp for the record info and
// REGISTERS info as its owner, in that one step: the timestamp does not exist
// until info is registered under it. When ready is set, info is also marked
// ready ([CommitInfo.MarkReady]) first, so the commit may be stamped and
// published by a helper from the instant its timestamp exists. A nil info
// registers an anonymous record that is never ready.
//
// # Why registration comes first (rmp #2932)
//
// The frontier is contiguous, so a commit whose owner is descheduled between
// allocating its timestamp and publishing it holds the frontier for every reader
// and every writer. Measured on bench/mvccwrite's mixed arm at 32 writers on 10
// cores, such windows reached 2.2-4.4 ms, and writers exhausted 64
// serialization-conflict retries on their own earlier commits behind them. A
// later publication can close the window — stamp the record and advance the
// frontier over it — but only if it can find the record, and a timestamp that
// exists before its record is registered opens a window no helper can close. So
// the allocation IS the registration: a timestamp is claimed by installing the
// record in the registry slot for that timestamp, and the allocation counter
// follows. An allocator descheduled between the two leaves the counter behind a
// claimed slot, and the next allocator to find the slot claimed advances the
// counter for it — the helping step of the Harris/Herlihy lock-free pattern,
// applied to the allocation itself.
//
// # What a helper may and may not do
//
// A helper publishes only a record that is ready and that it found registered
// under exactly the timestamp the frontier is stuck below. It stamps the record
// by compare-and-swap from the in-flight id and then advances the frontier by
// compare-and-swap, the same two steps the owner takes, in the same order, so
// whichever party wins each step, the result is the owner's commit at the owner's
// timestamp. It never publishes a record that is not ready — one whose WAL record
// is not yet durable, or whose index changes are not yet applied — and it never
// publishes one that was aborted, because an aborted record is never ready.
//
// # The invariants, and what enforces each
//
//  1. NO READER SEES A COMMIT BEFORE ALL ITS VERSIONS ARE STAMPED. A
//     transaction's versions share one record, so stamping the record stamps
//     them all at once. The frontier passes a timestamp only by the owner's
//     publication, which follows the owner's stamp, or by a helper's, which
//     follows the helper's stamp and a re-read that finds the record committed at
//     exactly that timestamp ([Clock.advance]).
//  2. THE FRONTIER IS MONOTONE. It moves only by compare-and-swap from f to a
//     greater value ([Clock.advance], [Clock.finishCommitTS], [Clock.RatchetTo]).
//  3. THE FRONTIER NEVER PASSES A COMMIT IN FLIGHT. It moves from f to f+1 only
//     when f+1 is this publication's own timestamp, or its record is finished, or
//     it is ready and has just been stamped.
//  4. THE OWNER'S ACKNOWLEDGEMENT STAYS CORRECT WHOEVER PUBLISHED. The owner
//     learns its timestamp from this call and nobody else can change it: the
//     record's stamp is decided by one compare-and-swap from the in-flight id,
//     and every party stamps the same value. The owner's own stamp and
//     publication are then no-ops, and it returns the same timestamp.
//  5. AN ABANDONED OR ABORTED TIMESTAMP IS RELEASED, NEVER PUBLISHED AS A COMMIT.
//     Such a record is never marked ready, so no helper stamps it; its owner
//     marks it finished without stamping it, and the frontier passes a record
//     that is aborted or still carries its transaction id, both of which every
//     reader treats as invisible.
//  6. INDEX MAINTENANCE KEEPS ITS PLACE. The owner applies its index changes
//     before it marks the record ready (graph/lpg's commit-apply step), so a
//     helper publishes a commit whose index changes are already in place; the
//     owner closes the index watermark only after its own publication, which
//     comes after any helper's, so the watermark still rises after publication.
//     A helper never runs the index step itself: it cannot, and it does not need
//     to.
//  7. EXACTLY ONE RECORD OWNS EACH TIMESTAMP, AND EACH RECORD AT MOST ONE. A
//     timestamp is allocated by the compare-and-swap that installs its record in
//     the registry slot, and the allocation counter only ever follows an
//     installed record. A record already registered is refused (below): its
//     second registration would overwrite the claim the first timestamp's
//     helpers look it up by, and leave that timestamp with no findable owner.
//
// # Contract
//
// AllocateFor panics when info has already been registered for a timestamp. A
// record is allocated for at most once in its life; a caller that allocates
// twice for one transaction has a bug that must surface, not a second timestamp
// that nothing will ever publish.
//
// Safe for concurrent use.
func (c *Clock) AllocateFor(info *CommitInfo, ready bool) uint64 {
	if info == nil {
		info = &CommitInfo{}
	} else {
		if claimed := info.claim.Load(); claimed != 0 {
			panic(fmt.Sprintf("mvcc: AllocateFor: record already registered for commit timestamp %d", claimed))
		}
		if ready {
			info.MarkReady()
		}
	}
	ts := c.reg.claim(&c.commit, &c.visible, info)
	if h := c.afterAllocate; h != nil {
		h(ts)
	}
	return ts
}

// advance carries the frontier over every timestamp just above it that has
// finished and, when help is set and the frontier is still below own, over every
// one that is READY, stamping its record and publishing it on its owner's behalf.
// It reports whether the frontier moved.
//
// Help is given only by a publication that is itself out of order — stuck below
// a commit still in flight — and only until the frontier reaches its own
// timestamp. An in-order publication has nobody to wait for, and helping the next
// owner, who is most likely an instruction away from publishing itself, would only
// contend on that owner's record.
func (c *Clock) advance(own uint64, help bool) bool {
	moved := false
	for {
		f := c.visible.Load()
		next := f + 1
		rec := c.reg.find(next)
		if rec == nil {
			return moved
		}
		if rec.finished.Load() == finishedNone {
			if !help || f >= own || !rec.ready.Load() {
				return moved
			}
			// Stamp first: the frontier must never pass a commit whose record is
			// still in flight, or a reader would start at an instant that includes
			// it and not see it. The owner may have stamped already; either way the
			// record is now committed at next — a ready record is never aborted.
			rec.stamp(next)
			if rec.ts.Load() != next {
				return moved
			}
			if c.visible.CompareAndSwap(f, next) {
				c.reg.helped.Add(1)
				moved = true
				// Its owner may have marked it finished after the check above; the
				// owner re-reads the frontier after marking, and this settle follows
				// the CAS, so whichever of the two comes second takes it out.
				c.reg.passed(rec)
			}
			continue
		}
		if c.visible.CompareAndSwap(f, next) {
			moved = true
			c.reg.passed(rec)
		}
	}
}

// PublishCommitTS announces that every change committed at ts is now visible.
//
// It does NOT simply raise the visible instant to ts. It records ts as finished
// and moves the instant to the newest timestamp below which nothing is still in
// flight, which is the same number only while commits are serialised. Publishing
// out of allocation order is the case this exists for: a reader must never be
// handed an instant that includes a commit but excludes an earlier one that has
// not finished yet. See [commitRegistry] for the shape and the prior art.
//
// Monotonic: the frontier only ever advances, and a late publisher whose
// timestamp is already behind it changes nothing.
func (c *Clock) PublishCommitTS(ts uint64) { c.finishCommitTS(ts, nil) }

// PublishCommit is [Clock.PublishCommitTS] for an owner that holds the record
// it registered for ts ([Clock.AllocateFor]). Passing the record lets the
// publication clear the record's lap link without looking it up: once the
// frontier has passed ts, a later lap may displace the record from its slot, and
// a lookup would then not find it (rmp #2932 re-audit, N3). A record that was not
// registered for ts is ignored, and the lookup is used instead.
func (c *Clock) PublishCommit(info *CommitInfo, ts uint64) { c.finishCommitTS(ts, info) }

// AbandonCommitTS records that ts was allocated but will never be published —
// the transaction failed after taking a timestamp and nothing it wrote will
// ever be visible under it.
//
// It exists because the frontier is CONTIGUOUS: a timestamp that is neither
// published nor abandoned stalls it forever, and every later commit becomes
// permanently invisible to new readers while the commit log grows without
// bound. An allocate-then-fail path is therefore obliged to call this, and the
// obligation is why it is a named operation rather than an internal detail of
// [Clock.PublishCommitTS].
//
// Every allocation in the module still publishes, and rmp #2300 did NOT change
// that: a transaction refused by write-write conflict detection aborts WITHOUT
// allocating a commit timestamp at all ([lpg.Graph.endWrite] marks the record
// [AbortedTS] and returns), so there is nothing to abandon. This remains the
// operation an allocate-THEN-fail path would owe the frontier, and it has no
// caller — which is the honest state to record rather than deleting it and
// leaving the obligation undocumented for whoever next writes such a path.
func (c *Clock) AbandonCommitTS(ts uint64) { c.finishCommitTS(ts, nil) }

// AbandonCommit is [Clock.AbandonCommitTS] for an owner that holds the record it
// registered for ts, for the reason [Clock.PublishCommit] gives.
func (c *Clock) AbandonCommit(info *CommitInfo, ts uint64) { c.finishCommitTS(ts, info) }

// finishCommitTS marks ts finished and republishes the frontier.
//
// # The in-order case (rmp #2362)
//
// Publication is USUALLY in order, and when it is, this is one compare-and-swap:
// the frontier is already at ts-1 and moving it to ts publishes ts. InnoDB
// short-circuits the same case in the same shape — Link_buf::add_link_advance_tail
// stores the tail directly when the reporting thread IS the tail
// (mysql/mysql-server, trunk 3b99be40, storage/innobase/include/ut0link_buf.h).
// The advance that follows costs one pointer load while nothing has ever finished
// out of order, and one slot load after that.
//
// # The out-of-order case (rmp #2932)
//
// When the frontier is below ts-1, an earlier commit is still in flight. This
// publication marks its own record finished, then helps: it carries the frontier
// over every finished record above it and, until the frontier reaches ts, stamps
// and publishes every READY one on its owner's behalf ([Clock.advance]). No lock
// is taken on either path. The file comment of commitlog.go records the convoy the
// previous locked slow path produced; [Clock.AllocateFor] states the invariants
// and who may publish what.
//
// The pairing that makes it exact: an out-of-order publication stores its
// record's finished flag and THEN reads the frontier; the publication that closes
// the gap below it stores the frontier with its compare-and-swap and THEN reads the
// record. Go's atomics are sequentially consistent, so at least one of the two
// sees the other's store and carries the frontier over it.
//
// own is the record the caller registered for ts, when it holds it, or nil.
func (c *Clock) finishCommitTS(ts uint64, own *CommitInfo) {
	if own != nil && own.claim.Load() != ts {
		own = nil
	}
	if c.visible.CompareAndSwap(ts-1, ts) {
		// IN ORDER. There is something above to carry the frontier over only if
		// some publication has finished out of order and not yet been passed; the
		// counter is written only by those, so in the common case this is one read
		// of a line no core is writing. The pairing that makes skipping the walk
		// exact: an out-of-order publication counts itself in BEFORE it reads the
		// frontier, and this read follows the CAS above, so either this read sees
		// the count or that publication sees this advance.
		if c.reg.pending.Load() != 0 {
			c.advance(ts, false)
		}
		if h := c.afterInOrderPublish; h != nil {
			h(ts)
		}
		// The record just passed may hold a link to the previous lap's record;
		// drop it. linked is non-zero only while more than registrySlots commits
		// have been in flight at once, so this is one read of a quiet line. The
		// owner's own record is used when the caller passed it: the frontier is
		// past ts now, so a later lap may already have displaced the record from
		// its slot without linking it, and a lookup would miss it.
		if c.reg.linked.Load() != 0 {
			rec := own
			if rec == nil {
				rec = c.reg.find(ts)
			}
			if rec != nil {
				c.reg.unlink(rec)
			}
		}
		if c.waiters.Load() > 0 {
			c.wakeWaiters()
		}
		return
	}
	advanced := false
	outOfOrder := false
	if ts > c.visible.Load() {
		// OUT OF ORDER: an earlier commit is still in flight. Mark it finished,
		// count it in, then read the frontier — the pairing that guarantees
		// whoever closes the gap sees it (see [Clock.AllocateFor]).
		//
		// The mark is a compare-and-swap from finishedNone, and only its winner
		// counts the record in: a timestamp finished twice would otherwise be
		// counted in twice and settled once, leaving pending above zero for ever,
		// and a second store would move a record already counted out back to
		// marked. A party that passes the record between the mark and the count
		// counts it out first, which leaves pending at -1 for that instant and
		// correct once the count lands.
		rec := own
		if rec == nil {
			rec = c.reg.find(ts)
		}
		if rec != nil && rec.finished.CompareAndSwap(finishedNone, finishedMarked) {
			c.reg.pending.Add(1)
			c.reg.outOfOrder.Add(1)
			outOfOrder = true
			// A helper may have passed ts between the check above and the mark;
			// then nobody will pass it again, so take it back out here.
			if c.visible.Load() >= ts {
				c.reg.settle(rec)
			}
		}
	}
	if c.advance(ts, outOfOrder) {
		advanced = true
	}
	// The broadcast happens AFTER the frontier is stored: a woken waiter re-reads
	// the frontier, so waking it before the store would send it straight back to
	// sleep having missed the advance it was waiting for (rmp #2328).
	if advanced && c.waiters.Load() > 0 {
		c.wakeWaiters()
	}
}

// raiseVisible moves the published frontier up to at least f, and reports whether
// it moved.
//
// It is a compare-and-swap loop rather than a store because every publication
// raises `visible` concurrently and without a lock (rmp #2362, rmp #2932). A plain
// store could land under a concurrent publication's advance and move the frontier
// BACKWARDS, handing a later reader an instant earlier than one already observed —
// a state no serial order produced. Its only caller is [Clock.RatchetTo].
func (c *Clock) raiseVisible(f uint64) bool { return c.raiseVisibleFrom(c.visible.Load(), f) }

// raiseVisibleFrom is [Clock.raiseVisible] starting from a frontier the caller has
// already read. A stale cur costs one failed compare-and-swap and a reload; it can
// never install a lower value, because the loop only ever swaps a value it has
// just observed for a strictly greater one.
func (c *Clock) raiseVisibleFrom(cur, f uint64) bool {
	for {
		if f <= cur {
			return false
		}
		if c.visible.CompareAndSwap(cur, f) {
			return true
		}
		cur = c.visible.Load()
	}
}

// RatchetTo raises the clock so that every timestamp it subsequently allocates,
// and the instant every new reader starts at, is at least floor. It NEVER lowers
// either, and it is a no-op when the clock has already passed floor.
//
// It returns the resulting allocation counter.
//
// # What it is for: recovery, and why the clock is DERIVED (rmp #2309)
//
// A process-local clock constructed at zero on every open would re-mint instants
// that a previous process already made visible and made durable. The fix is not a
// persisted counter — two of the three reference engines deliberately removed
// theirs. InnoDB keeps TRX_SYS_TRX_ID_STORE "only for the purpose of upgrading"
// and instead folds a max over every rollback segment at startup, then calls
// init_max_trx_id(max + 1). Memgraph derives max(delta_ts)+1 from the WAL and
// info.start_timestamp+1 from a snapshot, then restores
// timestamp_ = max(timestamp_, next_timestamp). PostgreSQL does persist nextXid in
// pg_control but STILL ratchets it per record during replay
// (AdvanceNextFullTransactionIdPastXid).
//
// So the clock is derived from what the durable record actually says, and RAISED
// rather than trusted — which is what this method is. A second source of truth
// would be one that can disagree with the log after a torn tail.
//
// # Why it raises the VISIBLE frontier too, and why that is not a shortcut
//
// Both counters move. Raising only the allocation counter would leave the frontier
// at zero, so every recovered commit would be invisible to a new reader until some
// later commit's publication happened to sweep the frontier past it — a graph that
// reads as empty immediately after recovery.
//
// It is sound here and ONLY here because recovery has no in-flight commits by
// construction: every transaction in the file either reached its durable marker or
// is discarded with the torn tail, so there is no allocated-but-unfinished instant
// for the frontier to be holding back. That is exactly the precondition the
// contiguous frontier normally enforces, satisfied by the situation rather than by
// the commit log — which is why this must not be called on a live clock.
//
// Not safe for concurrent use, and not safe on a clock with commits in flight:
// call it during open, before the graph is published to any reader or writer.
//
// # Why nothing else has to move (rmp #2932)
//
// Until rmp #2932 a THIRD thing had to be rebased with them: the commit log kept
// its own copy of the contiguity, and a log that still believed timestamp 1 was
// unfinished computed a frontier of 0 for ever, so every commit after the ratchet
// was invisible for the life of the process. internal/sim's full-stack
// crash-recovery scenario caught that as node LOSS against its oracle (21
// expected, 15 present). The registry holds no such copy: the contiguity IS the
// visible frontier, and a record names the exact timestamp it owns, so a record
// left from before the ratchet claims a timestamp at or below the new floor and
// can never be mistaken for a commit above it. TestClock_RatchetKeepsTheFrontierMovable
// still pins the property.
func (c *Clock) RatchetTo(floor uint64) uint64 {
	if cur := c.commit.Load(); cur < floor {
		c.commit.Store(floor)
	}
	// Raised with the same compare-and-swap loop the publish path uses: recovery
	// has no commit in flight by construction, but every publication raises the
	// frontier without a lock, and a plain store here would be the one place that
	// assumed otherwise.
	c.raiseVisible(floor)
	return c.commit.Load()
}

// InFlightCommits reports how many allocated commit timestamps have not yet
// finished: the distance between the frontier a reader starts at and the
// newest timestamp handed out.
//
// It is the quantity to watch when readers look stale — a commit stuck between
// allocation and publication holds the frontier for every reader — and it is
// what makes the commit log's memory bound observable, since the log retains
// exactly this window.
//
// Safe for concurrent use.
func (c *Clock) InFlightCommits() uint64 {
	allocated := c.commit.Load()
	visible := c.visible.Load()
	if allocated <= visible {
		return 0
	}
	return allocated - visible
}

// Allocated returns the newest commit timestamp allocated so far. Every timestamp
// at or below it has been allocated, so once [Clock.ReadTS] reaches it every
// commit that had allocated by the time of this call has finished. It is what a
// caller passes to [Clock.AwaitVisible] to wait out the commits in flight now,
// and only those (rmp #2936).
//
// Safe for concurrent use.
func (c *Clock) Allocated() uint64 { return c.commit.Load() }

// ReadTS returns the timestamp a reader starting now must use.
//
// # Why this is the PUBLISHED instant and not the allocated one
//
// Committing is two steps: allocate a timestamp, then store it into the shared
// record. Between them the transaction's changes are still invisible — every
// reader sees the in-flight transaction id — but the allocation counter has
// already moved.
//
// A reader that started at the allocated-but-unpublished value straddles that
// commit. It reads one object before the store and undoes the transaction
// there, reads another after the store and finds the transaction visible
// (its timestamp now equals the reader's own start timestamp), and reports a
// state that never existed. Example 27's bank-transfer invariant caught it
// exactly that way: "readers observed a torn total 40 time(s)". The barrier had
// been hiding it — a reader could not run while a writer held it — and it
// surfaced the moment reads stopped taking it (rmp #2290).
//
// Returning the published instant closes it: a transaction is either wholly
// before a reader's start or wholly after it, with no window in between.
//
// # And why the published instant is a CONTIGUOUS frontier, not a maximum
//
// This comment used to end by saying that publication happens in allocation
// order because commits are serialised by the write barrier, so one counter
// sufficed and no in-progress list was needed — "which is what PostgreSQL's
// snapshot xip_list and Memgraph's commit_log_->OldestActive() exist to supply
// when commits are NOT serialised". Sprint 334 is where commits stop being
// serialised, so that is exactly what rmp #2298 supplied.
//
// The counter is no longer a maximum over published timestamps. It is the
// newest timestamp below which NOTHING is still in flight, maintained by
// [commitRegistry] on the publish path. Without that, writer B allocating 5 and
// finishing before writer A's 4 would hand a reader an instant containing 5 but
// not 4 — the same straddled commit described above, arrived at from the other
// direction.
//
// The cost of the read is unchanged, and that is the point of the shape chosen:
// one atomic load here, one comparison in [Visible]. See [commitRegistry] for the
// prior art and the trade it accepts.
func (c *Clock) ReadTS() uint64 { return c.visible.Load() }

// NextTxID allocates a transaction id, drawn from above [TxIDBase] so it can
// never be mistaken for a commit timestamp.
func (c *Clock) NextTxID() uint64 { return TxIDBase + c.txSeq.Add(1) }
