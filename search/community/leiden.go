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

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
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
		// Phase 3: aggregation.
		nextG, nextComm := g.aggregate(comm, refined)
		nextQ := nextG.modularity(nextComm, opts.Resolution)
		if !moved && nextQ <= prevQ {
			break
		}
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
	sigmaIn := make([]float64, cMax)
	sigmaTot := make([]float64, cMax)
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
	sigmaTot := make([]float64, cMax)
	for v := 0; v < g.n; v++ {
		sigmaTot[comm[v]] += g.deg[v]
	}
	anyMoved := false
	// Scratch buffers for per-vertex community-weight accumulation:
	// kvcArr is zero-initialised and stays at zero between vertices
	// (reset via touched-list scan); touched records the indices that
	// have been written so the reset is O(unique-neighbour-communities)
	// rather than O(cMax).
	kvcArr := make([]float64, cMax)
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
	sigmaTot := make([]float64, g.n) // indexed by refined-community ID, initially == node ID
	for v := 0; v < g.n; v++ {
		sigmaTot[v] += g.deg[v]
	}
	// Scratch buffers (see localMove for the touched-list rationale).
	kvcArr := make([]float64, g.n)
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

// aggregate builds a new aggGraph in which each refined community is
// a single super-node. The returned comm slice projects the outer
// (non-refined) partition onto the super-nodes.
//
//nolint:gocyclo // canonical aggregation: refined-community renumber + super-edge accumulation + lifted projection
func (g *aggGraph) aggregate(parent, refined []int) (newG *aggGraph, newComm []int) {
	// Renumber refined communities into [0, nNew).
	remap := map[int]int{}
	next := 0
	for v := 0; v < g.n; v++ {
		r := refined[v]
		if _, ok := remap[r]; !ok {
			remap[r] = next
			next++
		}
	}
	nNew := next

	// New aggGraph: nodes = refined communities; edges accumulated by
	// summing weights between distinct refined communities; self-loops
	// accumulate internal weight.
	type edgeKey struct{ a, b int }
	weight := map[edgeKey]float64{}
	deg := make([]float64, nNew)
	loop := make([]float64, nNew)
	// Invariant maintained at every level: deg[v] = sum(g.weights for
	// non-self edges from v) + g.loop[v]. The previous level's loop
	// represents already-contracted internal mass; it contributes to
	// the new node's degree exactly once (already CSR-doubled).
	for v := 0; v < g.n; v++ {
		a := remap[refined[v]]
		loop[a] += g.loop[v]
		deg[a] += g.loop[v] // self-loop's contribution to degree
		for k := g.verts[v]; k < g.verts[v+1]; k++ {
			u := g.edges[k]
			b := remap[refined[u]]
			deg[a] += g.weights[k]
			if a == b {
				loop[a] += g.weights[k]
				continue
			}
			key := edgeKey{a, b}
			weight[key] += g.weights[k]
		}
	}
	// Each non-self edge was counted twice (a->b and b->a) — that's
	// the CSR doubling convention; we keep it. Build verts/edges.
	verts := make([]int, nNew+1)
	for key := range weight {
		verts[key.a+1]++
	}
	for i := 1; i <= nNew; i++ {
		verts[i] += verts[i-1]
	}
	edges := make([]int, verts[nNew])
	weights := make([]float64, verts[nNew])
	cursor := make([]int, nNew)
	for key, w := range weight {
		off := verts[key.a] + cursor[key.a]
		edges[off] = key.b
		weights[off] = w
		cursor[key.a]++
	}
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

	// Lift the level-0 compact NodeID -> current node mapping.
	newLifted := make([]int, len(g.lifted))
	for level0 := 0; level0 < len(g.lifted); level0++ {
		old := g.lifted[level0]
		newLifted[level0] = remap[refined[old]]
	}

	// Renumber newParent to [0, kOuter): preserves first-occurrence order.
	pRemap := map[int]int{}
	pNext := 0
	for _, p := range newParent {
		if _, ok := pRemap[p]; !ok {
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
