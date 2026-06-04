package centrality

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// pprPushReference is a faithful copy of the pre-compaction PPR push loop:
// an append-only worklist consumed via a read cursor that is never reset,
// so the backing array grows toward MaxSteps total pushes. It exists only
// to serve as the equivalence oracle for the compacting production loop —
// TestPPRPush_CompactionPreservesResults asserts the two agree bit-for-bit.
//
// Keep this in lock-step with the push math in
// PersonalisedPushPageRankCtx (everything except the worklist memory
// management). If the algorithm changes, this oracle must change with it.
func pprPushReference[W any](c *csr.CSR[W], src graph.NodeID, opts PPRPushOptions) []float64 {
	if opts.Damping == 0 {
		opts.Damping = 0.85
	}
	if opts.Epsilon == 0 {
		opts.Epsilon = 1e-6
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 10_000_000
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	if n <= 0 || uint64(src)+1 >= uint64(len(verts)) {
		return nil
	}
	rank := make([]float64, n)
	res := make([]float64, n)
	res[uint64(src)] = 1
	queue := []int{int(src)}
	inQ := make([]bool, n)
	inQ[uint64(src)] = true

	enqueueIfHot := func(node int) {
		if inQ[node] {
			return
		}
		deg := verts[node+1] - verts[node]
		var hot bool
		if deg == 0 {
			hot = res[node] >= opts.Epsilon
		} else {
			hot = res[node]/float64(deg) >= opts.Epsilon
		}
		if hot {
			queue = append(queue, node)
			inQ[node] = true
		}
	}

	steps := 0
	for qh := 0; qh < len(queue) && steps < opts.MaxSteps; qh++ {
		v := queue[qh]
		inQ[v] = false
		rv := res[v]
		deg := int(verts[v+1] - verts[v])
		if deg == 0 {
			rank[v] += (1 - opts.Damping) * rv
			res[v] = 0
			res[uint64(src)] += opts.Damping * rv
			enqueueIfHot(int(src))
			steps++
			continue
		}
		if rv/float64(deg) < opts.Epsilon {
			continue
		}
		rank[v] += (1 - opts.Damping) * rv
		share := opts.Damping * rv / float64(deg)
		res[v] = 0
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			res[w] += share
			enqueueIfHot(w)
		}
		steps++
	}
	return rank
}

// pprEquivCase describes one equivalence fixture: a CSR plus its source.
type pprEquivCase struct {
	name string
	csr  *csr.CSR[int64]
	src  graph.NodeID
}

// buildPPREquivCases assembles a spread of graph shapes, including a dense
// clique, on which the compacting and reference loops must agree exactly.
func buildPPREquivCases(t *testing.T) []pprEquivCase {
	t.Helper()
	mk := func(name string, sh shapegen.Shape[int, int64], directed bool, src int) pprEquivCase {
		g, err := sh.Build(adjlist.Config{Directed: directed})
		if err != nil {
			t.Fatalf("%s build: %v", name, err)
		}
		c := csr.BuildFromAdjList(g.AdjList())
		s, ok := g.AdjList().Mapper().Lookup(src)
		if !ok {
			t.Fatalf("%s: source %d not interned", name, src)
		}
		return pprEquivCase{name: name, csr: c, src: s}
	}
	return []pprEquivCase{
		mk("dense-clique-64", shapegen.Complete(64, false), false, 0),
		mk("dense-clique-128-directed", shapegen.Complete(128, true), true, 7),
		mk("path-200", shapegen.Path(200, false), false, 0),
		mk("star-out-300", shapegen.Star(300, true), true, 0),
		mk("grid-20x20", shapegen.Grid(20, 20, true), false, 0),
		mk("rmat-scale10", shapegen.RMAT(10, 8, 57, 19, 19, 5, 42), true, 1),
	}
}

// floatSliceEqual reports whether two rank vectors are bit-for-bit equal.
// Bit equality (not tolerance) is the contract: compaction is a pure
// memory optimisation and must not perturb a single ULP. NaN never occurs
// in a valid PPR result, so a plain == comparison is the right oracle.
func floatSliceEqual(a, b []float64) (int, bool) {
	if len(a) != len(b) {
		return -1, false
	}
	for i := range a {
		if a[i] != b[i] {
			return i, false
		}
	}
	return -1, true
}

