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
	"sync"
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
func (c *CommitInfo) Commit(commitTS uint64) { c.ts.Store(commitTS) }

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
	// what [commitLog] supplies: while commits were serialised, publication
	// happened in allocation order and the highest published timestamp was also
	// the highest contiguous one, so a monotone maximum was enough. It is not
	// enough once two writers may publish out of order (rmp #2298).
	visible atomic.Uint64
	txSeq   atomic.Uint64

	// pubMu guards log. It is taken once per commit, on the publish path only,
	// and NEVER on a read: readers see the frontier through the visible atomic
	// above, which this lock is what keeps monotone.
	pubMu sync.Mutex
	log   commitLog
}

// NextCommitTS allocates the next commit timestamp. Monotonic, never reused.
//
// Allocating is NOT publishing: the caller must call [Clock.PublishCommitTS]
// once the timestamp is stored in the transaction's commit record and its
// changes are therefore visible.
func (c *Clock) NextCommitTS() uint64 { return c.commit.Add(1) }

// PublishCommitTS announces that every change committed at ts is now visible.
//
// It does NOT simply raise the visible instant to ts. It records ts as finished
// and moves the instant to the newest timestamp below which nothing is still in
// flight, which is the same number only while commits are serialised. Publishing
// out of allocation order is the case this exists for: a reader must never be
// handed an instant that includes a commit but excludes an earlier one that has
// not finished yet. See [commitLog] for the shape and the prior art.
//
// Monotonic: the frontier only ever advances, and a late publisher whose
// timestamp is already behind it changes nothing.
func (c *Clock) PublishCommitTS(ts uint64) { c.finishCommitTS(ts) }

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
// Every allocation in the module publishes today (graph/lpg/mvcc_write.go:91,
// graph/lpg/mvcc_txn.go:133,171, graph/mvcc/stamp.go:148,163); the
// allocate-then-fail path arrives with write-write conflict detection
// (rmp #2300), which is what will call this.
func (c *Clock) AbandonCommitTS(ts uint64) { c.finishCommitTS(ts) }

// finishCommitTS marks ts finished and republishes the frontier.
//
// The visible store happens UNDER pubMu, not after it: two publishers releasing
// the lock and then racing to store their own frontier could otherwise land the
// older one last and move the visible instant backwards.
func (c *Clock) finishCommitTS(ts uint64) {
	c.pubMu.Lock()
	frontier := c.log.finish(ts)
	if frontier > c.visible.Load() {
		c.visible.Store(frontier)
	}
	c.pubMu.Unlock()
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
// [commitLog] on the publish path. Without that, writer B allocating 5 and
// finishing before writer A's 4 would hand a reader an instant containing 5 but
// not 4 — the same straddled commit described above, arrived at from the other
// direction.
//
// The cost of the read is unchanged, and that is the point of the shape chosen:
// one atomic load here, one comparison in [Visible]. See [commitLog] for the
// prior art and the trade it accepts.
func (c *Clock) ReadTS() uint64 { return c.visible.Load() }

// NextTxID allocates a transaction id, drawn from above [TxIDBase] so it can
// never be mistaken for a commit timestamp.
func (c *Clock) NextTxID() uint64 { return TxIDBase + c.txSeq.Add(1) }
