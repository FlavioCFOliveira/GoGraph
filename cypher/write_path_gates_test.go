package cypher

// write_path_gates_test.go — regression gate for rmp #2225 part A.
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
// Measured on `MATCH (a:P:Rare) SET a.t = true` with :Rare at 1/1000 of :P:
// 2.609 ms → 18 µs at 20 000 nodes (145×) and 10.568 ms → 86 µs at 80 000 (123×),
// with the read control unchanged. The write was scanning the whole :P
// population where the read re-anchored on :Rare.
//
// # What this file asserts
//
// That the min-label re-anchor engages for a WRITE statement, not merely for a
// read. It reads the planner's own [minLabelScanBuildCount] rather than
// Engine.Explain, because Explain renders the logical IR and cannot be trusted
// to report a physical substitution (round-4 audit finding P3).
//
// # What it deliberately does NOT assert
//
// The hash join. It is gated on bopts too and remains OFF for write statements,
// because it may build on either arm and therefore reorder rows, and `SET` is
// last-write-wins — two rows targeting the same node with different values are
// order-dependent. Admitting it requires pinning the probe side to the
// order-defining arm first; that is part B of #2225. [TestWritePathGates_HashJoinStaysOff]
// pins the current verdict so part B's arrival is visible as a change to this
// test rather than as silent drift — the same discipline #2182 used for #2183.

import (
	"context"
	"fmt"
	"testing"

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
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_sid FOR (x:P) ON (x.sid)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
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

// TestWritePathGates_HashJoinStaysOff pins the CURRENT verdict for the hash join
// inside a write statement: it does not fire, because admitting it needs the
// probe side pinned to the order-defining arm first (#2225 part B). The read
// control proves the shape is otherwise eligible, so this test fails the moment
// part B lands — which is the point.
func TestWritePathGates_HashJoinStaysOff(t *testing.T) {
	eng := writeGateFixture(t, 3000)
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
	if runWith(`UNWIND $rows AS r MATCH (a:P {sid: r.ss}) SET a.t = true`) {
		t.Fatal("the hash join now fires inside a write statement. If that is #2225 part B " +
			"landing, this test must be replaced by one asserting ORDER PRESERVATION — a hash " +
			"join may build on either arm, and SET is last-write-wins, so two rows targeting " +
			"the same node with different values are order-dependent")
	}
}
