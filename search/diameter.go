package search

import (
	"context"
	"math"
	"runtime"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Diameter returns the exact diameter of one connected component of c —
// the longest shortest-path distance between two of its vertices — as
// (lo, hi, exact) with lo == hi and exact == true.
//
// The component measured is the one containing the seed: the lowest
// NodeID with at least one incident edge. On a disconnected graph the
// result is that component's diameter, not the maximum over all
// components. A graph with no edges (or no vertices) returns (0, 0, true).
//
// The algorithm is BoundingDiameters: every BFS sweep bounds the
// eccentricity of every remaining candidate vertex from below and above,
// and a vertex leaves the candidate set once its bounds prove it cannot
// raise the diameter. The run ends when no candidate is left, and the
// result is exact on every input.
//
// Sources: F. W. Takes and W. A. Kosters, "Determining the Diameter of
// Small World Networks", CIKM 2011, doi:10.1145/2063576.2063748; the
// bounding and pruning rules follow NetworkX's _extrema_bounding
// (BSD-licensed), reimplemented here. No teexGraph code is copied.
//
// Cost: each sweep is one BFS, O(n + m). Real-world (small-world) graphs
// need few sweeps: 1717 on a 100k-vertex replica of example 11's social
// graph, and 746 on its 20k-vertex replica, where the previous iFUB walk
// made 14,603. Vertex-transitive shapes
// (cycles, complete graphs) need up to one sweep per vertex: a 600-vertex
// cycle takes 1997 µs against 659 µs for the previous iFUB walk. The worst
// case, O(n(n + m)), is unchanged.
//
// Batching: on a component of at least diameterParallelMinComponent (1024)
// vertices, sweeps run in batches of GOMAXPROCS. On the 100k-vertex
// replica (Apple M4, 10 cores) this measured about 0.61 s wall and 3.6 s
// CPU, against about 3.0 s wall and 3.0 s CPU with one sweep at a time:
// lower latency for about 18% more CPU. The result is identical for every
// batch size. The 1024-vertex threshold is not measured.
//
// Concurrency: Diameter is safe to invoke concurrently on a shared CSR,
// which it only reads. On a component of at least
// diameterParallelMinComponent vertices it runs up to GOMAXPROCS BFS
// sweeps at a time on worker goroutines it starts and joins before
// returning. c must encode an undirected graph (a symmetric directed CSR,
// as [csr.CSR.BuildSymmetric] produces).
func Diameter[W any](c *csr.CSR[W]) (lo, hi int, exact bool) {
	defer metrics.Time("search.Diameter").Stop()
	lo, hi, exact, _ = DiameterCtx(context.Background(), c)
	return lo, hi, exact
}

// DiameterCtx is the context-aware variant of [Diameter]. ctx.Err() is
// checked before the first sweep and before every batch of concurrent
// sweeps, so cancellation latency is one BFS sweep. On cancellation it
// returns ctx.Err() with exact == false and bounds lo <= D <= hi, where D
// is the diameter [Diameter] would return: lo is the largest eccentricity
// lower bound reached, and hi is min(largest candidate upper bound,
// 2 * smallest eccentricity measured), never below lo. Cancellation before
// the first sweep returns (0, 0, false, ctx.Err()).
func DiameterCtx[W any](ctx context.Context, c *csr.CSR[W]) (lo, hi int, exact bool, err error) {
	defer metrics.Time("search.DiameterCtx").Stop()
	lo, hi, exact, _, err = boundingDiameters(ctx, c, 0)
	if err != nil {
		metrics.IncCounter("search.DiameterCtx.errors", 1)
	}
	return lo, hi, exact, err
}

// diameterParallelMinComponent is the component order at or above which
// [DiameterCtx] runs its sweeps in batches across GOMAXPROCS workers.
// Below it a sweep is a few microseconds, and a batch's extra sweeps
// (sources chosen before their batch-mates' bounds are known) cost more
// than the parallelism returns.
const diameterParallelMinComponent = 1024

// diameterSlot is one sweep of a batch: its source, the eccentricity the
// sweep measured, and the (dist, queue) scratch pair [bfsFarthest] reuses
// across the sweeps this slot runs. Slot scratch is private: exactly one
// goroutine touches a slot during a batch.
type diameterSlot struct {
	dist  []int
	queue []graph.NodeID
	src   graph.NodeID
	ecc   int
}

// boundingDiameters implements [DiameterCtx]. workers > 0 fixes the batch
// size (the number of sweeps run concurrently); workers <= 0 selects
// GOMAXPROCS on a component of at least diameterParallelMinComponent
// vertices and 1 otherwise. The returned (lo, hi, exact) of an uncancelled
// run is the exact diameter for every batch size; only sweeps, the number
// of BFS sweeps run (the seed sweep included), depends on it.
//
// State, indexed by NodeID: lower[v] <= ecc(v) <= upper[v]. A sweep from s
// with eccentricity e and distances d gives, for every candidate v,
// lower[v] >= max(d(s,v), e-d(s,v)) and upper[v] <= e+d(s,v) (triangle
// inequality). maxL is the largest lower bound seen; it is a lower bound
// on D. A candidate v is removed when lower[v] == upper[v] (its
// eccentricity is known, hence <= maxL) or when upper[v] <= maxL and
// 2*lower[v] >= maxU, maxU being the largest candidate upper bound. Every
// removed vertex therefore has ecc <= maxL, so once no candidate is left
// D == maxL. Each sweep source s gets lower[s] == upper[s] == ecc(s) and
// is removed, so the run ends after at most |component| sweeps.
//
// Bounds are uint32: every distance is below the vertex count, and upper
// saturates at math.MaxUint32, which stays a valid upper bound while the
// component has fewer than 2^32 vertices.
func boundingDiameters[W any](ctx context.Context, c *csr.CSR[W], workers int) (lo, hi int, exact bool, sweeps int, err error) {
	n := int(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	// The seed is the lowest NodeID with an outgoing edge. A vertex with an
	// outgoing edge is always in c.LiveMask(), so the mask adds nothing to
	// this test and is not built.
	seed := -1
	for i := 0; i < n; i++ {
		if verts[i+1] > verts[i] {
			seed = i
			break
		}
	}
	if seed < 0 {
		return 0, 0, true, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, false, 0, err
	}

	// The seed sweep defines the component (its visited set, which the
	// returned queue lists exactly) and is the first bounding sweep.
	seedSlot := diameterSlot{dist: newBFSDist(n), queue: make([]graph.NodeID, 0, n), src: graph.NodeID(seed)}
	_, seedSlot.ecc, _, seedSlot.queue = bfsFarthest(verts, edges, seedSlot.src, seedSlot.dist, seedSlot.queue)
	sweeps = 1

	b := diameterBounds{
		cand:   append(make([]graph.NodeID, 0, len(seedSlot.queue)), seedSlot.queue...),
		lower:  make([]uint32, n),
		upper:  make([]uint32, n),
		minEcc: math.MaxUint32,
	}
	for _, v := range b.cand {
		b.upper[v] = math.MaxUint32
	}
	b.apply(seedSlot.dist, seedSlot.ecc)
	b.prune()

	if workers <= 0 {
		workers = 1
		if len(b.cand) >= diameterParallelMinComponent {
			workers = runtime.GOMAXPROCS(0)
		}
	}
	// One slot per worker; slot 0 keeps the seed sweep's scratch. The
	// capacity is final, so the backing array never moves while workers
	// hold the slice.
	slots := make([]diameterSlot, 1, workers)
	slots[0] = seedSlot

	// Worker pool for batches of more than one sweep: workers-1 goroutines,
	// the calling goroutine running slot 0. Started on the first batch that
	// needs it and joined before return.
	var (
		jobs chan int
		done chan struct{}
		wg   sync.WaitGroup
	)
	defer func() {
		if jobs != nil {
			close(jobs)
			wg.Wait()
		}
	}()

	picker := diameterPicker{verts: verts, first: true}
	for len(b.cand) > 0 {
		if err := ctx.Err(); err != nil {
			return b.cancelled(err, sweeps)
		}
		k := picker.pick(&b, workers)
		for len(slots) < k {
			slots = append(slots, diameterSlot{dist: newBFSDist(n), queue: make([]graph.NodeID, 0, n)})
		}
		slots = slots[:k]
		for i := range slots {
			slots[i].src = picker.picks[i]
		}
		if k > 1 && jobs == nil {
			jobs = make(chan int, workers)
			done = make(chan struct{}, workers)
			for w := 1; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range jobs {
						s := &slots[i]
						_, s.ecc, _, s.queue = bfsFarthest(verts, edges, s.src, s.dist, s.queue)
						done <- struct{}{}
					}
				}()
			}
		}
		for i := 1; i < k; i++ {
			jobs <- i
		}
		s0 := &slots[0]
		_, s0.ecc, _, s0.queue = bfsFarthest(verts, edges, s0.src, s0.dist, s0.queue)
		for i := 1; i < k; i++ {
			<-done
		}
		sweeps += k
		// Bound updates are min/max folds, so the order the batch's sweeps
		// are applied in does not change the state after the batch.
		for i := range slots {
			b.apply(slots[i].dist, slots[i].ecc)
		}
		b.prune()
	}
	d := int(b.maxL)
	return d, d, true, sweeps, nil
}

