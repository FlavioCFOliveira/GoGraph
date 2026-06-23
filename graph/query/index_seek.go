package query

// index_seek.go — secondary-index acceleration for WithProperty predicates
// (task #1651).
//
// A WithProperty equality (and a WithRange range) predicate used to degrade to
// a per-node scan in [Pattern.filterByPreds]: every candidate NodeID was
// resolved, its property fetched, and compared — O(working-set) property reads.
// When the owning [lpg.Graph] carries a secondary index that covers the
// predicate's (label, property) pair, this file routes the predicate through
// that index instead, turning the O(working-set) scan into an O(log n)/O(1)
// seek plus a Roaring bitmap intersection.
//
// # How an index is found (key -> index resolution)
//
// The engine deliberately adds NO read method to [index.Subscriber] or
// [index.Manager]: the type-erased Subscriber interface stays exactly as the
// Cypher engine and every other index user already rely on, so this change is
// additive and cannot break them. Instead it reuses the resolution pattern the
// Cypher engine already uses for its NodeByIndexSeek (cypher/api.go): iterate
// [index.Manager.ListIndexes], filter by [index.Subscriber.Kind], and match the
// (label, property) coverage a bound index advertises through its
// BoundNode() (label, property string, ok bool) method. The concrete value type
// V the [index.Manager] erased is recovered by a small, closed set of type
// assertions against the concrete index's own typed read interface
// (hashLookuper / btreeRanger below), one per supported V — never by re-boxing
// through a generic any-typed read on the Manager.
//
// Bound indexes are always label-scoped, so a covering index exists only when
// the same Vertex predicate set also constrains a label. A property predicate
// with no label sibling, or no covering index, falls through to the scan
// unchanged.
//
// # How a seek result is combined (graph-theory-expert, task #1651)
//
// Combine the index result with the current working set via
// workingSet.And(seek) as the default: the vendored RoaringBitmap/roaring
// intersection is internally container-adaptive (a sorted-merge below a ~64:1
// cardinality ratio, one-sided galloping above it — see setutil.go
// intersection2by2 / onesidedgallopingintersect2by2 — and a word-parallel
// bitset AND for dense containers). A hand-rolled sorted-merge would forfeit
// the galloping and bitset paths and can only tie in the all-small case
// (Lemire et al., arXiv:1402.6407 / 1603.06549). The one justified branch is
// clone-avoidance: for a tiny equality posting list (Cardinality <= smallSeek)
// the ids are drained via the hash index's allocation-light LookupAppend into a
// tiny bitmap that is then ANDed in, avoiding [hash.Index.Lookup]'s clone of
// the whole index bitmap just to AND-and-discard it. Ranges always And (the
// btree returns a fresh, frequently large union bitmap, the regime where And
// wins hardest).
//
// Intersecting — never replacing — the working set is also what keeps the seek
// tombstone-safe: the working set was already pruned of tombstoned NodeIDs by
// the seeding step, and W ∩ P ⊆ W can never reintroduce an id absent from W, so
// any transiently-stale id an index might carry is dropped by the intersection
// and no separate tombstone re-prune of the index result is needed.

