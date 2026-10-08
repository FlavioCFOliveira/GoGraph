package lpg

// edge_property.go — the public per-pair edge-property surface, backed by the
// columnar tier (sprint 222, design D1 in docs/columnar-edge-properties-design.md).
//
// The map[edgeKey]propBag store that previously held one boxed property bag per
// (src,dst) pair is retired. Edge properties now live in an opaque, immutable,
// per-source-node columnar block ([edgePropCols]) carried inside the adjacency
// entry as its [adjlist.AuxColumn], with one de-boxed typed column per
// (propertyKeyID, kind). See edge_property_column.go for the block.
//
// # Per-pair contract preserved exactly
//
// The public surface is unchanged: [Graph.EdgeProperties] returns one coalesced,
// latest-wins map per (src,dst), folding any parallel edges; [Graph.GetEdgeProperty],
// [Graph.SetEdgeProperty], and [Graph.DelEdgeProperty] keep their semantics. The
// reconciliation between the per-slot columns and the per-pair surface is:
//
//   - WRITE: SetEdgeProperty writes the value to EVERY adjacency slot of src
//     whose neighbour is dst (all parallel edges to dst get the identical value).
//   - READ: EdgeProperties / GetEdgeProperty COALESCE across every dst-matching
//     slot, latest slot winning per key. Because the write fans out to all
//     dst-matching slots, the live slots carry the identical set; a slot that was
//     appended after the last write (and so is still absent) simply contributes
//     nothing, which is exactly what the per-pair coalesce expects. This makes
//     the derived per-pair view byte-identical to the old single-bag-per-pair map.
//   - DELETE: DelEdgeProperty clears the key on EVERY dst-matching slot.
//
// Because SetEdgeProperty is gated on the edge existing (HasEdge), a property
// only ever lives on a live adjacency slot, and the per-pair state is dropped
// when the last edge between the pair is removed (clearPairSides). There is
// therefore no orphan tier for properties (unlike relationship labels, whose
// RemoveEdgeLabel can be called on an absent edge).

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// SetEdgeProperty records the named property on the directed edge
// (src, dst). The edge must already exist; otherwise the call is a
// no-op (mirroring SetEdgeLabel). Returns any error returned by the
// installed [SchemaValidator]; when the validator rejects the write the
// graph state is left unchanged.
//
// The value is written into the per-slot columnar block of src at every slot
// whose neighbour is dst, so the per-pair view coalesces to the latest value
// for the key. The write is copy-on-write under the adjacency shard lock: a new
// immutable column block is built with every dst-matching slot updated and is
// published with a single atomic store, so a concurrent lock-free reader
// observes either the prior block or the fully-updated one.
//
// It refuses a property key longer than [MaxTokenLen] bytes with an error wrapping
// [ErrTokenTooLong], before changing any state (rmp #2748).
//
// It runs as a single-operation transaction (rmp #2947): it refuses with an
// error wrapping [ErrDirectWriteConflict], and changes nothing, while another
// transaction holds an uncommitted write on src's adjacency entry. The refusal is retryable. See
// [ErrDirectWriteConflict].
func (g *Graph[N, W]) SetEdgeProperty(src, dst N, key string, value PropertyValue) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	return g.direct(func(tx *writeCtx) error { return g.setEdgePropertyInfo(src, dst, key, value, tx) })
}

