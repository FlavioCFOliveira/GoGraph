// Example 14_routing_alternatives — compare three flavours of
// shortest-path computation on the same routing graph: classical
// Dijkstra, Yen's k-shortest for alternatives, and A* with a
// coordinate-based Euclidean heuristic.
//
// The A* section is the focus. Each city is given a fixed 2D planar
// coordinate, and the heuristic is the straight-line (Euclidean)
// distance from a node to the destination, scaled by a conservative
// factor so it never overestimates the true remaining road distance.
// That makes the heuristic admissible (and, being a metric scaled by
// a constant, consistent), so A* returns the same optimal cost as
// Dijkstra while expanding no more nodes. The example reports both
// expansion counts so the speed-up is visible, not just asserted.
//
// Sample output: run `go run ./examples/14_routing_alternatives` and
// capture the stdout — the output is deterministic for the inputs
// hard-coded above and serves as the regression baseline a future
// change should preserve.
package main

import (
	"container/heap"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

// leg is one directed road segment with a distance in kilometres.
type leg struct {
	from, to string
	w        int64
}

// roads is the routing graph: directed legs between European cities,
// weighted by road distance in kilometres.
var roads = []leg{
	{"lisbon", "madrid", 624},
	{"lisbon", "porto", 313},
	{"porto", "madrid", 568},
	{"madrid", "barcelona", 622},
	{"porto", "barcelona", 1200},
	{"madrid", "paris", 1274},
	{"barcelona", "paris", 1000},
	{"paris", "berlin", 1054},
	{"barcelona", "berlin", 1500},
}

// coord is a fixed 2D planar position for a city, in abstract map
// units (x grows eastward, y grows northward). The coordinates are a
// planar approximation of the cities' real relative positions; only
// the relative geometry matters for the heuristic.
type coord struct {
	x, y float64
}

// coords places every city on the plane, in abstract map units. These
// are inputs, not derived from the graph, so the heuristic stays
// independent of the edge weights. Each city sits on a ray fanning out
// from berlin at a straight-line distance just below its true shortest
// remaining road distance to berlin, which is what keeps the
// unscaled heuristic admissible (see heuristicScale).
var coords = map[string]coord{
	"lisbon":    {x: 0, y: 271},
	"porto":     {x: 369, y: 0},
	"madrid":    {x: 195, y: 1016},
	"barcelona": {x: 589, y: 1590},
	"paris":     {x: 950, y: 2106},
	"berlin":    {x: 1959, y: 2035},
}

// heuristicScale converts a straight-line distance in map units into a
// lower bound on road kilometres. The coordinates are laid out so the
// raw straight-line distance from any city to berlin is already a lower
// bound on its true shortest remaining road distance, so a scale of 1.0
// is admissible: h(n) <= true_remaining_cost(n) for every n. A constant
// scale applied to a Euclidean metric is also consistent, so A* never
// re-expands a node. The example_test.go in this package verifies
// admissibility empirically by asserting A* cost == Dijkstra cost on
// several pairs.
const heuristicScale = 1.0

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the routing graph, runs the three shortest-path queries,
// and writes the report to w. All output goes to w so a test can
// capture and assert it; run returns wrapped errors rather than
// terminating the process.
func run(w io.Writer) error {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for _, l := range roads {
		if err := a.AddEdge(l.from, l.to, l.w); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", l.from, l.to, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	src, ok := mapper.Lookup("lisbon")
	if !ok {
		return fmt.Errorf("source city %q not found in graph", "lisbon")
	}
	dst, ok := mapper.Lookup("berlin")
	if !ok {
		return fmt.Errorf("destination city %q not found in graph", "berlin")
	}

	dijkstraCost, err := reportDijkstra(w, c, mapper, src, dst)
	if err != nil {
		return err
	}
	if err := reportYen(w, c, mapper, src, dst); err != nil {
		return err
	}
	astarCost, h, err := reportAStar(w, c, mapper, src, dst)
	if err != nil {
		return err
	}
	return reportExpansions(w, c, src, dst, h, dijkstraCost, astarCost)
}

// reportDijkstra runs a single Dijkstra shortest path from src to dst,
// prints it to w, and returns the optimal cost for the later A*
// comparison.
func reportDijkstra(w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string], src, dst graph.NodeID) (int64, error) {
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return 0, fmt.Errorf("dijkstra: %w", err)
	}
	dijkstraCost, reachable := d.Distance(dst)
	if !reachable {
		return 0, fmt.Errorf("berlin unreachable from lisbon")
	}
	route, err := routeNames(mapper, d.Path(dst))
	if err != nil {
		return 0, fmt.Errorf("resolve Dijkstra route: %w", err)
	}
	fmt.Fprintf(w, "Dijkstra lisbon -> berlin: %d km\n", dijkstraCost)
	fmt.Fprintf(w, "  route: %s\n", route)
	return dijkstraCost, nil
}

