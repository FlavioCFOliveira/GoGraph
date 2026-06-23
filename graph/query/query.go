// Package query provides a fluent, type-safe programmatic API for
// expressing MATCH-style pattern queries against a labelled property
// graph snapshot.
//
// The API is intentionally minimal in v1: it covers the high-value
// "MATCH (n:Label1) WHERE n.prop = v RETURN n" pattern and its
// single-hop extension "(:Label1)-[]->(:Label2)". Multi-hop chains
// compose via repeated [Pattern.Out] / [Pattern.Filter] calls; the
// engine transparently uses the [lpg.Graph]'s NodeIndex (Roaring
// bitmaps) when a [WithLabel] predicate seeds the pattern.
//
// A future iteration will plug in [graph/index.Manager] so the
// planner can choose between hash, btree, and full-scan plans based
// on cardinality estimates.
//
// # Concurrency
//
// An [Engine] is read-only and takes no lock across pattern steps, so it
// is safe for concurrent use by multiple goroutines only while the
// underlying [lpg.Graph] and CSR snapshot are quiescent (no concurrent
// mutation); this is the same quiescence the CSR snapshot already
// requires. A [Pattern] is a single MATCH expression under construction:
// it is mutated in place by each builder call ([Pattern.Vertex],
// [Pattern.Out]) and is NOT safe for concurrent use — a Pattern is
// owned by one goroutine.
package query

