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
//  3. The morsel-parallel leaves must report the node references their workers
//     consumed, and must report the SAME figure as the serial plan for the same
//     query on the same graph. This gate inverted at rmp #2762: it used to assert
//     the cell rendered "?" because nothing counted the walk, having asserted
//     before rmp #2760 only that the plan line said so in words (#2720). What it
//     asserts now is parity across the parallel threshold — the setting the reader
//     did not choose — for all THREE leaves, each driven by a query whose row count
//     differs from its node walk so that a rows-derived figure cannot pass.
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

// TestProfileDbHits_TypeFilteredExpandCountsSlotsWalked is gate 2: a single-hop
// Expand must report the adjacency slots it WALKED, not the ones it emitted.
//
// It used to be TestProfileDbHits_TypeFilteredExpandUnderReports, and it pinned
// the wrong number on purpose: Expand carried exec.StorageRecordScan, so its
// db-hits were derived from its rows, and a type filter admitting one edge in a
// hundred reported one db-hit for a hundred-slot walk. rmp #2761 gave the operator
// a counter and the assertion is inverted with it — the two arms below walk the
// SAME 100-slot CSR run and must now report the SAME 100 db-hits while emitting
// 100 rows and 1 row respectively.
//
// The control is what makes the subject falsifiable: `-->` and `-[:KNOWS]->` are
// provably the same walk on this graph, so a figure that still moves between them
// is tracking rows. The rows are asserted too, in both arms — a "fix" that made
// the filtered arm emit 100 rows would satisfy the db-hits equality and be a
// correctness regression.
//
// Neo4j 5.26.16 defines the figure the same way:
// RecordRelationshipTraversalCursor.next() calls tracer.onRelationship() INSIDE
// the do/while whose condition is the type-and-direction selection test, so a
// record read and rejected is still charged (rmp #2761).
func TestProfileDbHits_TypeFilteredExpandCountsSlotsWalked(t *testing.T) {
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

	allRows, allHits, allKnown, ok := profiledCells(all, "Expand")
	if !ok {
		t.Fatalf("no Expand in the unfiltered plan:\n%s", all)
	}
	filteredRows, filteredHits, filteredKnown, ok := profiledCells(filtered, "Expand")
	if !ok {
		t.Fatalf("no Expand in the filtered plan:\n%s", filtered)
	}

	// Expand implements exec.storageAccessCounter since rmp #2761, so both cells are
	// MEASURED figures and must render as numbers. A "?" here would mean the counter
	// was dropped, which would make every assertion below unobservable.
	if !allKnown || !filteredKnown {
		t.Fatalf("Expand rendered its db-hits as unknown (unfiltered known=%v, "+
			"filtered known=%v); it counts its own adjacency slots, so the cell is a "+
			"measured figure:\nunfiltered:\n%s\nfiltered:\n%s",
			allKnown, filteredKnown, all, filtered)
	}
	if allRows != slots || filteredRows != 1 {
		t.Fatalf("the arms emitted %d and %d rows, want %d and 1 — the graph no longer "+
			"has the shape this gate needs:\nunfiltered:\n%s\nfiltered:\n%s",
			allRows, filteredRows, slots, all, filtered)
	}
	if allHits != slots {
		t.Errorf("the unfiltered expand reported dbhits=%d, want %d — one per slot of "+
			"the Root's adjacency run:\n%s", allHits, slots, all)
	}
	if filteredHits != slots {
		t.Errorf("the type-filtered expand reported dbhits=%d, want %d. It walks the "+
			"SAME %d-slot adjacency run as the unfiltered arm and rejects 99 slots on "+
			"the relationship type; a slot read and then rejected is still a read "+
			"(rmp #2761). A figure of 1 here is the pre-#2761 derived count, which "+
			"tracked emitted rows:\n%s", filteredHits, slots, slots, filtered)
	}
	if filteredHits != allHits {
		t.Errorf("the two arms reported dbhits=%d and dbhits=%d for the same %d-slot "+
			"CSR walk. They differ only in a predicate applied to slots both of them "+
			"read, so a db-hits figure that moves between them is not counting storage "+
			"reads:\nunfiltered:\n%s\nfiltered:\n%s",
			allHits, filteredHits, slots, all, filtered)
	}
}

