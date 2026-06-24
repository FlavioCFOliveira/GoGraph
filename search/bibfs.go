package search

import (
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
// when the two endpoints are not connected. The returned path length
// always equals the true unweighted shortest distance: BiBFS completes
// the current level expansion on the chosen side and picks the global
// minimum-total collision before terminating, so it never returns a
// strictly longer path because of neighbour iteration order.
//
// The two frontiers expand alternately; the iteration always grows
// the smaller frontier next so the search space approximates
// O(b^(d/2)) instead of O(b^d) for forward-only BFS, where b is the
// branching factor and d is the path length.
func BiBFS[W any](c *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	defer metrics.Time("search.BiBFS").Stop()
	res, err := BiBFSCtx(context.Background(), c, src, dst)
	if err != nil {
		metrics.IncCounter("search.BiBFS.errors", 1)
	}
	return res, err
}

// BiBFSCtx is the context-aware variant of [BiBFS]. ctx.Err() is
// checked at every alternation between the forward and backward
// frontier expansion; on cancellation returns (nil, wrapped ctx.Err()).
//
// On undirected (symmetric) input it walks c in both directions; on
// directed input it builds the reverse CSR once and delegates to
// [BiBFSOnCtx]. The internal reverse build is O(V + E); callers
// running BiBFS many times on the same graph should hoist the build
// out via [BiBFSOnCtx]. The level-complete intersection rule
// documented on [BiBFS] is preserved.
func BiBFSCtx[W any](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	defer metrics.Time("search.BiBFSCtx").Stop()
	var (
		res []graph.NodeID
		err error
	)
	if c.IsSymmetric() {
		res, err = BiBFSOnCtx(ctx, c, c, src, dst)
	} else {
		res, err = BiBFSOnCtx(ctx, c, c.BuildReverse(), src, dst)
	}
	if err != nil {
		metrics.IncCounter("search.BiBFSCtx.errors", 1)
	}
	return res, err
}

// BiBFSOn is [BiBFS] with a caller-provided reverse CSR. Required
// for directed graphs (where the symmetric closure differs from c)
// and recommended for any high-frequency caller — the O(V+E)
// reverse-CSR construction is hoisted out of the inner loop. The
// level-complete intersection rule documented on [BiBFS] is
// preserved.
func BiBFSOn[W any](c, rev *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	defer metrics.Time("search.BiBFSOn").Stop()
	res, err := BiBFSOnCtx(context.Background(), c, rev, src, dst)
	if err != nil {
		metrics.IncCounter("search.BiBFSOn.errors", 1)
	}
	return res, err
}

// BiBFSOnCtx is the context-aware variant of [BiBFSOn]. The
// level-complete intersection rule documented on [BiBFS] is
// preserved: after the first cross-frontier collision the loop
// performs at most one additional expansion on the opposite side and
// then commits to the global-minimum (forwardDepth + backwardDepth)
// meet recorded across both expansions.
//
//nolint:gocyclo // canonical bidirectional BFS with separate forward/reverse adjacencies
func BiBFSOnCtx[W any](ctx context.Context, c, rev *csr.CSR[W], src, dst graph.NodeID) ([]graph.NodeID, error) {
	defer metrics.Time("search.BiBFSOnCtx").Stop()
	if uint64(src)+1 >= uint64(len(c.VerticesSlice())) ||
		uint64(dst)+1 >= uint64(len(c.VerticesSlice())) {
		metrics.IncCounter("search.BiBFSOnCtx.errors", 1)
		return nil, ErrNoPath
	}
	if src == dst {
		return []graph.NodeID{src}, nil
	}
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("search.BiBFSOnCtx.errors", 1)
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
	// depthF[n] / depthB[n] hold the BFS depth at which n was first
	// reached by the forward / backward search. -1 marks unvisited.
	// The arrays are required by the level-complete intersection
	// rule: at each collision we minimise newSameDepth+depthOther[nb]
	// rather than committing to the first meet encountered.
	depthF := make([]int32, maxID)
	depthB := make([]int32, maxID)
	for i := range depthF {
		depthF[i] = -1
	}
	for i := range depthB {
		depthB[i] = -1
	}

	frontierF := []graph.NodeID{src}
	frontierB := []graph.NodeID{dst}
	visitedF[uint64(src)] = true
	visitedB[uint64(dst)] = true
	parentF[uint64(src)] = src
	parentB[uint64(dst)] = dst
	depthF[uint64(src)] = 0
	depthB[uint64(dst)] = 0

	// currentDepthF / currentDepthB hold the BFS depth of the nodes
	// currently sitting in frontierF / frontierB. The next expansion
	// will mark its discovered nodes at depth currentDepthX+1.
	currentDepthF := int32(0)
	currentDepthB := int32(0)

	var (
		bestMeet  graph.NodeID
		bestTotal = int32(-1)
		// foundAny flips on the first cross-frontier collision. The
		// audit's recommended termination rule is the simplest
		// correct one: in unweighted BFS the next level on either
		// side can only add 1 to a depth, so a strictly shorter
		// total can only appear in the very next expansion of the
		// side that has not just expanded. Once foundAny is set we
		// pin the next expansion to the opposite side, observe it,
		// then commit to bestMeet / bestTotal.
		foundAny    bool
		expandOther uint8 // 0=heuristic, 1=force forward, 2=force backward
	)
	for len(frontierF) > 0 && len(frontierB) > 0 {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.BiBFSOnCtx.errors", 1)
			return nil, err
		}
		var (
			grew        []graph.NodeID
			levelMeet   graph.NodeID
			levelTotal  int32
			levelFound  bool
			expandedFwd bool
		)
		// Forward search walks out-edges (c); reverse search walks
		// in-edges (rev). On a symmetric graph both adjacencies are
		// the same CSR. The default branch expands the smaller
		// frontier first (canonical heuristic); after foundAny we
		// pin the choice to the opposite side for one extra pass.
		var expandFwd bool
		switch expandOther {
		case 1:
			expandFwd = true
		case 2:
			expandFwd = false
		default:
			expandFwd = len(frontierF) <= len(frontierB)
		}
		if expandFwd {
			grew, levelMeet, levelTotal, levelFound = bibfsExpand(
				verts, edges, frontierF, visitedF, visitedB, parentF,
				depthF, depthB, currentDepthF+1,
			)
			frontierF = grew
			currentDepthF++
			expandedFwd = true
		} else {
			grew, levelMeet, levelTotal, levelFound = bibfsExpand(
				revVerts, revEdges, frontierB, visitedB, visitedF, parentB,
				depthB, depthF, currentDepthB+1,
			)
			frontierB = grew
			currentDepthB++
		}
		if levelFound && (bestTotal < 0 || levelTotal < bestTotal) {
			bestMeet = levelMeet
			bestTotal = levelTotal
		}
		if foundAny {
			// We just completed the single permitted opposite-side
			// expansion after the initial collision. bestMeet now
			// holds the global minimum; terminate.
			break
		}
		if levelFound {
			foundAny = true
			// Pin the next expansion to the opposite side. If that
			// side has an empty frontier no further improvement is
			// possible and we terminate immediately.
			oppositeEmpty := (expandedFwd && len(frontierB) == 0) ||
				(!expandedFwd && len(frontierF) == 0)
			if oppositeEmpty {
				break
			}
			if expandedFwd {
				expandOther = 2 // next expansion: backward
			} else {
				expandOther = 1 // next expansion: forward
			}
		}
	}
	if !foundAny {
		metrics.IncCounter("search.BiBFSOnCtx.errors", 1)
		return nil, ErrNoPath
	}
	return joinPath(bestMeet, parentF, parentB, src, dst), nil
}

