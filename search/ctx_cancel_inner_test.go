package search

// ctx_cancel_inner_test.go — sprint #157, context-cancellation hardening.
//
// Regression tests for the inner-loop / long-traversal cancellation checks
// added to four traversals whose Ctx variant previously only polled ctx.Err()
// at a coarse outer boundary (or, for BFSDirectionOpt, not at all):
//
//   - #1304 BFSDirectionOptCtx — new Ctx variant; polls at the driver loop top
//     and at 4096-node granularity inside the bottom-up O(V) scan.
//   - #1305 DiameterCtx — polls before each per-vertex eccentricity BFS inside
//     an iFUB level, not just once per level.
//   - #1306 TarjanSCCCtx — polls every 4096 work-stack steps inside the inner
//     DFS loop, not just once per DFS root.
//   - #1307 CountTrianglesCtx — polls every 4096 candidate pairs in the inner
//     O(deg^2) pair-loop, not just every 4096 outer vertices.
//
// These checks only ADD cancellation; they never change the result of a
// non-cancelled run. Each cancelled case uses a deterministic, scheduler-
// independent context harness (see cancelAfterFirstCheck in
// dijkstra_ctx_cancel_test.go and cancelAfterNCalls below) so the assertion
// "cancelled inside the inner loop" does not depend on traversal speed.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// cancelAfterNCalls returns nil for its first n Err() calls and
// context.Canceled on every call thereafter. Unlike cancelAfterFirstCheck
// (which trips on the 2nd call), n lets a test step past a fixed number of
// pre-inner-loop polls so cancellation lands deterministically inside a
// specific inner loop. n is chosen empirically from the algorithm's poll
// sequence and documented at each call site.
type cancelAfterNCalls struct {
	context.Context
	n     int64
	calls atomic.Int64
}

func (c *cancelAfterNCalls) Err() error {
	if c.calls.Add(1) <= c.n {
		return nil
	}
	return context.Canceled
}

// --- #1304 BFSDirectionOptCtx --------------------------------------------

// TestBFSDirectionOptCtx_Cancel_DuringTraversal verifies that a context which
// reports cancellation after the first driver-loop poll causes
// BFSDirectionOptCtx to stop and return context.Canceled. The BA(10000,4,42)
// graph is the same power-law topology used by the BFS-DO equivalence test, so
// it provably enters the bottom-up phase; cancelAfterFirstCheck lets the first
// step run and the next poll (driver loop or the in-scan 4096-node check)
// observes the cancellation.
//
// Pre-fix BFSDirectionOpt had no ctx parameter and no Ctx sibling, so this test
// did not even compile against the old API; the new variant must honour
// cancellation rather than running the whole power-law traversal to completion.
func TestBFSDirectionOptCtx_Cancel_DuringTraversal(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(10_000, 4, 42).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not found in mapper")
	}

	ctx := &cancelAfterFirstCheck{Context: context.Background()}
	cerr := BFSDirectionOptCtx(ctx, c, srcID, func(graph.NodeID, int) bool { return true })
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cerr)
	}
}

// TestBFSDirectionOptCtx_Cancel_InBottomUpScan exercises the in-scan 4096-node
// poll specifically (distinct from the per-step driver-loop poll). On
// BA(50000,4,42) the traversal performs several bottom-up steps, each doing a
// full O(maxID) scan that polls ctx.Err() ~13 times; the vast majority of the
// ~44 total polls are therefore in-scan polls. cancelAfterNCalls(20) lets the
// 2-sweep, the early driver-loop steps, and part of a bottom-up scan run, then
// reports cancelled — a poll index that lands inside a bottom-up scan. The
// traversal must return context.Canceled.
//
// Without the in-scan poll, a cancellation arriving mid-scan would not be seen
// until the next driver-loop top, i.e. only after the rest of that O(maxID)
// scan completed — the very uninterruptible window this check closes. (Verified
// by disabling only the in-scan poll during development: this test then fails to
// observe the cancellation at the scan granularity.)
func TestBFSDirectionOptCtx_Cancel_InBottomUpScan(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(50_000, 4, 42).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not found in mapper")
	}

	ctx := &cancelAfterNCalls{Context: context.Background(), n: 20}
	cerr := BFSDirectionOptCtx(ctx, c, srcID, func(graph.NodeID, int) bool { return true })
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cerr)
	}
}

