package cypher

// explain_estimate_test.go — behavioural tests for the cardinality-estimate
// annotations surfaced in the physical EXPLAIN rendering (task #2099). Every
// assertion is black-box against [Engine.Explain]'s rendered string, the sole
// observable surface the annotations reach. The tests prove:
//
//   - each operator class carries the right estimate + provenance tag (label
//     scan, all-nodes scan, equality on a heavy-hitter vs a non-hitter, a range
//     predicate, an expand, and a range-seek leaf);
//   - the annotations are DISPLAY-ONLY: query results and the chosen plan shape
//     are byte-for-byte identical whether or not statistics are populated;
//   - an absent or stale statistic renders as NO annotation (fallback), never a
//     fabricated exact.
//
// The tests reuse the internal fixtures seedPersonGraph / statsTestSource /
// drainRows (stats_estimate_test.go, join_reorder_diff_test.go).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// planLineContaining returns the first line of a rendered plan that contains sub,
// or "" when no line does. It lets a test target one operator's line without
// depending on the full multi-line layout.
func planLineContaining(t *testing.T, plan, sub string) string {
	t.Helper()
	for _, ln := range strings.Split(plan, "\n") {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return ""
}

// stripEstimates removes the " (est. …)" annotation suffix from every line of a
// rendered plan, leaving the structural plan tree. Two plans with identical
// structure but different annotations compare equal after stripping — the proof
// that statistics changed only the display, not the chosen plan.
func stripEstimates(plan string) string {
	lines := strings.Split(plan, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, " (est. "); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// sortedBag returns a sorted copy of rows so result comparisons are order-
// insensitive (an unordered RETURN may emit rows in any order).
func sortedBag(rows []string) []string {
	out := append([]string(nil), rows...)
	sort.Strings(out)
	return out
}

// TestExplainEstimate_LabelScanExact asserts a NodeByLabelScan carries the exact
// live-node count for its label (estExact, from the label index).
func TestExplainEstimate_LabelScanExact(t *testing.T) {
	const n = 25
	e, _, _ := seedPersonGraph(t, n, 0.0)

	plan, err := e.Explain("MATCH (p:Person) RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	line := planLineContaining(t, plan, "NodeByLabelScan")
	want := fmt.Sprintf("(est. rows=%d, exact)", n)
	if !strings.Contains(line, want) {
		t.Errorf("label scan line = %q, want it to contain %q\nfull plan:\n%s", line, want, plan)
	}
}

// TestExplainEstimate_AllNodesScanExact asserts an AllNodesScan carries the exact
// total live-node count (estExact, from LiveOrder).
func TestExplainEstimate_AllNodesScanExact(t *testing.T) {
	const n = 17
	e, _, _ := seedPersonGraph(t, n, 0.0)

	plan, err := e.Explain("MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	line := planLineContaining(t, plan, "AllNodesScan")
	want := fmt.Sprintf("(est. rows=%d, exact)", n)
	if !strings.Contains(line, want) {
		t.Errorf("all-nodes scan line = %q, want it to contain %q\nfull plan:\n%s", line, want, plan)
	}
}

// TestExplainEstimate_EqualityHeavyHitterVsNonHitter asserts an equality
// predicate on a tracked heavy hitter renders an EXACT MCV count, while one on a
// non-tracked value renders the 1/NDV heuristic — the provenance tags let a
// reader trust-rank them.
func TestExplainEstimate_EqualityHeavyHitterVsNonHitter(t *testing.T) {
	const n = 3000
	e, heavyCount, _ := seedPersonGraph(t, n, 0.40)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	// Heavy hitter: age = 30 is the skew value → MCV-exact with its true count.
	plan, err := e.Explain("MATCH (p:Person) WHERE p.age = 30 RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain heavy: %v", err)
	}
	sel := planLineContaining(t, plan, "Selection")
	wantExact := fmt.Sprintf("(est. rows=%d, exact)", heavyCount)
	if !strings.Contains(sel, wantExact) {
		t.Errorf("heavy-hitter Selection line = %q, want it to contain %q\nfull plan:\n%s", sel, wantExact, plan)
	}

	// Non-hitter: name is distinct-per-node, so an absent name is never an MCV hit
	// → the 1/NDV heuristic (a non-gating hint).
	plan2, err := e.Explain(`MATCH (p:Person) WHERE p.name = 'does-not-exist' RETURN p`, nil)
	if err != nil {
		t.Fatalf("Explain non-hitter: %v", err)
	}
	sel2 := planLineContaining(t, plan2, "Selection")
	if !strings.Contains(sel2, ", heuristic)") {
		t.Errorf("non-hitter Selection line = %q, want a heuristic annotation\nfull plan:\n%s", sel2, plan2)
	}
	if strings.Contains(sel2, ", exact)") {
		t.Errorf("non-hitter must NOT be tagged exact: %q", sel2)
	}
}

// TestExplainEstimate_RangeStatsWithError asserts a single range comparison
// renders the equi-depth histogram estimate tagged stats, with its certified
// absolute error term.
func TestExplainEstimate_RangeStatsWithError(t *testing.T) {
	const n = 5000
	e, _, _ := seedPersonGraph(t, n, 0.30)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	plan, err := e.Explain("MATCH (p:Person) WHERE p.age < 30 RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	sel := planLineContaining(t, plan, "Selection")
	if !strings.Contains(sel, ", stats, err=") {
		t.Errorf("range Selection line = %q, want a stats annotation with an error term\nfull plan:\n%s", sel, plan)
	}
	// Δ = 0 right after a rebuild, so the certified error is exactly 1/B ≈ 0.0039.
	if !strings.Contains(sel, "err=0.0039") {
		t.Errorf("fresh range error should be 1/B = 0.0039; line = %q", sel)
	}
}

// TestExplainEstimate_ExpandDegreeExact asserts an Expand over a single
// relationship type in a directed traversal carries the exact count-store degree
// D(label, relType, dir). The graph is built through the engine so the count
// store is maintained. The assertion is robust to the anchor-swap peephole:
// D(Person,KNOWS,Out) == D(Person,KNOWS,In) == 3 for this symmetric fixture.
func TestExplainEstimate_ExpandDegreeExact(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := NewEngine(g)
	ctx := context.Background()
	if _, err := e.RunInTx(ctx, `CREATE (a:Person {n:'a'}), (b:Person {n:'b'}),
		(c:Person {n:'c'}), (d:Person {n:'d'}),
		(a)-[:KNOWS]->(b), (b)-[:KNOWS]->(c), (c)-[:KNOWS]->(d)`, nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	plan, err := e.Explain(`MATCH (n:Person)-[r:KNOWS]->(m:Person) RETURN n, r, m`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	exp := planLineContaining(t, plan, "Expand")
	if !strings.Contains(exp, "(est. rows=3, exact)") {
		t.Errorf("Expand line = %q, want it to contain %q\nfull plan:\n%s", exp, "(est. rows=3, exact)", plan)
	}
}

// TestExplainEstimate_RangeSeekLeafExact asserts the rewritten
// NodeByIndexRangeScan leaf carries the EXACT in-range index count. The fixture
// clears the range-seek gate (≥ 1024 nodes, ≤ 10% selectivity) so the peephole
// fires and the leaf is rendered.
func TestExplainEstimate_RangeSeekLeafExact(t *testing.T) {
	const n = 1100
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("i%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Item"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "code", lpg.StringValue(fmt.Sprintf("code%04d", i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	e := NewEngine(g)
	ctx := context.Background()
	if _, err := e.RunInTx(ctx, `CREATE INDEX item_code FOR (n:Item) ON (n.code) OPTIONS {indexType:'btree'}`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	// code <= "code0049" matches exactly code0000..code0049 = 50 rows, an inclusive
	// index range the seek serves. 50/1100 ≈ 4.5% ≤ 10%, population ≥ 1024 → fires.
	plan, err := e.Explain(`MATCH (n:Item) WHERE n.code <= 'code0049' RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	leaf := planLineContaining(t, plan, "NodeByIndexRangeScan")
	if leaf == "" {
		t.Fatalf("range seek did not fire (no NodeByIndexRangeScan leaf)\nfull plan:\n%s", plan)
	}
	if !strings.Contains(leaf, "(est. rows=50, exact)") {
		t.Errorf("range-scan leaf = %q, want it to contain %q\nfull plan:\n%s", leaf, "(est. rows=50, exact)", plan)
	}
}

// TestExplainEstimate_DisplayOnly is the differential proof that the annotations
// are display-only: for a battery of queries, both the RESULTS and the chosen
// plan SHAPE (annotations stripped) are identical whether or not statistics are
// populated — while the annotations themselves do appear once statistics exist.
func TestExplainEstimate_DisplayOnly(t *testing.T) {
	const n = 500
	e, _, _ := seedPersonGraph(t, n, 0.30)
	ctx := context.Background()

	queries := []string{
		"MATCH (p:Person) WHERE p.age = 30 RETURN p.age AS a ORDER BY a",
		"MATCH (p:Person) WHERE p.age < 30 RETURN count(p) AS c",
		"MATCH (p:Person) RETURN p.name AS nm ORDER BY nm LIMIT 10",
		"MATCH (n) RETURN count(n) AS c",
	}

	beforeRows := make(map[string][]string, len(queries))
	beforePlan := make(map[string]string, len(queries))
	for _, q := range queries {
		beforeRows[q] = sortedBag(drainRows(t, e, q))
		p, err := e.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain (before) %q: %v", q, err)
		}
		beforePlan[q] = p
	}

	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	for _, q := range queries {
		afterRows := sortedBag(drainRows(t, e, q))
		if !equalStringSlices(beforeRows[q], afterRows) {
			t.Errorf("statistics changed the RESULT of %q:\nbefore %v\nafter  %v", q, beforeRows[q], afterRows)
		}
		p, err := e.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain (after) %q: %v", q, err)
		}
		if got, want := stripEstimates(p), stripEstimates(beforePlan[q]); got != want {
			t.Errorf("statistics changed the PLAN SHAPE of %q:\nbefore:\n%s\nafter:\n%s", q, want, got)
		}
	}

	// Prove the annotations genuinely surfaced after the refresh (a stats-tagged
	// range estimate that was absent before).
	p, err := e.Explain("MATCH (p:Person) WHERE p.age < 30 RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(p, ", stats, err=") {
		t.Errorf("expected a stats annotation to appear after RefreshStatistics; plan:\n%s", p)
	}
}

// TestExplainEstimate_AbsentStatFallback asserts that with no statistics
// populated, a property-predicate Selection carries NO annotation (an absent
// statistic is estFallback → omitted), while the label scan below it still shows
// its genuinely-exact live count.
func TestExplainEstimate_AbsentStatFallback(t *testing.T) {
	const n = 200
	e, _, _ := seedPersonGraph(t, n, 0.30) // deliberately NO RefreshStatistics

	plan, err := e.Explain("MATCH (p:Person) WHERE p.age = 30 RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	sel := planLineContaining(t, plan, "Selection")
	if strings.Contains(sel, "(est. ") {
		t.Errorf("absent statistic must not annotate the equality Selection: %q\nfull plan:\n%s", sel, plan)
	}
	scan := planLineContaining(t, plan, "NodeByLabelScan")
	want := fmt.Sprintf("(est. rows=%d, exact)", n)
	if !strings.Contains(scan, want) {
		t.Errorf("label scan must still show its exact count %q; line = %q", want, scan)
	}
}

// TestExplainEstimate_StaleStatFallback asserts that once a statistic goes stale
// (Δ crosses the firing threshold via real engine writes), the range estimate
// demotes to NO annotation — never a fabricated exact — while the label scan's
// exact live count stays correct.
func TestExplainEstimate_StaleStatFallback(t *testing.T) {
	const n = 400
	e, _, _ := seedPersonGraph(t, n, 0.30)
	ctx := context.Background()
	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	// Fresh: the range Selection is stats-tagged.
	fresh, err := e.Explain("MATCH (p:Person) WHERE p.age < 30 RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain (fresh): %v", err)
	}
	if !strings.Contains(planLineContaining(t, fresh, "Selection"), ", stats") {
		t.Fatalf("fresh range should be stats-tagged; plan:\n%s", fresh)
	}

	// Drive engine SET writes past the Δ/N ≥ b − 1/B firing threshold.
	threshold := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	crossAt := int64(threshold*float64(n)) + 1
	for i := int64(0); i < crossAt+2; i++ {
		q := fmt.Sprintf("MATCH (p:Person {name:'name-%d'}) SET p.age = %d", i, 200+i)
		if _, err := e.RunInTx(ctx, q, nil); err != nil {
			t.Fatalf("SET write %d: %v", i, err)
		}
	}

	stale, err := e.Explain("MATCH (p:Person) WHERE p.age < 30 RETURN p", nil)
	if err != nil {
		t.Fatalf("Explain (stale): %v", err)
	}
	sel := planLineContaining(t, stale, "Selection")
	if strings.Contains(sel, "stats") {
		t.Errorf("stale range must not stay stats-tagged: %q", sel)
	}
	if strings.Contains(sel, "(est. ") {
		t.Errorf("stale range must omit its annotation, not fabricate one: %q", sel)
	}
	// The label scan count is genuinely exact and unaffected by staleness (SET
	// changes no node count) — never a wrong exact.
	scan := planLineContaining(t, stale, "NodeByLabelScan")
	want := fmt.Sprintf("(est. rows=%d, exact)", n)
	if !strings.Contains(scan, want) {
		t.Errorf("label scan must still show the correct exact count %q; line = %q", want, scan)
	}
}

// equalStringSlices reports whether a and b are element-wise equal.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
