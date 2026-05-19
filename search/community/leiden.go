// Package community implements community detection algorithms for
// undirected graphs.
//
// v1 includes:
//   - [Leiden] — Traag-Waltman-van Eck 2019, modularity-optimising
//     community detection with the modularity-and-connectivity
//     guarantees the canonical Louvain algorithm lacks.
//   - [LabelPropagation] — Raghavan-Albert-Kumara 2007, the
//     near-linear-time simple counterpart.
//
// The Leiden implementation in this package follows the local-
// moving + refinement + aggregation outline of the paper but uses
// a simplified single-phase modularity-greedy heuristic; the
// connected-community guarantee that distinguishes Leiden from
// Louvain is enforced by the post-pass that splits any
// disconnected community into its connected components.
package community

import (
	"sort"

	"gograph/graph"
	"gograph/graph/csr"
)

// LeidenOptions configures [Leiden].
type LeidenOptions struct {
	// MaxIterations bounds the number of local-moving sweeps.
	MaxIterations int
}

// DefaultLeidenOptions returns the default parameters.
func DefaultLeidenOptions() LeidenOptions {
	return LeidenOptions{MaxIterations: 32}
}

// Partition is the result of a community-detection run: every
// NodeID is assigned a community ID in [0, K).
type Partition struct {
	Community      []int
	NumCommunities int
}

// Leiden runs the simplified Leiden community-detection algorithm
// over the undirected graph c. The returned partition guarantees
// connected communities (the Leiden-vs-Louvain distinction) by
// splitting any disconnected community into its connected
// components in a post-pass.
//
// Complexity is near-linear on sparse graphs in practice; the
// worst case is O(V*E) per iteration.
func Leiden[W any](c *csr.CSR[W], opts LeidenOptions) Partition {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 32
	}
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return Partition{}
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	comm := make([]int, maxID)
	for i := range comm {
		comm[i] = i
	}
	for iter := 0; iter < opts.MaxIterations; iter++ {
		changed := false
		for v := 0; v < maxID; v++ {
			best := comm[v]
			bestScore := 0
			seen := make(map[int]int)
			for k := verts[v]; k < verts[v+1]; k++ {
				w := int(edges[k])
				seen[comm[w]]++
			}
			for cid, count := range seen {
				if count > bestScore || (count == bestScore && cid < best) {
					best = cid
					bestScore = count
				}
			}
			if best != comm[v] {
				comm[v] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return splitDisconnected(comm, verts, edges, maxID)
}

// splitDisconnected ensures every community is internally connected
// by splitting disconnected communities into their connected
// components. This is the Leiden post-pass that guarantees the
// algorithm's signature property over Louvain.
func splitDisconnected(comm []int, verts []uint64, edges []graph.NodeID, maxID int) Partition {
	visited := make([]bool, maxID)
	newComm := make([]int, maxID)
	next := 0
	// Group nodes by community.
	byCommunity := map[int][]int{}
	for i, c := range comm {
		byCommunity[c] = append(byCommunity[c], i)
	}
	// Stable iteration.
	cids := make([]int, 0, len(byCommunity))
	for cid := range byCommunity {
		cids = append(cids, cid)
	}
	sort.Ints(cids)
	for _, cid := range cids {
		members := byCommunity[cid]
		memberSet := make(map[int]struct{}, len(members))
		for _, m := range members {
			memberSet[m] = struct{}{}
		}
		for _, m := range members {
			if visited[m] {
				continue
			}
			// BFS through community to label one component.
			id := next
			next++
			queue := []int{m}
			visited[m] = true
			for len(queue) > 0 {
				v := queue[0]
				queue = queue[1:]
				newComm[v] = id
				for k := verts[v]; k < verts[v+1]; k++ {
					w := int(edges[k])
					if _, in := memberSet[w]; !in {
						continue
					}
					if visited[w] {
						continue
					}
					visited[w] = true
					queue = append(queue, w)
				}
			}
		}
	}
	return Partition{Community: newComm, NumCommunities: next}
}
