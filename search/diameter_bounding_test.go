package search

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"

	"pgregory.net/rapid"
)

// diameterOracle is the brute-force reference for [DiameterCtx]: the seed is
// the lowest NodeID with an incident edge, and the result is the largest BFS
// eccentricity over the seed's component; a graph without edges gives 0. Its
// BFS shares no code with the implementation under test.
func diameterOracle(c *csr.CSR[struct{}]) int {
	n := int(c.MaxNodeID())
	verts, edges := c.VerticesSlice(), c.EdgesSlice()
	seed := -1
	for v := 0; v < n; v++ {
		if verts[v+1] > verts[v] {
			seed = v
			break
		}
	}
	if seed < 0 {
		return 0
	}
	dist := make([]int, n)
	queue := make([]int, 0, n)
	bfs := func(src int) (ecc int, visited []int) {
		for i := range dist {
			dist[i] = -1
		}
		dist[src] = 0
		queue = append(queue[:0], src)
		for h := 0; h < len(queue); h++ {
			v := queue[h]
			for k := verts[v]; k < verts[v+1]; k++ {
				w := int(edges[k])
				if dist[w] < 0 {
					dist[w] = dist[v] + 1
					ecc = max(ecc, dist[w])
					queue = append(queue, w)
				}
			}
		}
		return ecc, queue
	}
	_, visited := bfs(seed)
	comp := append([]int(nil), visited...)
	best := 0
	for _, v := range comp {
		e, _ := bfs(v)
		best = max(best, e)
	}
	return best
}

// diameterTestGraph is an undirected multigraph under construction: n
// vertices keyed 0..n-1, an optional isolated key interned first (so the
// lowest NodeID is dead), and an edge list that may hold self-loops and
// parallel edges.
type diameterTestGraph struct {
	edges    [][2]int
	n        int
	deadLow  bool
	shapeTag string
}

func (g *diameterTestGraph) build(tb diameterTB) *csr.CSR[struct{}] {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{})
	if g.deadLow {
		if err := a.AddNode(-1); err != nil {
			tb.Fatalf("AddNode(-1): %v", err)
		}
	}
	for i := 0; i < g.n; i++ {
		if err := a.AddNode(i); err != nil {
			tb.Fatalf("AddNode(%d): %v", i, err)
		}
	}
	for _, e := range g.edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			tb.Fatalf("AddEdge(%d,%d): %v", e[0], e[1], err)
		}
	}
	return csr.BuildFromAdjList(a).BuildSymmetric()
}

// diameterShape fills g with one of the battery's shapes over n vertices,
// drawing every random choice from r.
func diameterShape(g *diameterTestGraph, shape, n int, r *rand.Rand, p float64) {
	add := func(u, v int) { g.edges = append(g.edges, [2]int{u, v}) }
	switch shape {
	case 0: // G(n, p)
		g.shapeTag = "gnp"
		for u := 0; u < n; u++ {
			for v := u + 1; v < n; v++ {
				if r.Float64() < p {
					add(u, v)
				}
			}
		}
	case 1: // random recursive tree
		g.shapeTag = "tree"
		for v := 1; v < n; v++ {
			add(r.IntN(v), v)
		}
	case 2: // path
		g.shapeTag = "path"
		for v := 1; v < n; v++ {
			add(v-1, v)
		}
	case 3: // cycle (odd or even with n)
		g.shapeTag = "cycle"
		if n >= 3 {
			for v := 0; v < n; v++ {
				add(v, (v+1)%n)
			}
		}
	case 4: // star
		g.shapeTag = "star"
		for v := 1; v < n; v++ {
			add(0, v)
		}
	case 5: // complete graph
		g.shapeTag = "complete"
		for u := 0; u < n; u++ {
			for v := u + 1; v < n; v++ {
				add(u, v)
			}
		}
	case 6: // communities joined by single bridges (a barbell for two cliques)
		g.shapeTag = "bridged"
		parts := 2 + r.IntN(3)
		size := max(n/parts, 1)
		for c := 0; c*size < n; c++ {
			lo, hi := c*size, min((c+1)*size, n)
			for u := lo; u < hi; u++ {
				for v := u + 1; v < hi; v++ {
					if r.Float64() < 0.5+p {
						add(u, v)
					}
				}
			}
			if lo > 0 {
				add(lo-1-r.IntN(size), lo+r.IntN(hi-lo))
			}
		}
	default: // Barabási–Albert, m in {1, 2}
		m := 1 + shape - 7
		g.shapeTag = "ba"
		var ends []int
		for v := 1; v < n; v++ {
			for j := 0; j < min(m, v); j++ {
				u := r.IntN(v)
				if len(ends) > 0 && r.IntN(2) == 0 {
					u = ends[r.IntN(len(ends))]
				}
				add(u, v)
				ends = append(ends, u, v)
			}
		}
	}
}

