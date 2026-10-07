package lpg

// writeview.go — the graph's transaction-carrying write surface (rmp #2320).
//
// # The defect this closes
//
// rmp #2301 gave every versioned store an internal `…Info(…, tx *writeCtx)`
// form, so a write CAN carry its transaction. Nothing outside this package could
// reach one. The Cypher engine's two mutator adapters call the EXPORTED
// mutators — `a.g.SetNodeProperty`, `a.g.AddEdge`, and nineteen more — every one
// of which passes a nil *writeCtx, so every delta a statement wrote resolved its
// commit record through the graph's AMBIENT slot ([mvcc.WriteStamp]).
//
// While the visibility barrier admitted one write bracket at a time the slot
// always named the writer's own transaction, so the shortcut was invisible.
// rmp #2304 removed the barrier from the autocommit path, measured 15.13x durable
// write scaling at 32 writers (267 -> 4041 commits/s) — and had to revert,
// because with two brackets open the slot names whichever published LAST:
//
//	writer A  opens bracket, publishes A          slot = A
//	writer A  writes n.x                          version -> A's record
//	writer B  opens bracket, publishes B          slot = B
//	writer A  writes n.y                          version -> B's RECORD
//	writer B  commits at T                        A's n.y becomes visible at T
//	writer A  commits at T'                       A's n.x becomes visible at T'
//
// A snapshot reader between T and T' observes half of A. Measured, not inferred:
// examples/27_concurrent_txn reported 105 942 torn observations, a counter on
// [mvcc.WriteStamp.Publish] recorded 147 overwrites of a still-ARMED
// transaction, and a counter on the untransacted branch of
// [mvcc.WriteStamp.Stamp] recorded ZERO — so the writes were not falling back to
// per-delta timestamps, they were adopting a live concurrent transaction's
// record.
//
// # The shape, and why an accessor
//
// [WriteView] is the graph bound to ONE write transaction. Its mutators are the
// exported names with the same signatures, each a single forward to the `…Info`
// form with the carried transaction — so a call site changes from `a.g.AddNode(n)`
// to `a.w().AddNode(n)` and nothing else, and no signature ripples through
// cypher/exec.
//
// It is Memgraph's shape. `memgraph::storage::Accessor` holds `Transaction
// transaction_` and exposes the mutating API — `virtual VertexAccessor
// CreateVertex() = 0;` — while the primitives beneath it take the transaction as
// an explicit parameter: `PrepareForWrite(Transaction *transaction, TObj *object)`
// and `CreateAndLinkDelta(Transaction *transaction, TObj *object, Args &&...args)`,
// whose `transaction->EnsureCommitInfoExists()` is this module's
// [mvcc.TxState.Ensure]. Read in `src/storage/v2/storage.hpp` and
// `src/storage/v2/mvcc.hpp` at commit
// 572d5b4311a279de550522344a6f10d352d11c48 (branch master, 2026-08-03).
//
// The alternative considered and rejected was to EMBED *[Graph] in WriteView so
// that promotion carried the read methods and same-named mutators shadowed the
// embedded ones. It costs zero call-site churn, and it fails on two counts that
// matter more: it exposes the whole graph — [Graph.ApplyVersioned],
// [Graph.LockBarrier], [Graph.SetIndexManager] — through a value whose responsibility
// is one transaction's writes, and a mutator added to Graph but not to WriteView
// would silently fall through to the ambient path with nothing to notice it.
// This shape makes the second impossible to miss instead: every mutator is
// listed here, and TestWriteView_CoversEveryTransactionalMutator fails if the
// list falls behind.
//
// # What it costs
//
// Two words, no allocation. [WriteView] is a [Graph] pointer plus a *[writeCtx],
// returned by value from [Graph.Writer], and every method is a single forward, so
// the transaction-carrying path and the untransacted path run the same code with
// a different value in one parameter.

import "github.com/FlavioCFOliveira/GoGraph/graph"

