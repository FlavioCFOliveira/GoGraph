package exec

// shortest_path_bidir_test.go — the differential suite for the two-sided
// shortestPath search (rmp #2220).
//
// It lives INSIDE package exec, not exec_test, because the whole point is to run
// the two algorithms against each other on the same operator: the forward-only
// walk is retained as [ShortestPath.bfsShortestPathForward] precisely so it can
// serve as the reference, and reaching it needs package access.
//
// What is compared is the FULL emitted hop list, not just the path length. The
// backward half resolves each hop's forward position and traversal orientation
// through a rule that is the dual of the forward half's, and getting that dual
// wrong yields a path of the right length whose hops hydrate to the wrong
// relationship instances — a defect a length check cannot see.
//
// Where several shortest paths exist the two algorithms may legitimately return
// different ones, so the comparison is: same length, and the returned path is
// independently VALIDATED as a real path (every hop's edge exists, connects the
// two nodes it claims to, and no relationship repeats).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// biCSR is a minimal [CSRAdjacency] for these tests. The shared test helpers
// live in package exec_test and are unreachable from here, and reaching the
// forward-only reference implementation requires being inside package exec — so
// this file carries its own small builders rather than exporting a seam purely
// for testing.
type biCSR struct {
	vertices []uint64
	edges    []graph.NodeID
	handles  []uint64
}

func (c *biCSR) VerticesSlice() []uint64    { return c.vertices }
func (c *biCSR) EdgesSlice() []graph.NodeID { return c.edges }
func (c *biCSR) HandlesSlice() []uint64     { return c.handles }

// biNoInput is an Operator that yields nothing. The tests drive the search
// functions directly, so the operator's input is never pulled.
type biNoInput struct{}

func (biNoInput) Init(context.Context) error { return nil }
func (biNoInput) Next(*Row) (bool, error)    { return false, nil }
func (biNoInput) Close() error               { return nil }

// biTestGraph is an edge list plus the node count, from which both CSR
// directions are derived so the reverse really is the reverse.
type biTestGraph struct {
	n     int
	edges [][2]int
}

// csrPair builds the forward CSR and its CANONICAL transpose — the same reverse
// CSR production uses, since cypher's csrPairFromGraph always derives it via
// [csr.CSR.BuildReverse]. Handles are assigned per edge so a parallel edge is
// distinguishable, which is what relationship-uniqueness and the reverse→forward
// position mapping both depend on.
//
// The bucket ORDER matters, not just the contents: the reverse type-admit bitset
// is built by replaying BuildReverse's counting-sort scatter, so a reverse CSR
// assembled in some other order is rejected and falls back. Building the fixture
// the way production does is what lets these tests exercise the fast path at all;
// [biTestGraph.csrPairNonCanonical] covers the other side.
func (g biTestGraph) csrPair() (fwd, rev *biCSR) {
	fwd = buildCSRWithHandles(g.n, g.edges)
	rev = transposeCSR(g.n, fwd)
	return fwd, rev
}

// csrPairNonCanonical builds the same edge set with a reverse CSR whose buckets
// are filled in ORIGINAL EDGE-LIST order rather than forward-CSR order. It holds
// exactly the same arcs — it is a perfectly valid reverse adjacency — but it is
// not the transpose the replay expects, so it exercises the fallback path.
//
// The two orders differ whenever two arcs into one destination come from
// different sources in a different relative order than source-ascending: for
// edges [{1,5},{0,5}] the canonical reverse bucket of 5 is [0,1] and this one is
// [1,0].
// Since rmp #2141 the FORWARD CSR must be destination-ordered — every production
// producer guarantees it and the O(log d) probes in csrprobe.go rely on it — but
// the REVERSE may be in any order: nothing binary-searches it. buildRevToFwd walks
// the reverse linearly and searches the FORWARD. So the reverse is built here
// WITHOUT ordering, which is what keeps this fixture genuinely non-canonical and
// the fallback path covered. Ordering it too would make it identical to the
// canonical transpose — both would be ordered by (source, handle) — and silently
// leave the fallback untested.
func (g biTestGraph) csrPairNonCanonical() (fwd, rev *biCSR) {
	fwd = buildCSRWithHandles(g.n, g.edges)
	rev = buildCSRWithHandlesUnordered(g.n, flipEdges(g.edges))
	return fwd, rev
}