import (
	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// smallSeek is the inclusive posting-list cardinality at or below which an
// equality seek drains the ids and probes the working set for membership
// (clone-free) instead of cloning the index bitmap to intersect it. A singleton
// (a unique-property hit, the dominant equality shape) is served with a single
// O(1) membership probe and zero bitmap allocation; above the threshold the
// container-adaptive Roaring And dominates (graph-theory-expert, #1651). The
// value mirrors the small-set crossover the index tier itself uses.
const smallSeek = 16

// boundNodeIndex is satisfied by the concrete hash and btree indexes built with
// NewBound, which advertise the single (label, property) pair they cover. The
// query engine matches that pair against a predicate to decide whether an index
// may serve it, exactly as the Cypher engine's indexCoversNode does. An index
// that does not implement this interface, or reports ok=false (an unbound,
// manually populated index), carries no coverage metadata and is never used to
// serve a query — its contents are not guaranteed to mirror the graph.
type boundNodeIndex interface {
	BoundNode() (label, property string, ok bool)
}

// hashLookuper is the typed equality-read interface of hash.Index[V]. The query
// engine asserts it for each supported V (string / int64 / float64 / bool) to
// recover the value type the index.Manager erased, without any generic read on
// the Manager. Cardinality drives the clone-avoidance branch; LookupAppend and
// Lookup are the two read shapes the combination strategy selects between.
type hashLookuper[V comparable] interface {
	Cardinality(value V) uint64
	LookupAppend(value V, dst []uint64) []uint64
	Lookup(value V) *roaring64.Bitmap
}

// btreeRanger is the typed range-read interface of btree.Index[V]. Only the
// ordered scalar kinds the btree supports are asserted (string / int64 /
// float64); a bool range is meaningless and never attempted.
type btreeRanger[V comparable] interface {
	Range(lo, hi V) *roaring64.Bitmap
}

// withRange matches nodes whose given property is an ordered scalar in the
// inclusive interval [lo, hi]. It is the range counterpart to withProperty:
// when a covering btree index exists the engine serves it with a single
// Range seek, otherwise it falls back to the per-node comparison below.
type withRange[N comparable, W any] struct {
	key    string
	lo, hi lpg.PropertyValue
}

// Match implements the per-node fallback for a range predicate: it keeps a node
// when its property value is the same kind as the bounds and lies within
// [lo, hi] under that kind's natural order. Mixed-kind bounds (lo and hi of
// different kinds) never match. This is the scan path; a covering btree index
// short-circuits it in filterByPreds.
func (p withRange[N, W]) Match(g *lpg.Graph[N, W], id graph.NodeID) bool {
	n, ok := g.AdjList().Mapper().Resolve(id)
	if !ok {
		return false
	}
	v, ok := g.GetNodeProperty(n, p.key)
	if !ok {
		return false
	}
	return valueInRange(v, p.lo, p.hi)
}

// WithRange returns a [Predicate] selecting nodes whose named property is an
// ordered scalar (string, int64, or float64) within the inclusive interval
// [lo, hi]. lo and hi must share the property's kind; bounds of a different
// kind, or of a non-ordered kind (bool, bytes, time, list), match nothing.
//
// When the owning graph carries a btree index covering the predicate's
// (label, property) pair — and the same [Pattern.Vertex] call also constrains
// that label — the engine serves the range from the index in O(log n + k)
// instead of scanning every node in the working set. The index result is
// identical to the scan result.
func WithRange[N comparable, W any](key string, lo, hi lpg.PropertyValue) Predicate[N, W] {
	return withRange[N, W]{key: key, lo: lo, hi: hi}
}

// valueInRange reports whether v lies within [lo, hi] under the kind shared by
// all three values. It returns false unless v, lo, and hi are the same ordered
// kind, so it is the exact scan-side mirror of the btree's typed Range.
func valueInRange(v, lo, hi lpg.PropertyValue) bool {
	if v.Kind() != lo.Kind() || v.Kind() != hi.Kind() {
		return false
	}
	switch v.Kind() {
	case lpg.PropString:
		x, _ := v.String()
		l, _ := lo.String()
		h, _ := hi.String()
		return x >= l && x <= h
	case lpg.PropInt64:
		x, _ := v.Int64()
		l, _ := lo.Int64()
		h, _ := hi.Int64()
		return x >= l && x <= h
	case lpg.PropFloat64:
		x, _ := v.Float64()
		l, _ := lo.Float64()
		h, _ := hi.Float64()
		return x >= l && x <= h
	}
	return false
}

// labelsInPreds collects the label names constrained by the WithLabel
// predicates in preds. A covering bound index is label-scoped, so a property
// seek is only attempted against an index whose label is one of these.
func labelsInPreds[N comparable, W any](preds []Predicate[N, W]) []string {
	var labels []string
	for _, pr := range preds {
		if lab, ok := pr.(withLabel[N, W]); ok {
			labels = append(labels, lab.name)
		}
	}
	return labels
}

// trySeekProperty attempts to satisfy an equality predicate from a covering
// hash index, intersecting the result into bm in place. ok reports whether the
// seek was served by an index; when false bm is untouched and the caller must
// apply the per-node fallback. labels are the label names the predicate set
// constrains (a bound index is label-scoped).
func (p *Pattern[N, W]) trySeekProperty(bm *roaring64.Bitmap, pred withProperty[N, W], labels []string) (ok bool) {
	mgr := p.engine.g.IndexManager()
	if mgr == nil || len(labels) == 0 {
		return false
	}
	for _, name := range mgr.ListIndexes() {
		sub, err := mgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" {
			continue
		}
		if !indexCovers(sub, labels, pred.key) {
			continue
		}
		if seekHashInto(bm, sub, pred.expected) {
			return true
		}
	}
	return false
}

// trySeekRange attempts to satisfy a range predicate from a covering btree
// index, intersecting the result into bm in place. ok reports whether the seek
// was served by an index; when false bm is untouched and the caller must apply
// the per-node fallback.
func (p *Pattern[N, W]) trySeekRange(bm *roaring64.Bitmap, pred withRange[N, W], labels []string) (ok bool) {
	mgr := p.engine.g.IndexManager()
	if mgr == nil || len(labels) == 0 {
		return false
	}
	if pred.lo.Kind() != pred.hi.Kind() {
		return false
	}
	for _, name := range mgr.ListIndexes() {
		sub, err := mgr.GetIndex(name)
		if err != nil || sub.Kind() != "btree" {
			continue
		}
		if !indexCovers(sub, labels, pred.key) {
			continue
		}
		if seekRangeInto(bm, sub, pred.lo, pred.hi) {
			return true
		}
	}
	return false
}