// TestProfileDbHits_ColumnarExpandCountsSlotsWalked is gate 2's columnar arm.
//
// The columnar presentation of a traversal is a DIFFERENT operator in the plan —
// `columnarExpand`, an exec.columnarExpand embedding the same *exec.Expand — and it
// is driven through FillChunk, not Next, so it advances the same cursors from a
// different state machine ([Expand.advanceInputChunk] rather than
// [Expand.advanceInput]). A correction applied to the row path alone would leave
// the identical query reporting two different figures according to whether the
// planner chose the chunked chain, which is the very failure mode rmp #2720
// recorded for the parallel threshold.
//
// The chain is engaged by a post-traversal property filter over the far node
// (rmp #2106): scan → columnar Expand → ColumnarFilter → ColumnarProject. The
// operator name in the rendered plan is what proves it engaged; a fallback to row
// mode would print "Expand" and this test would not find its subject.
func TestProfileDbHits_ColumnarExpandCountsSlotsWalked(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	const others = 99
	const slots = others + 1
	runHonestyWrite(t, eng, "CREATE (:Root {k:0})")
	for i := 0; i < others; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("MATCH (r:Root) CREATE (r)-[:LIKES]->(:Leaf {v:%d})", i))
	}
	runHonestyWrite(t, eng, "MATCH (r:Root) CREATE (r)-[:KNOWS]->(:Leaf {v:999})")

	ctx := context.Background()
	all, err := eng.Profile(ctx, "MATCH (r:Root)-->(p) WHERE p.v >= 0 RETURN p.v", nil)
	if err != nil {
		t.Fatalf("Profile(unfiltered): %v", err)
	}
	filtered, err := eng.Profile(ctx, "MATCH (r:Root)-[:KNOWS]->(p) WHERE p.v >= 0 RETURN p.v", nil)
	if err != nil {
		t.Fatalf("Profile(filtered): %v", err)
	}

	allRows, allHits, allKnown, ok := profiledCells(all, "columnarExpand")
	if !ok {
		t.Fatalf("no columnarExpand in the unfiltered plan — the columnar chain did not "+
			"engage, so this test has no subject:\n%s", all)
	}
	filteredRows, filteredHits, filteredKnown, ok := profiledCells(filtered, "columnarExpand")
	if !ok {
		t.Fatalf("no columnarExpand in the filtered plan — the columnar chain did not "+
			"engage, so this test has no subject:\n%s", filtered)
	}
	if !allKnown || !filteredKnown {
		t.Fatalf("columnarExpand rendered its db-hits as unknown (unfiltered known=%v, "+
			"filtered known=%v); it embeds *exec.Expand and inherits its counter:\n"+
			"unfiltered:\n%s\nfiltered:\n%s", allKnown, filteredKnown, all, filtered)
	}
	if allRows != slots || filteredRows != 1 {
		t.Fatalf("the arms emitted %d and %d rows, want %d and 1 — the graph no longer "+
			"has the shape this gate needs:\nunfiltered:\n%s\nfiltered:\n%s",
			allRows, filteredRows, slots, all, filtered)
	}
	if allHits != slots || filteredHits != slots {
		t.Errorf("the columnar arms reported dbhits=%d and dbhits=%d, want %d each. The "+
			"FillChunk path walks the SAME adjacency run as the Next path and must report "+
			"the same slot count (rmp #2761):\nunfiltered:\n%s\nfiltered:\n%s",
			allHits, filteredHits, slots, all, filtered)
	}
}

// parallelHonestyNodes is the fixture size for gate 3. It sits above the subject
// arm's threshold and below the control arm's, which is the only difference
// between the two engines.
const parallelHonestyNodes = 2000

// parallelHonestyGroups is the number of distinct `v` values the fixture carries,
// so a GROUP BY over `v` yields far fewer rows than there are nodes. That gap is
// what makes gate 3's aggregate arm discriminating.
const parallelHonestyGroups = 100

