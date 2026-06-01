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
// The closure is invoked inside the writer transaction that holds the
// engine's single-writer lock, so concurrent MERGE callers cannot both
// observe a zero-match result and both fire ON CREATE. The closure itself
// is read-only against the mutator and re-entrant.

import (
	"bytes"
	"context"
	"fmt"

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
// The function returned by [NewMergeSearchFnFromPattern] walks every
// interned node id, resolves the label and property bag, and admits the
// node iff every label and every property matches. Match scaling is O(N)
// where N is the number of interned nodes; the cost is acceptable for the
// typical MERGE workload (small N or label-restricted pattern). A future
// revision may use [labelResolver]'s bitmap intersection to short-circuit
// label scans.
func NewMergeSearchFnFromPattern(
	labels []string,
	propertiesRaw string,
	params map[string]expr.Value,
	mutator GraphMutator,
) (MergeSearchFn, error) {
	var props []propLiteral
	var err error
	if len(params) == 0 {
		props, err = parsePropLiteral(propertiesRaw)
	} else {
		props, err = parsePropLiteralWithParams(propertiesRaw, params)
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
		mutator.WalkNodeIDs(func(id graph.NodeID) bool {
			if cerr := ctx.Err(); cerr != nil {
				walkErr = cerr
				return false
			}
			nodeKey, ok := mutator.ResolveNodeLabel(id)
			if !ok {
				return true
			}
			if !nodeMatchesAllLabels(wantLabels, mutator.NodeLabels(nodeKey)) {
				return true
			}
			if !nodeMatchesAllProperties(props, mutator.NodeProperties(nodeKey)) {
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

// searchMergeNodes runs the same scan as the closure returned by
// [NewMergeSearchFnFromPattern] but with explicit (labels, props) inputs.
// Used by row-aware MERGE: the property map's expressions are evaluated
// against the driving child row and the resulting propLiterals drive the
// search predicate. Returns one Row per matching node carrying the node id
// as a single IntegerValue, identical in shape to the closure-returned rows.
func searchMergeNodes(ctx context.Context, mutator GraphMutator, labels []string, props []propLiteral) ([]Row, error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	var matches []Row
	var walkErr error
	mutator.WalkNodeIDs(func(id graph.NodeID) bool {
		if cerr := ctx.Err(); cerr != nil {
			walkErr = cerr
			return false
		}
		nodeKey, ok := mutator.ResolveNodeLabel(id)
		if !ok {
			return true
		}
		if !nodeMatchesAllLabels(labels, mutator.NodeLabels(nodeKey)) {
			return true
		}
		if !nodeMatchesAllProperties(props, mutator.NodeProperties(nodeKey)) {
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