// diameterBounds is the per-call BoundingDiameters state of
// [boundingDiameters]. It is owned by the calling goroutine.
type diameterBounds struct {
	cand   []graph.NodeID // remaining candidates, in no particular order
	lower  []uint32       // eccentricity lower bound, by NodeID
	upper  []uint32       // eccentricity upper bound, by NodeID
	maxL   uint32         // largest lower bound seen: a lower bound on D
	maxU   uint32         // largest candidate upper bound at the last prune
	minEcc uint32         // smallest eccentricity measured: D <= 2*minEcc
}

// apply folds one sweep (distances dist, eccentricity ecc) into the bounds
// of every candidate. Every candidate is in the sweep's component, so
// dist[v] is in [0, ecc].
func (b *diameterBounds) apply(dist []int, ecc int) {
	e := uint32(ecc) // ecc < component order < 2^32 (see boundingDiameters)
	b.minEcc = min(b.minEcc, e)
	lower, upper := b.lower, b.upper
	maxL := b.maxL
	for _, v := range b.cand {
		d := uint32(dist[v]) // 0 <= dist[v] <= ecc
		l := max(d, e-d)
		if l > lower[v] {
			lower[v] = l
			maxL = max(maxL, l)
		}
		u := e + d
		if u < e { // saturate: still an upper bound, as ecc(v) < 2^32
			u = math.MaxUint32
		}
		if u < upper[v] {
			upper[v] = u
		}
	}
	b.maxL = maxL
}