func flipEdges(edges [][2]int) [][2]int {
	out := make([][2]int, len(edges))
	for i, e := range edges {
		out[i] = [2]int{e[1], e[0]}
	}
	return out
}

// transposeCSR mirrors [csr.CSR.BuildReverse]: count in-degrees, prefix-sum them
// into bucket offsets, then scatter every forward arc — walking the forward CSR
// in (source ascending, slot order) — into its destination's bucket, carrying the
// handle to the slot it lands in.
func transposeCSR(maxNode int, fwd *biCSR) *biCSR {
	verts := make([]uint64, maxNode+1)
	for _, d := range fwd.edges {
		verts[int(d)+1]++
	}
	for i := 1; i <= maxNode; i++ {
		verts[i] += verts[i-1]
	}
	edges := make([]graph.NodeID, len(fwd.edges))
	handles := make([]uint64, len(fwd.edges))
	cursor := make([]uint64, maxNode+1)
	copy(cursor, verts)
	for u := 0; u+1 < len(fwd.vertices); u++ {
		for k := fwd.vertices[u]; k < fwd.vertices[u+1]; k++ {
			v := int(fwd.edges[k])
			pos := cursor[v]
			edges[pos] = graph.NodeID(u)
			handles[pos] = fwd.handles[k]
			cursor[v]++
		}
	}
	return &biCSR{vertices: verts, edges: edges, handles: handles}
}

// buildCSRWithHandles is buildCSR plus a stable per-edge handle. The handle is
// the edge's index in the ORIGINAL list, so the same logical edge carries the
// same handle in the forward and reverse CSRs — the invariant buildRevToFwd
// relies on to pair a reverse slot with its forward counterpart.
func buildCSRWithHandles(maxNode int, edgeList [][2]int) *biCSR {
	c := buildCSRWithHandlesUnordered(maxNode, edgeList)
	// Order each source's run exactly as the production builders do (rmp #2141).
	// This fixture is hand-assembled rather than produced by csr.BuildFromAdjList,
	// so without this it would carry input-order runs — a shape no FORWARD CSR
	// reaching the executor can have any more, since every producer either orders
	// (BuildFromAdjList, BuildFromAdjListLive, BuildReverse) or calls OrderRuns
	// itself (store/bulk). Calling the real function rather than re-implementing
	// the order keeps the fixture faithful by construction.
	csr.OrderRuns[struct{}](c.vertices, c.edges, nil, c.handles)
	return c
}

// buildCSRWithHandlesUnordered is [buildCSRWithHandles] without the ordering pass,
// leaving each source's run in ORIGINAL EDGE-LIST order. It exists for the
// deliberately non-canonical reverse CSR: see [biTestGraph.csrPairNonCanonical].
func buildCSRWithHandlesUnordered(maxNode int, edgeList [][2]int) *biCSR {
	type he struct {
		e [2]int
		h uint64
	}
	items := make([]he, len(edgeList))
	for i, e := range edgeList {
		items[i] = he{e: e, h: uint64(i)}
	}
	verts := make([]uint64, maxNode+1)
	for _, it := range items {
		verts[it.e[0]+1]++
	}
	for i := 1; i <= maxNode; i++ {
		verts[i] += verts[i-1]
	}
	edges := make([]graph.NodeID, verts[maxNode])
	handles := make([]uint64, verts[maxNode])
	cursor := make([]uint64, maxNode+1)
	copy(cursor, verts)
	for _, it := range items {
		pos := cursor[it.e[0]]
		edges[pos] = graph.NodeID(it.e[1])
		handles[pos] = it.h
		cursor[it.e[0]]++
	}
	return &biCSR{vertices: verts, edges: edges, handles: handles}
}

