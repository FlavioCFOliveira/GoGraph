package search

// ctx_cancel_entry_test.go — sprint 349, rmp #2593.
//
// Regression tests for the five entry points whose Ctx variant could not
// honour cancellation below its 4096-iteration poll stride, in breach of the
// EXTREME / MASSIVE Concurrent Ready mandate's context-aware-blocking rule
// (CLAUDE.md). Four shared one root cause — increment-then-mask, so the
// counter was already 1 on the first iteration and the mask was first true
// only at 4096 — and the fifth returned a domain error in preference to the
// context error:
//
//   - #2593 BellmanFordCtx / BellmanFordInto — bellmanFordCore polled the SPFA
//     deque loop AFTER incrementing yieldCtr, so no poll happened before the
//     4096th dequeue. Measured: nil error at 8 / 64 / 1500 nodes under an
//     already-cancelled context, with the FULL result returned.
//   - #2593 KCoreCtx — same ordering defect on the bucket-peel loop.
//   - #2593 KShortestPathsLooplessCtxWithOpts — same ordering defect on the
//     best-first pop loop, which was also the SOLE poll site: there was no
//     entry poll, so the unbounded, worst-case-exponential k-shortest family
//     (KShortestPathsLooplessCtx and the deprecated EppsteinKShortestCtx both
//     delegate here) could not be cancelled at all on a small frontier.
//   - #2593 TopologicalSortCtx — a distinct mechanism: on a fully cyclic graph
//     every live vertex has indegree >= 1, so the Kahn queue starts empty, the
//     polled loop never runs, and ErrCycle was returned in preference to the
//     context error at EVERY graph size.
//
// (flow.PushRelabelMaxFlowCtx carried the same ordering defect and is pinned
// by push_relabel_ctx_cancel_test.go in the flow package.)
//
// Every fixture here is 8 nodes — BELOW every poll stride in the package, so
// each cancelled assertion fails on the pre-fix code and passes on the fixed
// code. The instrument is a REAL pre-cancelled context.WithCancel, not the
// Err()-overriding harnesses in this package (cancelAfterFirstCheck in
// dijkstra_ctx_cancel_test.go, cancelAfterNCalls in ctx_cancel_inner_test.go):
// those embed a live context.Background() and override Err() only on the
// wrapper, which does not propagate into a derived context.
//
// Each entry point is asserted twice: once with a LIVE context, pinning that
// the documented result (or documented domain error) is unchanged, and once
// with the cancelled context. Moving or adding a poll can only ADD
// cancellation; it cannot change a non-cancelled result, and the live half of
// each pair is what demonstrates that rather than asserting it.
//
// Cancellation is verified with errors.Is, never with err != nil: ErrCycle,
// ErrNegativeCycle, ErrNoPath and ErrInvalidInput are all non-nil, so an
// err != nil oracle would score a domain error as a cancellation.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// entryCancelNodes is the fixture size shared by every test in this file.
// 8 is below every poll stride in the package (all 4096), which is precisely
// the regime in which these entry points were blind.
const entryCancelNodes = 8