// diameterTB is the subset of testing.TB that both *testing.T and *rapid.T
// provide.
type diameterTB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// diameterWorkerCounts is the batch-size set every case is run under.
func diameterWorkerCounts() []int { return []int{1, 4, runtime.GOMAXPROCS(0)} }

// checkDiameterCase asserts the full contract on one graph and returns
// whether a mid-run cancellation (after at least one completed sweep) was
// observed.
func checkDiameterCase(tb diameterTB, c *csr.CSR[struct{}], cancelAfter int, label string) (midRunCancel bool) {
	tb.Helper()
	want := diameterOracle(c)
	lo, hi, exact, err := DiameterCtx(context.Background(), c)
	if err != nil || !exact || lo != want || hi != want {
		tb.Fatalf("%s: DiameterCtx = (%d, %d, %t, %v), oracle %d", label, lo, hi, exact, err, want)
	}
	for _, k := range diameterWorkerCounts() {
		lo, hi, exact, sweeps, err := boundingDiameters(context.Background(), c, k)
		if err != nil || !exact || lo != want || hi != want {
			tb.Fatalf("%s k=%d: (%d, %d, %t, %v), oracle %d", label, k, lo, hi, exact, err, want)
		}
		if want > 0 && sweeps < 1 {
			tb.Fatalf("%s k=%d: %d sweeps on a graph with edges", label, k, sweeps)
		}

		cctx := &cancelAfterNCalls{Context: context.Background(), n: int64(cancelAfter)}
		lo, hi, exact, sweeps, err = boundingDiameters(cctx, c, k)
		if err == nil {
			if !exact || lo != want || hi != want {
				tb.Fatalf("%s k=%d uncancelled: (%d, %d, %t), oracle %d", label, k, lo, hi, exact, want)
			}
			continue
		}
		if !errors.Is(err, context.Canceled) || exact {
			tb.Fatalf("%s k=%d cancelled: exact=%t err=%v, want exact=false and context.Canceled", label, k, exact, err)
		}
		if sweeps == 0 { // cancelled before the seed sweep: no bound exists yet
			if lo != 0 || hi != 0 {
				tb.Fatalf("%s k=%d cancelled before any sweep: (%d, %d), want (0, 0)", label, k, lo, hi)
			}
			continue
		}
		if lo > want || hi < want || lo > hi {
			tb.Fatalf("%s k=%d cancelled after %d checks: (%d, %d, %t, %v) does not bracket oracle %d",
				label, k, cancelAfter, lo, hi, exact, err, want)
		}
		if sweeps > 0 {
			midRunCancel = true
		}
	}
	return midRunCancel
}

