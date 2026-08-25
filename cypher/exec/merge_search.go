package exec

// merge_search.go — real implementation of [MergeSearchFn] (T930).
//
// MERGE semantics (openCypher §11.4): given a pattern such as
// `(n:Label {key: value, ...})`, MERGE must first locate any existing node
// that matches the entire pattern (all labels AND all properties), and only
// when no such node exists may it fire the ON CREATE path. The previous
// implementation (api.go) returned an always-empty match set, which caused
// every MERGE call to fire ON CREATE and produced duplicate nodes on repeat
// invocations.
//
// [NewMergeSearchFnFromPattern] returns a [MergeSearchFn] that scans the
// supplied [GraphMutator] for every node whose labels are a superset of the
// pattern labels and whose properties equal every (key, value) parsed from
// the pattern's property map. Matches are returned as single-column rows
// carrying the matched node's [graph.NodeID] as an [expr.IntegerValue], the
// same shape produced by the ON CREATE branch — so [Merge.applyActions] can
// resolve the bound node via either the schema lookup or the row[0]
// fallback.
//
// # Concurrency
//
// The closure is read-only against the mutator and re-entrant, and is driven by
// the one goroutine that owns the operator tree.
//
// It does NOT exclude another writer. Two concurrent MERGE callers CAN both
// observe a zero-match result and both fire ON CREATE: the engine's writer mutex
// and the store's capacity-one semaphore, which used to prevent that, were
// retired by rmp #2306. See [Merge] for the measured behaviour and the
// uniqueness-constraint remedy.