// biOperator builds a ShortestPath over g, initialised and ready to search.
func biOperator(t *testing.T, g biTestGraph, dir Direction) *ShortestPath {
	t.Helper()
	fwd, rev := g.csrPair()
	return biOperatorFiltered(t, fwd, rev, dir, "", nil)
}

// biOperatorFiltered builds an initialised ShortestPath over an explicit CSR
// pair, optionally with a relationship-type filter.
//
// The filter reaches the operator through its [AdjacencySource], which Init
// resolves before it builds the reverse-position admit bitset (rmp #2317). That
// ordering used to be the CALLER's obligation — WithTypeFilter had to be chained
// on before Init or the operator could not run a typed two-sided search — and is
// now structural, because the filter and the adjacency it is keyed to arrive
// together at the one point that needs them.
func biOperatorFiltered(t *testing.T, fwd, rev *biCSR, dir Direction, edgeType string, filter map[uint64]string) *ShortestPath {
	t.Helper()
	op := NewShortestPath(biNoInput{}, StaticAdjacency(fwd, rev, filter), dir, 0, 1)
	if edgeType != "" {
		op.WithTypeFilter(edgeType)
	}
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return op
}

// biFilterCase is one type-filter configuration for the differential matrix.
// admits selects, by the edge's index in the fixture's ORIGINAL edge list, which
// edges the filter accepts; the map handed to the operator is keyed by forward
// CSR position, and the oracle consumes admits directly. That separation is
// deliberate — the oracle must not read the same structure the operator does.
type biFilterCase struct {
	name   string
	admits func(edgeIdx int) bool
}

var biFilterCases = []biFilterCase{
	{"no filter", nil},
	{"filter admits all", func(int) bool { return true }},
	{"filter admits even edges", func(i int) bool { return i%2 == 0 }},
	{"filter admits none", func(int) bool { return false }},
}

// filterFor renders a filter case into the forward-position-keyed map the
// operator takes. buildCSRWithHandles stamps each slot's handle with the edge's
// index in the original list, so the handle recovers which edge a slot holds.
func filterFor(fwd *biCSR, fc biFilterCase) map[uint64]string {
	if fc.admits == nil {
		return nil
	}
	out := make(map[uint64]string, len(fwd.edges))
	for pos := range fwd.edges {
		if fc.admits(int(fwd.handles[pos])) {
			out[uint64(pos)] = "K"
		}
	}
	return out
}

// oracleDist is the ABSOLUTE oracle: the src→dst hop distance computed by plain
// BFS over the fixture's raw edge list, honouring the direction and the admitted
// edge set, with no reference to any CSR, filter map, or operator state.
//
// It exists because both compared algorithms now share one reverse-side type
// check (the admit bitset). A differential between two arms that consult the same
// structure cannot see a defect INSIDE that structure — it would simply report
// agreement. This oracle re-derives the answer from the edge list instead, so a
// wrong admit bitset shows up as a wrong distance.
//
// A shortest path never repeats a vertex, hence never repeats an edge, so BFS
// distance is the correct oracle for the relationship-uniqueness-constrained
// shortest path. Returns -1 when unreachable. src == dst is out of scope (that is
// the cycle search).
func oracleDist(g biTestGraph, dir Direction, admits func(int) bool, src, dst int) int {
	type arc struct{ to int }
	adj := make([][]arc, g.n)
	for i, e := range g.edges {
		if admits != nil && !admits(i) {
			continue
		}
		// dir names how a PATH edge may be traversed: DirOut only along the
		// arrow, DirIn only against it, DirBoth either way.
		if dir != DirIn {
			adj[e[0]] = append(adj[e[0]], arc{to: e[1]})
		}
		if dir != DirOut {
			adj[e[1]] = append(adj[e[1]], arc{to: e[0]})
		}
	}
	dist := make([]int, g.n)
	for i := range dist {
		dist[i] = -1
	}
	dist[src] = 0
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		if u == dst {
			return dist[u]
		}
		for _, a := range adj[u] {
			if dist[a.to] >= 0 {
				continue
			}
			dist[a.to] = dist[u] + 1
			queue = append(queue, a.to)
		}
	}
	return dist[dst]
}

