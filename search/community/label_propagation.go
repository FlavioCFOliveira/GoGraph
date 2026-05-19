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
// every live node iteratively adopts the most common label among its
// neighbours, breaking ties deterministically by the smaller label
// ID. Converges when no node changes label or after MaxIterations.
//
// Only live NodeIDs participate; ghost slots receive the sentinel -1
// in the returned partition.
//
// Complexity is O(k * (V + E)) for k iterations.
//
//nolint:gocyclo // textbook label propagation: defaults + live mask + iteration loop + tie-break
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
	mask := c.LiveMask()
	labels := make([]int, maxID)
	for i := 0; i < maxID; i++ {
		if mask[i] {
			labels[i] = i
		} else {
			labels[i] = -1
		}
	}
	for iter := 0; iter < opts.MaxIterations; iter++ {
		changed := false
		for v := 0; v < maxID; v++ {
			if !mask[v] {
				continue
			}
			if verts[v+1] == verts[v] {
				continue
			}
			counts := map[int]int{}
			for k := verts[v]; k < verts[v+1]; k++ {
				w := int(edges[k])
				if !mask[w] {
					continue
				}
				counts[labels[w]]++
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
	return relabel(labels, mask)
}

// relabel compacts arbitrary label values into [0, K) for live nodes.
// Ghost slots remain at -1.
func relabel(labels []int, mask []bool) Partition {
	seen := map[int]int{}
	for i, l := range labels {
		if !mask[i] {
			continue
		}
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
		if !mask[i] {
			out[i] = -1
			continue
		}
		out[i] = seen[l]
	}
	return Partition{Community: out, NumCommunities: next}
}
