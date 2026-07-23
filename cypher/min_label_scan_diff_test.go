package cypher

// min_label_scan_diff_test.go — differential and EXPLAIN tests for the
// min-cardinality multi-label anchor scan (#2077).
//
// The differential test runs each representative query with the optimisation
// ENABLED and DISABLED and asserts a BYTE-IDENTICAL result multiset. Row order
// is unspecified for these queries (no ORDER BY) and re-anchoring the scan
// legitimately changes emission order, so both result sets are sorted to
// canonical form before comparison. A separate assertion confirms the
// optimisation was actually engaged (minLabelScanBuildCount advanced) for the
// cases that must deviate, so the test cannot silently pass by never triggering.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mlNode is one node in a min-label test graph: a unique key property "k" and
// the set of labels it carries.
type mlNode struct {
	k      int
	labels []string
}

// buildMinLabelGraph creates a graph whose nodes carry the given label sets and
// a unique integer "k" property. Labels are interned in first-seen order across
// the whole node list, so callers control label-id assignment (used by the
// deterministic tie-break) by ordering the labels they present.
func buildMinLabelGraph(t *testing.T, nodes []mlNode) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, n := range nodes {
		key := fmt.Sprintf("n%d", n.k)
		if err := g.AddNode(key); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(key, "k", lpg.Int64Value(int64(n.k))); err != nil {
			t.Fatal(err)
		}
		for _, lbl := range n.labels {
			if err := g.SetNodeLabel(key, lbl); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

// nestedLabelGraph builds a graph where the labels form nested subsets:
// counts[0] nodes carry labels[0]; of those, counts[1] also carry labels[1]; of
// those, counts[2] also carry labels[2]; etc. counts must be non-increasing.
// The intersection of ALL labels is counts[len-1] nodes, so a conjunctive query
// over every label returns exactly that many rows. Every node gets a unique k.
func nestedLabelGraph(t *testing.T, labels []string, counts []int) *lpg.Graph[string, float64] {
	t.Helper()
	if len(labels) != len(counts) {
		t.Fatalf("labels/counts length mismatch: %d vs %d", len(labels), len(counts))
	}
	var nodes []mlNode
	k := 0
	for i := 0; i < counts[0]; i++ {
		// A node in bucket i carries labels[0..depth-1], where depth is the number
		// of nested counts that still include position i.
		depth := 0
		for d := 0; d < len(counts); d++ {
			if i < counts[d] {
				depth++
			} else {
				break
			}
		}
		nodes = append(nodes, mlNode{k: k, labels: append([]string(nil), labels[:depth]...)})
		k++
	}
	return buildMinLabelGraph(t, nodes)
}

// drainSortedNil runs q and returns every row rendered as a canonical sorted
// string, matching the hash-join differential helper's rendering.
func drainSortedMinLabel(t *testing.T, e *Engine, q string) []string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		var sb []byte
		for i, c := range cols {
			if i > 0 {
				sb = append(sb, '|')
			}
			sb = append(sb, fmt.Sprintf("%s=%v", c, rec[c])...)
		}
		out = append(out, string(sb))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	sort.Strings(out)
	return out
}

// assertMinLabelIdentical runs q on a min-label-ENABLED and a min-label-DISABLED
// engine over the same graph and asserts the sorted result rows are identical.
// wantTrigger asserts whether the enabled run actually re-anchored the scan.
func assertMinLabelIdentical(t *testing.T, g *lpg.Graph[string, float64], q string, wantTrigger bool) {
	t.Helper()
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableMinLabelScan: true})

	before := minLabelScanBuildCount.Load()
	gotOn := drainSortedMinLabel(t, on, q)
	triggered := minLabelScanBuildCount.Load() > before
	gotOff := drainSortedMinLabel(t, off, q)

	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch for %q: minlabel=%d default=%d", q, len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("row %d differs for %q:\n  minlabel = %s\n  default  = %s", i, q, gotOn[i], gotOff[i])
		}
	}
	if wantTrigger && !triggered {
		t.Fatalf("expected min-label scan to be substituted for %q, but it was not", q)
	}
	if !wantTrigger && triggered {
		t.Fatalf("did NOT expect min-label scan to be substituted for %q, but it was", q)
	}
}