// TestPPRPush_CompactionPreservesResults is the key gate: the compacting
// production loop must return a rank vector bit-for-bit identical to the
// append-only reference loop, on every shape including a dense clique.
// This proves the worklist compaction did not alter the algorithm output.
func TestPPRPush_CompactionPreservesResults(t *testing.T) {
	t.Parallel()
	opts := DefaultPPRPushOptions()
	for _, tc := range buildPPREquivCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := PersonalisedPushPageRank(tc.csr, tc.src, opts)
			if err != nil {
				t.Fatalf("PersonalisedPushPageRank: %v", err)
			}
			want := pprPushReference(tc.csr, tc.src, opts)
			if idx, ok := floatSliceEqual(got, want); !ok {
				if idx < 0 {
					t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
				}
				t.Fatalf("rank[%d] differs: got %v, want %v (bit-for-bit equality required)",
					idx, got[idx], want[idx])
			}
		})
	}
}

// TestPPRPush_CompactionPreservesResults_VariedParams repeats the
// equivalence check across several (Damping, Epsilon) settings, since a
// tighter epsilon lengthens the worklist and exercises more compactions.
func TestPPRPush_CompactionPreservesResults_VariedParams(t *testing.T) {
	t.Parallel()
	g, err := shapegen.Complete(96, false).Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	src, _ := g.AdjList().Mapper().Lookup(0)

	params := []PPRPushOptions{
		{Damping: 0.85, Epsilon: 1e-4},
		{Damping: 0.85, Epsilon: 1e-7},
		{Damping: 0.50, Epsilon: 1e-6},
		{Damping: 0.99, Epsilon: 1e-8},
	}
	for _, opts := range params {
		got, err := PersonalisedPushPageRank(c, src, opts)
		if err != nil {
			t.Fatalf("opts %+v: %v", opts, err)
		}
		want := pprPushReference(c, src, opts)
		if idx, ok := floatSliceEqual(got, want); !ok {
			t.Fatalf("opts %+v: rank[%d] differs: got %v, want %v",
				opts, idx, got[idx], want[idx])
		}
	}
}

// withPPRWorklistObserver installs a test-only observer that records the
// peak worklist length and capacity across a single PPR call, restoring
// the previous observer (nil in production) on cleanup. The package's
// tests are not run in parallel against each other while the global is
// installed, so the t.Cleanup restore is sufficient.
func withPPRWorklistObserver(t *testing.T) *worklistPeak {
	t.Helper()
	peak := &worklistPeak{}
	prev := pprWorklistObserver
	pprWorklistObserver = peak.observe
	t.Cleanup(func() { pprWorklistObserver = prev })
	return peak
}

type worklistPeak struct {
	maxLen int
	maxCap int
}

func (p *worklistPeak) observe(qlen, qcap int) {
	if qlen > p.maxLen {
		p.maxLen = qlen
	}
	if qcap > p.maxCap {
		p.maxCap = qcap
	}
}