// pathLen returns the hop count encoded in a flat alternating path list.
func pathLen(v expr.Value) int {
	lv, ok := v.(expr.ListValue)
	if !ok {
		return -1
	}
	return (len(lv) - 1) / VLEHopStride
}

// validatePath checks that a returned path list is a REAL path in g: it starts
// at src, ends at dst, every hop's recorded forward position is an edge that
// genuinely connects the previous node to the claimed one in the orientation the
// hop records, and no relationship handle repeats.
//
// When admits is non-nil it additionally requires every hop to use an ADMITTED
// edge, judged by the edge's index in the fixture's original list (recovered from
// the slot's handle) rather than by the filter map the operator consults. A path
// that routes over an excluded edge is the exact defect obstacle 2 of rmp #2236
// fixed, and this is the assertion that keeps it fixed.
//
// This is the half of the comparison a length check cannot do. It is written
// against the graph rather than against the other algorithm, so it stays a valid
// oracle even if both algorithms were wrong in the same way.
// slotOfEmittedID maps an emitted relationship identity back to the forward-CSR
// slot it names: the handle's slot when the fixture carries a handle column, and
// the id itself when it does not (the handle-less fallback [Expand.emittedEdgeID]
// documents).
func slotOfEmittedID(fwd *biCSR, edgeID uint64) (uint64, bool) {
	if len(fwd.handles) == 0 {
		if edgeID >= uint64(len(fwd.edges)) {
			return 0, false
		}
		return edgeID, true
	}
	for pos, h := range fwd.handles {
		if h == edgeID {
			return uint64(pos), true
		}
	}
	return 0, false
}

func validatePath(t *testing.T, fwd *biCSR, v expr.Value, src, dst uint64, admits func(int) bool) {
	t.Helper()
	lv, ok := v.(expr.ListValue)
	if !ok {
		t.Fatalf("path is %T, want ListValue", v)
	}
	if got := uint64(lv[0].(expr.IntegerValue)); got != src {
		t.Fatalf("path starts at %d, want src %d", got, src)
	}
	cur := src
	seen := map[uint64]struct{}{}
	hops := (len(lv) - 1) / VLEHopStride
	for i := 0; i < hops; i++ {
		// The hop carries the EMITTED relationship identity, which since rmp #2317
		// is the stable HANDLE rather than a forward-CSR position. Resolve it back
		// to the slot it names, exactly as production does, so the structural checks
		// below still reason about the real arc.
		edgeID := uint64(lv[1+VLEHopStride*i].(expr.IntegerValue))
		next := uint64(lv[2+VLEHopStride*i].(expr.IntegerValue))
		dir := int64(lv[3+VLEHopStride*i].(expr.IntegerValue))

		fwdPos, resolved := slotOfEmittedID(fwd, edgeID)
		if !resolved {
			t.Fatalf("hop %d: emitted edge id %d names no slot of the forward CSR", i, edgeID)
		}
		// Recover the edge the forward position denotes: its source is the vertex
		// whose CSR range contains fwdPos, its destination is fwd.edges[fwdPos].
		var eSrc uint64
		for u := 0; u+1 < len(fwd.vertices); u++ {
			if fwdPos >= fwd.vertices[u] && fwdPos < fwd.vertices[u+1] {
				eSrc = uint64(u)
				break
			}
		}
		eDst := uint64(fwd.edges[fwdPos])

		wantFrom, wantTo := eSrc, eDst
		if dir == VLEDirReverse {
			wantFrom, wantTo = eDst, eSrc
		}
		if wantFrom != cur || wantTo != next {
			t.Fatalf("hop %d claims %d→%d but its edge (fwdPos %d, dir %d) runs %d→%d",
				i, cur, next, fwdPos, dir, wantFrom, wantTo)
		}
		h := fwd.handles[fwdPos]
		if _, dup := seen[h]; dup {
			t.Fatalf("hop %d repeats relationship handle %d — relationship-uniqueness violated", i, h)
		}
		seen[h] = struct{}{}
		if admits != nil && !admits(int(h)) {
			t.Fatalf("hop %d traverses edge %d (fwdPos %d), which the type filter EXCLUDES", i, h, fwdPos)
		}
		cur = next
	}
	if cur != dst {
		t.Fatalf("path ends at %d, want dst %d", cur, dst)
	}
}