func TestMinLabelScan_Differential_TwoLabels(t *testing.T) {
	// Big=100, Small=5 (Small ⊆ Big). L0=Big, so the scan must re-anchor on Small.
	g := nestedLabelGraph(t, []string{"Big", "Small"}, []int{100, 5})
	assertMinLabelIdentical(t, g, "MATCH (n:Big:Small) RETURN n.k AS k", true)
}

func TestMinLabelScan_Differential_TwoLabels_L0AlreadySmallest(t *testing.T) {
	// Small=5, Big=100 written as (n:Small:Big): L0=Small is already the minimum,
	// so the planner keeps the default plan (no trigger) — still result-identical.
	g := nestedLabelGraph(t, []string{"Big", "Small"}, []int{100, 5})
	assertMinLabelIdentical(t, g, "MATCH (n:Small:Big) RETURN n.k AS k", false)
}

func TestMinLabelScan_Differential_ThreeLabels(t *testing.T) {
	// A=100 ⊇ B=40 ⊇ C=6. L0=A, so the scan re-anchors on the smallest, C.
	g := nestedLabelGraph(t, []string{"A", "B", "C"}, []int{100, 40, 6})
	assertMinLabelIdentical(t, g, "MATCH (n:A:B:C) RETURN n.k AS k", true)
}

func TestMinLabelScan_Differential_FourLabels(t *testing.T) {
	// A=120 ⊇ B=80 ⊇ C=30 ⊇ D=4. Scan re-anchors on the smallest, D.
	g := nestedLabelGraph(t, []string{"A", "B", "C", "D"}, []int{120, 80, 30, 4})
	assertMinLabelIdentical(t, g, "MATCH (n:A:B:C:D) RETURN n.k AS k", true)
}

func TestMinLabelScan_Differential_ZeroPopulationLabel(t *testing.T) {
	// Big=50 nodes, but the label Ghost is never used → cardinality 0. The whole
	// conjunction is empty; the planner scans the empty label (min = 0) rather
	// than scanning Big and dropping every row. Result is empty under both plans.
	g := nestedLabelGraph(t, []string{"Big"}, []int{50})
	assertMinLabelIdentical(t, g, "MATCH (n:Big:Ghost) RETURN n.k AS k", true)
}

