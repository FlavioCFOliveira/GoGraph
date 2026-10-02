package lpg

// edge_handle_durable.go — Stage 2/3 durability surface for stable edge
// handles: the helpers the WAL replay and the snapshot load paths use to
// rebuild a recovered graph whose parallel edges keep their original
// per-CREATE identity (type + properties) and whose handle counter stays
// monotone across a reopen.
//
// # Why these exist
//
// edge_handle.go assigns handles in memory and keys per-instance edge
// metadata by them, but the handle was volatile: a recovered edge got a
// fresh handle and an empty per-handle store, so two distinctly-typed
// parallel CREATEs collapsed to one type on the next reopen. Persisting the
// handle (WAL: OpAddEdgeH / OpSetEdge{Label,Property}ByHandle; snapshot:
// the edgehandles.bin component) closes that, but recovery must replay it
// without double-inserting an edge the snapshot already loaded. These
// helpers give recovery the two primitives it needs:
//
//   - [Graph.HasEdgeHandle] — does this (src, dst) pair already carry an
//     edge stamped with `handle`? Lets replay of an OpAddEdgeH no-op when
//     the snapshot (or an earlier replayed frame) already materialised it,
//     making snapshot + full-WAL recovery idempotent (the examples/24
//     doubling fix). The per-pair handle column is the single source of
//     truth; no second live-handle set is kept (derive, don't duplicate).
//   - [Graph.AddEdgeHIfAbsent] — insert (src, dst, w) with the explicit
//     `handle` only when [Graph.HasEdgeHandle] is false, so the replay is
//     all-or-nothing per edge.
//   - [Graph.SeedEdgeHandle] — raise the handle high-water counter to at
//     least `next` after recovery so a post-recovery AddEdge never re-mints
//     a handle already live on disk (invariant I5).
//   - [Graph.WalkEdgeHandles] — enumerate every live (src, dst, handle)
//     triple so the snapshot writer can persist the handle column and the
//     per-handle metadata deterministically.
//
// # Concurrency
//
// All four are intended for the single-threaded recovery / snapshot phase.
// HasEdgeHandle and SeedEdgeHandle are individually safe for concurrent
// use (they take the same locks the in-memory write path takes);
// AddEdgeHIfAbsent and WalkEdgeHandles are not (they read-then-write or
// walk the whole adjacency without a global barrier).

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// HasEdgeHandle reports whether the directed (src, dst) pair carries a
// stored edge whose stable handle equals `handle`. It scans the pair's
// parallel handle column on the adjacency slot — the single source of
// truth for which handles are live on which pair — and returns false when
// handle is 0 (the no-handle sentinel), when either endpoint is unknown to
// the mapper, or when the pair has no slot stamped with that handle.
//
// HasEdgeHandle is the idempotency predicate WAL replay uses: an
// OpAddEdgeH whose handle is already present (loaded from the snapshot or
// applied by an earlier frame) is a no-op, so snapshot + full-WAL recovery
// does not double the edge.
//
// HasEdgeHandle is safe for concurrent use.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) HasEdgeHandle(src, dst N, handle uint64) bool {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.HasEdgeHandleAsOf(src, dst, handle, g.latestCommitted(&cs))
}

// HasEdgeHandleAsOf is [Graph.HasEdgeHandle] resolved at s: whether the edge
// carrying handle on (src, dst) exists as of that snapshot. A nil s reads the
// stored entry, including uncommitted writes.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasEdgeHandleAsOf(src, dst N, handle uint64, s *Snapshot) bool {
	if handle == 0 {
		return false
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	v := g.EntryViewAsOf(srcID, s)
	for i, nb := range v.Neighbours {
		if nb == dstID && i < len(v.Handles) && v.Handles[i] == handle {
			return true
		}
	}
	return false
}

