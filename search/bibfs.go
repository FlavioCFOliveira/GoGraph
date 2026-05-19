package search

import (
	"context"
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
)

// ErrNotUndirected was returned by older versions of [BiBFS] when
// the supplied CSR was not symmetric. As of Sprint 12 [BiBFS] now
// auto-builds the reverse CSR for directed inputs and the error is
// never produced; the sentinel is kept for backwards compatibility
// with callers using errors.Is to detect the legacy condition.
//
// Deprecated: BiBFS no longer requires a symmetric CSR.
var ErrNotUndirected = errors.New("search: BiBFS requires an undirected (symmetric) CSR")

// BiBFS performs bidirectional breadth-first search from src to dst
// on the unweighted graph captured by c. For undirected graphs (a
// symmetric directed CSR) the same CSR feeds both the forward and
// reverse expansion; for directed graphs c must be paired with the
// reverse CSR via [BiBFSOn].
//
// Returns the shortest path from src to dst inclusive, or [ErrNoPath]
// when the two endpoints are not connected.
//
// The two frontiers expand alternately; the iteration always grows
// the smaller frontier next so the search space approximates
// O(b^(d/2)) instead of O(b^d) for forward-only BFS, where b is the
// branching factor and d is the path length.
func BiBFS[W any](c *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	return BiBFSCtx(context.Background(), c, src, dst)
}

// BiBFSCtx is the context-aware variant of [BiBFS]. ctx.Err() is
// checked at every alternation between the forward and backward
// frontier expansion; on cancellation returns (nil, wrapped ctx.Err()).
//
// On undirected (symmetric) input it walks c in both directions; on
// directed input it builds the reverse CSR once and delegates to
// [BiBFSOnCtx]. The internal reverse build is O(V + E); callers
// running BiBFS many times on the same graph should hoist the build
// out via [BiBFSOnCtx].
func BiBFSCtx[W any](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	if c.IsSymmetric() {
		return BiBFSOnCtx(ctx, c, c, src, dst)
	}
	return BiBFSOnCtx(ctx, c, c.BuildReverse(), src, dst)
}

// BiBFSOn is [BiBFS] with a caller-provided reverse CSR. Required
// for directed graphs (where the symmetric closure differs from c)
// and recommended for any high-frequency caller — the O(V+E)
// reverse-CSR construction is hoisted out of the inner loop.
func BiBFSOn[W any](c, rev *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	return BiBFSOnCtx(context.Background(), c, rev, src, dst)
}

// BiBFSOnCtx is the context-aware variant of [BiBFSOn].
//
//nolint:gocyclo // canonical bidirectional BFS with separate forward/reverse adjacencies
func BiBFSOnCtx[W any](ctx context.Context, c, rev *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) ||
		uint64(dst)+1 >= uint64(len(c.VerticesSlice())) {
		return nil, ErrNoPath
	}
	if src == dst {
		return []graph.NodeID{src}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	maxID := uint64(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	revVerts := rev.VerticesSlice()
	revEdges := rev.EdgesSlice()

	visitedF := make([]bool, maxID)
	visitedB := make([]bool, maxID)
	parentF := make([]graph.NodeID, maxID)
	parentB := make([]graph.NodeID, maxID)

	frontierF := []graph.NodeID{src}
	frontierB := []graph.NodeID{dst}
	visitedF[uint64(src)] = true
	visitedB[uint64(dst)] = true
	parentF[uint64(src)] = src
	parentB[uint64(dst)] = dst

	meet := graph.NodeID(0)
	found := false
	for len(frontierF) > 0 && len(frontierB) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var grew []graph.NodeID
		// Forward search walks out-edges (c); reverse search walks
		// in-edges (rev). On a symmetric graph both adjacencies are
		// the same CSR.
		if len(frontierF) <= len(frontierB) {
			grew, meet, found = bibfsExpand(verts, edges, frontierF, visitedF, visitedB, parentF)
			frontierF = grew
		} else {
			grew, meet, found = bibfsExpand(revVerts, revEdges, frontierB, visitedB, visitedF, parentB)
			frontierB = grew
		}
		if found {
			break
		}
	}
	if !found {
		return nil, ErrNoPath
	}
	return joinPath(meet, parentF, parentB, src, dst), nil
}

// bibfsExpand expands one BFS level from currentFront. For each
// neighbour not yet seen by the same-direction visited set, it
// records the parent and tests against the opposite-direction
// visited set; the first collision returns meet=that NodeID and
// found=true.
func bibfsExpand(
	verts []uint64,
	edges []graph.NodeID,
	currentFront []graph.NodeID,
	visitedSame []bool,
	visitedOther []bool,
	parent []graph.NodeID,
) (next []graph.NodeID, meet graph.NodeID, found bool) {
	for _, n := range currentFront {
		start := verts[uint64(n)]
		end := verts[uint64(n)+1]
		for k := start; k < end; k++ {
			nb := edges[k]
			if visitedSame[uint64(nb)] {
				continue
			}
			visitedSame[uint64(nb)] = true
			parent[uint64(nb)] = n
			if visitedOther[uint64(nb)] {
				return next, nb, true
			}
			next = append(next, nb)
		}
	}
	return next, 0, false
}

// joinPath stitches the forward path src -> meet (via parentF) with
// the backward path meet -> dst (via parentB walked in reverse).
func joinPath(meet graph.NodeID, parentF, parentB []graph.NodeID, src, dst graph.NodeID) []graph.NodeID {
	var head []graph.NodeID
	for cur := meet; ; {
		head = append(head, cur)
		if cur == src {
			break
		}
		cur = parentF[uint64(cur)]
	}
	// Reverse head in place to get src ... meet.
	for i, j := 0, len(head)-1; i < j; i, j = i+1, j-1 {
		head[i], head[j] = head[j], head[i]
	}
	if meet == dst {
		return head
	}
	cur := parentB[uint64(meet)]
	for {
		head = append(head, cur)
		if cur == dst {
			break
		}
		cur = parentB[uint64(cur)]
	}
	return head
}