// TestBiBFS_DifferentialAgainstForwardOnly is the core gate. For every pair in
// every generated graph it requires three things to agree: the two-sided search,
// the forward-only reference, and an ABSOLUTE oracle that re-derives the distance
// by BFS over the raw edge list.
//
// The matrix is all three directions × both reverse-CSR bucket orders × four
// type-filter configurations. rmp #2220 delivered only untyped DirOut and pinned
// the other cases as negatives; #2236 widened the gate to every direction and to
// a typed search, so those negatives become positives here — except where the
// reverse CSR is not the canonical transpose, which no longer supports a typed
// two-sided search and must fall back.
//
// The oracle is not decoration. Both arms now consult ONE reverse-side type check
// (the admit bitset), so a defect inside it would make both arms wrong the same
// way and the differential would report agreement. Two of this project's audits
// lost a real defect exactly that way. The oracle reads the edge list.
func TestBiBFS_DifferentialAgainstForwardOnly(t *testing.T) {
	dirs := []struct {
		name string
		dir  Direction
	}{
		{"DirOut", DirOut},
		{"DirIn", DirIn},
		{"DirBoth", DirBoth},
	}
	revKinds := []struct {
		name string
		// canonical marks the reverse CSR production builds (a true transpose).
		// Only that one supports the reverse-position admit bitset, so only it can
		// run a TYPED two-sided search.
		canonical bool
	}{
		{"canonicalRev", true},
		{"nonCanonicalRev", false},
	}

	// Coverage counters, checked after the matrix. A configuration that silently
	// stopped reaching one of the two algorithms would otherwise leave this suite
	// green while testing half of what it claims.
	var twoSided, fellBack int

	for _, gc := range biGraphCases(t) {
		for _, d := range dirs {
			for _, rk := range revKinds {
				for _, fc := range biFilterCases {
					t.Run(gc.name+"/"+d.name+"/"+rk.name+"/"+fc.name, func(t *testing.T) {
						fwdCSR, revCSR := gc.g.csrPair()
						if !rk.canonical {
							fwdCSR, revCSR = gc.g.csrPairNonCanonical()
						}
						edgeType := ""
						if fc.admits != nil {
							edgeType = "K"
						}
						op := biOperatorFiltered(t, fwdCSR, revCSR, d.dir, edgeType, filterFor(fwdCSR, fc))

						// The gate, asserted only where it is determined. Untyped is always
						// two-sided: the shape check passes for both bucket orders. Typed is
						// two-sided whenever the admit bitset was built, which the CANONICAL
						// transpose always allows — that is #2236's new capability and is
						// asserted outright.
						//
						// The non-canonical typed case is deliberately NOT pinned either way.
						// "Non-canonical" is a property of the whole CSR, and the replay
						// validates arc by arc, so a fixture whose bucket orders happen to
						// coincide — any graph whose in-degrees are all ≤ 1, for instance —
						// legitimately passes validation and runs two-sided. Both outcomes
						// are correct; the oracle below is what checks the ANSWER, and the
						// counters check that each path was actually exercised.
						twoSide := op.canBidirectional()
						if fc.admits == nil || rk.canonical {
							if !twoSide {
								t.Fatalf("canBidirectional = false, want true — the admission gate "+
									"narrowed without this test moving with it (revExact=%v "+
									"revVerts=%d/%d revEdges=%d/%d)", op.admit.RevExact(),
									len(op.revVerts), len(op.fwdVerts), len(op.revEdges), len(op.fwdEdges))
							}
						}
						if twoSide {
							twoSided++
						} else {
							fellBack++
						}
						wantTwoSide := twoSide

						for src := 0; src < gc.g.n; src++ {
							for dst := 0; dst < gc.g.n; dst++ {
								if src == dst {
									continue // the cycle search, out of scope
								}
								s, dd := uint64(src), uint64(dst)
								wantDist := oracleDist(gc.g, d.dir, fc.admits, src, dst)

								gotFwd, foundFwd, err := op.bfsShortestPathForward(s, dd)
								if err != nil {
									t.Fatalf("forward-only %d→%d: %v", src, dst, err)
								}
								// The ABSOLUTE check, applied to the reference arm first: if
								// the reference disagrees with the oracle, the differential
								// below is worthless and this says so plainly.
								if foundFwd != (wantDist >= 0) {
									t.Fatalf("%d→%d: forward-only reachability=%v but the oracle says dist=%d",
										src, dst, foundFwd, wantDist)
								}
								if foundFwd {
									if got := pathLen(gotFwd); got != wantDist {
										t.Fatalf("%d→%d: forward-only length %d, oracle %d", src, dst, got, wantDist)
									}
									validatePath(t, fwdCSR, gotFwd, s, dd, fc.admits)
								}

								if !wantTwoSide {
									continue // the forward-only walk serves this configuration
								}
								gotBi, foundBi, err := op.biBFSShortestPath(s, dd)
								if err != nil {
									t.Fatalf("bidirectional %d→%d: %v", src, dst, err)
								}
								if foundBi != foundFwd {
									t.Fatalf("%d→%d: reachability differs — bidirectional=%v forward-only=%v (oracle dist=%d)",
										src, dst, foundBi, foundFwd, wantDist)
								}
								if !foundBi {
									continue
								}
								if got := pathLen(gotBi); got != wantDist {
									t.Fatalf("%d→%d: bidirectional length %d, oracle %d", src, dst, got, wantDist)
								}
								validatePath(t, fwdCSR, gotBi, s, dd, fc.admits)
							}
						}
					})
				}
			}
		}
	}

	// Both paths must actually have run. Without this a change that made every
	// configuration fall back — or one that admitted every configuration and left
	// the fallback dead — would still pass the whole matrix above, since the oracle
	// is satisfied either way.
	if twoSided == 0 {
		t.Error("no configuration ran the two-sided search; the matrix proves nothing about it")
	}
	if fellBack == 0 {
		t.Error("no configuration fell back to the forward-only walk, so the fallback path " +
			"is untested. A typed search over a non-transpose reverse CSR must fall back — if " +
			"no fixture produces one any more, add a graph whose in-degrees exceed 1 with its " +
			"sources out of ascending order")
	}
	t.Logf("coverage: %d configurations two-sided, %d fell back to the forward-only walk", twoSided, fellBack)
}