// AddEdgeHIfAbsent inserts a directed edge (src, dst, w) stamped with the
// explicit stable `handle`, but only when no edge with that handle already
// exists on the (src, dst) pair ([Graph.HasEdgeHandle]). When the handle is
// already present the call is a no-op and returns (false, nil): the edge
// was loaded by the snapshot or applied by an earlier WAL frame, so
// re-inserting it would create a spurious parallel duplicate. When the
// handle is absent the edge is inserted via the explicit-handle adjacency
// path ([adjlist.AdjList.AddEdgeH]) and the call returns (true, nil).
//
// AddEdgeHIfAbsent is the replay primitive that makes snapshot + full-WAL
// recovery idempotent without a second live-handle index. It does NOT
// advance the handle counter — the handle is supplied by the durable
// record, not freshly minted; [Graph.SeedEdgeHandle] re-seeds the counter
// once after replay.
//
// A handle of 0 is treated as "no durable identity" and falls back to a
// plain [Graph.AddEdge] so a pre-Stage-2 WAL frame (which carried no
// handle) still replays. AddEdgeHIfAbsent is NOT safe for concurrent use.
//
// It runs as a single-operation transaction (rmp #2947) and claims both
// existing endpoints as [Graph.AddEdge] does: it refuses with an error wrapping
// [ErrDirectWriteConflict], and changes nothing, while another transaction holds
// an uncommitted write on either endpoint's adjacency. The refusal is retryable.
// See [ErrDirectWriteConflict]. Inside a transaction
// ([WriteView.AddEdgeHIfAbsent], the path a durable store commit takes) the same
// claims, and the adjacency's refusal of any write over another transaction's
// uncommitted entry, refuse it with a [*mvcc.Conflict] that dooms that
// transaction instead.
func (g *Graph[N, W]) AddEdgeHIfAbsent(src, dst N, w W, handle uint64) (inserted bool, err error) {
	err = g.direct(func(tx *writeCtx) error {
		var e error
		inserted, e = g.addEdgeHIfAbsentInfo(src, dst, w, handle, tx)
		return e
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// addEdgeHIfAbsentInfo is [Graph.AddEdgeHIfAbsent] inside write transaction tx;
// tx is nil only on a graph whose versioning substrate is disarmed. See
// [writeCtx].
//
// It needs the transaction-carrying form for TWO callers that both run inside a
// write bracket, which is what makes it different from the other replay
// primitives in this file:
//
//   - the Cypher engine's in-memory undo log, whose inverse of a DELETE re-adds
//     the removed instance (cypher/undo_record.go) while the failing statement's
//     bracket is still open;
//   - the durable store's in-memory apply, which replays a transaction's
//     OpAddEdgeH frames inside one bracket (store/txn).
//
// A write on either path that resolved its commit record through the ambient slot
// would publish part of its transaction at another transaction's instant once two
// brackets overlap (rmp #2320).
//
// It CLAIMS both existing endpoints before the adjacency mutation, in one
// test-then-stamp step ([adjVersions.claimAppendPair]), exactly as
// [Graph.appendEdgeInfo] does, and stamps an endpoint the append creates after
// the insert. Until the ACID audit of rmp #2965 it claimed nothing, so a store
// commit's apply built its entry on another transaction's uncommitted one: the
// commit published that transaction's arc, and the transaction's abort then
// restored the stacked entry with the arc still in it. The adjacency itself now
// refuses that write structurally ([adjlist.AdjList] direct_conflict.go); the
// claim is what also orders it against a node removal or another append that
// the stamps record.
//
// A handle already present is a no-op that writes nothing and takes no claim:
// replaying a snapshot-loaded edge costs no stamp. The verdict is taken only
// once both endpoints' adjacency heads admit tx ([adjVersions.admits]) and the
// handle is present in what tx can SEE, because the stored entry also carries
// other transactions' uncommitted ones (ACID audit round 6, finding C1): a
// durable store commit that found a peer's pending identical handle there used
// to be acknowledged with no claim and no WAL record, and its edge was gone
// once the peer aborted.
//
// The read-then-write it performs is NOT made atomic by the transaction: the
// method's contract already says it is not safe for concurrent use, and its
// idempotence is against a snapshot that has already been loaded, not against a
// concurrent writer.
func (g *Graph[N, W]) addEdgeHIfAbsentInfo(src, dst N, w W, handle uint64, tx *writeCtx) (inserted bool, err error) {
	// The STORED entry (nil snapshot) first: it is the cheap filter, and an edge
	// absent from it is absent from every view.
	if handle != 0 && g.HasEdgeHandleAsOf(src, dst, handle, nil) {
		if tx == nil {
			return false, nil
		}
		var ids [2]graph.NodeID
		n := 0
		if srcID, ok := g.adj.Mapper().Lookup(src); ok {
			ids[n], n = srcID, n+1
		}
		if src != dst {
			if dstID, ok := g.adj.Mapper().Lookup(dst); ok {
				ids[n], n = dstID, n+1
			}
		}
		if err := g.adjVer.admits(ids, n, tx); err != nil {
			return false, err
		}
		// An edge this transaction, or a replay, already inserted is visible to
		// it whether or not it has committed.
		if g.HasEdgeHandleAsOf(src, dst, handle, &tx.snap) {
			return false, nil
		}
	}
	// The existing endpoints, claimed together so a refusal on either stamps
	// neither and allocates no commit record; see [Graph.appendEdgeInfo] for why
	// the destination is claimed on a directed graph too.
	var srcClaimed, dstClaimed bool
	if tx != nil {
		var ids [2]graph.NodeID
		n := 0
		if srcID, ok := g.adj.Mapper().Lookup(src); ok {
			ids[n], n, srcClaimed = srcID, n+1, true
		}
		if src != dst {
			if dstID, ok := g.adj.Mapper().Lookup(dst); ok {
				ids[n], n, dstClaimed = dstID, n+1, true
			}
		}
		if err := g.adjVer.claimAppendPair(ids, n, tx); err != nil {
			return false, err
		}
	}
	// Endpoints interned through the hooked path BEFORE either insert, so a node this
	// append CREATES is born at the transaction's instant rather than at the beginning
	// of time; see [Graph.internEndpoint] (rmp #2331).
	//
	// This is the path store/txn's OpAddEdgeH apply takes, and therefore the one every
	// DURABLE edge write reaches the graph through. The first fix covered
	// [Graph.addEdgeInfo] alone, which the direct Go API uses; a checkpoint capture
	// then measured the remainder at 154 visible nodes against 71 visible edges, where
	// 142 was owed.
	g.internEndpoint(src, tx)
	if src != dst {
		g.internEndpoint(dst, tx)
	}
	if tx.doomed() {
		return false, tx.err()
	}
	if handle == 0 {
		err = g.adj.Writer(tx.adjTx()).AddEdge(src, dst, w)
	} else {
		err = g.adj.Writer(tx.adjTx()).AddEdgeH(src, dst, w, handle)
	}
	if err != nil {
		return false, adjErr(tx, err)
	}
	// An endpoint this append CREATED is stamped after the insert, because its id
	// did not exist before it; see [adjVersions.claimAppend].
	if tx != nil {
		if !srcClaimed {
			if srcID, ok := g.adj.Mapper().Lookup(src); ok {
				g.adjVer.stampAppend(srcID, tx)
			}
		}
		if src != dst && !dstClaimed {
			if dstID, ok := g.adj.Mapper().Lookup(dst); ok {
				g.adjVer.stampAppend(dstID, tx)
			}
		}
	}
	// Invalidate every CSR-position-keyed cache at SOURCE; see [Graph.AddEdge].
	g.topoGeneration.Add(1)
	return true, nil
}

// SeedEdgeHandle raises the per-graph stable-handle high-water counter so
// the next [Graph.AddEdgeH] returns a value strictly greater than `next-1`
// — i.e. at least `next`. It is called once at the end of recovery with
// max(live handle)+1 so a post-recovery edge creation never re-mints a
// handle that is already live on disk (invariant I5: handles stay unique
// and monotone across a reopen).
//
// The operation is monotone: seeding with a value at or below the current
// counter is a no-op, so calling it with a stale `next` cannot rewind the
// counter. SeedEdgeHandle is safe for concurrent use, though recovery
// calls it from the single load goroutine.
func (g *Graph[N, W]) SeedEdgeHandle(next uint64) {
	if next == 0 {
		return
	}
	// Raise the counter to next-1 so the following Add(1) yields next. The
	// counter lives in the ADJACENCY (rmp #2317) and holds the last HANDED-OUT
	// handle, so to make the next handout >= next it must be >= next-1.
	g.adj.SeedHandleSeq(next - 1)
}

// EdgeHandleTriple is one live durable edge identity: the (src, dst)
// endpoint NodeIDs and the stable handle stamped on that slot. Emitted by
// [Graph.WalkEdgeHandles] for the snapshot writer.
type EdgeHandleTriple struct {
	Src    graph.NodeID
	Dst    graph.NodeID
	Handle uint64
}

// WalkEdgeHandles calls fn once for every live directed edge slot that
// carries a non-zero stable handle. It returns early if fn returns false.
// Slots with a 0 handle (the no-handle sentinel, e.g. a simple-graph edge
// or a pre-Stage-2 edge) are skipped: there is no durable identity to
// persist for them.
//
// The walk is the snapshot writer's enumeration of the adjacency handle
// column. It iterates source nodes in the underlying mapper's [Walk] order
// — the exact order the CSR, labels and properties snapshot writers use —
// and within each source in adjacency slot order (insertion order). That
// makes the persisted edgehandles.bin component byte-stable across writes
// of the same logical state and aligned with the CSR component, honouring
// the cross-process byte-equality contract the snapshot relies on.
//
// WalkEdgeHandles is NOT safe for concurrent use with mutations on g.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) WalkEdgeHandles(fn func(EdgeHandleTriple) bool) {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	g.WalkEdgeHandlesAsOf(g.latestCommitted(&cs), fn)
}

// walkEdgeHandlesRaw is the present-state body of [Graph.WalkEdgeHandles], reading the newest
// stored entry including uncommitted writes; [Graph.WalkEdgeHandlesAsOf] uses it
// for a nil snapshot.
func (g *Graph[N, W]) walkEdgeHandlesRaw(fn func(EdgeHandleTriple) bool) {
	adj := g.adj
	adj.Mapper().Walk(func(srcID graph.NodeID, _ N) bool {
		neighbours, _, handles := adj.LoadEntryH(srcID)
		if handles == nil {
			return true
		}
		for i, dstID := range neighbours {
			if i >= len(handles) {
				break
			}
			h := handles[i]
			if h == 0 {
				continue
			}
			if !fn(EdgeHandleTriple{Src: srcID, Dst: dstID, Handle: h}) {
				return false
			}
		}
		return true
	})
}

// EdgeLabelsByHandleID returns the labels recorded for the edge identified
// by `handle` on the directed (srcID, dstID) NodeID pair, resolving NodeIDs
// directly rather than through the natural key. It is the NodeID-keyed
// dual of [Graph.EdgeLabelsByHandle] used by the snapshot writer, which
// walks the adjacency by NodeID and must not pay a Resolve→Lookup round
// trip per handle. Returns nil when handle is 0, the handle was never
// labelled, or no handle store exists for the pair.
//
// EdgeLabelsByHandleID is safe for concurrent use.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) EdgeLabelsByHandleID(srcID, dstID graph.NodeID, handle uint64) []string {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, g.latestCommitted(&cs))
}

// EdgeLabelsByHandleIDAsOf is [Graph.EdgeLabelsByHandleID] as the instance
// stood at snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgeLabelsByHandleIDAsOf(srcID, dstID graph.NodeID, handle uint64, snap *Snapshot) []string {
	if handle == 0 {
		return nil
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// The lookup is INLINE rather than through handleLabelBagAsOf, and that is
	// measured, not stylistic: each extra call frame returning a 40-byte bag
	// cost 3.6 ns on BenchmarkEdgeSideRead_LabelsByHandle, a tenth of the whole
	// read. Indexing the nil OUTER map is deliberate — Go returns the zero
	// instMap, whose get reports "no record", which is exactly right.
	im := sh.m[k]
	bag, ok := im.get(handle)
	if snap != nil && !sh.v.empty() {
		bag, ok = sh.v.asOfSnap(edgeHandleKey{pair: k, handle: handle}, bag, ok, snap, snap.startTS, snap.txID)
	}
	if !ok {
		return nil
	}
	out := make([]string, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	})
	return out
}

