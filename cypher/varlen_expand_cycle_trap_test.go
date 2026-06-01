package cypher_test

// varlen_expand_cycle_trap_test.go — Cypher variable-length expand on
// pattern-cycle traps: diamond and lollipop graphs (task-655).
//
// # Diamond
//
// Graph: "a"→"b"→"d", "a"→"c"→"d".
// Paths of length 2 from "a": a→b→d and a→c→d  → "d" appears twice.
// This is correct: relationship-uniqueness permits two distinct paths that
// share the same terminal node.
//
// # Lollipop
//
// Graph: "a"→"b"→"c"→"b" (b has a self-loop via c).
// Expansion must terminate. [*1..4] from "a": the only cycle b→c→b requires
// re-traversing either the b→c or c→b edge if attempted a second time, which
// relationship-uniqueness prevents. So expansion is always finite.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildDiamond constructs "a"→"b"→"d" and "a"→"c"→"d".
func buildDiamond(tb testing.TB) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(n); err != nil {
			tb.Fatalf("AddNode %q: %v", n, err)
		}
		if err := g.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", n, err)
		}
	}
	for _, e := range [][2]string{{"a", "b"}, {"b", "d"}, {"a", "c"}, {"c", "d"}} {
		if err := g.AddEdge(e[0], e[1], 1.0); err != nil {
			tb.Fatalf("AddEdge %q→%q: %v", e[0], e[1], err)
		}
	}
	return g, cypher.NewEngine(g)
}

// buildLollipop constructs "a"→"b"→"c"→"b".
func buildLollipop(tb testing.TB) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			tb.Fatalf("AddNode %q: %v", n, err)
		}
		if err := g.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", n, err)
		}
	}
	for _, e := range [][2]string{{"a", "b"}, {"b", "c"}, {"c", "b"}} {
		if err := g.AddEdge(e[0], e[1], 1.0); err != nil {
			tb.Fatalf("AddEdge %q→%q: %v", e[0], e[1], err)
		}
	}
	return g, cypher.NewEngine(g)
}

// collectVarlenNames runs MATCH (n {name:"src"})-[*minHops..maxHops]->(m) RETURN m.name
// and returns sorted string results.
func collectVarlenNames(t *testing.T, eng *cypher.Engine, src string, minHops, maxHops int) []string {
	t.Helper()
	ctx := context.Background()
	q := fmt.Sprintf("MATCH (n {name:%q})-[*%d..%d]->(m) RETURN m.name", src, minHops, maxHops)
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	rows := collectRecords(t, res)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		sv, ok := r["m.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("m.name expected StringValue, got %T (%v)", r["m.name"], r["m.name"])
		}
		out = append(out, string(sv))
	}
	sort.Strings(out)
	return out
}

// TestDiamond_Depth1 checks single-hop results from "a": b and c.
func TestDiamond_Depth1(t *testing.T) {
	_, eng := buildDiamond(t)
	got := collectVarlenNames(t, eng, "a", 1, 1)
	want := []string{"b", "c"}
	if len(got) != len(want) {
		t.Fatalf("[*1..1] from a (diamond): got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..1] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDiamond_Depth2 checks that [*1..2] returns "b","c" at depth 1 and "d","d"
// at depth 2 — two distinct paths reach "d".  Total 4 rows with duplicates.
func TestDiamond_Depth2(t *testing.T) {
	_, eng := buildDiamond(t)
	got := collectVarlenNames(t, eng, "a", 1, 2)
	// Depth 1: b, c. Depth 2: d (via b), d (via c). Sorted = b, c, d, d.
	if len(got) != 4 {
		t.Fatalf("[*1..2] from a (diamond): expected 4 rows, got %d: %v", len(got), got)
	}
	want := []string{"b", "c", "d", "d"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..2] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLollipop_Terminates verifies that expansion on the lollipop does not
// loop infinitely. [*1..4] from "a" must return a finite result set.
//
// Reachable nodes from "a" in "a"→"b"→"c"→"b":
//
//	depth 1: b          (a→b)
//	depth 2: c          (a→b→c)
//	depth 3: b          (a→b→c→b)  — revisits b but uses a new edge (c→b)
//	depth 4: c          (a→b→c→b→c) — would need c→b again, already on path → blocked
//	              actually depth 4: nothing new (c→b edge already used)
//
// So [*1..4] yields 3 results: b, c, b (i.e., "b" twice, "c" once).
func TestLollipop_Terminates(t *testing.T) {
	_, eng := buildLollipop(t)
	got := collectVarlenNames(t, eng, "a", 1, 4)
	// Must be finite (not an infinite loop). The exact count depends on
	// relationship-uniqueness propagation through the b→c→b sub-cycle.
	// With relationship-uniqueness:
	//   a→b          (edge a→b used)
	//   a→b→c        (edges a→b, b→c used)
	//   a→b→c→b      (edges a→b, b→c, c→b used)
	//   depth 4 from b: only outgoing edge is b→c (already on path) → no new step
	// Result: 3 rows: b (depth1), c (depth2), b (depth3).
	if len(got) != 3 {
		t.Fatalf("[*1..4] from a (lollipop): expected 3 rows, got %d: %v", len(got), got)
	}
	want := []string{"b", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..4] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLollipop_NoInfiniteLoop is a belt-and-suspenders check: run with a
// large max bound and verify the result count is finite and bounded.
func TestLollipop_NoInfiniteLoop(t *testing.T) {
	_, eng := buildLollipop(t)
	ctx := context.Background()
	res, err := eng.Run(ctx, `MATCH (n {name:"a"})-[*1..20]->(m) RETURN m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	// Regardless of exact count, it must be small (3 unique edge-disjoint paths).
	const maxExpected = 10
	if len(rows) > maxExpected {
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			if sv, ok := r["m.name"].(expr.StringValue); ok {
				names = append(names, string(sv))
			}
		}
		sort.Strings(names)
		t.Fatalf("[*1..20] from a (lollipop): unexpectedly many rows %d (max %d): %v",
			len(rows), maxExpected, names)
	}
}