// biGraphCases returns the fixed shapes the acceptance criteria name plus a
// batch of randomised graphs. The random ones use a fixed seed sequence so a
// failure is reproducible.
func biGraphCases(t *testing.T) []struct {
	name string
	g    biTestGraph
} {
	t.Helper()
	cases := make([]struct {
		name string
		g    biTestGraph
	}, 0, 14)
	cases = append(cases, []struct {
		name string
		g    biTestGraph
	}{
		{"chain", biTestGraph{6, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}}}},
		{"diamond (two shortest paths)", biTestGraph{4, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}}}},
		{"disconnected", biTestGraph{6, [][2]int{{0, 1}, {1, 2}, {3, 4}, {4, 5}}}},
		{"self-loops", biTestGraph{5, [][2]int{{0, 0}, {0, 1}, {1, 1}, {1, 2}, {2, 3}, {3, 3}, {3, 4}}}},
		{"parallel edges (multigraph)", biTestGraph{4, [][2]int{{0, 1}, {0, 1}, {1, 2}, {1, 2}, {2, 3}}}},
		{"parallel + self-loop + disconnected", biTestGraph{7, [][2]int{
			{0, 1}, {0, 1}, {1, 1}, {1, 2}, {2, 0}, {4, 5}, {5, 4}, {5, 6},
		}}},
		{"star (high fan-out from one hub)", biTestGraph{9, [][2]int{
			{0, 1}, {0, 2}, {0, 3}, {0, 4}, {1, 5}, {2, 6}, {3, 7}, {4, 8},
		}}},
		{"bidirectional pair only", biTestGraph{2, [][2]int{{0, 1}, {1, 0}}}},
	}...)

	// Randomised graphs. Small enough to enumerate every pair exhaustively, dense
	// enough that the two frontiers actually meet in the middle.
	for i, seed := range []uint64{1, 2, 3, 7, 11, 13} {
		rng := rand.New(rand.NewPCG(seed, seed*2+1))
		n := 8 + int(rng.Uint64()%7) // 8..14
		m := n + int(rng.Uint64()%uint64(2*n))
		edges := make([][2]int, 0, m)
		for k := 0; k < m; k++ {
			edges = append(edges, [2]int{int(rng.Uint64() % uint64(n)), int(rng.Uint64() % uint64(n))})
		}
		cases = append(cases, struct {
			name string
			g    biTestGraph
		}{fmt.Sprintf("random/seed%d(n=%d,m=%d)", seed, n, m), biTestGraph{n, edges}})
		_ = i
	}
	return cases
}