// EdgePropertiesByHandleID returns the property map recorded for the edge
// identified by `handle` on the directed (srcID, dstID) NodeID pair. It is
// the NodeID-keyed dual of [Graph.EdgePropertiesByHandle] used by the
// snapshot writer. Returns nil when handle is 0, the handle was never
// written, or no handle store exists for the pair.
//
// EdgePropertiesByHandleID is safe for concurrent use.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) EdgePropertiesByHandleID(srcID, dstID graph.NodeID, handle uint64) map[string]PropertyValue {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, g.latestCommitted(&cs))
}

// EdgePropertiesByHandleIDAsOf is [Graph.EdgePropertiesByHandleID] as the
// instance stood at snap. A nil snapshot reads the current value; see
// snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesByHandleIDAsOf(srcID, dstID graph.NodeID, handle uint64, snap *Snapshot) map[string]PropertyValue {
	if handle == 0 {
		return nil
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// Inline for the reason given on [Graph.EdgeLabelsByHandleIDAsOf].
	im := sh.m[k]
	bag, ok := im.get(handle)
	if snap != nil && !sh.v.empty() {
		bag, ok = sh.v.asOfSnap(edgeHandleKey{pair: k, handle: handle}, bag, ok, snap, snap.startTS, snap.txID)
	}
	if !ok {
		return nil
	}
	out := make(map[string]PropertyValue, bag.len())
	bag.forEach(func(pid PropertyKeyID, v PropertyValue) {
		if name, ok := g.pkeys.Resolve(pid); ok {
			out[name] = v
		}
	})
	return out
}

