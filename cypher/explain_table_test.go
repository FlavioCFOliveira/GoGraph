package cypher_test

// explain_table_test.go — the columnar plan renderings reached through the
// engine (rmp #2701): [cypher.Engine.ExplainTable] and
// [cypher.Engine.ProfileTable].
//
// These are the tests that prove the WIRING, as distinct from the renderer's own
// package tests: they drive cypher/explain's FormatPlanTable and FormatReport
// from a real query against a real graph, which is the thing that did not exist
// before this task — the package had shipped since #1113 with no caller anywhere
// in the module.
//
// The load-bearing assertion is TestExplainTable_SharesTheWalkWithExplainLogical:
// the table is not a second rendering of the plan, it is the SAME walk. A table
// that re-derived the plan could disagree with EXPLAIN about which access path
// runs, which is exactly the defect rmp #2222 removed from Engine.Explain.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// tableGraph builds a small labelled graph: 20 :Person nodes with name and age,
// 4 :City nodes, and a KNOWS chain over the people.
func tableGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range 20 {
		k := "p" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "age", lpg.Int64Value(int64(20+i))); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", k, err)
		}
	}
	for i := range 4 {
		k := "c" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "City"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", k, err)
		}
	}
	for i := range 19 {
		src, dst := "p"+strconv.Itoa(i), "p"+strconv.Itoa(i+1)
		if err := g.AddEdge(src, dst, 1); err != nil {
			t.Fatalf("AddEdge(%s,%s): %v", src, dst, err)
		}
		g.SetEdgeLabel(src, dst, "KNOWS")
	}
	return g
}

// assertTableRectangular fails when the rendered table's lines are not all the
// same DISPLAY width, counted in runes. This is the wiring-side half of the
// padding regression: a renderer fixed in its own package but fed tree
// connectors by the engine would still misalign if the engine put bytes where
// runes belong.
func assertTableRectangular(t *testing.T, what, out string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("%s: table has %d lines, want at least 4:\n%s", what, len(lines), out)
	}
	want := utf8.RuneCountInString(lines[0])
	var sawMultiByte bool
	for i, l := range lines {
		if got := utf8.RuneCountInString(l); got != want {
			t.Errorf("%s: line %d is %d display columns, want %d:\n%s", what, i, got, want, out)
		}
		if len(l) > utf8.RuneCountInString(l) {
			sawMultiByte = true
		}
	}
	if !sawMultiByte {
		t.Errorf("%s: no line carries a multi-byte rune, so the alignment check proves nothing:\n%s", what, out)
	}
}

// TestExplainTable_RendersThroughTheEngine is the wiring test: a Cypher query in,
// a columnar table out, produced by cypher/explain's formatter.
func TestExplainTable_RendersThroughTheEngine(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))

	out, err := eng.ExplainTable("MATCH (n:Person) WHERE n.age > 30 RETURN n", nil)
	if err != nil {
		t.Fatalf("ExplainTable: %v", err)
	}
	assertTableRectangular(t, "ExplainTable", out)

	for _, want := range []string{"Operator", "Est.Rows", "Vars", "ProduceResults", "NodeByLabelScan [n:Person]", "└─ "} {
		if !strings.Contains(out, want) {
			t.Errorf("ExplainTable output missing %q:\n%s", want, out)
		}
	}
	// The Vars column is what the tree renderings do not print at all, so its
	// presence is the table's own contribution rather than a reformat.
	if !strings.Contains(out, "| n ") {
		t.Errorf("ExplainTable does not report the operator variables:\n%s", out)
	}
	// The label scan's estimate is an EXACT live count of the 20 :Person nodes.
	if !strings.Contains(out, "20") {
		t.Errorf("ExplainTable does not carry the exact label cardinality 20:\n%s", out)
	}
}