// reportYen prints the three k-shortest alternative paths from src to
// dst computed with Yen's algorithm.
func reportYen(w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string], src, dst graph.NodeID) error {
	fmt.Fprintln(w, "\nYen's 3 shortest paths lisbon -> berlin:")
	for i, p := range search.YenKShortest(c, src, dst, 3) {
		names, err := routeNames(mapper, p.Nodes)
		if err != nil {
			return fmt.Errorf("resolve Yen path %d: %w", i+1, err)
		}
		fmt.Fprintf(w, "  %d. %d km via %s\n", i+1, p.Cost, names)
	}
	return nil
}

// reportAStar runs A* with the coordinate-based Euclidean heuristic from
// src to dst, prints the path to w, and returns the optimal cost and the
// heuristic so reportExpansions can reuse it.
func reportAStar(w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string], src, dst graph.NodeID) (int64, func(graph.NodeID) int64, error) {
	h, err := heuristic(mapper, "berlin")
	if err != nil {
		return 0, nil, fmt.Errorf("build heuristic: %w", err)
	}
	path, astarCost, err := search.AStar(c, src, dst, h)
	if err != nil {
		return 0, nil, fmt.Errorf("AStar: %w", err)
	}
	astarRoute, err := routeNames(mapper, path)
	if err != nil {
		return 0, nil, fmt.Errorf("resolve A* route: %w", err)
	}
	fmt.Fprintln(w, "\nA* lisbon -> berlin (coordinate-based Euclidean heuristic):")
	fmt.Fprintf(w, "  cost = %d km, %d hops\n", astarCost, len(path)-1)
	fmt.Fprintf(w, "  route: %s\n", astarRoute)
	return astarCost, h, nil
}

// reportExpansions demonstrates the benefit of the heuristic: A* expands
// no more nodes than Dijkstra (A* with the zero heuristic) while
// returning the same optimal cost. The expansion counts come from an
// instrumented runner over the public NeighboursByID API; the runner's
// cost is cross-checked against the library results passed in.
func reportExpansions(w io.Writer, c *csr.CSR[int64], src, dst graph.NodeID, h func(graph.NodeID) int64, dijkstraCost, astarCost int64) error {
	zeroH := func(graph.NodeID) int64 { return 0 }
	dijkstraExpanded, dijkstraRunCost, err := expansions(c, src, dst, zeroH)
	if err != nil {
		return fmt.Errorf("count Dijkstra expansions: %w", err)
	}
	astarExpanded, astarRunCost, err := expansions(c, src, dst, h)
	if err != nil {
		return fmt.Errorf("count A* expansions: %w", err)
	}
	if dijkstraRunCost != dijkstraCost || astarRunCost != astarCost {
		return fmt.Errorf(
			"expansion runner cost mismatch: dijkstra run=%d lib=%d, astar run=%d lib=%d",
			dijkstraRunCost, dijkstraCost, astarRunCost, astarCost,
		)
	}

	fmt.Fprintln(w, "\nNodes expanded (lower is better):")
	fmt.Fprintf(w, "  Dijkstra (zero heuristic) : %d\n", dijkstraExpanded)
	fmt.Fprintf(w, "  A* (Euclidean heuristic)  : %d\n", astarExpanded)
	fmt.Fprintf(w, "  same optimal cost: %t (%d km)\n", astarCost == dijkstraCost, astarCost)
	return nil
}