// EdgePropertyByHandleIDAsOf returns the value recorded under key for the edge
// identified by handle on the directed (srcID, dstID) NodeID pair, as the
// instance stood at snap. A nil snapshot reads the current value. The boolean
// reports whether the instance carries that key at all; it is false when handle
// is 0, the key name was never interned, the handle was never written, or the
// instance's bag has no record for the key.
//
// It is the SINGLE-KEY dual of [Graph.EdgePropertiesByHandleIDAsOf] and returns
// exactly the PropertyValue that map would hold under key: the two read the same
// shard, resolve the same instance bag through the same snapshot reconstruction,
// and differ only in that this one asks the bag for one record instead of
// enumerating them all. The map builder drops a record whose PropertyKeyID does
// not resolve to a name; this one starts from the name and fails when it is not
// interned, which is the same set of keys viewed from the other side of the
// registry. It exists so a caller that needs ONE property of a bound parallel
// instance does not allocate the whole map to read one cell (rmp #2388).
//
// EdgePropertyByHandleIDAsOf is safe for concurrent use.
func (g *Graph[N, W]) EdgePropertyByHandleIDAsOf(srcID, dstID graph.NodeID, handle uint64, key string, snap *Snapshot) (PropertyValue, bool) {
	if handle == 0 {
		return PropertyValue{}, false
	}
	pid, known := g.pkeys.Lookup(key)
	if !known {
		return PropertyValue{}, false
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// Inline for the reason given on [Graph.EdgeLabelsByHandleIDAsOf].
	im := sh.m[k]
	bag, ok := im.get(handle)
	if snap != nil && !sh.v.empty() {
		bag, ok = sh.v.asOfSnap(edgeHandleKey{pair: k, handle: handle}, bag, ok, snap, snap.startTS, snap.txID)
	}
	if !ok {
		return PropertyValue{}, false
	}
	return bag.get(pid)
}

// SetEdgeLabelByHandleID attaches `name` to the edge identified by `handle`
// on the directed (srcID, dstID) NodeID pair, resolving by NodeID rather
// than natural key. It is the NodeID-keyed dual of
// [Graph.SetEdgeLabelByHandle] used by the snapshot/WAL recovery path,
// which has already restored the mapper by NodeID and must not pay a
// Resolve→Lookup round trip. No-op when handle is 0.
//
// SetEdgeLabelByHandleID is safe for concurrent use.
//
// It refuses a relationship type longer than [MaxTokenLen] bytes with an error wrapping
// [ErrTokenTooLong], before changing any state (rmp #2748). The error return is a
// breaking change: SetEdgeLabelByHandleID used to return nothing.
//
// It runs as a single-operation transaction (rmp #2947): it refuses with an
// error wrapping [ErrDirectWriteConflict], and changes nothing, while another
// transaction holds an uncommitted write on the instance's per-handle types. The refusal is retryable. See
// [ErrDirectWriteConflict].
func (g *Graph[N, W]) SetEdgeLabelByHandleID(srcID, dstID graph.NodeID, handle uint64, name string) error {
	if err := CheckToken("relationship type", name); err != nil {
		return err
	}
	return g.direct(func(tx *writeCtx) error {
		return g.setEdgeLabelByHandleIDInfo(srcID, dstID, handle, name, tx)
	})
}

// setEdgeLabelByHandleIDInfo is [Graph.SetEdgeLabelByHandleID] inside write transaction tx; tx is
// nil only on a graph whose versioning substrate is disarmed. See [writeCtx].
func (g *Graph[N, W]) setEdgeLabelByHandleIDInfo(srcID, dstID graph.NodeID, handle uint64, name string, tx *writeCtx) error {
	if err := CheckToken("relationship type", name); err != nil {
		return err
	}
	if handle == 0 {
		return nil
	}
	lid := g.reg.intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]instMap[uint64, labelBag])
	}
	// EVERY write tests the record's head before the presence guard, for the
	// reason given on [Graph.setEdgeLabelByHandleInfo] (rmp #2947).
	if tx != nil {
		if head := sh.v.headStamp(edgeHandleKey{pair: k, handle: handle}); tx.conflicts(head) {
			_ = tx.conflictErr(mvcc.StoreEdgeTypesHandle, head)
			return nil
		}
	}
	// Both tiers are held BY VALUE; each write-back is load-bearing.
	im := sh.m[k]
	bag, _ := im.get(handle)
	if bag.has(lid) {
		return nil
	}
	if !g.pushHandleLabelVersion(sh, k, handle, tx) {
		// Refused: the conflict is recorded on tx and this write must not land.
		return nil
	}
	bag.add(lid)
	im.set(handle, bag)
	sh.m[k] = im
	return nil
}

