package exec_test

// all_nodes_count_scan_test.go — tests for AllNodesCountScan (#2113 / #2066).
//
// The operator emits exactly one row carrying the live-node count. It reads the
// O(1) direct counter when the walker implements liveNodeCounter, and otherwise
// falls back to a single WalkNodeIDs count pass; both must yield the identical
// value. It spawns no goroutines.

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// countingWalker is a staticNodeWalker that also exposes the O(1) direct live
// count via LiveNodeCount, exercising the fast path.
type countingWalker struct {
	ids []graph.NodeID
}

func (w *countingWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

func (w *countingWalker) LiveNodeCount() (int64, bool) { return int64(len(w.ids)), true }

func drainCountScan(t *testing.T, op exec.Operator) int64 {
	t.Helper()
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("got %d rows (want 1) of width %d (want 1)", len(rows), lenOr0(rows))
	}
	iv, ok := rows[0][0].(expr.IntegerValue)
	if !ok {
		t.Fatalf("row[0][0] is %T, want IntegerValue", rows[0][0])
	}
	return int64(iv)
}

func lenOr0(rows []exec.Row) int {
	if len(rows) == 0 {
		return 0
	}
	return len(rows[0])
}

// TestAllNodesCountScan_DirectAndFallback proves both the O(1) direct-counter path
// (countingWalker implements liveNodeCounter) and the WalkNodeIDs fallback
// (staticNodeWalker does not) yield the exact live count, with no goroutine leak.
func TestAllNodesCountScan_DirectAndFallback(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, n := range []int{0, 1, 50, 50_000, 123_456} {
		// Direct O(1) path.
		if got := drainCountScan(t, exec.NewAllNodesCountScan(&countingWalker{ids: makeIDs(n)})); got != int64(n) {
			t.Errorf("direct count = %d, want %d", got, n)
		}
		// Fallback walk-and-count path (staticNodeWalker has no LiveNodeCount).
		if got := drainCountScan(t, exec.NewAllNodesCountScan(buildWalker(n))); got != int64(n) {
			t.Errorf("fallback count = %d, want %d", got, n)
		}
	}
}

