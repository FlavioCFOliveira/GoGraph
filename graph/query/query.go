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

// withProperty matches nodes whose given property is EQUAL to an expected
// PropertyValue under openCypher's equality relation — see [equalValue] for
// which relation that is and how it treats NaN.
type withProperty[N comparable, W any] struct {
	expected lpg.PropertyValue
	key      string
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

// WithProperty returns a [Predicate] selecting nodes whose named property is
// EQUAL to the given expected value.
//
// # Equality semantics
//
// The relation is openCypher EQUALITY, not comparability and not equivalence:
//
//   - INTEGER and FLOAT are one numeric kind, compared EXACTLY. An
//     [lpg.Int64Value] of 5 is equal to an [lpg.Float64Value] of 5, and an
//     int64 is never widened to float64, so 4611686018427387905 and
//     4611686018427387900 stay distinct even though both round to 2^62.
//   - A NaN expected value is equal to nothing, and nothing is equal to a
//     NaN-valued property — including another NaN. Equality is the relation in
//     which NaN = NaN is FALSE; under equivalence it is true, and that other
//     relation is not what this predicate implements.
//   - STRING, BOOLEAN, BYTES and TIME are equal within their own kind. Note
//     that openCypher's equatability is WIDER than its comparability, so for
//     these kinds this predicate matches where the degenerate [WithRange]
//     [v, v] does not: they are equatable but not ordered scalars.
//   - Every other cross-kind pair is not equal. INTEGER x FLOAT is the sole
//     off-diagonal entry openCypher unifies, so a number is never equal to a
//     string.
//
// # Index acceleration
//
// When the owning graph carries an index covering the predicate's
// (label, property) pair — and the same [Pattern.Vertex] call also constrains
// that label — the engine narrows the working set from the index before any
// property is read. A STRING or BOOLEAN equality is served EXACTLY from a
// bound hash index of that key type and the per-node comparison is skipped; a
// NUMERIC equality is served as a SUPERSET from the float64-keyed numeric btree
// companion and the exact comparison still runs over what the seek left. Either
// way the answer is identical to the answer the same query returns with no
// index present (see index_seek.go).
func WithProperty[N comparable, W any](key string, expected lpg.PropertyValue) Predicate[N, W] {
	return withProperty[N, W]{key: key, expected: expected}
}

// equalValue reports whether a and b are EQUAL under openCypher's equality
// relation.
//
// # Which relation this is
//
// Equality, comparability and equivalence all unify INTEGER and FLOAT, and this
// is the EQUALITY one. The three differ in two ways that matter here:
//
//   - At NaN. Under equality NaN = NaN is FALSE, which is the IEEE-754 outcome
//     openCypher adopts. Under EQUIVALENCE it is TRUE — a value set folds NaN
//     onto itself (cypher/exec/constraints.go floatCanonicalKey) — so that
//     canonical-key path must never be reused as an equality comparator.
//     [compareValues] returns ok=false for a NaN operand, which is exactly the
//     equality answer: not equal.
//   - In WIDTH. openCypher's equatability is WIDER than its comparability.
//     [lpg.PropBool], [lpg.PropBytes] and [lpg.PropTime] values are equal to
//     themselves here, while [compareValues] reports every pair over them as
//     unordered, so no [WithRange] over them can hold. That divergence between
//     an equality and a degenerate range is a property of the two relations and
//     is deliberate.
//
// # INTEGER and FLOAT are one numeric kind for equality (#2601)
//
// A numeric pair — INTEGER/INTEGER, FLOAT/FLOAT, or one of each — is routed
// through [compareValues], the SAME exact comparator [valueInRange] uses. That
// is what makes an equality and the degenerate range [v, v] agree over every
// orderable value, which they did not between #2600 (which unified the range)
// and #2601 (which unified this).
//
// It is EXACT: an int64 is never widened to float64, so 4611686018427387905 and
// 4611686018427387900 stay distinct from each other and from 2^62 even though
// all three share one float64. Every other cross-kind pair — a number against a
// string above all — is not equal, since INTEGER x FLOAT is the sole
// off-diagonal entry openCypher unifies.
func equalValue(a, b lpg.PropertyValue) bool {
	ak, bk := a.Kind(), b.Kind()
	if isNumericKind(ak) && isNumericKind(bk) {
		c, ok := compareValues(a, b)
		return ok && c == 0
	}
	if ak != bk {
		return false
	}
	switch ak {
	case lpg.PropString:
		x, _ := a.String()
		y, _ := b.String()
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
// secondary index can serve is narrowed by an index seek that intersects the
// index result into bm in place (see seekIndexablePreds and index_seek.go).
//
// A predicate is skipped by the scan loop only when the seek was EXACT — a
// STRING or BOOLEAN equality against a hash index of that key type, or a STRING
// range against a string-keyed btree — because there the index is the
// authoritative mirror of the graph for its (label, property) pair and the seek
// replaces an O(working-set) property read with an O(log n)/O(1) lookup plus a
// bitmap intersection.
//
// Every NUMERIC seek is only a SUPERSET of the answer, because the sole index
// shape that can carry both INTEGER and FLOAT under one order is the
// float64-keyed companion, whose keys round above 2^53. That holds for a numeric
// range (#2600) and, since INTEGER and FLOAT are also one kind for equality, for
// a numeric equality (#2601). Such a predicate is NOT skipped: the per-node
// comparison runs over the narrowed working set as the exact residual filter.
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

// seekIndexablePreds narrows bm from a covering secondary index for every
// property and range predicate in preds, intersecting each index result into bm
// in place, and returns a parallel slice marking which predicates the index
// DISCHARGED (and which the per-node scan in filterByPreds must therefore skip).
//
// Narrowing bm and discharging a predicate are two different things. An entry is
// true only when the index result is the answer; when the index result is merely
// a superset of the answer — any NUMERIC seek, equality or range, see
// index_seek.go's "Exact seek vs superset seek" — bm is narrowed but the entry
// stays false, so the per-node comparison still runs over what survived as the
// exact residual filter (#2600, #2601).
//
// A predicate with no covering index — or when the graph has no index manager —
// is left entirely to the scan: bm is untouched and its entry stays false. Label
// predicates are never index-served here (the label seed already applied them).
// The label names the predicate set constrains scope which bound indexes may
// serve a seek (a bound index is label-scoped).
func (p *Pattern[N, W]) seekIndexablePreds(bm *roaring64.Bitmap, preds []Predicate[N, W]) []bool {
	served := make([]bool, len(preds))
	labels := labelsInPreds(preds)
	for i, pr := range preds {
		switch pred := pr.(type) {
		case withProperty[N, W]:
			// As for a range below, the narrowed result is discarded: only
			// exactness decides whether the scan may skip this predicate.
			_, exact := p.trySeekProperty(bm, pred, labels)
			served[i] = exact
		case withRange[N, W]:
			// The narrowed result is discarded on purpose: only exactness decides
			// whether the scan may skip this predicate. An inexact seek has already
			// intersected its superset into bm.
			_, exact := p.trySeekRange(bm, pred, labels)
			served[i] = exact
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
