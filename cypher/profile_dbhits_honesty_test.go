package cypher_test

// profile_dbhits_honesty_test.go — rmp #2720.
//
// PROFILE's `dbhits=` column is not one kind of figure. For a scan or seek it is
// DERIVED from the emitted row count on the contract that such an operator reads
// one record per row; for a variable-length expansion it is MEASURED; for several
// operators that read storage it was 0, indistinguishable from a genuine zero —
// until rmp #2760 gave the figure an explicit UNKNOWN state, rendered "?". The
// output still does not say whether a NUMBER is measured or derived; it does now
// say when there is no number at all.
//
// This file is the gate on the parts of that which are claims about behaviour
// rather than about presentation. Each test is written so it FAILS on the
// misleading form:
//
//  1. A variable-length expansion's db-hits must NOT be its row count. The
//     control is the same BFS with a wider emission window: `[*1..3]` and
//     `[*3..3]` traverse the SAME relationship slots on this graph and emit 202
//     rows and 1 row, so a figure that tracks rows moves 202x while the storage
//     work does not move at all. Before the counter was wired, `[*3..3]` reported
//     dbhits=1.
//  2. A type-filtered single-hop expand under-reports, and the test pins the
//     CURRENT number rather than a corrected one — deliberately. The correction
//     needs a per-slot counter the operator does not have and that a non-PROFILE
//     run would pay for, so the divergence stands as a known property. Pinning it
//     is what stops it being re-described as an exact count in a future doc: the
//     test fails if the number silently changes, in EITHER direction.
//  3. The morsel-parallel leaves count no db-hits for a full scan, so their cell
//     must render as "?" and their plan line must name the gap. Without both, the
//     identical query reports N db-hits below the parallel threshold and 0 above
//     it, with nothing to tell a reader that the second zero means "not counted"
//     (rmp #2760 made the state itself renderable; #2720 added the words).
//
// Peer behaviour these gates were calibrated against, read in source: Neo4j
// 5.26.16 counts REAL kernel cursor accesses (OperatorProfileEvent implements
// KernelReadTracer) and charges a hit for a record it read and then rejected
// (DefaultNodeCursor.java:199-210), and leaves a cell it could not measure BLANK
// rather than zero (renderAsTreeTable.scala:415-431), printing "x + ?" for an
// incomplete total (renderSummary.scala:37-44). Memgraph's ACTUAL HITS is a count
// of Pull() invocations (scoped_profile.hpp:58), not of storage accesses, so it is
// derived too — GoGraph's derivation is not unusual among the incumbents; the
// absence of a marker for it is.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runHonestyWrite executes a writing statement and drains it. The name is local
// to this file: cypher_test already has a runWrite with a different signature.
func runHonestyWrite(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	r, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("drain %q: %v", q, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
}

// profiledCells returns the (rows, dbhits) an operator reported, located by the
// prefix of its rendered line, plus whether the db-hits cell was a FIGURE at all.
// It reads Engine.Profile's indented tree rather than the table so a change to
// either renderer cannot make the gate vacuous by simply not finding the
// operator — found is returned and every caller asserts it.
//
// dbhitsKnown is false when the cell rendered as exec.DbHitsUnknown ("?"), in
// which case dbhits is 0 and means nothing. Parsing the "?" explicitly rather
// than letting a numeric scan fail is deliberate: since rmp #2760 a "?" is a
// legitimate rendering, and a parser that returned found=false for it would turn
// every gate below into a t.Fatalf about the harness instead of a statement
// about the engine.
func profiledCells(plan, operator string) (rows, dbhits int64, dbhitsKnown, found bool) {
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, "│└├─ ")
		if !strings.HasPrefix(trimmed, operator) {
			continue
		}
		// The suffix is " (rows=%d, dbhits=%s, time=%s)", where the db-hits cell is
		// either a decimal count or "?".
		i := strings.Index(trimmed, "(rows=")
		if i < 0 {
			return 0, 0, false, false
		}
		var r int64
		var cell, ms string
		if _, err := fmt.Sscanf(trimmed[i:], "(rows=%d, dbhits=%s time=%s", &r, &cell, &ms); err != nil {
			return 0, 0, false, false
		}
		cell = strings.TrimSuffix(cell, ",")
		if cell == "?" {
			return r, 0, false, true
		}
		d, err := strconv.ParseInt(cell, 10, 64)
		if err != nil {
			return 0, 0, false, false
		}
		return r, d, true, true
	}
	return 0, 0, false, false
}

