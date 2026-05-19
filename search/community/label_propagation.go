package community

import (
	"sort"

	"gograph/graph/csr"
)

// LabelPropagationOptions configures [LabelPropagation].
type LabelPropagationOptions struct {
	MaxIterations int
}

// DefaultLabelPropagationOptions returns reasonable defaults.
func DefaultLabelPropagationOptions() LabelPropagationOptions {
	return LabelPropagationOptions{MaxIterations: 16}
}

// LabelPropagation runs the Raghavan-Albert-Kumara 2007 algorithm:
// every node iteratively adopts the most common label among its
// neighbours, breaking ties deterministically by the smaller label
// ID. Converges when no node changes label or after
// MaxIterations.
//
// Complexity is O(k * (V + E)) for k iterations.
func LabelPropagation[W any](c *csr.CSR[W], opts LabelPropagationOptions) Partition {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 16
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return Partition{}
	}
	labels := make([]int, maxID)
	for i := range labels {
		labels[i] = i
	}
	for iter := 0; iter < opts.MaxIterations; iter++ {
		changed := false
		for v := 0; v < maxID; v++ {
			if verts[v+1] == verts[v] {
				continue
			}
			counts := map[int]int{}
			for k := verts[v]; k < verts[v+1]; k++ {
				counts[labels[edges[k]]]++
			}
			best := labels[v]
			bestCount := -1
			for cid, c := range counts {
				if c > bestCount || (c == bestCount && cid < best) {
					best = cid
					bestCount = c
				}
			}
			if best != labels[v] {
				labels[v] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return relabel(labels)
}

// relabel compacts arbitrary label values into [0, K).
func relabel(labels []int) Partition {
	seen := map[int]int{}
	for _, l := range labels {
		if _, ok := seen[l]; !ok {
			seen[l] = -1
		}
	}
	cids := make([]int, 0, len(seen))
	for cid := range seen {
		cids = append(cids, cid)
	}
	sort.Ints(cids)
	next := 0
	for _, cid := range cids {
		seen[cid] = next
		next++
	}
	out := make([]int, len(labels))
	for i, l := range labels {
		out[i] = seen[l]
	}
	return Partition{Community: out, NumCommunities: next}
}
