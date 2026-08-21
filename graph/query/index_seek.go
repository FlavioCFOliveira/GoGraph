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
// (hashLookuper / btreeRanger below) — never by re-boxing through a generic
// any-typed read on the Manager. Which assertions the set contains is decided
// by the PREDICATE's semantics, not by which value types the indexes can
// encode: equality asserts one per supported kind, while a range asserts one
// per bound FAMILY (see [btreeRanger]).
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
//
// # Exact seek vs superset seek (task #2600)
//
// A seek may REPLACE the per-node comparison only when the index it read is an
// exact mirror of the predicate over its (label, property) pair. That holds for
// an equality against a hash index and for a STRING range against a
// string-keyed btree. It does NOT hold for a NUMERIC range.
//
// openCypher orders INTEGER and FLOAT in one numeric order — it is the sole
// off-diagonal entry of the comparability matrix in the normative CIP
// "Comparability and Orderability" (cip/1.adopted/CIP2016-06-14-*), and the TCK
// pins it in expressions/comparison/Comparison2.feature ("Comparing across
// types yields null, except numbers") — so a numeric range must consider both
// kinds at once. The only index shape carrying both is the engine's
// float64-keyed numeric companion (cypher/index_binding.go
// projectNumericPropValue), whose keys widen int64 to float64 and therefore
// round above 2^53. Its Range is consequently a SUPERSET of the answer, never
// the answer itself.
//
// So seekRangeInto reports BOTH whether it served the predicate and whether its
// result was exact, and seekIndexablePreds marks the predicate served only when
// it was exact. For an inexact seek [valueInRange] stays in place over the
// narrowed working set as the exact residual filter — the same
// superset-plus-residual shape the Cypher engine's planner already uses for the
// numeric companion.
//
// Before #2600 there was no residual filter and the two arms disagreed: with
// Float64Value bounds over an int64-valued property the seek returned the
// numeric matches (agreeing with the Cypher engine and with an independent
// model) while valueInRange required v, lo and hi to share a kind and returned
// nothing.

