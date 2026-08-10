package cypher

// parallel_scan_gate_bound_test.go — the cheap screen must work while MVCC
// history is LIVE (rmp #2392).
//
// The screen introduced by #2380 declines the parallel path without materialising
// a bitmap, but it demanded an EXACT label count — and the exact count is
// unavailable whenever the label bitmap would need snapshot filtering, which is
// whenever any history is live. A single concurrent writer therefore made it
// abstain on every query and fall through to cloning the bitmap, only to decline
// on it anyway. #2392 replaced the exact count with a sound UPPER BOUND, which is
// all a "can this exceed the threshold" decision needs.
//
// The sibling file's tests deliberately call waitQuiescent first, so they exercise
// only the quiescent case. These two exercise the case that actually occurs in an
// OLTP workload, and the first of them FAILS against the pre-#2392 build.

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// boundGateNodes is deliberately far smaller than the sibling file's gateNodes:
// under a pinned horizon every seeded node leaves history that cannot be reclaimed
// until the test ends, and nothing in these tests depends on the population size,
// only on its relationship to the threshold.
const boundGateNodes = 256

// slotPinTS is the start timestamp the pinning reader registers. 0 is the oldest
// possible instant, so it holds reclamation back for everything the seed writes.
const slotPinTS = 0

// seedGateGraphWithLiveHistory returns a graph carrying LIVE label history at
// QUERY time, which is the whole difficulty: simply not calling waitQuiescent is
// not enough, because the vacuum reclaims within milliseconds and the seed of a
// few thousand nodes takes longer than that. A first version of this helper did
// exactly that and passed against the pre-#2392 build — it asserted history was
// live straight after seeding, by which time the query had not run yet.
//
// So it PINS the reader horizon: registering a reader at the lowest possible start
// timestamp holds reclamation back for as long as the slot is occupied, which is
// what a long-running reader does in production. The pin is released by a cleanup.
//
// It fails rather than skips when no history is live, because a test that silently
// stopped exercising the non-quiescent path would pass for the wrong reason.
//
// # Why the population is deliberately small
//
// A pinned horizon means nothing the seed writes can be reclaimed for as long as
// the test runs, so the graph accumulates one label delta and one life record per
// node. The first version pinned boundGateNodes at the sibling file's 4096 and
// released the pin without waiting, which left a reclamation backlog of thousands
// of records per test to drain while LATER tests ran — and under `make ci` that
// starved two other tests' own waitQuiescent into a 60-second timeout, one of them
// pre-existing. Both symptoms looked exactly like host load and were not.
//
// The remedy is a SMALL population, not a drain on the way out. Waiting for the
// graph to quiesce after releasing the pin was tried and does not work: the
// reclaimer has to be woken, and nothing wakes it once the test has stopped
// writing, so each cleanup simply burned its whole deadline — three of them turned
// a sub-second package into 180 seconds. Since nothing here depends on the
// population size, only on its relationship to the threshold, keeping it small
// keeps the residue small, which is what the later tests actually care about.
func seedGateGraphWithLiveHistory(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})

	// Pin BEFORE writing, so nothing the seed produces can be reclaimed.
	slot := g.Horizon().Enter(slotPinTS)
	t.Cleanup(func() { g.Horizon().Leave(slot) })

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
	if g.LabelDeltaCount() == 0 {
		t.Fatalf("no LABEL history is live after seeding under a pinned horizon, so this test no longer "+
			"exercises the non-quiescent screen it exists for: labelDeltas=%d propDeltas=%d. "+
			"labelBitmapNeedsFilter consults the label and node-life counters, so property history alone "+
			"would not make the exact count unavailable",
			g.LabelDeltaCount(), g.PropDeltaCount())
	}
	return g
}

// TestParallelScanGate_DeclinesCheaplyWhileHistoryIsLive is the #2392 gate. With
// the threshold far above the population the parallel path cannot be chosen, so
// the screen must decline CHEAPLY — without ever materialising the label bitmap —
// even though live history makes the exact count unavailable.
//
// Against the pre-#2392 build this fails: the screen abstained on the missing
// exact count and the decline came from the materialised cardinality instead, so
// the cheap-decline counter never moved.
func TestParallelScanGate_DeclinesCheaplyWhileHistoryIsLive(t *testing.T) {
	g := seedGateGraphWithLiveHistory(t, boundGateNodes)
	eng := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: boundGateNodes * 8})

	before := parallelScanCheapDeclineCount.Load()
	rows := collectV(t, eng, gateQuery)
	after := parallelScanCheapDeclineCount.Load()

	if len(rows) != boundGateNodes {
		t.Fatalf("query returned %d rows, want %d — the plan is not the one under test", len(rows), boundGateNodes)
	}
	if after == before {
		t.Errorf("the cheap screen did not decline while label history was live (labelDeltas=%d): "+
			"it is still demanding an exact count, so every query in a mixed read/write workload "+
			"materialises the label bitmap only to decline on it",
			g.LabelDeltaCount())
	}
}