// TestBFSDirectionOptCtx_Cancel_PreCancelled verifies that a pre-cancelled
// context causes BFSDirectionOptCtx to return context.Canceled at the first
// driver-loop poll, before completing any step.
func TestBFSDirectionOptCtx_Cancel_PreCancelled(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(4_000, 4, 7).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not found in mapper")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cerr := BFSDirectionOptCtx(ctx, c, srcID, func(graph.NodeID, int) bool { return true })
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cerr)
	}
}

// TestBFSDirectionOptCtx_LiveContext_FullTraversal is the non-cancelled control:
// with a live context BFSDirectionOptCtx must visit exactly the same nodes as
// plain BFS and return a nil error (the cancellation checks must not perturb the
// result).
func TestBFSDirectionOptCtx_LiveContext_FullTraversal(t *testing.T) {
	t.Parallel()
	g, err := shapegen.BarabasiAlbert(10_000, 4, 42).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatalf("key 0 not found in mapper")
	}

	refDist := make(map[graph.NodeID]int, 10_000)
	BFS(c, srcID, func(node graph.NodeID, d int) bool {
		refDist[node] = d
		return true
	})

	doDist := make(map[graph.NodeID]int, 10_000)
	cerr := BFSDirectionOptCtx(context.Background(), c, srcID, func(node graph.NodeID, d int) bool {
		doDist[node] = d
		return true
	})
	if cerr != nil {
		t.Fatalf("live ctx: unexpected error %v", cerr)
	}
	if len(doDist) != len(refDist) {
		t.Fatalf("BFS-DO visited %d nodes, plain BFS visited %d", len(doDist), len(refDist))
	}
	for node, wantD := range refDist {
		if gotD, found := doDist[node]; !found || gotD != wantD {
			t.Fatalf("dist[%d] BFS-DO=%d/found=%v, plain BFS=%d", node, gotD, found, wantD)
		}
	}
}

// --- #1305 DiameterCtx ----------------------------------------------------

// TestDiameterCtx_Cancel_DuringInnerBFS verifies that cancellation is observed
// inside the iFUB per-vertex refinement loop, and that the per-vertex poll is
// load-bearing — the coarser per-level poll alone is not enough to interrupt a
// single wide level.
//
// A complete graph K1000 has diameter 1, so the iFUB refinement runs a single
// level (k == 1) that contains every non-centre vertex (~999): one giant level
// of ~999 inner eccentricity BFS sweeps. The poll sequence entering that level
// is: 2-sweep precondition (call 1), post-2-sweep (call 2), the level-1 per-level
// check (call 3); cancelAfterNCalls(3) reports cancelled from call 4 onward.
//
// With the per-vertex poll the very first inner BFS poll (call 4) observes the
// cancellation and the function returns context.Canceled having run no further
// sweeps. WITHOUT it the inner loop polls ctx.Err() zero times across all ~999
// sweeps; that single k == 1 iteration is the last, so the loop then exits and
// the function returns a NIL error after wasting the entire level — i.e. the
// cancel test fails (nil != Canceled) pre-fix. K1000 thus discriminates the
// per-vertex check from the per-level check it replaces.
func TestDiameterCtx_Cancel_DuringInnerBFS(t *testing.T) {
	t.Parallel()
	g, err := shapegen.Complete(1_000, false).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	ctx := &cancelAfterNCalls{Context: context.Background(), n: 3}
	_, _, _, cerr := DiameterCtx(ctx, c)
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cerr)
	}
}

// TestDiameterCtx_LiveContext_Unchanged is the non-cancelled control: the
// cancellation checks must not change the computed bounds. A 2000-cycle has
// diameter exactly 1000 and the refinement converges (exact == true).
func TestDiameterCtx_LiveContext_Unchanged(t *testing.T) {
	t.Parallel()
	g, err := shapegen.Cycle(2_000, false).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	lo, hi, exact, cerr := DiameterCtx(context.Background(), c)
	if cerr != nil {
		t.Fatalf("live ctx: unexpected error %v", cerr)
	}
	if lo != 1000 || hi != 1000 || !exact {
		t.Fatalf("Diameter(C2000) = (lo=%d, hi=%d, exact=%v); want (1000, 1000, true)", lo, hi, exact)
	}
}

// --- #1306 TarjanSCCCtx ---------------------------------------------------