import (
	"cmp"
	"math"

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
//
// All four instantiations are reachable, but only through a HAND-BUILT bound
// index: a Cypher CREATE INDEX always builds a hash.Index[string], so the
// int64 / float64 / bool arms are never taken for an engine-created index. They
// are exercised through hash.NewBound in TestSeek_EqualityMatchesScan_AllKinds
// (index_seek_test.go). They stay sound because equality — unlike the range
// comparison below — is NOT unified across kinds: [equalValue] requires the
// stored value and the expected value to share a kind, so an index keyed by one
// kind is an exact mirror of the equality it serves.
type hashLookuper[V comparable] interface {
	Cardinality(value V) uint64
	LookupAppend(value V, dst []uint64) []uint64
	Lookup(value V) *roaring64.Bitmap
}

// btreeRanger is the typed range-read interface of btree.Index[V]. Exactly two
// instantiations are asserted, and which two is decided by the predicate's
// semantics rather than by which value types the btree happens to encode:
//
//   - btreeRanger[string] serves a STRING range EXACTLY. A bound string btree
//     holds precisely the string-valued nodes of its (label, property) pair,
//     which is precisely the set a string-bounded [WithRange] can match, so the
//     per-node comparison is skipped for it.
//   - btreeRanger[float64] serves a NUMERIC range as a SUPERSET. INTEGER and
//     FLOAT share one numeric order, so a numeric range must match both kinds,
//     and a float64-keyed index is the only shape that can carry both. Its keys
//     round above 2^53, so [valueInRange] always runs over its output as the
//     exact residual filter.
//
// A btreeRanger[int64] arm existed until #2600 and was REMOVED rather than
// retained: once the range comparison unified INTEGER and FLOAT, an int64-keyed
// index became a SUBSET of the answer — it cannot hold the float-valued nodes a
// numeric range must also match — and a subset cannot be repaired by a residual
// filter. An int64-keyed btree is therefore no longer consulted at all and the
// predicate scans, which is correct, if slower.
//
// # Coverage contract for the numeric arm
//
// A bound float64-keyed btree covering (label, property) must key EVERY numeric
// value of that property — integers widened to float64 as well as floats — so
// that its Range is a superset of the numeric answer. The engine's numeric
// companion satisfies this by construction (cypher/index_binding.go
// projectNumericPropValue indexes both PropInt64 and PropFloat64). A hand-built
// float64-keyed bound index that projected only PropFloat64 would be a subset
// and would make the seek under-return; that is the one contract this file
// requires of an index beyond the (label, property) coverage [indexCovers]
// checks.
type btreeRanger[V comparable] interface {
	Range(lo, hi V) *roaring64.Bitmap
}

// withRange matches nodes whose given property lies in the inclusive interval
// [lo, hi] under openCypher comparability. It is the range counterpart to
// withProperty: a covering btree index narrows the working set with a single
// Range seek, which for a string range IS the answer and for a numeric range is
// a superset the per-node comparison below then filters exactly.
type withRange[N comparable, W any] struct {
	lo, hi lpg.PropertyValue
	key    string
}

// Match implements the per-node comparison for a range predicate: it keeps a
// node when its property value satisfies BOTH bound tests of [WithRange] —
// v >= lo and v <= hi — each evaluated independently under openCypher's
// comparability rules, so lo and hi need not share a kind. INTEGER and FLOAT
// compare in one numeric order, exactly (an int64 is never widened to float64);
// every other cross-kind pair is not comparable and its bound test is false.
//
// This is the scan path. A covering btree index narrows the working set ahead
// of it in filterByPreds, but only a STRING range skips it: a numeric range is
// served from a lossy float64-keyed index, so this comparison remains as the
// exact residual filter over the seek's output (#2600).
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

// WithRange returns a [Predicate] selecting nodes whose named property lies in
// the inclusive interval [lo, hi].
//
// # Comparison semantics
//
// The two bound tests — v >= lo and v <= hi — are INDEPENDENT, exactly as
// openCypher's comparability rules make them, so lo and hi need not share a
// kind. A bound test holds only when the two values are comparable AND the
// relation is true:
//
//   - STRING against STRING: byte-wise order.
//   - INTEGER against INTEGER, FLOAT against FLOAT, and INTEGER against FLOAT:
//     one numeric order, compared EXACTLY. An int64 is never widened to
//     float64, so 4611686018427387905 and 4611686018427387900 stay distinct
//     even though both round to the same float64 (2^62).
//   - Every other pair — a number against a string, or either side a stored
//     [lpg.PropBool], [lpg.PropBytes], [lpg.PropTime] or [lpg.PropList] — is
//     not comparable, and its bound test is false. (A Cypher temporal value is
//     delivered to the property layer as a TAGGED PropString, not as a
//     PropTime, so it is ordered as a string; that is unchanged by #2600.)
//
// A NaN operand makes every comparison FALSE (never null), which is the
// IEEE-754 outcome openCypher adopts: a NaN property value never matches, and a
// NaN bound matches nothing.
//
// # Index acceleration
//
// When the owning graph carries a btree index covering the predicate's
// (label, property) pair — and the same [Pattern.Vertex] call also constrains
// that label — the engine narrows the working set from the index with one
// O(log n + k) seek before any property is read. What happens next depends on
// whether that seek is exact:
//
//   - A STRING range is served EXACTLY, so the per-node comparison is skipped
//     entirely and no property is read.
//   - A NUMERIC range is served from a float64-keyed index whose keys round
//     above 2^53, so the seek is only a SUPERSET: the exact comparison above
//     still runs, but over what the seek left rather than over the whole
//     working set.
//
// Either way the answer is identical to the answer the same query returns with
// no index present.
func WithRange[N comparable, W any](key string, lo, hi lpg.PropertyValue) Predicate[N, W] {
	return withRange[N, W]{key: key, lo: lo, hi: hi}
}

// valueInRange reports whether v satisfies both bound tests of [WithRange]:
// v >= lo AND v <= hi, each under openCypher comparability and each independent
// of the other, so lo and hi need not share a kind.
//
// It is the predicate's DEFINITION, not a fallback. For a numeric range the
// index seek is only a superset of the answer (see [btreeRanger]), so
// filterByPreds runs this over the seek's output as the exact residual filter.
func valueInRange(v, lo, hi lpg.PropertyValue) bool {
	if c, ok := compareValues(v, lo); !ok || c < 0 {
		return false
	}
	c, ok := compareValues(v, hi)
	return ok && c <= 0
}

// compareValues orders two [lpg.PropertyValue]s under openCypher's
// comparability rules, returning -1, 0 or +1 for a < b, a == b and a > b.
//
// ok is false when the pair is NOT ordered, and the caller must then treat
// every relational test over it as false. Two situations produce ok=false, and
// collapsing them is deliberate because [WithRange] cannot distinguish their
// outcomes:
//
//   - A cross-type pair other than INTEGER/FLOAT. openCypher makes comparing
//     across types null, and a null bound test fails the filter exactly as a
//     false one does.
//   - A NaN operand. Every comparison involving NaN is FALSE, so reporting the
//     pair as unordered yields the same false for >=, <= and ==.
//
// It is NOT an equality or an equivalence test and must not be reused as one:
// all three unify INTEGER and FLOAT but they differ at NaN, where equivalence
// holds (a value set folds NaN onto itself — cypher/exec/constraints.go
// floatCanonicalKey) and comparability does not.
func compareValues(a, b lpg.PropertyValue) (int, bool) {
	switch a.Kind() {
	case lpg.PropString:
		if b.Kind() != lpg.PropString {
			return 0, false
		}
		x, _ := a.String()
		y, _ := b.String()
		return cmp.Compare(x, y), true
	case lpg.PropInt64:
		x, _ := a.Int64()
		switch b.Kind() {
		case lpg.PropInt64:
			y, _ := b.Int64()
			return cmp.Compare(x, y), true
		case lpg.PropFloat64:
			y, _ := b.Float64()
			return cmpInt64Float64(x, y)
		}
		return 0, false
	case lpg.PropFloat64:
		x, _ := a.Float64()
		switch b.Kind() {
		case lpg.PropInt64:
			y, _ := b.Int64()
			c, ok := cmpInt64Float64(y, x)
			return -c, ok
		case lpg.PropFloat64:
			y, _ := b.Float64()
			// Raw IEEE-754 comparison, deliberately: it is what makes every
			// relation against NaN FALSE rather than giving NaN a position in the
			// order. cmp.Compare cannot be used here — it treats NaN as less than
			// every non-NaN and equal to itself, which is a sortable total order
			// and neither the comparability rule nor openCypher's ORDER BY rule
			// (which places NaN after every number — cypher/expr/value.go
			// cmpFloat64).
			switch {
			case x < y:
				return -1, true
			case x > y:
				return 1, true
			case x == y:
				return 0, true
			}
			return 0, false // an operand is NaN
		}
		return 0, false
	}
	// PropBool, PropTime, PropBytes and PropList are not ordered scalars under
	// openCypher comparability, so no bound test over them can hold.
	return 0, false
}

// float64TwoTo63 is 2^63 as a float64, which is exactly representable. Every
// int64 is strictly below it and at or above its negation, so comparing a
// float64 against it is what makes the float64 -> int64 conversion in
// [cmpInt64Float64] well defined.
const float64TwoTo63 = 9223372036854775808.0

// cmpInt64Float64 compares an int64 against a float64 EXACTLY — as
// unlimited-precision numbers — returning -1, 0 or +1 for i < f, i == f and
// i > f. ok is false when f is NaN, which is not a position in the order.
//
// It never widens i to float64. float64(i) rounds for |i| > 2^53, so a widening
// comparison would fold 4611686018427387905 and 4611686018427387900 onto the
// same value (both round to 2^62) and report them equal — which the openCypher
// TCK explicitly forbids (expressions/comparison/Comparison1.feature, the
// large-integer inequality scenarios). The precedent for refusing a lossy
// widening in this repository is the int64 round-trip guard in
// cypher/exec/constraints.go floatCanonicalKey.
func cmpInt64Float64(i int64, f float64) (int, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	// A float outside int64's range settles the comparison on its own; the two
	// guards also cover +Inf and -Inf, and are what bound the conversion below.
	if f >= float64TwoTo63 {
		return -1, true
	}
	if f < -float64TwoTo63 {
		return 1, true
	}
	// f is finite and within int64 range, so its integral part converts exactly.
	t := math.Trunc(f)
	if c := cmp.Compare(i, int64(t)); c != 0 {
		return c, true
	}
	// Equal integral parts: the fractional part decides. Trunc rounds towards
	// zero, so a positive f has f > t and a negative f has f < t.
	switch {
	case f > t:
		return -1, true
	case f < t:
		return 1, true
	}
	return 0, true
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

// trySeekRange attempts to narrow bm from a covering btree index for a range
// predicate.
//
// narrowed reports whether an index was consulted and intersected into bm; when
// it is false bm is untouched. exact reports whether the intersected set is the
// ANSWER, so the caller may skip the per-node comparison, or merely a SUPERSET
// of it, so the caller must keep [valueInRange] as a residual filter. exact is
// meaningful only when narrowed is true.
//
// There is no bail-out on pred.lo.Kind() != pred.hi.Kind(). The two bound tests
// are independent under openCypher comparability, so a range with an INTEGER
// lower bound and a FLOAT upper bound is a well-formed numeric range; refusing
// it made the seek arm consistently wrong instead of merely divergent (#2600).
func (p *Pattern[N, W]) trySeekRange(
	bm *roaring64.Bitmap, pred withRange[N, W], labels []string,
) (narrowed, exact bool) {
	mgr := p.engine.g.IndexManager()
	if mgr == nil || len(labels) == 0 {
		return false, false
	}
	for _, name := range mgr.ListIndexes() {
		sub, err := mgr.GetIndex(name)
		if err != nil || sub.Kind() != "btree" {
			continue
		}
		if !indexCovers(sub, labels, pred.key) {
			continue
		}
		if served, isExact := seekRangeInto(bm, sub, pred.lo, pred.hi); served {
			return true, isExact
		}
	}
	return false, false
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
// [btreeRanger] for the FAMILY the bounds belong to, runs the range seek, and
// intersects its result into bm in place. The btree returns a fresh, frequently
// large union bitmap, so the combination is always the container-adaptive
// Roaring And (no clone-avoidance branch).
//
// narrowed reports whether a supported (index V, bound family) pair was found
// and served; exact reports whether the intersected set is the answer or a
// superset of it:
//
//   - STRING bounds against a string-keyed index: EXACT.
//   - NUMERIC bounds (INTEGER, FLOAT, or one of each) against the float64-keyed
//     index: a SUPERSET. The index widens int64 keys to float64, and
//     [numericSeekBounds] widens the bounds OUTWARDS so no true match can fall
//     outside the interval seeked.
//   - Bounds from two different families — a string against a number — can
//     never both hold, so no index is consulted and the per-node comparison
//     rejects every candidate.
func seekRangeInto(
	bm *roaring64.Bitmap, sub index.Subscriber, lo, hi lpg.PropertyValue,
) (narrowed, exact bool) {
	switch {
	case lo.Kind() == lpg.PropString && hi.Kind() == lpg.PropString:
		idx, isT := sub.(btreeRanger[string])
		if !isT {
			return false, false
		}
		l, _ := lo.String()
		h, _ := hi.String()
		bm.And(idx.Range(l, h))
		return true, true
	case isNumericKind(lo.Kind()) && isNumericKind(hi.Kind()):
		idx, isT := sub.(btreeRanger[float64])
		if !isT {
			return false, false
		}
		l, h, satisfiable := numericSeekBounds(lo, hi)
		if !satisfiable {
			// A NaN bound: no value can satisfy the predicate, so the answer is
			// empty and there is nothing to seek. exact stays false so the numeric
			// arm keeps exactly one contract — always residual-filtered — and the
			// residual then runs over an empty working set at no cost.
			bm.Clear()
			return true, false
		}
		bm.And(idx.Range(l, h))
		return true, false
	}
	return false, false
}

// isNumericKind reports whether k is one of the two kinds openCypher places in
// a single numeric order.
func isNumericKind(k lpg.PropertyKind) bool {
	return k == lpg.PropInt64 || k == lpg.PropFloat64
}

// numericSeekBounds converts a numeric bound pair into the float64 interval to
// seek in the float64-keyed companion index. satisfiable is false when either
// bound is NaN, because then no value can satisfy the predicate at all.
//
// # Why the interval is a superset
//
// The one invariant the superset property rests on is
//
//	lof <= lo   and   hi <= hif   (compared EXACTLY, never by widening)
//
// which [numericSeekBound] establishes rather than assumes. Given it, for any
// candidate property value v that satisfies the predicate — an int64 or a
// float64 with lo <= v <= hi — the companion key float64(v) lies inside
// [lof, hif]:
//
//   - float64(v) is one of the two float64 values bracketing v (it equals v
//     when v is a float64). Let D be the largest float64 <= v; then float64(v)
//     is D or the next float64 above it, so float64(v) >= D.
//   - lof is a float64 and lof <= lo <= v, so lof <= D <= float64(v).
//   - Symmetrically, with U the smallest float64 >= v, float64(v) <= U, and hif
//     is a float64 with hif >= hi >= v, so float64(v) <= U <= hif.
//
// The argument deliberately assumes only that an int64 -> float64 conversion
// lands on one of the two bracketing float64 values, and NOT that it rounds to
// nearest: the Go specification leaves the result implementation-dependent when
// the destination type cannot represent the value exactly.
func numericSeekBounds(lo, hi lpg.PropertyValue) (lof, hif float64, satisfiable bool) {
	lof, ok := numericSeekBound(lo, true)
	if !ok {
		return 0, 0, false
	}
	hif, ok = numericSeekBound(hi, false)
	if !ok {
		return 0, 0, false
	}
	return lof, hif, true
}

// numericSeekBound converts one numeric bound to a float64 on the correct side
// of it: lower=true returns a float64 <= b, lower=false a float64 >= b. ok is
// false for a NaN bound.
//
// A float64 bound is already on the right side of itself. An int64 bound may not
// be representable, so the conversion is CHECKED with the exact comparator and
// stepped one ULP outwards when it landed on the wrong side. One step always
// suffices: the conversion lands on one of the two float64 values bracketing i,
// so if it overshot, the next float64 in the other direction is the bracketing
// one on the correct side.
//
// The step is not merely defensive. At i = math.MaxInt64 the conversion yields
// 2^63, which is strictly GREATER than i, so without the step a lower bound of
// MaxInt64 would violate the lof <= lo invariant [numericSeekBounds] relies on.
// TestNumericSeekBound_StaysOnTheCorrectSideOfTheBound asserts the invariant
// directly for exactly that reason.
func numericSeekBound(b lpg.PropertyValue, lower bool) (float64, bool) {
	switch b.Kind() {
	case lpg.PropFloat64:
		f, _ := b.Float64()
		if math.IsNaN(f) {
			return 0, false
		}
		return f, true
	case lpg.PropInt64:
		i, _ := b.Int64()
		f := float64(i)
		// float64(i) is never NaN, so the comparator's ok result is always true.
		c, _ := cmpInt64Float64(i, f)
		switch {
		case lower && c < 0:
			// i < float64(i): the conversion overshot ABOVE a lower bound.
			return math.Nextafter(f, math.Inf(-1)), true
		case !lower && c > 0:
			// i > float64(i): the conversion undershot BELOW an upper bound.
			return math.Nextafter(f, math.Inf(1)), true
		}
		return f, true
	}
	return 0, false
}