// broomGraph builds a Root with `fan` out-edges, of which exactly one continues
// into a 3-hop chain. A BFS bounded at 3 hops therefore traverses fan+2
// relationship slots WHATEVER emission window it is given, which is what makes
// the two arms of the first gate comparable.
func broomGraph(t *testing.T, fan int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	runHonestyWrite(t, eng, "CREATE (:Root {k:0})")
	for i := 0; i < fan; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("MATCH (r:Root) CREATE (r)-[:E]->(:Mid {i:%d})", i))
	}
	runHonestyWrite(t, eng, "MATCH (m:Mid {i:0}) CREATE (m)-[:E]->(:X {i:0})")
	runHonestyWrite(t, eng, "MATCH (x:X {i:0}) CREATE (x)-[:E]->(:Y {i:0})")
	return eng
}

// TestProfileDbHits_VarLengthExpandCountsTraversalsNotRows is gate 1.
//
// The oracle is a CONTROL ARM, not a constant: `[*1..3]` emits one row per
// relationship slot the BFS enqueues, so its row count IS the number of slots
// traversed, measured by the engine itself on this run. `[*3..3]` runs the same
// BFS to the same depth and emits one row. A db-hits figure derived from rows
// reports the control's count for the control and 1 for the narrow window; a
// figure measured at the traversal reports the same count for both.
func TestProfileDbHits_VarLengthExpandCountsTraversalsNotRows(t *testing.T) {
	t.Parallel()
	const fan = 200
	eng := broomGraph(t, fan)
	ctx := context.Background()

	wide, err := eng.Profile(ctx, "MATCH (r:Root)-[*1..3]->(z) RETURN z", nil)
	if err != nil {
		t.Fatalf("Profile(wide): %v", err)
	}
	narrow, err := eng.Profile(ctx, "MATCH (r:Root)-[*3..3]->(z) RETURN z", nil)
	if err != nil {
		t.Fatalf("Profile(narrow): %v", err)
	}

	wideRows, wideHits, wideKnown, ok := profiledCells(wide, "VarLengthExpand")
	if !ok {
		t.Fatalf("no VarLengthExpand in the wide plan, so this gate covers nothing:\n%s", wide)
	}
	narrowRows, narrowHits, narrowKnown, ok := profiledCells(narrow, "VarLengthExpand")
	if !ok {
		t.Fatalf("no VarLengthExpand in the narrow plan, so this gate covers nothing:\n%s", narrow)
	}

	// Non-vacuity: the two arms must really differ in what they EMIT, or the
	// comparison below proves nothing.
	if wideRows == narrowRows {
		t.Fatalf("both arms emitted %d rows; the graph no longer separates the "+
			"emission window from the traversal, so this gate is vacuous\nwide:\n%s\nnarrow:\n%s",
			wideRows, wide, narrow)
	}
	if narrowRows != 1 {
		t.Errorf("the narrow arm emitted %d rows, want 1 — the broom graph has exactly "+
			"one 3-hop path:\n%s", narrowRows, narrow)
	}
	// The traversal count the control measured.
	// Both arms must report a FIGURE. Since rmp #2760 an operator that claims none
	// of the three db-hits markers renders "?", so an accidental removal of
	// VarLengthExpand's storageAccessCounter would make every comparison below
	// compare 0 with 0 and pass. This is the guard against that.
	if !wideKnown || !narrowKnown {
		t.Fatalf("VarLengthExpand rendered its db-hits as unknown (wide known=%v, "+
			"narrow known=%v). It implements exec's storageAccessCounter, so its "+
			"figure is MEASURED and must render as a number:\nwide:\n%s\nnarrow:\n%s",
			wideKnown, narrowKnown, wide, narrow)
	}
	if wideHits != wideRows {
		t.Errorf("wide arm: dbhits=%d but rows=%d; on `[*1..3]` every enqueued slot "+
			"becomes a row, so the two must agree:\n%s", wideHits, wideRows, wide)
	}
	if narrowHits != wideHits {
		t.Errorf("VarLengthExpand reported dbhits=%d for `[*3..3]` and dbhits=%d for "+
			"`[*1..3]`. Both run the SAME level-synchronous BFS over the same %d "+
			"relationship slots and differ only in which hop counts they emit, so a "+
			"db-hits figure that moves between them is tracking rows, not storage "+
			"reads (rmp #2720).\nnarrow:\n%s\nwide:\n%s",
			narrowHits, wideHits, wideHits, narrow, wide)
	}
	if narrowHits <= narrowRows {
		t.Errorf("narrow arm: dbhits=%d <= rows=%d. A traversal that emitted one row "+
			"after walking a %d-way fan cannot honestly report %d storage reads:\n%s",
			narrowHits, narrowRows, fan, narrowHits, narrow)
	}
}

