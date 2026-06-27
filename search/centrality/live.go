package centrality

import "github.com/FlavioCFOliveira/GoGraph/graph"

// liveMask reports which CSR slots are participating nodes — those with at
// least one incident edge (in or out). The immutable CSR cannot distinguish a
// genuinely isolated node from an unused slot in the sharded NodeID space, so
// matrix-based measures ([Eigenvector], [Katz]) score only participating nodes;
// isolated/ghost slots receive 0. This mirrors [PageRank]'s live detection.
func liveMask(verts []uint64, edges []graph.NodeID, slots int) (live []bool, count int) {
	live = make([]bool, slots)
	for src := 0; src < slots; src++ {
		if verts[src] != verts[src+1] {
			live[src] = true
			for k := verts[src]; k < verts[src+1]; k++ {
				live[edges[k]] = true
			}
		}
	}
	for _, l := range live {
		if l {
			count++
		}
	}
	return live, count
}
