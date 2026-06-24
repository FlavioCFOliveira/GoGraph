package search

import (
	"context"
	"runtime"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Diameter estimates the diameter of c — the longest shortest-path
// distance between any two vertices — using a 2-sweep BFS lower
// bound combined with an iFUB-style fringe upper-bound refinement.
// Returns (lo, hi, exact) where lo <= true diameter <= hi; exact is
// true when the bounds have converged and the value is exact.
//
// 2-sweep alone is the standard heuristic for road-network-shaped
// graphs and consistently delivers lo == true diameter in practice;
// the iFUB refinement tightens hi to lo on most real-world inputs.
// On adversarial inputs (e.g. expanders) hi may remain strictly
// above lo, in which case the caller can choose to accept the
// lower bound or to brute-force the true diameter (V * BFS).
//
// Concurrency: Diameter is safe to invoke concurrently on a shared
// CSR. The implementation expects c to encode an undirected graph
// (symmetric directed CSR).
func Diameter[W any](c *csr.CSR[W]) (lo, hi int, exact bool) {
	defer metrics.Time("search.Diameter").Stop()
	lo, hi, exact, _ = DiameterCtx(context.Background(), c)
	return lo, hi, exact
}

// DiameterCtx is the context-aware variant of [Diameter]. ctx.Err()
// is checked once per BFS sweep and before each per-vertex
// eccentricity BFS inside an iFUB level (a single level can hold O(V)
// vertices, each costing a full O(V+E) BFS, so a per-level check alone
// leaves an O(V*(V+E)) uninterruptible window); on cancellation returns
// the bounds reached so far together with the wrapped ctx.Err().
//
//nolint:gocyclo // 2-sweep + iFUB refinement: precondition checks + sweeps + level walk
func DiameterCtx[W any](ctx context.Context, c *csr.CSR[W]) (lo, hi int, exact bool, err error) {
	defer metrics.Time("search.DiameterCtx").Stop()
	n := int(c.MaxNodeID())
	if n == 0 {
		return 0, 0, true, nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	mask := c.LiveMask()
	// Pick a non-isolated seed.
	seed := -1
	for i := 0; i < n; i++ {
		if mask[i] && verts[i+1] > verts[i] {
			seed = i
			break
		}
	}
	if seed < 0 {
		return 0, 0, true, nil
	}
	// 2-sweep lower bound: BFS from seed, find farthest u; BFS from u,
	// find farthest w. dist[w] is a lower bound on the true diameter.
	//
	// Two scratch slices are kept: distFromU is the BFS layering from
	// the 2-sweep anchor farU and MUST NOT be reused by the inner-loop
	// BFS below — otherwise the level filter `distFromU[v]==k` would
	// be corrupted after the first iteration. distInner is the scratch
	// used exclusively by the per-vertex eccentricity sweeps.
	scratch := make([]int, n)
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("search.DiameterCtx.errors", 1)
		return 0, 0, false, err
	}
	farU, _ := bfsFarthest(verts, edges, graph.NodeID(seed), scratch)
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("search.DiameterCtx.errors", 1)
		return 0, 0, false, err
	}
	farW, _ := bfsFarthest(verts, edges, farU, scratch)
	distFromU := make([]int, n)
	copy(distFromU, scratch)
	lo = distFromU[farW]

	// iFUB upper bound. From the centre-most vertex on the lo path
	// (here: farU, the source of the longest known path), the
	// eccentricity gives an upper bound diameter <= 2 * ecc(farU).
	// We tighten by walking the BFS levels outward: as long as the
	// maximum distance at level k satisfies 2*k > lo, the current
	// hi stays at 2*k; once 2*k <= lo the bound has converged and
	// hi can be set to lo.
	maxLevel := 0
	for _, d := range distFromU {
		if d > maxLevel {
			maxLevel = d
		}
	}
	hi = 2 * maxLevel
	if hi <= lo {
		return lo, lo, true, nil
	}
	// iFUB-style refinement: walk levels k from maxLevel down,
	// computing the eccentricity of each vertex at level k and
	// updating lo. After processing level k, the contributions from
	// levels strictly below k are bounded above by 2*(k-1); when
	// that bound is no greater than the current lo, no further
	// improvement is possible and the bounds have converged.
	// Serial eccentricity scratch, reused across levels; the parallel path
	// (wide levels) allocates private per-worker scratch.
	distInner := make([]int, n)
	numWorkers := runtime.GOMAXPROCS(0)
	var levelVerts []int
	for k := maxLevel; k > 0; k-- {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.DiameterCtx.errors", 1)
			return lo, hi, false, err
		}
		// Collect the vertices at BFS level k. Each costs a full O(V+E)
		// eccentricity BFS and the sweeps are independent, so a wide level is
		// computed in parallel. The level contribution is an integer max, so the
		// refined lower bound is identical regardless of worker count or order —
		// the (lo, hi, exact) result stays bit-identical to the serial walk.
		levelVerts = levelVerts[:0]
		for v := 0; v < n; v++ {
			if distFromU[v] == k {
				levelVerts = append(levelVerts, v)
			}
		}
		maxEcc, err := levelMaxEccentricity(ctx, verts, edges, levelVerts, n, numWorkers, distInner)
		if err != nil {
			metrics.IncCounter("search.DiameterCtx.errors", 1)
			return lo, hi, false, err
		}
		if maxEcc > lo {
			lo = maxEcc
		}
		// After this level, the best the unprocessed levels (< k)
		// can prove is 2*(k-1). The upper bound is max(lo, 2*(k-1)).
		newHi := 2 * (k - 1)
		if newHi < lo {
			newHi = lo
		}
		hi = newHi
		if hi <= lo {
			break
		}
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi, hi == lo, nil
}

