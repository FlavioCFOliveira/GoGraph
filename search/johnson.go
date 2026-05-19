package search

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// JohnsonAPSP computes APSP on c by running [Dijkstra] from every
// live vertex. The v1 implementation handles non-negative weights
// only; negative-cycle detection / reweighting via Bellman-Ford is
// deferred — callers on graphs that may contain negative edges
// should use [FloydWarshall] instead (which tolerates negative
// weights without negative cycles).
//
// For non-negative graphs the complexity is O(V * (V + E) log V),
// which beats Floyd-Warshall's O(V^3) on sparse graphs.
func JohnsonAPSP[W Weight](c *csr.CSR[W]) (*APSP[W], error) {
	maxID := int(c.MaxNodeID())
	mask := c.LiveMask()
	compact := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = live
			live++
		} else {
			compact[i] = -1
		}
	}
	out := &APSP[W]{
		live:    live,
		maxID:   maxID,
		compact: compact,
		dist:    make([]W, live*live),
	}
	if live == 0 {
		return out, nil
	}
	infV := out.inf()
	for i := range out.dist {
		out.dist[i] = infV
	}
	for src := 0; src < maxID; src++ {
		si := compact[src]
		if si < 0 {
			continue
		}
		d, err := Dijkstra(c, graph.NodeID(src))
		if err != nil {
			return nil, err
		}
		for dst := 0; dst < maxID; dst++ {
			di := compact[dst]
			if di < 0 {
				continue
			}
			if v, ok := d.Distance(graph.NodeID(dst)); ok {
				out.dist[si*live+di] = v
			}
		}
	}
	return out, nil
}