// SetEdgePropertyByHandleID records key=value on the edge identified by
// `handle` on the directed (srcID, dstID) NodeID pair. It is the
// NodeID-keyed dual of [Graph.SetEdgePropertyByHandle] used by the
// snapshot/WAL recovery path. No-op when handle is 0.
//
// This method is intentionally called only by the snapshot/WAL recovery
// path and bypasses the SchemaValidator: values replayed here were
// validated at the time of the original write and must not fail during
// recovery.
//
// SetEdgePropertyByHandleID is safe for concurrent use.
//
// It refuses a property key longer than [MaxTokenLen] bytes with an error wrapping
// [ErrTokenTooLong], before changing any state (rmp #2748). The error return is a
// breaking change: SetEdgePropertyByHandleID used to return nothing.
//
// It runs as a single-operation transaction (rmp #2947): it refuses with an
// error wrapping [ErrDirectWriteConflict], and changes nothing, while another
// transaction holds an uncommitted write on the instance's per-handle properties. The refusal is retryable. See
// [ErrDirectWriteConflict].
func (g *Graph[N, W]) SetEdgePropertyByHandleID(srcID, dstID graph.NodeID, handle uint64, key string, value PropertyValue) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	return g.direct(func(tx *writeCtx) error {
		return g.setEdgePropertyByHandleIDInfo(srcID, dstID, handle, key, value, tx)
	})
}

