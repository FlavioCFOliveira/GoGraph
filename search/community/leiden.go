// Package community implements community detection algorithms for
// undirected graphs.
//
// v1 includes:
//   - [Leiden] — Traag-Waltman-van Eck 2019, modularity-optimising
//     community detection with the modularity-and-connectivity
//     guarantees the canonical Louvain algorithm lacks.
//   - [LabelPropagation] — Raghavan-Albert-Kumara 2007, the
//     near-linear-time simple counterpart.
package community

import (
	"context"
	"runtime"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// LeidenOptions configures [Leiden].
type LeidenOptions struct {
	// MaxIterations bounds the number of local-moving sweeps per pass.
	MaxIterations int
	// MaxPasses bounds the number of Leiden passes (local-move +
	// refine + aggregate) before the algorithm stops.
	MaxPasses int
	// Resolution scales the modularity expectation term (typically 1.0).
	// Larger values bias toward smaller communities; smaller values
	// toward fewer larger communities.
	Resolution float64
}

// DefaultLeidenOptions returns the default parameters.
func DefaultLeidenOptions() LeidenOptions {
	return LeidenOptions{MaxIterations: 64, MaxPasses: 16, Resolution: 1.0}
}

// Partition is the result of a community-detection run.
//
// Community is a NodeID-indexed slice of length MaxNodeID(): for each
// live NodeID it holds a community ID in [0, NumCommunities); for ghost
// NodeID slots (created by sharded packing on small graphs) it holds
// the sentinel value -1. NumCommunities counts only live communities.
type Partition struct {
	Community      []int
	NumCommunities int
}

// Leiden runs the Traag-Waltman-van Eck Leiden community-detection
// algorithm on the undirected graph c.
//
// Three phases per pass:
//  1. Local moving — each node greedily moves to the community that
//     maximises the modularity gain ΔQ (Newman's formula).
//  2. Refinement — within each community, restart with singletons and
//     allow moves only inside the parent community, producing a
//     well-separated refined partition. This is the Leiden-vs-Louvain
//     guarantee that no community is internally poorly connected.
//  3. Aggregation — contract each refined sub-community into a single
//     super-node; iterate on the smaller graph.
//
// Repeats passes until modularity no longer improves or MaxPasses is
// reached. A final splitDisconnected post-pass guarantees every
// returned community is internally connected.
//
// Only live NodeIDs (those with at least one incident edge) are
// assigned to a community; ghost slots receive the sentinel -1.
//
// Concurrency: safe to invoke from any number of goroutines on a
// shared CSR.
//
// # Determinism
//
// Leiden's local-move and refinement phases visit nodes in NodeID
// order — the inner loops iterate over [0, MaxNodeID) without
// randomisation. The tie-breaking rule for ΔQ comparisons is "first
// candidate community wins": when two destination communities yield
// equal modularity gain, the algorithm keeps the community with the
// lower NodeID anchor. This makes Leiden bit-for-bit reproducible
// across runs on the same input, on the same machine, with the same
// Go toolchain, provided opts is unchanged.
//
// Two callers running Leiden on the same csr.CSR snapshot in
// parallel are guaranteed to produce the same Partition (modulo
// goroutine scheduling on the sequential algorithm — there is no
// internal goroutine pool). Across different machines the result
// is still deterministic as long as the floating-point
// implementation is IEEE-754 compliant; the small floating-point
// rounding deltas between architectures do not cross the
// equal-modularity tie-breaking threshold for typical inputs but
// pathological cases at exact equality may diverge.
func Leiden[W any](c *csr.CSR[W], opts LeidenOptions) Partition {
	defer metrics.Time("search.community.Leiden")()
	out, _ := LeidenCtx(context.Background(), c, opts)
	return out
}

// LeidenCtx is the context-aware variant of [Leiden]. ctx.Err() is
// checked at every pass boundary; on cancellation returns (zero
// Partition, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical Leiden: defaults + pass loop dispatches three phases through helpers
func LeidenCtx[W any](ctx context.Context, c *csr.CSR[W], opts LeidenOptions) (Partition, error) {
	defer metrics.Time("search.community.LeidenCtx")()
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 64
	}
	if opts.MaxPasses <= 0 {
		opts.MaxPasses = 16
	}
	if opts.Resolution <= 0 {
		opts.Resolution = 1.0
	}
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return Partition{}, nil
	}
	mask := c.LiveMask()

	// Build the compact aggregation-graph view of c.
	g, idMap := aggGraphFromCSR(c, mask)
	if g.n == 0 {
		return Partition{Community: makeAllMinusOne(maxID), NumCommunities: 0}, nil
	}

	// Each live node starts in its own community.
	comm := make([]int, g.n)
	for i := range comm {
		comm[i] = i
	}

	prevQ := g.modularity(comm, opts.Resolution)
	// free holds aggGraph output buffer-sets retired from earlier levels,
	// reused to build later levels (see aggregate's pooling note and the
	// graphBufs type). Because aggregate reads only the current g and never
	// an older level, the buffer-set displaced by `g = nextG` below is
	// fully dead and safe to recycle on the next pass; the surviving g is
	// never pushed here, so the final projection's g stays valid.
	var free graphBufFreeList
	for pass := 0; pass < opts.MaxPasses; pass++ {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.community.LeidenCtx.errors", 1)
			return Partition{}, err
		}
		runtime.Gosched()
		// Phase 1: local moving in g.
		moved := g.localMove(comm, opts)
		// Phase 2: refinement.
		refined := g.refine(comm, opts)
		// Phase 3: aggregation, drawing the new level's persistent output
		// arrays from the free list when one is available.
		nextG, nextComm := g.aggregate(comm, refined, free.get())
		nextQ := nextG.modularity(nextComm, opts.Resolution)
		if !moved && nextQ <= prevQ {
			break
		}
		// g is now displaced and fully dead (nextG was built reading it,
		// and nothing else holds it). Recycle its output arrays.
		free.put(g.bufs())
		g = nextG
		comm = nextComm
		prevQ = nextQ
	}

	// Project the final super-graph partition back to the original
	// CSR NodeID space via idMap, building the result slice.
	out := makeAllMinusOne(maxID)
	for origID := 0; origID < maxID; origID++ {
		if !mask[origID] {
			continue
		}
		// Walk the chain of compact mappings: original -> level-0 idMap[origID]
		// -> ... -> final super-node. idMap is the inverse: idMap[level0] = origID.
		// We maintain the projection through a running per-pass mapping inside
		// aggregate(); after the loop, comm[finalNodeIdx] holds the partition
		// id, and the per-original-node mapping is stored on g.lifted.
		out[origID] = comm[g.lifted[idMap[origID]]]
	}

	// Compact final community IDs into [0, K).
	out, k := compactIDs(out)
	// Final connectivity guarantee.
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	out, k = splitDisconnectedPartition(out, mask, verts, edges, maxID, k)
	return Partition{Community: out, NumCommunities: k}, nil
}