// WriteView is a [Graph] bound to ONE write transaction: every mutation made
// through it stamps its versions with that transaction's shared commit record,
// tests write-write conflicts against that transaction's snapshot, and claims an
// adjacency shard's copy-on-write builder under that transaction's identity.
//
// Obtain one with [Graph.Writer]. A view built from the zero [WriteTx] carries no
// transaction, and each of its writes runs as the graph's own direct mutators do
// (rmp #2947): as a single-operation transaction of its own, or inside the
// exclusive bracket that is open — the right answer for a caller outside any
// transaction and the wrong one inside one.
//
// A WriteView does NOT take the raw mutators' index-maintenance refusal
// ([ErrIndexedRawWrite]), with or without a transaction: it is the write surface
// of a caller that maintains the secondary indexes itself, as the Cypher engine
// does by recording every change it makes and fanning the changes out at commit.
// A caller that writes an indexed graph through a WriteView without doing so
// leaves the indexes stale.
//
// It is valid only while its transaction's bracket is open and must NOT be
// retained past it: the state it names is recycled on the unwind. A retained view
// does not corrupt anything — a retracted transaction's writes fall back to a
// fresh untransacted timestamp — but it silently stops being transactional.
//
// It carries no mutable state of its own, so it is safe for concurrent use
// exactly as far as the graph and the transaction it names are: two goroutines
// must not drive ONE write transaction concurrently, and two goroutines holding
// their own transactions may write concurrently.
type WriteView[N comparable, W any] struct {
	g *Graph[N, W]
	w *writeCtx
}

// Writer returns g's write surface bound to transaction tx.
//
// It allocates nothing.
func (g *Graph[N, W]) Writer(tx WriteTx) WriteView[N, W] {
	return WriteView[N, W]{g: g, w: tx.w}
}

// Graph returns the graph this view writes to, for the read-side and
// bookkeeping methods that need no transaction — side-effect counters, the label
// and property-key registries, the secondary indexes.
//
// A read that must observe the transaction's own uncommitted work goes through
// [Graph.WriterViewOf] instead, which is what the snapshot on the carried
// transaction is for.
func (wv WriteView[N, W]) Graph() *Graph[N, W] { return wv.g }

// Tx returns the transaction this view carries.
func (wv WriteView[N, W]) Tx() WriteTx { return WriteTx{w: wv.w} }

// Read returns the graph as this view's transaction READS it: as of the instant
// the transaction began, plus the versions the transaction has written itself.
//
// A write path that reads — a DELETE enumerating the labels it must strip, a
// MERGE testing what already exists — must read through this and not through
// [WriteView.Graph], which answers with the current stored value and therefore
// with other in-flight writers' uncommitted work. It is [Graph.WriterViewOf] for
// a caller that already holds the view, so the snapshot comes from the carried
// transaction rather than from the graph's slot.
//
// A view carrying no transaction reads the present, which is the correct answer
// outside a bracket.
func (wv WriteView[N, W]) Read() *ReadView[N, W] { return wv.g.WriterViewOf(wv.Tx()) }

// NoteConstraintTouch records that this transaction made a write to n that could
// INTRODUCE a property-existence violation — a property removal, a label gain, a
// node creation — and reports the write-write conflict it hit, or nil.
//
// # Why an embedder must call this, and only sometimes
//
// Conflict detection here is per SUBSTORE, so two transactions writing DIFFERENT
// substores of one node never meet. A NOT NULL constraint binds a label to a
// property — two substores — so write skew across them committed a state violating
// the declared invariant while neither transaction violated it on its own snapshot
// (rmp #2353). This is the seam that makes such a pair collide: both halves stamp
// the same per-node slot, so the second one to arrive is refused.
//
// CALL IT ONLY FOR WRITES A DECLARED CONSTRAINT ACTUALLY COVERS. The stamp is
// node-granular — every reference engine's granularity for this, because
// PostgreSQL and InnoDB version the whole row and Memgraph the whole vertex — and
// node granularity conflicts more than substore granularity does. Applying it to
// every write would raise the conflict rate for the majority of workloads, which
// declare no constraint and cannot suffer the anomaly at all. cypher calls it
// only under a schema declaring an existence or uniqueness constraint, and there
// only for a node creation or for an effective write of a label or property key
// some constraint names (rmp #3008).
//
// The conflict is RECORDED on the transaction as well as returned, so a caller that
// cannot report one still dooms the transaction and commit refuses to publish it;
// see [WriteTx.Err]. A view carrying no transaction, or a key that was never
// interned, is a no-op returning nil.
func (wv WriteView[N, W]) NoteConstraintTouch(n N) error {
	if wv.w == nil {
		return nil
	}
	id, ok := wv.g.adj.Mapper().Lookup(n)
	if !ok {
		return nil
	}
	return wv.g.conVer.note(id, wv.w)
}

