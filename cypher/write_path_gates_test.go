package cypher

// write_path_gates_test.go — regression gate for rmp #2225, parts A and B.
//
// # The defect
//
// [buildPlanWithMutatorFull] — the build path taken by every statement that
// contains a write clause — constructed its [buildOpts] with only
// maxCollectItems and edgeTypeFilterCache set. Every optimisation gate the read
// path threads in at api.go:1948-1997 therefore defaulted to false, so a MATCH
// inside a writing statement was planned by the unoptimised planner while the
// identical MATCH with a RETURN was not.
//
// Measured for part A on `MATCH (a:P:Rare) SET a.t = true` with :Rare at 1/1000
// of :P: 2.609 ms → 18 µs at 20 000 nodes (145×) and 10.568 ms → 86 µs at 80 000
// (123×), with the read control unchanged. The write was scanning the whole :P
// population where the read re-anchored on :Rare.
//
// Part A deliberately left the hash join off, so the bulk-load idiom
// `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)` still ran the
// nested-loop Cartesian product: 226.8 ms at N=2000 growing to 1.860 s at
// N=16000, linear in the node population, against 4.9 ms → 9.1 ms for the
// identical MATCH with a scalar RETURN, which the hash join served. Part B closes
// that: 2.9 ms → 10.9 ms, i.e. 171× at N=16000 and within 1.2× of the read.
//
// # What this file asserts
//
//   - Part A: the min-label re-anchor engages for a WRITE statement, not merely
//     for a read.
//   - Part B: the hash join engages for a WRITE statement, AND the substitution
//     preserves the emitted row order — proved through the last-write-wins `SET`
//     case, whose final stored value IS a readout of emission order, plus full
//     final-state and side-effect-counter identity against the nested loop.
//
// Both read the planner's own counters ([minLabelScanBuildCount],
// [hashJoinBuildCount]) rather than Engine.Explain, because Explain renders the
// logical IR and cannot be trusted to report a physical substitution (round-4
// audit finding P3).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// writeGateFixture builds a :P population of n nodes of which every 1000th also
// carries :Rare, plus an index on sid, so both the min-label re-anchor and the
// index seek have something to prefer.
func writeGateFixture(t *testing.T, n int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if i%1000 == 0 {
			if err := g.SetNodeLabel(key, "Rare"); err != nil {
				t.Fatalf("SetNodeLabel(Rare): %v", err)
			}
		}
		if err := g.SetNodeProperty(key, "sid", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	eng := NewEngine(g)
	// drain() rather than a bare RunAny: an abandoned Result is collected by a
	// later forced GC and counted by TestResult_Close_DisarmsFinalizer, which
	// samples the process-global leak counter across its own GC. Discarding the
	// Result here fails that test from a different file.
	drain(t, eng, `CREATE INDEX p_sid FOR (x:P) ON (x.sid)`)
	return eng
}

// drain runs stmt and discards its rows, failing on any error.
func drain(t *testing.T, eng *Engine, stmt string) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), stmt, nil)
	if err != nil {
		t.Fatalf("run %q: %v", stmt, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", stmt, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", stmt, err)
	}
}

// TestWritePathGates_MinLabelReAnchorsInsideAWrite is the #2225 part A gate: the
// re-anchor must fire for a writing statement, exactly as it does for a read.
// Before part A the write cases returned false.
func TestWritePathGates_MinLabelReAnchorsInsideAWrite(t *testing.T) {
	cases := []struct {
		name string
		stmt string
	}{
		{"read (control, fired before part A too)", `MATCH (a:P:Rare) RETURN count(a)`},
		{"write SET", `MATCH (a:P:Rare) SET a.t = true`},
		{"write CREATE relationship", `MATCH (a:P:Rare) CREATE (a)-[:K]->(:Z)`},
		{"write REMOVE", `MATCH (a:P:Rare) REMOVE a.sid`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := writeGateFixture(t, 5000)
			before := minLabelScanBuildCount.Load()
			drain(t, eng, tc.stmt)
			if minLabelScanBuildCount.Load() == before {
				t.Errorf("min-label re-anchor did NOT fire for %q; the write path is planning "+
					"with the optimisation gates off again (#2225 part A)", tc.stmt)
			}
		})
	}
}