// setEdgePropertyByHandleIDInfo is [Graph.SetEdgePropertyByHandleID] inside write transaction tx; tx is
// nil only on a graph whose versioning substrate is disarmed. See [writeCtx].
func (g *Graph[N, W]) setEdgePropertyByHandleIDInfo(srcID, dstID graph.NodeID, handle uint64, key string, value PropertyValue, tx *writeCtx) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	if handle == 0 {
		return nil
	}
	pid := g.pkeys.intern(key)
	k := edgeKey{src: srcID, dst: dstID}
	// Latch BEFORE the lock; see [Graph.anyHandleProp] and the sibling comment
	// in setEdgePropertyByHandleInfo. This is the recovery/snapshot-replay
	// writer, so it is also the path that must re-latch a graph rebuilt from
	// durable state — a restored by-handle property is a written one.
	g.anyHandleProp.Store(true)
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]instMap[uint64, propBag])
	}
	// Both tiers are held BY VALUE; each write-back is load-bearing.
	im := sh.m[k]
	bag, _ := im.get(handle)
	if !g.pushHandlePropVersion(sh, k, handle, tx) {
		return nil
	}
	bag.set(pid, value)
	im.set(handle, bag)
	sh.m[k] = im
	return nil
}

// DelEdgePropertyByHandleID removes exactly key from the property bag of the
// edge identified by `handle` on the directed (srcID, dstID) NodeID pair. It
// is the NodeID-keyed dual of [Graph.DelEdgePropertyByHandle], provided for
// parity with the other durable NodeID-keyed setters; the WAL recovery path
// itself uses the natural-key [Graph.DelEdgePropertyByHandle] because its
// endpoints are codec-decoded natural keys at replay time. No-op when handle
// is 0, when the key was never interned, when no handle store exists for the
// pair, or when the handle never carried key. Empties are pruned exactly as
// [Graph.DelEdgePropertyByHandle] prunes them.
//
// DelEdgePropertyByHandleID is safe for concurrent use.
//
// It refuses a property key longer than [MaxTokenLen] bytes with an error wrapping
// [ErrTokenTooLong] and changes nothing (rmp #2748): no such token can exist,
// and the WAL-backed store refuses the same call. The error return is a
// breaking change: DelEdgePropertyByHandleID used to return nothing.
//
// It runs as a single-operation transaction (rmp #2947): it refuses with an
// error wrapping [ErrDirectWriteConflict], and changes nothing, while another
// transaction holds an uncommitted write on the instance's per-handle properties. The refusal is retryable. See
// [ErrDirectWriteConflict].
func (g *Graph[N, W]) DelEdgePropertyByHandleID(srcID, dstID graph.NodeID, handle uint64, key string) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	return g.direct(func(tx *writeCtx) error {
		g.delEdgePropertyByHandleIDInfo(srcID, dstID, handle, key, tx)
		return nil
	})
}