// TestBiBFS_TypeFilteredSearchIsTwoSidedAndExact is the positive replacement for
// #2220's TestBiBFS_TypeFilterKeepsTheForwardOnlyWalk, which pinned a type filter
// as DISABLING the two-sided search. rmp #2236 removed that narrowing, so the
// same fixture now asserts the opposite — and, crucially, asserts the exactness
// that earned it: an excluded hop must break the path, not be quietly admitted.
//
// A three-hop chain with the MIDDLE edge excluded is the discriminating shape.
// The rejection has to happen during frontier expansion rather than at the seed,
// and it has to happen in the BACKWARD half too — which is the half that scans
// reverse slots and therefore the half a permissive reverse check let through.
func TestBiBFS_TypeFilteredSearchIsTwoSidedAndExact(t *testing.T) {
	g := biTestGraph{4, [][2]int{{0, 1}, {1, 2}, {2, 3}}}
	fwdCSR, revCSR := g.csrPair()

	// Which forward slot holds the middle edge {1,2}? Its handle is its index in
	// the original list, so look it up rather than assuming slot order.
	const middleEdge = 1

	for _, d := range []struct {
		name string
		dir  Direction
	}{{"DirOut", DirOut}, {"DirIn", DirIn}, {"DirBoth", DirBoth}} {
		t.Run(d.name, func(t *testing.T) {
			// Control: admit everything. The path must exist, and the search must be
			// the two-sided one — otherwise the exclusion case below would prove
			// nothing about the two-sided search.
			admitAll := biFilterCase{"all", func(int) bool { return true }}
			op := biOperatorFiltered(t, fwdCSR, revCSR, d.dir, "K", filterFor(fwdCSR, admitAll))
			if !op.canBidirectional() {
				t.Fatal("a type-filtered search did not take the two-sided path; rmp #2236 " +
					"admits it whenever the reverse-position admit bitset was built")
			}
			src, dst := uint64(0), uint64(3)
			if d.dir == DirIn {
				src, dst = dst, src // DirIn walks the chain against its arrows
			}
			got, found, err := op.biBFSShortestPath(src, dst)
			if err != nil {
				t.Fatalf("control search: %v", err)
			}
			if !found {
				t.Fatalf("control: no %d→%d path under an all-admitting filter", src, dst)
			}
			if l := pathLen(got); l != 3 {
				t.Fatalf("control: path length %d, want 3", l)
			}
			validatePath(t, fwdCSR, got, src, dst, admitAll.admits)

			// The exclusion: drop the middle edge and the chain is severed.
			noMiddle := biFilterCase{"no middle", func(i int) bool { return i != middleEdge }}
			op = biOperatorFiltered(t, fwdCSR, revCSR, d.dir, "K", filterFor(fwdCSR, noMiddle))
			if !op.canBidirectional() {
				t.Fatal("the excluded-edge case fell back to the forward-only walk, so it no " +
					"longer tests the two-sided search's reverse type check")
			}
			if _, found, err = op.biBFSShortestPath(src, dst); err != nil {
				t.Fatalf("excluded search: %v", err)
			}
			if found {
				t.Fatalf("%d→%d found a path with edge %d excluded — the only route uses it, so "+
					"the reverse-side type check admitted an edge the filter rejects (the rmp "+
					"#2236 obstacle-2 defect, in the two-sided half)", src, dst, middleEdge)
			}
		})
	}
}