// TestWritePathGates_ResultIdentity proves part A changed only the access path,
// never the outcome: the same statement run with the gates on and off must leave
// the graph in the same state and report the same side-effect counts.
func TestWritePathGates_ResultIdentity(t *testing.T) {
	const stmt = `MATCH (a:P:Rare) SET a.t = true`

	count := func(eng *Engine, q string) int64 {
		t.Helper()
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		var n int64
		for res.Next() {
			iv, ok := res.ValueAt(0).(expr.IntegerValue)
			if !ok {
				t.Fatalf("count %q: expected IntegerValue, got %T", q, res.ValueAt(0))
			}
			n = int64(iv)
		}
		if err := res.Err(); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		_ = res.Close()
		return n
	}

	// Two identical fixtures, one planned with the re-anchor and one with it
	// disabled engine-wide (the supported way to get the pre-#2077 plan), so the
	// comparison isolates the access path and nothing else.
	on := writeGateFixture(t, 5000)
	offEng := writeGateFixture(t, 5000)
	offEng = NewEngineWithOptions(offEng.g, EngineOptions{DisableMinLabelScan: true})

	before := minLabelScanBuildCount.Load()
	drain(t, on, stmt)
	if minLabelScanBuildCount.Load() == before {
		t.Fatal("the gates-on arm did not re-anchor, so this test is not comparing the two plans")
	}
	before = minLabelScanBuildCount.Load()
	drain(t, offEng, stmt)
	if minLabelScanBuildCount.Load() != before {
		t.Fatal("the gates-off arm re-anchored; DisableMinLabelScan is not taking effect")
	}

	gotOn := count(on, `MATCH (a:P) WHERE a.t = true RETURN count(a)`)
	gotOff := count(offEng, `MATCH (a:P) WHERE a.t = true RETURN count(a)`)
	if gotOn != gotOff {
		t.Fatalf("touched-node count differs: gates-on=%d gates-off=%d", gotOn, gotOff)
	}
	// :Rare is every 1000th node of 5000 → exactly 5.
	if gotOn != 5 {
		t.Fatalf("expected the statement to touch exactly the 5 :Rare nodes, got %d", gotOn)
	}
}

// hjGateNodes is the :P population for the part-B cases. The hash join's size
// floor is [hashJoinSizeFloor] build rows, so the fixture only has to clear that
// comfortably — these cases assert plan choice and result identity, never a
// timing, and every one of them builds two fixtures (join on and off). The
// performance evidence lives in bench/r4audit, at sizes where it means something.
const hjGateNodes = 400

// TestWritePathGates_HashJoinFiresInsideAWrite is the #2225 part B gate. It
// replaces TestWritePathGates_HashJoinStaysOff, which pinned the pre-part-B
// verdict and instructed its successor to assert ORDER PRESERVATION rather than
// mere firing — so this file carries both: the trigger here, the order proof in
// [TestWritePathGates_HashJoinPreservesOrder].
func TestWritePathGates_HashJoinFiresInsideAWrite(t *testing.T) {
	eng := writeGateFixture(t, hjGateNodes)
	rows := make([]any, 0, 64)
	for i := 0; i < 64; i++ {
		rows = append(rows, map[string]any{"ss": fmt.Sprintf("s%d", i)})
	}
	runWith := func(stmt string) bool {
		before := hashJoinBuildCount.Load()
		res, err := eng.RunAny(context.Background(), stmt, map[string]any{"rows": rows})
		if err != nil {
			t.Fatalf("run %q: %v", stmt, err)
		}
		for res.Next() {
		}
		_ = res.Close()
		return hashJoinBuildCount.Load() > before
	}

	if !runWith(`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN a.sid`) {
		t.Fatal("read control: the hash join did not fire, so this test can no longer " +
			"distinguish the write case; fix the control before trusting the write verdict")
	}
	for _, stmt := range []string{
		`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) SET a.t = true`,
		`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`,
		`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) REMOVE a.t`,
	} {
		if !runWith(stmt) {
			t.Errorf("the hash join did NOT fire for %q; the write path is planning the "+
				"nested-loop Cartesian product again (#2225 part B)", stmt)
		}
	}
}

// TestWritePathGates_HashJoinPreservesOrder is the order proof #2225 part B owes.
//
// The concern part A recorded, and the round-4 audit repeated, is that a hash
// join "may build on either arm and therefore reorder rows", which would make it
// unsafe for `SET` (last-write-wins). That premise does not hold in this
// codebase: the planner PINS build=inner / probe=outer at the single construction
// site, so the substitution is order-PRESERVING, not merely order-insensitive.
// See [hashJoinBuildOnLeft].
//
// This test proves it where it matters most — the last-write-wins case. Two
// UNWIND rows target the SAME node with DIFFERENT values; whichever row is
// applied last determines the stored value, so the final graph state is a direct readout
// of the emission order. It must match the nested loop exactly.
func TestWritePathGates_HashJoinPreservesOrder(t *testing.T) {
	// Every row targets sid "s0", so all 64 rows collide on one node and the
	// stored value at the end is whichever row the plan emitted last.
	rows := make([]any, 0, 64)
	for i := 0; i < 64; i++ {
		rows = append(rows, map[string]any{"ss": "s0", "v": int64(i)})
	}
	const stmt = `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) SET a.v = r.v`

	run := func(disableHashJoin bool) (int64, bool, *exec.QueryCounters) {
		eng := writeGateFixture(t, hjGateNodes)
		if disableHashJoin {
			eng = NewEngineWithOptions(eng.g, EngineOptions{DisableHashJoin: true})
		}
		before := hashJoinBuildCount.Load()
		res, err := eng.RunAny(context.Background(), stmt, map[string]any{"rows": rows})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("run: %v", err)
		}
		counters := res.Counters()
		_ = res.Close()
		fired := hashJoinBuildCount.Load() > before

		rd, err := eng.RunAny(context.Background(), `MATCH (a:P {sid: 's0'}) RETURN a.v`, nil)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		var got int64 = -1
		for rd.Next() {
			iv, ok := rd.ValueAt(0).(expr.IntegerValue)
			if !ok {
				t.Fatalf("read back: expected IntegerValue, got %T", rd.ValueAt(0))
			}
			got = int64(iv)
		}
		_ = rd.Close()
		return got, fired, counters
	}

	onVal, onFired, onCounters := run(false)
	offVal, offFired, offCounters := run(true)

	if !onFired {
		t.Fatal("the hash join did not fire on the ON arm, so this test is not comparing two plans")
	}
	if offFired {
		t.Fatal("the hash join fired on the OFF arm; DisableHashJoin is not taking effect")
	}
	if onVal != offVal {
		t.Fatalf("LAST-WRITE-WINS DIVERGED: hash join stored a.v=%d, nested loop stored a.v=%d. "+
			"The substitution reordered rows — see hashJoinBuildOnLeft for the invariant "+
			"that is supposed to prevent this", onVal, offVal)
	}
	// The last UNWIND row carries v = 63; a plan that preserves order stores it.
	if onVal != 63 {
		t.Fatalf("expected the LAST row's value (63) to win, got %d — both plans agree but "+
			"neither is emitting in UNWIND order", onVal)
	}
	// AC 5: the side-effect counters (#2212) must be identical with and without
	// the join. A reordering or a dropped/duplicated row would show up here even
	// where the final value happens to coincide.
	if *onCounters != *offCounters {
		t.Fatalf("side-effect counters differ: hash join %+v, nested loop %+v", *onCounters, *offCounters)
	}
}

