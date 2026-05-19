package search

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// JohnsonAPSP computes APSP on c by running [Dijkstra] from every
// vertex. The v1 implementation handles non-negative weights only;
// negative-cycle detection / reweighting via Bellman-Ford is
// deferred — callers on graphs that may contain negative edges
// should use [FloydWarshall] instead (which tolerates negative
// weights without negative cycles).
//
// For non-negative graphs the complexity is O(V * (V + E) log V),
// which beats Floyd-Warshall's O(V^3) on sparse graphs.
func JohnsonAPSP[W Weight](c *csr.CSR[W]) (*APSP[W], error) {
	maxID := int(c.MaxNodeID())
	out := &APSP[W]{n: maxID, dist: make([]W, maxID*maxID)}
	infV := out.inf()
	for i := range out.dist {
		out.dist[i] = infV
	}
	for src := 0; src < maxID; src++ {
		d, err := Dijkstra(c, graph.NodeID(src))
		if err != nil {
			return nil, err
		}
		for dst := 0; dst < maxID; dst++ {
			if v, ok := d.Distance(graph.NodeID(dst)); ok {
				out.dist[src*maxID+dst] = v
			}
		}
	}
	return out, nil
}