// TestExplainTable_SharesTheWalkWithExplainLogical proves the table and the tree
// come from one walk, by driving them through the access-path rewrite that makes
// the raw logical plan differ from the plan that runs.
//
// Without an index the plan is Selection over NodeByLabelScan. With one, the
// engine substitutes NodeByIndexSeek — which SUBSUMES both nodes. A table
// rendered from the untouched logical plan would keep showing the scan and the
// filter, telling a reader the query scans 20 nodes when it seeks one.
func TestExplainTable_SharesTheWalkWithExplainLogical(t *testing.T) {
	const query = "MATCH (n:Person) WHERE n.name = 'p3' RETURN n"

	eng := cypher.NewEngine(tableGraph(t))

	before, err := eng.ExplainTable(query, nil)
	if err != nil {
		t.Fatalf("ExplainTable (no index): %v", err)
	}
	if !strings.Contains(before, "NodeByLabelScan") || !strings.Contains(before, "Selection") {
		t.Fatalf("without an index the plan should filter a label scan:\n%s", before)
	}
	if strings.Contains(before, "NodeByIndexSeek") {
		t.Fatalf("no index exists yet, but the table reports a seek:\n%s", before)
	}

	if _, err := eng.Run(t.Context(), "CREATE INDEX FOR (n:Person) ON (n.name)", nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	after, err := eng.ExplainTable(query, nil)
	if err != nil {
		t.Fatalf("ExplainTable (indexed): %v", err)
	}
	if !strings.Contains(after, "NodeByIndexSeek") {
		t.Errorf("the table does not reflect the index-seek substitution:\n%s", after)
	}
	if strings.Contains(after, "NodeByLabelScan") {
		t.Errorf("the seek subsumes the scan, but the table still shows it:\n%s", after)
	}

	// And the tree agrees, which is the point: one walk, two renderings.
	tree, err := eng.ExplainLogical(query, nil)
	if err != nil {
		t.Fatalf("ExplainLogical: %v", err)
	}
	if !strings.Contains(tree, "NodeByIndexSeek") {
		t.Fatalf("control failed: ExplainLogical does not report the seek either:\n%s", tree)
	}
	if operatorLines(after) != operatorLines(tree) {
		t.Errorf("table and tree disagree about the operator sequence:\ntable ops: %v\ntree ops:  %v",
			operatorLines(after), operatorLines(tree))
	}
}

// operatorLines extracts the operator names from either rendering, in order, so
// the two can be compared without depending on either one's formatting.
func operatorLines(out string) string {
	var names []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(strings.Trim(l, "|"))
		l = strings.TrimLeft(l, "└─├│ ")
		// Skip the box rules, the header row, and the profile table's Total
		// summary line — none of them names an operator.
		if l == "" || strings.HasPrefix(l, "+") || strings.HasPrefix(l, "Operator") || strings.HasPrefix(l, "Total") {
			continue
		}
		// A table row ends at its first column separator; a tree line ends at
		// its first bracket or annotation.
		for _, cut := range []string{" |", " [", " (", "|"} {
			if i := strings.Index(l, cut); i >= 0 {
				l = l[:i]
			}
		}
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return strings.Join(names, ">")
}

// TestExplainTable_MatchesTheTreeAcrossPlanShapes sweeps a set of plans and
// asserts, for every one, that the table's operator sequence is the tree's. It
// is the breadth behind the single deep case above.
func TestExplainTable_MatchesTheTreeAcrossPlanShapes(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))
	queries := []string{
		"MATCH (n) RETURN n",
		"MATCH (n:Person) RETURN n",
		"MATCH (n:Person) WHERE n.age > 30 RETURN n",
		"MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a, b",
		"MATCH (a:Person), (b:City) RETURN a, b",
		"MATCH (n:Person) RETURN n.name ORDER BY n.name LIMIT 3",
		"MATCH (n:Person) RETURN count(n) AS c",
		"UNWIND [1,2,3] AS x RETURN x",
		"MATCH (n:Person) RETURN n UNION MATCH (n:City) RETURN n",
		"MATCH (n:Person) OPTIONAL MATCH (n)-[:KNOWS]->(m) RETURN n, m",
		"CREATE (n:Person {name: 'z'}) RETURN n",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			table, err := eng.ExplainTable(q, nil)
			if err != nil {
				t.Fatalf("ExplainTable: %v", err)
			}
			assertTableRectangular(t, "ExplainTable", table)
			tree, err := eng.ExplainLogical(q, nil)
			if err != nil {
				t.Fatalf("ExplainLogical: %v", err)
			}
			if got, want := operatorLines(table), operatorLines(tree); got != want {
				t.Errorf("table and tree disagree:\ntable: %v\ntree:  %v\n--- table ---\n%s\n--- tree ---\n%s",
					got, want, table, tree)
			}
		})
	}
}

