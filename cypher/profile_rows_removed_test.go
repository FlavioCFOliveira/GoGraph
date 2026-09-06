package cypher_test

// profile_rows_removed_test.go — the engine-level gates on PROFILE's
// rows-removed-by-filter figure (rmp #2764).
//
// # What was invisible before, and what these gates hold visible
//
// A Filter that consumed 1000 rows and emitted 3 rendered IDENTICALLY to one that
// consumed 3 and emitted 3: only the emitted count was printed. So a reader could
// not tell a selective access path from a scan that filtered afterwards — the
// question `dbhits` exists to answer and cannot answer alone, because db-hits are
// charged on the operator that READ the record and say nothing about which operator
// later discarded it. docs/explain-profile-honesty-audit-2026-09-03.md §5 records
// the gap as divergence D2, "the sharpest missing figure", against PostgreSQL's
// `Rows Removed by Filter` (19 print sites in explain.c, REL_17_STABLE).
//
// Every gate below is written as a PAIR, because the figure's whole purpose is a
// comparison between two access paths and a single arm proves nothing:
//
//   - the scan-then-filter arm and the index arm answer the SAME question with the
//     SAME 3 rows, and must render differently;
//   - the boxed Filter and the columnar ColumnarFilter run the SAME predicate over
//     the SAME 1000 nodes, and must render the SAME figure;
//   - the unfiltered and type-filtered expansions walk the SAME 100 adjacency
//     slots, and their removed counts must sum with their rows to that 100.
//
// The cells are read back with profiledRemoved (cypher/profile_dbhits_honesty_test.go),
// whose two booleans are kept distinct on purpose: "the operator was not in the
// plan" is a harness failure, "the operator was there and rendered no cell" is an
// engine statement.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// removedFixture builds `total` :P nodes of which exactly `matches` carry age=7,
// and returns an engine over them.
//
// The non-matching ages are spread over a range wide enough that no equality
// predicate other than 7 could accidentally select the same three, and none of them
// is 7. The count is asserted by the caller against the rows the query emits, so a
// generator that drifted would fail the gate rather than silently weaken it.
func removedFixture(t *testing.T, total, matches int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()
	for i := 0; i < total; i++ {
		age := 8 + i // never 7, and distinct
		if i < matches {
			age = 7
		}
		if _, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (:P {age:%d, id:%d})", age, i), nil); err != nil {
			t.Fatalf("seed node %d: %v", i, err)
		}
	}
	return eng
}