// NoteConstraintTouchByID is [WriteView.NoteConstraintTouch] for a node named by
// its id. It exists for commit-time constraint validation, which knows the nodes a
// transaction touched by id (rmp #2936).
func (wv WriteView[N, W]) NoteConstraintTouchByID(id graph.NodeID) error {
	if wv.w == nil {
		return nil
	}
	return wv.g.conVer.note(id, wv.w)
}

// ── nodes ────────────────────────────────────────────────────────────────────
//
// Every mutator below that writes runs, over the zero [WriteTx], through
// [Graph.direct]: the same single-operation transaction the graph's own direct
// mutators run as, without their index refusal (rmp #2947). Before that, a view
// over the zero transaction wrote with no transaction at all, conflict-tested
// nothing, and could undo another transaction's committed delete
// ([WriteView.Revive], audit F6).

// AddNode is [Graph.AddNode] inside this view's transaction.
func (wv WriteView[N, W]) AddNode(n N) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error { return wv.g.addNodeInfo(n, tx) })
	}
	return wv.g.addNodeInfo(n, wv.w)
}

// RemoveNode is [Graph.RemoveNode] inside this view's transaction.
//
// It reports whether the removal was ADMITTED, as [WriteView.RemoveEdge],
// [WriteView.RemoveEdgeByHandle] and [WriteView.RemoveAllEdgesFrom] already do:
// FALSE means this transaction hit a write-write conflict on the node — its
// existence record, its properties, its labels or its adjacency — and the node
// was NOT retired.
//
// A caller that journals an inverse MUST gate the journal entry on it, and must
// gate any side-effect COUNTER on it too. An inverse recorded for a retirement
// that never happened revives a node the conflicting peer went on to delete for
// real, and the revival restores no labels and no properties — the peer's commit
// removed them — so what comes back is a BARE node that no transaction ever
// created, alive in the present with no life record (rmp #2726, the node mirror
// of rmp #2694 and rmp #2725). The counter must move with the journal entry
// because the inverse's DecrNodesRemoved is what reverses the increment.
//
// TRUE does not mean a node was taken out: a removal of an already-tombstoned or
// never-interned key is admitted and retires nothing, and reports true. The
// caller's own presence probe answers that; this answers only whether the write
// was ADMITTED — the same reading [WriteView.RemoveEdge] takes.
//
// The error is the refusal behind a FALSE, and nil with a TRUE. Inside a
// transaction it is the conflict the transaction recorded on itself
// ([WriteTx.Err]); the transaction is doomed whether or not the caller acts on
// it. Over the zero [WriteTx] it is the direct write's refusal, wrapping
// [ErrDirectWriteConflict] (rmp #2947), and the call changed nothing. The
// (bool, error) result is a breaking change: RemoveNode used to return only the
// bool.
func (wv WriteView[N, W]) RemoveNode(n N) (bool, error) {
	if wv.w == nil {
		err := wv.g.removeNodeDirect(n)
		return err == nil, err
	}
	if !wv.g.removeNodeInfo(n, wv.w) {
		return false, wv.w.err()
	}
	return true, nil
}

// Revive is [Graph.Revive] inside this view's transaction. It returns nil inside
// a transaction, whose refusal is recorded on the transaction ([WriteTx.Err]),
// and over the zero [WriteTx] the refusal of the direct write, wrapping
// [ErrDirectWriteConflict] (rmp #2947, audit F6). The error return is a breaking
// change: Revive used to return nothing.
func (wv WriteView[N, W]) Revive(n N) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.reviveInfo(n, tx)
			return nil
		})
	}
	wv.g.reviveInfo(n, wv.w)
	return nil
}

// SetNodeLabel is [Graph.SetNodeLabel] inside this view's transaction.
func (wv WriteView[N, W]) SetNodeLabel(n N, name string) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error { return wv.g.setNodeLabelInfo(n, name, tx) })
	}
	return wv.g.setNodeLabelInfo(n, name, wv.w)
}

