package cypher

// parallel_scan_gate_cost_test.go — regression gate for rmp #2380.
//
// # The defect
//
// [tryBuildParallelScanProject] decides whether a single-label leaf is worth
// running in parallel by comparing the label's cardinality against
// bopts.parallelScanThreshold (DefaultParallelScanThreshold = 50 000, strict >).
// It obtained that cardinality from [newLabelWalker], which resolves the label's
// bitmap — and resolving it CLONES the label's live set.
//
// So every graph below the threshold paid a full bitmap clone for a verdict that
// was fixed at "no" before the question was asked, then threw the clone away.
// The multi-label sibling had already been fixed for exactly this reason;
// [exactIntersectionCardinality] states the rule — "a gate must cost less than
// the decision it informs" — and records +85.8% B/op on a query the gate then
// declined.
//
// # What it cost, measured
//
// Found by profiling examples/35_mvcc_mixed_workload (3000 nodes, so the gate can
// never admit) with the -profile-dir flag rmp #2377 added to every example.
// roaring's arrayContainer.clone was 47.9% of ALL allocation in the process,
// reached 100% through this gate. Fixing it, over three interleaved rounds:
//
//	total allocation   12.75-13.01 GB  ->  9.06-9.15 GB   (-29%)
//	roaring clone       6.18-6.36 GB   ->  777-789 MB     (-87.5%)
//	baseline throughput   465 629 ops/s -> 657 296 ops/s  (+41%)
//
// with every deterministic fact the example pins unchanged in both arms.
//
// # Why these assertions and not an allocation pin alone
//
// An allocation pin cannot tell "the screen worked" from "the shape never
// reached the gate" — and the microbenchmark written alongside this test showed
// exactly that trap: its `seek` arm measured byte-identical allocations in both
// arms because that shape never reaches the screen at all. The engagement
// counter is what makes a green result mean something, and the paired
// above-threshold case is what stops the gate passing vacuously if parallel scan
// were ever disabled outright.
//
// # Why these tests no longer wait for the graph to quiesce
//
// They used to call a waitQuiescent helper first, because the screen demanded an
// EXACT label count and that is unavailable while any MVCC history is live. The
// helper was a standing liability: it waited on a BACKGROUND reclaim goroutine, so
// under `make ci` — every package's binary at once, under -race — it timed out and
// turned the gate red twice, once at a 5-second deadline and once at 60 seconds
// having drained only 3343 of 4096 records. rmp #2392 removed the reason it
// existed: the screen now reads a sound UPPER BOUND, which live history inflates
// upwards but never invalidates, so these tests hold with history in flight and the
// wait is gone rather than lengthened.

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// gateNodes is comfortably below the threshold the test sets, so the gate's
// verdict is fixed at "decline" and the only question is what it costs to reach.
const gateNodes = 4096

// seedGateGraph builds n :P nodes with an int64 v property.
func seedGateGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range n {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// collectV runs q and returns the v column, so two plans can be compared row for
// row rather than merely by count.
func collectV(t *testing.T, eng *Engine, q string) []int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []int64
	for res.Next() {
		rec := res.Record()
		v, ok := rec["v"]
		if !ok {
			t.Fatalf("row without column v: %v", rec)
		}
		n, ok := v.(expr.IntegerValue)
		if !ok {
			t.Fatalf("column v is %T, want expr.IntegerValue", v)
		}
		out = append(out, int64(n))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate %q: %v", q, err)
	}
	return out
}

const gateQuery = "MATCH (n:P) RETURN n.v AS v"

// TestParallelScanGate_DeclinesWithoutMaterialising is the primary gate: below
// the threshold the screen must fire, which is what proves no bitmap was cloned
// to answer a question whose answer was fixed.
func TestParallelScanGate_DeclinesWithoutMaterialising(t *testing.T) {
	g := seedGateGraph(t, gateNodes)
	// Threshold well above the population: the gate can only decline. The margin is
	// generous because the screen now reads an UPPER BOUND that adds every live
	// suspect, so a graph with history still in flight bounds higher than its
	// population (rmp #2392).
	eng := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: gateNodes * 8})

	before := parallelScanCheapDeclineCount.Load()
	rows := collectV(t, eng, gateQuery)
	after := parallelScanCheapDeclineCount.Load()

	if len(rows) != gateNodes {
		t.Fatalf("got %d rows, want %d", len(rows), gateNodes)
	}
	if after == before {
		t.Fatal("the cheap cardinality screen never fired: the gate still resolves " +
			"the label bitmap to decide it does not want it (rmp #2380/#2392)")
	}
}

// TestParallelScanGate_StillAdmitsAboveThreshold is the paired positive case.
// Without it the gate above could be satisfied by disabling parallel scan
// altogether, which would "fix" the allocation by removing the feature.
func TestParallelScanGate_StillAdmitsAboveThreshold(t *testing.T) {
	g := seedGateGraph(t, gateNodes)
	// Threshold below the population: the screen must NOT decline, and the
	// materialised path must take over.
	eng := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: gateNodes / 4})

	before := parallelScanCheapDeclineCount.Load()
	rows := collectV(t, eng, gateQuery)
	after := parallelScanCheapDeclineCount.Load()

	if len(rows) != gateNodes {
		t.Fatalf("got %d rows, want %d", len(rows), gateNodes)
	}
	if after != before {
		t.Errorf("the cheap screen declined a label of %d nodes against a threshold "+
			"of %d: it must abstain when the gate should admit, or parallel scan is "+
			"silently disabled", gateNodes, gateNodes/4)
	}
}

// TestParallelScanGate_ResultsAreIdenticalEitherSide pins the property that makes
// the screen safe at all: it selects an execution strategy, never a result.
func TestParallelScanGate_ResultsAreIdenticalEitherSide(t *testing.T) {
	g := seedGateGraph(t, gateNodes)

	below := collectV(t,
		NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: gateNodes * 4}), gateQuery)
	above := collectV(t,
		NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: gateNodes / 4}), gateQuery)

	if len(below) != len(above) {
		t.Fatalf("serial returned %d rows, parallel %d", len(below), len(above))
	}
	// The parallel plan does not promise order, so compare as multisets.
	seen := make(map[int64]int, len(below))
	for _, v := range below {
		seen[v]++
	}
	for _, v := range above {
		seen[v]--
	}
	for v, n := range seen {
		if n != 0 {
			t.Fatalf("row v=%d differs between the serial and parallel plans (delta %d)", v, n)
		}
	}
}