// setEdgePropertyInfo is [Graph.SetEdgeProperty] inside write transaction tx; tx
// is nil only on a graph whose versioning substrate is disarmed. See [writeCtx].
//
// It exists because the columnar edge-property write goes through the ADJACENCY
// ENTRY — [adjlist.AdjList.UpdateEntryAux] publishes a whole new entry carrying
// the rebuilt block — and not through any node-side store, so it had no
// transaction-carrying form when rmp #2301 built the rest of them. Without it a
// statement that set a relationship property had that write stamped with
// whichever transaction the ambient slot named, splitting the statement across
// two commit records (rmp #2320).
//
// It CLAIMS the source node's adjacency ([adjVersions.noteExclusive]) before it
// reads or rebuilds the entry, like every other write that rebuilds one
// (rmp #2966). An adjacency entry is an immutable snapshot of every edge out of
// the node, so a write that rebuilds it embeds whatever the entry holds; without
// the claim an explicit transaction rebuilt an entry another explicit
// transaction had not committed, carried that transaction's arcs into its own
// commit, and made the other's abort unrecoverable. The claim makes the node the
// unit of conflict, exactly as it is for appends since rmp #2445: two
// transactions setting properties on two relationships out of the same node
// now conflict, and the second retries.
func (g *Graph[N, W]) setEdgePropertyInfo(src, dst N, key string, value PropertyValue, tx *writeCtx) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	if err := g.adjVer.noteExclusive(srcID, tx); err != nil {
		return err
	}
	// The presence guard reads the PRESENT entry, which may be showing another
	// transaction's uncommitted removal. A direct write leaves the decision to
	// the column update instead (rmp #2947): it tests the entry under its shard
	// lock before it looks for a slot, and finds none to change only once no
	// uncommitted entry can be hiding one.
	if !tx.implicit() && !g.HasEdgeAsOf(src, dst, nil) { // stored entry: see own writes
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	keyID := g.pkeys.intern(key)
	_, err := g.adj.Writer(tx.adjTx()).UpdateEntryAux(srcID, func(cur adjlist.AuxColumn, neighbours []graph.NodeID) (adjlist.AuxColumn, bool) {
		block := asEdgePropCols(cur)
		length := len(neighbours)
		changed := false
		for i, nb := range neighbours {
			if nb != dstID {
				continue
			}
			block = block.set(keyID, i, length, value)
			changed = true
		}
		if !changed {
			return cur, false
		}
		return block, true
	})
	return adjErr(tx, err)
}

// GetEdgeProperty returns the property value attached to the
// directed edge (src, dst) under key. When several parallel edges connect the
// pair the latest-winning value across their slots is returned (the slots carry
// the identical value by the SetEdgeProperty fan-out, so this is well-defined).
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) GetEdgeProperty(src, dst N, key string) (PropertyValue, bool) {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.GetEdgePropertyAsOf(src, dst, key, g.latestCommitted(&cs))
}

// GetEdgePropertyAsOf is [Graph.GetEdgeProperty] as the edge stood at snap. A
// nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) GetEdgePropertyAsOf(src, dst N, key string, snap *Snapshot) (PropertyValue, bool) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return PropertyValue{}, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return PropertyValue{}, false
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	// ONE entry, so the neighbours and the columnar block are the same version.
	v := g.EntryViewAsOf(srcID, snap)
	block := asEdgePropCols(v.Aux)
	if block == nil {
		return PropertyValue{}, false
	}
	nbs := v.Neighbours
	n := minInt(len(nbs), block.lenOrZero())
	var out PropertyValue
	found := false
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		if v, present := block.get(keyID, i); present {
			out, found = v, true // latest dst-matching slot wins
		}
	}
	return out, found
}

// EdgeHasProperty reports whether the directed edge (src, dst) carries a value
// under key that would materialise to a NON-NULL Cypher value — without
// building that value. It is the storage-presence fast path behind a bound
// relationship's `r.key IS NOT NULL` / `IS NULL` predicate: the caller needs
// only the boolean presence, so fetching and boxing the value (as
// [Graph.GetEdgeProperty] / [Graph.EdgeProperties] would) is pure waste.
//
// Congruence with the value path is BY CONSTRUCTION, on two axes:
//
//   - Per-pair coalescing — it folds parallel edges exactly as
//     [Graph.EdgeProperties] does: the LATEST dst-matching adjacency slot that
//     carries key wins. The returned answer therefore reflects the same single
//     coalesced value the Cypher evaluator would observe via EdgeProperties,
//     never an earlier shadowed write.
//   - Kind gating — the winning slot's storage kind is tested with
//     [kindMapsToNonNullCypher], which mirrors cypher.lpgPropToExpr's
//     nullability table. A present-but-null-mapping property (a stored PropTime
//     or PropBytes) reads as Null through Cypher, so this reports false for it,
//     exactly as `r.key IS NOT NULL` would evaluate to false.
//
// The scan reads only validity bits and per-column kind tags (no value cell), so
// it allocates nothing. Returns false when either endpoint or key is unknown, or
// when no dst-matching slot carries a non-null-mapping value for key.
//
// Concurrency-safe under the same lock-free contract as [Graph.GetEdgeProperty]:
// it reads an immutable published columnar block and bounds its scan by the
// shorter of the block and the neighbours snapshot, so a concurrent copy-on-write
// writer is observed atomically (old block or new, never half-built).
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) EdgeHasProperty(src, dst N, key string) bool {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.EdgeHasPropertyAsOf(src, dst, key, g.latestCommitted(&cs))
}

