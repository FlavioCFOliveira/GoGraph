package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/search/flow"
)

// stoerWagnerMaxSites caps the site count at which the global min-cut is
// computed. StoerWagner is O(V^3) time over a dense O(V^2) weight matrix, so
// it is only run for modest networks; larger runs skip it with a telemetry
// note rather than stall. The default and test scales sit far below the cap.
const stoerWagnerMaxSites = 2048

// buildFlowNetwork materialises the backbone as a directed capacitated
// flow.Network over the same dense site space the other analyses use. Each
// undirected link becomes a pair of opposing directed arcs of equal capacity.
// A fresh Network is returned every call because the max-flow algorithms
// consume residual capacity in place — each algorithm needs its own copy.
func buildFlowNetwork(net *network) *flow.Network {
	g := flow.NewNetwork(net.sites)
	for _, l := range net.links {
		g.AddEdge(l.a, l.b, l.cap)
		g.AddEdge(l.b, l.a, l.cap)
	}
	return g
}

// reportFlowAgreement runs the two remaining max-flow algorithms —
// Edmonds-Karp and push-relabel — over the same backbone and asserts both
// return the value Dinic already produced. Three independent algorithms
// agreeing on the maximum flow is a strong cross-check: a disagreement is a
// library defect, so run surfaces it as an error rather than printing a fact.
// The per-algorithm wall-clock is telemetry; the agreement is the fact.
func reportFlowAgreement(ctx context.Context, w io.Writer, net *network, dinic int) error {
	start := time.Now()
	edmondsKarp, err := flow.EdmondsKarpCtx(ctx, buildFlowNetwork(net), net.source, net.sink)
	if err != nil {
		return fmt.Errorf("EdmondsKarp: %w", err)
	}
	ekElapsed := time.Since(start)

	start = time.Now()
	pushRelabel, err := flow.PushRelabelMaxFlowCtx(ctx, buildFlowNetwork(net), net.source, net.sink)
	if err != nil {
		return fmt.Errorf("PushRelabelMaxFlow: %w", err)
	}
	prElapsed := time.Since(start)

	if edmondsKarp != dinic || pushRelabel != dinic {
		return fmt.Errorf("max-flow algorithms disagree: dinic=%d, edmonds-karp=%d, push-relabel=%d",
			dinic, edmondsKarp, pushRelabel)
	}

	fmt.Fprintf(w, "maxflow.dinic=%d\n", dinic)
	fmt.Fprintf(w, "maxflow.edmondskarp=%d\n", edmondsKarp)
	fmt.Fprintf(w, "maxflow.pushrelabel=%d\n", pushRelabel)
	fmt.Fprintf(w, "maxflow.algorithms_agree=%t\n", true)
	fmt.Fprintf(w, "# maxflow.edmondskarp_elapsed=%s\n", ekElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# maxflow.pushrelabel_elapsed=%s\n", prElapsed.Round(time.Microsecond))
	return nil
}

// reportGlobalCut computes the GLOBAL minimum cut of the undirected backbone
// with Stoer-Wagner. Unlike the source-to-sink cut (reportThroughput), which
// is fixed to the spine bottleneck, the global cut ranges over every partition
// and finds the cheapest one anywhere: the single off-spine bridge (capBridge
// Gb/s), which severs the stub from the spine and is strictly cheaper than the
// source-to-sink min-cut. The smaller side of that cut is exactly the stub
// cluster, so its size equals cluster-size. Both are deterministic facts.
//
// StoerWagner is O(V^3), so it is skipped above stoerWagnerMaxSites with a
// telemetry note; the default and test scales always run it.
func reportGlobalCut(ctx context.Context, w io.Writer, net *network) error {
	if net.sites > stoerWagnerMaxSites {
		fmt.Fprintf(w, "# stoerwagner.skipped=%d sites exceeds the O(V^3) budget of %d\n",
			net.sites, stoerWagnerMaxSites)
		return nil
	}

	// Dense symmetric weight matrix in row-major order, as StoerWagner
	// requires. Parallel links between a pair accumulate (the topology has
	// none, but += keeps the mapping faithful regardless).
	n := net.sites
	weights := make([]int, n*n)
	for _, l := range net.links {
		weights[l.a*n+l.b] += l.cap
		weights[l.b*n+l.a] += l.cap
	}

	start := time.Now()
	res, err := flow.StoerWagnerCtx(ctx, weights, n)
	if err != nil {
		return fmt.Errorf("StoerWagner: %w", err)
	}
	elapsed := time.Since(start)

	smaller := min(len(res.A), len(res.B))
	fmt.Fprintf(w, "stoerwagner.mincut_weight=%d\n", res.Weight)
	fmt.Fprintf(w, "stoerwagner.smaller_side_sites=%d\n", smaller)
	fmt.Fprintf(w, "# stoerwagner.elapsed=%s\n", elapsed.Round(time.Microsecond))
	return nil
}

// reportMinCostFlow solves a small, self-contained min-cost routing scenario
// that exercises flow.MinCostMaxFlow with per-link cost. A regional hub must
// deliver its full throughput ceiling (20 Gb/s — the backbone's source-to-sink
// min-cut) to a peer across two transit providers: an economy provider that is
// cheap but narrow, and a premium provider that is wide but pricey.
//
// Successive-shortest-paths fills the cheapest augmenting path first, so it
// saturates the economy provider before spilling onto the premium one. The
// resulting flow and total cost are therefore deterministic:
//
//	flow = 8 + 12 = 20 Gb/s
//	cost = 8*(1+1) + 12*(5+5) = 16 + 120 = 136
func reportMinCostFlow(ctx context.Context, w io.Writer) error {
	const (
		hub = iota
		economy
		premium
		peer
		mcNodes
	)

	g := flow.NewCostNetwork(mcNodes)
	g.AddCostEdge(hub, economy, 8, 1) // economy provider: cap 8 Gb/s, cost 1/Gb/s per arc
	g.AddCostEdge(economy, peer, 8, 1)
	g.AddCostEdge(hub, premium, 12, 5) // premium provider: cap 12 Gb/s, cost 5/Gb/s per arc
	g.AddCostEdge(premium, peer, 12, 5)

	start := time.Now()
	value, cost, err := flow.MinCostMaxFlowCtx(ctx, g, hub, peer)
	if err != nil {
		return fmt.Errorf("MinCostMaxFlow: %w", err)
	}
	elapsed := time.Since(start)

	fmt.Fprintf(w, "mincostflow.flow=%d\n", value)
	fmt.Fprintf(w, "mincostflow.cost=%d\n", cost)
	fmt.Fprintf(w, "# mincostflow.elapsed=%s\n", elapsed.Round(time.Microsecond))
	return nil
}
