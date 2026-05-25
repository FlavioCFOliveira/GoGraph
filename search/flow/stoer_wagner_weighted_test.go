package flow

import "testing"

// TestStoerWagner_RingOfCliques builds a ring of k cliques, each of
// size c, and verifies that Stoer-Wagner finds the correct global
// min-cut.
//
// Structure:
//   - k=4 cliques, each containing c=5 vertices.
//   - Intra-clique edges: weight Win=3 (all pairs within a clique).
//   - Inter-clique edges: weight Wout=1, arranged as a ring so each
//     clique connects to exactly 2 neighbours (last node of clique i
//     to first node of clique (i+1) mod k).
//
// Total vertices n = k*c = 20.
//
// Min-cut analysis:
//   - Isolating one clique severs exactly 2 inter-clique edges
//     (one to each neighbour), contributing 2*Wout = 2 to the cut.
//   - Splitting a clique internally requires severing (c-1)*Win = 12
//     intra-clique edges — far heavier than the inter-clique cut.
//   - The global minimum cut is therefore 2.
func TestStoerWagner_RingOfCliques(t *testing.T) {
	t.Parallel()

	const (
		k    = 4
		c    = 5
		Win  = 3
		Wout = 1
		n    = k * c
	)

	w := make([]int, n*n)
	set := func(i, j, v int) {
		w[i*n+j] += v
		w[j*n+i] += v
	}

	// Intra-clique edges: all pairs within each clique.
	for ci := 0; ci < k; ci++ {
		for i := ci * c; i < (ci+1)*c; i++ {
			for j := i + 1; j < (ci+1)*c; j++ {
				set(i, j, Win)
			}
		}
	}

	// Inter-clique edges: ring — last node of clique ci to first node
	// of clique (ci+1)%k.
	for ci := 0; ci < k; ci++ {
		next := (ci + 1) % k
		set(ci*c+c-1, next*c, Wout)
	}

	result := StoerWagner(w, n)

	const wantWeight = 2 * Wout // two inter-clique edges severed
	if result.Weight != wantWeight {
		t.Fatalf("StoerWagner min cut = %d, want %d", result.Weight, wantWeight)
	}

	// Verify partition is non-trivial: both sides must be non-empty.
	if len(result.A) == 0 || len(result.B) == 0 {
		t.Fatalf("degenerate partition: A=%v B=%v", result.A, result.B)
	}
	// Verify partition covers all vertices.
	if len(result.A)+len(result.B) != n {
		t.Fatalf("partition size %d+%d != n=%d", len(result.A), len(result.B), n)
	}
}
