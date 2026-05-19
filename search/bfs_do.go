package search

import (
	"sync"

	"gograph/graph"
	"gograph/graph/csr"
)

// BFSDirectionOpt performs direction-optimising breadth-first
// search (Beamer, Asanovic, Patterson — SC 2012) over the
// symmetric adjacency captured by c. The algorithm dynamically
// switches between top-down (push, expand the current frontier
// forward) and bottom-up (pull, scan unvisited nodes and check
// whether any of their neighbours are in the frontier) phases
// based on the alpha threshold from the paper, giving 3-7x
// speedups on power-law graphs at the cost of one extra full scan
// per direction switch.
//
// The implementation expects c to be symmetric (typical for
// undirected graphs built with [adjlist.Config.Directed]=false).
// For a directed graph callers should pre-build a symmetric CSR
// containing both edges and their reverses; the v1 algorithm does
// not maintain a separate in-edge CSR.
//
// Memory: visited and frontier bitmaps and the cur/next list slices
// are acquired from a pool; in the steady state BFSDirectionOpt is
// zero-allocation per call.
func BFSDirectionOpt[W any](c *csr.CSR[W], src graph.NodeID, visit func(node graph.NodeID, depth int) bool) {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	if uint64(src)+1 >= uint64(len(verts)) {
		return
	}
	maxID := uint64(c.MaxNodeID())
	words := int((maxID + 63) / 64)

	scr := acquireBFSDOScratch()
	defer releaseBFSDOScratch(scr)
	scr.resize(words)

	visited := scr.visited
	frontier := scr.frontier
	cur := scr.curList[:0]
	next := scr.nextList[:0]

	visited[uint64(src)>>6] |= 1 << (uint64(src) & 63)
	cur = append(cur, src)
	depth := 0
	// Beamer 2012 thresholds:
	//   - alpha gates the top-down -> bottom-up switch when the
	//     frontier's outgoing edges exceed unvisitedEdges / alpha;
	//   - beta gates the bottom-up -> top-down switch back when the
	//     frontier shrinks below maxID / beta.
	// Together they bracket the bottom-up phase to the dense middle
	// of a power-law BFS, recovering the 3-7x headline win.
	const alpha uint64 = 14
	const beta uint64 = 24
	inBottomUp := false
	for len(cur) > 0 {
		var frontierEdges uint64
		for _, n := range cur {
			frontierEdges += verts[uint64(n)+1] - verts[uint64(n)]
		}
		unvisitedEdges := edgesUnvisited(verts, visited)
		if !inBottomUp && frontierEdges > unvisitedEdges/alpha {
			inBottomUp = true
		} else if inBottomUp && uint64(len(cur)) < maxID/beta {
			inBottomUp = false
		}
		var stopped bool
		if inBottomUp {
			if bfsDoStepObserver != nil {
				bfsDoStepObserver(depth, true)
			}
			cur, next, stopped = bottomUpStep(verts, edges, visited, frontier, maxID, cur, next, &depth, visit)
		} else {
			if bfsDoStepObserver != nil {
				bfsDoStepObserver(depth, false)
			}
			cur, next, stopped = topDownStep(verts, edges, visited, cur, next, &depth, visit)
		}
		if stopped {
			scr.curList = cur
			scr.nextList = next
			return
		}
	}
	scr.curList = cur
	scr.nextList = next
}

func edgesUnvisited(verts, visited []uint64) uint64 {
	var sum uint64
	for i := 0; i < len(verts)-1; i++ {
		if visited[uint64(i)>>6]&(1<<(uint64(i)&63)) == 0 {
			sum += verts[i+1] - verts[i]
		}
	}
	return sum
}

// topDownStep expands the current frontier. cur is the input
// frontier (list); next is the output buffer (reused across calls).
// The returned cur is the old next, and the returned next is the old
// cur truncated to zero — the caller swaps them every iteration.
func topDownStep(
	verts []uint64,
	edges []graph.NodeID,
	visited []uint64,
	cur, next []graph.NodeID,
	depth *int,
	visit func(graph.NodeID, int) bool,
) (newCur, newNext []graph.NodeID, stopped bool) {
	next = next[:0]
	for _, n := range cur {
		if !visit(n, *depth) {
			return cur, next, true
		}
		start := verts[uint64(n)]
		end := verts[uint64(n)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			word := uint64(nb) >> 6
			bit := uint64(1) << (uint64(nb) & 63)
			if visited[word]&bit != 0 {
				continue
			}
			visited[word] |= bit
			next = append(next, nb)
		}
	}
	*depth++
	return next, cur[:0], false
}