// deadContext returns a context that is already cancelled when it is
// returned. Unlike the Err()-overriding harnesses elsewhere in this package
// this is a real *context.cancelCtx, so it behaves identically however the
// callee derives from it.
func deadContext(tb testing.TB) context.Context {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		tb.Fatalf("deadContext: ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	return ctx
}

// buildDirectedChain8 returns a CSR over the 8-node directed chain
// 0 ->1-> 1 ->1-> ... ->1-> 7, plus the shortcut 0 ->100-> 7 so that the
// k-shortest entries have exactly two distinct src->dst paths (costs 7 and
// 100). Also returns the NodeIDs of vertices 0 and 7.
func buildDirectedChain8(tb testing.TB) (*csr.CSR[int64], graph.NodeID, graph.NodeID) {
	tb.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < entryCancelNodes-1; i++ {
		if err := a.AddEdge(i, i+1, 1); err != nil {
			tb.Fatalf("AddEdge(%d,%d): %v", i, i+1, err)
		}
	}
	if err := a.AddEdge(0, entryCancelNodes-1, 100); err != nil {
		tb.Fatalf("AddEdge shortcut: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, ok := a.Mapper().Lookup(0)
	if !ok {
		tb.Fatal("Lookup(0) failed")
	}
	dst, ok := a.Mapper().Lookup(entryCancelNodes - 1)
	if !ok {
		tb.Fatalf("Lookup(%d) failed", entryCancelNodes-1)
	}
	return c, src, dst
}

// buildUndirectedRing8 returns a CSR over the 8-cycle 0-1-2-...-7-0. Every
// vertex has degree 2 and belongs to the 2-core, so KCore must report
// coreness 2 for all eight.
func buildUndirectedRing8(tb testing.TB) (*csr.CSR[struct{}], *adjlist.AdjList[int, struct{}]) {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < entryCancelNodes; i++ {
		if err := a.AddEdge(i, (i+1)%entryCancelNodes, struct{}{}); err != nil {
			tb.Fatalf("AddEdge(%d,%d): %v", i, (i+1)%entryCancelNodes, err)
		}
	}
	return csr.BuildFromAdjList(a), a
}

// buildDirectedRing8 returns a CSR over the directed 8-cycle
// 0->1->2->...->7->0. It is FULLY cyclic: every live vertex has indegree 1,
// so Kahn's algorithm starts with an empty queue and emits nothing.
func buildDirectedRing8(tb testing.TB) *csr.CSR[struct{}] {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < entryCancelNodes; i++ {
		if err := a.AddEdge(i, (i+1)%entryCancelNodes, struct{}{}); err != nil {
			tb.Fatalf("AddEdge(%d,%d): %v", i, (i+1)%entryCancelNodes, err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// --- BellmanFordCtx / BellmanFordInto ------------------------------------

// TestBellmanFordCtx_CancelBelowStride pins that BellmanFordCtx reports
// cancellation on a graph far smaller than the 4096-dequeue poll stride.
//
// RED before the fix: bellmanFordCore incremented yieldCtr BEFORE testing
// yieldCtr&0xFFF, so the first poll landed on the 4096th dequeue. On this
// 8-node chain the deque drains in 8 dequeues, the poll never ran, and the
// call returned the complete, correct distance set with a nil error under an
// already-dead context.
func TestBellmanFordCtx_CancelBelowStride(t *testing.T) {
	t.Parallel()
	c, src, dst := buildDirectedChain8(t)

	// Live context: the result must be unchanged.
	d, err := BellmanFordCtx(context.Background(), c, src)
	if err != nil {
		t.Fatalf("live BellmanFordCtx: %v", err)
	}
	got, ok := d.Distance(dst)
	if !ok || got != int64(entryCancelNodes-1) {
		t.Fatalf("live BellmanFordCtx: dist(0->7) = %v (found=%v), want %d", got, ok, entryCancelNodes-1)
	}

	// Cancelled context: cancellation must be reported, not the answer.
	res, err := BellmanFordCtx(deadContext(t), c, src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled BellmanFordCtx: err = %v, want context.Canceled (result=%v)", err, res)
	}
	if res != nil {
		t.Errorf("cancelled BellmanFordCtx: result = %v, want nil alongside the context error", res)
	}
}

// TestBellmanFordInto_CancelBelowStride pins the same fix through the
// zero-allocation primitive, which shares bellmanFordCore with
// BellmanFordCtx and was therefore blind in exactly the same regime.
func TestBellmanFordInto_CancelBelowStride(t *testing.T) {
	t.Parallel()
	c, src, dst := buildDirectedChain8(t)
	maxID := int(c.MaxNodeID())
	dist := make([]int64, maxID)
	parent := make([]graph.NodeID, maxID)
	found := make([]bool, maxID)

	if err := BellmanFordInto(context.Background(), c, src, dist, parent, found); err != nil {
		t.Fatalf("live BellmanFordInto: %v", err)
	}
	if !found[uint64(dst)] || dist[uint64(dst)] != int64(entryCancelNodes-1) {
		t.Fatalf("live BellmanFordInto: dist[7] = %d (found=%v), want %d",
			dist[uint64(dst)], found[uint64(dst)], entryCancelNodes-1)
	}

	if err := BellmanFordInto(deadContext(t), c, src, dist, parent, found); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled BellmanFordInto: err = %v, want context.Canceled", err)
	}
}

// --- KCoreCtx -------------------------------------------------------------

// TestKCoreCtx_CancelBelowStride pins that KCoreCtx reports cancellation on
// an 8-vertex ring.
//
// RED before the fix: peelCount was incremented BEFORE the peelCount&0xFFF
// test, so the first poll landed on the 4096th peel. The threshold was on the
// CSR's QUANTISED slot count rather than the live vertex count — MaxNodeID()
// >= 4096 was the real rule — so every graph below that returned the complete
// coreness slice with a nil error under a dead context.
func TestKCoreCtx_CancelBelowStride(t *testing.T) {
	t.Parallel()
	c, a := buildUndirectedRing8(t)

	coreness, err := KCoreCtx(context.Background(), c)
	if err != nil {
		t.Fatalf("live KCoreCtx: %v", err)
	}
	for i := 0; i < entryCancelNodes; i++ {
		id, _ := a.Mapper().Lookup(i)
		if coreness[id] != 2 {
			t.Fatalf("live KCoreCtx: coreness[%d] = %d, want 2", i, coreness[id])
		}
	}

	got, err := KCoreCtx(deadContext(t), c)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled KCoreCtx: err = %v, want context.Canceled (result=%v)", err, got)
	}
	if got != nil {
		t.Errorf("cancelled KCoreCtx: result = %v, want nil alongside the context error", got)
	}
}

// --- the KShortestPathsLoopless family -----------------------------------

// TestKShortestPathsLooplessFamily_CancelBelowStride pins that all three
// public entries of the unbounded, worst-case-exponential loopless k-shortest
// family report cancellation on an 8-node graph.
//
// RED before the fix: KShortestPathsLooplessCtxWithOpts is THE
// implementation — KShortestPathsLooplessCtx and the deprecated
// EppsteinKShortestCtx both delegate to it — and its single poll site
// incremented tick BEFORE testing tick&0xFFF, with no entry poll at all. On
// this fixture the enumeration finishes in a handful of pops, so all three
// returned the full path list with a nil error under a dead context. This is
// the family whose runtime is unbounded and worst-case exponential in V
// (see the EppsteinKShortest godoc), so it is the one that most needs to be
// cancellable.
func TestKShortestPathsLooplessFamily_CancelBelowStride(t *testing.T) {
	t.Parallel()
	c, src, dst := buildDirectedChain8(t)

	type entry struct {
		name string
		call func(ctx context.Context) ([]YenPath[int64], error)
	}
	entries := []entry{
		{
			name: "KShortestPathsLooplessCtxWithOpts",
			call: func(ctx context.Context) ([]YenPath[int64], error) {
				return KShortestPathsLooplessCtxWithOpts(ctx, c, src, dst, 3, KShortestPathsLooplessOpts{})
			},
		},
		{
			name: "KShortestPathsLooplessCtx",
			call: func(ctx context.Context) ([]YenPath[int64], error) {
				return KShortestPathsLooplessCtx(ctx, c, src, dst, 3)
			},
		},
		{
			name: "EppsteinKShortestCtx",
			call: func(ctx context.Context) ([]YenPath[int64], error) {
				//nolint:staticcheck // the deprecated alias is part of the public Ctx surface and delegates to the fixed body
				return EppsteinKShortestCtx(ctx, c, src, dst, 3)
			},
		},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()

			// Live context: exactly two src->dst paths, costs 7 and 100.
			paths, err := e.call(context.Background())
			if err != nil {
				t.Fatalf("live %s: %v", e.name, err)
			}
			if len(paths) != 2 {
				t.Fatalf("live %s: %d paths, want 2", e.name, len(paths))
			}
			if paths[0].Cost != int64(entryCancelNodes-1) || paths[1].Cost != 100 {
				t.Fatalf("live %s: costs = (%v, %v), want (%d, 100)",
					e.name, paths[0].Cost, paths[1].Cost, entryCancelNodes-1)
			}

			// Cancelled context.
			got, err := e.call(deadContext(t))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s: err = %v, want context.Canceled (%d paths returned)", e.name, err, len(got))
			}
		})
	}
}

// TestKShortestPathsLooplessCtxWithOpts_CancelBeatsEarlyReturn pins the ENTRY
// poll specifically: on the shapes that return before the pop loop is ever
// entered the loop poll cannot help, so only a poll taken before any work can
// report cancellation.
//
// RED before the fix: no entry poll existed, so each of these returned its
// documented non-cancellation result under a dead context.
func TestKShortestPathsLooplessCtxWithOpts_CancelBeatsEarlyReturn(t *testing.T) {
	t.Parallel()
	c, src, dst := buildDirectedChain8(t)

	cases := []struct {
		name     string
		k        int
		src, dst graph.NodeID
	}{
		// k <= 0 returns (nil, nil) before the loop.
		{name: "k_zero", k: 0, src: src, dst: dst},
		// src == dst returns the trivial one-node path before the loop.
		{name: "src_eq_dst", k: 3, src: src, dst: src},
		// An out-of-range endpoint returns (nil, nil) before the loop.
		{name: "dst_out_of_range", k: 3, src: src, dst: graph.NodeID(1 << 20)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := KShortestPathsLooplessCtxWithOpts(
				deadContext(t), c, tc.src, tc.dst, tc.k, KShortestPathsLooplessOpts{})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled KShortestPathsLooplessCtxWithOpts(%s): err = %v, want context.Canceled (%d paths)",
					tc.name, err, len(got))
			}
		})
	}
}

