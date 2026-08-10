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

import (
	"context"
	"strconv"
	"testing"
	"time"

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

// waitQuiescent blocks until the graph carries no live label or property history,
// or the deadline passes.
//
// It is required, not hygiene. The cheap screen reads the label cardinality
// through [lpgLabelResolver.ResolveLabelCount], which reports exact=false
// whenever the bitmap would need snapshot filtering — and seeding leaves MVCC
// deltas live until they are reclaimed. Without this wait the screen legitimately
// abstains on whichever runs still have history, and the assertion below failed 7
// times in 100. With it: 0 in 300.
//
// The abstention is correct behaviour, so the wait belongs in the test rather
// than a relaxation belonging in the gate. Note that probing exactness with a NIL
// snapshot reports exact=true unconditionally and hides this entirely — the
// engine's resolver pins a real snapshot.
// The deadline is generous on purpose. Reclamation is done by a BACKGROUND
// goroutine, so what is being waited on is the scheduler, not a correctness
// property — and under `make ci` this test competes with every other package's
// binary at once under -race. A 5-second deadline was not enough there: it failed
// once with labelDeltas stalled at 577 of the 4096 seeded, i.e. mid-drain rather
// than never started, while the same test passes 20 consecutive runs in isolation
// in well under a second. Slow progress for a background reclaim on a saturated
// host is the graceful degradation the module promises, not a defect, so the fix
// is a deadline that tolerates it rather than a gate that flakes.
func waitQuiescent(t *testing.T, g *lpg.Graph[string, float64]) {
	t.Helper()
	const quiesceDeadline = 60 * time.Second
	deadline := time.Now().Add(quiesceDeadline)
	for g.LabelDeltaCount() != 0 || g.PropDeltaCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("graph never quiesced within %s: labelDeltas=%d propDeltas=%d — "+
				"if these are far below the seeded population the vacuum is progressing and merely starved, "+
				"which points at host load rather than at the code under test",
				quiesceDeadline, g.LabelDeltaCount(), g.PropDeltaCount())
		}
		time.Sleep(time.Millisecond)
	}
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
	waitQuiescent(t, g)
	// Threshold well above the population: the gate can only decline.
	eng := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: gateNodes * 4})

	before := parallelScanCheapDeclineCount.Load()
	rows := collectV(t, eng, gateQuery)
	after := parallelScanCheapDeclineCount.Load()

	if len(rows) != gateNodes {
		t.Fatalf("got %d rows, want %d", len(rows), gateNodes)
	}
	if after == before {
		t.Fatal("the cheap cardinality screen never fired: the gate still resolves " +
			"the label bitmap to decide it does not want it (rmp #2380)")
	}
}

// TestParallelScanGate_StillAdmitsAboveThreshold is the paired positive case.
// Without it the gate above could be satisfied by disabling parallel scan
// altogether, which would "fix" the allocation by removing the feature.
func TestParallelScanGate_StillAdmitsAboveThreshold(t *testing.T) {
	g := seedGateGraph(t, gateNodes)
	waitQuiescent(t, g)
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
	waitQuiescent(t, g)

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