// EdgeHasPropertyAsOf is [Graph.EdgeHasProperty] as the edge stood at snap. A
// nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgeHasPropertyAsOf(src, dst N, key string, snap *Snapshot) bool {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return false
	}
	// ONE entry, so the neighbours and the columnar block are the same version.
	v := g.EntryViewAsOf(srcID, snap)
	block := asEdgePropCols(v.Aux)
	if block == nil {
		return false
	}
	nbs := v.Neighbours
	n := minInt(len(nbs), block.lenOrZero())
	found := false
	var winning PropertyKind
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		if k, present := block.slotKind(keyID, i); present {
			winning, found = k, true // latest dst-matching slot wins
		}
	}
	if !found {
		return false
	}
	return kindMapsToNonNullCypher(winning)
}

// DelEdgeProperty removes the named property from the directed edge
// (src, dst). No-op if absent. The key is cleared on every dst-matching slot so
// the per-pair view no longer reports it.
//
// It refuses a property key longer than [MaxTokenLen] bytes with an error wrapping
// [ErrTokenTooLong] and changes nothing (rmp #2748): no such token can exist,
// and the WAL-backed store refuses the same call. The error return is a
// breaking change: DelEdgeProperty used to return nothing.
//
// It runs as a single-operation transaction (rmp #2947): it refuses with an
// error wrapping [ErrDirectWriteConflict], and changes nothing, while another
// transaction holds an uncommitted write on src's adjacency entry. The refusal is retryable. See
// [ErrDirectWriteConflict].
func (g *Graph[N, W]) DelEdgeProperty(src, dst N, key string) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	return g.direct(func(tx *writeCtx) error {
		g.delEdgePropertyInfo(src, dst, key, tx)
		return nil
	})
}

// delEdgePropertyInfo is [Graph.DelEdgeProperty] inside write transaction tx; tx
// is nil only on a graph whose versioning substrate is disarmed. It is the
// removal half of [Graph.setEdgePropertyInfo] and exists for exactly the same
// reason — see there for why this store had no transaction-carrying form and
// what refuses it.
func (g *Graph[N, W]) delEdgePropertyInfo(src, dst N, key string, tx *writeCtx) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return
	}
	// A removal that finds the key on no slot of the pair changes nothing, so it
	// takes no claim (rmp #3006): a claim is a write stamp on src's WHOLE entry,
	// and taking one for an absent key refused every concurrent writer of src —
	// another arc, another pair's property — over a change that never happened,
	// spent a commit-record version, and so made the durable path log a removal.
	//
	// The STORED entry is the cheap filter: a key absent from it is absent from
	// every view. It may, though, be showing another transaction's uncommitted
	// removal, or a commit tx cannot see, so "nothing to do" is the verdict only
	// once src's adjacency stamps admit tx and the key is absent from what tx
	// SEES — the rule [Graph.removeAllEdgesFromInfo] applies (ACID audit round 6,
	// finding C1). A peer that wrote the key after tx's snapshot, committed or
	// not, left a stamp admits refuses; the refusal is recorded on tx and dooms
	// it, exactly as the claim's would have.
	if tx != nil && !edgeKeyOnPair(g.EntryViewAsOf(srcID, nil), dstID, keyID) {
		if g.adjVer.admits([2]graph.NodeID{srcID}, 1, tx) != nil {
			return
		}
		// Read after the admit through a view no earlier read of tx has pinned
		// ([Graph.admittedRead], rmp #3032).
		var cs Snapshot
		if !edgeKeyOnPair(g.EntryViewAsOf(srcID, g.admittedRead(&cs, tx)), dstID, keyID) {
			return
		}
	}
	// Claimed like every write that rebuilds the entry; see
	// [Graph.setEdgePropertyInfo]. A refusal is recorded on tx and dooms it.
	if g.adjVer.noteExclusive(srcID, tx) != nil {
		return
	}
	_, err := g.adj.Writer(tx.adjTx()).UpdateEntryAux(srcID, func(cur adjlist.AuxColumn, neighbours []graph.NodeID) (adjlist.AuxColumn, bool) {
		block := asEdgePropCols(cur)
		if block == nil {
			return cur, false
		}
		changed := false
		for i, nb := range neighbours {
			if nb != dstID {
				continue
			}
			next, did := block.del(keyID, i)
			if did {
				block = next
				changed = true
			}
		}
		if !changed {
			return cur, false
		}
		return block, true
	})
	// The refusal is recorded on tx; this primitive returns nothing.
	_ = adjErr(tx, err)
}