// makeAllMinusOne returns a slice of length n filled with -1.
func makeAllMinusOne(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = -1
	}
	return out
}

// compactIDs renumbers community IDs into [0, K) preserving order of
// first occurrence and leaves -1 sentinels untouched.
func compactIDs(p []int) (community []int, k int) {
	remap := map[int]int{}
	next := 0
	for _, v := range p {
		if v < 0 {
			continue
		}
		if _, ok := remap[v]; !ok {
			remap[v] = next
			next++
		}
	}
	for i, v := range p {
		if v >= 0 {
			p[i] = remap[v]
		}
	}
	return p, next
}

// splitDisconnectedPartition is the Leiden post-pass that ensures
// every community is internally connected. Disjoint pieces of a
// nominally-single community are split into separate new IDs.
func splitDisconnectedPartition(comm []int, mask []bool, verts []uint64, edges []graph.NodeID, maxID, _ int) (out []int, k int) {
	visited := make([]bool, maxID)
	out = makeAllMinusOne(maxID)
	for start := 0; start < maxID; start++ {
		if !mask[start] || visited[start] {
			continue
		}
		cid := comm[start]
		id := k
		k++
		// BFS through the same-community connected component.
		queue := []int{start}
		visited[start] = true
		for qh := 0; qh < len(queue); qh++ {
			v := queue[qh]
			out[v] = id
			for ki := verts[v]; ki < verts[v+1]; ki++ {
				w := int(edges[ki])
				if !mask[w] || visited[w] || comm[w] != cid {
					continue
				}
				visited[w] = true
				queue = append(queue, w)
			}
		}
	}
	return out, k
}