// TestWritePathGates_HashJoinWriteResultIdentity proves the substitution changes
// nothing observable for each writing clause: the full final graph state and the
// side-effect counters must match the nested loop exactly.
//
// It also covers the case the order test cannot: a write whose CREATE feeds the
// BUILD arm's own label. The hash join drains the build side once, whereas the
// nested loop re-initialises the inner arm per outer row — so if a label scan
// observed rows created mid-statement the two plans would diverge. They do not
// (both snapshot the label bitmap at Init), and this pins that.
func TestWritePathGates_HashJoinWriteResultIdentity(t *testing.T) {
	rows := make([]any, 0, 48)
	for i := 0; i < 48; i++ {
		// Deliberately overlapping keys: several rows hit the same node.
		rows = append(rows, map[string]any{"ss": fmt.Sprintf("s%d", i%16), "v": int64(i)})
	}

	cases := []struct {
		name  string
		stmt  string
		probe string
	}{
		{
			"SET with colliding keys",
			`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) SET a.v = r.v`,
			`MATCH (a:P) WHERE a.v IS NOT NULL RETURN a.sid, a.v ORDER BY a.sid`,
		},
		{
			"CREATE relationship",
			`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`,
			`MATCH (a:P)-[:K]->(z:Z) RETURN a.sid, count(z) ORDER BY a.sid`,
		},
		{
			"REMOVE property",
			`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) REMOVE a.sid`,
			`MATCH (a:P) WHERE a.sid IS NULL RETURN count(a)`,
		},
		{
			"CREATE feeding the build arm's own label",
			`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (:P {sid: 'spawned'})`,
			`MATCH (a:P) RETURN count(a)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(disableHashJoin bool) ([]string, bool, exec.QueryCounters) {
				eng := writeGateFixture(t, hjGateNodes)
				if disableHashJoin {
					eng = NewEngineWithOptions(eng.g, EngineOptions{DisableHashJoin: true})
				}
				before := hashJoinBuildCount.Load()
				res, err := eng.RunAny(context.Background(), tc.stmt, map[string]any{"rows": rows})
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					t.Fatalf("run: %v", err)
				}
				var counters exec.QueryCounters
				if c := res.Counters(); c != nil {
					counters = *c
				}
				_ = res.Close()
				fired := hashJoinBuildCount.Load() > before
				return readRows(t, eng, tc.probe), fired, counters
			}

			onRows, onFired, onCounters := run(false)
			offRows, offFired, offCounters := run(true)

			if !onFired {
				t.Fatal("the hash join did not fire on the ON arm; this case compares one plan with itself")
			}
			if offFired {
				t.Fatal("the hash join fired on the OFF arm; DisableHashJoin is not taking effect")
			}
			if len(onRows) != len(offRows) {
				t.Fatalf("row count differs: hash join %d, nested loop %d", len(onRows), len(offRows))
			}
			for i := range onRows {
				if onRows[i] != offRows[i] {
					t.Fatalf("final state differs at row %d: hash join %q, nested loop %q", i, onRows[i], offRows[i])
				}
			}
			if onCounters != offCounters {
				t.Fatalf("side-effect counters differ: hash join %+v, nested loop %+v", onCounters, offCounters)
			}
		})
	}
}

// readRows runs q and renders every row as a single comparable string.
func readRows(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("probe %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		var b strings.Builder
		for i := range res.Columns() {
			fmt.Fprintf(&b, "%v|", res.ValueAt(i))
		}
		out = append(out, b.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("probe %q: %v", q, err)
	}
	_ = res.Close()
	return out
}