// TestAllNodesCountScan_SingleRow proves the operator emits exactly one row and
// then reports end-of-stream, and that Close is a safe no-op.
func TestAllNodesCountScan_SingleRow(t *testing.T) {
	op := exec.NewAllNodesCountScan(&countingWalker{ids: makeIDs(7)})
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil || !ok {
		t.Fatalf("first Next = (%v, %v), want (true, nil)", ok, err)
	}
	if iv, _ := row[0].(expr.IntegerValue); int64(iv) != 7 {
		t.Fatalf("count = %v, want 7", row[0])
	}
	ok, err = op.Next(&row)
	if ok || err != nil {
		t.Fatalf("second Next = (%v, %v), want (false, nil)", ok, err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func makeIDs(n int) []graph.NodeID {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	return ids
}

// decliningWalker implements liveNodeCounter but REFUSES to answer, which is the
// shape the production walker actually takes: lpgNodeWalker.LiveNodeCount returns
// (0, false) for a morsel-restricted walker and whenever
// lpg.ReadView.LiveNodeCountExact cannot vouch for the count at the reader's
// snapshot. It is distinct from staticNodeWalker, which does not implement the
// interface at all, because the two reach the fallback by different branches.
type decliningWalker struct {
	ids   []graph.NodeID
	asked int
}

func (w *decliningWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

func (w *decliningWalker) LiveNodeCount() (int64, bool) { w.asked++; return 0, false }

// profileCountScan drains op through a Profiler and returns the captured node, so
// the db-hits figure is read from the same rendering surface PROFILE reads it
// from rather than from an unexported method this package cannot reach.
func profileCountScan(t *testing.T, op exec.Operator) exec.PlanNode {
	t.Helper()
	wrapped := exec.NewProfiler().Wrap(op)
	if _, err := exec.Drain(context.Background(), wrapped); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	tree := exec.PlanTree(wrapped)
	if tree.Name != "AllNodesCountScan" {
		t.Fatalf("the captured node is %q, want AllNodesCountScan — this gate is not "+
			"measuring the operator it names", tree.Name)
	}
	if !tree.Profiled {
		t.Fatal("the captured node carries no measurements")
	}
	return tree
}

// TestAllNodesCountScan_DbHitsZeroOnTheCounterAndCountedOnTheWalk is rmp #2777's
// acceptance gate, in both directions.
//
// The operator answers the same question two ways, and the db-hits column has to
// say which one happened: the O(1) counter read is a genuine ZERO, and the
// fallback walk reads one node reference per live node. A single type-level
// [noStorageAccess] marker could state only the first, which is why the operator
// implements storageAccessCounter instead — and why a test that checked only the
// zero would have licensed exactly the under-report rmp #2760 removed.
//
// The fallback figure is checked against an INDEPENDENT oracle rather than
// against itself: AllNodesScan over the same walker is DERIVED (one node
// reference per emitted row) and must report the same number for the same walk.
func TestAllNodesCountScan_DbHitsZeroOnTheCounterAndCountedOnTheWalk(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 50, 4096, 12_345} {
		ids := makeIDs(n)

		// ── The fast path: a real, measured zero ──────────────────────────────
		fast := profileCountScan(t, exec.NewAllNodesCountScan(&countingWalker{ids: ids}))
		if !fast.DbHitsKnown {
			t.Fatalf("n=%d: the O(1) path rendered db-hits UNKNOWN. The zero is the whole "+
				"point of the counter: an operator that read one maintained counter and no "+
				"records has a figure, and it is 0", n)
		}
		if fast.DbHits != 0 {
			t.Errorf("n=%d: the O(1) path reported dbhits=%d, want 0 — it reads one "+
				"maintained counter and walks nothing", n, fast.DbHits)
		}
		// Non-vacuity: the zero must come from an operator that actually answered.
		if fast.Rows != 1 {
			t.Fatalf("n=%d: the O(1) path emitted %d rows, want 1; a zero measured over an "+
				"operator that produced nothing proves nothing", n, fast.Rows)
		}

		// ── The fallback, reached by a walker that has no counter ─────────────
		absent := profileCountScan(t, exec.NewAllNodesCountScan(buildWalker(n)))
		// ── The fallback, reached by a counter that DECLINES — production's shape
		decliner := &decliningWalker{ids: ids}
		declined := profileCountScan(t, exec.NewAllNodesCountScan(decliner))
		if decliner.asked != 1 {
			t.Fatalf("n=%d: the declining walker was asked %d times, want 1 — the operator "+
				"is no longer consulting the counter, so this arm reaches the fallback for "+
				"the wrong reason", n, decliner.asked)
		}

		for _, arm := range []struct {
			name string
			node exec.PlanNode
		}{
			{"no liveNodeCounter at all", absent},
			{"liveNodeCounter that declines", declined},
		} {
			if !arm.node.DbHitsKnown {
				t.Fatalf("n=%d, %s: the fallback rendered db-hits UNKNOWN; the walk counts "+
					"its own node ids", n, arm.name)
			}
			if arm.node.DbHits != int64(n) {
				t.Errorf("n=%d, %s: the fallback reported dbhits=%d, want %d — one node "+
					"reference per node id WalkNodeIDs yielded", n, arm.name, arm.node.DbHits, n)
			}
		}

		// ── The independent oracle: the DERIVED scan over the same walk ───────
		scan := exec.NewProfiler().Wrap(exec.NewAllNodesScan(buildWalker(n)))
		if _, err := exec.Drain(context.Background(), scan); err != nil {
			t.Fatalf("n=%d: oracle drain: %v", n, err)
		}
		oracle := exec.PlanTree(scan)
		if !oracle.DbHitsKnown || oracle.DbHits != absent.DbHits {
			t.Errorf("n=%d: the count leaf's fallback reported dbhits=%d for the same whole-graph "+
				"walk an AllNodesScan reports %d (known=%v) for. Both read one node reference "+
				"per live node and may not disagree", n, absent.DbHits, oracle.DbHits, oracle.DbHitsKnown)
		}

		// ── The two directions must actually DIFFER ───────────────────────────
		if fast.DbHits == absent.DbHits {
			t.Errorf("n=%d: the O(1) path and the fallback both reported dbhits=%d, so this "+
				"test no longer distinguishes them and a flat figure would pass it",
				n, fast.DbHits)
		}
	}
}

// TestAllNodesCountScan_DbHitsAccumulateAcrossInit pins storageAccessCounter's
// documented contract: the figure covers the operator's whole lifetime, including
// every Init a re-driving parent restarts it with. Assigning instead of
// accumulating would report one walk where two happened, and no single-execution
// test could see the difference.
func TestAllNodesCountScan_DbHitsAccumulateAcrossInit(t *testing.T) {
	t.Parallel()
	const n = 500

	op := exec.NewAllNodesCountScan(buildWalker(n))
	wrapped := exec.NewProfiler().Wrap(op)
	for i := 1; i <= 3; i++ {
		if _, err := exec.Drain(context.Background(), wrapped); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
		got := exec.PlanTree(wrapped)
		if !got.DbHitsKnown {
			t.Fatalf("drain %d: db-hits rendered UNKNOWN", i)
		}
		if want := int64(i * n); got.DbHits != want {
			t.Fatalf("after %d walks of %d nodes the leaf reported dbhits=%d, want %d",
				i, n, got.DbHits, want)
		}
	}

	// And the fast path adds nothing, so a mixed lifetime keeps the walks it made
	// and gains nothing from the reads it did not.
	mixed := exec.NewProfiler().Wrap(exec.NewAllNodesCountScan(&countingWalker{ids: makeIDs(n)}))
	for i := 0; i < 3; i++ {
		if _, err := exec.Drain(context.Background(), mixed); err != nil {
			t.Fatalf("counter drain %d: %v", i, err)
		}
	}
	if got := exec.PlanTree(mixed); !got.DbHitsKnown || got.DbHits != 0 {
		t.Errorf("three O(1) counter reads reported dbhits=%d (known=%v), want a known 0",
			got.DbHits, got.DbHitsKnown)
	}
}
