package mvcc

// commitlog.go — which allocated commit timestamps have finished, so a reader
// can be handed an instant below which nothing is still in flight (rmp #2298).
//
// # The problem it exists to solve
//
// Committing is two steps: allocate a timestamp ([Clock.NextCommitTS]), then
// announce it ([Clock.PublishCommitTS]). While commits are serialised by a
// global write barrier those two steps happen in allocation order, so "the
// highest timestamp published" and "the highest timestamp below which
// everything is published" are the same number and one monotone counter
// suffices. That is what [Clock.ReadTS] used to return, and
// docs/audit-mvcc-sole-cc-2026-08-02.md §4.1 records it as the first thing that
// has to change before two writers may commit at once.
//
// Once they may, the two numbers diverge and the difference is a wrong answer.
// Writer A allocates 4 and starts its fsync; writer B allocates 5 and finishes
// first. A reader starting at "highest published" would take 5 — and see
// commit 5 while commit 4, which was allocated EARLIER, is still invisible. It
// straddles a commit, exactly the torn read Example 27's bank-transfer
// invariant caught during rmp #2290 (see [Clock.ReadTS]), and when A finally
// publishes, 4 becomes visible to a reader that already reported a state
// without it.
//
// # The shape, and why this one
//
// Two reference implementations solve this, and they solve it differently.
//
// PostgreSQL builds a per-snapshot LIST of what is in flight. GetSnapshotData
// (postgres/postgres, branch REL_17_STABLE, read 2026-08-02;
// src/backend/storage/ipc/procarray.c) walks the proc array and fills xmin,
// xmax and an xip[] array of running xids; visibility is then
// XidInMVCCSnapshot: below xmin visible, at or above xmax invisible, otherwise
// SEARCH THE ARRAY. The array is sized from GetMaxSnapshotXidCount(), i.e.
// max_connections, and the comment there is explicit that allocating for
// maxProcs is "usually overkill" but is done to avoid holding a lock.
//
// Memgraph keeps a BITSET of finished ids instead. CommitLog
// (memgraph/memgraph, branch master, read 2026-08-02;
// src/storage/v2/commit_log.hpp and .cpp) is a singly-linked chain of blocks,
// each `uint64_t field[kBlockSize]` with kBlockSize = 8192, so 524 288 ids per
// block; MarkFinished sets a bit and, when the id was the oldest active one,
// UpdateOldestActive rescans forward; a block whose every word is
// numeric_limits<uint64_t>::max() is deallocated and head_start_ advances by
// kIdsInBlock. OldestActive() is then a single field read under a spin lock.
//
// GoGraph takes MEMGRAPH'S CONTIGUOUS FRONTIER rather than PostgreSQL's
// per-snapshot list, for three reasons that are about this engine and not about
// taste:
//
//  1. THE READ PATH DOES NOT MOVE. With a contiguous frontier, [Clock.ReadTS]
//     stays one atomic load and [Visible] stays one comparison — the read-side
//     test that runs per version-chain node on every versioned read. Adopting
//     xip would put an xmin/xmax pair plus an array search on that path.
//     PostgreSQL can afford it: its visibility test runs per heap tuple behind
//     a current-transaction fast path, and its snapshots are long-lived enough
//     to amortise building the list.
//
//  2. GOGRAPH'S IN-FLIGHT WINDOW IS A COMMIT, NOT A TRANSACTION. The commit
//     timestamp is allocated at commit time (graph/lpg/mvcc_write.go:91), not
//     at BEGIN, so the set of allocated-but-unfinished timestamps only ever
//     holds transactions inside their commit critical section. PostgreSQL's
//     xip exists because a backend holds an xid from its first write until
//     commit — possibly minutes — and excluding everything above the oldest
//     running xid would make every snapshot uselessly stale. That reasoning
//     does not transfer.
//
//  3. THE MEMORY IS PER CLOCK, NOT PER READ. What must be retained is the
//     window between the oldest unfinished timestamp and the newest allocated
//     one — bounded by how many writers are committing at once — and it is held
//     once, on the publish side. PostgreSQL bounds xip by max_connections and
//     pays that allocation per snapshot; GoGraph would pay it per read.
//
// # How the shape changed: a lock-free registry of commits (rmp #2932)
//
// The first implementation held Memgraph's shape literally: a bitmap of blocks
// behind a publish lock, with a lock-free fast path in front for the in-order
// case. Memgraph can afford the lock because it never contends on it — its commit
// allocates the timestamp and marks it finished under one engine_lock_
// (memgraph/memgraph, master ebf1bc0d, src/storage/v2/inmemory/storage.cpp,
// Commit), and CommitLog::MarkFinished takes a spin lock per call
// (src/storage/v2/commit_log.cpp). GoGraph commits in parallel, and that lock
// became the bottleneck: measured on bench/mvccwrite's mixed arm at 32 writers,
// a straggling publication pushed every later one onto the lock, and a writer
// descheduled while holding it held the frontier for milliseconds — long enough
// for writers to exhaust 64 serialization-conflict retries on their own earlier
// commits.
//
// Removing the lock was not enough on its own. The frontier is contiguous, so a
// commit whose OWNER is descheduled between allocating its timestamp and
// publishing it holds the frontier just the same, and on 10 cores running 32
// writers such windows reached 2.2-4.4 ms. So the registry does two things:
//
//   - OUT-OF-ORDER COMPLETION WITHOUT A LOCK. Every allocated timestamp owns a
//     record in a fixed ring of slots, and a publication that finishes out of
//     order marks its record finished with one atomic store; whoever closes the
//     gap below it carries the frontier over it with a compare-and-swap. This is
//     the structural idea of InnoDB's Link_buf (mysql/mysql-server, trunk
//     3b99be40, storage/innobase/include/ut0link_buf.h: slots filled out of
//     order, a tail any thread advances over the contiguous run), and its log
//     writer's reliance on concurrent threads reporting completion into it
//     (storage/innobase/log/log0buf.cc, log_buffer_write_completed).
//   - HELPING. A publication stuck behind a commit that is READY — every
//     precondition of its publication holds — stamps that commit's record and
//     advances the frontier over it on its owner's behalf. This is the helping
//     discipline of lock-free algorithms (Herlihy and Shavit, The Art of
//     Multiprocessor Programming, ch. 10-11; Harris, "A Pragmatic
//     Implementation of Non-Blocking Linked-Lists", DISC 2001): an operation
//     that would otherwise wait on a stalled peer completes the peer's
//     operation instead, each step decided by a compare-and-swap so that exactly
//     one party wins it. PostgreSQL's group commit is the lock-based cousin:
//     ProcArrayGroupClearXid has the lock holder clear every queued backend's
//     xid on its behalf (postgres/postgres, REL_17_STABLE 6dba8019,
//     src/backend/storage/ipc/procarray.c) — the same idea of completing others'
//     work, but its followers sleep until the leader is done, and the registry
//     has no leader and no sleep.
//
// A timestamp is allocated by REGISTERING its record in the slot for that
// timestamp, so the record exists — and a helper can find it — from the instant
// the timestamp does. See [Clock.AllocateFor] for the protocol, the invariants
// and who may publish what.
//
// THE COST, STATED PLAINLY. A reader cannot observe a commit above the oldest
// unfinished one even when it has already published. The staleness is the
// duration of the longest in-flight commit — a WAL fsync, measured at 3.73 ms
// in docs/benchmarks/mvcc-write-scaling-2026-08-02.md. Memgraph accepts exactly
// this trade, and it buys a hot path that does not grow.