// diameterParallelMinLevel is the level-vertex count at or above which one
// iFUB level's independent eccentricity sweeps are run in parallel. Below it
// the fan-out overhead is not worth it (and 2-sweep-converging inputs — the
// common road-network case — never reach the refinement loop at all).
const diameterParallelMinLevel = 8

// levelMaxEccentricity returns the maximum eccentricity over levelVerts. With
// enough vertices and workers it runs the independent eccentricity BFS sweeps
// in parallel, each worker holding private scratch, and reduces by integer max;
// the result is identical to the serial walk regardless of worker count or
// scheduling, so DiameterCtx's refined bound stays bit-identical. The serial
// path reuses the caller's serialScratch; the parallel path allocates private
// per-worker scratch (only for wide levels, where the O(V+E) sweeps dwarf it).
// ctx is polled before every sweep so cancellation latency is one inner BFS.
func levelMaxEccentricity(ctx context.Context, verts []uint64, edges []graph.NodeID, levelVerts []int, n, numWorkers int, serialScratch []int) (int, error) {
	if numWorkers <= 1 || len(levelVerts) < diameterParallelMinLevel {
		maxEcc := 0
		for _, v := range levelVerts {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			_, distV := bfsFarthest(verts, edges, graph.NodeID(v), serialScratch)
			ecc := 0
			for _, d := range distV {
				if d > ecc {
					ecc = d
				}
			}
			if ecc > maxEcc {
				maxEcc = ecc
			}
		}
		return maxEcc, nil
	}
	if numWorkers > len(levelVerts) {
		numWorkers = len(levelVerts)
	}
	localMax := make([]int, numWorkers)
	localErr := make([]error, numWorkers)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dist := make([]int, n) // private per-worker scratch
			m := 0
			for i := w; i < len(levelVerts); i += numWorkers {
				if err := ctx.Err(); err != nil {
					localErr[w] = err
					return
				}
				_, distV := bfsFarthest(verts, edges, graph.NodeID(levelVerts[i]), dist)
				ecc := 0
				for _, d := range distV {
					if d > ecc {
						ecc = d
					}
				}
				if ecc > m {
					m = ecc
				}
			}
			localMax[w] = m
		}(w)
	}
	wg.Wait()
	maxEcc := 0
	var firstErr error
	for w := 0; w < numWorkers; w++ {
		if localErr[w] != nil && firstErr == nil {
			firstErr = localErr[w]
		}
		if localMax[w] > maxEcc {
			maxEcc = localMax[w]
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return maxEcc, nil
}

// bfsFarthest runs a single BFS from src and returns the farthest
// vertex (smallest NodeID on tie) along with the per-vertex distance
// slice. dist is the caller-provided scratch (resized in place).
func bfsFarthest(verts []uint64, edges []graph.NodeID, src graph.NodeID, dist []int) (farthest graph.NodeID, distOut []int) {
	for i := range dist {
		dist[i] = -1
	}
	dist[uint64(src)] = 0
	queue := []graph.NodeID{src}
	farthest = src
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		dv := dist[uint64(v)]
		for k := verts[uint64(v)]; k < verts[uint64(v)+1]; k++ {
			nb := edges[k]
			if dist[uint64(nb)] >= 0 {
				continue
			}
			dist[uint64(nb)] = dv + 1
			queue = append(queue, nb)
			if dist[uint64(nb)] > dist[uint64(farthest)] || (dist[uint64(nb)] == dist[uint64(farthest)] && uint64(nb) < uint64(farthest)) {
				farthest = nb
			}
		}
	}
	return farthest, dist
}