import (
	"bytes"
	"context"
	"fmt"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// NewMergeSearchFnFromPattern returns a [MergeSearchFn] that finds every
// node in mutator whose label set contains every label in labels and whose
// property bag is equal to every (key, value) parsed from propertiesRaw.
//
// labels is the slice of pattern labels (may be empty when the pattern is
// e.g. `(n {key: v})`). propertiesRaw is the opaque literal-map string
// surfaced by the IR (e.g. `{name: "Alice", age: 30}`); it may be empty.
// params binds `$name` references in propertiesRaw to query parameters; when
// empty the parser ignores parameter substitution.
//
// The function returned by [NewMergeSearchFnFromPattern] enumerates candidate
// nodes, resolves the label and property bag, and admits the node iff every
// label and every property matches. When the pattern carries at least one label
// and labelSrc is non-nil the candidates come from the smallest matching
// label's posting list, so cost tracks that label's population rather than the
// whole graph; otherwise every interned node is examined. See
// [walkMergeCandidates].
//
// labelSrc may be nil, in which case the search falls back to the full walk.
func NewMergeSearchFnFromPattern(
	labels []string,
	propertiesRaw string,
	params map[string]expr.Value,
	mutator GraphMutator,
	labelSrc MergeLabelSource,
) (MergeSearchFn, error) {
	var props []propLiteral
	var err error
	if len(params) == 0 {
		props, err = parsePropLiteral(propertiesRaw)
	} else {
		props, err = parsePropLiteralWithParamsMerge(propertiesRaw, params)
	}
	if err != nil {
		return nil, fmt.Errorf("exec: NewMergeSearchFnFromPattern: parse properties %q: %w", propertiesRaw, err)
	}

	wantLabels := make([]string, len(labels))
	copy(wantLabels, labels)

	return func(ctx context.Context) ([]Row, error) {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		var matches []Row
		var walkErr error
		walkMergeCandidates(mutator, labelSrc, wantLabels, func(id graph.NodeID) bool {
			if cerr := ctx.Err(); cerr != nil {
				walkErr = cerr
				return false
			}
			nodeKey, ok := mutator.ResolveNodeLabel(id)
			if !ok {
				return true
			}
			if !nodeMatchesAllLabels(wantLabels, labelsInTx(mutator, nodeKey)) {
				return true
			}
			if !nodeMatchesAllPropertiesInTx(mutator, nodeKey, props) {
				return true
			}
			matches = append(matches, Row{expr.IntegerValue(int64(id))})
			return true
		})
		if walkErr != nil {
			return nil, walkErr
		}
		return matches, nil
	}, nil
}

// MergeLabelSource narrows the MERGE match phase to the nodes that carry a
// pattern label. lpg's label index satisfies it, and its bitmap reflects
// uncommitted writes made by the enclosing transaction — verified for a
// same-transaction CREATE and for a label added by SET — so driving from it
// cannot miss a node the caller has just written and thereby create a duplicate.
type MergeLabelSource interface {
	// ResolveLabelBitmap returns the NodeIDs carrying name, or an empty bitmap
	// when the label is unknown. An empty bitmap is authoritative: no node can
	// carry a label that was never interned, so the MERGE correctly finds no
	// match and fires ON CREATE.
	ResolveLabelBitmap(name string) *roaring64.Bitmap
}

// mergeLabelCounter is an optional refinement of [MergeLabelSource]: a source
// that can report a label's cardinality without materialising its bitmap lets
// the search pick the smallest label to drive from, which is the same
// min-cardinality choice the label-scan planner makes.
type mergeLabelCounter interface {
	ResolveLabelCount(name string) (int64, bool)
}

// walkMergeCandidates calls fn for every node that could match a MERGE pattern
// carrying labels.
//
// With at least one label and a non-nil src it drives from a label posting
// list; otherwise it falls back to every interned node. The candidate set is a
// SUPERSET of the matches — it constrains only WHICH nodes are examined, never
// how many are admitted — so fn must still verify every label and every
// property. That is the same discipline the range-seek and expand-into work
// use, and it is what preserves MERGE's requirement to bind EVERY match: no
// candidate is skipped and no enumeration is cut short.
//
// fn returns false to stop early (used for context cancellation only).
func walkMergeCandidates(mutator GraphMutator, src MergeLabelSource, labels []string, fn func(graph.NodeID) bool) {
	if src == nil || len(labels) == 0 {
		mutator.WalkNodeIDs(fn)
		return
	}
	bm := src.ResolveLabelBitmap(mergeDrivingLabel(src, labels))
	if bm == nil {
		// A source that cannot answer is treated as absent rather than as an
		// empty result, so a missing capability can never turn a match into a
		// spurious create.
		mutator.WalkNodeIDs(fn)
		return
	}
	it := bm.Iterator()
	for it.HasNext() {
		if !fn(graph.NodeID(it.Next())) {
			return
		}
	}
}

// mergeDrivingLabel picks the pattern label whose posting list should drive the
// candidate enumeration: the one with the fewest nodes when the source can
// report cardinality cheaply, else the first label. Every label is re-verified
// per candidate regardless, so this choice is a cost decision only.
func mergeDrivingLabel(src MergeLabelSource, labels []string) string {
	counter, ok := src.(mergeLabelCounter)
	if !ok || len(labels) == 1 {
		return labels[0]
	}
	best, bestCount := labels[0], int64(-1)
	for _, l := range labels {
		n, known := counter.ResolveLabelCount(l)
		if !known {
			continue
		}
		if bestCount < 0 || n < bestCount {
			best, bestCount = l, n
		}
	}
	return best
}

// searchMergeNodes runs the same scan as the closure returned by
// [NewMergeSearchFnFromPattern] but with explicit (labels, props) inputs.
// Used by row-aware MERGE: the property map's expressions are evaluated
// against the driving child row and the resulting propLiterals drive the
// search predicate. Returns one Row per matching node carrying the node id
// as a single IntegerValue, identical in shape to the closure-returned rows.
//
// labelSrc narrows the candidate enumeration exactly as in the closure and may
// be nil. This is the path the UNWIND-MERGE bulk-ingest idiom drives, so it is
// the one where the whole-graph walk cost B×N.
func searchMergeNodes(ctx context.Context, mutator GraphMutator, labelSrc MergeLabelSource, labels []string, props []propLiteral) ([]Row, error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	var matches []Row
	var walkErr error
	walkMergeCandidates(mutator, labelSrc, labels, func(id graph.NodeID) bool {
		if cerr := ctx.Err(); cerr != nil {
			walkErr = cerr
			return false
		}
		nodeKey, ok := mutator.ResolveNodeLabel(id)
		if !ok {
			return true
		}
		if !nodeMatchesAllLabels(labels, labelsInTx(mutator, nodeKey)) {
			return true
		}
		if !nodeMatchesAllPropertiesInTx(mutator, nodeKey, props) {
			return true
		}
		matches = append(matches, Row{expr.IntegerValue(int64(id))})
		return true
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return matches, nil
}

// nodeMatchesAllLabels reports whether every label in want is also present
// in got. An empty want list always matches. Comparison is exact, case-
// sensitive, and order-independent.
func nodeMatchesAllLabels(want, got []string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// nodeMatchesAllProperties reports whether every (key, value) entry in want
// is present in got with a kind-and-value-equal [lpg.PropertyValue]. An
// empty want list always matches. A partial match — some properties of the
// pattern present, others absent — does NOT match: every property in want
// must be present.
// nodeMatchesAllPropertiesInTx is [nodeMatchesAllProperties] resolved through the
// mutator's own transaction view.
//
// It is the property half of rmp #2365, and it has the identical exposure the
// label half had: GraphMutator.NodeProperties is a bare shard read returning the
// NEWEST stored value, another transaction's eager uncommitted write included.
// MEASURED on 2026-08-25, with a peer holding an uncommitted `SET n.k = 'new'`
// on the only :Target node:
//
//	T2: MATCH (n:Target {k:'old'}) RETURN count(n)  => 1   (correctly visible)
//	T2: MERGE (n:Target {k:'old'}) …                => DUPLICATE created
//
// MERGE contradicting MATCH inside ONE transaction is the sharpest statement of
// the defect, and it is why the fix belongs at the decision site rather than in
// the enumeration: the candidate was enumerated correctly and then dropped by
// this comparison.
//
// It reads per KEY rather than fetching the whole bag, because that is what the
// transaction view offers ([txVisibleNodeReader.NodePropertyInTx]) and because
// MERGE only ever asks about the keys its pattern names.
func nodeMatchesAllPropertiesInTx(mut GraphMutator, n string, want []propLiteral) bool {
	if len(want) == 0 {
		return true
	}
	r := nodeStateReaderFor(mut)
	for _, w := range want {
		gv, ok := r.property(n, w.key)
		if !ok {
			return false
		}
		if !mergePropValueEquals(w.value, gv) {
			return false
		}
	}
	return true
}

func nodeMatchesAllProperties(want []propLiteral, got map[string]lpg.PropertyValue) bool {
	for _, w := range want {
		gv, ok := got[w.key]
		if !ok {
			return false
		}
		if !mergePropValueEquals(w.value, gv) {
			return false
		}
	}
	return true
}

// mergePropValueEquals reports whether two [lpg.PropertyValue]s are equal for
// the purposes of MERGE's match phase. The comparison mirrors the openCypher
// `=` operator (and therefore MATCH and WHERE): two values match when they are
// equal under that operator, including cross-type numeric equality such as
// `1 = 1.0`. Without this, a node stored as integer `(:N {x:1})` would fail to
// match `MERGE (n:N {x:1.0})` and MERGE would wrongly create a duplicate
// (openCypher conformance bug, rmp #1240).
//
// The helper is shared by both node MERGE ([nodeMatchesAllProperties]) and
// relationship MERGE ([MergeRelationship.matchesRelProps]). It is symmetric in
// its arguments, so call-site ordering of want/got is irrelevant.
//
// Same-kind comparisons keep the historical strict semantics verbatim:
// PropString/PropInt64/PropFloat64/PropBool use the language's == operator;
// PropTime uses [time.Time.Equal] (which normalises monotonic clock readings
// and timezone offsets); PropBytes uses [bytes.Equal]; PropList compares
// element-wise, recursing through this helper so cross-type list elements such
// as `[1]` versus `[1.0]` match while temporal/byte elements stay on the
// strict same-kind path.
//
// Cross-kind comparisons are the only new behaviour: both operands are
// converted via [lpgPropToExprBinding] and compared with the canonical
// [expr.Value.Equal]. A match requires both conversions to succeed AND the
// result to be exactly true. PropTime and PropBytes do not convert, so a
// cross-kind comparison involving them yields false rather than a spurious
// match.
func mergePropValueEquals(a, b lpg.PropertyValue) bool {
	if a.Kind() == b.Kind() {
		switch a.Kind() {
		case lpg.PropString:
			av, _ := a.String()
			bv, _ := b.String()
			return av == bv
		case lpg.PropInt64:
			av, _ := a.Int64()
			bv, _ := b.Int64()
			return av == bv
		case lpg.PropFloat64:
			av, _ := a.Float64()
			bv, _ := b.Float64()
			return av == bv
		case lpg.PropBool:
			av, _ := a.Bool()
			bv, _ := b.Bool()
			return av == bv
		case lpg.PropTime:
			av, _ := a.Time()
			bv, _ := b.Time()
			return av.Equal(bv)
		case lpg.PropBytes:
			av, _ := a.Bytes()
			bv, _ := b.Bytes()
			return bytes.Equal(av, bv)
		case lpg.PropList:
			ae, _ := a.List()
			be, _ := b.List()
			if len(ae) != len(be) {
				return false
			}
			for i := range ae {
				if !mergePropValueEquals(ae[i], be[i]) {
					return false
				}
			}
			return true
		}
		return false
	}
	// Kinds differ: fall back to the canonical `=` operator semantics so that
	// cross-type numeric equality (1 == 1.0) matches exactly as MATCH/WHERE do.
	av, aok := lpgPropToExprBinding(a)
	bv, bok := lpgPropToExprBinding(b)
	if !aok || !bok {
		return false
	}
	return av.Equal(bv) == expr.BoolValue(true)
}