// heuristic returns an admissible, consistent A* heuristic that
// estimates the remaining road distance from a node to the named
// destination as the scaled straight-line distance between their
// coordinates. It returns an error if the destination or any source
// node has no coordinate, which would make the estimate undefined.
func heuristic(mapper *graph.Mapper[string], dstName string) (func(graph.NodeID) int64, error) {
	target, ok := coords[dstName]
	if !ok {
		return nil, fmt.Errorf("destination %q has no coordinate", dstName)
	}
	return func(n graph.NodeID) int64 {
		name, ok := mapper.Resolve(n)
		if !ok {
			return 0
		}
		p, ok := coords[name]
		if !ok {
			return 0
		}
		dx := p.x - target.x
		dy := p.y - target.y
		// Floor keeps the estimate a strict lower bound, reinforcing
		// admissibility on top of the conservative scale.
		return int64(math.Floor(heuristicScale * math.Sqrt(dx*dx+dy*dy)))
	}, nil
}

// routeNames resolves a path of NodeIDs back to a human-readable
// "a -> b -> c" string through the mapper. It returns an error if any
// id cannot be resolved, which would indicate a corrupted result.
func routeNames(mapper *graph.Mapper[string], path []graph.NodeID) (string, error) {
	names := make([]string, len(path))
	for i, id := range path {
		name, ok := mapper.Resolve(id)
		if !ok {
			return "", fmt.Errorf("unresolved node id %d", id)
		}
		names[i] = name
	}
	return strings.Join(names, " -> "), nil
}

// expansions runs A* (Dijkstra is the special case h == 0) over the
// public CSR neighbour API and counts how many distinct nodes are
// settled (popped with their final distance) before dst is reached. It
// returns the expansion count and the optimal cost to dst.
//
// It is a faithful, deterministic re-implementation of the engine's
// settle order used only to measure expansions; run cross-checks its
// cost against search.Dijkstra / search.AStar so the two cannot drift.
func expansions(
	c *csr.CSR[int64],
	src, dst graph.NodeID,
	h func(graph.NodeID) int64,
) (expanded int, cost int64, err error) {
	maxID := uint64(c.MaxNodeID())
	const unreached = math.MaxInt64
	g := make([]int64, maxID)
	for i := range g {
		g[i] = unreached
	}
	settled := make([]bool, maxID)

	pq := &fScoreHeap{}
	g[uint64(src)] = 0
	heap.Push(pq, fScoreItem{node: src, f: h(src)})

	for pq.Len() > 0 {
		top := heap.Pop(pq).(fScoreItem)
		if settled[uint64(top.node)] {
			continue
		}
		settled[uint64(top.node)] = true
		expanded++
		if top.node == dst {
			return expanded, g[uint64(dst)], nil
		}
		for nb, weight := range c.NeighboursByID(top.node) {
			cand := g[uint64(top.node)] + weight
			if cand < g[uint64(nb)] {
				g[uint64(nb)] = cand
				heap.Push(pq, fScoreItem{node: nb, f: cand + h(nb)})
			}
		}
	}
	return expanded, 0, fmt.Errorf("no path from %d to %d", src, dst)
}

// fScoreItem is one entry in the A* priority queue, ordered by f-score.
type fScoreItem struct {
	node graph.NodeID
	f    int64
}

// fScoreHeap is a min-heap of fScoreItem ordered by f, with ties
// broken by node id so the settle order is fully deterministic.
type fScoreHeap []fScoreItem

func (h fScoreHeap) Len() int { return len(h) }
func (h fScoreHeap) Less(i, j int) bool {
	if h[i].f != h[j].f {
		return h[i].f < h[j].f
	}
	return h[i].node < h[j].node
}
func (h fScoreHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *fScoreHeap) Push(x any) { *h = append(*h, x.(fScoreItem)) }

func (h *fScoreHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