// TestProfileRowsRemoved_ScanThenFilterReportsWhatItThrewAway is the headline
// gate: 1000 :P nodes, 3 of them matching, and the Filter must say it removed 997.
//
// Three assertions, not one, and each rules out a different wrong implementation:
//
//   - removed == 997 rules out a figure that is not the rejection count;
//   - rows + removed == the scan's rows rules out an off-by-one and a halving,
//     because it ties the figure to a number the SCAN reported independently; and
//   - the scan itself must render NO removed cell, which is the honesty half —
//     an operator that removes no rows omits the figure rather than printing 0.
func TestProfileRowsRemoved_ScanThenFilterReportsWhatItThrewAway(t *testing.T) {
	t.Parallel()
	const total, matches = 1000, 3
	eng := removedFixture(t, total, matches)

	plan, err := eng.Profile(context.Background(), "MATCH (n:P) WHERE n.age = 7 RETURN n", nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}

	filterRows, _, _, ok := profiledCells(plan, "Filter")
	if !ok {
		t.Fatalf("no Filter in the plan, so this gate covers nothing:\n%s", plan)
	}
	if filterRows != matches {
		t.Fatalf("the Filter emitted %d rows, want %d — the fixture no longer has "+
			"exactly %d matches and every count below would be measured against the "+
			"wrong input:\n%s", filterRows, matches, matches, plan)
	}
	scanRows, _, _, ok := profiledCells(plan, "NodeByLabelScan")
	if !ok {
		t.Fatalf("no NodeByLabelScan in the plan:\n%s", plan)
	}
	if scanRows != total {
		t.Fatalf("the scan emitted %d rows, want %d:\n%s", scanRows, total, plan)
	}

	removed, known, found := profiledRemoved(plan, "Filter")
	if !found {
		t.Fatalf("no Filter line to read:\n%s", plan)
	}
	if !known {
		t.Fatalf("the Filter rendered NO removed cell. It rejects rows, so the figure "+
			"exists and must be printed — this is the whole of rmp #2764:\n%s", plan)
	}
	if want := int64(total - matches); removed != want {
		t.Errorf("the Filter reported removed=%d, want %d. It pulled %d rows from the "+
			"scan and emitted %d:\n%s", removed, want, scanRows, filterRows, plan)
	}
	if got := filterRows + removed; got != scanRows {
		t.Errorf("the Filter reported rows=%d and removed=%d, summing to %d, but its "+
			"child emitted %d rows. Every row pulled is either emitted or removed, so "+
			"a figure that breaks this identity is not a count of rejections:\n%s",
			filterRows, removed, got, scanRows, plan)
	}

	// The honesty half: an operator that cannot remove rows omits the cell.
	if _, scanKnown, scanFound := profiledRemoved(plan, "NodeByLabelScan"); !scanFound || scanKnown {
		t.Errorf("NodeByLabelScan rendered a removed cell (found=%v known=%v). A scan "+
			"removes no rows, so it must OMIT the figure rather than print 0 — the "+
			"same rule rmp #2760 established for db-hits:\n%s", scanFound, scanKnown, plan)
	}
}