// indexCovers reports whether sub is a bound index covering (label, propKey)
// for one of the candidate labels. An index without coverage metadata (an
// unbound, manually populated index) is NOT used: unlike the Cypher engine —
// whose name convention historically blessed such indexes — the query engine
// has no name contract, so it serves a predicate only from an index it can
// prove mirrors the graph. Matching is case-sensitive on both label and
// property (the LPG keys are case-sensitive).
func indexCovers(sub index.Subscriber, labels []string, propKey string) bool {
	b, ok := sub.(boundNodeIndex)
	if !ok {
		return false
	}
	bl, bp, bound := b.BoundNode()
	if !bound || bp != propKey {
		return false
	}
	for _, l := range labels {
		if bl == l {
			return true
		}
	}
	return false
}

// seekHashInto recovers the hash index's value type by asserting the typed
// hashLookuper for each supported scalar kind, runs the seek for the matching
// PropertyValue kind, and intersects the matches into bm in place. ok reports
// whether a supported (index V, value kind) pair was found and served. A
// kind/V mismatch (e.g. a string value against an int64 index) returns false so
// the caller falls back to the scan, which yields the same (empty under
// openCypher type rules) result a seek would.
func seekHashInto(bm *roaring64.Bitmap, sub index.Subscriber, value lpg.PropertyValue) (ok bool) {
	switch value.Kind() {
	case lpg.PropString:
		if idx, isT := sub.(hashLookuper[string]); isT {
			v, _ := value.String()
			intersectHashEq(bm, idx, v)
			return true
		}
	case lpg.PropInt64:
		if idx, isT := sub.(hashLookuper[int64]); isT {
			v, _ := value.Int64()
			intersectHashEq(bm, idx, v)
			return true
		}
	case lpg.PropFloat64:
		if idx, isT := sub.(hashLookuper[float64]); isT {
			v, _ := value.Float64()
			intersectHashEq(bm, idx, v)
			return true
		}
	case lpg.PropBool:
		if idx, isT := sub.(hashLookuper[bool]); isT {
			v, _ := value.Bool()
			intersectHashEq(bm, idx, v)
			return true
		}
	}
	return false
}

// intersectHashEq narrows bm to the NodeIDs the hash index associates with v.
// The operation is always bm <- bm ∩ index(v), so it can only remove ids and
// never introduces one outside the already-pruned working set.
//
// For a tiny posting list (Cardinality <= smallSeek) it drains the ids via the
// hash index's allocation-light LookupAppend into a small reused slice and ANDs
// that tiny set into bm — avoiding [hash.Index.Lookup], which would clone the
// whole index bitmap just to intersect-and-discard it. This keeps the dominant
// singleton/small equality seek free of a full-bitmap clone. Above the
// threshold the index bitmap is materialised once (Lookup, already a
// caller-owned clone) and the container-adaptive Roaring And dominates
// (graph-theory-expert, #1651).
func intersectHashEq[V comparable](bm *roaring64.Bitmap, idx hashLookuper[V], v V) {
	if idx.Cardinality(v) <= smallSeek {
		// Clone-free path: drain the small posting list into a tiny bitmap and
		// AND it in — no full index-bitmap clone. The AND, not the drain, does
		// the intersection, so a stale id the index might carry that is not in
		// the working set is dropped exactly as on the Lookup path.
		ids := idx.LookupAppend(v, make([]uint64, 0, smallSeek))
		small := roaring64.New()
		small.AddMany(ids)
		bm.And(small)
		return
	}
	bm.And(idx.Lookup(v))
}

// seekRangeInto recovers the btree index's value type by asserting the typed
// btreeRanger for each supported ordered kind, runs the range seek for the
// matching bound kind, and intersects the union into bm in place. ok reports
// whether a supported (index V, bound kind) pair was found and served. The
// btree returns a fresh, frequently large union bitmap, so the combination is
// always the container-adaptive Roaring And (no clone-avoidance branch).
func seekRangeInto(bm *roaring64.Bitmap, sub index.Subscriber, lo, hi lpg.PropertyValue) (ok bool) {
	switch lo.Kind() {
	case lpg.PropString:
		if idx, isT := sub.(btreeRanger[string]); isT {
			l, _ := lo.String()
			h, _ := hi.String()
			bm.And(idx.Range(l, h))
			return true
		}
	case lpg.PropInt64:
		if idx, isT := sub.(btreeRanger[int64]); isT {
			l, _ := lo.Int64()
			h, _ := hi.Int64()
			bm.And(idx.Range(l, h))
			return true
		}
	case lpg.PropFloat64:
		if idx, isT := sub.(btreeRanger[float64]); isT {
			l, _ := lo.Float64()
			h, _ := hi.Float64()
			bm.And(idx.Range(l, h))
			return true
		}
	}
	return false
}