// prune removes every candidate whose bounds prove it cannot raise the
// diameter above maxL: lower == upper, or upper <= maxL with
// 2*lower >= maxU.
func (b *diameterBounds) prune() {
	lower, upper := b.lower, b.upper
	var maxU uint32
	for _, v := range b.cand {
		maxU = max(maxU, upper[v])
	}
	b.maxU = maxU
	keep := b.cand[:0]
	for _, v := range b.cand {
		l, u := lower[v], upper[v]
		if l == u || (u <= b.maxL && 2*uint64(l) >= uint64(maxU)) {
			continue
		}
		keep = append(keep, v)
	}
	b.cand = keep
}

// cancelled returns the bounds reached when the run stops early. D <= maxU
// unless D == maxL (a removed vertex has ecc <= maxL), and D <= 2*ecc(x)
// for every x.
func (b *diameterBounds) cancelled(err error, sweeps int) (lo, hi int, exact bool, sweepsOut int, errOut error) {
	h := min(uint64(b.maxU), 2*uint64(b.minEcc))
	h = max(h, uint64(b.maxL))
	return int(b.maxL), int(h), false, sweeps, err
}

// diameterPicker selects the sources of each batch: the highest-degree
// candidate first, then alternately the candidate with the smallest lower
// bound and the one with the largest upper bound. Ties go to the higher
// degree, then to the lower NodeID, so the order is total and the run is
// deterministic.
type diameterPicker struct {
	verts []uint64
	picks []graph.NodeID
	first bool
	high  bool
}

