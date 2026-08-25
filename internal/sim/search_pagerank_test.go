package sim

import (
	"context"
	"math"
	"strings"
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

// TestPageRankerStatefulArm_AliasClausesFire proves the two clauses of the
// per-tick stateful arm can FAIL, which neither of them can do on correct code:
// one is a CONTROL for the other, and a control that has never been shown to
// fire is not a control. The scenario file has a perturbation harness for the
// same pair ([prPerturbAliasCopyAliases], [prPerturbFreezePrevSlice]); this is
// the unit-level equivalent for the cheap per-tick arm, which has none.
func TestPageRankerStatefulArm_AliasClausesFire(t *testing.T) {
	t.Parallel()
	first := []float64{0.10, 0.20, 0.30, 0.40}
	second := []float64{0.15, 0.25, 0.35, 0.25}

	t.Run("control-is-silent", func(t *testing.T) {
		t.Parallel()
		// The correct shape: a real copy, and a slice the next Run overwrote.
		copyOut := append([]float64(nil), first...)
		hash := prHashFloats(copyOut)
		overwritten := append([]float64(nil), second...)
		if vs := pagerankerAliasViolations(1, len(first), second, overwritten, copyOut, hash); len(vs) != 0 {
			t.Fatalf("the correct shape reported violations: %v", vs)
		}
	})

	t.Run("copy-that-aliases-fires-the-control", func(t *testing.T) {
		t.Parallel()
		// The caller mistake the aliasing note warns about: the "copy" IS the
		// returned slice, so its hash moves when the next Run overwrites the buffer.
		buffer := append([]float64(nil), first...)
		hash := prHashFloats(buffer)
		copy(buffer, second) // the next Run overwrites the buffer in place
		vs := pagerankerAliasViolations(1, len(first), second, buffer, buffer, hash)
		if len(vs) == 0 {
			t.Fatalf("an aliasing copy did not fire the control clause")
		}
		if !strings.Contains(vs[0].Message, "aliasing control is unsound") {
			t.Fatalf("wrong clause fired: %v", vs)
		}
	})

	t.Run("frozen-buffer-fires-the-pin", func(t *testing.T) {
		t.Parallel()
		// A Run that stopped reusing the buffer: the first result's slice still
		// reads its old values although the two results differ.
		copyOut := append([]float64(nil), first...)
		hash := prHashFloats(copyOut)
		frozen := append([]float64(nil), first...)
		vs := pagerankerAliasViolations(1, len(first), second, frozen, copyOut, hash)
		if len(vs) != 1 {
			t.Fatalf("want exactly the pin clause, got %d: %v", len(vs), vs)
		}
		if !strings.Contains(vs[0].Message, "invalidated by the next Run") {
			t.Fatalf("wrong clause fired: %v", vs)
		}
	})

	t.Run("equal-results-leave-the-pin-unarmed", func(t *testing.T) {
		t.Parallel()
		// The vacuity the gate exists for: when the two runs agree, a frozen buffer
		// is indistinguishable from an overwritten one, so the pin must stay silent
		// rather than fire on correct code.
		copyOut := append([]float64(nil), first...)
		hash := prHashFloats(copyOut)
		frozen := append([]float64(nil), first...)
		if vs := pagerankerAliasViolations(1, len(first), first, frozen, copyOut, hash); len(vs) != 0 {
			t.Fatalf("the pin fired although the two results are identical: %v", vs)
		}
	})
}

// TestPageRankerStatefulArm_DrivesBothRunsCleanly asserts the arm's own numbers
// on a fixture rather than only the absence of a violation: two Runs, both
// bit-identical to their one-shot, and a second result that differs from the
// first (or the aliasing pin would be unarmed on every tick).
func TestPageRankerStatefulArm_DrivesBothRunsCleanly(t *testing.T) {
	t.Parallel()
	armed := 0
	for tick := int64(1); tick <= 25; tick++ {
		if v := pagerankerStatefulViolations(tick); len(v) != 0 {
			t.Fatalf("tick %d: %v", tick, v)
		}
		seed := NewSeed(uint64(tick) ^ pagerankerStatefulSalt)
		n, edges := pagerankGenGraph(seed)
		c := pagerankBuildCSR(n, edges)
		pr := centrality.NewPageRanker(c)
		a, _, err := pr.Run(context.Background(), pagerankerStatefulOpts()[0])
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		aCopy := append([]float64(nil), a...)
		b, _, err := pr.Run(context.Background(), pagerankerStatefulOpts()[1])
		if err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if diff, _, _ := prCompareBits(b, aCopy); diff > 0 {
			armed++
		}
	}
	if armed == 0 {
		t.Fatalf("the arm's two option sets produced identical results on all 25 ticks, so its "+
			"aliasing pin is never armed: dampings %v", pagerankerStatefulOpts())
	}
	t.Logf("the aliasing pin was armed on %d of 25 ticks", armed)
}
