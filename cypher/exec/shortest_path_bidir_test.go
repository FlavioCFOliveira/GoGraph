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
)

// biCSR is a minimal [csrAdjacency] for these tests. The shared test helpers
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

// csrPair builds the forward and reverse CSRs. Handles are assigned per edge so
// a parallel edge is distinguishable, which is what relationship-uniqueness and
// the reverse→forward position mapping both depend on.
func (g biTestGraph) csrPair() (fwd, rev *biCSR) {
	fwd = buildCSRWithHandles(g.n, g.edges)
	rev = buildCSRWithHandles(g.n, flipEdges(g.edges))
	return fwd, rev
}

func flipEdges(edges [][2]int) [][2]int {
	out := make([][2]int, len(edges))
	for i, e := range edges {
		out[i] = [2]int{e[1], e[0]}
	}
	return out
}

// buildCSRWithHandles is buildCSR plus a stable per-edge handle. The handle is
// the edge's index in the ORIGINAL list, so the same logical edge carries the
// same handle in the forward and reverse CSRs — the invariant buildRevToFwd
// relies on to pair a reverse slot with its forward counterpart.
func buildCSRWithHandles(maxNode int, edgeList [][2]int) *biCSR {
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
	op := NewShortestPath(biNoInput{}, fwd, rev, dir, 0, 1)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return op
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
// This is the half of the comparison a length check cannot do. It is written
// against the graph rather than against the other algorithm, so it stays a valid
// oracle even if both algorithms were wrong in the same way.
func validatePath(t *testing.T, fwd *biCSR, v expr.Value, src, dst uint64) {
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
		fwdPos := uint64(lv[1+VLEHopStride*i].(expr.IntegerValue))
		next := uint64(lv[2+VLEHopStride*i].(expr.IntegerValue))
		dir := int64(lv[3+VLEHopStride*i].(expr.IntegerValue))

		if fwdPos >= uint64(len(fwd.edges)) {
			t.Fatalf("hop %d: forward position %d out of range", i, fwdPos)
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
		cur = next
	}
	if cur != dst {
		t.Fatalf("path ends at %d, want dst %d", cur, dst)
	}
}

// TestBiBFS_DifferentialAgainstForwardOnly is the core gate. For every pair in
// every generated graph, the two algorithms must agree on reachability and on
// path LENGTH, and the two-sided path must independently validate.
func TestBiBFS_DifferentialAgainstForwardOnly(t *testing.T) {
	// Only DirOut admits the two-sided search (see canBidirectional). DirIn and
	// DirBoth are still exercised, as the negative half of the gate: they must
	// keep taking the forward-only walk, so a future widening cannot land without
	// this file changing with it.
	dirs := []struct {
		name    string
		dir     Direction
		twoSide bool
	}{
		{"DirOut", DirOut, true},
		{"DirIn", DirIn, false},
		{"DirBoth", DirBoth, false},
	}

	for _, gc := range biGraphCases(t) {
		for _, d := range dirs {
			t.Run(gc.name+"/"+d.name, func(t *testing.T) {
				op := biOperator(t, gc.g, d.dir)
				if got := op.canBidirectional(); got != d.twoSide {
					t.Fatalf("canBidirectional = %v, want %v — the admission gate moved without "+
						"this test moving with it (revVerts=%d/%d revEdges=%d/%d)",
						got, d.twoSide, len(op.revVerts), len(op.fwdVerts), len(op.revEdges), len(op.fwdEdges))
				}
				if !d.twoSide {
					return // the forward-only walk serves this mode; nothing to differ against
				}
				fwdCSR, _ := gc.g.csrPair()

				for src := 0; src < gc.g.n; src++ {
					for dst := 0; dst < gc.g.n; dst++ {
						if src == dst {
							continue // the cycle search, out of scope for #2220
						}
						s, dd := uint64(src), uint64(dst)

						gotBi, foundBi, err := op.biBFSShortestPath(s, dd)
						if err != nil {
							t.Fatalf("bidirectional %d→%d: %v", src, dst, err)
						}
						gotFwd, foundFwd, err := op.bfsShortestPathForward(s, dd)
						if err != nil {
							t.Fatalf("forward-only %d→%d: %v", src, dst, err)
						}

						if foundBi != foundFwd {
							t.Fatalf("%d→%d: reachability differs — bidirectional=%v forward-only=%v",
								src, dst, foundBi, foundFwd)
						}
						if !foundBi {
							continue
						}
						if lb, lf := pathLen(gotBi), pathLen(gotFwd); lb != lf {
							t.Fatalf("%d→%d: path LENGTH differs — bidirectional=%d forward-only=%d",
								src, dst, lb, lf)
						}
						validatePath(t, fwdCSR, gotBi, s, dd)
						validatePath(t, fwdCSR, gotFwd, s, dd)
					}
				}
			})
		}
	}
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

// TestBiBFS_TypeFilterKeepsTheForwardOnlyWalk pins the narrowing recorded on
// canBidirectional: a relationship-type filter disables the two-sided search.
//
// This is the negative gate for a defect the differential suite found and this
// task chose not to paper over. Resolving a reverse slot's forward position for
// the filter is either slower than making no change at all (the prebuilt table,
// a measured 26% end-to-end regression) or occasionally wrong (the per-slot
// scan's unresolvable case, which admitted edges the filter excludes — the
// two-sided search found paths the forward-only walk correctly rejected).
//
// The test also proves the fallback still answers correctly under a filter, so
// the narrowing costs nothing but speed.
func TestBiBFS_TypeFilterKeepsTheForwardOnlyWalk(t *testing.T) {
	g := biTestGraph{6, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}}}
	fwdCSR, _ := g.csrPair()

	op := biOperator(t, g, DirOut)
	if !op.canBidirectional() {
		t.Fatal("the untyped control is not taking the two-sided search, so this test cannot " +
			"show that the FILTER is what disables it")
	}
	// Admit every slot, so the filter changes nothing semantically and any
	// difference is attributable to the gate rather than to the graph.
	all := map[uint64]string{}
	for pos := range fwdCSR.edges {
		all[uint64(pos)] = "K"
	}
	op.WithTypeFilter("K", all)
	if op.canBidirectional() {
		t.Fatal("a type-filtered search took the two-sided path. It must not until the " +
			"reverse-slot type check is both exact and cheaper than the revToFwd table — " +
			"see canBidirectional for the two measurements that rejected the alternatives")
	}

	got, found, err := op.bfsShortestPath(0, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !found {
		t.Fatal("no path found under an all-admitting filter")
	}
	if l := pathLen(got); l != 5 {
		t.Fatalf("path length %d, want 5", l)
	}
	validatePath(t, fwdCSR, got, 0, 5)
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

	op := NewShortestPath(biNoInput{}, fwd, placeholder, DirOut, 0, 1)
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