// bottomUpStep scans every unvisited node, checking whether any of
// its neighbours is in the current frontier. The frontier is encoded
// as a bitmap (built from cur on entry) so the inner membership test
// is a single inlined bit-test rather than a map lookup. The returned
// cur is the next frontier (list) and the returned next is the old
// cur truncated to zero — same swap convention as topDownStep.
func bottomUpStep(
	verts []uint64,
	edges []graph.NodeID,
	visited []uint64,
	frontier []uint64,
	maxID uint64,
	cur, next []graph.NodeID,
	depth *int,
	visit func(graph.NodeID, int) bool,
) (newCur, newNext []graph.NodeID, stopped bool) {
	for _, n := range cur {
		if !visit(n, *depth) {
			return cur, next, true
		}
	}
	// Build the frontier bitmap from cur. The bitmap is reused from
	// the scratch pool; clear only the words covering maxID before
	// setting cur's bits.
	clearWords := (maxID + 63) / 64
	for i := uint64(0); i < clearWords; i++ {
		frontier[i] = 0
	}
	for _, n := range cur {
		frontier[uint64(n)>>6] |= 1 << (uint64(n) & 63)
	}
	next = next[:0]
	for id := uint64(0); id < maxID; id++ {
		word := id >> 6
		bit := uint64(1) << (id & 63)
		if visited[word]&bit != 0 {
			continue
		}
		start := verts[id]
		end := verts[id+1]
		for k := start; k < end; k++ {
			nb := uint64(edges[k])
			if frontier[nb>>6]&(1<<(nb&63)) != 0 {
				visited[word] |= bit
				next = append(next, graph.NodeID(id))
				break
			}
		}
	}
	*depth++
	return next, cur[:0], false
}

// bfsDoStepObserver is a test-only hook invoked once per BFS-DO step
// with (depth, isBottomUp). Tests set it via [setBFSDoStepObserver]
// to verify the Beamer alpha/beta switch behaviour from outside; it
// is nil and a single inlined branch in production builds.
//
//nolint:gochecknoglobals // test-only observer hook
var bfsDoStepObserver func(depth int, isBottomUp bool)

// SetBFSDoStepObserver installs (or clears with nil) the BFS-DO step
// observer for use by package tests. Not part of the public API.
func setBFSDoStepObserver(f func(depth int, isBottomUp bool)) {
	bfsDoStepObserver = f
}

// bfsDoScratch bundles the BFS-DO per-call working storage so it can
// be pooled across invocations. Slice headers are reused; the
// underlying arrays grow monotonically with the largest maxID seen.
type bfsDoScratch struct {
	visited  []uint64
	frontier []uint64
	curList  []graph.NodeID
	nextList []graph.NodeID
}

func (s *bfsDoScratch) resize(words int) {
	if cap(s.visited) < words {
		s.visited = make([]uint64, words)
	} else {
		s.visited = s.visited[:words]
		for i := range s.visited {
			s.visited[i] = 0
		}
	}
	if cap(s.frontier) < words {
		s.frontier = make([]uint64, words)
	} else {
		s.frontier = s.frontier[:words]
		// frontier is rebuilt per bottomUpStep, no need to clear here.
	}
}

//nolint:gochecknoglobals // package-level pool
var bfsDoScratchPool = sync.Pool{New: func() any { return &bfsDoScratch{} }}

func acquireBFSDOScratch() *bfsDoScratch {
	s, _ := bfsDoScratchPool.Get().(*bfsDoScratch)
	if s == nil {
		s = &bfsDoScratch{}
	}
	return s
}

func releaseBFSDOScratch(s *bfsDoScratch) {
	bfsDoScratchPool.Put(s)
}