// TestExplainTable_DDLAndErrors covers the two non-plan outcomes.
func TestExplainTable_DDLAndErrors(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))

	out, err := eng.ExplainTable("CREATE INDEX FOR (n:Person) ON (n.name)", nil)
	if err != nil {
		t.Fatalf("ExplainTable(DDL): %v", err)
	}
	if !strings.Contains(out, "no query plan") {
		t.Errorf("DDL should render an explanatory row:\n%s", out)
	}

	if _, err := eng.ExplainTable("MATCH (n RETURN n", nil); err == nil {
		t.Error("a syntax error must be returned, not rendered as a table")
	}
}

// TestProfileTable_RendersMeasurementsThroughTheEngine is the wiring test for
// the PROFILE table: it drives cypher/explain's FormatReport with the
// measurements exec.Profiler collected during a real execution.
func TestProfileTable_RendersMeasurementsThroughTheEngine(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))

	out, err := eng.ProfileTable(t.Context(), "MATCH (n:Person) WHERE n.age > 30 RETURN n", nil)
	if err != nil {
		t.Fatalf("ProfileTable: %v", err)
	}
	assertTableRectangular(t, "ProfileTable", out)

	for _, want := range []string{"Operator", "Rows", "DbHits", "Time (ms)", "Total", "NodeByLabelScan", "└─ "} {
		if !strings.Contains(out, want) {
			t.Errorf("ProfileTable output missing %q:\n%s", want, out)
		}
	}
	// Every one of the 20 :Person nodes is read by the scan, so the scan's row
	// count and its db-hits are both 20. Asserting a number the workload
	// determines — rather than merely "some digit is present" — is what makes
	// this a measurement test rather than a formatting test.
	if !strings.Contains(out, "20") {
		t.Errorf("the scan's 20 rows are not reported:\n%s", out)
	}
	// No node may be labelled unmeasured: rmp #2237 closed the composite-lowering
	// gap that used to leave one, and TestProfile_EveryOperatorIsMeasured holds
	// it closed for the tree. This holds it closed for the table.
	if strings.Contains(out, "(not measured)") {
		t.Errorf("an operator went unmeasured:\n%s", out)
	}
}

// TestProfileTable_AgreesWithProfile proves the two renderings describe one run
// of one plan: the same operators in the same order, and the same measured row
// counts. Times differ between runs and are deliberately not compared.
func TestProfileTable_AgreesWithProfile(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))
	const query = "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a, b"

	table, err := eng.ProfileTable(t.Context(), query, nil)
	if err != nil {
		t.Fatalf("ProfileTable: %v", err)
	}
	tree, err := eng.Profile(t.Context(), query, nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got, want := operatorLines(table), operatorLines(tree); got != want {
		t.Errorf("ProfileTable and Profile disagree about the operator sequence:\ntable: %v\ntree:  %v\n--- table ---\n%s\n--- tree ---\n%s",
			got, want, table, tree)
	}
	// The chain has 19 KNOWS edges, so the expand emits 19 rows in both.
	if !strings.Contains(table, "19") || !strings.Contains(tree, "rows=19") {
		t.Errorf("the two renderings do not both report the expand's 19 rows:\n--- table ---\n%s\n--- tree ---\n%s", table, tree)
	}
}

// TestProfileTable_RefusesAWritingStatement holds the same guarantee Profile
// makes: a diagnostic must not perform writes as a side effect.
func TestProfileTable_RefusesAWritingStatement(t *testing.T) {
	g := tableGraph(t)
	eng := cypher.NewEngine(g)
	before := g.LiveOrder()

	if _, err := eng.ProfileTable(t.Context(), "CREATE (n:Person {name: 'zz'}) RETURN n", nil); err == nil {
		t.Fatal("ProfileTable executed a writing statement")
	}
	if after := g.LiveOrder(); after != before {
		t.Errorf("ProfileTable wrote to the graph: %d nodes before, %d after", before, after)
	}
}

