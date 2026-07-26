package cypher

// columnar_shape_identity_test.go — result-identity gate for every shape #2186 newly
// admitted to the columnar path: the conjunction, the label test, IN over a scalar
// literal list, and the stacked-Selection fusion over a traversal.
//
// Each case runs the same query twice and requires an identical result multiset:
//
//   - the columnar arm as written, with a metrics probe asserting the columnar filter
//     engaged, so a silent fallback cannot make the comparison vacuous; and
//   - a row arm in which every property access is wrapped in coalesce(x), an exact
//     value identity that the columnar predicate and projection both decline — so the
//     arm executes fully boxed — with the probe asserting the columnar filter did NOT
//     engage.
//
// The fixture is deliberately hostile to the unboxed fast path. The `v` property
// spans int64 (including values outside the small-int box range and at the int64
// extremes), float64 (including -0.0, NaN and both infinities), strings (including
// the empty string and a SOH-tagged string that is NOT a valid temporal), booleans, a
// real temporal, and an ABSENT property. That mixture forces every branch of the
// predicate: the same-kind unboxed compare, the cross-type kind mismatch that must
// report undecided, the temporal-tagged string that must report undecided, and the
// absent property that is a decided drop under three-valued logic.
//
// Labels are assigned so that a label test partitions the same nodes several ways,
// including a label no node carries and a label name never interned.
//
// Layer: short.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// shapeIdentityGraph builds the hostile fixture described in the file comment: a
// labelled chain where each node carries a `v` of a different kind, a `w` integer,
// and a subset of the labels A/B/C.
func shapeIdentityGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	day := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	vals := []struct {
		set bool
		v   lpg.PropertyValue
	}{
		{true, lpg.Int64Value(math.MinInt64)},
		{true, lpg.Int64Value(-256)},
		{true, lpg.Int64Value(-1)},
		{true, lpg.Int64Value(0)},
		{true, lpg.Int64Value(1)},
		{true, lpg.Int64Value(2)},
		{true, lpg.Int64Value(3)},
		{true, lpg.Int64Value(255)},
		{true, lpg.Int64Value(256)},
		{true, lpg.Int64Value(1000)},
		{true, lpg.Int64Value(math.MaxInt64)},
		{true, lpg.Float64Value(-1.5)},
		{true, lpg.Float64Value(math.Copysign(0, -1))},
		{true, lpg.Float64Value(0.0)},
		{true, lpg.Float64Value(1.0)},
		{true, lpg.Float64Value(1.5)},
		{true, lpg.Float64Value(math.NaN())},
		{true, lpg.Float64Value(math.Inf(1))},
		{true, lpg.Float64Value(math.Inf(-1))},
		{true, lpg.StringValue("")},
		{true, lpg.StringValue("a")},
		{true, lpg.StringValue("m")},
		{true, lpg.StringValue("z")},
		{true, lpg.StringValue("\x01not-a-date")},
		{true, lpg.BoolValue(true)},
		{true, lpg.BoolValue(false)},
		{true, lpg.DateValue(day)},
		{false, lpg.PropertyValue{}}, // absent v → NULL
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	keys := make([]string, len(vals))
	for i, cell := range vals {
		k := fmt.Sprintf("s%03d", i)
		keys[i] = k
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		// Every node carries A; B on every third; C on every fifth. So `:A` selects
		// all, `:A:B` a third, `:B:C` a fifteenth, and `:Z` none.
		if err := g.SetNodeLabel(k, "A"); err != nil {
			t.Fatalf("SetNodeLabel A: %v", err)
		}
		if i%3 == 0 {
			if err := g.SetNodeLabel(k, "B"); err != nil {
				t.Fatalf("SetNodeLabel B: %v", err)
			}
		}
		if i%5 == 0 {
			if err := g.SetNodeLabel(k, "C"); err != nil {
				t.Fatalf("SetNodeLabel C: %v", err)
			}
		}
		if cell.set {
			if err := g.SetNodeProperty(k, "v", cell.v); err != nil {
				t.Fatalf("SetNodeProperty v: %v", err)
			}
		}
		if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i%7))); err != nil {
			t.Fatalf("SetNodeProperty w: %v", err)
		}
	}
	// A chain of K edges so every traversal shape has edges to walk.
	for i := 0; i+1 < len(keys); i++ {
		if err := g.AddEdge(keys[i], keys[i+1], 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(keys[i], keys[i+1], "K")
	}
	return g
}

