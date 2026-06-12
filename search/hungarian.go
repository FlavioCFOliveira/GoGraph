package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrInvalidInput is returned by algorithms that detect invalid input:
// NaN or +/-Inf in float-valued arrays, or constraint violations such as
// n > m in [Hungarian]. The sentinel is shared across the search and
// centrality packages so callers can errors.Is against it uniformly.
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
// cost matrix using the Jonker-Volgenant / Kuhn-Munkres algorithm.
// cost is given in row-major order (length n*m). When n <= m every
// row is matched; rows beyond min(n, m) are unassigned. The
// algorithm minimises the total cost.
//
// # Complexity
//
// Let V = max(n, m). The algorithm runs in O(V^3) time and O(V^2)
// space (the cost matrix dominates). Each outer iteration performs
// O(V^2) work to extend the shortest-augmenting-path tree; there
// are V iterations. The constant factor is small (the inner loop
// is two pointer-chasing reductions over a row), so 1024x1024
// instances complete in well under a second on the headline
// hardware in [docs/profiling.md].
//
// Memory growth is linear in n*m on the call (the cost slice the
// caller owns) plus O(V) scratch on the heap for the potentials
// and the row queue. The scratch slices are freshly allocated per
// call; pool them externally if invoking Hungarian on a tight loop.
//
// Adapted from the standard "potential" formulation. Pass costs
// directly; do not negate for maximisation.
//
// Returns an empty Assignment paired with [ErrInvalidInput] when cost
// contains any NaN or +/-Inf entry — the dual potentials accumulate
// across iterations and a single non-finite value silently corrupts
// the entire run, so validation is mandatory.
//
// Returns [ErrInvalidInput] when n > m. The Kuhn-Munkres augmenting-path
// formulation requires at least as many columns as rows; when n > m the
// inner loop exhausts all m columns without finding a free slot and spins
// forever. To assign rows to columns when n > m, transpose the cost matrix
// to m×n, call Hungarian, and re-map the result.
//
// v1 limitation. Hungarian is float64-only; the [Weight] constraint
// supports both integer and float types, but Hungarian's dual-update
// step requires a representable "infinity" sentinel that Go's generics
// cannot cleanly produce for arbitrary named numeric types
// (math.MaxFloat64 is not assignable to a ~int8 or ~uint64 W).
// Integer-weighted assignment is therefore deferred; callers with
// integer cost matrices should currently convert to float64.
func Hungarian(cost []float64, n, m int) (Assignment, error) {
	defer metrics.Time("search.Hungarian")()
	res, err := HungarianCtx(context.Background(), cost, n, m)
	if err != nil {
		metrics.IncCounter("search.Hungarian.errors", 1)
	}
	return res, err
}

// HungarianCtx is the context-aware variant of [Hungarian]. ctx.Err()
// is checked both at every row-augmenting iteration and inside the inner
// augmenting-path loop, so cancellation is honoured promptly even on
// large matrices. On cancellation returns (zero Assignment, wrapped ctx.Err()).
//
// Returns [ErrInvalidInput] when n > m. See [Hungarian] for details and
// the transposition workaround.
//
//nolint:gocyclo // textbook Hungarian: validation + dual update + augment
func HungarianCtx(ctx context.Context, cost []float64, n, m int) (Assignment, error) {
	defer metrics.Time("search.HungarianCtx")()
	if n == 0 || m == 0 {
		return Assignment{RowToCol: make([]int, n)}, nil
	}
	if n > m {
		metrics.IncCounter("search.HungarianCtx.errors", 1)
		return Assignment{}, fmt.Errorf("search: Hungarian requires n <= m (got n=%d, m=%d): %w", n, m, ErrInvalidInput)
	}
	if len(cost) != n*m {
		metrics.IncCounter("search.HungarianCtx.errors", 1)
		return Assignment{}, errors.New("search: Hungarian cost length must equal n*m")
	}
	for _, v := range cost {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			metrics.IncCounter("search.HungarianCtx.errors", 1)
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
			metrics.IncCounter("search.HungarianCtx.errors", 1)
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
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.HungarianCtx.errors", 1)
				return Assignment{}, err
			}
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