// bibfsExpand expands one BFS level from currentFront, marking every
// freshly discovered neighbour at newSameDepth and recording each
// cross-frontier collision. After iterating the full currentFront it
// returns the next frontier together with the meet node minimising
// (newSameDepth + depthOther[nb]) — that is, the smallest total
// shortest-path distance achievable through this level expansion.
//
// The accumulate-then-pick contract is what guarantees the function
// returns the true shortest path: an early return on the first
// collision, combined with neighbour iteration order, could surface
// a strictly longer meet than another collision encountered later in
// the same expansion.
func bibfsExpand(
	verts []uint64,
	edges []graph.NodeID,
	currentFront []graph.NodeID,
	visitedSame []bool,
	visitedOther []bool,
	parent []graph.NodeID,
	depthSame []int32,
	depthOther []int32,
	newSameDepth int32,
) (next []graph.NodeID, meet graph.NodeID, bestTotal int32, found bool) {
	bestTotal = -1
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
			depthSame[uint64(nb)] = newSameDepth
			if visitedOther[uint64(nb)] {
				total := newSameDepth + depthOther[uint64(nb)]
				if !found || total < bestTotal {
					meet = nb
					bestTotal = total
					found = true
				}
				// Collisions are not added to next: their other-side
				// depth is already known, so re-expanding them would
				// only produce equal-or-worse totals.
				continue
			}
			next = append(next, nb)
		}
	}
	return next, meet, bestTotal, found
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