// parallelHonestyEngine seeds a fresh engine holding parallelHonestyNodes :B
// nodes, each with a group property `v` in [0, parallelHonestyGroups) and a
// distinct `k`. threshold is the engine's ParallelScanThreshold: pass a low value
// for the parallel arm and one above the node count for the serial control.
func parallelHonestyEngine(t *testing.T, threshold int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{ParallelScanThreshold: threshold})
	t.Cleanup(func() { _ = eng.Close() })
	for i := 0; i < parallelHonestyNodes; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("CREATE (:B {v:%d, k:%d})", i%parallelHonestyGroups, i))
	}
	return eng
}

// TestProfileDbHits_ParallelLeavesCountTheirWorkersNodeWalk is gate 3.
//
// # What it used to assert, and why that inverted
//
// This gate was TestProfileDbHits_ParallelLeafDeclaresItsGap, and it asserted the
// OPPOSITE of what it asserts now: that a morsel-parallel leaf renders its db-hits
// cell as unknown ("?") because nothing counted its workers' node walk. Its own
// failure message said what would have to happen for it to be rewritten — "if a
// later task (#2762) taught the leaf to count, this gate is what forces its
// documentation and its markers to be updated together". rmp #2762 is that task,
// and this is that rewrite.
//
// # The shape of each arm
//
// One arm per leaf, and each arm is a PAIR of PROFILE runs over IDENTICAL graphs
// that differ only in ParallelScanThreshold — the setting a reader never chose and
// cannot see in the output. The subject arm plans the morsel-parallel leaf; the
// control arm plans the serial pipeline. The assertion is that the two report the
// SAME db-hits figure for the same work, which is precisely what
// docs/explain-profile-honesty-audit-2026-09-03.md §3 refutation 3 measured them
// failing to do:
//
//	ParallelScanThreshold = 20000 :  NodeByLabelScan     rows=2000  dbhits=2000
//	ParallelScanThreshold = 10    :  ParallelScanProject rows=2000  dbhits=0
//
// # Why each arm is discriminating rather than coincidental
//
// A gate whose expected db-hits equalled the expected rows would pass on a leaf
// marked [exec.StorageRecordScan], which derives db-hits FROM rows — the exact
// misreport rmp #2762 had to avoid. Every arm therefore drives a shape whose row
// count differs from its node walk:
//
//   - the fused scan carries a WHERE admitting 1 in parallelHonestyGroups nodes,
//     so rows=20 against dbhits=2000;
//   - the aggregate groups 2000 nodes into 100, so rows=100;
//   - the count emits one row for the whole graph, so rows=1.
//
// Each arm asserts the inequality explicitly, so an accidental fixture change that
// made rows equal the node count would fail here rather than quietly making the
// arm vacuous.
func TestProfileDbHits_ParallelLeavesCountTheirWorkersNodeWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// profile runs q at the given threshold and returns the rendered plan.
	profile := func(t *testing.T, threshold int, q string) string {
		t.Helper()
		plan, err := parallelHonestyEngine(t, threshold).Profile(ctx, q, nil)
		if err != nil {
			t.Fatalf("Profile(threshold=%d, %q): %v", threshold, q, err)
		}
		return plan
	}
	const (
		parallelThreshold = 10
		serialThreshold   = parallelHonestyNodes * 10
	)

	// cells locates one operator and fails the arm — rather than returning a zero —
	// when it is absent, because an absent operator means the arm planned something
	// else entirely and proved nothing.
	cells := func(t *testing.T, plan, operator, role string) (rows, hits int64) {
		t.Helper()
		r, d, known, ok := profiledCells(plan, operator)
		if !ok {
			t.Fatalf("the %s arm planned no %s, so this arm covers NOTHING — it did not "+
				"exercise the operator it names:\n%s", role, operator, plan)
		}
		if !known {
			t.Fatalf("the %s arm's %s rendered its db-hits as %q. Since rmp #2762 every "+
				"operator in this gate counts or derives its accesses, so an unknown cell "+
				"means the marker interface was dropped:\n%s",
				role, operator, exec.DbHitsUnknown, plan)
		}
		return r, d
	}

	t.Run("ParallelScanProject", func(t *testing.T) {
		t.Parallel()
		// `n.k + 1` rather than a bare `n.k`: the columnar filter chain claims a plain
		// property projection at every threshold, and the parallel tier would then never
		// be reached (the same reason recorded in
		// docs/benchmarks/min-label-anchor-vs-parallel-scan-2026-08-26.md).
		const q = "MATCH (n:B) WHERE n.v = 0 RETURN n.k + 1"
		wantRows := int64(parallelHonestyNodes / parallelHonestyGroups)

		serial := profile(t, serialThreshold, q)
		_, serialHits := cells(t, serial, "NodeByLabelScan", "serial control")
		if serialHits != parallelHonestyNodes {
			t.Fatalf("control: NodeByLabelScan reported dbhits=%d, want %d. The control is "+
				"the baseline the subject is compared against, so a wrong baseline makes the "+
				"comparison meaningless:\n%s", serialHits, parallelHonestyNodes, serial)
		}

		parallel := profile(t, parallelThreshold, q)
		parRows, parHits := cells(t, parallel, "ParallelScanProject", "parallel subject")
		if parRows != wantRows {
			t.Fatalf("the parallel leaf emitted %d rows, want %d. This arm is discriminating "+
				"only because the predicate admits far fewer rows than it scans:\n%s",
				parRows, wantRows, parallel)
		}
		if parHits != serialHits {
			t.Errorf("ParallelScanProject reported dbhits=%d where the serial plan for the "+
				"IDENTICAL query on an identical graph reported %d. The two arms differ only "+
				"in ParallelScanThreshold, a setting the reader did not choose and cannot see "+
				"in the output, so the figures must agree (rmp #2762, audit §3 refutation 3)."+
				"\nparallel:\n%s\nserial control:\n%s", parHits, serialHits, parallel, serial)
		}
		if parHits == parRows {
			t.Errorf("ParallelScanProject reported dbhits=%d equal to its %d emitted rows. "+
				"The fused sub-plan filtered %d nodes down to %d, so a figure that tracks "+
				"rows is the exact under-report a StorageRecordScan marker would have "+
				"produced:\n%s", parHits, parRows, parallelHonestyNodes, parRows, parallel)
		}
	})

	t.Run("ParallelAggregateScan", func(t *testing.T) {
		t.Parallel()
		const q = "MATCH (n) RETURN n.v AS g, count(*) AS c ORDER BY g"

		serial := profile(t, serialThreshold, q)
		_, serialHits := cells(t, serial, "AllNodesScan", "serial control")
		if serialHits != parallelHonestyNodes {
			t.Fatalf("control: AllNodesScan reported dbhits=%d, want %d:\n%s",
				serialHits, parallelHonestyNodes, serial)
		}

		parallel := profile(t, parallelThreshold, q)
		parRows, parHits := cells(t, parallel, "ParallelAggregateScan", "parallel subject")
		if parRows != parallelHonestyGroups {
			t.Fatalf("the aggregate leaf emitted %d rows, want %d groups:\n%s",
				parRows, parallelHonestyGroups, parallel)
		}
		if parHits != serialHits {
			t.Errorf("ParallelAggregateScan reported dbhits=%d where the serial pipeline's "+
				"AllNodesScan reported %d over the same graph. Both walk every node; only "+
				"the threshold differs (rmp #2762).\nparallel:\n%s\nserial control:\n%s",
				parHits, serialHits, parallel, serial)
		}
		if parHits == parRows {
			t.Errorf("ParallelAggregateScan reported dbhits=%d equal to its %d emitted rows. "+
				"Its rows are GROUPS: a figure tracking them would report the size of the "+
				"aggregate's OUTPUT for a walk of %d nodes:\n%s",
				parHits, parRows, parallelHonestyNodes, parallel)
		}
	})

	t.Run("ParallelCountScan", func(t *testing.T) {
		t.Parallel()
		const q = "MATCH (n) RETURN count(*)"

		// This leaf has no scanning serial twin, and the gate says so rather than
		// papering over it. Below the threshold the SAME query plans an
		// AllNodesCountScan, which answers from the maintained live-node counter in
		// O(1): it walks nothing, counts nothing, and honestly renders "?". Asserting
		// that here is what stops this explanation going stale — if the sub-threshold
		// plan ever becomes a real scan, this fails and the control below can be
		// replaced by the direct twin.
		countControl := profile(t, serialThreshold, q)
		if _, _, known, ok := profiledCells(countControl, "AllNodesCountScan"); !ok || known {
			t.Fatalf("the sub-threshold plan for %q is no longer an AllNodesCountScan with "+
				"an unknown db-hits cell (found=%v, known=%v). The comment above and the "+
				"substitute control below both depend on that being so:\n%s",
				q, ok, known, countControl)
		}

		// The substitute control: the same whole-graph node walk, done serially, by the
		// query whose sub-threshold plan IS a scan.
		serial := profile(t, serialThreshold, "MATCH (n) RETURN n.k")
		_, serialHits := cells(t, serial, "AllNodesScan", "serial control")
		if serialHits != parallelHonestyNodes {
			t.Fatalf("control: AllNodesScan reported dbhits=%d, want %d:\n%s",
				serialHits, parallelHonestyNodes, serial)
		}

		parallel := profile(t, parallelThreshold, q)
		parRows, parHits := cells(t, parallel, "ParallelCountScan", "parallel subject")
		if parRows != 1 {
			t.Fatalf("the count leaf emitted %d rows, want exactly 1:\n%s", parRows, parallel)
		}
		if parHits != serialHits {
			t.Errorf("ParallelCountScan reported dbhits=%d where a serial walk of the same "+
				"%d-node graph reported %d. The count leaf reads every node reference its "+
				"workers consume, so the two must agree (rmp #2762).\nparallel:\n%s\n"+
				"serial control:\n%s", parHits, parallelHonestyNodes, serialHits, parallel, serial)
		}
		if parHits == parRows {
			t.Errorf("ParallelCountScan reported dbhits=%d equal to its single emitted row. "+
				"This is the extreme case of a derived figure being unrelated to the work: "+
				"rows=1 for a walk of %d nodes:\n%s", parHits, parallelHonestyNodes, parallel)
		}
	})
}

