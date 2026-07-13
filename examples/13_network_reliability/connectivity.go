package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"slices"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// reportConnectivity counts the weakly-connected components of the backbone
// before and after the single articulation bridge is removed, and confirms
// the parallel component analysis agrees with the serial one.
//
// The intact backbone is a single connected component. The bridge found by
// the structural analysis is, by construction, the ONLY link joining the
// off-spine stub cluster to the spine, so severing it must split the network
// into exactly two components — the spine and the stub. The 1 -> 2 transition
// is the deterministic fact this analysis pins.
//
// search.WCCParallel is then run over the severed graph and must yield the
// identical partition: wccRelabel assigns component IDs in ascending order of
// each component's minimum NodeID, so any union-find encoding the same
// equivalence classes — serial or the parallel merge — produces byte-identical
// labels. A divergence would be a library defect, not a topology quirk.
func reportConnectivity(ctx context.Context, w io.Writer, net *network, bridges [][2]graph.NodeID) error {
	if len(bridges) != 1 {
		return fmt.Errorf("expected exactly one articulation bridge, got %d", len(bridges))
	}
	bridge := bridges[0]

	// Intact backbone: one connected component.
	start := time.Now()
	_, kIntact, err := search.WCCCtx(ctx, csr.BuildFromAdjList(net.adj))
	if err != nil {
		return fmt.Errorf("WCC (intact): %w", err)
	}
	intactElapsed := time.Since(start)

	// Sever the bridge and recompute: the stub, reachable only through that
	// link, becomes its own component.
	severed, err := net.adjExcludingBridge(bridge)
	if err != nil {
		return err
	}
	severedCSR := csr.BuildFromAdjList(severed)

	start = time.Now()
	comp, kSevered, err := search.WCCCtx(ctx, severedCSR)
	if err != nil {
		return fmt.Errorf("WCC (severed): %w", err)
	}
	serialElapsed := time.Since(start)

	// The parallel analysis over the SAME snapshot must reproduce the exact
	// partition (component count and per-node labels).
	start = time.Now()
	compPar, kPar, err := search.WCCParallelCtx(ctx, severedCSR, runtime.GOMAXPROCS(0))
	if err != nil {
		return fmt.Errorf("WCCParallel (severed): %w", err)
	}
	parallelElapsed := time.Since(start)

	matches := kPar == kSevered && slices.Equal(comp, compPar)

	fmt.Fprintf(w, "wcc.components_connected=%d\n", kIntact)
	fmt.Fprintf(w, "wcc.components_after_bridge_removal=%d\n", kSevered)
	fmt.Fprintf(w, "wcc.parallel_matches_serial=%t\n", matches)
	fmt.Fprintf(w, "# wcc.intact_elapsed=%s\n", intactElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# wcc.serial_elapsed=%s\n", serialElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# wcc.parallel_elapsed=%s\n", parallelElapsed.Round(time.Microsecond))
	return nil
}

// adjExcludingBridge builds a fresh undirected adjacency over the SAME site
// names as the backbone but omits the one bridge link. Every site still
// appears — each is part of its cluster's Hamiltonian cycle, none of which is
// the bridge — so the resulting CSR covers the full node set and the WCC
// component count reflects the true partition, not a dropped-node artefact.
func (net *network) adjExcludingBridge(bridge [2]graph.NodeID) (*adjlist.AdjList[string, int64], error) {
	adj := adjlist.New[string, int64](adjlist.Config{Directed: false})
	for _, l := range net.links {
		ua, ub := net.idOf[l.a], net.idOf[l.b]
		if (ua == bridge[0] && ub == bridge[1]) || (ua == bridge[1] && ub == bridge[0]) {
			continue // omit the bridge
		}
		na, _ := net.mapper.Resolve(ua)
		nb, _ := net.mapper.Resolve(ub)
		if err := adj.AddEdge(na, nb, int64(l.cap)); err != nil {
			return nil, fmt.Errorf("AddEdge %s-%s: %w", na, nb, err)
		}
	}
	return adj, nil
}
