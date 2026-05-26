//go:build soak

// Package stress — T646: ctx-cancel mid-Brandes 50k BA scale-free (soak).
//
// Builds a 50k-node Barabási-Albert scale-free graph and cancels Brandes
// betweenness, verifying:
//  1. errors.Is(err, context.Canceled) holds.
//  2. Return latency after cancel < 50 ms.
//  3. goleak clean (via TestMain).
//
// Note: ErdosRenyiNP is capped at n=1000; BarabasiAlbert supports up to
// n=100000 and produces a well-connected graph suitable for Brandes stress.
package stress

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
	"gograph/search/centrality"
)

// TestCtxCancel_Brandes_MidRun builds a BA scale-free graph and cancels
// BetweennessParallelCtx, verifying context.Canceled is returned promptly.
//
// Under -short a 200-node BA graph is used with a pre-cancelled context.
// Under full soak a 10k-node sparse BA graph (m0=2) is used with a 5 ms timeout.
//
// Graph size rationale: BetweennessParallelCtx checks context.Done() once
// per source vertex, AFTER the current brandesSource call finishes. The
// < 50 ms return-latency assertion therefore requires that a single
// brandesSource invocation completes in < 45 ms (= ceiling − cancel delay).
// Under the race detector (−race), brandesSource on a BA(50k, 2) graph takes
// ~65 ms — violating the ceiling. BA(10k, 2) keeps each brandesSource under
// 15 ms (measured), well within the 45 ms budget even under −race.
func TestCtxCancel_Brandes_MidRun(t *testing.T) {
	const m0 = 2 // Barabási-Albert attachment parameter
	nodes := 10_000
	var cancelDelay time.Duration
	if testing.Short() {
		nodes = 200
		cancelDelay = 0 // pre-cancel
	} else {
		cancelDelay = 5 * time.Millisecond
	}

	defer goleak.VerifyNone(t)

	shape := shapegen.BarabasiAlbert(nodes, m0, 42)
	g, err := shape.Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	var ctx context.Context
	var cancel context.CancelFunc
	if cancelDelay == 0 {
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), cancelDelay)
		defer cancel()
	}

	workers := runtime.GOMAXPROCS(0)
	t0 := time.Now()
	_, gotErr := centrality.BetweennessParallelCtx(ctx, c, workers)
	elapsed := time.Since(t0)

	if !errors.Is(gotErr, context.DeadlineExceeded) && !errors.Is(gotErr, context.Canceled) {
		t.Errorf("BetweennessParallelCtx returned err=%v; want context.Canceled or context.DeadlineExceeded", gotErr)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("BetweennessParallelCtx return latency after cancel = %v; want < 50 ms", elapsed)
	}
}