// TestParallelScanGate_BoundNeverDeclinesAboveTheThreshold pins the direction that
// matters for correctness of the DECISION. The bound is deliberately loose — it
// adds every live suspect, including ones that would remove a member or concern
// another label entirely — and loose upwards is the safe direction: it can only
// make the gate keep the parallel path under consideration, never wrongly rule it
// out. With the threshold set below the population the screen must NOT decline.
func TestParallelScanGate_BoundNeverDeclinesAboveTheThreshold(t *testing.T) {
	g := seedGateGraphWithLiveHistory(t, boundGateNodes)
	// Threshold well below the population, so the parallel path is admissible and
	// a cheap decline would be flatly wrong.
	eng := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: boundGateNodes / 8})

	before := parallelScanCheapDeclineCount.Load()
	rows := collectV(t, eng, gateQuery)
	after := parallelScanCheapDeclineCount.Load()

	if after != before {
		t.Errorf("the cheap screen declined a label of %d nodes against a threshold of %d: the bound is "+
			"reading BELOW the true cardinality, which would rule out the parallel path for a label that "+
			"qualifies for it", boundGateNodes, boundGateNodes/8)
	}
	if len(rows) != boundGateNodes {
		t.Fatalf("query returned %d rows, want %d", len(rows), boundGateNodes)
	}
}

// TestLabelCountBound_IsAnUpperBound checks the bound against the authoritative
// filtered cardinality directly, rather than only through the planner. It is the
// absolute oracle for the soundness claim in lpg.Graph.LabelCountBound: whatever
// the live history, the bound may not fall below the number of rows a scan of that
// label actually yields.
func TestLabelCountBound_IsAnUpperBound(t *testing.T) {
	g := seedGateGraphWithLiveHistory(t, boundGateNodes)

	eng := NewEngineWithOptions(g, EngineOptions{})
	// The row count a real scan yields is the authoritative filtered cardinality.
	rows := collectV(t, eng, gateQuery)

	res := &lpgLabelResolver{g: g.ReadAt(nil), eng: eng}
	bound, _ := res.ResolveLabelCountBound("P")
	if bound < int64(len(rows)) {
		t.Errorf("LabelCountBound reported %d for label P while a scan of it yields %d rows: the bound is "+
			"not an upper bound, so a threshold screen built on it could rule out a qualifying label",
			bound, len(rows))
	}

	// And an unknown label must answer zero rather than declining, matching the
	// empty bitmap ResolveLabelBitmap returns for one.
	if bound, exact := res.ResolveLabelCountBound("NoSuchLabel"); bound != 0 || !exact {
		t.Errorf("an unknown label bounded to (%d, %v), want (0, true)", bound, exact)
	}
}

// TestLabelCountBound_IsExactWithNoHistory is the control for the other half of the
// contract: with no history at all the bound must be the exact count, so the loose
// path is genuinely reserved for the case that needs it and is not silently taken
// everywhere.
//
// It uses a graph that was NEVER WRITTEN rather than one waited into quiescence,
// deliberately. Waiting on the vacuum is what made the sibling file's tests fragile
// enough to turn `make ci` red twice; a graph with no history needs no vacuum to
// have run, so the assertion is deterministic under any load.
func TestLabelCountBound_IsExactWithNoHistory(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := NewEngineWithOptions(g, EngineOptions{})

	if g.LabelDeltaCount() != 0 {
		t.Fatalf("a graph that was never written already carries %d label deltas", g.LabelDeltaCount())
	}
	res := &lpgLabelResolver{g: g.ReadAt(nil), eng: eng}
	if n, exact := res.ResolveLabelCountBound("P"); !exact || n != 0 {
		t.Errorf("with no history the bound for an unwritten label is (%d, %v), want the exact (0, true): "+
			"the loose path is being taken even when nothing can differ", n, exact)
	}

	// And the cheap decline must still fire on that graph, so the exact branch is
	// wired to the screen and not merely reachable.
	before := parallelScanCheapDeclineCount.Load()
	if _, err := eng.Run(context.Background(), gateQuery, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if parallelScanCheapDeclineCount.Load() == before {
		t.Errorf("the cheap decline did not fire on a history-free graph")
	}
}
