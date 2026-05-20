package flow

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaxFlow_CLRS(t *testing.T) {
	t.Parallel()
	// CLRS Fig. 26.1(a): s=0, t=5; max flow = 23.
	g := NewNetwork(6)
	g.AddEdge(0, 1, 16)
	g.AddEdge(0, 2, 13)
	g.AddEdge(1, 2, 10)
	g.AddEdge(2, 1, 4)
	g.AddEdge(1, 3, 12)
	g.AddEdge(2, 4, 14)
	g.AddEdge(3, 2, 9)
	g.AddEdge(3, 5, 20)
	g.AddEdge(4, 3, 7)
	g.AddEdge(4, 5, 4)
	if got := MaxFlow(g, 0, 5); got != 23 {
		t.Fatalf("MaxFlow = %d, want 23", got)
	}
}

func TestMaxFlow_NoPath(t *testing.T) {
	t.Parallel()
	g := NewNetwork(3)
	g.AddEdge(0, 1, 10)
	if got := MaxFlow(g, 0, 2); got != 0 {
		t.Fatalf("MaxFlow = %d, want 0 when sink unreachable", got)
	}
}

// TestMaxFlow_DeepPipeline runs Dinic on a linear pipeline V=1e5 nodes
// (each only connected to the next). The previous recursive
// augmentFlow would blow the goroutine stack at this depth; the
// iterative replacement must complete cleanly.
func TestMaxFlow_DeepPipeline(t *testing.T) {
	t.Parallel()
	const n = 100_000
	g := NewNetwork(n)
	for i := 0; i < n-1; i++ {
		g.AddEdge(i, i+1, 1)
	}
	if got := MaxFlow(g, 0, n-1); got != 1 {
		t.Fatalf("MaxFlow on pipeline = %d, want 1", got)
	}
}

// TestMaxFlowCtx_AlreadyCancelled exercises the outer ctx.Err()
// check at the BFS-rebuild boundary: a cancelled ctx must surface
// context.Canceled before any meaningful work happens.
func TestMaxFlowCtx_AlreadyCancelled(t *testing.T) {
	t.Parallel()
	g := NewNetwork(4)
	g.AddEdge(0, 1, 5)
	g.AddEdge(1, 3, 5)
	g.AddEdge(0, 2, 5)
	g.AddEdge(2, 3, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := MaxFlowCtx(ctx, g, 0, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if got != 0 {
		t.Fatalf("got=%d, want 0 (no work should happen on already-cancelled ctx)", got)
	}
}

// TestMaxFlowCtx_AugmentCancellation asserts that cancelling ctx
// during a MaxFlow run on a large graph returns within a bounded
// time even when the cancellation lands inside the inner DFS rather
// than at a phase boundary. The pre-fix augmentFlow ignored ctx; on
// a dense phase it could run for seconds after cancel().
//
// The graph is a long pipeline (V=200k, depth=200k) so a single
// augmenting DFS traverses every vertex; cancelling 1ms in must
// land squarely inside augmentFlow rather than at the outer
// BFS rebuild.
func TestMaxFlowCtx_AugmentCancellation(t *testing.T) {
	t.Parallel()
	const n = 200_000
	g := NewNetwork(n)
	for i := 0; i < n-1; i++ {
		g.AddEdge(i, i+1, 1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var got int
	var gotErr error
	go func() {
		defer close(done)
		got, gotErr = MaxFlowCtx(ctx, g, 0, n-1)
	}()

	// Give the runner a moment to enter the augmenting DFS, then
	// cancel. The pipeline is deep enough that one augmentation
	// alone walks 200k stack ops, well past the 4096-iter ctx poll.
	time.Sleep(1 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("MaxFlowCtx did not return within 5s after ctx cancel: augmenting DFS does not honour ctx")
	}
	// On this geometry the only legitimate outcomes are:
	//   - The DFS noticed the cancellation mid-walk (err=Canceled).
	//   - Dinic happened to complete before the cancel signal landed
	//     (err=nil, got==1). This is a benign race on very fast hosts.
	if gotErr != nil && !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled or nil", gotErr)
	}
	if got < 0 {
		t.Fatalf("got=%d, must be non-negative", got)
	}
}

// BenchmarkDinic_Pipeline measures Dinic on a moderately deep
// pipeline so the per-augment work is dominated by stack manipulation
// — the new iterative version should match the recursive one within
// 5% on this geometry (task #130 acceptance).
func BenchmarkDinic_Pipeline(b *testing.B) {
	const n = 1024
	g := NewNetwork(n)
	for i := 0; i < n-1; i++ {
		g.AddEdge(i, i+1, 100)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset capacities between runs.
		for j := range g.cap {
			if j%2 == 0 {
				g.cap[j] = 100
			} else {
				g.cap[j] = 0
			}
		}
		_ = MaxFlow(g, 0, n-1)
	}
}