// TestTarjanSCCCtx_Cancel_DuringInnerLoop verifies that cancellation is observed
// inside the inner work-stack loop of a single giant SCC. A directed cycle Cn is
// one SCC explored from a single root, so the outer per-root poll fires exactly
// once (call 1, nil under cancelAfterFirstCheck) and the inner work-stack poll
// (popCount==0, call 2) observes the cancellation. Pre-fix the inner loop had no
// poll, so the entire O(V+E) traversal of the cycle ran as one uninterruptible
// outer iteration.
func TestTarjanSCCCtx_Cancel_DuringInnerLoop(t *testing.T) {
	t.Parallel()
	g, err := shapegen.Cycle(20_000, true).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	ctx := &cancelAfterFirstCheck{Context: context.Background()}
	_, cerr := TarjanSCCCtx(ctx, c)
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cerr)
	}
}

// TestTarjanSCCCtx_LiveContext_Unchanged is the non-cancelled control: a single
// directed ring must still be recognised as exactly one SCC containing all n
// vertices.
func TestTarjanSCCCtx_LiveContext_Unchanged(t *testing.T) {
	t.Parallel()
	const n = 20_000
	g, err := shapegen.Cycle(n, true).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	sccs, cerr := TarjanSCCCtx(context.Background(), c)
	if cerr != nil {
		t.Fatalf("live ctx: unexpected error %v", cerr)
	}
	if len(sccs) != 1 || len(sccs[0]) != n {
		t.Fatalf("TarjanSCC(C%d): got %d SCCs (first size %d); want 1 SCC of size %d",
			n, len(sccs), firstLen(sccs), n)
	}
}

func firstLen(sccs [][]graph.NodeID) int {
	if len(sccs) == 0 {
		return 0
	}
	return len(sccs[0])
}

// --- #1307 CountTrianglesCtx ----------------------------------------------

// TestCountTrianglesCtx_Cancel_InHubPairLoop verifies that cancellation is
// observed inside the inner O(deg^2) pair-loop, and that polling on inner work
// (candidate pairs) rather than on outer vertices is load-bearing.
//
// A complete graph K300 has only 300 vertices but ~4.4M candidate pairs, almost
// all concentrated in the lowest-ranked vertices' pair-loops (in the degree-asc
// canonical order the lowest-rank vertex examines C(299,2) higher-ranked pairs).
// The pre-loop poll consumes call 1 (nil under cancelAfterFirstCheck) and the
// first candidate-pair poll (loops==0, call 2) observes the cancellation.
//
// WITHOUT the fix the poll counter advances per OUTER vertex; with only 300
// vertices it never reaches the 4096 stride, so the in-loop check fires zero
// times and the whole ~4.4M-pair computation runs to completion and returns a
// nil error — the cancel test fails (nil != Canceled) pre-fix. K300 thus
// discriminates the inner-work poll from the per-outer-vertex poll it replaces.
func TestCountTrianglesCtx_Cancel_InHubPairLoop(t *testing.T) {
	t.Parallel()
	g, err := shapegen.Complete(300, false).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	ctx := &cancelAfterFirstCheck{Context: context.Background()}
	total, perNode, cerr := CountTrianglesCtx(ctx, c)
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cerr)
	}
	if total != 0 || perNode != nil {
		t.Fatalf("cancelled run must return zero result; got total=%d perNode=%v", total, perNode)
	}
}

// TestCountTrianglesCtx_LiveContext_Unchanged is the non-cancelled control: a
// complete graph K_m has exactly C(m, 3) triangles. The cancellation checks
// must not change this count. m = 300 → C(300, 3) = 4,455,100.
func TestCountTrianglesCtx_LiveContext_Unchanged(t *testing.T) {
	t.Parallel()
	const m = 300
	g, err := shapegen.Complete(m, false).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	total, _, cerr := CountTrianglesCtx(context.Background(), c)
	if cerr != nil {
		t.Fatalf("live ctx: unexpected error %v", cerr)
	}
	// Number of triangles in a complete graph K_m is m*(m-1)*(m-2)/6,
	// i.e. 4_455_100 for m = 300.
	const want = int64(m) * (m - 1) * (m - 2) / 6
	if total != want {
		t.Fatalf("CountTriangles(K%d) = %d; want %d", m, total, want)
	}
}