// TestProfileDbHits_ParallelLeafPlanDetailNoLongerClaimsItIsUncounted is the
// documentation half of gate 3.
//
// The rendered plan carries two statements about a parallel leaf: the db-hits
// number, and the PlanDetail beside the operator name. Until rmp #2762 that detail
// read "parallel tier; db-hits not counted", which was true. It is not any more,
// and a plan line that contradicts its own number is worse than one that says
// nothing — so this asserts the old words are GONE and that what replaced them
// still names the tier.
//
// Asserting the absence alone would pass on a leaf that rendered no detail at all,
// which would lose the one thing the detail is for: telling a reader that an
// operator's whole fused sub-plan — filter, projection, every worker and every
// morsel — collapsed into this single line, with no children below it to subtract.
func TestProfileDbHits_ParallelLeafPlanDetailNoLongerClaimsItIsUncounted(t *testing.T) {
	t.Parallel()

	plan, err := parallelHonestyEngine(t, 10).Profile(context.Background(), "MATCH (n) RETURN n.k", nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if _, _, _, ok := profiledCells(plan, "ParallelScanProject"); !ok {
		t.Fatalf("no ParallelScanProject in the plan, so this gate covers nothing:\n%s", plan)
	}
	if strings.Contains(plan, "db-hits not counted") {
		t.Errorf("the parallel leaf's plan line still says its db-hits are not counted, "+
			"beside a db-hits number it now measures (rmp #2762). A detail that "+
			"contradicts the cell next to it is a worse defect than the missing figure "+
			"it used to describe:\n%s", plan)
	}
	if !strings.Contains(plan, "parallel tier") {
		t.Errorf("the parallel leaf renders no \"parallel tier\" detail at all. The "+
			"detail is what tells a reader the whole fused phase — filter, projection, "+
			"every worker, every morsel — is attributed to this one line with no children "+
			"to subtract; removing it rather than correcting it loses that:\n%s", plan)
	}
}