import "sync/atomic"

// registrySlots is the number of slots in the commit registry. A slot holds the
// records of the timestamps congruent to it modulo registrySlots, newest first; a
// timestamp's record stays findable until the frontier passes it, however many
// laps of the ring are in flight at once (see [commitRegistry.claim]), so the
// number bounds nothing but the length of those chains, which is one while fewer
// than registrySlots commits are in flight. 4096 slots are 32 KiB, allocated by
// the first allocation.
const registrySlots = 1 << 12

const registryMask = registrySlots - 1

// commitRegistry maps every allocated commit timestamp that the frontier has not
// yet passed to the record that owns it. See the file comment and
// [Clock.AllocateFor].
//
// Safe for concurrent use.
type commitRegistry struct {
	slots atomic.Pointer[[registrySlots]atomic.Pointer[CommitInfo]]
	// outOfOrder counts publications that finished while an earlier timestamp
	// was still in flight; helped counts publications performed on an owner's
	// behalf; chained counts claims that found the previous lap's record still
	// pending and linked behind it. See the [Clock] accessors.
	outOfOrder atomic.Uint64
	helped     atomic.Uint64
	chained    atomic.Uint64
	// pending is how many records are marked finished and not yet passed by the
	// frontier. While it is zero an in-order publication has nothing above it to
	// carry the frontier over, and skips the walk (see [Clock.finishCommitTS]).
	// It can read -1 for an instant: a record is marked before it is counted in,
	// so a party that passes it in between counts it out first.
	pending atomic.Int64
	// linked is how many records hold a non-nil [CommitInfo.older]. While it is
	// zero an in-order publication has no link to clear on the record it passes,
	// and skips looking the record up (see [commitRegistry.unlink]).
	linked atomic.Int64
}