// delEdgePropertyByHandleIDInfo is [Graph.DelEdgePropertyByHandleID] inside write transaction tx; tx is
// nil only on a graph whose versioning substrate is disarmed. See [writeCtx].
func (g *Graph[N, W]) delEdgePropertyByHandleIDInfo(srcID, dstID graph.NodeID, handle uint64, key string, tx *writeCtx) {
	if handle == 0 {
		return
	}
	pid, ok := g.pkeys.Lookup(key)
	if !ok {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// A missing map entry reads as the zero instMap, whose get reports no record.
	im := sh.m[k]
	bag, ok := im.get(handle)
	if !ok {
		// No bag to change is still a write to conflict-test (rmp #2943).
		g.checkHandlePropConflict(sh, k, handle, tx)
		return
	}
	if !g.pushHandlePropVersion(sh, k, handle, tx) {
		return
	}
	if bag.del(pid) {
		im.del(handle)
		if im.len() == 0 {
			delete(sh.m, k)
		} else {
			sh.m[k] = im
		}
		return
	}
	im.set(handle, bag)
	sh.m[k] = im
}

// WalkEdgeHandlesAsOf is [Graph.WalkEdgeHandles] resolving every node's adjacency AS
// OF s rather than as of the present (rmp #2310).
//
// A nil snapshot walks the current entries, so it is exactly [Graph.WalkEdgeHandles]
// and a caller may pass whatever it has.
//
// It exists for the checkpoint capture, which must read every structure at ONE
// transactional instant while writers keep committing. The unversioned form reads
// each node's LIVE entry, so a capture using it would fold the handles of a
// transaction that committed part-way through the walk — the partial-transaction
// image that the exclusion this task removes used to prevent.
//
// Nodes are visited in mapper order, which is the order the unversioned form uses;
// a node that did not exist at s simply has no entry as of s and contributes
// nothing, so existence needs no separate test here.
//
// Safe for concurrent use.
func (g *Graph[N, W]) WalkEdgeHandlesAsOf(s *Snapshot, fn func(EdgeHandleTriple) bool) {
	if s == nil {
		g.walkEdgeHandlesRaw(fn)
		return
	}
	adj := g.adj
	adj.Mapper().Walk(func(srcID graph.NodeID, _ N) bool {
		v := adj.EntryViewAsOf(srcID, s.startTS, s.txID)
		if v.Handles == nil {
			return true
		}
		for i, dstID := range v.Neighbours {
			if i >= len(v.Handles) {
				break
			}
			h := v.Handles[i]
			if h == 0 {
				continue
			}
			if !fn(EdgeHandleTriple{Src: srcID, Dst: dstID, Handle: h}) {
				return false
			}
		}
		return true
	})
}