func TestMinLabelScan_Differential_CardinalityTie(t *testing.T) {
	// Two disjoint labels of EQUAL cardinality; their intersection is empty, so
	// the result is empty. The point is result-identity and a stable, reproducible
	// plan under the deterministic tie-break — asserted separately below.
	var nodes []mlNode
	k := 0
	for i := 0; i < 20; i++ {
		nodes = append(nodes, mlNode{k: k, labels: []string{"Left"}})
		k++
	}
	for i := 0; i < 20; i++ {
		nodes = append(nodes, mlNode{k: k, labels: []string{"Right"}})
		k++
	}
	g := buildMinLabelGraph(t, nodes)
	q := "MATCH (n:Left:Right) RETURN n.k AS k"
	// Identity holds regardless of which equal-cardinality label anchors the scan.
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableMinLabelScan: true})
	gotOn := drainSortedMinLabel(t, on, q)
	gotOff := drainSortedMinLabel(t, off, q)
	if len(gotOn) != 0 || len(gotOff) != 0 {
		t.Fatalf("expected empty result for a disjoint-label conjunction, got on=%d off=%d", len(gotOn), len(gotOff))
	}
	// Plan stability: the EXPLAIN of a tie must be reproducible across runs.
	e1, err := on.Explain(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := on.Explain(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e1 != e2 {
		t.Fatalf("EXPLAIN of a cardinality tie is not reproducible:\n%s\nvs\n%s", e1, e2)
	}
}

func TestMinLabelScan_Differential_InlineProperty(t *testing.T) {
	// A multi-label node WITH an inline property predicate: the property Selection
	// sits above the label Selection, which the peephole re-anchors underneath it.
	// Big=100, Small=8 (Small ⊆ Big); half of Small carry p=1.
	var nodes []mlNode
	k := 0
	for i := 0; i < 100; i++ {
		labels := []string{"Big"}
		if i < 8 {
			labels = append(labels, "Small")
		}
		nodes = append(nodes, mlNode{k: k, labels: labels})
		k++
	}
	g := buildMinLabelGraph(t, nodes)
	// Set p=1 on the first 4 Small nodes (n0..n3) so the property predicate keeps
	// a non-trivial subset.
	for i := 0; i < 4; i++ {
		if err := g.SetNodeProperty(fmt.Sprintf("n%d", i), "p", lpg.Int64Value(1)); err != nil {
			t.Fatal(err)
		}
	}
	assertMinLabelIdentical(t, g, "MATCH (n:Big:Small {p: 1}) RETURN n.k AS k", true)
}

func TestMinLabelScan_Differential_UnderWhere(t *testing.T) {
	// The extra label arrives via WHERE, not the pattern: MATCH (n:Big) WHERE n:Small.
	// The IR is the same bare-LabelPredicate Selection over NodeByLabelScan(Big),
	// so the peephole re-anchors on Small.
	g := nestedLabelGraph(t, []string{"Big", "Small"}, []int{100, 7})
	assertMinLabelIdentical(t, g, "MATCH (n:Big) WHERE n:Small RETURN n.k AS k", true)
}

func TestMinLabelScan_Differential_OptionalMatch(t *testing.T) {
	// A multi-label node inside OPTIONAL MATCH. The null-row synthesis lives in the
	// Optional wrapper; the inner access path is still re-anchored on the smaller
	// label. An anchor node with no match exercises the null row.
	var nodes []mlNode
	nodes = append(nodes, mlNode{k: 1000, labels: []string{"Anchor"}})
	k := 0
	for i := 0; i < 60; i++ {
		labels := []string{"Big"}
		if i < 5 {
			labels = append(labels, "Small")
		}
		nodes = append(nodes, mlNode{k: k, labels: labels})
		k++
	}
	g := buildMinLabelGraph(t, nodes)
	q := "MATCH (a:Anchor) OPTIONAL MATCH (n:Big:Small) RETURN a.k AS ak, n.k AS nk"
	assertMinLabelIdentical(t, g, q, true)
}

func TestMinLabelScan_IndexSeekStillWins(t *testing.T) {
	// A single-label node with an equality predicate backed by a hash index must
	// still lower to a NodeByIndexSeek. The min-label peephole recognises only a
	// bare LabelPredicate, so it never pre-empts the seek; assert the seek is used
	// and the min-label substitution did NOT fire.
	g := nestedLabelGraph(t, []string{"Person"}, []int{200})
	// Give a few Person nodes a distinct email; create the hash index.
	for i := 0; i < 200; i++ {
		if err := g.SetNodeProperty(fmt.Sprintf("n%d", i), "email", lpg.StringValue(fmt.Sprintf("user%d@x", i))); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(g)
	if _, err := e.Run(context.Background(), "CREATE INDEX FOR (p:Person) ON (p.email)", nil); err != nil {
		t.Fatal(err)
	}
	q := "MATCH (p:Person {email: 'user42@x'}) RETURN p.k AS k"

	plan, err := e.Explain(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("expected NodeByIndexSeek in plan, got:\n%s", plan)
	}

	before := minLabelScanBuildCount.Load()
	got := drainSortedMinLabel(t, e, q)
	if minLabelScanBuildCount.Load() != before {
		t.Fatal("min-label scan must NOT fire for a single-label indexed equality")
	}
	if len(got) != 1 || got[0] != "k=42" {
		t.Fatalf("expected exactly [k=42], got %v", got)
	}
}

func TestMinLabelScan_Explain_TargetsSmallerLabel(t *testing.T) {
	// EXPLAIN must show the NodeByLabelScan anchored on the smaller label.
	g := nestedLabelGraph(t, []string{"Big", "Small"}, []int{100, 5})
	e := NewEngine(g)
	plan, err := e.Explain("MATCH (n:Big:Small) RETURN n.k AS k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "NodeByLabelScan [n:Small]") {
		t.Fatalf("expected the scan to target the smaller label Small, got:\n%s", plan)
	}
	if strings.Contains(plan, "NodeByLabelScan [n:Big]") {
		t.Fatalf("plan must NOT anchor on the larger label Big, got:\n%s", plan)
	}
}
