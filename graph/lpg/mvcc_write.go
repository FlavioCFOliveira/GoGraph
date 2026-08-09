package lpg

import "context"

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
// The graph slot is still published, because a write that carries no transaction
// has to resolve one somehow (the whole Cypher write path is such a write); what
// changed is that a transaction's own lifecycle no longer depends on it.
//
// Nested calls remain forbidden: a nested bracket would be a nested transaction,
// which this design has no meaning for. [Graph.ApplyInsideLocked] deliberately
// does not call this, and the re-entrancy guard catches the rest under -race or
// -tags gograph_debug.
func (g *Graph[N, W]) beginWrite() *writeCtx {
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
	g.stamp.Publish(&w.tx)
	g.writeTx.Store(w)
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

// Valid reports whether tx names an open write transaction.
//
// It is false for the zero value and for a bracket opened on a graph whose
// versioning substrate is disarmed (see [Graph.disarmMVCCForTest]), where there is no
// transaction to name and every write is committed as it is made.
func (tx WriteTx) Valid() bool { return tx.w != nil }

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
		tx.w.commitTS = g.mvccClock.NextCommitTS()
	}
	return tx.w.commitTS
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
	g.mvccClock.AbandonCommitTS(w.commitTS)
	w.commitTS = 0
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
	if info == nil {
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
		if w.err() != nil {
			g.writeCounts.Abort(w.txID)
		}
		return 0
	}
	// A transaction that hit a serialization conflict ABORTS. See below for the
	// measured atomicity violation that this closes, and why it is not the same
	// thing as the rolled-back-statement case the file comment describes.
	if w.err() != nil {
		info.Abort()
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
		g.abortWake(created)
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
	ts := w.commitTS
	if ts == 0 {
		ts = g.mvccClock.NextCommitTS()
	}
	w.commitTS = 0
	info.Commit(ts)
	g.mvccClock.PublishCommitTS(ts)
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
