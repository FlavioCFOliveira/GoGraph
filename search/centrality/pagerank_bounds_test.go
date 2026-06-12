package centrality

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildRing returns a directed k-node ring CSR.
func buildRing(k int) *csr.CSR[struct{}] {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < k; i++ {
		if err := a.AddEdge(i, (i+1)%k, struct{}{}); err != nil {
			panic(err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// buildRingWithEdge builds a simple 2-node CSR (0→1) for tests that
// only need a non-empty graph and do not care about topology.
func buildMinGraph() *csr.CSR[struct{}] {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		panic(err)
	}
	return csr.BuildFromAdjList(a)
}

// --- PageRank bounds ---

func TestPageRank_InvalidDamping_ReturnsError(t *testing.T) {
	t.Parallel()
	c := buildMinGraph()
	_, _, err := PageRank(c, PageRankOptions{Damping: 1.5})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Damping=1.5: expected ErrInvalidInput, got %v", err)
	}
}

func TestPageRank_DampingAtBoundary_ReturnsError(t *testing.T) {
	t.Parallel()
	c := buildMinGraph()
	// Damping == 1 is not in the open interval (0,1).
	_, _, err := PageRank(c, PageRankOptions{Damping: 1.0})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Damping=1.0: expected ErrInvalidInput, got %v", err)
	}
	// Damping == 0 is the sentinel "use default" — must NOT error.
	// Covered by TestPageRank_ZeroDamping_UsesDefault below.
}

func TestPageRank_NegativeTolerance_ReturnsError(t *testing.T) {
	t.Parallel()
	c := buildMinGraph()
	_, _, err := PageRank(c, PageRankOptions{Damping: 0.85, Tolerance: -1.0})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Tolerance=-1.0: expected ErrInvalidInput, got %v", err)
	}
}

func TestPageRank_ZeroDamping_UsesDefault(t *testing.T) {
	t.Parallel()
	c := buildRing(3)
	// Damping=0 must be treated as "use 0.85 default" — no error.
	ranks, iters, err := PageRank(c, PageRankOptions{})
	if err != nil {
		t.Fatalf("zero Damping: unexpected error %v", err)
	}
	if iters == 0 {
		t.Fatal("zero Damping: expected at least one iteration")
	}
	var total float64
	for _, r := range ranks {
		total += r
	}
	if math.Abs(total-1.0) > 1e-5 {
		t.Fatalf("zero Damping: total rank = %.10f, want 1.0", total)
	}
}

func TestPageRank_ValidOptions_StillWorks(t *testing.T) {
	t.Parallel()
	c := buildRing(3)
	ranks, iters, err := PageRank(c, PageRankOptions{Damping: 0.85, Tolerance: 1e-6})
	if err != nil {
		t.Fatalf("valid opts: unexpected error %v", err)
	}
	if iters == 0 {
		t.Fatal("valid opts: expected at least one iteration")
	}
	var total float64
	for _, r := range ranks {
		total += r
	}
	if math.Abs(total-1.0) > 1e-5 {
		t.Fatalf("valid opts: total rank = %.10f, want 1.0", total)
	}
}

// --- PersonalisedPushPageRank bounds ---

func TestPPR_InvalidDamping_ReturnsError(t *testing.T) {
	t.Parallel()
	c := buildMinGraph()
	_, err := PersonalisedPushPageRank(c, 0, PPRPushOptions{Damping: 1.5})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PPR Damping=1.5: expected ErrInvalidInput, got %v", err)
	}
}

func TestPPR_NegativeEpsilon_ReturnsError(t *testing.T) {
	t.Parallel()
	c := buildMinGraph()
	_, err := PersonalisedPushPageRank(c, 0, PPRPushOptions{Damping: 0.85, Epsilon: -1.0})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PPR Epsilon=-1.0: expected ErrInvalidInput, got %v", err)
	}
}

// TestPPR_MaxStepsExceeded_ReturnsTruncatedWithError verifies that when
// the step budget runs out before the worklist drains, the function
// returns the partial rank vector together with ErrMaxStepsExceeded.
func TestPPR_MaxStepsExceeded_ReturnsTruncatedWithError(t *testing.T) {
	t.Parallel()
	// A ring of 16 nodes with MaxSteps=1 guarantees budget exhaustion
	// before convergence because the worklist will hold more than one
	// entry after the very first push.
	c := buildRing(16)
	opts := PPRPushOptions{Damping: 0.85, Epsilon: 1e-10, MaxSteps: 1}
	ranks, err := PersonalisedPushPageRank(c, 0, opts)
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("MaxSteps=1: expected ErrMaxStepsExceeded, got %v", err)
	}
	if len(ranks) == 0 {
		t.Fatal("MaxSteps=1: expected non-empty partial rank vector")
	}
}

func TestPPR_ValidOptions_StillWorks(t *testing.T) {
	t.Parallel()
	c := buildRing(8)
	ranks, err := PersonalisedPushPageRank(c, 0, PPRPushOptions{Damping: 0.85, Epsilon: 1e-6})
	if err != nil {
		t.Fatalf("PPR valid opts: unexpected error %v", err)
	}
	var total float64
	for _, r := range ranks {
		total += r
	}
	// PPR is a local measure seeded at src; total mass across all nodes
	// is less than 1 because residue left in the worklist is not drained.
	// We assert only that mass is non-negative and does not exceed 1.
	if total < 0 || total > 1.0+1e-9 {
		t.Fatalf("PPR valid opts: total mass = %.10f, want in [0,1]", total)
	}
	if total == 0 {
		t.Fatal("PPR valid opts: total mass is zero — no propagation occurred")
	}
}
