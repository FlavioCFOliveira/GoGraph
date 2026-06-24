package flow

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// MinCutResult is the output of [StoerWagner].
type MinCutResult struct {
	Weight int
	A      []int // one side of the cut
	B      []int // the other side
}

// StoerWagner computes the global minimum cut on an undirected
// weighted graph represented as a dense n*n weight matrix in row-
// major order. Weights must be non-negative; symmetry w[i*n+j] ==
// w[j*n+i] is assumed.
//
// Complexity O(V^3) with the simple maximum-adjacency form.
func StoerWagner(weights []int, n int) MinCutResult {
	defer metrics.Time("search.flow.StoerWagner").Stop()
	out, _ := StoerWagnerCtx(context.Background(), weights, n)
	return out
}

// StoerWagnerCtx is the context-aware variant of [StoerWagner].
// ctx.Err() is checked at every phase boundary; on cancellation
// returns (zero MinCutResult, wrapped ctx.Err()).
func StoerWagnerCtx(ctx context.Context, weights []int, n int) (MinCutResult, error) {
	defer metrics.Time("search.flow.StoerWagnerCtx").Stop()
	if n <= 1 {
		return MinCutResult{Weight: 0}, nil
	}
	// Copy the matrix so we can merge vertices in place.
	w := make([]int, len(weights))
	copy(w, weights)
	alive := make([]bool, n)
	for i := range alive {
		alive[i] = true
	}
	// Group records which original vertices were merged into i.
	group := make([][]int, n)
	for i := 0; i < n; i++ {
		group[i] = []int{i}
	}

	bestWeight := -1
	var bestA []int

	for phase := 0; phase < n-1; phase++ {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.flow.StoerWagnerCtx.errors", 1)
			return MinCutResult{}, err
		}
		s, tIdx, cutOfPhase := maxAdjacencyPhase(w, alive, n)
		if bestWeight < 0 || cutOfPhase < bestWeight {
			bestWeight = cutOfPhase
			bestA = append([]int(nil), group[tIdx]...)
		}
		// Merge t into s: combine groups, sum edge weights.
		group[s] = append(group[s], group[tIdx]...)
		group[tIdx] = nil
		for i := 0; i < n; i++ {
			if i == s || i == tIdx || !alive[i] {
				continue
			}
			w[i*n+s] += w[i*n+tIdx]
			w[s*n+i] = w[i*n+s]
		}
		alive[tIdx] = false
	}

	inA := make(map[int]struct{}, len(bestA))
	for _, v := range bestA {
		inA[v] = struct{}{}
	}
	var b []int
	for i := 0; i < n; i++ {
		if _, ok := inA[i]; !ok {
			b = append(b, i)
		}
	}
	return MinCutResult{Weight: bestWeight, A: bestA, B: b}, nil
}

// maxAdjacencyPhase runs one maximum-adjacency-ordering phase.
// Returns the two vertices last added (s = penultimate, tIdx =
// last) plus the cut-of-the-phase weight (the weighted degree of
// tIdx at the moment it was picked).
func maxAdjacencyPhase(w []int, alive []bool, n int) (s, tIdx, cutOfPhase int) {
	inA := make([]bool, n)
	adjacency := make([]int, n)
	last := -1
	prev := -1
	for picks := 0; picks < countAlive(alive); picks++ {
		k := -1
		bestVal := -1
		for i := 0; i < n; i++ {
			if !alive[i] || inA[i] {
				continue
			}
			if adjacency[i] > bestVal {
				bestVal = adjacency[i]
				k = i
			}
		}
		inA[k] = true
		prev = last
		last = k
		cutOfPhase = bestVal
		for j := 0; j < n; j++ {
			if alive[j] && !inA[j] {
				adjacency[j] += w[k*n+j]
			}
		}
	}
	return prev, last, cutOfPhase
}

func countAlive(alive []bool) int {
	n := 0
	for _, a := range alive {
		if a {
			n++
		}
	}
	return n
}
