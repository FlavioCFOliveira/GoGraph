//go:build soak || nightly

// Package stress — T636: ctx-cancel mid-Dijkstra 1M-node (soak).
//
// Builds a 1M-node graph and cancels the Dijkstra context, verifying:
//  1. errors.Is(err, context.Canceled) holds.
//  2. Return latency after cancel < 50 ms.
//  3. goleak clean (via TestMain).
package stress

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestCtxCancel_Dijkstra_MidRun builds a chain graph and cancels DijkstraCtx,
// verifying that context.Canceled is propagated promptly.
//
// Under -short a 10k-node chain is used with an already-cancelled context
// (guaranteeing the error path is exercised even when the traversal is fast).
// Under full soak a 1M-node chain is used with a 5 ms timeout, ensuring
// cancellation happens mid-traversal.
func TestCtxCancel_Dijkstra_MidRun(t *testing.T) {
	nodes := 1_000_000
	var cancelDelay time.Duration
	if testing.Short() {
		// Use a pre-cancelled context: avoids a race between Dijkstra
		// completing on a small graph and the cancel firing.
		nodes = 10_000
		cancelDelay = 0
	} else {
		// 1M-node chain takes >> 5 ms so the cancel fires mid-traversal.
		cancelDelay = 5 * time.Millisecond
	}

	defer goleak.VerifyNone(t)

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < nodes-1; i++ {
		if err := a.AddEdge(i, i+1, 1); err != nil {
			t.Fatalf("AddEdge %d→%d: %v", i, i+1, err)
		}
	}
	c := csr.BuildFromAdjList(a)

	src, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("Lookup(0): not found")
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if cancelDelay == 0 {
		// Pre-cancel: context is already done before Dijkstra is called.
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), cancelDelay)
		defer cancel()
	}

	t0 := time.Now()
	_, err := search.DijkstraCtx(ctx, c, src)
	elapsed := time.Since(t0)

	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("DijkstraCtx returned err=%v; want context.Canceled or context.DeadlineExceeded", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("DijkstraCtx return latency after cancel = %v; want < 50 ms", elapsed)
	}
}