// TestBiBFS_HopBounds pins the interaction with minHops / maxHops, which the
// two-sided search evaluates against the JOINED length rather than against a
// single search's level counter.
func TestBiBFS_HopBounds(t *testing.T) {
	g := biTestGraph{6, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}}} // 0→5 is 5 hops

	cases := []struct {
		name             string
		minHops, maxHops int
		wantFound        bool
		wantLen          int
	}{
		{"unbounded", 1, shortestNoMaxHops, true, 5},
		{"maxHops exactly the distance", 1, 5, true, 5},
		{"maxHops one short", 1, 4, false, 0},
		{"minHops exactly the distance", 5, shortestNoMaxHops, true, 5},
		{"minHops above the distance", 6, shortestNoMaxHops, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := biOperator(t, g, DirOut)
			op.minHops, op.maxHops = tc.minHops, tc.maxHops

			gotBi, foundBi, err := op.biBFSShortestPath(0, 5)
			if err != nil {
				t.Fatalf("bidirectional: %v", err)
			}
			gotFwd, foundFwd, err := op.bfsShortestPathForward(0, 5)
			if err != nil {
				t.Fatalf("forward-only: %v", err)
			}
			if foundBi != foundFwd {
				t.Fatalf("found differs: bidirectional=%v forward-only=%v", foundBi, foundFwd)
			}
			if foundBi != tc.wantFound {
				t.Fatalf("found=%v, want %v", foundBi, tc.wantFound)
			}
			if !tc.wantFound {
				return
			}
			if got := pathLen(gotBi); got != tc.wantLen {
				t.Fatalf("bidirectional length %d, want %d", got, tc.wantLen)
			}
			if got := pathLen(gotFwd); got != tc.wantLen {
				t.Fatalf("forward-only length %d, want %d", got, tc.wantLen)
			}
		})
	}
}

// TestBiBFS_FallsBackWithoutAUsableReverseCSR pins the guard that stopped a
// placeholder reverse CSR from producing a silent "no path". Several in-tree
// tests construct a DirOut operator with buildCSR(n, nil) as its reverse,
// because before #2220 nothing read it.
func TestBiBFS_FallsBackWithoutAUsableReverseCSR(t *testing.T) {
	fwd := buildCSRWithHandles(4, [][2]int{{0, 1}, {1, 2}, {2, 3}})
	placeholder := buildCSRWithHandles(4, nil)

	op := NewShortestPath(biNoInput{}, StaticAdjacency(fwd, placeholder, nil), DirOut, 0, 1)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if op.canBidirectional() {
		t.Fatal("the two-sided search accepted an empty placeholder reverse CSR; it would " +
			"report no path for a connected pair")
	}
	got, found, err := op.bfsShortestPath(0, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !found {
		t.Fatal("no path found for 0→3, which is connected by three edges — the fallback did not engage")
	}
	if l := pathLen(got); l != 3 {
		t.Fatalf("path length %d, want 3", l)
	}
}