// --- TopologicalSortCtx ---------------------------------------------------

// TestTopologicalSortCtx_CancelBeatsCycle pins that a cancelled context wins
// over a cycle diagnosis, and that ErrCycle is still returned when the
// context is live.
//
// RED before the fix: on a fully cyclic graph every live vertex has indegree
// >= 1, so the Kahn queue starts empty, the polled loop never executed, and
// the function fell through to ErrCycle. Measured at 8 / 64 / 1500 / 20000
// nodes: the cycle error was returned at every size under an already-dead
// context, so this entry point was blind to cancellation on this shape at ANY
// graph size, not merely below the stride.
func TestTopologicalSortCtx_CancelBeatsCycle(t *testing.T) {
	t.Parallel()
	cyclic := buildDirectedRing8(t)

	// Live context: the cycle verdict is unchanged.
	order, err := TopologicalSortCtx(context.Background(), cyclic)
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("live TopologicalSortCtx on a ring: err = %v, want ErrCycle", err)
	}
	if order != nil {
		t.Errorf("live TopologicalSortCtx on a ring: order = %v, want nil", order)
	}

	// Cancelled context: cancellation outranks the cycle diagnosis.
	got, err := TopologicalSortCtx(deadContext(t), cyclic)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled TopologicalSortCtx on a ring: err = %v, want context.Canceled (order=%v)", err, got)
	}
	if errors.Is(err, ErrCycle) {
		t.Errorf("cancelled TopologicalSortCtx: err reports ErrCycle, want the context error to win")
	}
	if got != nil {
		t.Errorf("cancelled TopologicalSortCtx: order = %v, want nil", got)
	}
}