// edgeKeyOnPair reports whether keyID is present on any slot of entry v whose
// neighbour is dstID — exactly the slots [Graph.delEdgePropertyInfo] would
// clear. It reads only the validity bitmaps and allocates nothing.
func edgeKeyOnPair[W any](v adjlist.EntryView[W], dstID graph.NodeID, keyID PropertyKeyID) bool {
	block := asEdgePropCols(v.Aux)
	if block == nil {
		return false
	}
	nbs := v.Neighbours
	for i := range minInt(len(nbs), block.lenOrZero()) {
		if nbs[i] == dstID && block.keyPresentAt(keyID, i) {
			return true
		}
	}
	return false
}

// EdgeProperties returns a snapshot of every property currently
// attached to the directed edge (src, dst). When several parallel edges connect
// the pair the result is the latest-wins coalesced union across their slots.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) EdgeProperties(src, dst N) map[string]PropertyValue {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.EdgePropertiesAsOf(src, dst, g.latestCommitted(&cs))
}

// EdgePropertiesAsOf is [Graph.EdgeProperties] as the edge stood at snap.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesAsOf(src, dst N, snap *Snapshot) map[string]PropertyValue {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgePropertiesByIDAsOf(srcID, dstID, snap)
}

// EdgePropertiesByID is the NodeID-keyed counterpart of [Graph.EdgeProperties]:
// it returns the latest-wins coalesced property map of the directed edge
// identified by the endpoint NodeIDs (srcID, dstID), or nil when the pair
// carries no properties. It is the edge dual of [Graph.NodePropertiesByID].
//
// Unlike [Graph.EdgeProperties] it performs NO Mapper access — no external-key →
// NodeID lookup — so a caller that already holds both endpoint NodeIDs can
// resolve edge properties without re-entering the Mapper. This is precisely what
// the snapshot collectors require: they enumerate endpoints from inside
// [graph.Mapper.Walk], which holds a Mapper shard read lock across its callback,
// and the Mapper contract forbids re-entry there while a writer may be running
// (graph/mapper.go:337-345, #1648). The read is served from the lock-free
// immutable adjacency entry, so EdgePropertiesByID is safe for concurrent use.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) EdgePropertiesByID(srcID, dstID graph.NodeID) map[string]PropertyValue {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.EdgePropertiesByIDAsOf(srcID, dstID, g.latestCommitted(&cs))
}

// EdgePropertiesByIDAsOf is [Graph.EdgePropertiesByID] as the edge stood at
// snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesByIDAsOf(srcID, dstID graph.NodeID, snap *Snapshot) map[string]PropertyValue {
	var out map[string]PropertyValue
	g.ForEachEdgePropertyByIDAsOf(srcID, dstID, snap, func(name string, v PropertyValue) {
		if out == nil {
			out = make(map[string]PropertyValue, 2)
		}
		out[name] = v // latest dst-matching slot wins
	})
	return out
}

// ForEachEdgeProperty streams the latest-wins coalesced property set of the
// directed edge (src, dst), invoking visit once per emitted (name, value)
// without building the intermediate per-pair map that [Graph.EdgeProperties]
// returns. It is the allocation-fusing counterpart of [Graph.EdgeProperties],
// the edge analogue of [Graph.NodePropertiesByIDFunc]: a caller that re-keys
// every property into a different map (chiefly the Cypher result path, which
// converts each lpg.PropertyValue into a cypher/expr value) would otherwise
// allocate a throwaway map[string]PropertyValue only to range over it once.
// Streaming the values lets the caller build its target map directly, removing
// that intermediate allocation per relationship row.
//
// visit is called zero times when either endpoint is unknown or the pair carries
// no properties. See [Graph.ForEachEdgePropertyByID] for the coalescing and
// concurrency contract.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) ForEachEdgeProperty(src, dst N, visit func(name string, pv PropertyValue)) {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	g.ForEachEdgePropertyAsOf(src, dst, g.latestCommitted(&cs), visit)
}