// TestProfileRowsRemoved_IndexAccessPathReportsNoFigure is the other half of the
// pair, and the reason the figure was worth adding: the SAME query answered by an
// index must render differently from the same query answered by a scan.
//
// Both arms return the same 3 rows. The scan arm's Filter says `removed=997`; the
// index arm's access path says nothing at all, because it removed nothing — it went
// straight to the three. That difference IS the diagnostic, and before rmp #2764 the
// two plans differed only in an operator name.
//
// The residual Filter the planner leaves above the index arm reports a MEASURED
// `removed=0`, which is a deliberate divergence from PostgreSQL (it suppresses a
// zero count in text mode, explain.c:3638) and is asserted here rather than tolerated:
// a filter that rejected nothing is exactly the finding a reader wants, and here it
// says the index answered the predicate completely.
func TestProfileRowsRemoved_IndexAccessPathReportsNoFigure(t *testing.T) {
	t.Parallel()
	const total, matches = 1000, 3
	eng := removedFixture(t, total, matches)
	ctx := context.Background()

	scanPlan, err := eng.Profile(ctx, "MATCH (n:P) WHERE n.age = 7 RETURN n", nil)
	if err != nil {
		t.Fatalf("Profile(scan arm): %v", err)
	}
	if _, err := eng.RunInTx(ctx, "CREATE INDEX FOR (n:P) ON (n.age)", nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	indexPlan, err := eng.Profile(ctx, "MATCH (n:P) WHERE n.age = 7 RETURN n", nil)
	if err != nil {
		t.Fatalf("Profile(index arm): %v", err)
	}

	// Non-vacuity: the two arms must really have taken different access paths.
	if strings.Contains(indexPlan, "NodeByLabelScan") {
		t.Fatalf("the index arm still planned a NodeByLabelScan, so the two arms are "+
			"the same plan and this gate compares nothing:\n%s", indexPlan)
	}
	accessPath := "NodeByIndexRangeScan"
	if !strings.Contains(indexPlan, accessPath) {
		t.Fatalf("the index arm planned neither a %s nor a label scan; this gate no "+
			"longer knows which line to read:\n%s", accessPath, indexPlan)
	}

	// … and must return the same answer.
	scanRows, _, _, _ := profiledCells(scanPlan, "Filter")
	indexRows, _, _, ok := profiledCells(indexPlan, accessPath)
	if !ok {
		t.Fatalf("no %s line in the index plan:\n%s", accessPath, indexPlan)
	}
	if scanRows != matches || indexRows != matches {
		t.Fatalf("the two arms answered with %d and %d rows, want %d each. They must "+
			"answer identically for the difference in their removed cells to mean "+
			"anything:\nscan:\n%s\nindex:\n%s", scanRows, indexRows, matches, scanPlan, indexPlan)
	}

	// The scan arm says what it threw away.
	scanRemoved, scanKnown, _ := profiledRemoved(scanPlan, "Filter")
	if !scanKnown || scanRemoved != int64(total-matches) {
		t.Fatalf("the scan arm's Filter reported removed=%d (known=%v), want %d:\n%s",
			scanRemoved, scanKnown, total-matches, scanPlan)
	}

	// The index arm's access path says nothing, because it removed nothing.
	_, seekKnown, seekFound := profiledRemoved(indexPlan, accessPath)
	if !seekFound {
		t.Fatalf("no %s line to read in the index plan:\n%s", accessPath, indexPlan)
	}
	if seekKnown {
		t.Errorf("%s rendered a removed cell. An index access path returns only the "+
			"matching entries, so it removes nothing and must OMIT the figure — that "+
			"omission is what distinguishes it from the scan arm, which reports "+
			"removed=%d for the identical answer:\nindex:\n%s\nscan:\n%s",
			accessPath, scanRemoved, indexPlan, scanPlan)
	}

	// The residual Filter above the index reports a MEASURED zero.
	resRemoved, resKnown, resFound := profiledRemoved(indexPlan, "Filter")
	if resFound && resKnown && resRemoved != 0 {
		t.Errorf("the Filter above the index reported removed=%d, want 0: the index "+
			"answered `age = 7` exactly, so nothing was left for it to reject:\n%s",
			resRemoved, indexPlan)
	}
	if resFound && !resKnown {
		t.Errorf("the Filter above the index rendered NO removed cell. A Filter always "+
			"reports the figure; a measured 0 is the finding that the index answered "+
			"the predicate completely, and suppressing it would lose that:\n%s", indexPlan)
	}
}

// TestProfileRowsRemoved_ColumnarFilterReportsTheSameFigure is the columnar arm.
//
// The columnar filter never calls Filter.Next — a columnar parent drives FillChunk
// — so a counter placed only in the boxed loop would report 0 for every query that
// projects a property. The two queries below differ ONLY in what they return
// (`RETURN n` boxes the node and plans a Filter; `RETURN n.id` projects a column
// and plans a ColumnarFilter), so they run the same predicate over the same 1000
// nodes and must report the same figure. Comparing them is what makes this arm a
// measurement rather than a restatement of the implementation.
func TestProfileRowsRemoved_ColumnarFilterReportsTheSameFigure(t *testing.T) {
	t.Parallel()
	const total, matches = 1000, 3
	eng := removedFixture(t, total, matches)
	ctx := context.Background()

	boxed, err := eng.Profile(ctx, "MATCH (n:P) WHERE n.age = 7 RETURN n", nil)
	if err != nil {
		t.Fatalf("Profile(boxed): %v", err)
	}
	columnar, err := eng.Profile(ctx, "MATCH (n:P) WHERE n.age = 7 RETURN n.id", nil)
	if err != nil {
		t.Fatalf("Profile(columnar): %v", err)
	}

	if !strings.Contains(columnar, "ColumnarFilter") {
		t.Fatalf("the projecting query did not plan a ColumnarFilter, so this gate "+
			"never reaches the columnar path:\n%s", columnar)
	}
	if strings.Contains(boxed, "ColumnarFilter") {
		t.Fatalf("the boxed query planned a ColumnarFilter too, so the two arms are "+
			"the same path and this gate compares nothing:\n%s", boxed)
	}

	colRows, _, _, ok := profiledCells(columnar, "ColumnarFilter")
	if !ok {
		t.Fatalf("no ColumnarFilter line:\n%s", columnar)
	}
	if colRows != matches {
		t.Fatalf("the ColumnarFilter emitted %d rows, want %d:\n%s", colRows, matches, columnar)
	}

	colRemoved, colKnown, colFound := profiledRemoved(columnar, "ColumnarFilter")
	if !colFound {
		t.Fatalf("no ColumnarFilter line to read:\n%s", columnar)
	}
	if !colKnown {
		t.Fatalf("the ColumnarFilter rendered NO removed cell. It rejects rows inside "+
			"FillChunk, which never calls Filter.Next, so this is what a counter placed "+
			"only in the boxed loop looks like:\n%s", columnar)
	}
	boxedRemoved, _, _ := profiledRemoved(boxed, "Filter")
	if want := int64(total - matches); colRemoved != want {
		t.Errorf("the ColumnarFilter reported removed=%d, want %d:\n%s",
			colRemoved, want, columnar)
	}
	if colRemoved != boxedRemoved {
		t.Errorf("the columnar path reported removed=%d and the boxed path removed=%d "+
			"for the SAME predicate over the SAME %d nodes. The two paths of one filter "+
			"decide every row identically, so their rejection counts must agree:\n"+
			"columnar:\n%s\nboxed:\n%s", colRemoved, boxedRemoved, total, columnar, boxed)
	}
	if got := colRows + colRemoved; got != total {
		t.Errorf("the ColumnarFilter reported rows=%d and removed=%d, summing to %d, "+
			"but its scan child read %d nodes:\n%s", colRows, colRemoved, got, total, columnar)
	}
}

// TestProfileRowsRemoved_TypeFilteredExpandSaysWhatItSkipped covers the third
// reporting family, and closes the loop on the audit's refutation 2 from the
// READER's side.
//
// rmp #2761 made `Expand` report the adjacency slots it WALKED, so a type-filtered
// hop and an unfiltered one over the same 100-slot run both report dbhits=100. That
// fixed the storage figure but left the reader to infer the rejections by
// subtraction. Here the operator says it directly, and the gate ties the three
// figures together: rows + removed == dbhits, on both arms.
//
// The unfiltered arm is the control. It walks the same slots and rejects NONE, so
// it reports a MEASURED `removed=0` — the case PostgreSQL would suppress and this
// codebase prints, because "walked 100 and rejected none" and "walked 100 and
// rejected 99" is exactly the comparison the figure exists to make.
func TestProfileRowsRemoved_TypeFilteredExpandSaysWhatItSkipped(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	const others = 99
	const slots = others + 1
	runHonestyWrite(t, eng, "CREATE (:Root {k:0})")
	for i := 0; i < others; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("MATCH (r:Root) CREATE (r)-[:LIKES]->(:Leaf {i:%d})", i))
	}
	runHonestyWrite(t, eng, "MATCH (r:Root) CREATE (r)-[:KNOWS]->(:Leaf {i:999})")

	ctx := context.Background()
	all, err := eng.Profile(ctx, "MATCH (r:Root)-->(b) RETURN b", nil)
	if err != nil {
		t.Fatalf("Profile(unfiltered): %v", err)
	}
	filtered, err := eng.Profile(ctx, "MATCH (r:Root)-[:KNOWS]->(b) RETURN b", nil)
	if err != nil {
		t.Fatalf("Profile(filtered): %v", err)
	}

	for _, arm := range []struct {
		name        string
		plan        string
		wantRows    int64
		wantRemoved int64
	}{
		{"unfiltered `-->`", all, slots, 0},
		{"type-filtered `-[:KNOWS]->`", filtered, 1, others},
	} {
		rows, hits, hitsKnown, ok := profiledCells(arm.plan, "Expand")
		if !ok {
			t.Fatalf("%s: no Expand in the plan:\n%s", arm.name, arm.plan)
		}
		removed, known, _ := profiledRemoved(arm.plan, "Expand")
		if !known {
			t.Fatalf("%s: Expand rendered NO removed cell; it implements "+
				"rowsRemovedCounter so the figure must be printed:\n%s", arm.name, arm.plan)
		}
		if rows != arm.wantRows {
			t.Errorf("%s: Expand emitted %d rows, want %d:\n%s",
				arm.name, rows, arm.wantRows, arm.plan)
		}
		if removed != arm.wantRemoved {
			t.Errorf("%s: Expand reported removed=%d, want %d. The Root has %d "+
				"out-edges, of which %d are :KNOWS:\n%s",
				arm.name, removed, arm.wantRemoved, slots, 1, arm.plan)
		}
		if !hitsKnown {
			t.Fatalf("%s: Expand's db-hits rendered unknown, so the identity below "+
				"cannot be checked:\n%s", arm.name, arm.plan)
		}
		if got := rows + removed; got != hits {
			t.Errorf("%s: Expand reported rows=%d + removed=%d = %d against dbhits=%d. "+
				"Every adjacency slot the cursor consumed is either emitted or "+
				"discarded, so the two counters must close over the same walk — they "+
				"are counted independently, one from the cursor positions and one from "+
				"the reject branches, which is what makes the identity a test:\n%s",
				arm.name, rows, removed, got, hits, arm.plan)
		}
	}
}

