package cypher_test

// agg_columnar_grouping_test.go — correctness gate for the columnar (unboxed)
// EagerAggregation grouping-key hash path (#2049).
//
// The columnar path hashes and compares scalar grouping keys UNBOXED, boxing a
// key only on new-group creation. Its whole correctness argument is byte-identity
// with the row-at-a-time (boxed) path: both delegate to the same
// expr.EquivalentHash / expr.Equivalent, so two rows group together under one path
// iff they do under the other. These tests PIN that invariant against a query
// forced onto the boxed fallback (a non-property, non-bare-variable grouping-key
// expression, which tryBuildColumnarAggInput rejects), plus the openCypher
// grouping-equivalence hazard cases the float64-domain hash must preserve.
//
// The differential assertions (columnar == fallback) are the primary gate and stay
// valid regardless of any future change to the shared equivalence comparator; the
// absolute-value assertions pin the cases whose openCypher outcome is unambiguous.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runGroup runs query against g and returns one "key=aggs" line per result row,
// sorted so row order is irrelevant. keyCol names the grouping-key column; aggCols
// name the aggregate columns to append.
func runGroup(t *testing.T, g *lpg.Graph[string, float64], query, keyCol string, aggCols ...string) []string {
	t.Helper()
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer res.Close()
	var out []string
	for res.Next() {
		rec := res.Record()
		line := fmt.Sprintf("k=%v", rec[keyCol])
		for _, c := range aggCols {
			line += fmt.Sprintf(" %s=%v", c, rec[c])
		}
		out = append(out, line)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	sort.Strings(out)
	return out
}

// assertColumnarMatchesFallback runs the same grouping/aggregation two ways — the
// columnar path (bare `n.g` grouping key) and the boxed fallback (a coalesce()
// wrapper the columnar builder rejects, an identity over n.g) — and asserts the
// results are byte-identical. It returns the (shared) result for absolute checks.
func assertColumnarMatchesFallback(t *testing.T, g *lpg.Graph[string, float64], keyProp, aggClause string, aggCols ...string) []string {
	t.Helper()
	columnar := runGroup(t, g,
		fmt.Sprintf("MATCH (n) RETURN n.%s AS g, %s", keyProp, aggClause), "g", aggCols...)
	fallback := runGroup(t, g,
		fmt.Sprintf("MATCH (n) RETURN coalesce(n.%s, n.%s) AS g, %s", keyProp, keyProp, aggClause), "g", aggCols...)
	if fmt.Sprint(columnar) != fmt.Sprint(fallback) {
		t.Fatalf("columnar != fallback\n  columnar: %v\n  fallback: %v", columnar, fallback)
	}
	return columnar
}

// gInt builds a fresh graph whose nodes carry integer property "g" with the given
// values.
func gInt(t *testing.T, vals ...int64) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i, v := range vals {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(v)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

func TestColumnarAggGrouping_HazardCases(t *testing.T) {
	big := int64(1) << 53 // two-to-the-53rd, i.e. 9007199254740992

	t.Run("distinct big ints sharing a float64 bit-pattern are separate groups", func(t *testing.T) {
		// 2^53 and 2^53+1 hash into the same float64 bucket but are distinct int64,
		// so grouping (exact int64 equivalence) keeps them apart — openCypher-correct.
		g := gInt(t, big, big+1, big, big+1, big)
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 2 {
			t.Fatalf("want 2 groups (int64 %d and %d are distinct), got %d: %v", big, big+1, len(got), got)
		}
		// 2^53 appears 3×, 2^53+1 appears 2×.
		want := []string{
			fmt.Sprintf("k=%d c=2", big+1),
			fmt.Sprintf("k=%d c=3", big),
		}
		sort.Strings(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("integer 1 groups with float 1.0", func(t *testing.T) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.Int64Value(1))
		mustNode(t, g, "b", lpg.Float64Value(1.0))
		mustNode(t, g, "c", lpg.Int64Value(1))
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 1 {
			t.Fatalf("want 1 group (1 ≡ 1.0), got %d: %v", len(got), got)
		}
		if got[0] != "k=1 c=3" {
			t.Fatalf("got %v, want [k=1 c=3]", got)
		}
	})

	t.Run("NaN groups with NaN", func(t *testing.T) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.Float64Value(math.NaN()))
		mustNode(t, g, "b", lpg.Float64Value(math.NaN()))
		mustNode(t, g, "c", lpg.Float64Value(math.NaN()))
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 1 {
			t.Fatalf("want 1 group (NaN ≡ NaN), got %d: %v", len(got), got)
		}
		if got[0] != "k=NaN c=3" {
			t.Fatalf("got %v, want [k=NaN c=3]", got)
		}
	})

	t.Run("negative zero groups with positive zero", func(t *testing.T) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.Float64Value(math.Copysign(0, -1)))
		mustNode(t, g, "b", lpg.Float64Value(0.0))
		mustNode(t, g, "c", lpg.Float64Value(math.Copysign(0, -1)))
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 1 {
			t.Fatalf("want 1 group (-0.0 ≡ +0.0), got %d: %v", len(got), got)
		}
	})

	t.Run("null keys group together", func(t *testing.T) {
		// Nodes without property "g" → n.g is null → all one group.
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		for _, k := range []string{"a", "b", "c"} {
			if err := g.AddNode(k); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 1 {
			t.Fatalf("want 1 group (all nulls), got %d: %v", len(got), got)
		}
		if got[0] != "k=null c=3" {
			t.Fatalf("got %v, want [k=null c=3]", got)
		}
	})

	t.Run("mixed null and non-null keys", func(t *testing.T) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.Int64Value(7))
		if err := g.AddNode("b"); err != nil { // null key
			t.Fatalf("AddNode: %v", err)
		}
		mustNode(t, g, "c", lpg.Int64Value(7))
		if err := g.AddNode("d"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 2 { // {7: 2} and {null: 2}
			t.Fatalf("want 2 groups, got %d: %v", len(got), got)
		}
	})

	t.Run("cross-type int/float differential over big ints", func(t *testing.T) {
		// int 2^53, int 2^53+1, and float 2^53 together. This exercises the shared
		// cross-type comparator (whose exactness is a separate, pre-existing concern);
		// the columnar path must AGREE with the boxed fallback whatever that comparator
		// decides. Differential only — the absolute grouping here is not asserted.
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.Int64Value(big))
		mustNode(t, g, "b", lpg.Int64Value(big+1))
		mustNode(t, g, "c", lpg.Float64Value(float64(big)))
		mustNode(t, g, "d", lpg.Int64Value(big))
		_ = assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
	})

	t.Run("string keys", func(t *testing.T) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.StringValue("x"))
		mustNode(t, g, "b", lpg.StringValue("y"))
		mustNode(t, g, "c", lpg.StringValue("x"))
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 2 {
			t.Fatalf("want 2 string groups, got %d: %v", len(got), got)
		}
	})

	t.Run("bool keys", func(t *testing.T) {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		mustNode(t, g, "a", lpg.BoolValue(true))
		mustNode(t, g, "b", lpg.BoolValue(false))
		mustNode(t, g, "c", lpg.BoolValue(true))
		mustNode(t, g, "d", lpg.BoolValue(true))
		got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
		if len(got) != 2 {
			t.Fatalf("want 2 bool groups, got %d: %v", len(got), got)
		}
	})
}