// ForEachEdgePropertyAsOf is [Graph.ForEachEdgeProperty] as the edge stood at
// snap.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ForEachEdgePropertyAsOf(src, dst N, snap *Snapshot, visit func(name string, pv PropertyValue)) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	g.ForEachEdgePropertyByIDAsOf(srcID, dstID, snap, visit)
}

// ForEachEdgePropertyByID is the NodeID-keyed counterpart of
// [Graph.ForEachEdgeProperty] and the streaming counterpart of
// [Graph.EdgePropertiesByID]: it invokes visit once per (name, value) of the
// latest-wins coalesced property set of the edge identified by the endpoint
// NodeIDs (srcID, dstID), without materialising the intermediate map.
//
// Like [Graph.EdgePropertiesByID] it performs NO Mapper access, so a caller that
// already holds both endpoint NodeIDs avoids re-entering the Mapper.
//
// Coalescing: the per-pair view folds parallel edges by taking the LATEST
// dst-matching adjacency slot per key. Because the columns are per-slot and a key
// is present in at most one column per slot, visit fires at most once per name
// PER SLOT; across the parallel slots a name MAY be visited more than once, so a
// consumer that needs the single coalesced value MUST apply last-write-wins (the
// last emission for a name is the coalesced winner, exactly as
// [Graph.EdgePropertiesByID] records out[name] = v). A map-building consumer gets
// this for free.
//
// Concurrency-safe under the same lock-free contract as
// [Graph.EdgePropertiesByID]: it reads an immutable, atomically-published
// columnar block and neighbours snapshot and bounds the scan by the shorter of
// the two, so a concurrent copy-on-write writer is observed atomically (old
// snapshot or new, never half-built). Unlike [Graph.NodePropertiesByIDFunc] NO
// lock is held across visit — the reads are lock-free atomic-pointer loads — so
// visit imposes no re-entrancy restriction. The PropertyValue passed to visit is
// a value copy of the immutable cell, so copying it out (or deriving an
// independent value from it) is safe; for the boxed Bytes/List kinds the same
// slice-aliasing caveat as [Graph.GetEdgeProperty] applies.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) ForEachEdgePropertyByID(srcID, dstID graph.NodeID, visit func(name string, pv PropertyValue)) {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	g.ForEachEdgePropertyByIDAsOf(srcID, dstID, g.latestCommitted(&cs), visit)
}

// ForEachEdgePropertyByIDAsOf is [Graph.ForEachEdgePropertyByID] as the edge
// stood at snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// It resolves the neighbours and the columnar block from ONE entry, so the two
// are the same version. The previous form loaded them separately and bounded
// the scan by the shorter length, which kept the index in range but did not
// make the columns agree — adequate under the barrier, not under a snapshot.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ForEachEdgePropertyByIDAsOf(srcID, dstID graph.NodeID, snap *Snapshot, visit func(name string, pv PropertyValue)) {
	v := g.EntryViewAsOf(srcID, snap)
	block := asEdgePropCols(v.Aux)
	if block == nil {
		return
	}
	nbs := v.Neighbours
	// The two columns now come from one entry, so this bound can only be an
	// equality; it is kept because a column may legitimately be shorter when the
	// higher layer attached it after some slots already existed.
	n := minInt(len(nbs), block.lenOrZero())
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		block.forEachAt(i, func(kk PropertyKeyID, v PropertyValue) {
			if name, ok := g.pkeys.Resolve(kk); ok {
				visit(name, v) // latest dst-matching slot wins (caller dedups)
			}
		})
	}
}

// asEdgePropCols narrows the opaque [adjlist.AuxColumn] to the concrete
// [edgePropCols] this package stores there, returning nil when the column is
// absent. The aux column on an LPG adjacency entry is always an *edgePropCols
// (this package is the only writer), so the type assertion never fails for a
// non-nil column; a failed assertion yields nil and is treated as "no
// properties", which is safe.
func asEdgePropCols(c adjlist.AuxColumn) *edgePropCols {
	if c == nil {
		return nil
	}
	b, _ := c.(*edgePropCols)
	return b
}

// minInt returns the smaller of two ints.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
