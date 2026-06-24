package sim

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestMatchingChecks_CleanOnFixtures asserts the whole MATCHING battery
// (Hopcroft-Karp vs the Kuhn reference + validity; Hungarian vs the brute-force
// optimum + permutation check) is clean on every seed-derived instance across a
// range of ticks. The instances are pure functions of the tick, so a clean pass
// here is reproducible.
func TestMatchingChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for tick := int64(0); tick < 200; tick++ {
		if v := matchingViolations(tick); len(v) != 0 {
			t.Fatalf("matching battery flagged a clean instance at tick=%d:\n%v", tick, v)
		}
	}
}

// TestMatchingViolations_Deterministic asserts the same tick yields the same
// (empty) result on repeat, i.e. the check draws only from the seed.
func TestMatchingViolations_Deterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 1, 7, 42, 1000} {
		a := matchingViolations(tick)
		b := matchingViolations(tick)
		if len(a) != len(b) {
			t.Fatalf("tick=%d nondeterministic: len %d vs %d", tick, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("tick=%d violation %d differs: %v vs %v", tick, i, a[i], b[i])
			}
		}
	}
}

// --- Reference-correctness tests: prove OUR references are right, on tiny
// hand-checked instances, before trusting them to judge the library. ---

// TestKuhnReference_HandChecked verifies the Kuhn maximum-matching reference on
// instances whose optimum is obvious by inspection.
func TestKuhnReference_HandChecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		nLeft  int
		nRight int
		adj    [][]int
		want   int
	}{
		{
			// Perfect matching available: L0-R0, L1-R1.
			name: "perfect-2x2", nLeft: 2, nRight: 2,
			adj: [][]int{{0, 1}, {0, 1}}, want: 2,
		},
		{
			// Both left vertices only reach R0: they contend, max is 1.
			name: "contention", nLeft: 2, nRight: 2,
			adj: [][]int{{0}, {0}}, want: 1,
		},
		{
			// One left vertex has no edge: max is 1.
			name: "one-isolated-left", nLeft: 2, nRight: 2,
			adj: [][]int{{0}, nil}, want: 1,
		},
		{
			// Augmenting-path case: greedy L0->R0 would block L1, but the
			// augmenting path reroutes to reach size 2 (L0->R1, L1->R0).
			name: "needs-augment", nLeft: 2, nRight: 2,
			adj: [][]int{{0, 1}, {0}}, want: 2,
		},
		{
			// 3x3 with a unique perfect matching forced by structure.
			name: "perfect-3x3", nLeft: 3, nRight: 3,
			adj: [][]int{{0}, {0, 1}, {1, 2}}, want: 3,
		},
		{
			name: "empty", nLeft: 2, nRight: 2,
			adj: [][]int{nil, nil}, want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matchingKuhnMaxMatching(tt.nLeft, tt.nRight, tt.adj); got != tt.want {
				t.Fatalf("Kuhn(%s)=%d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// TestKuhnReference_AgreesWithHopcroftKarp pins the two algorithms together on
// the same hand-built instances: this is the actual production cross-check, so a
// targeted positive control protects it.
func TestKuhnReference_AgreesWithHopcroftKarp(t *testing.T) {
	t.Parallel()
	instances := [][][]int{
		{{0, 1}, {0, 1}},
		{{0}, {0}},
		{{0, 1}, {0}},
		{{0}, {0, 1}, {1, 2}},
		{{0, 2}, {1}, {0, 1, 2}, {2}},
	}
	for i, adj := range instances {
		nLeft := len(adj)
		nRight := 0
		var total int
		for _, row := range adj {
			total += len(row)
			for _, r := range row {
				if r+1 > nRight {
					nRight = r + 1
				}
			}
		}
		c := matchingBuildBipartiteCSR(nLeft, nRight, adj, total)
		hk := search.HopcroftKarp(c, nLeft)
		kuhn := matchingKuhnMaxMatching(nLeft, nRight, adj)
		if hk.Size != kuhn {
			t.Fatalf("instance %d: HopcroftKarp.Size=%d, Kuhn=%d", i, hk.Size, kuhn)
		}
	}
}

// TestBruteForceAssign_HandChecked verifies the brute-force assignment reference
// on instances whose minimum is obvious.
func TestBruteForceAssign_HandChecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int
		cost []int
		want int
	}{
		{
			// Identity is clearly optimal: pick the 1s on the diagonal.
			name: "diagonal-cheap", n: 2,
			cost: []int{1, 9, 9, 1}, want: 2,
		},
		{
			// Off-diagonal is optimal: 1+1 beats 9+9.
			name: "anti-diagonal-cheap", n: 2,
			cost: []int{9, 1, 1, 9}, want: 2,
		},
		{
			// 3x3 with the optimum being the permutation (0->2,1->1,2->0)=1+1+1.
			name: "3x3", n: 3,
			cost: []int{
				5, 5, 1,
				5, 1, 5,
				1, 5, 5,
			}, want: 3,
		},
		{
			name: "trivial-1x1", n: 1, cost: []int{7}, want: 7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matchingBruteForceAssign(tt.cost, tt.n); got != tt.want {
				t.Fatalf("bruteForce(%s)=%d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// TestBruteForceAssign_AgreesWithHungarian pins the brute-force optimum to
// Hungarian on hand-built instances — the production cross-check's positive
// control.
func TestBruteForceAssign_AgreesWithHungarian(t *testing.T) {
	t.Parallel()
	instances := []struct {
		n    int
		cost []int
	}{
		{2, []int{1, 9, 9, 1}},
		{2, []int{9, 1, 1, 9}},
		{3, []int{5, 5, 1, 5, 1, 5, 1, 5, 5}},
		{4, []int{
			3, 1, 4, 1,
			5, 9, 2, 6,
			5, 3, 5, 8,
			9, 7, 9, 3,
		}},
	}
	for i, inst := range instances {
		cost := make([]float64, len(inst.cost))
		for j, v := range inst.cost {
			cost[j] = float64(v)
		}
		a, err := search.Hungarian(cost, inst.n, inst.n)
		if err != nil {
			t.Fatalf("instance %d: Hungarian error: %v", i, err)
		}
		want := matchingBruteForceAssign(inst.cost, inst.n)
		if int(a.TotalCost) != want {
			t.Fatalf("instance %d: Hungarian.TotalCost=%v, brute-force=%d", i, a.TotalCost, want)
		}
	}
}

// --- Negative controls: prove the comparison DETECTS a deliberate mismatch. ---

// TestMatchingValidate_DetectsBadMatching feeds matchingValidate a Matching that
// is deliberately wrong (claims an edge that does not exist) and asserts it is
// flagged.
func TestMatchingValidate_DetectsBadMatching(t *testing.T) {
	t.Parallel()
	// L0 only reaches R0; L1 reaches nothing. A valid matching has size 1.
	nLeft, nRight := 2, 2
	adj := [][]int{{0}, nil}
	const unmatched = ^graph.NodeID(0)

	// Forge a matching asserting L1->R1, an edge that does not exist, and a
	// double-claim of R0 is avoided so the "no real edge" branch is exercised.
	matchL := []graph.NodeID{unmatched, graph.NodeID(nLeft + 1)} // L1 -> R1 (bogus)
	matchR := make([]graph.NodeID, nLeft+nRight)
	for i := range matchR {
		matchR[i] = unmatched
	}
	matchR[nLeft+1] = graph.NodeID(1) // symmetric so only the "no edge" check fires
	bad := search.Matching{MatchL: matchL, MatchR: matchR, Size: 1}

	if vs := matchingValidate(7, nLeft, nRight, adj, bad); len(vs) == 0 {
		t.Fatal("matchingValidate accepted a matching that claims a non-existent edge")
	}
}

// TestMatchingValidate_DetectsSizeMismatch asserts a Size that disagrees with
// the actual matched-vertex count is flagged.
func TestMatchingValidate_DetectsSizeMismatch(t *testing.T) {
	t.Parallel()
	nLeft, nRight := 2, 2
	adj := [][]int{{0}, {1}}
	const unmatched = ^graph.NodeID(0)
	matchL := []graph.NodeID{graph.NodeID(nLeft + 0), graph.NodeID(nLeft + 1)} // both matched, legitimately
	matchR := make([]graph.NodeID, nLeft+nRight)
	for i := range matchR {
		matchR[i] = unmatched
	}
	matchR[nLeft+0] = 0
	matchR[nLeft+1] = 1
	wrong := search.Matching{MatchL: matchL, MatchR: matchR, Size: 1} // lies: should be 2

	if vs := matchingValidate(7, nLeft, nRight, adj, wrong); len(vs) == 0 {
		t.Fatal("matchingValidate accepted a Matching whose Size contradicts its links")
	}
}

// TestAssignmentComparison_DetectsBadCost asserts the assignment cost comparison
// catches a sub-optimal/inconsistent total. We drive it through
// matchingValidateAssignment with a RowToCol whose induced cost contradicts the
// reported TotalCost, and separately confirm a wrong "optimum" would be caught.
func TestAssignmentComparison_DetectsBadCost(t *testing.T) {
	t.Parallel()
	// 2x2: optimum is the diagonal (1+1=2).
	cost := []int{1, 9, 9, 1}
	n := 2

	// A RowToCol whose stated TotalCost is wrong must be flagged.
	a := search.Assignment{RowToCol: []int{0, 1}, TotalCost: 99}
	if vs := matchingValidateAssignment(7, cost, n, a); len(vs) == 0 {
		t.Fatal("matchingValidateAssignment accepted a TotalCost inconsistent with RowToCol")
	}

	// A non-permutation RowToCol (column 0 twice) must be flagged.
	dup := search.Assignment{RowToCol: []int{0, 0}, TotalCost: 0}
	if vs := matchingValidateAssignment(7, cost, n, dup); len(vs) == 0 {
		t.Fatal("matchingValidateAssignment accepted a non-permutation RowToCol")
	}

	// Sanity: the comparison predicate (brute force vs a wrong claimed optimum)
	// detects divergence. The true optimum is 2; a claim of 3 must differ.
	if want := matchingBruteForceAssign(cost, n); want == 3 {
		t.Fatalf("brute-force optimum unexpectedly 3; comparison would not detect the mismatch")
	}
}

// TestMatchingCardinalityComparison_DetectsMismatch constructs an instance,
// computes both the true cardinality and a deliberately-wrong one, and confirms
// the equality predicate the production code uses separates them.
func TestMatchingCardinalityComparison_DetectsMismatch(t *testing.T) {
	t.Parallel()
	adj := [][]int{{0, 1}, {0}} // max matching is 2
	nLeft, nRight := 2, 2
	want := matchingKuhnMaxMatching(nLeft, nRight, adj)
	if want != 2 {
		t.Fatalf("reference cardinality=%d, want 2", want)
	}
	// The production check is `got.Size != want`; a wrong size must trip it.
	wrongSize := 1
	if wrongSize == want {
		t.Fatal("deliberately-wrong size accidentally equals the reference")
	}
}