// The states of [CommitInfo.finished].
const (
	finishedNone    = 0
	finishedMarked  = 1
	finishedCounted = 2
)

// settle takes a record the frontier has passed back out of pending, exactly
// once whoever calls it.
func (r *commitRegistry) settle(rec *CommitInfo) {
	if rec.finished.CompareAndSwap(finishedMarked, finishedCounted) {
		r.pending.Add(-1)
	}
}

// passed is what every party that carries the frontier over rec owes it: take it
// out of pending, and drop its link to the previous lap's record.
func (r *commitRegistry) passed(rec *CommitInfo) {
	r.settle(rec)
	r.unlink(rec)
}

// unlink clears rec's link to the previous lap's record of its slot, exactly
// once whoever calls it. The caller has just carried the frontier over rec, so
// the frontier is past the linked record too and nothing needs to find it any
// more. Clearing the link is what lets a chained record be reclaimed: rec is
// referenced by every version its transaction wrote, so a link kept for rec's
// lifetime would keep the chain behind it alive as long (rmp #2932 audit).
//
// No unpassed record becomes unreachable: a link is cleared only on a record the
// frontier has passed, and a chain is ordered newest first, so every record
// behind a passed one has been passed as well.
func (r *commitRegistry) unlink(rec *CommitInfo) {
	if o := rec.older.Load(); o != nil && rec.older.CompareAndSwap(o, nil) {
		r.linked.Add(-1)
	}
}

// table returns the registry's slots, allocating them the first time.
func (r *commitRegistry) table() *[registrySlots]atomic.Pointer[CommitInfo] {
	if t := r.slots.Load(); t != nil {
		return t
	}
	fresh := new([registrySlots]atomic.Pointer[CommitInfo])
	if r.slots.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return r.slots.Load()
}

// claim allocates the next commit timestamp by installing info as the head of
// that timestamp's slot, and returns it. The compare-and-swap on the slot is the
// arbiter: exactly one record wins each timestamp. The allocation counter
// follows the slot, and an allocator that finds a slot claimed ahead of the
// counter advances the counter for the claimant, so a claimant descheduled
// between the two steps delays nobody.
//
// When the slot's current head belongs to a timestamp the frontier has not yet
// passed — more than registrySlots commits in flight — info is linked in front
// of it rather than replacing it, so every pending record stays findable. Nothing
// ever waits for room.
func (r *commitRegistry) claim(commit, visible *atomic.Uint64, info *CommitInfo) uint64 {
	t := r.table()
	for {
		ts := commit.Load() + 1
		slot := &t[ts&registryMask]
		head := slot.Load()
		if head != nil && head.claim.Load() >= ts {
			commit.CompareAndSwap(ts-1, ts)
			continue
		}
		info.claim.Store(ts)
		var older *CommitInfo
		if head != nil && head.claim.Load() > visible.Load() {
			older = head
			// Counted in BEFORE the record is installed: the timestamp does not
			// exist until then, so whoever later passes it — and reads linked to
			// decide whether to look for a link — reads it after this.
			r.linked.Add(1)
		}
		info.older.Store(older)
		if slot.CompareAndSwap(head, info) {
			if older != nil {
				r.chained.Add(1)
			}
			commit.CompareAndSwap(ts-1, ts)
			return ts
		}
		// Not installed, so nobody else can see the link: take it back.
		if older != nil {
			info.older.Store(nil)
			r.linked.Add(-1)
		}
	}
}

// find returns the record that owns ts, or nil when ts has no record the
// frontier has not passed. A slot's chain is ordered newest first, so the walk
// stops at the first older claim.
func (r *commitRegistry) find(ts uint64) *CommitInfo {
	t := r.slots.Load()
	if t == nil {
		return nil
	}
	for rec := t[ts&registryMask].Load(); rec != nil; rec = rec.older.Load() {
		switch c := rec.claim.Load(); {
		case c == ts:
			return rec
		case c < ts:
			return nil
		}
	}
	return nil
}