import (
	"iter"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Engine bundles an [lpg.Graph] with its CSR snapshot for read-only
// query execution. The CSR is used for adjacency traversal; the LPG
// is used for label / property lookups.
//
// Removed nodes are invisible: every working-set construction step
// (seeding, label intersection, [Pattern.Out] expansion) prunes
// NodeIDs that [lpg.Graph.IsTombstoned] reports as removed, so
// [Pattern.Cardinality], [Pattern.Collect], and [Pattern.NodeIDs]
// never observe deleted state. Each pattern step reads the tombstone
// set at the moment it executes; the Engine takes no lock across
// steps, so callers that mutate the graph concurrently with query
// construction must serialise externally (the same quiescence the CSR
// snapshot already requires).
type Engine[N comparable, W any] struct {
	g   *lpg.Graph[N, W]
	csr *csr.CSR[W]
}

// New returns an Engine wrapping g and the CSR snapshot c.
func New[N comparable, W any](g *lpg.Graph[N, W], c *csr.CSR[W]) *Engine[N, W] {
	return &Engine[N, W]{g: g, csr: c}
}

// Predicate is the type-safe interface a Vertex constraint
// implements. Implementations may consult the [lpg.Graph] freely;
// returning true keeps the NodeID in the working set.
type Predicate[N comparable, W any] interface {
	Match(g *lpg.Graph[N, W], id graph.NodeID) bool
}

// withLabel matches nodes carrying a given label.
type withLabel[N comparable, W any] struct{ name string }

func (p withLabel[N, W]) Match(g *lpg.Graph[N, W], id graph.NodeID) bool {
	n, ok := g.AdjList().Mapper().Resolve(id)
	if !ok {
		return false
	}
	return g.HasNodeLabel(n, p.name)
}

// WithLabel returns a [Predicate] selecting nodes carrying the
// given label.
func WithLabel[N comparable, W any](name string) Predicate[N, W] {
	return withLabel[N, W]{name: name}
}

// withProperty matches nodes whose given property equals an expected
// PropertyValue under the graph's PropertyValue comparison rules.
type withProperty[N comparable, W any] struct {
	key      string
	expected lpg.PropertyValue
}

func (p withProperty[N, W]) Match(g *lpg.Graph[N, W], id graph.NodeID) bool {
	n, ok := g.AdjList().Mapper().Resolve(id)
	if !ok {
		return false
	}
	v, ok := g.GetNodeProperty(n, p.key)
	if !ok {
		return false
	}
	return equalValue(v, p.expected)
}

// WithProperty returns a [Predicate] selecting nodes whose named
// property matches the given expected value (kind and value-equality
// both required).
func WithProperty[N comparable, W any](key string, expected lpg.PropertyValue) Predicate[N, W] {
	return withProperty[N, W]{key: key, expected: expected}
}

// equalValue compares two PropertyValues by kind and value.
func equalValue(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		x, _ := a.String()
		y, _ := b.String()
		return x == y
	case lpg.PropInt64:
		x, _ := a.Int64()
		y, _ := b.Int64()
		return x == y
	case lpg.PropFloat64:
		x, _ := a.Float64()
		y, _ := b.Float64()
		return x == y
	case lpg.PropBool:
		x, _ := a.Bool()
		y, _ := b.Bool()
		return x == y
	case lpg.PropTime:
		x, _ := a.Time()
		y, _ := b.Time()
		return x.Equal(y)
	case lpg.PropBytes:
		x, _ := a.Bytes()
		y, _ := b.Bytes()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return false
}

// Pattern is a single MATCH expression under construction. Its working set
// (the current NodeID bitmap) is mutated in place by each builder call
// ([Pattern.Vertex], [Pattern.Out]), so a Pattern is NOT safe for
// concurrent use: a Pattern is owned by one goroutine.
type Pattern[N comparable, W any] struct {
	engine *Engine[N, W]
	bm     *roaring64.Bitmap // current working set (NodeIDs)
}

// Match opens a new MATCH expression seeded with every live node in
// the graph: interned NodeIDs only (never the ghost slots the sharded
// id packing leaves in [0, MaxNodeID)), minus the tombstoned set.
func (e *Engine[N, W]) Match() *Pattern[N, W] {
	return &Pattern[N, W]{engine: e}
}

// Vertex constrains the working set by the conjunction of preds.
// The first call with a [WithLabel] predicate uses the LPG's
// label index (Roaring intersect) for the planner's fast path;
// subsequent calls fall back to a per-node scan.
func (p *Pattern[N, W]) Vertex(preds ...Predicate[N, W]) *Pattern[N, W] {
	if p.bm == nil {
		p.bm = p.seedFromPreds(preds)
		// Apply remaining non-label predicates (already filtered by seed)
		p.bm = p.filterByPreds(p.bm, preds, true)
		return p
	}
	p.bm = p.filterByPreds(p.bm, preds, false)
	return p
}

// seedFromPreds builds the initial working set. When at least one
// predicate is a WithLabel, we intersect the corresponding bitmaps
// directly from the LPG NodeIndex — orders-of-magnitude faster than
// scanning every node in the graph. Either way the seed is pruned of
// tombstoned (removed) NodeIDs, so a deleted node never enters the
// working set even when its labels were not stripped from the
// NodeIndex before removal.
func (p *Pattern[N, W]) seedFromPreds(preds []Predicate[N, W]) *roaring64.Bitmap {
	var labelIDs []uint32
	for _, pr := range preds {
		if lab, ok := pr.(withLabel[N, W]); ok {
			lid, exists := p.engine.g.Registry().Lookup(lab.name)
			if !exists {
				return roaring64.New()
			}
			labelIDs = append(labelIDs, uint32(lid))
		}
	}
	if len(labelIDs) > 0 {
		bm := p.engine.g.NodeIndex().Intersect(labelIDs...)
		p.engine.pruneTombstones(bm)
		return bm
	}
	return p.engine.seedAllLive()
}

// seedChunkSize is the flush threshold for the Mapper.Walk → AddMany
// buffer used by seedAllLive. 4096 ids (32 KiB) keeps the intermediate
// allocation bounded regardless of graph size while amortising the
// per-call overhead of roaring AddMany.
const seedChunkSize = 4096

// seedAllLive returns a bitmap holding every interned, non-tombstoned
// NodeID in the graph. The Mapper packs NodeIDs as (intra<<8)|shard
// across 256 shards, so the id space is sparse: Mapper.MaxNodeID()
// rounds up to maxIntra*256 and a blanket [0, MaxNodeID) range would
// count never-interned ghost slots (a 3-node graph would seed 256
// ids). Walking the Mapper enumerates exactly the interned ids in
// O(V) instead.
//
// The Walk callback only appends to a local buffer and never re-enters
// the Mapper or takes any lpg lock, satisfying Mapper.Walk's
// re-entrancy contract; tombstone pruning runs after Walk returns.
func (e *Engine[N, W]) seedAllLive() *roaring64.Bitmap {
	bm := roaring64.New()
	buf := make([]uint64, 0, seedChunkSize)
	e.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ N) bool {
		buf = append(buf, uint64(id))
		if len(buf) == seedChunkSize {
			bm.AddMany(buf)
			buf = buf[:0]
		}
		return true
	})
	if len(buf) > 0 {
		bm.AddMany(buf)
	}
	e.pruneTombstones(bm)
	return bm
}