// TestProfileDbHits_TypeFilteredExpandUnderReports is gate 2: it PINS a known
// divergence rather than asserting a correct number.
//
// A single-hop Expand walks every slot of the source node's adjacency run and
// counts only the slots it emitted, so a type filter that admits one edge in a
// hundred reports one db-hit for a hundred-slot walk. Correcting it needs a
// per-slot counter the operator does not maintain and whose cost a non-PROFILE
// run would pay, which is the trade the derived model exists to refuse — so the
// divergence stands, and this test exists to keep it VISIBLE. It fails if the
// figure changes in either direction: upwards means somebody fixed it and the
// documentation must follow, downwards means something else broke.
func TestProfileDbHits_TypeFilteredExpandUnderReports(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	const others = 99
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

	_, allHits, allKnown, ok := profiledCells(all, "Expand")
	if !ok {
		t.Fatalf("no Expand in the unfiltered plan:\n%s", all)
	}
	filteredRows, filteredHits, filteredKnown, ok := profiledCells(filtered, "Expand")
	if !ok {
		t.Fatalf("no Expand in the filtered plan:\n%s", filtered)
	}

	// Expand is marked exec.StorageRecordScan, so both cells are DERIVED figures
	// and must render as numbers. A "?" here would mean the marker was dropped,
	// which would silently make the pinned under-report below unobservable.
	if !allKnown || !filteredKnown {
		t.Fatalf("Expand rendered its db-hits as unknown (unfiltered known=%v, "+
			"filtered known=%v); it carries exec.StorageRecordScan, so the cell is a "+
			"derived figure:\nunfiltered:\n%s\nfiltered:\n%s",
			allKnown, filteredKnown, all, filtered)
	}
	if allHits != others+1 {
		t.Fatalf("the unfiltered expand reported dbhits=%d, want %d — the graph no "+
			"longer has the shape this gate needs:\n%s", allHits, others+1, all)
	}
	if filteredRows != 1 {
		t.Fatalf("the filtered expand emitted %d rows, want 1:\n%s", filteredRows, filtered)
	}
	if filteredHits != 1 {
		t.Errorf("the type-filtered expand reported dbhits=%d, want 1. This test PINS a "+
			"known under-report (rmp #2720): both queries walk the same %d-slot "+
			"adjacency run and the filtered one charges only the slot it emitted. If "+
			"this number is now the true slot count, the fix is welcome — update this "+
			"test, docs/cypher.md and the StorageRecordScan documentation together, "+
			"because all three currently state the under-report as a fact.\n%s",
			filteredHits, others+1, filtered)
	}
}