// TestProfileTableRowsRemoved_ColumnAppearsOnlyWhenSomethingRemoves pins the
// columnar-table rendering, whose rule differs from the tree's by one level.
//
// The tree omits a CELL. The table omits the whole COLUMN when no operator in the
// plan reports the figure — Neo4j's rule for an argument no plan node carries
// (renderAsTreeTable.scala, 5.26.16) — so a plan with no filter renders exactly the
// four columns it always did and no existing reader of the table is disturbed.
//
// The Total cell is asserted BLANK. No plan-wide total is claimed: unlike db-hits,
// which every operator is classified for, rejection is reported by three operator
// families and others discard rows for reasons this figure would misdescribe, so a
// summed cell would be a floor presented as a total (see exec's rowsRemovedCounter).
func TestProfileTableRowsRemoved_ColumnAppearsOnlyWhenSomethingRemoves(t *testing.T) {
	t.Parallel()
	const total, matches = 1000, 3
	eng := removedFixture(t, total, matches)
	ctx := context.Background()

	withFilter, err := eng.ProfileTable(ctx, "MATCH (n:P) WHERE n.age = 7 RETURN n", nil)
	if err != nil {
		t.Fatalf("ProfileTable(with filter): %v", err)
	}
	noFilter, err := eng.ProfileTable(ctx, "MATCH (n:P) RETURN n", nil)
	if err != nil {
		t.Fatalf("ProfileTable(no filter): %v", err)
	}

	if !strings.Contains(withFilter, "| Removed |") {
		t.Errorf("the filtered plan's table carries no Removed column:\n%s", withFilter)
	}
	if strings.Contains(noFilter, "Removed") {
		t.Errorf("a plan with nothing that removes rows still rendered a Removed "+
			"column. The column is omitted entirely in that case, so every existing "+
			"reader of this table sees exactly the four columns it always had:\n%s", noFilter)
	}

	// The Filter's cell carries the figure; the scan's is blank; the Total's is blank.
	//
	// The column is located by its HEADER rather than by a fixed index: rmp #2765
	// inserted an Est.Rows column to the LEFT of Rows, which moves Removed one place
	// right whenever some operator carries an estimate. A positional read would then
	// have asserted about the Time column while still passing on some plans.
	removedCol, okCol := tableColumnIndex(withFilter, "Removed")
	if !okCol {
		t.Fatalf("no Removed column header in the filtered plan's table:\n%s", withFilter)
	}
	var filterCell, scanCell, totalCell string
	for _, line := range strings.Split(withFilter, "\n") {
		if !strings.HasPrefix(line, "|") || strings.Contains(line, "Operator") {
			continue
		}
		cols := strings.Split(strings.Trim(line, "|"), "|")
		if len(cols) <= removedCol {
			continue
		}
		cell := strings.TrimSpace(cols[removedCol])
		switch {
		case strings.Contains(cols[0], "Filter"):
			filterCell = cell
		case strings.Contains(cols[0], "NodeByLabelScan"):
			scanCell = cell
		case strings.Contains(cols[0], "Total"):
			totalCell = cell
		}
	}
	if filterCell != fmt.Sprintf("%d", total-matches) {
		t.Errorf("the Filter's Removed cell is %q, want %q:\n%s",
			filterCell, fmt.Sprintf("%d", total-matches), withFilter)
	}
	if scanCell != "" {
		t.Errorf("the NodeByLabelScan's Removed cell is %q, want blank. A blank cell "+
			"says the operator removes no rows; the %q one column left says a figure "+
			"exists and nobody counted it. The two must not print the same thing:\n%s",
			scanCell, "?", withFilter)
	}
	if totalCell != "" {
		t.Errorf("the Total row's Removed cell is %q, want blank. No plan-wide total "+
			"is claimed, because operators outside the three reporting families discard "+
			"rows this figure does not describe and a sum would be a floor presented as "+
			"a total:\n%s", totalCell, withFilter)
	}
}