func (p *diameterPicker) deg(v graph.NodeID) uint64 { return p.verts[v+1] - p.verts[v] }

// pick chooses up to k distinct candidates into p.picks and returns how
// many it chose. Each chosen candidate is swapped to the tail of b.cand,
// outside the range the next choice scans, so a batch never repeats a
// source; b.cand's order carries no meaning.
func (p *diameterPicker) pick(b *diameterBounds, k int) int {
	p.picks = p.picks[:0]
	r := len(b.cand)
	for len(p.picks) < k && r > 0 {
		best := 0
		for i := 1; i < r; i++ {
			if p.better(b, b.cand[i], b.cand[best]) {
				best = i
			}
		}
		p.picks = append(p.picks, b.cand[best])
		b.cand[best], b.cand[r-1] = b.cand[r-1], b.cand[best]
		r--
		if p.first {
			p.first = false
		} else {
			p.high = !p.high
		}
	}
	return len(p.picks)
}

// better reports whether candidate x precedes y under the current
// selection rule.
func (p *diameterPicker) better(b *diameterBounds, x, y graph.NodeID) bool {
	if !p.first {
		if p.high {
			if b.upper[x] != b.upper[y] {
				return b.upper[x] > b.upper[y]
			}
		} else if b.lower[x] != b.lower[y] {
			return b.lower[x] < b.lower[y]
		}
	}
	if dx, dy := p.deg(x), p.deg(y); dx != dy {
		return dx > dy
	}
	return x < y
}

// newBFSDist returns a distance scratch of length n in the state
// [bfsFarthest] requires on first use: every entry -1 (unvisited).
func newBFSDist(n int) []int {
	dist := make([]int, n)
	for i := range dist {
		dist[i] = -1
	}
	return dist
}

// bfsFarthest runs a single BFS from src and returns the farthest
// vertex (smallest NodeID on tie), its distance from src — the
// eccentricity of src within its component — and the per-vertex distance
// slice (-1 for vertices src does not reach).
//
// dist and queue are caller-provided scratch that travel as a PAIR: on
// entry, dist must be -1 everywhere except at the vertices listed in queue
// (the previous sweep's visited set). The sweep resets only those vertices,
// O(visited) instead of O(V), so a fresh pair is [newBFSDist] with an empty
// queue, and a caller that sweeps again must pass back the queue the
// previous sweep returned with the same dist. queue is returned as its
// (possibly grown) header so the caller keeps the capacity across sources —
// the same contract search/centrality.brandesSource uses for its queue and
// stack. Passing a queue with capacity len(dist) makes the traversal
// allocation-free, because a BFS enqueues each vertex at most once.
//
// Traversal order is that of a head-first FIFO in enqueue order, so
// (farthest, ecc, dist) are bit-identical to a sweep over a freshly reset
// dist.
func bfsFarthest(verts []uint64, edges []graph.NodeID, src graph.NodeID, dist []int, queue []graph.NodeID) (farthest graph.NodeID, ecc int, distOut []int, queueOut []graph.NodeID) {
	for _, v := range queue {
		dist[uint64(v)] = -1
	}
	dist[uint64(src)] = 0
	queue = append(queue[:0], src)
	farthest = src
	// BFS assigns distances in non-decreasing enqueue order, so a newly
	// reached vertex is never nearer than the current farthest: the local
	// farD replaces the two dist loads per discovery.
	farD := 0
	for qh := 0; qh < len(queue); qh++ {
		v := queue[qh]
		nd := dist[uint64(v)] + 1
		for k := verts[uint64(v)]; k < verts[uint64(v)+1]; k++ {
			nb := edges[k]
			if dist[uint64(nb)] >= 0 {
				continue
			}
			dist[uint64(nb)] = nd
			queue = append(queue, nb)
			if nd > farD || (nd == farD && uint64(nb) < uint64(farthest)) {
				farthest = nb
				farD = nd
			}
		}
	}
	return farthest, farD, dist, queue
}
