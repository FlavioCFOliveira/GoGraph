// Example 13_network_reliability — analyse the resilience of a single
// communication backbone two ways over ONE coherent network:
//
//  1. Structural single points of failure — the articulation points
//     and bridges whose loss partitions the network, found with
//     search.HopcroftTarjanBCC over the CSR snapshot.
//  2. Throughput and its bottleneck — the maximum flow between two
//     sites via Dinic's max-flow (search/flow), followed by the
//     minimum cut: the saturated links that cap that throughput.
//
// Both analyses run on the SAME node set and the SAME capacitated edge
// list: the flow.Network is built from the very edges the structural
// analysis sees, with capacities assigned programmatically, so the two
// views describe one network rather than two unrelated graphs.
//
// Sample output: run `go run ./examples/13_network_reliability` and
// capture the stdout — the output is deterministic for the inputs
// hard-coded above and serves as the regression baseline a future
// change should preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/flow"
)

// link is one undirected backbone connection between two sites with a
// capacity expressed in Gb/s. The whole network — both the structural
// reliability analysis and the max-flow throughput analysis — is
// derived from this single list, so the two views can never drift
// apart.
type link struct {
	a, b string
	cap  int // Gb/s
}

// backbone is the one coherent topology this example analyses. It is a
// redundant core ring (lisbon-madrid-paris-frankfurt, with a madrid-
// paris cross-link) that no single failure can partition, plus three
// spur sites (london, berlin, warsaw) hanging off single links. The
// redundancy gives more than one path between any two core sites, which
// is what makes both the bridge analysis and the max-flow non-trivial.
var backbone = []link{
	{"lisbon", "madrid", 12},
	{"lisbon", "paris", 8},
	{"madrid", "paris", 6},
	{"madrid", "frankfurt", 10},
	{"paris", "frankfurt", 7},
	{"paris", "london", 5},
	{"frankfurt", "berlin", 9},
	{"berlin", "warsaw", 4},
}

