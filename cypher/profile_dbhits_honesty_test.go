package cypher_test

// profile_dbhits_honesty_test.go — rmp #2720.
//
// PROFILE's `dbhits=` column is not one kind of figure. For a scan or seek it is
// DERIVED from the emitted row count on the contract that such an operator reads
// one record per row; for a variable-length expansion it is MEASURED; for several
// operators that read storage it is 0. Nothing in the rendered output says which
// of the three a reader is looking at.
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
//  3. The morsel-parallel leaves report 0 db-hits for a full scan, and their plan
//     line must say so. Without the marker the identical query reports N db-hits
//     below the parallel threshold and 0 above it, with nothing to tell a reader
//     that the second zero means "not counted".
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
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
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
// prefix of its rendered line. It reads Engine.Profile's indented tree rather
// than the table so a change to either renderer cannot make the gate vacuous by
// simply not finding the operator — found is returned and every caller asserts
// it.
func profiledCells(plan, operator string) (rows, dbhits int64, found bool) {
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, "│└├─ ")
		if !strings.HasPrefix(trimmed, operator) {
			continue
		}
		var r, d int64
		var ms string
		// The suffix is " (rows=%d, dbhits=%d, time=%s)".
		i := strings.Index(trimmed, "(rows=")
		if i < 0 {
			return 0, 0, false
		}
		if _, err := fmt.Sscanf(trimmed[i:], "(rows=%d, dbhits=%d, time=%s", &r, &d, &ms); err != nil {
			return 0, 0, false
		}
		return r, d, true
	}
	return 0, 0, false
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

	wideRows, wideHits, ok := profiledCells(wide, "VarLengthExpand")
	if !ok {
		t.Fatalf("no VarLengthExpand in the wide plan, so this gate covers nothing:\n%s", wide)
	}
	narrowRows, narrowHits, ok := profiledCells(narrow, "VarLengthExpand")
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

	_, allHits, ok := profiledCells(all, "Expand")
	if !ok {
		t.Fatalf("no Expand in the unfiltered plan:\n%s", all)
	}
	filteredRows, filteredHits, ok := profiledCells(filtered, "Expand")
	if !ok {
		t.Fatalf("no Expand in the filtered plan:\n%s", filtered)
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
// A morsel-parallel leaf reports 0 db-hits while scanning the whole label, and 0
// is also what a pure row transformer reports. The rendered line must therefore
// say which zero it is. The control arm is the SAME query below the parallel
// threshold, which plans a NodeByLabelScan and reports one db-hit per node: the
// two arms together are what make the zero a reporting gap rather than a fact
// about the workload.
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
	serialRows, serialHits, ok := profiledCells(serial, "NodeByLabelScan")
	if !ok {
		t.Fatalf("the control arm did not plan a NodeByLabelScan, so this gate has no "+
			"baseline:\n%s", serial)
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
	parRows, parHits, ok := profiledCells(parallel, "ParallelScanProject")
	if !ok {
		t.Fatalf("the subject arm did not plan a ParallelScanProject, so this gate "+
			"covers nothing:\n%s", parallel)
	}
	if parRows != nodes {
		t.Fatalf("the parallel leaf emitted %d rows, want %d:\n%s", parRows, nodes, parallel)
	}
	if parHits != 0 {
		t.Logf("the parallel leaf now reports dbhits=%d; if it counts its node walk, "+
			"the marker below is no longer needed:\n%s", parHits, parallel)
	}
	if parHits == 0 && !strings.Contains(parallel, "db-hits not counted") {
		t.Errorf("the parallel leaf reports dbhits=0 for a scan of all %d nodes and its "+
			"plan line does not say the figure is uncounted. The identical query "+
			"reports dbhits=%d below the parallel threshold, so the zero is a "+
			"reporting gap, not a property of the query — and nothing in the column "+
			"tells the two zeros apart (rmp #2720). Neo4j leaves such a cell blank "+
			"and prints \"x + ?\" for the total; PostgreSQL suppresses the figure "+
			"entirely.\nparallel:\n%s\nserial control:\n%s",
			nodes, serialHits, parallel, serial)
	}
}
