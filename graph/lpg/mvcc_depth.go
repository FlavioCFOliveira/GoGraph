package lpg

// mvcc_depth.go — which stores report a version-chain depth distribution, and why
// one of them does not (rmp #2312).
//
// The histograms themselves, the bucketing and what a reading means are in
// [mvcc.DepthHist]. This file is only the enumeration and the accessors, so a
// reclaimer in any of the four files that fill one has a single place to look up
// which histogram is its own.

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// depthStore names a versioned store that keeps CHAINS, and therefore has a depth
// distribution to report.
//
// Node EXISTENCE is deliberately absent. It is versioned — nodeLifeShards records a
// birth and a death per node — but the records are not a chain: there is at most one
// birth and at most one death per node id, held in two maps, so the depth is 0, 1 or
// 2 by construction and a distribution over it says nothing. Its retention is
// already reported by MVCCStats.NodeLifeRecords.
type depthStore int

const (
	depthNodeLabels depthStore = iota
	depthNodeProps
	depthEdgeSides
	depthAdjacency
	depthStoreCount = iota
)

// depthStoreNames are the metric-name suffixes the four distributions publish
// under, in [depthStore] order.
var depthStoreNames = [depthStoreCount]string{"node_labels", "node_properties", "edge_sides", "adjacency"}

// depth returns the histogram store s fills.
func (g *Graph[N, W]) depth(s depthStore) *mvcc.DepthHist { return &g.chainDepth[s] }

// ChainDepths returns the retained version-chain depth distribution, summed over
// every store that keeps chains.
//
// Each store's contribution describes that store's most recent complete sweep; see
// [mvcc.DepthHist] for what that means and why it is not an instant.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ChainDepths() mvcc.Depths {
	var d mvcc.Depths
	for i := range g.chainDepth {
		d.Add(g.chainDepth[i].Load())
	}
	return d
}

// ChainDepthsOf returns one store's distribution, for a caller that needs to know
// WHICH structure is holding the long chains.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ChainDepthsOf(store int) mvcc.Depths { return g.chainDepth[store].Load() }

// ChainDepthStores returns the metric-name suffix of each store that reports a
// distribution, so a caller can label [Graph.ChainDepthsOf] without knowing the
// enumeration.
func ChainDepthStores() []string { return depthStoreNames[:] }