// --- aggregation graph -----------------------------------------------
//
// aggGraph is a weighted compact representation used by Leiden's
// inner loop. Each Leiden pass turns the previous aggGraph into a
// smaller one whose nodes are refined communities.

type aggGraph struct {
	n       int       // number of nodes
	verts   []int     // length n+1, CSR-style offsets
	edges   []int     // adjacency (compact node IDs)
	weights []float64 // parallel to edges
	deg     []float64 // node degree (sum of incident weights, including self-loop *2)
	loop    []float64 // self-loop weight per node (carries internal mass after aggregation)
	m2      float64   // 2 * total edge weight (sum of deg)
	// lifted maps level-0 compact node ID -> current node ID at this aggregation level.
	// It is rewritten on each aggregate() call so that, at the end, lifted[origLevel0]
	// returns the index into comm[] for the original node.
	lifted []int
	// sigA, sigB are reusable float64 accumulator scratch for the per-level
	// modularity / localMove / refine passes (sigmaIn/sigmaTot/kvcArr). Those
	// methods run sequentially on a given g and never nest, so a single shared
	// pair, re-zeroed with growZero at each use, replaces their per-call
	// make([]float64, …) allocations.
	sigA, sigB floatBuf
}

// aggGraphFromCSR builds the initial aggGraph from c (using only live
// NodeIDs). idMap[level0Compact] returns the original CSR NodeID.
//
//nolint:gocyclo // two-pass CSR build + degree + identity-lift initialisation
func aggGraphFromCSR[W any](c *csr.CSR[W], mask []bool) (g *aggGraph, idMap []int) {
	maxID := int(c.MaxNodeID())
	compact := make([]int, maxID)
	level0Order := make([]int, 0, maxID)
	n := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = n
			level0Order = append(level0Order, i)
			n++
		} else {
			compact[i] = -1
		}
	}

	cVerts := c.VerticesSlice()
	cEdges := c.EdgesSlice()
	verts := make([]int, n+1)
	for src := 0; src < maxID; src++ {
		if !mask[src] {
			continue
		}
		s := compact[src]
		var count int
		for k := cVerts[src]; k < cVerts[src+1]; k++ {
			if mask[cEdges[k]] {
				count++
			}
		}
		verts[s+1] = count
	}
	for i := 1; i <= n; i++ {
		verts[i] += verts[i-1]
	}
	edges := make([]int, verts[n])
	weights := make([]float64, verts[n])
	cursor := make([]int, n)
	for src := 0; src < maxID; src++ {
		if !mask[src] {
			continue
		}
		s := compact[src]
		for k := cVerts[src]; k < cVerts[src+1]; k++ {
			dst := cEdges[k]
			if !mask[dst] {
				continue
			}
			d := compact[dst]
			off := verts[s] + cursor[s]
			edges[off] = d
			weights[off] = 1.0 // unweighted
			cursor[s]++
		}
	}
	deg := make([]float64, n)
	loop := make([]float64, n)
	var m2 float64
	for v := 0; v < n; v++ {
		for k := verts[v]; k < verts[v+1]; k++ {
			deg[v] += weights[k]
		}
		m2 += deg[v]
	}
	lifted := make([]int, n)
	for i := range lifted {
		lifted[i] = i
	}
	// idMap from level-0 compact -> original NodeID; level-0 compact is identity here.
	idMap = make([]int, maxID)
	for i := range idMap {
		idMap[i] = -1
	}
	for compactID, origID := range level0Order {
		idMap[origID] = compactID
	}
	g = &aggGraph{
		n:       n,
		verts:   verts,
		edges:   edges,
		weights: weights,
		deg:     deg,
		loop:    loop,
		m2:      m2,
		lifted:  lifted,
	}
	return g, idMap
}

