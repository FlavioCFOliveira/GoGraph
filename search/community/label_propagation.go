package community

import (
	"context"
	"sort"

	"gograph/graph/csr"
	"gograph/internal/metrics"
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
func LabelPropagation[W any](c *csr.CSR[W], opts LabelPropagationOptions) Partition {
	defer metrics.Time("search.community.LabelPropagation")()
	out, _ := LabelPropagationCtx(context.Background(), c, opts)
	return out
}

// LabelPropagationCtx is the context-aware variant of [LabelPropagation].
// ctx.Err() is checked at every iteration boundary; on cancellation
// returns (zero Partition, wrapped ctx.Err()).
//
//nolint:gocyclo // textbook label propagation: defaults + live mask + iteration loop + tie-break
func LabelPropagationCtx[W any](ctx context.Context, c *csr.CSR[W], opts LabelPropagationOptions) (Partition, error) {
	defer metrics.Time("search.community.LabelPropagationCtx")()
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 16
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return Partition{}, nil
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
	// Scratch buffers for per-vertex label-count accumulation:
	// counts is zero-initialised and the touched list records which
	// indices were written so the reset is O(unique-neighbour-labels)
	// rather than O(maxID) per vertex.
	counts := make([]int, maxID)
	touched := make([]int, 0, 32)
	for iter := 0; iter < opts.MaxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.community.LabelPropagationCtx.errors", 1)
			return Partition{}, err
		}
		changed := false
		for v := 0; v < maxID; v++ {
			if !mask[v] {
				continue
			}
			if verts[v+1] == verts[v] {
				continue
			}
			touched = touched[:0]
			for k := verts[v]; k < verts[v+1]; k++ {
				w := int(edges[k])
				if !mask[w] {
					continue
				}
				lw := labels[w]
				if counts[lw] == 0 {
					touched = append(touched, lw)
				}
				counts[lw]++
			}
			best := labels[v]
			bestCount := -1
			for _, cid := range touched {
				cnt := counts[cid]
				if cnt > bestCount || (cnt == bestCount && cid < best) {
					best = cid
					bestCount = cnt
				}
			}
			// Reset touched entries to zero for the next vertex.
			for _, cid := range touched {
				counts[cid] = 0
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
	return relabel(labels, mask), nil
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