// pruneTombstones removes every tombstoned NodeID from bm in place.
// The lock-free TombstoneCount gate keeps the common never-deleted
// case free of the tombstone lock and the TombstonedIDs allocation.
func (e *Engine[N, W]) pruneTombstones(bm *roaring64.Bitmap) {
	if e.g.TombstoneCount() == 0 {
		return
	}
	for _, id := range e.g.TombstonedIDs() {
		bm.Remove(uint64(id))
	}
}

// filterByPreds removes NodeIDs that fail any predicate. When
// skipLabel is true, WithLabel predicates are skipped (already
// applied by seedFromPreds).
//
// Before the per-node scan, any property/range predicate that a covering
// secondary index can serve is satisfied by an index seek that intersects the
// match set into bm in place (see seekIndexablePreds and index_seek.go). A
// served predicate is then skipped by the scan loop: the index is the
// authoritative mirror of the graph for its (label, property) pair, so seek and
// scan return the identical set, and the seek replaces an O(working-set)
// property read with an O(log n)/O(1) lookup plus a bitmap intersection.
// Predicates with no covering index keep the per-node scan path unchanged.
func (p *Pattern[N, W]) filterByPreds(bm *roaring64.Bitmap, preds []Predicate[N, W], skipLabel bool) *roaring64.Bitmap {
	served := p.seekIndexablePreds(bm, preds)
	out := roaring64.New()
	it := bm.Iterator()
	for it.HasNext() {
		id := graph.NodeID(it.Next())
		keep := true
		for i, pr := range preds {
			if served[i] {
				continue
			}
			if _, isLabel := pr.(withLabel[N, W]); isLabel && skipLabel {
				continue
			}
			if !pr.Match(p.engine.g, id) {
				keep = false
				break
			}
		}
		if keep {
			out.Add(uint64(id))
		}
	}
	return out
}

// seekIndexablePreds attempts to serve every property and range predicate in
// preds from a covering secondary index, intersecting each index result into bm
// in place, and returns a parallel slice marking which predicates were served
// (and so must be skipped by the per-node scan in filterByPreds). A predicate
// with no covering index — or when the graph has no index manager — is left
// for the scan: its entry in the returned slice stays false. Label predicates
// are never index-served here (the label seed already applied them). The label
// names the predicate set constrains scope which bound indexes may serve a
// property seek (a bound index is label-scoped).
func (p *Pattern[N, W]) seekIndexablePreds(bm *roaring64.Bitmap, preds []Predicate[N, W]) []bool {
	served := make([]bool, len(preds))
	labels := labelsInPreds(preds)
	for i, pr := range preds {
		switch pred := pr.(type) {
		case withProperty[N, W]:
			served[i] = p.trySeekProperty(bm, pred, labels)
		case withRange[N, W]:
			served[i] = p.trySeekRange(bm, pred, labels)
		}
	}
	return served
}

// Out expands the working set to the out-neighbours of every node
// in it. Neighbours that have been tombstoned since the CSR snapshot
// was built are pruned: the snapshot still stores their incident
// edges, but a removed node must never re-enter the working set.
func (p *Pattern[N, W]) Out() *Pattern[N, W] {
	if p.bm == nil {
		p.bm = roaring64.New()
		return p
	}
	next := roaring64.New()
	verts := p.engine.csr.VerticesSlice()
	edges := p.engine.csr.EdgesSlice()
	it := p.bm.Iterator()
	for it.HasNext() {
		src := uint64(it.Next())
		if src+1 >= uint64(len(verts)) {
			continue
		}
		for k := verts[src]; k < verts[src+1]; k++ {
			next.Add(uint64(edges[k]))
		}
	}
	p.engine.pruneTombstones(next)
	p.bm = next
	return p
}

// Cardinality returns the size of the current working set.
func (p *Pattern[N, W]) Cardinality() uint64 {
	if p.bm == nil {
		return 0
	}
	return p.bm.GetCardinality()
}

// Collect returns the user-facing N values in the working set.
func (p *Pattern[N, W]) Collect() []N {
	if p.bm == nil {
		return nil
	}
	out := make([]N, 0, p.bm.GetCardinality())
	for v := range p.NodeIDs() {
		n, ok := p.engine.g.AdjList().Mapper().Resolve(v)
		if ok {
			out = append(out, n)
		}
	}
	return out
}

// NodeIDs returns an iterator over the NodeIDs in the working set.
func (p *Pattern[N, W]) NodeIDs() iter.Seq[graph.NodeID] {
	return func(yield func(graph.NodeID) bool) {
		if p.bm == nil {
			return
		}
		it := p.bm.Iterator()
		for it.HasNext() {
			if !yield(graph.NodeID(it.Next())) {
				return
			}
		}
	}
}
