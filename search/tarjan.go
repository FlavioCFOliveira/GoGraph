package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// tarjanFrame is one stack record of Tarjan's iterative DFS.
type tarjanFrame struct {
	v    graph.NodeID
	next uint64 // index into the edges slice for the next neighbour to inspect
}

// TarjanSCC returns the strongly connected components of the directed
// graph captured by c using Tarjan's algorithm (1972). Each component
// is returned as a slice of NodeIDs. The implementation is iterative
// (using an explicit work stack) so it does not risk Go goroutine
// stack overflow on graphs with deep DFS chains.
//
// Complexity is O(V + E). The implementation is single-threaded.
//
// Only NodeIDs that participate in at least one edge (as source or
// destination) are considered; NodeIDs that were assigned by the
// Mapper but never gained an edge are not emitted as singleton SCCs.
func TarjanSCC[W any](c *csr.CSR[W]) [][]graph.NodeID {
	defer metrics.Time("search.TarjanSCC")()
	out, _ := TarjanSCCCtx(context.Background(), c)
	return out
}

// TarjanSCCCtx is the context-aware variant of [TarjanSCC]. ctx.Err()
// is checked at every restart of the outer DFS loop (i.e. before each
// new root) and every 4096 work-stack steps inside the inner DFS loop;
// on cancellation returns (nil, wrapped ctx.Err()). The inner check is
// required because a single strongly connected component (e.g. one
// giant cycle) is explored from a single root, so the whole O(V+E)
// traversal would otherwise be one uninterruptible outer iteration.
func TarjanSCCCtx[W any](ctx context.Context, c *csr.CSR[W]) ([][]graph.NodeID, error) {
	defer metrics.Time("search.TarjanSCCCtx")()
	maxID := uint64(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	live := make([]bool, maxID)
	for from := uint64(0); from < maxID; from++ {
		if verts[from+1] > verts[from] {
			live[from] = true
		}
		for k := verts[from]; k < verts[from+1]; k++ {
			live[uint64(edges[k])] = true
		}
	}

	const unvisited int32 = -1
	index := make([]int32, maxID)
	lowlink := make([]int32, maxID)
	onStack := make([]bool, maxID)
	for i := range index {
		index[i] = unvisited
	}
	var stack []graph.NodeID
	var nextIndex int32
	var out [][]graph.NodeID

	var work []tarjanFrame
	// popCount drives the inner-loop ctx poll at the canonical 4096
	// stride; it spans every root so one giant SCC explored from a
	// single root is still cancellable mid-traversal.
	popCount := 0

	for start := uint64(0); start < maxID; start++ {
		if !live[start] || index[start] != unvisited {
			continue
		}
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.TarjanSCCCtx.errors", 1)
			return nil, err
		}
		// Begin DFS from start.
		index[start] = nextIndex
		lowlink[start] = nextIndex
		nextIndex++
		stack = append(stack, graph.NodeID(start))
		onStack[start] = true
		work = append(work, tarjanFrame{v: graph.NodeID(start), next: verts[start]})

		for len(work) > 0 {
			if popCount&0xFFF == 0 {
				if err := ctx.Err(); err != nil {
					metrics.IncCounter("search.TarjanSCCCtx.errors", 1)
					return nil, err
				}
			}
			popCount++
			top := &work[len(work)-1]
			vIdx := uint64(top.v)
			if top.next < verts[vIdx+1] {
				w := edges[top.next]
				top.next++
				tarjanVisitNeighbour(w, vIdx, &nextIndex, index, lowlink, onStack, &stack, &work, verts)
				continue
			}
			// Finished v's neighbours.
			work = work[:len(work)-1]
			if len(work) > 0 {
				parent := uint64(work[len(work)-1].v)
				if lowlink[vIdx] < lowlink[parent] {
					lowlink[parent] = lowlink[vIdx]
				}
			}
			if lowlink[vIdx] == index[vIdx] {
				out = append(out, tarjanPopComponent(&stack, onStack, top.v))
			}
		}
	}
	return out, nil
}

// tarjanVisitNeighbour processes one neighbour during Tarjan's DFS.
// When the neighbour is unvisited it is pushed onto the work stack;
// when it is on the active DFS stack the parent's lowlink is updated.
func tarjanVisitNeighbour(
	w graph.NodeID,
	vIdx uint64,
	nextIndex *int32,
	index, lowlink []int32,
	onStack []bool,
	stack *[]graph.NodeID,
	work *[]tarjanFrame,
	verts []uint64,
) {
	wIdx := uint64(w)
	if index[wIdx] == -1 {
		index[wIdx] = *nextIndex
		lowlink[wIdx] = *nextIndex
		*nextIndex++
		*stack = append(*stack, w)
		onStack[wIdx] = true
		*work = append(*work, tarjanFrame{v: w, next: verts[wIdx]})
		return
	}
	if onStack[wIdx] && index[wIdx] < lowlink[vIdx] {
		lowlink[vIdx] = index[wIdx]
	}
}

// tarjanPopComponent pops nodes from the DFS stack until and
// including root, returning them as a single SCC.
func tarjanPopComponent(stack *[]graph.NodeID, onStack []bool, root graph.NodeID) []graph.NodeID {
	var comp []graph.NodeID
	for {
		last := (*stack)[len(*stack)-1]
		*stack = (*stack)[:len(*stack)-1]
		onStack[uint64(last)] = false
		comp = append(comp, last)
		if last == root {
			return comp
		}
	}
}