// source and sink are the two sites whose throughput the example
// measures. They sit on opposite ends of the redundant core, so the
// flow can split across several paths.
const (
	source = "lisbon"
	sink   = "frankfurt"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the one backbone network, reports its structural single
// points of failure and its source-to-sink throughput with the cut
// that limits it, and writes the whole report to w. All output goes to
// w so a test can capture and assert it; run returns wrapped errors
// rather than terminating the process.
func run(w io.Writer) error {
	// Build the mutable graph from the single backbone edge list. The
	// mapper assigns every site a compact NodeID; that same NodeID
	// space is reused for the flow network below.
	a := adjlist.New[string, int64](adjlist.Config{Directed: false})
	for _, l := range backbone {
		if err := a.AddEdge(l.a, l.b, int64(l.cap)); err != nil {
			return fmt.Errorf("AddEdge %s-%s: %w", l.a, l.b, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	if err := reportSPOF(w, c, mapper); err != nil {
		return err
	}
	return reportThroughput(w, mapper)
}

// reportSPOF prints the structural single points of failure of the
// network captured by c: the articulation points (sites) and bridges
// (links) whose individual loss partitions the backbone.
func reportSPOF(w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string]) error {
	res := search.HopcroftTarjanBCC(c)

	fmt.Fprintln(w, "Single points of failure:")
	if len(res.Articulation) == 0 {
		fmt.Fprintln(w, "  (no articulation points — every site survives any single-site loss)")
	}
	for _, id := range res.Articulation {
		name, ok := mapper.Resolve(id)
		if !ok {
			return fmt.Errorf("unresolved articulation node id %d", id)
		}
		fmt.Fprintf(w, "  articulation point: %s\n", name)
	}
	if len(res.Bridges) == 0 {
		fmt.Fprintln(w, "  (no bridges — every link is part of a redundant cycle)")
	}
	for _, b := range res.Bridges {
		u, ok := mapper.Resolve(b[0])
		if !ok {
			return fmt.Errorf("unresolved bridge endpoint id %d", b[0])
		}
		v, ok := mapper.Resolve(b[1])
		if !ok {
			return fmt.Errorf("unresolved bridge endpoint id %d", b[1])
		}
		fmt.Fprintf(w, "  bridge: %s -- %s\n", u, v)
	}
	return nil
}

// reportThroughput builds a capacitated flow network from the SAME
// backbone edge list (mapped through mapper, so the node identities
// match the structural analysis), computes the maximum flow from source
// to sink, then derives and prints the minimum cut — the saturated
// links that bottleneck the throughput. The cut capacity is verified to
// equal the reported max flow.
func reportThroughput(w io.Writer, mapper *graph.Mapper[string]) error {
	src, ok := mapper.Lookup(source)
	if !ok {
		return fmt.Errorf("source site %q not found in graph", source)
	}
	snk, ok := mapper.Lookup(sink)
	if !ok {
		return fmt.Errorf("sink site %q not found in graph", sink)
	}

	// The flow network reuses the mapper's NodeID space directly: a
	// site's index in the residual graph is its NodeID. Every
	// undirected backbone link becomes a pair of opposing directed
	// edges of equal capacity, so flow may traverse it either way.
	net := newResidual(int(mapper.MaxNodeID()) + 1) //nolint:gosec // G115: bounded example graph size, no realistic overflow
	for _, l := range backbone {
		ua, _ := mapper.Lookup(l.a)
		ub, _ := mapper.Lookup(l.b)
		net.addUndirected(int(ua), int(ub), l.cap) //nolint:gosec // G115: bounded example graph size, no realistic overflow
	}

	flowValue := net.maxFlow(int(src), int(snk)) //nolint:gosec // G115: bounded example graph size, no realistic overflow

	// Cross-check the value against the library's Dinic implementation
	// built from the identical edge list: the example's own residual
	// solver and search/flow must agree on the answer.
	if libValue := libMaxFlow(mapper); libValue != flowValue {
		return fmt.Errorf("max-flow mismatch: residual solver=%d, search/flow=%d", flowValue, libValue)
	}

	fmt.Fprintf(w, "\nMax throughput %s -> %s: %d Gb/s\n", source, sink, flowValue)

	// Derive the minimum cut from the residual graph: the set of sites
	// still reachable from the source over edges with spare capacity is
	// the source side of the cut. Every backbone link crossing from
	// that side to the rest is saturated and forms the bottleneck.
	cut, cutCap := net.minCut(int(src), backbone, mapper) //nolint:gosec // G115: bounded example graph size, no realistic overflow
	if cutCap != flowValue {
		return fmt.Errorf("min-cut capacity %d != max flow %d (max-flow min-cut theorem violated)", cutCap, flowValue)
	}

	fmt.Fprintf(w, "Bottleneck (min-cut, %d Gb/s) — saturated links:\n", cutCap)
	for _, l := range cut {
		fmt.Fprintf(w, "  %s -- %s (%d Gb/s, fully utilised)\n", l.a, l.b, l.cap)
	}
	return nil
}

// libMaxFlow rebuilds the backbone as a search/flow network and returns
// the Dinic max flow from source to sink. It exists only to cross-check
// the example's in-line residual solver against the library.
func libMaxFlow(mapper *graph.Mapper[string]) int {
	g := flow.NewNetwork(int(mapper.MaxNodeID()) + 1) //nolint:gosec // G115: bounded example graph size, no realistic overflow
	for _, l := range backbone {
		ua, _ := mapper.Lookup(l.a)
		ub, _ := mapper.Lookup(l.b)
		// An undirected link is two opposing directed edges, each of
		// the link's capacity.
		g.AddEdge(int(ua), int(ub), l.cap) //nolint:gosec // G115: bounded example graph size, no realistic overflow
		g.AddEdge(int(ub), int(ua), l.cap) //nolint:gosec // G115: bounded example graph size, no realistic overflow
	}
	src, _ := mapper.Lookup(source)
	snk, _ := mapper.Lookup(sink)
	return flow.MaxFlow(g, int(src), int(snk)) //nolint:gosec // G115: bounded example graph size, no realistic overflow
}

// residual is a small Edmonds-Karp max-flow solver kept inside the
// example so it can also expose the residual graph after the flow
// settles — which is what lets reportThroughput derive the minimum
// cut. It is deterministic: BFS visits neighbours in ascending node
// order, so the same input always yields the same flow assignment.
type residual struct {
	n   int
	cap [][]int // cap[u][v] = remaining capacity on the u->v residual edge
}

func newResidual(n int) *residual {
	capacity := make([][]int, n)
	for i := range capacity {
		capacity[i] = make([]int, n)
	}
	return &residual{n: n, cap: capacity}
}

// addUndirected adds an undirected link as two opposing directed
// residual edges of equal capacity.
func (r *residual) addUndirected(u, v, c int) {
	r.cap[u][v] += c
	r.cap[v][u] += c
}

// maxFlow runs Edmonds-Karp from src to snk, mutating the residual
// capacities in place, and returns the total flow.
func (r *residual) maxFlow(src, snk int) int {
	total := 0
	for {
		parent := make([]int, r.n)
		for i := range parent {
			parent[i] = -1
		}
		parent[src] = src
		queue := []int{src}
		for len(queue) > 0 && parent[snk] == -1 {
			u := queue[0]
			queue = queue[1:]
			for v := 0; v < r.n; v++ {
				if parent[v] == -1 && r.cap[u][v] > 0 {
					parent[v] = u
					queue = append(queue, v)
				}
			}
		}
		if parent[snk] == -1 {
			return total // no augmenting path remains
		}
		// Bottleneck capacity along the discovered path.
		push := 1 << 30
		for v := snk; v != src; v = parent[v] {
			if r.cap[parent[v]][v] < push {
				push = r.cap[parent[v]][v]
			}
		}
		// Push the flow, updating both directions of every residual edge.
		for v := snk; v != src; v = parent[v] {
			r.cap[parent[v]][v] -= push
			r.cap[v][parent[v]] += push
		}
		total += push
	}
}

// minCut derives the minimum cut from the settled residual graph. The
// set S of nodes still reachable from src over edges with spare
// capacity is the source side of the cut; every original backbone link
// with exactly one endpoint in S is saturated and forms the bottleneck.
// It returns those links (in backbone order, so the output is stable)
// and their total capacity, which equals the max flow.
func (r *residual) minCut(src int, links []link, mapper *graph.Mapper[string]) (cut []link, cutCap int) {
	reachable := make([]bool, r.n)
	reachable[src] = true
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for v := 0; v < r.n; v++ {
			if !reachable[v] && r.cap[u][v] > 0 {
				reachable[v] = true
				queue = append(queue, v)
			}
		}
	}
	for _, l := range links {
		ua, _ := mapper.Lookup(l.a)
		ub, _ := mapper.Lookup(l.b)
		if reachable[ua] != reachable[ub] {
			cut = append(cut, l)
			cutCap += l.cap
		}
	}
	return cut, cutCap
}