func TestColumnarAggGrouping_AggregateSemantics(t *testing.T) {
	// Nodes: g in {0,1,2}, value v = i, so per group we can check every aggregate.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < 30; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(int64(i%3))); err != nil {
			t.Fatalf("SetNodeProperty g: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(fmt.Sprintf("s%d", i%3))); err != nil {
			t.Fatalf("SetNodeProperty name: %v", err)
		}
	}

	clauses := []struct {
		name    string
		clause  string
		aggCols []string
	}{
		{"count(*)", "count(*) AS c", []string{"c"}},
		{"count(v)", "count(n.v) AS c", []string{"c"}},
		{"sum", "sum(n.v) AS s", []string{"s"}},
		{"avg", "avg(n.v) AS a", []string{"a"}},
		{"min", "min(n.v) AS mn", []string{"mn"}},
		{"max", "max(n.v) AS mx", []string{"mx"}},
		{"collect", "collect(n.v) AS xs", []string{"xs"}},
		{"count(distinct name)", "count(DISTINCT n.name) AS c", []string{"c"}},
		{"multi", "count(*) AS c, sum(n.v) AS s, min(n.v) AS mn, max(n.v) AS mx", []string{"c", "s", "mn", "mx"}},
	}
	for _, tc := range clauses {
		t.Run(tc.name, func(t *testing.T) {
			_ = assertColumnarMatchesFallback(t, g, "g", tc.clause, tc.aggCols...)
		})
	}
}

func TestColumnarAggGrouping_CrossBatchTypeChange(t *testing.T) {
	// Force the dynamic grouping-key column to commit int64 in the first FillChunk
	// batch and float64 in a later one: create > DefaultChunkCapacity (4096) integer
	// nodes, then the same numeric values as floats. int k must group with float k
	// across the batch boundary because both the hash (float64 domain) and the
	// comparator (cross-type numeric) route through the same equivalence the boxed
	// path uses.
	const per = 4200
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < per; i++ {
		k := fmt.Sprintf("i%05d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i < per; i++ {
		k := fmt.Sprintf("f%05d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Float64Value(float64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	got := assertColumnarMatchesFallback(t, g, "g", "count(*) AS c", "c")
	if len(got) != per {
		t.Fatalf("want %d groups (int k ≡ float k across batches), got %d", per, len(got))
	}
	for _, line := range got {
		if want := " c=2"; line[len(line)-4:] != want {
			t.Fatalf("every group must have count 2 (one int + one float), got %q", line)
		}
	}
}

func mustNode(t *testing.T, g *lpg.Graph[string, float64], key string, v lpg.PropertyValue) {
	t.Helper()
	if err := g.AddNode(key); err != nil {
		t.Fatalf("AddNode(%q): %v", key, err)
	}
	if err := g.SetNodeProperty(key, "g", v); err != nil {
		t.Fatalf("SetNodeProperty(%q): %v", key, err)
	}
}