// TestTopologicalSortCtx_CancelOnAcyclic pins the acyclic counterpart: the
// chain fixture is emitted successfully under a live context and reports
// cancellation under a dead one.
func TestTopologicalSortCtx_CancelOnAcyclic(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < entryCancelNodes-1; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	dag := csr.BuildFromAdjList(a)

	order, err := TopologicalSortCtx(context.Background(), dag)
	if err != nil {
		t.Fatalf("live TopologicalSortCtx on a chain: %v", err)
	}
	if len(order) != entryCancelNodes {
		t.Fatalf("live TopologicalSortCtx on a chain: %d emitted, want %d", len(order), entryCancelNodes)
	}

	if _, err := TopologicalSortCtx(deadContext(t), dag); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled TopologicalSortCtx on a chain: err = %v, want context.Canceled", err)
	}
}

// --- the Johnson APSP family -----------------------------------------------

// buildNegativeCycleRing8 returns a CSR over the directed 8-cycle
// 0->1->...->7->0 with every edge weighted -1, so the cycle sums to -8 and
// Johnson's reweighting prologue must diagnose a negative cycle. Every vertex
// is primed into the prologue's SPFA deque, so the polled loop is always
// entered at least once.
func buildNegativeCycleRing8(tb testing.TB) *csr.CSR[int64] {
	tb.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < entryCancelNodes; i++ {
		if err := a.AddEdge(i, (i+1)%entryCancelNodes, -1); err != nil {
			tb.Fatalf("AddEdge(%d,%d): %v", i, (i+1)%entryCancelNodes, err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestJohnsonAPSPFamily_CancelBeatsNegativeCycle pins that a cancelled context
// wins over a negative-cycle diagnosis in both Johnson entry points, and that
// ErrNegativeCycle is still returned when the context is live.
//
// RED before the fix: bellmanFordVirtualSource (search/johnson.go) incremented
// yieldCtr BEFORE testing yieldCtr&0xFFF, so the first poll of the reweighting
// prologue landed on the 4096th dequeue. Johnson runs that prologue -- via the
// shared johnsonPrepare, before either entry point reaches its own per-source
// poll -- so on a graph below the stride the prologue's terminal error was
// returned in preference to the context error. Measured on this fixture with
// an already-dead context:
//
//	JohnsonAPSPCtx:           err=search: negative cycle reachable from source
//	JohnsonAPSPParallelCtx:   err=search: negative cycle reachable from source
//
// while all seven sibling entry points on the same fixture family returned
// context.Canceled. One root cause, the same ordering swap applied to the five
// sites above.
//
// The assertion uses errors.Is against context.Canceled, never err != nil:
// ErrNegativeCycle, ErrInvalidInput and ErrNegativeEdgeAPSP are all non-nil,
// so an err != nil oracle would score the prologue's refusal as a
// cancellation and the test could not fail.
func TestJohnsonAPSPFamily_CancelBeatsNegativeCycle(t *testing.T) {
	t.Parallel()
	c := buildNegativeCycleRing8(t)

	type entry struct {
		name string
		call func(ctx context.Context) (*APSP[int64], error)
	}
	entries := []entry{
		{
			name: "JohnsonAPSPCtx",
			call: func(ctx context.Context) (*APSP[int64], error) {
				return JohnsonAPSPCtx(ctx, c)
			},
		},
		{
			name: "JohnsonAPSPParallelCtx",
			call: func(ctx context.Context) (*APSP[int64], error) {
				return JohnsonAPSPParallelCtx(ctx, c, 4)
			},
		},
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()

			// Live context: the negative-cycle verdict is unchanged.
			res, err := e.call(context.Background())
			if !errors.Is(err, ErrNegativeCycle) {
				t.Fatalf("live %s: err = %v, want ErrNegativeCycle", e.name, err)
			}
			if res != nil {
				t.Errorf("live %s: result = %v, want nil alongside ErrNegativeCycle", e.name, res)
			}

			// Cancelled context: cancellation outranks the negative-cycle
			// diagnosis produced by the reweighting prologue.
			got, err := e.call(deadContext(t))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s: err = %v, want context.Canceled (result=%v)", e.name, err, got)
			}
			if errors.Is(err, ErrNegativeCycle) {
				t.Errorf("cancelled %s: err reports ErrNegativeCycle, want the context error to win", e.name)
			}
			if got != nil {
				t.Errorf("cancelled %s: result = %v, want nil", e.name, got)
			}
		})
	}
}

// TestJohnsonAPSPFamily_CancelOnAcyclic pins the well-formed counterpart: a
// sub-stride graph with no negative cycle is solved under a live context and
// reports cancellation under a dead one, so the fix is not specific to the
// terminal-error path.
func TestJohnsonAPSPFamily_CancelOnAcyclic(t *testing.T) {
	t.Parallel()
	c, src, dst := buildDirectedChain8(t)

	res, err := JohnsonAPSPCtx(context.Background(), c)
	if err != nil {
		t.Fatalf("live JohnsonAPSPCtx: %v", err)
	}
	// buildDirectedChain8 weights every hop 1, so the cheapest 0->7 route is
	// the 7-hop chain rather than the 100-weight shortcut -- the same value
	// TestBellmanFordCtx_CancelBelowStride pins on this fixture. NodeIDs come
	// from the Mapper, so the source must be the interned id, not a literal 0.
	d, ok := res.At(src, dst)
	if !ok {
		t.Fatalf("live JohnsonAPSPCtx: no path %d->%d, want one", src, dst)
	}
	if d != int64(entryCancelNodes-1) {
		t.Fatalf("live JohnsonAPSPCtx: dist(%d->%d) = %d, want %d", src, dst, d, entryCancelNodes-1)
	}

	if _, err := JohnsonAPSPCtx(deadContext(t), c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled JohnsonAPSPCtx: err = %v, want context.Canceled", err)
	}
	if _, err := JohnsonAPSPParallelCtx(deadContext(t), c, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled JohnsonAPSPParallelCtx: err = %v, want context.Canceled", err)
	}
}