// modularity computes Q for the given partition on this aggregation graph.
func (g *aggGraph) modularity(comm []int, resolution float64) float64 {
	if g.m2 == 0 {
		return 0
	}
	cMax := 0
	for _, c := range comm {
		if c+1 > cMax {
			cMax = c + 1
		}
	}
	sigmaIn := g.sigA.growZero(cMax)
	sigmaTot := g.sigB.growZero(cMax)
	for v := 0; v < g.n; v++ {
		c := comm[v]
		sigmaTot[c] += g.deg[v]
		sigmaIn[c] += g.loop[v]
		for k := g.verts[v]; k < g.verts[v+1]; k++ {
			u := g.edges[k]
			if comm[u] == c {
				sigmaIn[c] += g.weights[k]
			}
		}
	}
	var q float64
	for c := 0; c < cMax; c++ {
		q += sigmaIn[c]/g.m2 - resolution*(sigmaTot[c]/g.m2)*(sigmaTot[c]/g.m2)
	}
	return q
}

// localMove performs modularity-greedy local moving until no node
// improves or MaxIterations is reached. Returns true if at least one
// node moved.
//
//nolint:gocyclo // canonical Louvain inner loop: sigma maintenance + per-node best-community search
func (g *aggGraph) localMove(comm []int, opts LeidenOptions) bool {
	if g.m2 == 0 {
		return false
	}
	cMax := 0
	for _, c := range comm {
		if c+1 > cMax {
			cMax = c + 1
		}
	}
	sigmaTot := g.sigA.growZero(cMax)
	for v := 0; v < g.n; v++ {
		sigmaTot[comm[v]] += g.deg[v]
	}
	anyMoved := false
	// Scratch buffers for per-vertex community-weight accumulation:
	// kvcArr is zero-initialised and stays at zero between vertices
	// (reset via touched-list scan); touched records the indices that
	// have been written so the reset is O(unique-neighbour-communities)
	// rather than O(cMax).
	kvcArr := g.sigB.growZero(cMax)
	touched := make([]int, 0, 32)
	for iter := 0; iter < opts.MaxIterations; iter++ {
		moved := false
		for v := 0; v < g.n; v++ {
			cv := comm[v]
			touched = touched[:0]
			// k_v_to_c for every neighbour community.
			for k := g.verts[v]; k < g.verts[v+1]; k++ {
				u := g.edges[k]
				if u == v {
					continue
				}
				cu := comm[u]
				if kvcArr[cu] == 0 {
					touched = append(touched, cu)
				}
				kvcArr[cu] += g.weights[k]
			}
			// Pretend v is removed from cv for the candidate
			// evaluation: subtract its degree from sigmaTot[cv]. The
			// k_v,c values were collected with v's edges; v's own
			// degree contributes to deg[v] but not to k_v,c for c == cv
			// because we skipped self-loops.
			sigmaTot[cv] -= g.deg[v]
			bestC := cv
			bestDelta := 0.0
			// Hoist per-vertex invariants out of the inner candidate
			// loop. ΔQ for v joining community c is the Louvain formula:
			//   ΔQ = (2*k_v,c)/m2 - resolution * (2*deg[v]*sigmaTot[c]) / (m2^2)
			invM2 := 1.0 / g.m2
			twoInvM2 := 2 * invM2
			vFactor := opts.Resolution * 2 * g.deg[v] * invM2 * invM2
			for _, c := range touched {
				delta := twoInvM2*kvcArr[c] - vFactor*sigmaTot[c]
				if delta > bestDelta || (delta == bestDelta && c < bestC) {
					bestDelta = delta
					bestC = c
				}
			}
			// Reset touched entries to zero for the next vertex.
			for _, c := range touched {
				kvcArr[c] = 0
			}
			// Add v to bestC. If bestC == cv it's a no-op except for
			// restoring sigmaTot[cv].
			sigmaTot[bestC] += g.deg[v]
			if bestC != cv {
				comm[v] = bestC
				moved = true
				anyMoved = true
			}
		}
		if !moved {
			break
		}
	}
	return anyMoved
}

