package cypher_test

// varlen_expand_path_test.go — Cypher variable-length expand [min..max] on a
// directed path P6 (task-645).
//
// Graph: "v0"→"v1"→"v2"→"v3"→"v4"→"v5"  (5 edges).

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// buildPath builds a directed path v0→v1→…→v{n-1}.
func buildPath(tb testing.TB, n int) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range n {
		name := fmt.Sprintf("v%d", i)
		if err := g.AddNode(name); err != nil {
			tb.Fatalf("AddNode %q: %v", name, err)
		}
		if err := g.SetNodeProperty(name, "name", lpg.StringValue(name)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", name, err)
		}
	}
	for i := range n - 1 {
		src := fmt.Sprintf("v%d", i)
		dst := fmt.Sprintf("v%d", i+1)
		if err := g.AddEdge(src, dst, 1.0); err != nil {
			tb.Fatalf("AddEdge %q→%q: %v", src, dst, err)
		}
	}
	return g, cypher.NewEngine(g)
}

// runVarlenFromV0 runs `MATCH (n {name:"v0"})-[*minHops..maxHops]->(m) RETURN m.name`
// and returns the sorted list of m.name string values.
func runVarlenFromV0(t *testing.T, eng *cypher.Engine, minHops, maxHops int) []string {
	t.Helper()
	ctx := context.Background()
	q := fmt.Sprintf(`MATCH (n {name:"v0"})-[*%d..%d]->(m) RETURN m.name`, minHops, maxHops)
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

func TestVarlenPath_1to1(t *testing.T) {
	_, eng := buildPath(t, 6)
	got := runVarlenFromV0(t, eng, 1, 1)
	want := []string{"v1"}
	if len(got) != len(want) {
		t.Fatalf("[*1..1] from v0: got %v, want %v", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("[*1..1] row 0: got %q, want %q", got[0], want[0])
	}
}

func TestVarlenPath_1to2(t *testing.T) {
	_, eng := buildPath(t, 6)
	got := runVarlenFromV0(t, eng, 1, 2)
	want := []string{"v1", "v2"}
	if len(got) != len(want) {
		t.Fatalf("[*1..2] from v0: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..2] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVarlenPath_1to3(t *testing.T) {
	_, eng := buildPath(t, 6)
	got := runVarlenFromV0(t, eng, 1, 3)
	want := []string{"v1", "v2", "v3"}
	if len(got) != len(want) {
		t.Fatalf("[*1..3] from v0: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..3] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVarlenPath_1to6(t *testing.T) {
	// P6 has 5 edges; [*1..6] from v0 reaches at most v5.
	_, eng := buildPath(t, 6)
	got := runVarlenFromV0(t, eng, 1, 6)
	want := []string{"v1", "v2", "v3", "v4", "v5"}
	if len(got) != len(want) {
		t.Fatalf("[*1..6] from v0: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*1..6] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVarlenPath_2to4(t *testing.T) {
	_, eng := buildPath(t, 6)
	got := runVarlenFromV0(t, eng, 2, 4)
	want := []string{"v2", "v3", "v4"}
	if len(got) != len(want) {
		t.Fatalf("[*2..4] from v0: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[*2..4] row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