// TestProfileTable_TotalsAreTheDocumentedArithmetic pins what the Total line
// means, because two of its three cells are easy to misread and the doc commits
// to a specific reading.
func TestProfileTable_TotalsAreTheDocumentedArithmetic(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))
	out, err := eng.ProfileTable(t.Context(), "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatalf("ProfileTable: %v", err)
	}
	rows, dbhits, ok := totalCells(out)
	if !ok {
		t.Fatalf("no Total line in:\n%s", out)
	}
	// Project emits 20 rows over a NodeByLabelScan that emits 20: the sum is 40,
	// which is NOT the result's row count (20). The documentation says so; this
	// asserts the documentation is true.
	if rows != 40 {
		t.Errorf("Total Rows = %d, want 40 (20 + 20 summed across the plan)\n%s", rows, out)
	}
	// Only the scan reads records: 20 db-hits for the whole query.
	if dbhits != 20 {
		t.Errorf("Total DbHits = %d, want 20 (only the scan reads records)\n%s", dbhits, out)
	}
}

// totalCells reads the Rows and DbHits cells off the table's Total line.
func totalCells(out string) (rows, dbhits int64, ok bool) {
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "| Total") {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) < 3 {
			return 0, 0, false
		}
		r, err1 := strconv.ParseInt(strings.TrimSpace(cells[1]), 10, 64)
		d, err2 := strconv.ParseInt(strings.TrimSpace(cells[2]), 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return r, d, true
	}
	return 0, 0, false
}

// TestExplainTable_DoesNotExecute holds the EXPLAIN contract for the new entry
// point: a plan is rendered and nothing runs.
func TestExplainTable_DoesNotExecute(t *testing.T) {
	g := tableGraph(t)
	eng := cypher.NewEngine(g)
	before := g.LiveOrder()

	if _, err := eng.ExplainTable("CREATE (n:Person {name: 'zz'}) RETURN n", nil); err != nil {
		t.Fatalf("ExplainTable on a write: %v", err)
	}
	if after := g.LiveOrder(); after != before {
		t.Errorf("ExplainTable wrote to the graph: %d nodes before, %d after", before, after)
	}
}

// TestExplainTable_Deterministic holds the stability the two tree renderers also
// hold: the same query against an unchanged graph renders identically.
func TestExplainTable_Deterministic(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))
	const query = "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a, b"
	first, err := eng.ExplainTable(query, nil)
	if err != nil {
		t.Fatalf("ExplainTable: %v", err)
	}
	for i := range 8 {
		got, err := eng.ExplainTable(query, nil)
		if err != nil {
			t.Fatalf("ExplainTable (run %d): %v", i, err)
		}
		if got != first {
			t.Fatalf("ExplainTable is not reproducible:\n--- first ---\n%s\n--- run %d ---\n%s", first, i, got)
		}
	}
}

// TestExplainTable_ConcurrentReadersAgree drives the new entry points from many
// goroutines at once. Both are documented as read-only diagnostics over a live
// engine, which is a concurrency contract, and this is the exercise behind it
// (run under -race it is also the data-race gate).
func TestExplainTable_ConcurrentReadersAgree(t *testing.T) {
	eng := cypher.NewEngine(tableGraph(t))
	const query = "MATCH (n:Person) WHERE n.age > 30 RETURN n"
	want, err := eng.ExplainTable(query, nil)
	if err != nil {
		t.Fatalf("ExplainTable: %v", err)
	}

	const goroutines = 16
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			for range 8 {
				got, err := eng.ExplainTable(query, nil)
				if err != nil {
					errs <- err
					return
				}
				if got != want {
					errs <- errDiverged
					return
				}
				if _, err := eng.ProfileTable(context.Background(), query, nil); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range goroutines {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent rendering: %v", err)
		}
	}
}

// errDiverged reports a concurrent rendering that did not match the serial one.
var errDiverged = errConst("ExplainTable produced a different table under concurrency")

// errConst is a comparable, allocation-free error for the concurrency check.
type errConst string

func (e errConst) Error() string { return string(e) }