// TestProfileRowsRemoved_ReverseDirectionRejectionsAreCounted covers the three
// rejection branches on Expand's REVERSE cursor, which the forward-only gate above
// leaves entirely untouched.
//
// This test exists because a mutation said so. Removing all four reverse-side
// increments — the self-loop deduplication, the reverse type filter, the reverse
// cyphermorphism check and the reverse expand-into comparison — left
// `go test ./cypher/ ./cypher/exec/` fully green (rmp #2764, mutation M6). Half of
// Expand's rejection accounting was therefore unproved, and a figure that is only
// right in one direction is worse than no figure, because the reader cannot tell
// which direction they are looking at.
//
// The three arms are chosen so that each isolates ONE branch, and each is checked
// against the same rows + removed == dbhits identity the forward gate uses, so the
// count is tied to the independently-counted slot walk rather than asserted alone.
func TestProfileRowsRemoved_ReverseDirectionRejectionsAreCounted(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	// A Hub with 99 :LIKES in-edges and one :KNOWS in-edge — the mirror of the
	// forward fixture, so a DirIn hop exercises reverseEdgePassesFilter.
	runHonestyWrite(t, eng, "CREATE (:Hub {k:0})")
	for i := 0; i < 99; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("MATCH (h:Hub) CREATE (:Src {i:%d})-[:LIKES]->(h)", i))
	}
	runHonestyWrite(t, eng, "MATCH (h:Hub) CREATE (:Src {i:999})-[:KNOWS]->(h)")

	// A node carrying a self-loop plus one ordinary out-edge. An undirected hop
	// walks the self-loop once forward and once in reverse, and the reverse copy is
	// deduplicated (openCypher 9: an undirected pattern matches each edge once).
	runHonestyWrite(t, eng, "CREATE (:Solo {k:1})")
	runHonestyWrite(t, eng, "MATCH (s:Solo) CREATE (s)-[:SELF]->(s)")
	runHonestyWrite(t, eng, "MATCH (s:Solo) CREATE (s)-[:OTHER]->(:Leaf2 {k:2})")

	// A single directed edge, for the reverse cyphermorphism branch: the second,
	// undirected hop sees the SAME relationship coming back and must reject it.
	runHonestyWrite(t, eng, "CREATE (:M1 {k:1})-[:E]->(:M2 {k:2})")

	ctx := context.Background()
	for _, arm := range []struct {
		name        string
		query       string
		branch      string
		wantRows    int64
		wantRemoved int64
		wantHits    int64
	}{
		{
			name:   "reverse type filter",
			query:  "MATCH (h:Hub)<-[:KNOWS]-(x) RETURN x",
			branch: "Expand.reverseEdgePassesFilter",
			// 100 in-edges walked in reverse, 1 admitted.
			wantRows: 1, wantRemoved: 99, wantHits: 100,
		},
		{
			name:   "undirected self-loop deduplication",
			query:  "MATCH (s:Solo)--(x) RETURN x",
			branch: "the DirBoth self-loop guard in advanceRevEdge",
			// 2 forward slots (the self-loop and :OTHER) both emitted, plus 1 reverse
			// slot (the self-loop again) rejected as already emitted.
			wantRows: 2, wantRemoved: 1, wantHits: 3,
		},
		{
			name:   "reverse cyphermorphism",
			query:  "MATCH (a:M1)-[r1:E]->(b)--(c) RETURN c",
			branch: "Expand.passesRelMorphism on the reverse cursor",
			// The outer hop walks b's single reverse slot, which is r1 — already
			// bound by the first hop, so openCypher 9 §3.2.2 rejects it.
			wantRows: 0, wantRemoved: 1, wantHits: 1,
		},
	} {
		plan, err := eng.Profile(ctx, arm.query, nil)
		if err != nil {
			t.Fatalf("%s: Profile: %v", arm.name, err)
		}
		// The outermost Expand is the one under test; profiledCells reads the FIRST
		// line naming the operator, which in a nested plan is the outer hop.
		rows, hits, hitsKnown, ok := profiledCells(plan, "Expand")
		if !ok {
			t.Fatalf("%s: no Expand in the plan:\n%s", arm.name, plan)
		}
		removed, known, _ := profiledRemoved(plan, "Expand")
		if !known {
			t.Fatalf("%s: Expand rendered NO removed cell:\n%s", arm.name, plan)
		}
		if rows != arm.wantRows || hits != arm.wantHits {
			t.Fatalf("%s: Expand reported rows=%d dbhits=%d, want %d and %d — the "+
				"fixture no longer produces the walk this arm describes, so its "+
				"removed count would be measured against the wrong shape:\n%s",
				arm.name, rows, hits, arm.wantRows, arm.wantHits, plan)
		}
		if !hitsKnown {
			t.Fatalf("%s: Expand's db-hits rendered unknown:\n%s", arm.name, plan)
		}
		if removed != arm.wantRemoved {
			t.Errorf("%s: Expand reported removed=%d, want %d. This arm exists to "+
				"cover %s, and no other test reaches it:\n%s",
				arm.name, removed, arm.wantRemoved, arm.branch, plan)
		}
		if got := rows + removed; got != hits {
			t.Errorf("%s: rows=%d + removed=%d = %d against dbhits=%d:\n%s",
				arm.name, rows, removed, got, hits, plan)
		}
	}
}

