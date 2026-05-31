// Example 10_dimacs9_routing — build a small synthetic road-network
// graph with the DIMACS 9 harness, run a concrete single-source
// shortest-paths query over it, and print an environment-dependent
// latency summary from the harness for flavour.
//
// The structural output — node count, edge count, the shortest path
// and its distance — is deterministic for the inputs hard-coded below
// and serves as the regression baseline a future change should
// preserve. The trailing latency lines (p50/p95/p99) are timing
// measurements and vary from run to run and machine to machine; they
// are clearly separated and are not part of the regression baseline.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"gograph/bench/dimacs9"
	"gograph/graph"
	"gograph/graph/csr"
	"gograph/search"
)

// Fixed, deterministic generator inputs. With these the synthetic
// generator produces 12 nodes and 24 directed edges (average out-degree
// 2), small enough to print and reason about by hand.
const (
	vertices = 12
	edges    = 30
	srcNode  = uint32(0)
	dstNode  = uint32(11)
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the synthetic road graph, runs a concrete Dijkstra query
// from srcNode to dstNode, prints the deterministic result, then prints
// an environment-dependent latency summary. All output goes to w so a
// test can capture and assert the deterministic parts; run returns
// wrapped errors rather than terminating the process.
func run(w io.Writer) error {
	ctx := context.Background()

	// Build the synthetic road-network-shaped graph. The generator is
	// deterministic for fixed (vertices, edges): destinations and edge
	// weights are pure functions of the endpoint indices, so the graph
	// — and therefore every shortest path over it — is identical on
	// every run and every machine.
	a, err := dimacs9.Synthetic(ctx, vertices, edges)
	if err != nil {
		return fmt.Errorf("dimacs9.Synthetic: %w", err)
	}

	c := csr.BuildFromAdjList(a)
	fmt.Fprintf(w, "Graph:  %d nodes, %d edges\n", c.Order(), c.Size())

	// Resolve the source and destination node values to their compact
	// NodeIDs through the adjacency list's mapper.
	mapper := a.Mapper()
	src, ok := mapper.Lookup(srcNode)
	if !ok {
		return fmt.Errorf("source node %d not found in graph", srcNode)
	}
	dst, ok := mapper.Lookup(dstNode)
	if !ok {
		return fmt.Errorf("destination node %d not found in graph", dstNode)
	}

	// Run a single-source shortest-paths query and read back both the
	// distance and the reconstructed route to the destination.
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return fmt.Errorf("dijkstra: %w", err)
	}
	dist, reachable := d.Distance(dst)
	if !reachable {
		return fmt.Errorf("node %d is not reachable from node %d", dstNode, srcNode)
	}
	route, err := routeNodes(mapper, d.Path(dst))
	if err != nil {
		return fmt.Errorf("resolve route to %d: %w", dstNode, err)
	}
	fmt.Fprintf(w, "SSSP:   node %d -> node %d\n", srcNode, dstNode)
	fmt.Fprintf(w, "  distance: %d\n", dist)
	fmt.Fprintf(w, "  path:     %s\n", route)

	// Latency summary from the benchmark harness. These numbers are
	// wall-clock measurements: they depend on the host and vary from
	// run to run, so they are deliberately kept out of the regression
	// baseline above.
	rep := dimacs9.Run(ctx, dimacs9.Spec{Vertices: vertices, Edges: edges, Queries: 100})
	fmt.Fprintln(w, "Latency (environment-dependent, not a regression baseline):")
	fmt.Fprintf(w, "  p50: %v\n", rep.Percentile(0.50))
	fmt.Fprintf(w, "  p95: %v\n", rep.Percentile(0.95))
	fmt.Fprintf(w, "  p99: %v\n", rep.Percentile(0.99))
	return nil
}

// routeNodes resolves a path of NodeIDs back to a "0 -> 1 -> 2" string
// of node values through the mapper. It returns an error if any id
// cannot be resolved, which would indicate a corrupted result.
func routeNodes(mapper *graph.Mapper[uint32], path []graph.NodeID) (string, error) {
	parts := make([]string, len(path))
	for i, id := range path {
		val, ok := mapper.Resolve(id)
		if !ok {
			return "", fmt.Errorf("unresolved node id %d", id)
		}
		parts[i] = strconv.FormatUint(uint64(val), 10)
	}
	return strings.Join(parts, " -> "), nil
}
