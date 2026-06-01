package cypher_test

// varlen_expand_cycle_test.go — Cypher variable-length expand [1..k] on a
// directed cycle C4 (task-649).
//
// Graph: "a"→"b"→"c"→"d"→"a"  (4 nodes, 4 edges).
//
// The engine uses relationship-uniqueness semantics: each relationship (edge)
// may appear at most once per path. It does NOT enforce node-uniqueness, so
// the terminal node may coincide with the start node.
//
// Concrete expectations for [*1..4] from "a":
//   - depth 1: "b"            (a→b)
//   - depth 2: "c"            (a→b→c)
//   - depth 3: "d"            (a→b→c→d)
//   - depth 4: "a"            (a→b→c→d→a)  — terminal revisits start; valid
//
// [*1..5] would attempt a 5th hop from "a" but the only outgoing edge from
// "a" is a→b which is already on the path → no new paths, still 4 rows total.

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

// buildCycle builds a directed cycle of n nodes: "a"→"b"→…→back to "a".
// Only works for n ≤ 26 (single-letter names).
func buildCycle(tb testing.TB, n int) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	if n > 26 {
		tb.Fatal("buildCycle: n must be ≤ 26")
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	names := make([]string, n)
	for i := range n {
		names[i] = string(rune('a' + i))
		if err := g.AddNode(names[i]); err != nil {
			tb.Fatalf("AddNode %q: %v", names[i], err)
		}
		if err := g.SetNodeProperty(names[i], "name", lpg.StringValue(names[i])); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", names[i], err)
		}
	}
	for i := range n {
		src := names[i]
		dst := names[(i+1)%n]
		if err := g.AddEdge(src, dst, 1.0); err != nil {
			tb.Fatalf("AddEdge %q→%q: %v", src, dst, err)
		}
	}
	return g, cypher.NewEngine(g)
}

// runVarlenFromA runs `MATCH (n {name:"a"})-[*minHops..maxHops]->(m) RETURN m.name`
// and returns the sorted result strings.
func runVarlenFromA(t *testing.T, eng *cypher.Engine, minHops, maxHops int) []string {
	t.Helper()
	ctx := context.Background()
	q := fmt.Sprintf(`MATCH (n {name:"a"})-[*%d..%d]->(m) RETURN m.name`, minHops, maxHops)
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	rows := collectRecords(t, res)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		sv, ok := r["m.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("m.name expected StringValue, got %T (%v)", r["m.name"], r["m.name"])
		}
		got = append(got, string(sv))
	}
	sort.Strings(got)
	return got
}

func TestVarlenCycle_C4_1to1(t *testing.T) {
	_, eng := buildCycle(t, 4)
	got := runVarlenFromA(t, eng, 1, 1)
	want := []string{"b"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("[*1..1] from a on C4: got %v, want %v", got, want)
	}
}

func TestVarlenCycle_C4_1to2(t *testing.T) {
	_, eng := buildCycle(t, 4)
	got := runVarlenFromA(t, eng, 1, 2)
	want := []string{"b", "c"}
	if len(got) != len(want) {
		t.Fatalf("[*1..2] from a on C4: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..2] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVarlenCycle_C4_1to3(t *testing.T) {
	_, eng := buildCycle(t, 4)
	got := runVarlenFromA(t, eng, 1, 3)
	want := []string{"b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("[*1..3] from a on C4: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..3] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestVarlenCycle_C4_1to4 verifies that depth-4 expansion reaches "a" again
// (relationship-uniqueness semantics; the terminal node may revisit the start).
func TestVarlenCycle_C4_1to4(t *testing.T) {
	_, eng := buildCycle(t, 4)
	got := runVarlenFromA(t, eng, 1, 4)
	// Relationship-uniqueness: a→b→c→d→a uses 4 distinct edges, so "a" at
	// depth 4 is a valid result.
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("[*1..4] from a on C4: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..4] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestVarlenCycle_C4_Finite verifies that expansion terminates (no infinite
// loop) even when max > cycle length. [*1..5] from "a" in C4: at depth 5 the
// only path would require re-traversing the a→b edge already on the path, so
// no new results are added beyond depth 4. Total = 4 rows.
func TestVarlenCycle_C4_Finite(t *testing.T) {
	_, eng := buildCycle(t, 4)
	got := runVarlenFromA(t, eng, 1, 5)
	if len(got) != 4 {
		t.Fatalf("[*1..5] from a on C4: expected 4 rows (cycle saturates at depth 4), got %d: %v",
			len(got), got)
	}
}
