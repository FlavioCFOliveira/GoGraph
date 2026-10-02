package lpg

// edge_instance_props.go — per-CREATE-instance edge property storage.
//
// Mirror of edge_instance_labels.go for properties. Each CREATE call
// records its property map under the 1-based instance index returned
// by [Graph.IncEdgeCreateCount]. The per-pair [Graph.EdgeProperties]
// surface keeps returning the latest-wins merge (existing behaviour);
// [Graph.EdgePropertiesAt] returns the snapshot captured at one
// specific CREATE.

import (
	"sync"
)

// edgeInstancePropShard holds the per-(src, dst, idx) property bags. The
// innermost per-instance bag is the compact tiered [propBag] (sprint 221,
// #1633), stored by value, so a 1-2-property edge instance pays a small slice
// instead of a ~300 B Go map.
type edgeInstancePropShard struct {
	m map[edgeKey]instMap[int64, propBag]
	// v indexes the pre-image chains of the instances a writer has touched.
	// See [edgeInstanceLabelShard].v.
	v sideVersions[edgeInstanceKey, propBag]
	// mu guards m. Writers (SetEdgePropertyAt, RemoveEdgeInstance) take the
	// write lock; EdgePropertiesAt reads under a read lock so concurrent
	// per-instance property reads on a shard proceed in parallel.
	mu sync.RWMutex
}

// SetEdgePropertyAt records the property `key`=`value` for the directed
// edge instance (src, dst) at the supplied 1-based CREATE index. Returns
// any error returned by the installed [SchemaValidator]; when the validator
// rejects the write the graph state is left unchanged.
//
// SetEdgePropertyAt is safe for concurrent use.//
// It refuses a property key longer than [MaxTokenLen] bytes with an error wrapping
// [ErrTokenTooLong], before changing any state (rmp #2748).
//
// It runs as a single-operation transaction (rmp #2947): it refuses with an
// error wrapping [ErrDirectWriteConflict], and changes nothing, while another
// transaction holds an uncommitted write on the instance's per-ordinal properties. The refusal is retryable. See
// [ErrDirectWriteConflict].
func (g *Graph[N, W]) SetEdgePropertyAt(src, dst N, idx int64, key string, value PropertyValue) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	return g.direct(func(tx *writeCtx) error {
		return g.setEdgePropertyAtInfo(src, dst, idx, key, value, tx)
	})
}

// setEdgePropertyAtInfo is [Graph.SetEdgePropertyAt] inside write transaction tx; tx is
// nil only on a graph whose versioning substrate is disarmed. See [writeCtx].
func (g *Graph[N, W]) setEdgePropertyAtInfo(src, dst N, idx int64, key string, value PropertyValue, tx *writeCtx) error {
	if err := CheckToken("property key", key); err != nil {
		return err
	}
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	if idx <= 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	pid := g.pkeys.intern(key)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeInstancePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]instMap[int64, propBag])
	}
	// Both the instMap and the propBag inside it are stored BY VALUE: mutate
	// local copies and write both back under the shard lock. Each write-back is
	// load-bearing — set may grow or promote either tier.
	im := sh.m[k]
	bag, _ := im.get(idx)
	if !g.pushInstancePropVersion(sh, k, idx, tx) {
		// Refused: the conflict is recorded on tx and this write must not land.
		return nil
	}
	bag.set(pid, value)
	im.set(idx, bag)
	sh.m[k] = im
	return nil
}

// EdgePropertiesAt returns the property map recorded at instance `idx`
// of the directed edge (src, dst). Returns nil when the instance was
// never written or when either endpoint is unknown.
//
// This per-instance store is guarded by its own per-shard mutex and is
// only per-operation atomic: it is NOT cross-store consistent with
// [Graph.EdgeCreateCount], [Graph.EdgeLabelsAt], or the adjacency layer
// outside a transaction barrier. A reader correlating the count of
// populated instance indices with [Graph.EdgeCreateCount] while a
// multi-CREATE multigraph transaction commits can observe a partial
// cross-store state (count ahead of the populated indices). To read a
// consistent cross-store view, take a snapshot with [Graph.BeginRead] and
// resolve every correlated read through the [ReadView] that [Graph.ReadAt]
// returns, releasing it with [Graph.EndRead]. (This used to say "bracket the
// correlated reads in Graph.View"; that method was removed by rmp #2344 — see
// rmp #2379.) Writers must share ONE transaction record for their writes to land
// at one instant; see [Graph.ApplyAtomically]. Also see docs/isolation-design.md.
//
// EdgePropertiesAt is safe for concurrent use.
//
// It reads the newest COMMITTED state: a version no transaction has published
// is stepped back over (rmp #2965, round 5). A transaction reads its own
// writes through [Graph.WriterViewOf].
func (g *Graph[N, W]) EdgePropertiesAt(src, dst N, idx int64) map[string]PropertyValue {
	var cs Snapshot // the read position: newest committed (rmp #2965)
	return g.EdgePropertiesAtAsOf(src, dst, idx, g.latestCommitted(&cs))
}

// EdgePropertiesAtAsOf is [Graph.EdgePropertiesAt] as the instance stood at
// snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesAtAsOf(src, dst N, idx int64, snap *Snapshot) map[string]PropertyValue {
	if idx <= 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeInstancePropShardFor(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	// Inline for the reason given on [Graph.EdgeLabelsByHandleIDAsOf].
	im := sh.m[k]
	bag, ok := im.get(idx)
	if snap != nil && !sh.v.empty() {
		bag, ok = sh.v.asOfSnap(edgeInstanceKey{pair: k, idx: idx}, bag, ok, snap, snap.startTS, snap.txID)
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

// edgeInstancePropShardFor selects the responsible shard.
func (g *Graph[N, W]) edgeInstancePropShardFor(k edgeKey) *edgeInstancePropShard {
	return &g.edgeInstancePropShards[uint64(k.src)&(propMapShards-1)]
}