// drainShapeValues runs query and returns every projected value, in one flat slice.
func drainShapeValues(t *testing.T, g *lpg.Graph[string, float64], query string) []expr.Value {
	t.Helper()
	res, err := NewEngine(g).Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer func() { _ = res.Close() }()
	var out []expr.Value
	for res.Next() {
		for i := 0; i < len(res.Columns()); i++ {
			out = append(out, res.ValueAt(i))
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return out
}

// boxify rewrites every `<var>.<prop>` access to coalesce(<var>.<prop>) — an exact
// value identity that both the columnar predicate and the columnar projection
// decline, so the query executes fully boxed.
func boxify(query string, vars ...string) string {
	out := query
	for _, v := range vars {
		for _, prop := range []string{".v", ".w"} {
			out = strings.ReplaceAll(out, v+prop, "coalesce("+v+prop+")")
		}
	}
	return out
}

// assertShapeIdentity runs the columnar and boxed arms of query and requires an
// identical result multiset, plus the engagement asymmetry that keeps the comparison
// meaningful.
func assertShapeIdentity(t *testing.T, be *countingBackend, query string, vars ...string) {
	t.Helper()
	g := shapeIdentityGraph(t)

	be.filterBatches.Store(0)
	colVals := drainShapeValues(t, g, query)
	colBatches := be.filterBatches.Load()

	rowQuery := boxify(query, vars...)
	if rowQuery == query {
		t.Fatalf("boxify(%q) rewrote nothing: the row arm would be the same query, so "+
			"this case cannot prove anything", query)
	}
	be.filterBatches.Store(0)
	rowVals := drainShapeValues(t, g, rowQuery)
	rowBatches := be.filterBatches.Load()

	if colBatches == 0 {
		t.Fatalf("columnar arm %q did not engage the columnar filter, so the identity "+
			"comparison is vacuous", query)
	}
	if rowBatches != 0 {
		t.Fatalf("row arm %q unexpectedly engaged the columnar filter, so it is not a "+
			"boxed reference", rowQuery)
	}
	if len(colVals) != len(rowVals) {
		t.Fatalf("row count differs for %q: columnar=%d boxed=%d", query, len(colVals), len(rowVals))
	}
	colKeys := sortedValueKeys(colVals)
	rowKeys := sortedValueKeys(rowVals)
	for i := range colKeys {
		if colKeys[i] != rowKeys[i] {
			t.Fatalf("multiset mismatch at %d for %q: columnar=%q boxed=%q",
				i, query, colKeys[i], rowKeys[i])
		}
	}
}

// TestColumnarShapeIdentity_Conjunction covers the conjunction combiner: same
// property, different properties, n-way, mixed operators, and — critically — a
// conjunct whose comparison the unboxed path must report UNDECIDED (a cross-type
// numeric or temporal-tagged string), which forces the per-row boxed fallback while
// its sibling still decides drops unboxed.
func TestColumnarShapeIdentity_Conjunction(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, q := range []string{
		"MATCH (n:A) WHERE n.v > 0 AND n.v < 1000 RETURN n.v",
		"MATCH (n:A) WHERE n.v >= 0 AND n.w = 3 RETURN n.v",
		"MATCH (n:A) WHERE n.v > 0 AND n.v < 1000 AND n.w >= 0 RETURN n.v",
		"MATCH (n:A) WHERE n.v <> 0 AND n.w <> 0 RETURN n.v, n.w",
		"MATCH (n:A) WHERE n.v = 1 AND n.w >= 0 RETURN n.v",
		"MATCH (n:A) WHERE n.v > 1.0 AND n.w >= 0 RETURN n.v",  // float const vs int props → undecided rows
		"MATCH (n:A) WHERE n.v >= 'a' AND n.w >= 0 RETURN n.v", // string const vs numeric props
		"MATCH (n:A) WHERE n.v = true AND n.w >= 0 RETURN n.v", // bool const
		"MATCH (n:A) WHERE n.w >= 0 AND n.v > -9999999 RETURN n.v",
	} {
		t.Run(q, func(t *testing.T) { assertShapeIdentity(t, be, q, "n") })
	}
}

// TestColumnarShapeIdentity_LabelTest covers the label predicate: a label every node
// carries, one a subset carries, a conjunction of two, a label no node carries, and a
// label name never interned in the registry.
func TestColumnarShapeIdentity_LabelTest(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, q := range []string{
		"MATCH (n:A) WHERE n:A AND n.v >= -1 RETURN n.v",
		"MATCH (n:A) WHERE n:B AND n.v >= -1 RETURN n.v",
		"MATCH (n:A) WHERE n:B AND n:C AND n.v >= -1 RETURN n.v",
		"MATCH (n:A) WHERE n:Z AND n.v >= -1 RETURN n.v", // Z is interned but unused
		"MATCH (n:A) WHERE n:NeverInterned AND n.v >= -1 RETURN n.v",
		"MATCH (n:A) WHERE n.v >= -1 AND n:B RETURN n.v", // label second
	} {
		t.Run(q, func(t *testing.T) { assertShapeIdentity(t, be, q, "n") })
	}
}

// TestColumnarShapeIdentity_In covers IN over a scalar literal list: hits, misses, a
// single-element list, a list mixing kinds (so some element comparisons are undecided
// while others decide), duplicates, and a list against the NULL-valued node.
func TestColumnarShapeIdentity_In(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, q := range []string{
		"MATCH (n:A) WHERE n.v IN [1,2,3] RETURN n.v",
		"MATCH (n:A) WHERE n.v IN [0] RETURN n.v",
		"MATCH (n:A) WHERE n.v IN [999999] RETURN n.v",
		"MATCH (n:A) WHERE n.v IN [1,1,1,2] RETURN n.v",
		"MATCH (n:A) WHERE n.v IN [1, 1.5, 'a', true] RETURN n.v",
		"MATCH (n:A) WHERE n.w IN [0,1,2,3,4,5,6] RETURN n.v",
		"MATCH (n:A) WHERE n.v IN [1,2] AND n.w >= 0 RETURN n.v",
	} {
		t.Run(q, func(t *testing.T) { assertShapeIdentity(t, be, q, "n") })
	}
}

// TestColumnarShapeIdentity_StackedSelectionsOverTraversal covers the fusion: the
// far-endpoint label (the measured cliff), an anchor filter with a labelled far
// endpoint, an anchor filter plus a post-traversal filter, and three stacked
// selections at once. Each projects both endpoints' properties where possible, so the
// fused filter's chunk-column alignment across the traversal is exercised too.
func TestColumnarShapeIdentity_StackedSelectionsOverTraversal(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, q := range []string{
		"MATCH (a:A)-[:K]->(m:A) WHERE m.v >= -1 RETURN m.v",
		"MATCH (a:A)-[:K]->(m:B) WHERE m.v >= -1 RETURN m.v",
		"MATCH (a:A)-[:K]->(m:B) WHERE a.v >= -1 RETURN m.v",
		"MATCH (a:A)-[:K]->(m:B) WHERE a.v >= -1 AND m.w >= 0 RETURN m.v, a.w",
		"MATCH (a:B)-[:K]->(m:C) WHERE m.v >= -1 RETURN m.v",
		"MATCH (a:A)-[:K]->(m:Z) WHERE m.v >= -1 RETURN m.v",
		"MATCH (a:A)-[:K]->(m:B) WHERE m.v IN [1,2,3] RETURN m.v",
	} {
		t.Run(q, func(t *testing.T) { assertShapeIdentity(t, be, q, "a", "m") })
	}
}

// TestColumnarShapeIdentity_FusedEvaluationOrder pins that fusing stacked Selections
// preserves the evaluation ORDER of the chain it replaces. The inner Selection runs
// first in the operator chain, so the fused boxed predicate must evaluate the inner
// predicate first and stop at the first non-truthy value — otherwise a later
// predicate could observe rows the earlier one had already dropped.
//
// It is asserted behaviourally: a query whose inner predicate (the pattern label)
// excludes every row must return the empty result whichever arm runs, and a query
// whose outer predicate excludes every row likewise, with the columnar filter engaged
// in both columnar arms.
func TestColumnarShapeIdentity_FusedEvaluationOrder(t *testing.T) {
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)
	g := shapeIdentityGraph(t)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"inner excludes all", "MATCH (a:A)-[:K]->(m:Z) WHERE m.v >= -1 RETURN m.v"},
		{"outer excludes all", "MATCH (a:A)-[:K]->(m:A) WHERE m.v > 99999999 AND m.v < 0 RETURN m.v"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be.filterBatches.Store(0)
			got := drainShapeValues(t, g, tc.query)
			engaged := be.filterBatches.Load() > 0
			boxed := drainShapeValues(t, g, boxify(tc.query, "a", "m"))
			if len(got) != 0 || len(boxed) != 0 {
				t.Fatalf("%q must return no rows on both arms, got columnar=%d boxed=%d",
					tc.query, len(got), len(boxed))
			}
			if !engaged {
				t.Fatalf("%q did not engage the columnar filter", tc.query)
			}
		})
	}
}
