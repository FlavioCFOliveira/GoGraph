package cypher

// label_count_pushdown_gate_test.go — regression gate for rmp #2654: the
// constant-time labelled count pushdown must not be gated on graph size or on
// the morsel-parallel feature flag.
//
// # The defect these tests pin
//
// tryBuildLabelCountScan used to end with `!useParallelScan(walker, bopts)`, so
// the O(1) maintained label-index read was only reachable when
//
//   - the morsel-parallel path was ENABLED (EngineOptions.DisableParallelScan
//     false), and
//   - the WHOLE GRAPH's live node count strictly exceeded
//     ParallelScanThreshold (DefaultParallelScanThreshold = 50 000).
//
// Neither condition has anything to do with whether the read is correct —
// exactness is enforced one layer down by lpg.Graph.LabelCountExact, which is
// exact-or-nothing, with exec.LabelCountScan falling back to the cardinality of
// LabelBitmapAsOf under this query's pinned snapshot, the same snapshot-aware
// source exec.NodeByLabelScan reads. Nor has either anything to do with whether
// the read is worth doing: LabelCountScan spawns no worker at all.
//
// Every test below FAILS on the pre-#2654 build. That was verified, not assumed:
// the four tests in bench/audit352/labelcount_gate_ab_test.go assert the OLD
// behaviour (serial plan below the threshold, pushdown for a 100-node label in a
// 60 000-node graph) and all four pass at the defective HEAD and fail after the
// fix — so the two files are exact inverses of one another over the same shapes.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The two physical plans a bare labelled count can compile to. Pinning the exact
// text means an unexpected third plan fails loudly instead of being accepted by a
// substring match. Kept in step with the same constants in
// bench/audit352/labelcount_gate_ab_test.go.
const (
	lcPlanPushdown = "Project\n└─ LabelCountScan"
	lcPlanSerial   = "Project\n" +
		"└─ GlobalAggregateAdapter\n" +
		"   └─ EagerAggregation\n" +
		"      └─ ColumnarProject\n" +
		"         └─ NodeByLabelScan [Item]"
)

// buildLeanLabelGraph creates total nodes carrying `label`, with no properties
// and no edges — the shape a bare labelled count actually reads. It is the cheap
// fixture for the size-boundary tests, which need tens of thousands of nodes and
// nothing else about them.
func buildLeanLabelGraph(t *testing.T, total int, label string) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range total {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, label); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestLabelCount_EngagesBelowAndAtThreshold is acceptance criterion 1 of rmp
// #2654: a bare labelled count engages the pushdown at every size, and returns
// the value the serial control returns.
//
// 50 000 is not an arbitrary third size: DefaultParallelScanThreshold is 50 000
// and the deleted gate was STRICT (>), so a graph of exactly 50 000 live nodes is
// the largest graph the old build refused — the boundary case, and the one where
// the measured cost of refusing was greatest (929x time, 220x bytes, 1 718x
// allocations).
func TestLabelCount_EngagesBelowAndAtThreshold(t *testing.T) {
	if DefaultParallelScanThreshold != 50_000 {
		t.Fatalf("DefaultParallelScanThreshold = %d, want 50000: this test's 50 000-node "+
			"case is the strict-> boundary of the DELETED gate and stops being the "+
			"boundary if the constant moves", DefaultParallelScanThreshold)
	}
	for _, n := range []int{1_000, 10_000, 50_000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			g := buildLeanLabelGraph(t, n, "Item")
			on, off := labelCountEngines(g)
			want := fmt.Sprintf("c=%d", n)

			for _, q := range []string{
				`MATCH (p:Item) RETURN count(*) AS c`,
				`MATCH (p:Item) RETURN count(p) AS c`,
			} {
				before := labelCountScanBuildCount.Load()
				gotOn := drainSortedPS(t, on, q)
				if labelCountScanBuildCount.Load() == before {
					t.Fatalf("%q over %d nodes did NOT engage the label count pushdown: a "+
						"size gate is back on a constant-time index read", q, n)
				}
				beforeCtl := labelCountScanBuildCount.Load()
				gotOff := drainSortedPS(t, off, q)
				if labelCountScanBuildCount.Load() != beforeCtl {
					t.Fatalf("the serial control arm engaged the pushdown for %q", q)
				}
				assertEqualRows(t, q, gotOn, gotOff)
				if len(gotOn) != 1 || gotOn[0] != want {
					t.Fatalf("%s = %v, want [%s]", q, gotOn, want)
				}
			}

			// And the plan says so, independently of the build counter.
			plan, err := on.Explain(`MATCH (p:Item) RETURN count(p) AS c`, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if got := strings.TrimSpace(plan); got != lcPlanPushdown {
				t.Fatalf("plan at n=%d is\n%s\nwant\n%s", n, got, lcPlanPushdown)
			}
			ctlPlan, err := off.Explain(`MATCH (p:Item) RETURN count(p) AS c`, nil)
			if err != nil {
				t.Fatalf("Explain (control): %v", err)
			}
			if got := strings.TrimSpace(ctlPlan); got != lcPlanSerial {
				t.Fatalf("control plan at n=%d is\n%s\nwant\n%s", n, got, lcPlanSerial)
			}
		})
	}
}

