package centrality

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// PPRPushOptions controls [PersonalisedPushPageRank].
type PPRPushOptions struct {
	// Damping is the random-jump probability (alpha; typical 0.85).
	Damping float64
	// Epsilon stops propagation when residue/outdeg falls below it.
	Epsilon float64
	// MaxSteps caps the number of push operations for safety.
	MaxSteps int
}

// DefaultPPRPushOptions returns the Andersen-Chung-Lang reference
// parameters (damping 0.85, epsilon 1e-6, max 1e7 steps).
func DefaultPPRPushOptions() PPRPushOptions {
	return PPRPushOptions{Damping: 0.85, Epsilon: 1e-6, MaxSteps: 10_000_000}
}

// PersonalisedPushPageRank computes the personalised PageRank
// vector seeded at src using the local-push algorithm
// (Andersen-Chung-Lang, FOCS 2006). Returns the rank vector indexed
// by NodeID.
//
// The algorithm pays only for the edges it touches, so on large
// graphs with a small high-probability cluster it runs in roughly
// O(1/epsilon) time rather than O(V+E).
//
// Dangling-node handling matches the ACL paper: residue at a node
// with out-degree 0 is teleported back to src (probability alpha)
// rather than redistributed to non-existent neighbours. This keeps
// the rank vector summing to 1 within numerical tolerance.
//
// Concurrency: safe to invoke from any number of goroutines on a
// shared CSR.
func PersonalisedPushPageRank[W any](c *csr.CSR[W], src graph.NodeID, opts PPRPushOptions) ([]float64, error) {
	defer metrics.Time("search.centrality.PersonalisedPushPageRank")()
	res, err := PersonalisedPushPageRankCtx(context.Background(), c, src, opts)
	if err != nil {
		metrics.IncCounter("search.centrality.PersonalisedPushPageRank.errors", 1)
	}
	return res, err
}