// TestPPRPush_WorklistTracksFrontier asserts the memory bound (the AC):
// on a dense graph the peak worklist capacity stays near the live frontier
// (≈ the number of nodes) rather than growing toward the total push count.
//
// Construction: a dense clique drives a large number of pushes — far more
// than the node count — because every popped node re-activates its
// neighbours, so the append-only loop's backing array would grow toward
// the push total. With compaction, the live frontier is bounded by the
// node count (inQ dedups membership), so the worklist length and capacity
// stay within a small constant factor of n.
//
// This test FAILS against the pre-fix append-only loop (peakCap grows with
// the push count, far above the node bound) and PASSES with compaction.
func TestPPRPush_WorklistTracksFrontier(t *testing.T) {
	const order = 200 // dense clique: order*(order-1) directed arcs
	g, err := shapegen.Complete(order, true).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	src, _ := g.AdjList().Mapper().Lookup(0)

	// Tight epsilon so the push wavefront keeps re-activating nodes,
	// maximising total pushes and thus the pre-fix worklist growth.
	opts := PPRPushOptions{Damping: 0.85, Epsilon: 1e-9, MaxSteps: 10_000_000}

	peak := withPPRWorklistObserver(t)
	rank, err := PersonalisedPushPageRank(c, src, opts)
	if err != nil {
		t.Fatalf("PersonalisedPushPageRank: %v", err)
	}
	if rank == nil {
		t.Fatal("nil rank")
	}

	// The live frontier can hold at most `order` distinct nodes (inQ
	// dedups), plus a small slack for the about-to-be-consumed head and
	// the compaction floor. We allow a generous constant factor and an
	// additive floor term, but cap well below the push total.
	bound := 4*order + pprCompactFloor
	if peak.maxCap > bound {
		t.Fatalf("peak worklist cap %d exceeds frontier bound %d (order=%d); "+
			"worklist is growing with total pushes, not the live frontier",
			peak.maxCap, bound, order)
	}
	if peak.maxLen > bound {
		t.Fatalf("peak worklist len %d exceeds frontier bound %d (order=%d)",
			peak.maxLen, bound, order)
	}
	if peak.maxLen == 0 {
		t.Fatal("observer never fired; worklist instrumentation not wired")
	}
	t.Logf("dense clique order=%d: peak worklist len=%d cap=%d (bound=%d)",
		order, peak.maxLen, peak.maxCap, bound)
}

// TestCompactWorklist_Unit exercises the compaction helper directly: it
// preserves element order, fires only above the floor and the half-prefix
// threshold, and resets the cursor.
func TestCompactWorklist_Unit(t *testing.T) {
	t.Parallel()

	t.Run("below-floor-is-noop", func(t *testing.T) {
		t.Parallel()
		q := make([]int, pprCompactFloor-1)
		gotQ, gotH := compactWorklist(q, len(q)-1) // qh > len/2 but below floor
		if len(gotQ) != len(q) || gotH != len(q)-1 {
			t.Fatalf("below-floor must be no-op: got len=%d qh=%d", len(gotQ), gotH)
		}
	})

	t.Run("small-prefix-is-noop", func(t *testing.T) {
		t.Parallel()
		q := make([]int, 4*pprCompactFloor)
		qh := len(q)/2 - 1 // qh <= len/2: must not compact
		gotQ, gotH := compactWorklist(q, qh)
		if len(gotQ) != len(q) || gotH != qh {
			t.Fatalf("qh<=len/2 must be no-op: got len=%d qh=%d", len(gotQ), gotH)
		}
	})

	t.Run("compacts-and-preserves-order", func(t *testing.T) {
		t.Parallel()
		// Build a worklist where the consumed prefix dominates.
		total := 4 * pprCompactFloor
		q := make([]int, total)
		for i := range q {
			q[i] = i // distinct values to detect reordering
		}
		qh := total - pprCompactFloor // unconsumed tail = last pprCompactFloor entries
		wantTail := append([]int(nil), q[qh:]...)

		gotQ, gotH := compactWorklist(q, qh)
		if gotH != 0 {
			t.Fatalf("cursor not reset: got %d, want 0", gotH)
		}
		if len(gotQ) != len(wantTail) {
			t.Fatalf("length after compaction = %d, want %d", len(gotQ), len(wantTail))
		}
		for i := range gotQ {
			if gotQ[i] != wantTail[i] {
				t.Fatalf("order not preserved at %d: got %d, want %d", i, gotQ[i], wantTail[i])
			}
		}
	})
}

// TestPPRPush_CancellationStillWorks confirms the context cancellation
// path is unaffected by the new compaction/observer code in the loop head.
func TestPPRPush_CancellationStillWorks(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 50; i++ {
		for j := 0; j < 50; j++ {
			if i != j {
				if err := a.AddEdge(i, j, struct{}{}); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	_, err := PersonalisedPushPageRankCtx(ctx, c, src, PPRPushOptions{Epsilon: 1e-9})
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
}