// TestProfileRowsRemoved_ColumnarExpandReportsThroughAPlannedQuery is the
// engine-level half of the columnar gate, and it exists for the one thing the
// exec-level gate in cypher/exec/expand_columnar_rows_removed_test.go cannot show:
// that the PLANNER actually reaches the columnar expand with rejections to report.
//
// The exec gate constructs a columnarExpand directly and controls its config, which
// makes it precise but says nothing about reachability — a planner that stopped
// building columnarExpand would leave it green while no real query exercised the
// path. This drives four real queries and asserts on the rendered `columnarExpand`
// line, so a lost columnar chain fails here (loudly, via the t.Fatalf below) rather
// than silently retiring the figure.
//
// Both directions are covered, because the columnar path charges its forward and
// reverse rejections at two separate sites: a per-site deletion sweep showed that
// deleting EITHER one left every gate package green before this test and its exec
// counterpart existed (rmp #2764, sites S5 and S6).
//
// The trailing `WHERE p.v >= 0 RETURN p.v` is what engages the columnar chain; it
// is a predicate every node satisfies, so it removes nothing of its own and the
// ColumnarFilter above reports removed=0 in all four arms.
func TestProfileRowsRemoved_ColumnarExpandReportsThroughAPlannedQuery(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	const others = 99
	const slots = others + 1

	// A Root with 99 :LIKES and one :KNOWS OUT-edge, for the forward arm.
	runHonestyWrite(t, eng, "CREATE (:Root {k:0})")
	for i := 0; i < others; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("MATCH (r:Root) CREATE (r)-[:LIKES]->(:Leaf {v:%d})", i))
	}
	runHonestyWrite(t, eng, "MATCH (r:Root) CREATE (r)-[:KNOWS]->(:Leaf {v:999})")
	// A Hub with 99 :LIKES and one :KNOWS IN-edge, for the reverse arm.
	runHonestyWrite(t, eng, "CREATE (:Hub {k:0})")
	for i := 0; i < others; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("MATCH (h:Hub) CREATE (:Src {v:%d})-[:LIKES]->(h)", i))
	}
	runHonestyWrite(t, eng, "MATCH (h:Hub) CREATE (:Src {v:999})-[:KNOWS]->(h)")

	ctx := context.Background()
	for _, arm := range []struct {
		name        string
		query       string
		site        string
		wantRows    int64
		wantRemoved int64
	}{
		{"forward control", "MATCH (r:Root)-->(p) WHERE p.v >= 0 RETURN p.v",
			"fillOneChunkRow forward (S5)", slots, 0},
		{"forward subject", "MATCH (r:Root)-[:KNOWS]->(p) WHERE p.v >= 0 RETURN p.v",
			"fillOneChunkRow forward (S5)", 1, others},
		{"reverse control", "MATCH (h:Hub)<--(p) WHERE p.v >= 0 RETURN p.v",
			"fillOneChunkRow reverse (S6)", slots, 0},
		{"reverse subject", "MATCH (h:Hub)<-[:KNOWS]-(p) WHERE p.v >= 0 RETURN p.v",
			"fillOneChunkRow reverse (S6)", 1, others},
	} {
		plan, err := eng.Profile(ctx, arm.query, nil)
		if err != nil {
			t.Fatalf("%s: Profile: %v", arm.name, err)
		}
		rows, hits, hitsKnown, ok := profiledCells(plan, "columnarExpand")
		if !ok {
			t.Fatalf("%s: no columnarExpand in the plan — the columnar chain did not "+
				"engage, so this gate has no subject and the columnar charge sites are "+
				"unreachable by any query:\n%s", arm.name, plan)
		}
		removed, known, _ := profiledRemoved(plan, "columnarExpand")
		if !known {
			t.Fatalf("%s: columnarExpand rendered NO removed cell. It embeds *Expand "+
				"and implements rowsRemovedCounter, so the figure exists:\n%s", arm.name, plan)
		}
		if rows != arm.wantRows || !hitsKnown || hits != slots {
			t.Fatalf("%s: columnarExpand reported rows=%d dbhits=%d (known=%v), want %d "+
				"and %d — the fixture no longer produces the walk this arm describes:\n%s",
				arm.name, rows, hits, hitsKnown, arm.wantRows, slots, plan)
		}
		if removed != arm.wantRemoved {
			t.Errorf("%s: columnarExpand reported removed=%d, want %d. The %d-slot run "+
				"admits one :KNOWS edge, and this arm is the only planned query that "+
				"reaches %s:\n%s", arm.name, removed, arm.wantRemoved, slots, arm.site, plan)
		}
		if got := rows + removed; got != hits {
			t.Errorf("%s: rows=%d + removed=%d = %d against dbhits=%d. Every slot the "+
				"columnar cursor consumed is either appended or discarded, and the two "+
				"figures are counted by unrelated state:\n%s",
				arm.name, rows, removed, got, hits, plan)
		}
	}
}