// PersonalisedPushPageRankCtx is the context-aware variant of
// [PersonalisedPushPageRank]. ctx.Err() is checked every 4096 worklist
// pops; on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical ACL push: defaults + worklist loop + dangling teleport
func PersonalisedPushPageRankCtx[W any](ctx context.Context, c *csr.CSR[W], src graph.NodeID, opts PPRPushOptions) ([]float64, error) {
	defer metrics.Time("search.centrality.PersonalisedPushPageRankCtx")()
	if hasInvalidFloat(opts.Damping, opts.Epsilon) {
		metrics.IncCounter("search.centrality.PersonalisedPushPageRankCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	// Zero is the Go zero-value sentinel meaning "use the default".
	// Only explicitly out-of-range values are rejected.
	if opts.Damping != 0 && (opts.Damping <= 0 || opts.Damping >= 1) {
		metrics.IncCounter("search.centrality.PersonalisedPushPageRankCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	if opts.Epsilon < 0 {
		metrics.IncCounter("search.centrality.PersonalisedPushPageRankCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	if opts.Damping == 0 {
		opts.Damping = 0.85
	}
	if opts.Epsilon == 0 {
		opts.Epsilon = 1e-6
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 10_000_000
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	if n <= 0 || uint64(src)+1 >= uint64(len(verts)) {
		return nil, nil
	}
	rank := make([]float64, n)
	res := make([]float64, n)
	res[uint64(src)] = 1
	queue := []int{int(src)}
	inQ := make([]bool, n)
	inQ[uint64(src)] = true

	enqueueIfHot := func(node int) {
		if inQ[node] {
			return
		}
		deg := verts[node+1] - verts[node]
		var hot bool
		if deg == 0 {
			// Dangling: any residue above epsilon is "hot" — its
			// mass will be teleported to src on the next pop.
			hot = res[node] >= opts.Epsilon
		} else {
			hot = res[node]/float64(deg) >= opts.Epsilon
		}
		if hot {
			queue = append(queue, node)
			inQ[node] = true
		}
	}

	steps := 0
	// qh is the read cursor into queue; the [0:qh) prefix has already
	// been consumed. The slice is append-only between compactions: a
	// node re-enters via enqueueIfHot whenever it re-activates, so the
	// consumed prefix is dead memory. compactWorklist reclaims it once
	// the prefix dominates, keeping cap(queue) tracking the live frontier
	// rather than growing toward MaxSteps total pushes (see the helper).
	qh := 0
	for qh < len(queue) && steps < opts.MaxSteps {
		if steps&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.centrality.PersonalisedPushPageRankCtx.errors", 1)
				return nil, err
			}
		}
		queue, qh = compactWorklist(queue, qh)
		if pprWorklistObserver != nil {
			pprWorklistObserver(len(queue), cap(queue))
		}
		v := queue[qh]
		qh++
		inQ[v] = false
		rv := res[v]
		deg := int(verts[v+1] - verts[v])
		if deg == 0 {
			// Dangling node: absorb (1-alpha)*rv into rank, teleport
			// alpha*rv back to src per ACL.
			rank[v] += (1 - opts.Damping) * rv
			res[v] = 0
			res[uint64(src)] += opts.Damping * rv
			enqueueIfHot(int(src))
			steps++
			continue
		}
		// Threshold check (no +1 hack: deg > 0 here).
		if rv/float64(deg) < opts.Epsilon {
			continue
		}
		rank[v] += (1 - opts.Damping) * rv
		share := opts.Damping * rv / float64(deg)
		res[v] = 0
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			res[w] += share
			enqueueIfHot(w)
		}
		steps++
	}
	// Note: no final residue-drain pass. The canonical PPR invariant
	// is that rank[i] + alpha-weighted residue accumulates the true
	// stationary mass within Epsilon. Folding the residue with a
	// (1-alpha) factor (as an earlier implementation did) double-counted the absorption
	// and biased the rank vector. Leaving residue in place keeps
	// rank monotonically convergent.
	//
	// Signal budget exhaustion when the worklist is not fully drained.
	// The caller receives the partial rank accumulated so far together
	// with ErrMaxStepsExceeded so it can decide whether to use or
	// discard the approximate result.
	if steps >= opts.MaxSteps && qh < len(queue) {
		metrics.IncCounter("search.centrality.PersonalisedPushPageRankCtx.errors", 1)
		return rank, ErrMaxStepsExceeded
	}
	return rank, nil
}

// pprCompactFloor is the minimum worklist length below which
// compaction is skipped. Reclaiming a handful of consumed entries is
// not worth the copy; the win only matters once the consumed prefix is
// large. Kept small so dense graphs compact promptly.
const pprCompactFloor = 64

// compactWorklist reclaims the consumed prefix of a push worklist.
//
// The PPR push loop consumes queue front-to-back via the read cursor qh
// while enqueueIfHot appends re-activated nodes to the back. Left alone,
// the backing array grows toward the total push count (MaxSteps),
// retaining tens of megabytes of dead, already-consumed entries for the
// duration of a single call. compactWorklist drops that dead prefix once
// it dominates the slice, so cap(queue) tracks the live frontier
// (length - qh) instead.
//
// It compacts only when the consumed prefix is at least half the current
// length and the length clears pprCompactFloor; this bounds the amortised
// copy cost to O(1) per consumed element while keeping the retained
// capacity within a constant factor of the live frontier.
//
// The relative order of the unconsumed elements [qh:len) is preserved
// exactly (a left shift), so FIFO consumption order — and therefore the
// computed rank vector — is byte-for-byte identical to the non-compacting
// loop. The returned cursor is the position of the first unconsumed
// element in the rewritten slice.
//
// inQ membership is keyed by NodeID, not by queue position, so no index
// remapping is required after the shift.
func compactWorklist(queue []int, qh int) ([]int, int) {
	if qh <= len(queue)/2 || len(queue) < pprCompactFloor {
		return queue, qh
	}
	// Shift the unconsumed tail to the front of the same backing array
	// and truncate. cap is retained, but length now reflects only the
	// live frontier, so subsequent appends reuse the reclaimed space
	// instead of growing the array.
	n := copy(queue, queue[qh:])
	return queue[:n], 0
}

// pprWorklistObserver is a test-only seam: when non-nil it is invoked at
// the top of every worklist iteration with the current (len, cap) of the
// push worklist, after any compaction. Production code leaves it nil, so
// the hot loop pays only a single nil-pointer comparison per pop. Tests
// install it to assert that the worklist capacity tracks the live
// frontier rather than the total push count.
var pprWorklistObserver func(qlen, qcap int)