// RemoveNodeLabel is [Graph.RemoveNodeLabel] inside this view's transaction. It refuses an over-long token
// with [ErrTokenTooLong] and changes nothing (rmp #2748); the error return is a
// breaking change.
func (wv WriteView[N, W]) RemoveNodeLabel(n N, name string) error {
	if err := CheckToken("node label", name); err != nil {
		return err
	}
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.removeNodeLabelInfo(n, name, tx)
			return nil
		})
	}
	wv.g.removeNodeLabelInfo(n, name, wv.w)
	return nil
}

// SetNodeProperty is [Graph.SetNodeProperty] inside this view's transaction.
func (wv WriteView[N, W]) SetNodeProperty(n N, key string, value PropertyValue) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error { return wv.g.setNodePropertyInfo(n, key, value, tx) })
	}
	return wv.g.setNodePropertyInfo(n, key, value, wv.w)
}

// DelNodeProperty is [Graph.DelNodeProperty] inside this view's transaction. It refuses an over-long token
// with [ErrTokenTooLong] and changes nothing (rmp #2748); the error return is a
// breaking change.
func (wv WriteView[N, W]) DelNodeProperty(n N, key string) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.delNodePropertyInfo(n, key, tx)
			return nil
		})
	}
	wv.g.delNodePropertyInfo(n, key, wv.w)
	return nil
}

// ── topology ─────────────────────────────────────────────────────────────────

// AddEdge is [Graph.AddEdge] inside this view's transaction.
func (wv WriteView[N, W]) AddEdge(src, dst N, w W) error {
	if wv.w == nil {
		var h uint64
		return wv.g.direct(func(tx *writeCtx) error {
			_, err := wv.g.appendEdgeInfo(src, dst, w, 0, nil, &h, tx)
			return err
		})
	}
	return wv.g.addEdgeInfo(src, dst, w, wv.w)
}

// AddEdgeLabeledWithProperty is [Graph.AddEdgeLabeledWithProperty] inside this
// view's transaction.
func (wv WriteView[N, W]) AddEdgeLabeledWithProperty(src, dst N, w W, relType, key string, value PropertyValue) error {
	if wv.w == nil {
		return wv.g.AddEdgeLabeledWithProperty(src, dst, w, relType, key, value)
	}
	enc, payload, err := wv.g.labeledPropAppend(relType, key, value)
	if err != nil {
		return err
	}
	var h uint64
	_, err = wv.g.appendEdgeInfo(src, dst, w, enc, payload, &h, wv.w)
	return err
}

// AddEdgeH is [Graph.AddEdgeH] inside this view's transaction.
func (wv WriteView[N, W]) AddEdgeH(src, dst N, w W) (uint64, error) {
	if wv.w == nil {
		var h, handle uint64
		err := wv.g.direct(func(tx *writeCtx) error {
			var e error
			handle, e = wv.g.appendEdgeInfo(src, dst, w, 0, nil, &h, tx)
			return e
		})
		if err != nil {
			return 0, err
		}
		return handle, nil
	}
	return wv.g.addEdgeHInfo(src, dst, w, wv.w)
}

// AddEdgeHIfAbsent is [Graph.AddEdgeHIfAbsent] inside this view's transaction.
func (wv WriteView[N, W]) AddEdgeHIfAbsent(src, dst N, w W, handle uint64) (bool, error) {
	if wv.w == nil {
		var inserted bool
		err := wv.g.direct(func(tx *writeCtx) error {
			var e error
			inserted, e = wv.g.addEdgeHIfAbsentInfo(src, dst, w, handle, tx)
			return e
		})
		if err != nil {
			return false, err
		}
		return inserted, nil
	}
	return wv.g.addEdgeHIfAbsentInfo(src, dst, w, handle, wv.w)
}

