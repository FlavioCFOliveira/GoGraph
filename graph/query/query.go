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

// Pattern is a single MATCH expression under construction.
type Pattern[N comparable, W any] struct {
	engine *Engine[N, W]
	bm     *roaring64.Bitmap // current working set (NodeIDs)
}

// Match opens a new MATCH expression seeded with every node in the
// graph.
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
// scanning every node in the graph.
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
		return p.engine.g.NodeIndex().Intersect(labelIDs...)
	}
	// No label predicate: seed with all NodeIDs in the CSR snapshot.
	// AddRange is O(1) amortised per container compared to the
	// per-bit Add loop which is O(MaxNodeID) — measurable on
	// million-node graphs.
	bm := roaring64.New()
	if maxID := uint64(p.engine.csr.MaxNodeID()); maxID > 0 {
		bm.AddRange(0, maxID)
	}
	return bm
}

// filterByPreds removes NodeIDs that fail any predicate. When
// skipLabel is true, WithLabel predicates are skipped (already
// applied by seedFromPreds).
func (p *Pattern[N, W]) filterByPreds(bm *roaring64.Bitmap, preds []Predicate[N, W], skipLabel bool) *roaring64.Bitmap {
	out := roaring64.New()
	it := bm.Iterator()
	for it.HasNext() {
		id := graph.NodeID(it.Next())
		keep := true
		for _, pr := range preds {
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

// Out expands the working set to the out-neighbours of every node
// in it.
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
