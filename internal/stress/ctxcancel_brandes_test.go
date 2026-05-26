//go:build soak

// Package stress — T646: ctx-cancel mid-Brandes 50k ER (soak).
//
// Builds a 50k-node Erdos-Renyi graph and cancels Brandes betweenness,
// verifying:
//  1. errors.Is(err, context.Canceled) holds.
//  2. Return latency after cancel < 50 ms.
//  3. goleak clean (via TestMain).
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

// TestCtxCancel_Brandes_MidRun builds an ER graph and cancels
// BetweennessParallelCtx, verifying context.Canceled is returned promptly.
//
// Under -short a 200-node ER graph is used with a pre-cancelled context.
// Under full soak a 50k-node sparse ER graph is used with a 5 ms timeout.
func TestCtxCancel_Brandes_MidRun(t *testing.T) {
	const pPercent = 1 // 0.01% edge probability — sparse
	nodes := 50_000
	var cancelDelay time.Duration
	if testing.Short() {
		nodes = 200
		cancelDelay = 0 // pre-cancel
	} else {
		cancelDelay = 5 * time.Millisecond
	}

	defer goleak.VerifyNone(t)

	shape := shapegen.ErdosRenyiNP(nodes, pPercent, 42)
	g, err := shape.Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("ErdosRenyiNP.Build: %v", err)
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