// RemoveEdge is [Graph.RemoveEdge] inside this view's transaction.
//
// It reports whether the removal was APPLIED, as
// [WriteView.RemoveAllEdgesFrom] and [WriteView.RemoveEdgeByHandle] already do:
// FALSE means this transaction hit a write-write conflict and nothing was
// mutated. A caller that journals an inverse MUST gate the journal entry on it —
// an inverse recorded for a removal that never happened re-adds an arc the
// conflicting peer still owns, and once that peer has rolled its own arc back the
// inverse leaves an arc no transaction ever created (rmp #2725, the per-edge
// mirror of rmp #2694).
//
// TRUE does not mean an arc was taken out: a removal of an absent edge applies
// and removes nothing, and reports true. The caller's presence probe answers
// that; this answers only whether the write was ADMITTED. The two siblings
// differ here — both fold "nothing to remove" into their false — so a caller
// switching between them must read each contract rather than assume it.
//
// A view over the zero [WriteTx] reports FALSE when the removal was refused with
// [ErrDirectWriteConflict] (rmp #2947), in which case it changed nothing.
func (wv WriteView[N, W]) RemoveEdge(src, dst N) bool {
	if wv.w == nil {
		applied := false
		err := wv.g.direct(func(tx *writeCtx) error {
			applied = wv.g.removeEdgeInfo(src, dst, tx)
			return nil
		})
		return err == nil && applied
	}
	return wv.g.removeEdgeInfo(src, dst, wv.w)
}

// RemoveEdgeByHandle is [Graph.RemoveEdgeByHandle] inside this view's
// transaction.
//
// A view over the zero [WriteTx] reports FALSE for a direct-write refusal, as
// [WriteView.RemoveEdge] does.
func (wv WriteView[N, W]) RemoveEdgeByHandle(src, dst N, handle uint64) bool {
	if wv.w == nil {
		removed := false
		err := wv.g.direct(func(tx *writeCtx) error {
			removed = wv.g.removeEdgeByHandleInfo(src, dst, handle, tx)
			return nil
		})
		return err == nil && removed
	}
	return wv.g.removeEdgeByHandleInfo(src, dst, handle, wv.w)
}

// RemoveAllEdgesFrom is [Graph.RemoveAllEdgesFrom] inside this view's
// transaction.
//
// It reports whether the removal was APPLIED, exactly as
// [WriteView.RemoveEdgeByHandle] does: FALSE means this transaction hit a
// write-write conflict and nothing was mutated. A caller that journals an inverse
// MUST gate the journal entry on it — an inverse recorded for a removal that
// never happened re-creates an arc the conflicting peer still owns (rmp #2694).
//
// A view over the zero [WriteTx] reports FALSE for a direct-write refusal, as
// [WriteView.RemoveEdge] does.
func (wv WriteView[N, W]) RemoveAllEdgesFrom(src N) bool {
	if wv.w == nil {
		applied := false
		err := wv.g.direct(func(tx *writeCtx) error {
			applied = wv.g.removeAllEdgesFromInfo(src, tx)
			return nil
		})
		return err == nil && applied
	}
	return wv.g.removeAllEdgesFromInfo(src, wv.w)
}

// ── per-pair relationship types and properties ───────────────────────────────

// SetEdgeLabel is [Graph.SetEdgeLabel] inside this view's transaction. The error return is a
// breaking change (rmp #2748).
func (wv WriteView[N, W]) SetEdgeLabel(src, dst N, name string) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error { return wv.g.setEdgeLabelInfo(src, dst, name, tx) })
	}
	return wv.g.setEdgeLabelInfo(src, dst, name, wv.w)
}

// RemoveEdgeLabel is [Graph.RemoveEdgeLabel] inside this view's transaction. It refuses an over-long token
// with [ErrTokenTooLong] and changes nothing (rmp #2748); the error return is a
// breaking change.
func (wv WriteView[N, W]) RemoveEdgeLabel(src, dst N, name string) error {
	if err := CheckToken("relationship type", name); err != nil {
		return err
	}
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.removeEdgeLabelInfo(src, dst, name, tx)
			return nil
		})
	}
	wv.g.removeEdgeLabelInfo(src, dst, name, wv.w)
	return nil
}

// SetEdgeProperty is [Graph.SetEdgeProperty] inside this view's transaction.
func (wv WriteView[N, W]) SetEdgeProperty(src, dst N, key string, value PropertyValue) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error { return wv.g.setEdgePropertyInfo(src, dst, key, value, tx) })
	}
	return wv.g.setEdgePropertyInfo(src, dst, key, value, wv.w)
}

// DelEdgeProperty is [Graph.DelEdgeProperty] inside this view's transaction. It refuses an over-long token
// with [ErrTokenTooLong] and changes nothing (rmp #2748); the error return is a
// breaking change.
func (wv WriteView[N, W]) DelEdgeProperty(src, dst N, key string) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.delEdgePropertyInfo(src, dst, key, tx)
			return nil
		})
	}
	wv.g.delEdgePropertyInfo(src, dst, key, wv.w)
	return nil
}

