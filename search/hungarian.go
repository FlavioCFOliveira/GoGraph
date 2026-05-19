package search

import (
	"context"
	"errors"
	"math"
	"runtime"
)

// ErrInvalidInput is returned by algorithms that detect NaN or +/-Inf
// in float-valued input arrays. The sentinel is shared across the
// search and centrality packages so callers can errors.Is against it
// uniformly.
var ErrInvalidInput = errors.New("search: input contains NaN or Inf")

// Assignment is the result of [Hungarian]: the minimum total cost
// and the column assigned to each row.
//
// Concurrency: Assignment is a value type returned freshly per call
// and is safe for concurrent reads.
type Assignment struct {
	TotalCost float64
	RowToCol  []int
}

// Hungarian solves the rectangular assignment problem on the n*m
// cost matrix using the O(V^3) Jonker-Volgenant / Kuhn-Munkres
// algorithm. cost is given in row-major order (length n*m). When
// n <= m every row is matched; rows beyond min(n, m) are
// unassigned. The algorithm minimises the total cost.
//
// Adapted from the standard "potential" formulation. Pass costs
// directly; do not negate for maximisation.
//
// Returns an empty Assignment paired with [ErrInvalidInput] when cost
// contains any NaN or +/-Inf entry — the dual potentials accumulate
// across iterations and a single non-finite value silently corrupts
// the entire run, so validation is mandatory.
func Hungarian(cost []float64, n, m int) (Assignment, error) {
	return HungarianCtx(context.Background(), cost, n, m)
}

// HungarianCtx is the context-aware variant of [Hungarian]. ctx.Err()
// is checked at every row-augmenting iteration; on cancellation
// returns (zero Assignment, wrapped ctx.Err()).
//
//nolint:gocyclo // textbook Hungarian: validation + dual update + augment
func HungarianCtx(ctx context.Context, cost []float64, n, m int) (Assignment, error) {
	if n == 0 || m == 0 {
		return Assignment{RowToCol: make([]int, n)}, nil
	}
	if len(cost) != n*m {
		return Assignment{}, errors.New("search: Hungarian cost length must equal n*m")
	}
	for _, v := range cost {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Assignment{}, ErrInvalidInput
		}
	}
	const inf = math.MaxFloat64

	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)

	for i := 1; i <= n; i++ {
		if err := ctx.Err(); err != nil {
			return Assignment{}, err
		}
		if i&0x3F == 0 {
			runtime.Gosched()
		}
		p[0] = i
		j0 := 0
		minv := make([]float64, m+1)
		used := make([]bool, m+1)
		for k := range minv {
			minv[k] = inf
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := cost[(i0-1)*m+(j-1)] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}

	rowToCol := make([]int, n)
	for i := range rowToCol {
		rowToCol[i] = -1
	}
	total := 0.0
	for j := 1; j <= m; j++ {
		if p[j] > 0 {
			rowToCol[p[j]-1] = j - 1
			total += cost[(p[j]-1)*m+(j-1)]
		}
	}
	return Assignment{TotalCost: total, RowToCol: rowToCol}, nil
}