// TestLabelCount_DisableParallelScanKeepsConstantTimeCount is acceptance
// criterion 2 of rmp #2654: EngineOptions.DisableParallelScan no longer forfeits
// the O(1) labelled count.
//
// DisableParallelScan is documented as the escape hatch for the MORSEL-PARALLEL
// path. LabelCountScan starts no worker, so a caller switching that path off had
// no reason to expect a labelled count to go from O(1) to O(n) — and the measured
// cost of it doing so was 1 117x-1 872x above the threshold, where the flag was
// the only thing standing between the query and the constant-time read.
func TestLabelCount_DisableParallelScanKeepsConstantTimeCount(t *testing.T) {
	g := buildLabelCountGraph(t, 200)
	// The one variable against the control below is the pushdown seam; the flag
	// is set on BOTH arms so it cannot be what explains the difference.
	on := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})
	off := withoutLabelCountPushdown(NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true}))

	for _, q := range []string{
		`MATCH (p:Item) RETURN count(*) AS c`,
		`MATCH (p:Item) RETURN count(p) AS c`,
	} {
		before := labelCountScanBuildCount.Load()
		gotOn := drainSortedPS(t, on, q)
		if labelCountScanBuildCount.Load() == before {
			t.Fatalf("%q with DisableParallelScan:true did NOT engage the label count "+
				"pushdown: the morsel-parallel escape hatch is still wired to a serial "+
				"constant-time read", q)
		}
		gotOff := drainSortedPS(t, off, q)
		assertEqualRows(t, q, gotOn, gotOff)
		if len(gotOn) != 1 || gotOn[0] != "c=200" {
			t.Fatalf("%s = %v, want [c=200]", q, gotOn)
		}
	}
	plan, err := on.Explain(`MATCH (p:Item) RETURN count(p) AS c`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := strings.TrimSpace(plan); got != lcPlanPushdown {
		t.Fatalf("plan with DisableParallelScan:true is\n%s\nwant\n%s", got, lcPlanPushdown)
	}
}

