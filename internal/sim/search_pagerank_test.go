package sim

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// TestPageRankChecks_CleanOnFixtures runs the PageRank battery for several ticks
// and asserts the library agrees with the independent reference.
func TestPageRankChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for tick := int64(1); tick <= 50; tick++ {
		if v := pagerankViolations(tick); len(v) != 0 {
			t.Fatalf("tick %d: PageRank battery: %v", tick, v)
		}
	}
}

// TestPageRankReference_UniformOnCycle checks the reference (and the library)
// give a uniform distribution on a directed cycle, where symmetry forces 1/n.
func TestPageRankReference_UniformOnCycle(t *testing.T) {
	t.Parallel()
	// 0->1->2->0: a single cycle, no dangling node.
	edges := [][2]int{{0, 1}, {1, 2}, {2, 0}}
	want := pagerankReference(3, edges, 0.85)
	for i, r := range want {
		if math.Abs(r-1.0/3.0) > 1e-9 {
			t.Fatalf("reference rank[%d]=%v, want 1/3 on a 3-cycle", i, r)
		}
	}
	// The library must agree.
	got, iters, err := centrality.PageRank(pagerankBuildCSR(3, edges), centrality.DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}
	if iters == 0 {
		t.Fatal("expected at least one iteration")
	}
	for i, r := range got {
		if !pagerankClose(r, want[i]) {
			t.Fatalf("library rank[%d]=%v disagrees with reference %v", i, r, want[i])
		}
	}
}

// TestPageRankReference_RanksSumToOne checks the reference conserves total mass.
func TestPageRankReference_RanksSumToOne(t *testing.T) {
	t.Parallel()
	// A graph with a dangling sink (3 has no out-edge), exercising redistribution.
	edges := [][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 3}}
	rank := pagerankReference(4, edges, 0.85)
	var sum float64
	for _, r := range rank {
		sum += r
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("ranks sum to %v, want 1.0 (dangling mass must be conserved)", sum)
	}
}

// TestPageRankClose checks the comparison tolerance boundary.
func TestPageRankClose(t *testing.T) {
	t.Parallel()
	if !pagerankClose(0.25, 0.25+5e-5) {
		t.Fatal("difference within epsilon must be accepted")
	}
	if pagerankClose(0.25, 0.25+1e-3) {
		t.Fatal("difference beyond epsilon must be rejected")
	}
}
