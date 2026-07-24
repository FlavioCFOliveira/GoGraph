package cypher

// all_nodes_count_scan_diff_test.go — differential + correctness tests for the
// serial full-node count pushdown (#2113 / #2066): a group-by-less count(*) /
// count(<scan-var>) over a bare full-node scan reads the maintained live-node
// count in O(1) instead of scanning every node. Bit-identical because WalkNodeIDs
// skips tombstones, so a bare scan emits exactly LiveOrder() rows.
//
// The tests assert the pushdown ENGAGES below the parallel threshold (and when the
// parallel path is disabled), that it does NOT engage above the threshold with the
// parallel path enabled (the ParallelCountScan owns that), and that the count is
// bit-identical to a full serial scan even after deletions leave tombstones.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countViaScan computes the live-node count by materialising every node row (the
// pre-#2113 full O(N) scan), so the O(1) pushdown can be proven bit-identical to it.
func countViaScan(t *testing.T, e *Engine) int64 {
	t.Helper()
	res, err := e.Run(context.Background(), `MATCH (n) RETURN n`, nil)
	if err != nil {
		t.Fatalf("scan-count Run: %v", err)
	}
	var n int64
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("scan-count Err: %v", err)
	}
	_ = res.Close()
	return n
}

// TestAllNodesCountPushdown_BelowThreshold proves the O(1) pushdown engages on a
// small graph (below the parallel threshold) for count(*) and count(n), and that
// the result equals the full-scan count.
func TestAllNodesCountPushdown_BelowThreshold(t *testing.T) {
	g := buildPSTestGraph(t, 40) // 40 < psTestThreshold (50): parallel declines
	e := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})
	want := countViaScan(t, e)

	for _, q := range []string{
		`MATCH (n) RETURN count(*) AS c`,
		`MATCH (n) RETURN count(n) AS c`,
	} {
		before := allNodesCountScanBuildCount.Load()
		got := drainSortedPS(t, e, q)
		if allNodesCountScanBuildCount.Load() == before {
			t.Errorf("O(1) count pushdown did not engage for %q below threshold", q)
		}
		if len(got) != 1 || got[0] != fmt.Sprintf("c=%d", want) {
			t.Errorf("count %q = %v, want [c=%d]", q, got, want)
		}
	}
}

// TestAllNodesCountPushdown_ParallelDisabled proves the O(1) pushdown engages even
// above the threshold when the parallel path is disabled (it is the serial count
// pushdown), and never engages the parallel reduce.
func TestAllNodesCountPushdown_ParallelDisabled(t *testing.T) {
	g := buildPSTestGraph(t, 200) // above threshold
	e := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})

	before := allNodesCountScanBuildCount.Load()
	pbefore := parallelCountScanBuildCount.Load()
	got := drainSortedPS(t, e, `MATCH (n) RETURN count(*) AS c`)
	if allNodesCountScanBuildCount.Load() == before {
		t.Error("O(1) count pushdown did not engage with the parallel path disabled")
	}
	if parallelCountScanBuildCount.Load() != pbefore {
		t.Error("parallel count reduce engaged on a parallel-disabled engine")
	}
	if len(got) != 1 || got[0] != "c=200" {
		t.Errorf("count = %v, want [c=200]", got)
	}
}

// TestAllNodesCountPushdown_AboveThresholdDefersToParallel proves that above the
// threshold with the parallel path enabled, the parallel reduce (not the O(1)
// serial pushdown) serves the bare count — preserving the shipped behaviour.
func TestAllNodesCountPushdown_AboveThresholdDefersToParallel(t *testing.T) {
	g := buildPSTestGraph(t, 200)
	e := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: psTestThreshold})

	abefore := allNodesCountScanBuildCount.Load()
	pbefore := parallelCountScanBuildCount.Load()
	got := drainSortedPS(t, e, `MATCH (n) RETURN count(*) AS c`)
	if parallelCountScanBuildCount.Load() == pbefore {
		t.Error("parallel count reduce did not engage above threshold")
	}
	if allNodesCountScanBuildCount.Load() != abefore {
		t.Error("serial O(1) pushdown engaged above threshold (should defer to the parallel reduce)")
	}
	if len(got) != 1 || got[0] != "c=200" {
		t.Errorf("count = %v, want [c=200]", got)
	}
}

// TestAllNodesCountPushdown_Tombstones proves the O(1) count is bit-identical to a
// full scan after deletions: WalkNodeIDs skips tombstones, so LiveOrder() equals
// the number of rows a bare scan emits.
func TestAllNodesCountPushdown_Tombstones(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	const total = 80
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	// Tombstone every third node directly via the graph mutator, so WalkNodeIDs
	// skips them and LiveOrder() drops accordingly.
	deleted := 0
	for i := 0; i < total; i += 3 {
		g.RemoveNode(fmt.Sprintf("n%d", i))
		deleted++
	}
	e := NewEngineWithOptions(g, EngineOptions{DisableParallelScan: true})
	want := countViaScan(t, e)
	if want != int64(total-deleted) {
		t.Fatalf("full-scan live count = %d, want %d live after %d deletions", want, total-deleted, deleted)
	}

	before := allNodesCountScanBuildCount.Load()
	got := drainSortedPS(t, e, `MATCH (n) RETURN count(*) AS c`)
	if allNodesCountScanBuildCount.Load() == before {
		t.Error("O(1) count pushdown did not engage over a tombstoned graph")
	}
	if len(got) != 1 || got[0] != fmt.Sprintf("c=%d", want) {
		t.Errorf("O(1) count over tombstoned graph = %v, want [c=%d] (full-scan count)", got, want)
	}
}