// TestLabelCount_LabelCardinalityNotGraphOrder is acceptance criterion 3 of rmp
// #2654: the count a labelled scan would emit no longer depends on nodes that do
// not carry the label.
//
// The deleted gate read lpg.Graph.LiveOrder — the WHOLE GRAPH's live count — so
// the decision was taken on a number the query never consults. Measured on the
// defective build, that inverted the ordering it was supposed to impose:
//
//	100 :Rare nodes inside a 60 000-node graph    → pushdown (graph order wins)
//	50 000 :Item nodes in a 50 000-node graph     → full serial scan
//
// The 500x SMALLER count was the one answered in constant time. Both must now
// plan LabelCountScan, and the assertion on the ratio keeps the test's purpose
// legible: it is about which cardinality is read, not merely about two plans.
func TestLabelCount_LabelCardinalityNotGraphOrder(t *testing.T) {
	// Tiny label inside a large graph: what the old gate ADMITTED.
	big := lpg.New[string, float64](adjlist.Config{Directed: true})
	const bigOrder, rareN = 60_000, 100
	for i := range bigOrder {
		k := fmt.Sprintf("b%d", i)
		if err := big.AddNode(k); err != nil {
			t.Fatal(err)
		}
		label := "Common"
		if i < rareN {
			label = "Rare"
		}
		if err := big.SetNodeLabel(k, label); err != nil {
			t.Fatal(err)
		}
	}
	bigOn := NewEngineWithOptions(big, EngineOptions{})

	// Whole graph labelled, exactly at the threshold: what the old gate REFUSED.
	const denseN = 50_000
	dense := buildLeanLabelGraph(t, denseN, "Item")
	denseOn, denseOff := labelCountEngines(dense)

	rarePlan, err := bigOn.Explain(`MATCH (n:Rare) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("Explain rare: %v", err)
	}
	if got := strings.TrimSpace(rarePlan); got != lcPlanPushdown {
		t.Errorf("count over %d :Rare nodes in a %d-node graph plans\n%s\nwant\n%s",
			rareN, bigOrder, got, lcPlanPushdown)
	}

	densePlan, err := denseOn.Explain(`MATCH (p:Item) RETURN count(p) AS c`, nil)
	if err != nil {
		t.Fatalf("Explain dense: %v", err)
	}
	if got := strings.TrimSpace(densePlan); got != lcPlanPushdown {
		t.Errorf("count over %d :Item nodes in a %d-node graph plans\n%s\nwant\n%s: "+
			"the %dx LARGER count is still the one denied the constant-time read",
			denseN, denseN, got, lcPlanPushdown, denseN/rareN)
	}

	// Both answers are correct, and the dense one is the larger count — so the
	// inversion the old gate produced is what this test is about, not a
	// coincidence of two plans that happen to match.
	rareGot := drainSortedPS(t, bigOn, `MATCH (n:Rare) RETURN count(n) AS c`)
	if len(rareGot) != 1 || rareGot[0] != fmt.Sprintf("c=%d", rareN) {
		t.Fatalf("rare count = %v, want [c=%d]", rareGot, rareN)
	}
	denseGot := drainSortedPS(t, denseOn, `MATCH (p:Item) RETURN count(p) AS c`)
	denseCtl := drainSortedPS(t, denseOff, `MATCH (p:Item) RETURN count(p) AS c`)
	assertEqualRows(t, "dense labelled count", denseGot, denseCtl)
	if len(denseGot) != 1 || denseGot[0] != fmt.Sprintf("c=%d", denseN) {
		t.Fatalf("dense count = %v, want [c=%d]", denseGot, denseN)
	}
	if denseN/rareN != 500 {
		t.Fatalf("fixture drift: the dense/rare ratio is %d, and the measured finding "+
			"this test encodes is a 500x inversion", denseN/rareN)
	}
}

// countItemsInTx runs the bare labelled count inside tx and returns the scalar it
// produces, together with whether the pushdown built the plan.
func countItemsInTx(t *testing.T, tx *ExplicitTx) (n int64, pushedDown bool) {
	t.Helper()
	before := labelCountScanBuildCount.Load()
	res, err := tx.Exec(`MATCH (p:Item) RETURN count(p) AS c`, nil)
	if err != nil {
		t.Fatalf("Exec count: %v", err)
	}
	pushedDown = labelCountScanBuildCount.Load() > before
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("close count result: %v", cerr)
		}
	}()
	if !res.Next() {
		t.Fatalf("count returned no row (err=%v)", res.Err())
	}
	v, ok := res.Record()["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("count returned %T, want expr.IntegerValue", res.Record()["c"])
	}
	if err := res.Err(); err != nil {
		t.Fatalf("count drain: %v", err)
	}
	return int64(v), pushedDown
}

// autocommitLC runs one statement outside any explicit transaction and drains it.
// It is the "someone else commits" half of the isolation assertions below, and
// follows the idiom of cypher/readtx_snapshot_test.go's autocommit.
func autocommitLC(t *testing.T, e *Engine, query string) {
	t.Helper()
	res, err := e.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("autocommit %q: %v", query, err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("autocommit %q: %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("autocommit %q close: %v", query, err)
	}
}

// TestLabelCount_ReadTxPinsPushedDownCount is acceptance criteria 4 and 5 of rmp
// #2654 together: inside Engine.BeginReadTx the pushed-down labelled count keeps
// answering the PINNED number while other transactions commit outside it, and it
// does so through the bitmap fallback because the exact path declines.
//
// The two halves are inseparable, and the oracle is the same fact in both
// directions. Once a commit lands after the read transaction's instant, MVCC
// history is live, so lpg.Graph.LabelCountExact DECLINES for the pinned snapshot
// — asserted directly below — and exec.LabelCountScan must fall back to
// ResolveLabelBitmap, i.e. to LabelBitmapAsOf under that same snapshot. The raw
// index cardinality is a DIFFERENT number by then (203 after the inserts, and the
// deferred-removal count after the deletes), so a pushdown that read the raw
// count instead of the fallback fails this test with a wrong answer rather than
// merely a different plan.
//
// The count is also compared against a serial control transaction opened at the
// same point, so the assertion is differential and not merely absolute.
//
// NOTE ON PROVENANCE: the task text cites a 2000-vs-2100 discrepancy from
// docs/benchmarks/count-star-row-count-2026-08-27.md. That evidence concerns
// count.Store, the derived EDGE count store, which takes no snapshot at all. This
// test deliberately exercises a different store — the snapshot-aware node label
// index — and must not be read as covering the other one.
func TestLabelCount_ReadTxPinsPushedDownCount(t *testing.T) {
	itemLID := func(g *lpg.Graph[string, float64]) lpg.LabelID {
		t.Helper()
		lid, ok := g.Registry().Lookup("Item")
		if !ok {
			t.Fatal(`label "Item" is not interned; the fixture never labelled anything`)
		}
		return lid
	}

	t.Run("outside-insert", func(t *testing.T) {
		g := buildLabelCountGraph(t, 200)
		on := NewEngineWithOptions(g, EngineOptions{})
		off := withoutLabelCountPushdown(NewEngineWithOptions(g, EngineOptions{}))

		onTx, err := on.BeginReadTx(context.Background())
		if err != nil {
			t.Fatalf("BeginReadTx: %v", err)
		}
		defer func() { _ = onTx.Rollback() }()
		offTx, err := off.BeginReadTx(context.Background())
		if err != nil {
			t.Fatalf("BeginReadTx (control): %v", err)
		}
		defer func() { _ = offTx.Rollback() }()

		got, pushed := countItemsInTx(t, onTx)
		if !pushed {
			t.Fatal("the labelled count inside a read transaction did NOT engage the pushdown")
		}
		if got != 200 {
			t.Fatalf("first statement saw %d :Item, want 200", got)
		}
		if ctl, ctlPushed := countItemsInTx(t, offTx); ctlPushed || ctl != 200 {
			t.Fatalf("control: got %d (pushed=%v), want 200 with no pushdown", ctl, ctlPushed)
		}

		// Three whole transactions commit, and acknowledge their commits, while
		// both read transactions are open.
		for i := range 3 {
			autocommitLC(t, on, fmt.Sprintf("CREATE (:Item {v: %d})", 1000+i))
		}

		// The exact path must now decline for the pinned snapshot — this is what
		// makes the assertion below a test of the FALLBACK and not of the exact
		// read that happened to be available at the first statement.
		if _, exact := g.LabelCountExact(itemLID(g), onTx.view.snap); exact {
			t.Fatal("lpg.Graph.LabelCountExact still reports an EXACT count for the pinned " +
				"snapshot after three outside commits: the premise of this test is gone and " +
				"the bitmap fallback is no longer the path under test")
		}
		// And the raw index now holds a different number, so reading it instead of
		// the snapshot-filtered bitmap is observable rather than benign.
		if raw, exact := g.LabelCountExact(itemLID(g), nil); exact && raw == 200 {
			t.Fatal("the raw label index still says 200 after three commits: the outside " +
				"writes did not land, so this test cannot distinguish the pinned answer " +
				"from the current one")
		}

		got, pushed = countItemsInTx(t, onTx)
		if !pushed {
			t.Fatal("the second statement did not engage the pushdown")
		}
		if got != 200 {
			t.Fatalf("second statement saw %d :Item, want 200: the pushed-down count "+
				"observed commits that landed after the read transaction began, which is "+
				"read-committed, not snapshot isolation", got)
		}
		if ctl, _ := countItemsInTx(t, offTx); ctl != got {
			t.Fatalf("pushdown says %d, serial control says %d", got, ctl)
		}

		if err := onTx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		after, err := on.BeginReadTx(context.Background())
		if err != nil {
			t.Fatalf("BeginReadTx after: %v", err)
		}
		defer func() { _ = after.Rollback() }()
		if got, _ := countItemsInTx(t, after); got != 203 {
			t.Fatalf("a read transaction opened AFTER the commits saw %d :Item, want 203: "+
				"the pushed-down count is stuck rather than per-transaction", got)
		}
	})

	t.Run("outside-delete", func(t *testing.T) {
		g := buildLabelCountGraph(t, 200)
		on := NewEngineWithOptions(g, EngineOptions{})
		off := withoutLabelCountPushdown(NewEngineWithOptions(g, EngineOptions{}))

		onTx, err := on.BeginReadTx(context.Background())
		if err != nil {
			t.Fatalf("BeginReadTx: %v", err)
		}
		defer func() { _ = onTx.Rollback() }()
		offTx, err := off.BeginReadTx(context.Background())
		if err != nil {
			t.Fatalf("BeginReadTx (control): %v", err)
		}
		defer func() { _ = offTx.Rollback() }()

		if got, pushed := countItemsInTx(t, onTx); !pushed || got != 200 {
			t.Fatalf("first statement: got %d (pushed=%v), want 200 with the pushdown", got, pushed)
		}

		// 50 of the 200 :Item nodes are deleted outside the open read transactions.
		// A delete is the direction the raw index gets WRONG in the other sense:
		// the removal is deferred while a reader can still reach the node, so the
		// index over-reports for the current value and under-reports nothing.
		autocommitLC(t, on, `MATCH (p:Item) WHERE p.v < 50 DETACH DELETE p`)

		if _, exact := g.LabelCountExact(itemLID(g), onTx.view.snap); exact {
			t.Fatal("LabelCountExact still reports an EXACT count for the pinned snapshot " +
				"after 50 outside deletes: the bitmap fallback is not the path under test")
		}

		got, pushed := countItemsInTx(t, onTx)
		if !pushed {
			t.Fatal("the second statement did not engage the pushdown")
		}
		if got != 200 {
			t.Fatalf("second statement saw %d :Item, want 200: the pushed-down count "+
				"observed a delete that committed after the read transaction began", got)
		}
		if ctl, _ := countItemsInTx(t, offTx); ctl != got {
			t.Fatalf("pushdown says %d, serial control says %d", got, ctl)
		}

		if err := onTx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		after, err := on.BeginReadTx(context.Background())
		if err != nil {
			t.Fatalf("BeginReadTx after: %v", err)
		}
		defer func() { _ = after.Rollback() }()
		if got, _ := countItemsInTx(t, after); got != 150 {
			t.Fatalf("a read transaction opened AFTER the deletes saw %d :Item, want 150", got)
		}
	})
}
