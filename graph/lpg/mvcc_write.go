package lpg

import (
	"context"
	"math/bits"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// mvcc_write.go — MVCC P4a (rmp #2288): one shared commit record per write, and
// the arming of the versioning substrate.
//
// # The defect this closes
//
// P1 to P3 built version chains in every store a read touches, but nothing
// armed them and no write allocated a commit record. A delta made outside a
// transaction therefore took its timestamp from [Graph.deltaStamp], which minted
// a FRESH one per delta. For a single-property write that is right — it is
// committed the instant it is made. For a multi-op statement it is wrong, and
// wrong in the way that matters most: `CREATE (a)-[:R]->(b)` would stamp the
// node, the edge and each property at different instants, so a reader could
// observe the node without the edge. Atomic visibility is the property the
// whole module rests on.
//
// # Where the record is allocated, and why there
//
// A write transaction already has exact brackets in this package —
// [Graph.ApplyAtomically] for one applied statement and
// [Graph.LockBarrier]/[Graph.UnlockBarrier] for an explicit multi-statement
// transaction — and those brackets already open and close the adjacency's
// commit window. The commit record is allocated and published on the same
// brackets, so a transaction's labels, properties, topology, relationship types
// and edge properties all point at ONE record and all become visible with one
// atomic store, however many stores they span.
//
// [Graph.ApplyInsideLocked] deliberately allocates NOTHING: it is the
// statement-inside-an-explicit-transaction path, and its statements must share
// the outer record or the transaction is not atomically visible.
//
// # Publication on rollback
//
// A rolled-back statement PUBLISHES its record rather than aborting it, and the
// reason is that GoGraph rolls back PHYSICALLY. The in-memory undo log
// (cypher/undo.go, #1282) replays inverse mutations through the same lpg
// mutators, so each inverse records its own delta on the same chain:
//
//	set L      stored: L present   chain: [undoRemove L]
//	inverse    stored: L absent    chain: [undoAdd L, undoRemove L]
//
// The stored value is already correct when the barrier is released. Committing
// the record makes the chain agree with it: a reader from before the statement
// walks both records and lands on the original value, and a reader from after
// it takes the stored value directly. Aborting the record would also be
// correct — every reader would undo both — but it would keep the chain alive
// past the point where anything needs it, and it would make the eager-reclaim
// signal ([mvcc.AbortedTS]) mean two different things. Pinned by
// TestLabelTx_ComposesWithPhysicalUndo.
//
// The substrate still publishes a rolled-back transaction its owner did not
// abandon, but the Cypher engine no longer leaves one un-abandoned: its explicit
// rollback (rmp #2973) and its failed autocommit statement (rmp #2976) call
// [WriteTx.Abandon] before the transaction closes. A published rollback is a
// commit after the snapshot of every older transaction, so first-updater-wins
// refuses such a transaction's later write to an object the rollback touched,
// although nothing it can see changed. That refusal outweighs the cost above.
//
// # Rollback is not ABORT (rmp #2300)
//
// The paragraph above is about a statement that ROLLED BACK: its inverses have run,
// so the stored value is already right and the chain nets out either way. It does
// NOT extend to a transaction that ABORTED — one refused by write-write conflict
// detection — and treating the two alike was an atomicity violation, measured at
// the substrate level:
//
//	transaction writes n.v = 1
//	transaction hits a conflict on its second write, which is refused
//	the bracket returns mvcc.ErrSerializationConflict
//	a FRESH SNAPSHOT then reads n.v = 1
//
// The caller was told the transaction failed and half of it was visible. It had not
// surfaced because the undo log is what rescues the Cypher path — and the undo log
// is a CYPHER structure, which a caller using [Graph.ApplyVersioned] directly (the
// durable store's apply among them) does not have. [Graph.endWrite] therefore
// ABORTS a doomed transaction instead of publishing it. See there.

// beginWrite opens the stamping window for one write transaction and RETURNS
// the state that transaction owns, or nil when the substrate is disarmed.
//
// It ALLOCATES NOTHING. The shared commit record is created by the first
// version that needs one, so a bracket that versions nothing — a read-only
// apply, or a write that changes no value — stays allocation-free, which
// [Graph.ApplyAtomically] is guarded to be. See [mvcc.WriteStamp].
//
// # The caller owns the returned state (rmp #2304)
//
// It returns the [writeCtx] rather than leaving the bracket to find it again on
// the graph, and every caller must hand the same value back to [Graph.endWrite]
// and [Graph.releaseWriterSnapshot]. Until rmp #2304 those two re-read the
// graph's slot, which named the caller's own transaction only because the
// exclusive barrier admitted one write bracket at a time. Once two brackets can
// overlap, re-reading the slot means closing SOMEONE ELSE's transaction:
// [mvcc.WriteStamp.EndFor] documents what that loses on the stamping side, and
// [Graph.releaseWriterSnapshot] what it loses on the snapshot side — where it is
// worse, because it recycles a live writer's state underneath it.
//
// # Who may own the ambient slot (rmp #2947)
//
// publish says whether the transaction also claims the graph's AMBIENT slot —
// [Graph.stamp]'s open transaction and [Graph.writeTx] — through which a write
// that carries no transaction ([Graph.deltaStamp] with a nil record,
// [Graph.AmbientWriteTx]) resolves one.
//
// Only an EXCLUSIVE bracket publishes: [Graph.ApplyAtomically],
// [Graph.ApplyAtomicallyTx] and [Graph.LockBarrierCtx]. Each holds the schema
// barrier exclusively, so it is the only write bracket open, and the slot can
// name no one else. It serves [Graph.ApplyInsideLockedTx]. A direct Go-API call
// never resolves through it: it runs as an implicit transaction of its own,
// inside a bracket or not (rmp #2947, audit F7). Nor does a raw adjacency write
// made through [Graph.AdjList]: it too is its own transaction (rmp #2967; see
// [adjlist.AdjList.SetWriteStamp]).
//
// A SHARED bracket ([Graph.ApplyVersioned]) or an explicit transaction
// ([Graph.BeginVersionedTx]) does not. Several can be open at once, so the slot
// would name whichever published last; and an explicit transaction stays open
// across client round-trips. Publishing either one made every direct write
// anywhere in the process — on an object the transaction never touched — join
// it: invisible to new readers until the transaction ended, and rolled back by
// its undo. Both thread their [writeCtx] through every write instead (rmp
// #2320), so neither needs the slot.
//
// Nested calls remain forbidden: a nested bracket would be a nested transaction,
// which this design has no meaning for. [Graph.ApplyInsideLocked] deliberately
// does not call this, and the re-entrancy guard catches the rest under -race or
// -tags gograph_debug.
func (g *Graph[N, W]) beginWrite(publish bool) *writeCtx {
	if !g.mvccArmed {
		return nil
	}
	// The writer reads through a snapshot of its OWN (rmp #2299): as of the
	// instant it began, PLUS the versions it has written itself, which is
	// exactly what the ts == txID branch of [mvcc.Visible] delivers. Before
	// this the write path read the present (ReadAt(nil)), which is correct only
	// while a barrier guarantees there is no other writer whose uncommitted
	// work "the present" could contain.
	//
	// Registered with the horizon in the same order a reader is — slot first,
	// then clock, then publish — because a reclaimer landing between the clock
	// read and the registration would compute a watermark past this writer's
	// start timestamp and free versions it is about to read. See
	// [mvcc.Horizon.EnterHolding]. Until rmp #2299 the horizon covered readers
	// only, which was sound only because the writer read no snapshot at all
	// (audit finding E22).
	slot := g.horizon.EnterHolding()
	startTS := g.mvccClock.ReadTS()
	g.horizon.Publish(slot, startTS)
	// The start timestamp is read BEFORE the id is minted, matching
	// [Graph.beginWriteCtx] and [Graph.beginLabelTx], so a transaction can never
	// see a commit that happened after it began. It is the reverse of what this
	// path did until rmp #2301, when the id came from the stamp's own Begin.
	txID := g.nextTxID()
	// PER-TRANSACTION state, recycled so the bracket still allocates nothing
	// (rmp #2301, audit finding E3). Everything mutable a write transaction owns
	// — its commit record, its version count, its snapshot — lives on this
	// object; the graph keeps only a slot naming it.
	w := g.acquireWriteCtx(startTS, txID)
	w.snap.slot = slot
	if publish {
		g.stamp.Publish(&w.tx)
		g.writeTx.Store(w)
	}
	// The writer gauge (rmp #2312). Paired with the EndWriter in
	// [Graph.releaseWriterSnapshot], which every bracket reaches — including the ones
	// that version nothing, which [Graph.endWrite] returns early from. Both carry the
	// same transaction id, so both hit the same counter stripe and the per-stripe
	// value can never go negative.
	g.writeCounts.BeginWriter(txID)
	return w
}

// writerView returns the graph bound to the current writer's snapshot, or to
// the present when no write transaction is open.
//
// It is what the write path reads through. A nil snapshot means "the current
// stored value", which is the right answer outside a transaction and the wrong
// one inside one the moment a second writer exists.
func (g *Graph[N, W]) writerView() *ReadView[N, W] {
	return g.ReadAt(g.writerSnapshot())
}

// writerSnapshot returns the snapshot of the write transaction whose bracket is
// currently open, or nil when there is none.
//
// The pointer is into the transaction's own state, so it is valid only while the
// bracket is open — which is the only window any caller has a use for it in.
func (g *Graph[N, W]) writerSnapshot() *Snapshot {
	w := g.writeTx.Load()
	if w == nil {
		return nil
	}
	return &w.snap
}

// WriteTx names one open write transaction to a caller in another package.
//
// It is what [Graph.ApplyVersioned] hands its closure, and what the Cypher
// engine's write path carries so that its reads resolve through its OWN
// transaction rather than through whichever transaction the graph's slot happens
// to name (rmp #2304). Memgraph threads `Transaction *transaction` into every
// accessor for the same reason (memgraph/memgraph, branch master, read
// 2026-08-02; src/storage/v2/).
//
// The zero value names no transaction, which reads as "the present" — correct
// for a direct mutation outside any transaction, and wrong inside one, so a
// caller inside a bracket must pass the value it was given rather than the zero.
//
// It is valid only while its bracket is open, and it must not be retained past
// it: the state it names is recycled on the unwind.
type WriteTx struct{ w *writeCtx }

// CreatedNodes appends to dst the ids of the nodes this transaction CREATED —
// ids it interned first, and unborn ids it revived (keys whose only earlier
// creation aborted) — in creation order, and returns the extended slice. An id
// whose creation a statement rollback later withdrew is still listed: the key
// was bound to it, and the durable record must say so (WAL v2 step 3,
// docs/design-wal-v2.md §5.5). The zero value lists nothing.
//
// It must be called while the transaction is still open (before
// [Graph.EndVersionedTx] or the end of the bracket), because the state behind
// tx is recycled afterwards. Safe for concurrent use with writes through tx.
func (tx WriteTx) CreatedNodes(dst []graph.NodeID) []graph.NodeID {
	if tx.w == nil {
		return dst
	}
	tx.w.createdMu.Lock()
	dst = append(dst, tx.w.created...)
	tx.w.createdMu.Unlock()
	return dst
}

// Valid reports whether tx names an open write transaction.
//
// It is false for the zero value and for a bracket opened on a graph whose
// versioning substrate is disarmed (see [Graph.disarmMVCCForTest]), where there is no
// transaction to name and every write is committed as it is made.
func (tx WriteTx) Valid() bool { return tx.w != nil }

// StartTS returns the instant this transaction reads at: every commit at or below
// it is visible to the transaction, and no commit above it is. It is 0 for the
// zero value, which names no transaction.
//
// It exists so a caller holding a structure maintained OUTSIDE the versioned
// stores — a secondary index, which is written at commit time and read at the
// present — can prove that the structure describes this transaction's snapshot
// before trusting it as an access path (rmp #2812).
func (tx WriteTx) StartTS() uint64 {
	if tx.w == nil {
		return 0
	}
	return tx.w.startTS
}

// CommitApplier is work that must observe a transaction's commit exactly as
// committed, before any reader can: the engine's secondary-index fan-out
// (rmp #2931).
//
// # Why it runs inside the publication, and under the node-shard locks
//
// A secondary index is maintained from the changes a transaction recorded, but
// several of those changes also need a fact about the node that the change does
// not carry — whether the node is live and labelled, and its current value of the
// indexed property. Read from the graph's present, that fact includes OTHER
// transactions' eager, uncommitted writes, and an index entry derived from one
// outlives that transaction when it rolls back. Read from the committing
// transaction's own snapshot instead, it misses the commits that landed after the
// snapshot was taken. Neither is the state the commit produces.
//
// [Graph.endWrite] therefore runs the applier BEFORE it stamps the commit record,
// and hands it a snapshot that sees every STAMPED commit, its own writes, and
// nothing else in flight. After the apply the record is marked ready, stamped and
// published. On the in-memory path the instant is allocated only then, after the
// apply, so an applier holds back no one's frontier; on the WAL path it was
// allocated before the fsync ([Graph.AllocateCommitTS]) and is held, not ready,
// across the apply. An
// index entry describes one node, so what must be ordered is the appliers that
// touch a COMMON node: the applier, the stamp and the publication run holding the
// lock of every node shard the applier names ([CommitApplier.CommitApplyShards]),
// taken in ascending order. Two appliers on a common node therefore run one after
// the other, the second seeing the first's commit; appliers on disjoint nodes run
// in parallel and cannot affect each other's entries. The result is the committed
// state whatever the interleaving. No reader can observe the gap: the instant is
// not yet published, so no snapshot can start at or after it. A transaction that
// registers no applier pays nothing.
//
// # Nodes no other transaction can name need no lock
//
// A node BORN in the committing transaction, and not alive before it
// ([CommitNodes.Private]), is invisible to every other transaction until this
// one publishes, so no concurrent transaction can have written it and no
// concurrent applier can name it; a transaction that names it later started
// after this publication, which follows this apply. Its shard is therefore left
// out of the set. A bulk CREATE names only such nodes and takes no lock at all.
// Measured before this rule (rmp #2931 audit, F3): 256 writers of 128-row
// UNWIND…CREATE batches on an indexed label over the WAL spent 88% of their
// blocked time in these locks, queued behind one another holding allocated,
// not-ready instants, and lost 14.6% of HEAD's throughput.
type CommitApplier interface {
	// CommitApplyShards returns the set of node shards the applier's work
	// concerns, one bit per [CommitApplyShard] value, for every node whose state
	// the applier will read or whose index entries it will write and that nodes
	// does not report private. Zero means no shard lock is needed.
	CommitApplyShards(nodes CommitNodes) uint64
	// ApplyCommitted runs once, holding the named shard locks, before the
	// transaction's commit record is stamped. On the in-memory path the commit
	// instant is not yet allocated; on the WAL path it was allocated before the
	// fsync and is not yet ready. snap sees every commit stamped so far plus this
	// transaction's own writes, and nothing else uncommitted; it is valid only
	// for the duration of the call.
	//
	// A panic out of this method or out of CommitApplyShards is not
	// recovered: it propagates to the caller of the transaction's end, and the
	// state of whatever the applier maintains is undefined afterwards — the
	// applier must record that itself (the engine fail-stops; see
	// [index.Manager.MarkUndefined]). On the way out the TRANSACTION is settled
	// so that memory and the durable log agree, and so the frontier is not
	// stalled (rmp #2931 re-audit, N2):
	//
	//   - On the WAL path its commit record is already durable — the applier is
	//     registered only after [Graph.AllocateCommitTS]'s instant has been
	//     fsynced — and recovery will replay it as committed. It is therefore
	//     PUBLISHED, without the applier's work, never aborted: aborting it in
	//     memory would make the running process and a reopened one disagree.
	//   - On the in-memory path nothing is durable and no instant is allocated,
	//     so it is ABORTED exactly as a doomed transaction is: its versions become
	//     permanently invisible and are handed to the reclaimer, so the nodes it
	//     wrote are writable again.
	ApplyCommitted(snap *Snapshot)
	// Committed runs right after the instant ts is allocated, stamped and
	// published, still holding the locks.
	Committed(ts uint64)
	// DiscardCommitted runs instead of both when the transaction publishes
	// nothing: it aborted, or it versioned nothing and so changed no state.
	DiscardCommitted()
}

// commitApplyShards is the number of node shards [CommitApplier] work is ordered
// by. 64, so a shard set is one uint64.
const commitApplyShards = 64

// CommitApplyShard returns the shard, in [0, 64), that orders commit-time work
// concerning node id; see [CommitApplier]. Fibonacci hashing, so node ids that
// share low bits — an adjacency that encodes its shard there — still spread.
func CommitApplyShard(id graph.NodeID) uint {
	return uint((uint64(id) * 0x9E3779B97F4A7C15) >> 58)
}

// CommitNodes answers, for the transaction being published, which of the nodes
// its changes name no other transaction can name; see [CommitApplier]. The zero
// value reports no node private. It is valid only during
// [CommitApplier.CommitApplyShards].
type CommitNodes struct {
	life *[propMapShards]nodeLifeShard
	info *commitInfo
}

// Private reports whether node id was born in the committing transaction and was
// not alive immediately before it: created by it, or revived by it from a
// committed death. Such a node is invisible to every other transaction until
// this one publishes. A node the transaction deleted and then restored — the
// undo of a rolled-back DELETE — was alive before it and is not private.
//
// Safe for concurrent use.
func (c CommitNodes) Private(id graph.NodeID) bool {
	if c.life == nil || c.info == nil {
		return false
	}
	sh := &c.life[uint64(id)&(propMapShards-1)]
	sh.mu.RLock()
	born, ok := sh.born[id]
	sh.mu.RUnlock()
	return ok && born.info == c.info && !born.wasAlive
}

// commitApplyShard is one shard lock of [Graph.commitApply] and the snapshot an
// applier whose lowest shard it is reads through, alone on its cache lines.
type commitApplyShard struct {
	mu   sync.Mutex
	snap Snapshot
	memo snapMemo // snap's verdict memo; see [Snapshot.memo]
	_    [64]byte
}

// SetCommitApplier registers a to run when this transaction's bracket publishes
// it (see [CommitApplier]). At most one applier is held; a second call replaces
// the first. A no-op on the zero value, whose writes are committed as they are
// made.
func (tx WriteTx) SetCommitApplier(a CommitApplier) {
	if tx.w == nil {
		return
	}
	tx.w.applier = a
}

// Err returns the serialization conflict this transaction has been doomed by, or
// nil when it is still viable. The error wraps [mvcc.ErrSerializationConflict] and
// carries the store attribution through [mvcc.Conflict].
//
// # Why an embedder needs this and cannot do without it
//
// Most primitives report a conflict by returning it, so a caller learns at the
// statement. But a conflict hit by a primitive that CANNOT return an error — a
// label removal, a property delete, any of the five per-edge side stores — is
// recorded on the transaction instead, and the only way to observe it is to ask
// (rmp #2300).
//
// [labelTx.commit] asks, which is what makes the substrate's own transactions
// safe. An embedder that drives the write bracket itself — cypher's ExplicitTx and
// its autocommit path both do — must ask too, and BEFORE it makes anything
// durable, or a transaction whose only conflicting write went through such a
// primitive commits successfully having dropped it. That is a lost update with
// nothing anywhere reporting it, and it is what rmp #2354 measured: a `REMOVE
// n:Label` that collided with a peer's uncommitted removal returned nil from the
// statement AND nil from Commit, and the label was still there afterwards.
//
// Memgraph reads the same record in the same place — Storage::Commit tests
// `transaction_.must_abort` and returns SerializationError
// (src/storage/v2/storage.cpp).
//
// Nil on the zero value, so a caller can ask unconditionally.
func (tx WriteTx) Err() error { return tx.w.err() }

// Versions returns how many versions this transaction has written so far, and
// whether the count is meaningful. ok is false for the zero value and for a
// bracket on a graph whose versioning substrate is disarmed, where writes leave
// no version to count.
//
// # What the count is for — effect logging (rmp #2965, round 5)
//
// On an armed graph every change a write makes is a version: the version is the
// pre-image an abort restores and the claim that refuses every other writer
// until this transaction ends. A write that changes nothing — re-asserting a
// present label, setting a property to the value it already holds, deleting
// what is absent, creating a live node — writes no version and therefore holds
// no claim. Sampling the count before and after one operation tells a durable
// caller whether that operation took effect, so it can log only the operations
// that did. A logged operation then always holds a claim, which is what makes
// WAL order agree with the in-memory order of every change.
//
// The count only grows during the transaction. It is not safe to compare across
// transactions.
func (tx WriteTx) Versions() (n int64, ok bool) {
	if tx.w == nil {
		return 0, false
	}
	return tx.w.tx.Versions(), true
}

// Abandon marks this transaction so that closing it with [Graph.EndVersionedTx]
// (or [Session.EndVersionedTx]) ABORTS it instead of publishing it: none of its
// versions ever becomes visible, it is counted in [mvcc.WriteCounts] Aborts and
// not in Commits, and a commit timestamp allocated for it is abandoned.
//
// # Why a rollback must abort rather than publish (rmp #2973)
//
// An embedder that rolls a multi-statement transaction back PHYSICALLY — the
// Cypher engine replays its undo log through the ordinary mutators — leaves the
// stored values right either way. Publishing the record, however, makes it a
// COMMIT whose instant postdates the snapshot of every transaction that began
// earlier, so first-updater-wins refuses such a transaction when it later writes
// an object the rolled-back one touched, although nothing it can see changed.
// Aborting the record is what PostgreSQL and InnoDB do on ROLLBACK, and it is
// what [Graph.endWrite] already does for a doomed transaction.
//
// # Preconditions
//
// Call it only BEFORE the transaction is closed; on a closed transaction it is
// meaningless and must not be called. Never call it on a path whose commit record
// is already DURABLE — after the WAL fsync of [Graph.AllocateCommitTS]'s instant
// — because recovery replays such a transaction as committed, and aborting it in
// memory would make the running process and a reopened one disagree.
//
// A no-op on the zero value. Not safe for concurrent use: like every other
// operation on one write transaction, it must be called from the goroutine that
// drives it.
func (tx WriteTx) Abandon() {
	if tx.w != nil {
		tx.w.abandon = true
	}
}

// EnterUndo marks the start of this transaction's PHYSICAL undo replay, during
// which its writes are withdrawals of work it already applied rather than new
// updates.
//
// It must be paired with exactly one [WriteTx.ExitUndo], and the region must
// cover the whole replay. Inside it, a write is no longer refused merely because
// the transaction is doomed — which it always is when an undo has to run — while
// the per-object head test still applies, so an inverse can withdraw this
// transaction's own versions and nothing else. [writeCtx.undoing] carries the
// full reasoning, the prior art and the lost update this closes.
//
// Both are no-ops on the zero value, so a caller can bracket unconditionally.
//
// The region must not be entered concurrently from two goroutines, which is
// already the contract for driving one write transaction.
func (tx WriteTx) EnterUndo() {
	if tx.w != nil {
		tx.w.undoing.Store(true)
	}
}

// ExitUndo ends the region [WriteTx.EnterUndo] opened, restoring the ordinary
// rule that a doomed transaction refuses further writes.
func (tx WriteTx) ExitUndo() {
	if tx.w != nil {
		tx.w.undoing.Store(false)
	}
}

// WriterView is [Graph.writerView] for a caller in another package — the Cypher
// engine's write path, which must read as of the writing transaction rather
// than as of the present.
//
// It resolves the transaction through the graph's slot, so it answers with
// whichever write bracket published LAST. That is the caller's own only while at
// most one bracket is open at a time; prefer [Graph.WriterViewOf], which cannot
// be wrong. This form is kept for the explicit-transaction path, which holds the
// barrier exclusively and therefore is the only open bracket by construction.
//
// Safe for concurrent use; the returned view is immutable.
func (g *Graph[N, W]) WriterView() *ReadView[N, W] { return g.writerView() }

// WriterViewOf returns the graph as write transaction tx reads it: as of the
// instant tx began, plus the versions tx has written itself.
//
// This is the form the ordinary write path must use. The snapshot comes from the
// transaction the caller was HANDED rather than from the graph's slot, so a
// concurrent writer that opened its own bracket in between cannot substitute its
// snapshot for this one — which is the whole difference rmp #2304 turns on, and
// the same lesson rmp #2301 learned one level down: reading the writer's identity
// off the graph produced a FALSE conflict between goroutines writing disjoint
// nodes (see graph/lpg/mvcc_writectx.go).
//
// A zero tx reads the present, which is the correct answer outside a transaction.
//
// Safe for concurrent use; the returned view is immutable.
func (g *Graph[N, W]) WriterViewOf(tx WriteTx) *ReadView[N, W] {
	if tx.w == nil {
		return g.ReadAt(nil)
	}
	return g.ReadAt(&tx.w.snap)
}

// LatestViewOf returns a view of the LATEST committed state — every commit
// stamped so far, whether or not its instant is published yet — with tx's own
// writes when withOwn is set and without them otherwise. It is the state a
// commit by tx will leave behind, read before (without) or after (with) tx's
// writes land, as opposed to tx's snapshot, which cannot see a commit made after
// tx began.
//
// It exists for commit-time constraint validation of a transaction open across
// a new constraint (rmp #2936). The versions it reads are the newest of each
// chain, which reclamation never removes. A zero tx reads the present.
//
// Safe for concurrent use.
func (g *Graph[N, W]) LatestViewOf(tx WriteTx, withOwn bool) *ReadView[N, W] {
	if tx.w == nil {
		return g.ReadAt(nil)
	}
	var own uint64
	if withOwn {
		own = tx.w.txID
	}
	return g.ReadAt(newSharedSnapshot(mvcc.TxIDBase-1, own, 0))
}

// AmbientWriteTx returns the write transaction the graph's slot currently names,
// for a caller that holds the barrier EXCLUSIVELY and is therefore the only open
// bracket — the explicit-transaction path, which opens its transaction in
// [Graph.LockBarrier] and runs its statements through
// [Graph.ApplyInsideLocked] later, with no closure to carry the handle in.
//
// Any other caller must use the handle [Graph.ApplyVersioned] gave it. This one
// is correct by virtue of the exclusive hold and by nothing else.
func (g *Graph[N, W]) AmbientWriteTx() WriteTx { return WriteTx{w: g.writeTx.Load()} }

// RestoreMVCCClock raises this graph's MVCC clock so every instant it subsequently
// allocates, and every instant a new reader starts at, is at or above floor. It
// never lowers the clock.
//
// It is a no-op when the versioning substrate is disarmed, so a recovery path may
// call it unconditionally.
//
// # Why the clock is restored at all, and why by derivation (rmp #2309)
//
// [mvcc.Clock] is process-local and constructed at zero on every open. Nothing
// persists it, deliberately: two of the three reference engines removed their
// persisted counter and derive instead — InnoDB folds a max over the rollback
// segments at startup, Memgraph derives max(delta_ts)+1 from the WAL. A second
// durable source of truth is one that can disagree with the log after a torn tail.
//
// So recovery reads the largest commit timestamp the WAL actually carries and hands
// it here as a floor. Without it a reopened graph re-mints instants a previous
// process already published and made durable, and a reader could reach a version
// that is simultaneously in its past and its future.
//
// # It must be called before the graph has readers or writers
//
// It moves the visible frontier as well as the allocation counter, which is sound
// only because recovery has no commits in flight: every transaction in the file
// either reached its durable marker or went with the torn tail. See
// [mvcc.Clock.RatchetTo].
//
// Not safe for concurrent use.
func (g *Graph[N, W]) RestoreMVCCClock(floor uint64) {
	if !g.mvccArmed {
		return
	}
	g.mvccClock.RatchetTo(floor)
}

// AllocateCommitTS reserves this transaction's commit timestamp WITHOUT making it
// visible, and returns it. It is idempotent: a second call returns the same value.
//
// It returns zero — meaning "no timestamp" — for the zero transaction and for a
// graph whose versioning substrate is disarmed, so a durable caller may invoke it
// unconditionally and encode whatever it gets.
//
// # What it is for (rmp #2309)
//
// A durable writer must put the commit instant INTO the WAL record, because the
// MVCC clock is restored at recovery by deriving it from the WAL rather than by
// trusting a persisted counter. That is impossible if the instant is minted after
// the record is written, which is what [Graph.endWrite] used to do — it runs from
// the caller's deferred teardown, strictly after the append and the fsync.
//
// So the sequence becomes:
//
//	AllocateCommitTS → encode the OpCommit marker → fsync → EndVersionedTx (publish)
//
// which is PostgreSQL's ordering: the XID is assigned before XLogFlush, the flushed
// record carries it, and only then is the commit marked visible.
//
// # The caller's obligation, and why it is discharged elsewhere
//
// An allocated timestamp MUST eventually be published or abandoned. One that is
// neither stalls the contiguous commit frontier permanently — every later commit
// becomes invisible to new readers, and the commit log grows without bound.
//
// The caller does NOT discharge it directly. [Graph.endWrite] does, on every path:
// it publishes on success and abandons on abort and on the versioned-nothing case.
// That is deliberate — a discharge placed beside each caller is one a new caller can
// forget, and the failure mode is a silent, permanent stall rather than a crash. So
// the only obligation here is the one that already existed: call
// [Graph.EndVersionedTx] exactly once per transaction.
//
// # It lengthens the in-flight window, on purpose
//
// Between this call and the publish sits a WAL fsync, so a transaction now holds an
// unpublished timestamp for milliseconds rather than nanoseconds, and ONE in-flight
// commit holds the frontier back for every reader. That cost is real and it is
// observable: MVCCStats.InFlightCommits is the measure.
//
// Safe for concurrent use; each goroutine must pass its own transaction.
func (g *Graph[N, W]) AllocateCommitTS(tx WriteTx) uint64 {
	if !g.mvccArmed || tx.w == nil {
		return 0
	}
	if tx.w.commitTS == 0 {
		// Registered with the transaction's record but NOT ready: the WAL record
		// is not durable yet, so no helper may publish it (rmp #2932). The bracket
		// marks it ready in [Graph.endWrite], after the fsync.
		// A transaction that has versioned nothing yet has no record: register an
		// anonymous one and keep it, so its discharge passes it to the clock
		// instead of looking it up after a later lap may have displaced it.
		rec := tx.w.tx.OpenRecord()
		if rec == nil {
			rec = new(mvcc.CommitInfo)
		}
		tx.w.allocRec = rec
		tx.w.commitTS = g.mvccClock.AllocateFor(rec, false)
	}
	return tx.w.commitTS
}

// AwaitAllocatedCommits blocks until every commit timestamp allocated before the
// call has been published or abandoned — until a snapshot started afterwards sees
// every commit that had allocated by then — or until ctx finishes, returning ctx's
// error. Unlike [Graph.AwaitCommitQuiescence] it does not wait for commits that
// allocate after the call, so a steady stream of commits cannot starve it.
//
// It is what an index build uses, once no further commit can escape its
// recording, to make its snapshot include every commit that could (rmp #2936).
// It returns immediately on a graph whose versioning substrate is disarmed.
//
// Safe for concurrent use.
func (g *Graph[N, W]) AwaitAllocatedCommits(ctx context.Context) error {
	if !g.mvccArmed {
		return nil
	}
	return g.mvccClock.AwaitVisible(ctx, g.mvccClock.Allocated())
}

// AwaitCommitQuiescence blocks until every commit timestamp this graph has allocated
// has been published or abandoned — until [MVCCStats.InFlightCommits] would read zero
// — or until ctx finishes.
//
// It is the counterpart obligation to [Graph.AllocateCommitTS], and it exists for
// the one observer that cannot tolerate the window that method opens: a durable
// checkpoint, which pairs a WAL DURABILITY position with an MVCC VISIBILITY position
// and truncates the WAL prefix the first one names. A transaction between its fsync
// and its publish sits below the durable offset and above the visible frontier, so
// the image does not carry it and the truncation destroys it — an acknowledged
// commit lost (rmp #2349). Waiting here makes the two positions describe the same
// set of transactions.
//
// The wait is on the OBSERVER, never on the committer: a writer that is not observed
// pays nothing, which is the whole reason the durability and visibility steps are not
// held under one lock. See [mvcc.Clock.AwaitQuiescent] for the prior art this follows
// and for the reference engine that chose the other route.
//
// It returns immediately on a graph whose versioning substrate is disarmed, which
// allocates no timestamps at all.
//
// Concurrency: safe for concurrent use. It takes no lock the write path takes, so a
// caller may hold a write-admission gate closed across it — and the intended caller
// does, which is what bounds the wait.
func (g *Graph[N, W]) AwaitCommitQuiescence(ctx context.Context) error {
	if !g.mvccArmed {
		return nil
	}
	return g.mvccClock.AwaitQuiescent(ctx)
}

// abandonAllocatedCommitTS discharges a commit timestamp that was allocated by
// [Graph.AllocateCommitTS] and will never be published, and clears it so a second
// discharge cannot double-count.
//
// A no-op when nothing was allocated, which is every non-durable path.
func (g *Graph[N, W]) abandonAllocatedCommitTS(w *writeCtx) {
	if w.commitTS == 0 {
		return
	}
	g.mvccClock.AbandonCommit(w.allocRec, w.commitTS)
	w.commitTS, w.allocRec = 0, nil
}

// endWrite publishes every version the write transaction w created, atomically,
// and closes its stamping window.
//
// One store, however many versions there are across however many stores: that
// is the whole reason the record is shared. Publication is a single atomic store
// of the commit timestamp into the shared record, so the instant a transaction
// becomes visible is one instant however many structures it spanned — which is
// what took over from the exclusive barrier at rmp #2304 and is why removing the
// barrier moves no visibility boundary.
//
// w must be the value [Graph.beginWrite] returned for this bracket; see
// [mvcc.WriteStamp.EndFor] for what closing the graph's slot instead cost once
// two brackets could overlap.
//
// It publishes on the ROLLBACK path too; see the file comment for why.
//
// # It does NOT publish on the ABORT path (rmp #2300)
//
// A transaction that hit a write-write conflict is ABORTED rather than published:
// its record is marked [mvcc.AbortedTS], so every reader undoes its versions
// forever and the pre-transaction value is what they land on.
//
// Rollback and abort are different, and conflating them was an ATOMICITY
// violation — measured, at the substrate level, before this branch existed:
//
//	transaction writes n.v = 1
//	transaction hits a conflict on its second write, which is refused
//	the bracket returns mvcc.ErrSerializationConflict
//	a FRESH SNAPSHOT then reads n.v = 1
//
// The caller was told the transaction failed and half of it was visible anyway.
// The reason it did not surface earlier is that the Cypher engine has an undo log
// which physically restores the stored value (cypher/undo.go), so on that path the
// chain nets out and publication is harmless — which is exactly what the file
// comment above describes. But the undo log is a CYPHER structure: a caller using
// [Graph.ApplyVersioned] directly has none, and the durable store's apply
// (store/txn) is such a caller. The substrate has to be atomic on its own.
//
// Aborting is also the better answer where the undo log DOES run. The file comment
// notes that aborting "would also be correct — every reader would undo both"; it
// was not chosen because it keeps the chain alive past the point anything needs it.
// That cost is real and it is rmp #2318's to reclaim; it does not justify leaving a
// failed transaction partly visible.
//
// A commit timestamp allocated by [Graph.AllocateCommitTS] before the WAL fsync IS
// abandoned on this path, via [mvcc.Clock.AbandonCommitTS] — the shape that call
// exists for. Before rmp #2309 no path allocated one early, so there was nothing to
// abandon; there is now, and failing to would stall the frontier permanently.
// It returns the instant the transaction published at, or zero when it published
// none — a transaction that versioned nothing, and one that aborted. That value is
// what a [Session] records as its read floor (rmp #2328), and it is returned from
// here rather than read off the transaction afterwards because the state is RECYCLED
// on the unwind: by the time a caller could look, the record is gone and the object
// may already belong to another bracket.
func (g *Graph[N, W]) endWrite(w *writeCtx) uint64 {
	if !g.mvccArmed || w == nil {
		return 0
	}
	info, created := g.stamp.EndFor(&w.tx)
	applier := w.applier
	w.applier = nil
	if info == nil {
		if applier != nil {
			applier.DiscardCommitted()
		}
		// The transaction versioned nothing, so there is no record to publish,
		// nothing to reclaim, and no reason to allocate a commit timestamp.
		//
		// It may nonetheless HAVE one, allocated before the WAL fsync by
		// [Graph.AllocateCommitTS] (rmp #2309). Abandon it: a timestamp that is
		// neither published nor abandoned stalls the contiguous frontier forever,
		// which makes every later commit invisible to new readers and grows the
		// commit log without bound.
		g.abandonAllocatedCommitTS(w)
		// It may ALSO have failed: a transaction doomed by its first write records no
		// version, so it arrives here rather than at the abort branch below. It is
		// still a refusal and it is counted as one (rmp #2312) — an abort the substrate
		// does not count is a failure an operator cannot see, and it would leave
		// Commits+Aborts short of the transactions that actually reached an outcome.
		if w.err() != nil || w.abandon {
			g.writeCounts.Abort(w.txID)
		}
		return 0
	}
	// A transaction that hit a serialization conflict ABORTS. See below for the
	// measured atomicity violation that this closes, and why it is not the same
	// thing as the rolled-back-statement case the file comment describes. So does
	// one its owner abandoned ([writeCtx.abandon]): a durable apply whose WAL
	// record was refused or never became durable.
	if w.err() != nil || w.abandon {
		if applier != nil {
			applier.DiscardCommitted()
		}
		adjFreed := g.abortRecord(&w.tx, info)
		// Counted here, where publication is REFUSED, because Commits and Aborts are
		// the two outcomes and must partition the transactions that reached one. The
		// conflict that caused it is counted separately, at the detection site, and is
		// a SUBSET of this — a cause, not a third outcome (rmp #2312).
		g.writeCounts.Abort(w.txID)
		// Same obligation as the versioned-nothing branch above: an aborted
		// transaction's allocated timestamp is never published, so it must be
		// abandoned or the frontier stalls on it.
		g.abandonAllocatedCommitTS(w)
		// Charged AND woken unconditionally: the version records exist and occupy
		// memory whatever their commit record says, and until the sweep withdraws
		// them the stored value still carries this transaction's writes (rmp #2318).
		g.abortWake(created-adjFreed, &w.tx)
		return 0
	}
	// Allocate, store into the shared record, THEN publish. A reader must never
	// start at a timestamp whose commit is still between the first two steps;
	// see [mvcc.Clock.ReadTS] for the torn read that caused.
	//
	// The allocation may already have happened, in [Graph.AllocateCommitTS], for a
	// durable transaction that had to put its timestamp INTO the WAL record before
	// the fsync (rmp #2309). Reusing it is what makes the durable record and the
	// visible instant the same number; minting a second one here would make the
	// derived clock floor disagree with what actually became visible.
	var ts uint64
	if applier != nil {
		ts = g.commitAndApply(info, w, applier, created)
	} else {
		ts = g.allocateReady(info, w)
		info.Commit(ts)
		g.mvccClock.PublishCommit(w.registeredRecord(info), ts)
	}
	w.commitTS, w.allocRec = 0, nil
	// A transaction that got this far PUBLISHED an instant, which is the only
	// definition of "committed" the substrate has (rmp #2312). The versioned-nothing
	// branch above is deliberately NOT counted: it published no instant, so counting
	// it would put commits above the number of instants the clock ever allocated and
	// make the conflict rate's denominator meaningless.
	g.writeCounts.Commit(w.txID)
	published := ts
	// Accounting only. The sweep itself moved off this path at rmp #2308: a
	// committer charges its versions and, once per [reclaimThreshold], wakes the
	// background vacuum. It no longer sweeps, so a commit's cost no longer
	// depends on how much garbage other transactions left behind.
	g.chargeReclaimDebt(created)
	return published
}

// releaseWriterSnapshot closes write transaction w's read view, returns its
// horizon slot and recycles its per-transaction state.
//
// Split from [Graph.endWrite] because endWrite returns early when the
// transaction versioned nothing, and a slot must be returned whether or not
// anything was written — a bracket that writes nothing still took one.
//
// It must run AFTER endWrite: the state is recycled here, so the record must
// already have been published from it.
//
// # w is a parameter, not a slot read (rmp #2304)
//
// Until rmp #2304 this swapped the graph's slot and released whatever it found.
// With one write bracket at a time that was always the caller's own; with two it
// is the most damaging of the three slot hazards, because it does not merely
// mis-attribute state — it hands a LIVE writer's [writeCtx] to the free list
// while that writer is still reading through the snapshot inside it, and returns
// a horizon slot the live writer still needs, so the versions it is about to read
// become reclaimable underneath it.
//
// The slot is cleared only if it still names w, for the same reason
// [mvcc.WriteStamp.EndFor] clears conditionally.
func (g *Graph[N, W]) releaseWriterSnapshot(w *writeCtx) {
	if !g.mvccArmed || w == nil {
		return
	}
	g.writeTx.CompareAndSwap(w, nil)
	g.horizon.Leave(w.snap.slot)
	// BEFORE the state is recycled: the id is read off w, and after releaseWriteCtx
	// another bracket may already own it (rmp #2312).
	g.writeCounts.EndWriter(w.txID)
	g.releaseWriteCtx(w)
	// NO DRAIN WAKE HERE, deliberately (rmp #2308). A writer holds the horizon back
	// exactly as a reader does (rmp #2299), so its departure does advance the
	// watermark — but the versions it made were already charged to the reclamation
	// debt, so the CHURN signal already accounts for them and a second signal here
	// would only make the sweep run sooner, not more completely.
	//
	// Sooner is not free: with no reader registered the watermark is the clock
	// itself, so a wake on every write transaction's release means one sweep pass
	// per COMMIT rather than one per [reclaimThreshold] versions. That is the
	// amortisation the debt counter exists to provide, and paying it on a
	// background goroutine instead of on the committer does not make it cost
	// nothing — it spends a core the write path wants.
	//
	// The drain wake belongs where the churn signal cannot reach: a READER's
	// departure, which releases versions nothing has charged. See
	// [Graph.EndRead].
}

// armMVCC arms the whole versioning substrate: node labels, node properties and the
// adjacency — which carries the per-slot relationship types and the columnar edge
// properties inside the same immutable entry, so versioning the entry versions all
// three.
//
// It is called once, by [New]. There is NO WAY TO DISARM IT (rmp #2311).
//
// # Why the public switch is gone
//
// Graph.DisableMVCC / EnableMVCC / MVCCEnabled were an exported switch that turned
// versioning OFF at runtime. They directly contradicted the sprint's mandate that MVCC
// is the module's ONLY concurrency-control mechanism: an exported switch tells the next
// reader there is a choice, and there is not. A disarmed graph has no snapshot
// isolation, so every guarantee the rest of this package documents would have been
// conditional on a setter any caller could reach.
//
// # What replaced the capability, and why it was not merely deleted
//
// The switch existed so a benchmark could compare both arms in ONE process, and that
// had real value: this project's hardware has repeatedly manufactured phantom
// regressions from byte-identical control binaries, which is exactly the failure a
// same-process A/B rules out.
//
// The replacement is [disarmMVCCForTest], an unexported seam with no exported setter,
// reachable only from this package's own tests and benchmarks. A comparison that needs
// it lives beside the code it measures; a consumer cannot reach it at all.
func (g *Graph[N, W]) armMVCC() {
	g.mvccArmed = true
	g.labelDeltas = true
	g.propDeltas = true
	g.stamp.SetClock(&g.mvccClock)
	g.adj.EnableVersioning()
	g.adj.SetWriteStamp(&g.stamp)
}

// disarmMVCCForTest turns the versioning substrate off, for a SAME-PROCESS A/B
// comparison inside this package's own tests and benchmarks (rmp #2311).
//
// It is not part of the module's contract. A disarmed graph has no snapshot isolation,
// no write-write conflict detection and no reclamation horizon, so nothing outside a
// measurement may run on one — which is why there is no exported setter and why this
// name says what it is for.
//
// Must be called before any write and never concurrently with another operation.
func (g *Graph[N, W]) disarmMVCCForTest() {
	g.mvccArmed = false
	g.labelDeltas = false
	g.propDeltas = false
	g.adj.DisableVersioning()
	g.adj.SetWriteStamp(nil)
}

// AmbientVersionResolutions returns how many versions have resolved their
// transaction through the graph's AMBIENT slot rather than carrying it — the
// resolution rmp #2320 removed from the Cypher and store write paths.
//
// It is the observable form of an invariant that is otherwise only assertable by
// inspecting version chains: a write path that carries its transaction leaves this
// counter untouched, and a single ambient resolution inside a statement is enough
// to split that statement across two commit records once a second write bracket is
// open. Sample it before and after a region and require the difference to be zero.
//
// A non-zero difference is not automatically a defect: the direct Go-API mutators
// resolve this way BY CONTRACT — they are per-operation atomic, not transactional
// — as do the bulk builders, WAL replay and snapshot apply. It is a defect for any
// path that runs inside a write bracket.
//
// Cumulative and never reset, so two observers cannot take it from each other.
//
// Safe for concurrent use.
func (g *Graph[N, W]) AmbientVersionResolutions() int64 { return g.stamp.AmbientResolutions() }

// commitAndApply runs applier and then stamps info with the commit instant,
// allocating it first when the transaction has none yet, all while holding every
// shard lock the applier names (see [CommitApplier]); it then publishes the
// instant and returns it.
//
// # Why the instant is allocated AFTER the apply
//
// The frontier is contiguous: an instant that is allocated and not yet published
// holds back every later one, so every snapshot started meanwhile is older than
// it needs to be, and a writer whose previous commit sits above the stalled
// frontier then sees its own newest version as invisible and is refused with a
// serialization conflict. Measured with the instant allocated before the apply:
// the mixed contention arm at 32 writers exhausted 64 retries on every run,
// because a writer preempted inside the apply held the frontier for a scheduler
// quantum. The apply reads the transaction's own writes through its id instead,
// so it needs no instant, and the window shrinks to the stamp and the
// publication. [CommitApplier.Committed] runs after the publication for the
// same reason: measured with it between the stamp and the publication, its two
// atomics on process-wide cache lines widened the window enough that later
// commits published out of order and queued on the clock's publish lock, and a
// frontier held for 4.7 ms exhausted the same 64 retries. A transaction that already holds an instant — the WAL path
// allocates it before the fsync — keeps it; that window is the fsync's and
// predates this.
//
// The deferred unlocks keep a panicking applier from wedging later commits.
func (g *Graph[N, W]) commitAndApply(info *mvcc.CommitInfo, w *writeCtx, applier CommitApplier, created int64) uint64 {
	// A PANICKING APPLIER SETTLES THE TRANSACTION, AND NEVER AGAINST THE LOG. This
	// defer does not recover — a panic is a programmer error and must surface — it
	// only settles what the panic would otherwise strand, in the one way that
	// keeps memory consistent with the durable log (see
	// [CommitApplier.ApplyCommitted]):
	//
	//   - An instant allocated before the fsync means the WAL record is durable,
	//     because the applier is registered only after the fsync succeeded. The
	//     commit is PUBLISHED without the applier's work. Aborting it here, as the
	//     first version of this handler did, left the running process without a
	//     commit that recovery then replays as committed.
	//   - No instant means the in-memory path: nothing is durable, so the record
	//     is ABORTED and handed to the reclaimer exactly as a doomed transaction's
	//     is. Leaving it in flight, as the first version did, made every node it
	//     wrote unwritable for ever.
	//
	// The index delivery the applier may have opened is left open, which keeps
	// [index.Manager.DescribesSnapshot] false, and the applier has recorded the
	// indexes as undefined, which fail-stops the engine.
	published := false
	defer func() {
		if published {
			return
		}
		if w.commitTS != 0 {
			ts := g.allocateReady(info, w)
			info.Commit(ts)
			g.mvccClock.PublishCommit(w.registeredRecord(info), ts)
			w.commitTS, w.allocRec = 0, nil
			g.writeCounts.Commit(w.txID)
			g.chargeReclaimDebt(created)
			return
		}
		adjFreed := g.abortRecord(&w.tx, info)
		g.writeCounts.Abort(w.txID)
		g.abortWake(created-adjFreed, &w.tx)
	}()
	mask := applier.CommitApplyShards(CommitNodes{life: &g.nodeLifeShards, info: info})
	for m := mask; m != 0; m &= m - 1 {
		g.commitApply[bits.TrailingZeros64(m)].mu.Lock()
	}
	defer func() {
		for m := mask; m != 0; m &= m - 1 {
			g.commitApply[bits.TrailingZeros64(m)].mu.Unlock()
		}
	}()
	// Every stamped commit is below TxIDBase and therefore at or below this
	// instant, and txID makes this transaction's own versions visible; every
	// other in-flight record carries a different transaction id, so nothing else
	// uncommitted is visible. An applier that held a common shard before this one
	// stamped its record before releasing it, so it is seen. The snapshot belongs
	// to the lowest shard held, so no concurrent applier shares it, and it is
	// reused so a commit allocates nothing for it. An applier that holds no shard
	// takes one from the graph's pool instead and returns it after the apply.
	var snap *Snapshot
	if mask != 0 {
		sh := &g.commitApply[bits.TrailingZeros64(mask)]
		snap = &sh.snap
		snap.memo = &sh.memo
	} else {
		if s, ok := g.commitApplySnaps.Get().(*Snapshot); ok {
			snap = s
		} else {
			snap = newSharedSnapshot(0, 0, 0)
		}
		defer g.commitApplySnaps.Put(snap)
	}
	snap.startTS, snap.txID = mvcc.TxIDBase-1, w.txID
	// Cleared rather than dropped, so the pinned-verdict map the apply's reads
	// fill is allocated once per shard and not once per commit.
	snap.memo.mu.Lock()
	clear(snap.memo.verdict)
	snap.memo.mu.Unlock()
	applier.ApplyCommitted(snap)
	// Ready only now: the index changes are applied, so a helper that publishes
	// this commit on its behalf publishes it with them in place (rmp #2932).
	ts := g.allocateReady(info, w)
	info.Commit(ts)
	g.mvccClock.PublishCommit(w.registeredRecord(info), ts)
	published = true
	// AFTER the publication, still under the locks: nothing may sit between the
	// allocation and the publication. See the comment above.
	applier.Committed(ts)
	return ts
}

// registeredRecord is the record w's commit instant is registered for: the one
// [Graph.AllocateCommitTS] registered when the instant was allocated early, and
// otherwise info, which [Graph.allocateReady] registers.
func (w *writeCtx) registeredRecord(info *mvcc.CommitInfo) *mvcc.CommitInfo {
	if w.allocRec != nil {
		return w.allocRec
	}
	return info
}

// allocateReady returns the commit instant of w, whose record is info, with the
// record marked ready: every precondition of publishing it holds, so a later
// publication that finds the frontier stuck on it may stamp and publish it on
// this transaction's behalf (see [mvcc.Clock.AllocateFor]). The caller calls it
// only once its versions are complete, its WAL record is durable, and its index
// changes are applied.
//
// One path reaches here with its WAL record NOT durable: the fsync failed. The
// engine then replays the transaction's undo log inside the same record before
// closing it (cypher's ExplicitTx.Commit, rollbackInBarrierLocked), so every
// value it wrote is written back and its versions net to zero. Publishing that
// record — by its owner or by a helper — makes visible a state identical to the
// one before the transaction, which is why marking it ready is sound there too.
//
// A transaction whose instant was allocated before its fsync
// ([Graph.AllocateCommitTS]) keeps that instant and is marked ready here; any
// other allocates and becomes ready in one step. Either way the caller then
// stamps and publishes, and does not care whether a helper did so first.
func (g *Graph[N, W]) allocateReady(info *mvcc.CommitInfo, w *writeCtx) uint64 {
	if ts := w.commitTS; ts != 0 {
		info.MarkReady()
		return ts
	}
	return g.mvccClock.AllocateFor(info, true)
}