// TestDiameterCtx_BoundingMatchesOracle is the property test for the
// BoundingDiameters rewrite (rmp #3085): on random shapes of 0 to 64
// vertices, with injected self-loops, parallel edges and a dead lowest
// NodeID, the result equals the brute-force oracle exactly for every batch
// size, and a cancelled run brackets it.
func TestDiameterCtx_BoundingMatchesOracle(t *testing.T) {
	t.Parallel()
	perCheck := 5
	if testing.Short() {
		perCheck = 25 // rapid runs a fifth of its checks under -short
	}
	var cases, midRun atomic.Int64
	rapid.Check(t, func(rt *rapid.T) {
		for i := 0; i < perCheck; i++ {
			n := rapid.IntRange(0, 64).Draw(rt, "n")
			shape := rapid.IntRange(0, 8).Draw(rt, "shape")
			p := rapid.Float64Range(0.02, 0.5).Draw(rt, "p")
			seed := rapid.Uint64().Draw(rt, "seed")
			g := &diameterTestGraph{n: n, deadLow: rapid.Bool().Draw(rt, "deadLow")}
			r := rand.New(rand.NewPCG(seed, 3085)) //nolint:gosec // deterministic test RNG
			diameterShape(g, shape, n, r, p)
			if n > 0 {
				for j := rapid.IntRange(0, 3).Draw(rt, "loops"); j > 0; j-- {
					v := r.IntN(n)
					g.edges = append(g.edges, [2]int{v, v})
				}
				if len(g.edges) > 0 {
					for j := rapid.IntRange(0, 3).Draw(rt, "parallel"); j > 0; j-- {
						g.edges = append(g.edges, g.edges[r.IntN(len(g.edges))])
					}
				}
			}
			cancelAfter := rapid.IntRange(0, 12).Draw(rt, "cancelAfter")
			if checkDiameterCase(rt, g.build(rt), cancelAfter, g.shapeTag) {
				midRun.Add(1)
			}
			cases.Add(1)
		}
	})
	if got := cases.Load(); got < 500 {
		t.Fatalf("property ran %d graph cases, want at least 500", got)
	}
	if midRun.Load() == 0 {
		t.Fatal("no case was cancelled after a completed sweep: the cancellation bound was never exercised")
	}
	t.Logf("%d graph cases, %d with a mid-run cancellation", cases.Load(), midRun.Load())
}

// TestDiameterCtx_BoundingFixedSeeds2000 checks fixed seeds 1-32 at 2000
// vertices against the oracle. 2000 vertices clears
// diameterParallelMinComponent on connected shapes, so DiameterCtx's own
// worker selection takes the batched arm.
func TestDiameterCtx_BoundingFixedSeeds2000(t *testing.T) {
	t.Parallel()
	const n = 2000
	for seed := uint64(1); seed <= 32; seed++ {
		r := rand.New(rand.NewPCG(seed, 2000)) //nolint:gosec // deterministic test RNG
		g := &diameterTestGraph{n: n, deadLow: seed%5 == 0}
		switch seed % 4 {
		case 0:
			diameterShape(g, 0, n, r, 3.0/n) // sparse G(n,p): disconnected
		case 1:
			diameterShape(g, 7, n, r, 0) // BA m=1 (a tree)
		case 2:
			diameterShape(g, 8, n, r, 0) // BA m=2
		default:
			diameterShape(g, 6, n, r, 0.002) // bridged dense communities
		}
		c := g.build(t)
		checkDiameterCase(t, c, int(seed%7)+1, g.shapeTag)
	}
}

// TestDiameterCtx_ParallelBatchStillExact runs the batched worker arm on a
// spider (a hub with equal legs) below diameterParallelMinComponent, by
// fixing the batch size at GOMAXPROCS, and checks the exact diameter.
func TestDiameterCtx_ParallelBatchStillExact(t *testing.T) {
	t.Parallel()
	const (
		legs   = 12
		length = 24
	)
	a := adjlist.New[int, struct{}](adjlist.Config{})
	key := 1
	for l := 0; l < legs; l++ {
		prev := 0
		for i := 0; i < length; i++ {
			if err := a.AddEdge(prev, key, struct{}{}); err != nil {
				t.Fatalf("AddEdge(%d,%d): %v", prev, key, err)
			}
			prev = key
			key++
		}
	}
	c := csr.BuildFromAdjList(a).BuildSymmetric()
	lo, hi, exact, _, err := boundingDiameters(context.Background(), c, max(runtime.GOMAXPROCS(0), 2))
	if err != nil {
		t.Fatalf("boundingDiameters: %v", err)
	}
	if want := bruteDiameter(c); lo != want || hi != want || !exact {
		t.Fatalf("boundingDiameters = (%d, %d, %t), brute force = %d", lo, hi, exact, want)
	}
}