// refine implements Leiden's refinement phase: within each community
// in the input partition, restart with singletons and run a restricted
// local move where each node may join only neighbour communities that
// are subsets of its parent community. Produces a refined partition
// in fresh community IDs; returned slice is comm-indexed.
//
//nolint:gocyclo // canonical Leiden refinement: per-parent singleton restart with restricted moves
func (g *aggGraph) refine(parent []int, opts LeidenOptions) []int {
	if g.m2 == 0 {
		return parent
	}
	refined := make([]int, g.n)
	for i := range refined {
		refined[i] = i // each node starts in its own refined community
	}
	cMax := 0
	for _, c := range parent {
		if c+1 > cMax {
			cMax = c + 1
		}
	}
	sigmaTot := g.sigA.growZero(g.n) // indexed by refined-community ID, initially == node ID
	for v := 0; v < g.n; v++ {
		sigmaTot[v] += g.deg[v]
	}
	// Scratch buffers (see localMove for the touched-list rationale).
	kvcArr := g.sigB.growZero(g.n)
	touched := make([]int, 0, 32)
	for iter := 0; iter < opts.MaxIterations; iter++ {
		moved := false
		for v := 0; v < g.n; v++ {
			parentV := parent[v]
			cv := refined[v]
			touched = touched[:0]
			for k := g.verts[v]; k < g.verts[v+1]; k++ {
				u := g.edges[k]
				if u == v {
					continue
				}
				if parent[u] != parentV {
					continue
				}
				ru := refined[u]
				if kvcArr[ru] == 0 {
					touched = append(touched, ru)
				}
				kvcArr[ru] += g.weights[k]
			}
			sigmaTot[cv] -= g.deg[v]
			bestC := cv
			bestDelta := 0.0
			invM2 := 1.0 / g.m2
			twoInvM2 := 2 * invM2
			vFactor := opts.Resolution * 2 * g.deg[v] * invM2 * invM2
			for _, c := range touched {
				delta := twoInvM2*kvcArr[c] - vFactor*sigmaTot[c]
				if delta > bestDelta || (delta == bestDelta && c < bestC) {
					bestDelta = delta
					bestC = c
				}
			}
			for _, c := range touched {
				kvcArr[c] = 0
			}
			sigmaTot[bestC] += g.deg[v]
			if bestC != cv {
				refined[v] = bestC
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	return refined
}

// graphBufs holds the persistent output arrays of one aggGraph level so
// they can be recycled into a later level. The arrays escape into the
// returned aggGraph, so they cannot live in the per-call aggScratch pool;
// instead LeidenCtx threads a small free list of retired buffer-sets
// (graphBufFreeList) through the pass loop. Reuse is sound because
// aggregate's read-dependency depth is exactly 1: it reads only the
// current level g, so once `g = nextG` displaces g, g's buffers are dead.
type graphBufs struct {
	verts   []int
	edges   []int
	weights []float64
	deg     []float64
	loop    []float64
	lifted  []int
}

// bufs packages g's persistent output arrays for recycling. Used by
// LeidenCtx to return a displaced (dead) level's arrays to the free list.
func (g *aggGraph) bufs() *graphBufs {
	return &graphBufs{
		verts:   g.verts,
		edges:   g.edges,
		weights: g.weights,
		deg:     g.deg,
		loop:    g.loop,
		lifted:  g.lifted,
	}
}

// graphBufFreeList is a tiny LIFO of retired graphBufs. At most one entry
// is ever held during the Leiden pass loop (one level is displaced per
// pass and consumed by the next), but the slice form keeps put/get total
// and makes the "empty -> fresh allocation" path explicit.
type graphBufFreeList struct {
	bufs []*graphBufs
}

// get returns a recycled buffer-set, or nil when none is available (the
// caller then allocates fresh).
func (f *graphBufFreeList) get() *graphBufs {
	n := len(f.bufs)
	if n == 0 {
		return nil
	}
	b := f.bufs[n-1]
	f.bufs = f.bufs[:n-1]
	return b
}

// put returns a dead buffer-set for later reuse.
func (f *graphBufFreeList) put(b *graphBufs) { f.bufs = append(f.bufs, b) }

// intBuf is a reusable, capacity-growing []int scratch slot.
type intBuf []int

// grow returns the buffer resliced to length n, reusing the backing array
// when its capacity already suffices and reallocating (growing) otherwise.
// The returned contents are not zeroed; callers that need zeros use
// growZero.
func (b *intBuf) grow(n int) []int {
	if cap(*b) < n {
		*b = make([]int, n)
		return *b
	}
	*b = (*b)[:n]
	return *b
}

// growZero is grow with the returned slice zeroed.
func (b *intBuf) growZero(n int) []int {
	s := b.grow(n)
	for i := range s {
		s[i] = 0
	}
	return s
}

// floatBuf is a reusable, capacity-growing []float64 scratch slot.
type floatBuf []float64

func (b *floatBuf) grow(n int) []float64 {
	if cap(*b) < n {
		*b = make([]float64, n)
		return *b
	}
	*b = (*b)[:n]
	return *b
}

func (b *floatBuf) growZero(n int) []float64 {
	s := b.grow(n)
	for i := range s {
		s[i] = 0
	}
	return s
}

// growReuseInts reslices src to length n, reusing its backing array when
// capacity suffices and reallocating otherwise. Used for the recyclable
// persistent output arrays of aggGraph (see graphBufs). zero requests the
// returned slice be zeroed; callers that fully overwrite skip it.
func growReuseInts(src []int, n int, zero bool) []int {
	var s []int
	if cap(src) < n {
		s = make([]int, n)
		return s // make already zeroes
	}
	s = src[:n]
	if zero {
		for i := range s {
			s[i] = 0
		}
	}
	return s
}

// growReuseFloats is growReuseInts for []float64.
func growReuseFloats(src []float64, n int, zero bool) []float64 {
	var s []float64
	if cap(src) < n {
		s = make([]float64, n)
		return s
	}
	s = src[:n]
	if zero {
		for i := range s {
			s[i] = 0
		}
	}
	return s
}

// aggScratch bundles the transient working buffers of one aggregate call.
// All fields are pure scratch (dead once aggregate returns) and are
// reused across the aggregation levels of a Leiden call via aggScratchPool.
// The buffers grow monotonically to the largest level's size, so the pool
// amortises the per-level allocations that previously dominated Leiden's
// profile.
type aggScratch struct {
	remap    intBuf   // refined-community -> [0,nNew) relabel
	pRemap   intBuf   // parent-community -> [0,kOuter) relabel
	srcStart intBuf   // counting-sort per-source offsets
	fill     intBuf   // per-source scatter cursor
	ordDst   intBuf   // counting-sorted targets
	ordW     floatBuf // counting-sorted weights
	acc      floatBuf // per-source coalesce accumulator
	touched  []int    // first-emission target order within a source
}

var aggScratchPool = sync.Pool{New: func() any { return &aggScratch{} }}

func acquireAggScratch() *aggScratch { return aggScratchPool.Get().(*aggScratch) }

func releaseAggScratch(sc *aggScratch) { aggScratchPool.Put(sc) }

// aggregate builds a new aggGraph in which each refined community is
// a single super-node. The returned comm slice projects the outer
// (non-refined) partition onto the super-nodes.
//
// # Allocation strategy
//
// The super-edge accumulation avoids the map[edgeKey]float64 that an
// earlier implementation allocated per aggregation level (it dominated
// Leiden's allocation profile). Instead it uses the CSR-build idiom:
// inter-community edges are emitted as (source, target, weight) triples
// in vertex-ascending, CSR-edge order, counting-sorted by source into
// contiguous per-source runs, then coalesced — equal targets within a
// run are summed using a dense accumulator and a touched-list (the same
// O(degree)-reset pattern localMove uses). The community-renumber maps
// (refined → [0,nNew); parent → [0,kOuter)) are likewise replaced by
// dense []int relabel slices, since both source spaces are already dense
// in [0, g.n).
//
// # Determinism
//
// Each super-edge's float64 weight is summed in the exact vertex-ascending
// CSR-edge order the map version used (the counting sort and the
// touched-list coalesce are both stable in emission order), so the per
// (a,b) sum is bit-identical. The resulting adjacency is ordered by
// first-emission within each source — a fixed deterministic order, unlike
// the map version's iteration-order-dependent layout. Because Leiden's
// edge weights are exact integers (every aggGraph edge starts at 1.0 and
// only ever accumulates integer sums), the downstream modularity and
// local-move/refine results are invariant to this within-source ordering;
// see the determinism note on [LeidenCtx]. A future weighted-Leiden
// variant with non-integer weights would make the chosen order
// load-bearing, which this canonical (first-emission) order already pins.
//
//nolint:gocyclo // canonical aggregation: dense renumber + counting-sort super-edge coalesce + lifted projection
func (g *aggGraph) aggregate(parent, refined []int, out *graphBufs) (newG *aggGraph, newComm []int) {
	// Transient scratch (relabel maps, counting-sort scatter buffers,
	// coalesce accumulator) is drawn from a pool and reused across every
	// aggregation level of this Leiden call — the buffers are dead once
	// aggregate returns, so pooling them removes the per-level allocations
	// that dominated the profile.
	//
	// The persistent outputs (verts, edges, weights, deg, loop, lifted)
	// escape into the returned graph, so they cannot use the per-call
	// scratch pool. When the caller passes a recycled buffer-set (out !=
	// nil, drawn from a displaced earlier level — see graphBufs), they are
	// grown-or-reused from it; otherwise they are freshly allocated. newComm
	// (the projected parent partition) is always freshly allocated and never
	// pooled. out must NOT be the buffer-set of the current g (the caller
	// guarantees this: it only recycles a level displaced two passes ago,
	// never the live g aggregate is reading).
	if out == nil {
		out = &graphBufs{}
	}
	sc := acquireAggScratch()
	defer releaseAggScratch(sc)

	// Renumber refined communities into [0, nNew) via a dense relabel
	// slice (refined ids live in [0, g.n)). First-occurrence order over v
	// ascending is identical to the previous map-based remap.
	remap := sc.remap.grow(g.n)
	for i := range remap {
		remap[i] = -1
	}
	nNew := 0
	for v := 0; v < g.n; v++ {
		r := refined[v]
		if remap[r] < 0 {
			remap[r] = nNew
			nNew++
		}
	}

	deg := growReuseFloats(out.deg, nNew, true)
	loop := growReuseFloats(out.loop, nNew, true)
	// Counting-sort the inter-community super-edges by source into
	// per-source contiguous runs, scattering directly from g (no
	// intermediate triple buffers). Pass 1 counts inter-community edges per
	// source and accumulates deg/loop; pass 2 scatters (target, weight)
	// into each source's run in vertex-ascending CSR order — the order the
	// float sums must follow. Invariant maintained at every level:
	// deg[v] = sum(g.weights for non-self edges from v) + g.loop[v]. The
	// previous level's loop is already-contracted internal mass and
	// contributes to the new node's degree exactly once (already
	// CSR-doubled).
	srcStart := sc.srcStart.growZero(nNew + 1)
	for v := 0; v < g.n; v++ {
		a := remap[refined[v]]
		loop[a] += g.loop[v]
		deg[a] += g.loop[v] // self-loop's contribution to degree
		for k := g.verts[v]; k < g.verts[v+1]; k++ {
			b := remap[refined[g.edges[k]]]
			w := g.weights[k]
			deg[a] += w
			if a == b {
				loop[a] += w
				continue
			}
			srcStart[a+1]++
		}
	}
	for i := 1; i <= nNew; i++ {
		srcStart[i] += srcStart[i-1]
	}
	nTri := srcStart[nNew]
	ordDst := sc.ordDst.grow(nTri)
	ordW := sc.ordW.grow(nTri)
	fill := sc.fill.growZero(nNew)
	for v := 0; v < g.n; v++ {
		a := remap[refined[v]]
		for k := g.verts[v]; k < g.verts[v+1]; k++ {
			b := remap[refined[g.edges[k]]]
			if a == b {
				continue
			}
			pos := srcStart[a] + fill[a]
			ordDst[pos] = b
			ordW[pos] = g.weights[k]
			fill[a]++
		}
	}

	// Coalesce equal targets within each source run using a dense
	// accumulator reset via a touched-list (O(distinct targets) per
	// source). touched records first-emission order so the output
	// adjacency is deterministic. acc holds the running float sum for the
	// currently-processed source's targets, in emission order.
	acc := sc.acc.growZero(nNew)
	touched := sc.touched[:0]
	// verts[0] must be 0; verts[a+1] is overwritten for every a below, so
	// only index 0 relies on zeroing — request a full zero for safety.
	verts := growReuseInts(out.verts, nNew+1, true)
	// Pass A: count distinct targets per source to size edges/weights.
	for a := 0; a < nNew; a++ {
		lo, hi := srcStart[a], srcStart[a+1]
		touched = touched[:0]
		for p := lo; p < hi; p++ {
			b := ordDst[p]
			if acc[b] == 0 {
				touched = append(touched, b)
			}
			acc[b] += ordW[p]
		}
		verts[a+1] = len(touched)
		for _, b := range touched {
			acc[b] = 0
		}
	}
	for i := 1; i <= nNew; i++ {
		verts[i] += verts[i-1]
	}
	// edges/weights are fully overwritten in Pass B (verts[nNew] slots, one
	// per distinct super-edge), so they need no zeroing on reuse.
	edges := growReuseInts(out.edges, verts[nNew], false)
	weights := growReuseFloats(out.weights, verts[nNew], false)
	// Pass B: coalesce and emit each source's distinct targets in
	// first-emission order, summing weights in emission order.
	for a := 0; a < nNew; a++ {
		lo, hi := srcStart[a], srcStart[a+1]
		touched = touched[:0]
		for p := lo; p < hi; p++ {
			b := ordDst[p]
			if acc[b] == 0 {
				touched = append(touched, b)
			}
			acc[b] += ordW[p]
		}
		off := verts[a]
		for _, b := range touched {
			edges[off] = b
			weights[off] = acc[b]
			off++
			acc[b] = 0
		}
	}
	sc.touched = touched // retain grown capacity for reuse next level

	var m2 float64
	for _, d := range deg {
		m2 += d
	}

	// Project the outer (non-refined) partition: each super-node
	// inherits parent[v] from one of its underlying v's (all members
	// share the same parent by construction).
	newParent := make([]int, nNew)
	seen := make([]bool, nNew)
	for v := 0; v < g.n; v++ {
		a := remap[refined[v]]
		if !seen[a] {
			newParent[a] = parent[v]
			seen[a] = true
		}
	}

	// Lift the level-0 compact NodeID -> current node mapping. Fully
	// overwritten, so no zeroing on reuse. out.lifted is a displaced
	// earlier level's array, distinct from g.lifted (the caller never
	// recycles the live g), so reading g.lifted while writing newLifted is
	// safe. len(g.lifted) is the constant level-0 node count at every level.
	newLifted := growReuseInts(out.lifted, len(g.lifted), false)
	for level0 := 0; level0 < len(g.lifted); level0++ {
		old := g.lifted[level0]
		newLifted[level0] = remap[refined[old]]
	}

	// Renumber newParent to [0, kOuter) via a dense relabel slice,
	// preserving first-occurrence order. Parent ids are community ids of a
	// partition over g.n nodes, hence in [0, g.n).
	pRemap := sc.pRemap.grow(g.n)
	for i := range pRemap {
		pRemap[i] = -1
	}
	pNext := 0
	for _, p := range newParent {
		if pRemap[p] < 0 {
			pRemap[p] = pNext
			pNext++
		}
	}
	for i, p := range newParent {
		newParent[i] = pRemap[p]
	}

	return &aggGraph{
		n:       nNew,
		verts:   verts,
		edges:   edges,
		weights: weights,
		deg:     deg,
		loop:    loop,
		m2:      m2,
		lifted:  newLifted,
	}, newParent
}
