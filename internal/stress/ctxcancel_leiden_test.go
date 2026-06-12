//go:build soak || nightly

// Package stress — T653: ctx-cancel mid-Leiden LFR 10k (soak).
//
// Builds a 10k-node LFR community-benchmark graph and cancels LeidenCtx,
// verifying:
//  1. errors.Is(err, context.Canceled) holds.
//  2. Return latency after cancel < 50 ms.
//  3. goleak clean (via TestMain).
//  4. No partial partition leaked (returned Partition is zero on cancel).
package stress

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

// TestCtxCancel_Leiden_MidRun builds an LFR benchmark graph and cancels
// LeidenCtx, verifying prompt cancellation and zero leaked partition state.
//
// Under -short a 200-node LFR graph is used with a pre-cancelled context.
// Under full soak a 10k-node graph is used with a 5 ms timeout.
func TestCtxCancel_Leiden_MidRun(t *testing.T) {
	const (
		gammaPercent = 200 // γ = 2.0 × 100
		betaPercent  = 100 // β = 1.0 × 100
		avgDeg       = 10
		maxDeg       = 50
		muPercent    = 10 // μ = 0.10 × 100
	)
	nodes := 10_000
	minCom := 50
	maxCom := 200
	var cancelDelay time.Duration
	if testing.Short() {
		nodes = 200
		minCom = 10
		maxCom = 30
		cancelDelay = 0 // pre-cancel
	} else {
		cancelDelay = 5 * time.Millisecond
	}

	defer goleak.VerifyNone(t)

	shape := shapegen.LFR(nodes, gammaPercent, betaPercent, avgDeg, maxDeg, minCom, maxCom, muPercent, 42)
	g, err := shape.Build(adjlist.Config{Directed: false})
	if err != nil {
		// LFR can fail with ErrLFRAssignmentFailed on tight parameter combos;
		// skip rather than fail — this test exercises cancellation, not topology.
		t.Skipf("LFR.Build: %v (skipping)", err)
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

	opts := community.DefaultLeidenOptions()
	opts.MaxPasses = 1000 // increase to make mid-run cancel more likely under soak

	t0 := time.Now()
	part, gotErr := community.LeidenCtx(ctx, c, opts)
	elapsed := time.Since(t0)

	if !errors.Is(gotErr, context.DeadlineExceeded) && !errors.Is(gotErr, context.Canceled) {
		t.Errorf("LeidenCtx returned err=%v; want context.Canceled or context.DeadlineExceeded", gotErr)
	}

	// AC4: on cancellation, LeidenCtx must return a zero Partition.
	if part.NumCommunities != 0 || len(part.Community) != 0 {
		t.Errorf("LeidenCtx on cancel returned non-zero Partition (NumCommunities=%d, len=%d); want zero",
			part.NumCommunities, len(part.Community))
	}

	if elapsed > 50*time.Millisecond {
		t.Errorf("LeidenCtx return latency after cancel = %v; want < 50 ms", elapsed)
	}
}