// ── per-instance surfaces, addressed by CREATE ordinal ───────────────────────

// SetEdgeLabelAt is [Graph.SetEdgeLabelAt] inside this view's transaction. The error return is a
// breaking change (rmp #2748).
func (wv WriteView[N, W]) SetEdgeLabelAt(src, dst N, idx int64, name string) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error { return wv.g.setEdgeLabelAtInfo(src, dst, idx, name, tx) })
	}
	return wv.g.setEdgeLabelAtInfo(src, dst, idx, name, wv.w)
}

// SetEdgePropertyAt is [Graph.SetEdgePropertyAt] inside this view's transaction.
func (wv WriteView[N, W]) SetEdgePropertyAt(src, dst N, idx int64, key string, value PropertyValue) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			return wv.g.setEdgePropertyAtInfo(src, dst, idx, key, value, tx)
		})
	}
	return wv.g.setEdgePropertyAtInfo(src, dst, idx, key, value, wv.w)
}

// RemoveEdgeInstance is [Graph.RemoveEdgeInstance] inside this view's
// transaction.
//
// It returns nil inside a transaction, whose refusal is recorded on the
// transaction ([WriteTx.Err]), and over the zero [WriteTx] the refusal of the
// direct write, wrapping [ErrDirectWriteConflict] (rmp #2947). The error return
// is a breaking change: RemoveEdgeInstance used to return nothing.
func (wv WriteView[N, W]) RemoveEdgeInstance(src, dst N, idx int64) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.removeEdgeInstanceInfo(src, dst, idx, tx)
			return nil
		})
	}
	wv.g.removeEdgeInstanceInfo(src, dst, idx, wv.w)
	return nil
}

// ── per-instance surfaces, addressed by stable handle ────────────────────────

// SetEdgeLabelByHandle is [Graph.SetEdgeLabelByHandle] inside this view's
// transaction. The error return is a
// breaking change (rmp #2748).
func (wv WriteView[N, W]) SetEdgeLabelByHandle(src, dst N, handle uint64, name string) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			return wv.g.setEdgeLabelByHandleInfo(src, dst, handle, name, tx)
		})
	}
	return wv.g.setEdgeLabelByHandleInfo(src, dst, handle, name, wv.w)
}

// SetEdgePropertyByHandle is [Graph.SetEdgePropertyByHandle] inside this view's
// transaction.
func (wv WriteView[N, W]) SetEdgePropertyByHandle(src, dst N, handle uint64, key string, value PropertyValue) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			return wv.g.setEdgePropertyByHandleInfo(src, dst, handle, key, value, tx)
		})
	}
	return wv.g.setEdgePropertyByHandleInfo(src, dst, handle, key, value, wv.w)
}

// DelEdgePropertyByHandle is [Graph.DelEdgePropertyByHandle] inside this view's
// transaction. It refuses an over-long token
// with [ErrTokenTooLong] and changes nothing (rmp #2748); the error return is a
// breaking change.
func (wv WriteView[N, W]) DelEdgePropertyByHandle(src, dst N, handle uint64, key string) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.delEdgePropertyByHandleInfo(src, dst, handle, key, tx)
			return nil
		})
	}
	wv.g.delEdgePropertyByHandleInfo(src, dst, handle, key, wv.w)
	return nil
}

// RemoveEdgeInstanceByHandle is [Graph.RemoveEdgeInstanceByHandle] inside this
// view's transaction.
//
// It returns nil inside a transaction and the direct write's refusal over the
// zero [WriteTx], as [WriteView.RemoveEdgeInstance] does. The error return is a
// breaking change: RemoveEdgeInstanceByHandle used to return nothing.
func (wv WriteView[N, W]) RemoveEdgeInstanceByHandle(src, dst N, handle uint64) error {
	if wv.w == nil {
		return wv.g.direct(func(tx *writeCtx) error {
			wv.g.removeEdgeInstanceByHandleInfo(src, dst, handle, tx)
			return nil
		})
	}
	wv.g.removeEdgeInstanceByHandleInfo(src, dst, handle, wv.w)
	return nil
}