// TestProfileDbHits_ParallelLeafDeclaresItsGap is gate 3.
//
// A morsel-parallel leaf counts no db-hits while scanning the whole label, and
// until rmp #2760 it printed the same `dbhits=0` a pure row transformer prints.
// The control arm is the SAME query below the parallel threshold, which plans a
// NodeByLabelScan and reports one db-hit per node: the two arms together are what
// make the leaf's figure a reporting gap rather than a fact about the workload.
//
// Since rmp #2760 the gate asserts the STATE, not the words: the leaf's cell must
// render as unknown ("?") and the control's as the number. The PlanDetail marker
// #2720 added is still asserted, because it says in words which gap the "?" is.
func TestProfileDbHits_ParallelLeafDeclaresItsGap(t *testing.T) {
	t.Parallel()
	const nodes = 2000

	seed := func(threshold int) *cypher.Engine {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{ParallelScanThreshold: threshold})
		t.Cleanup(func() { _ = eng.Close() })
		for i := 0; i < nodes; i++ {
			runHonestyWrite(t, eng, fmt.Sprintf("CREATE (:B {v:%d})", i%100))
		}
		return eng
	}

	const q = "MATCH (n:B) RETURN n.v"
	ctx := context.Background()

	// Control: below the threshold, the same query plans a serial label scan.
	serial, err := seed(nodes*10).Profile(ctx, q, nil)
	if err != nil {
		t.Fatalf("Profile(serial): %v", err)
	}
	serialRows, serialHits, serialKnown, ok := profiledCells(serial, "NodeByLabelScan")
	if !ok {
		t.Fatalf("the control arm did not plan a NodeByLabelScan, so this gate has no "+
			"baseline:\n%s", serial)
	}
	if !serialKnown {
		t.Fatalf("control arm: the serial NodeByLabelScan rendered its db-hits as "+
			"unknown. It carries exec.StorageRecordScan, so its cell is a derived "+
			"figure and must be a number — otherwise the subject arm below has "+
			"nothing to be compared against:\n%s", serial)
	}
	if serialHits != serialRows || serialHits != nodes {
		t.Fatalf("control arm: rows=%d dbhits=%d, want %d for both:\n%s",
			serialRows, serialHits, nodes, serial)
	}

	// Subject: above the threshold, the parallel leaf.
	parallel, err := seed(10).Profile(ctx, q, nil)
	if err != nil {
		t.Fatalf("Profile(parallel): %v", err)
	}
	parRows, _, parKnown, ok := profiledCells(parallel, "ParallelScanProject")
	if !ok {
		t.Fatalf("the subject arm did not plan a ParallelScanProject, so this gate "+
			"covers nothing:\n%s", parallel)
	}
	if parRows != nodes {
		t.Fatalf("the parallel leaf emitted %d rows, want %d:\n%s", parRows, nodes, parallel)
	}
	if parKnown {
		t.Errorf("the parallel leaf rendered a db-hits FIGURE for a scan of all %d "+
			"nodes, but nothing counts its workers' node walk. The identical query "+
			"reports dbhits=%d below the parallel threshold, so a number here would "+
			"be a measurement claim the engine cannot stand behind — the cell must "+
			"render %q (rmp #2760). If a later task (#2762) taught the leaf to count, "+
			"this gate is what forces its documentation and its markers to be updated "+
			"together.\nparallel:\n%s\nserial control:\n%s",
			nodes, serialHits, exec.DbHitsUnknown, parallel, serial)
	}
	if !strings.Contains(parallel, "db-hits not counted") {
		t.Errorf("the parallel leaf's plan line no longer says its db-hits are "+
			"uncounted. The \"?\" says a figure is missing; this detail says WHICH "+
			"gap it is, and rmp #2720 added it for that reason:\n%s", parallel)
	}
	// The "?" must actually reach the rendered line: asserting the parsed state
	// alone would pass even if the renderer printed a zero, because the parser
	// would then report known=true — but only this pins the exact glyph a reader
	// sees, and the mutation that reverts the renderer to "%d" is caught here.
	if !strings.Contains(parallel, "dbhits="+exec.DbHitsUnknown) {
		t.Errorf("the parallel leaf's line does not render dbhits=%s; a reader is "+
			"shown a number for storage nobody counted:\n%s",
			exec.DbHitsUnknown, parallel)
	}
}
