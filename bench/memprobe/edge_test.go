//go:build threeway

package memprobe

import (
	"os"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The edge probes use a smaller node population than the node probes because
// each node carries probeDegree edges, and the quantity being measured is the
// per-EDGE cost.
// The node count and degree are env-settable so that the SAME total edge
// count can be built at different degrees. That is the experiment that
// separates a per-EDGE cost from a per-SOURCE-NODE one: a structure allocated
// once per source node shows a per-edge cost that falls as degree rises, while
// a genuine per-edge cost does not move at all.
var (
	probeEdgeNodes = probeEnvInt("PROBE_NODES", 500_000)
	probeDegree    = probeEnvInt("PROBE_DEGREE", 8)
	probeEdges     = probeEdgeNodes * probeDegree
)

func probeEnvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// buildEdges builds the same deterministic graph the containerised comparison
// loads, through the Go API rather than through Cypher, so that the cost
// measured is the storage structure's and not the query pipeline's.
func buildEdges(t *testing.T, multigraph, labelled bool, propKey string) any {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: multigraph})
	key := func(i int) string { return "__cx_" + strconv.FormatUint(uint64(i), 16) }
	for i := range probeEdgeNodes {
		if err := g.AddNode(key(i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := range probeEdgeNodes {
		for k := 1; k <= probeDegree; k++ {
			dst := (i*31 + k*2654435761) % probeEdgeNodes
			if err := g.AddEdge(key(i), key(dst), 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if labelled {
				// SetEdgeLabel returns nothing: the type is recorded in the
				// adjacency's parallel label column, which cannot fail once
				// the edge exists.
				g.SetEdgeLabel(key(i), key(dst), "KNOWS")
			}
			if propKey != "" {
				if err := g.SetEdgeProperty(key(i), key(dst), propKey, lpg.Int64Value(int64(2000+k))); err != nil {
					t.Fatalf("SetEdgeProperty: %v", err)
				}
			}
		}
	}
	return g
}

// TestProbe_EdgeNodesOnly is the baseline the edge probes subtract: the node
// population with no edges at all.
func TestProbe_EdgeNodesOnly(t *testing.T) {
	measure(t, "edgebase/nodes-only", probeEdgeNodes, func() any {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := range probeEdgeNodes {
			if err := g.AddNode("__cx_" + strconv.FormatUint(uint64(i), 16)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		return g
	})
}

// TestProbe_EdgesMultigraph measures the untyped adjacency in the shape the
// Bolt server runs (bench/comparison/ggserver constructs its graph with
// Multigraph: true, because openCypher requires two CREATEs between the same
// pair to produce two distinct relationships).
func TestProbe_EdgesMultigraph(t *testing.T) {
	measure(t, "edges/multigraph-untyped", probeEdges, func() any {
		return buildEdges(t, true, false, "")
	})
}

// TestProbe_EdgesSimple is the counterfactual: the same edges in a simple
// graph, where the per-instance and per-handle stores stay empty. The
// difference between this and the probe above is what parallel-edge support
// costs per edge.
func TestProbe_EdgesSimple(t *testing.T) {
	measure(t, "edges/simple-untyped", probeEdges, func() any {
		return buildEdges(t, false, false, "")
	})
}

// TestProbe_EdgesTyped adds the relationship type every realistic Cypher edge
// carries, exercising the adjacency's parallel label column.
func TestProbe_EdgesTyped(t *testing.T) {
	measure(t, "edges/multigraph-typed", probeEdges, func() any {
		return buildEdges(t, true, true, "")
	})
}

// TestProbe_EdgesTypedWithProp adds one small integer property, exercising the
// columnar edge-property tier landed in sprint 222.
func TestProbe_EdgesTypedWithProp(t *testing.T) {
	measure(t, "edges/multigraph-typed+int-prop", probeEdges, func() any {
		return buildEdges(t, true, true, "since")
	})
}

// TestProbe_EdgesFusedTypedWithProp is the counterfactual for
// TestProbe_EdgesTypedWithProp. Both end with the same graph — every edge
// typed and carrying one integer property — but this one writes the property
// AT INSERTION through the fused call, where the columnar tier can append to
// the column, instead of updating an already-built column afterwards.
//
// Sprint 222 landed the columnar edge-property tier with a documented hazard:
// a per-edge SetEdgeProperty on that tier copies the whole column for the
// source node, so a bulk build through it is quadratic in degree. The fused
// call exists to avoid that. This probe measures whether the choice of call
// also changes the RESIDENT result, not merely the time to reach it.
func TestProbe_EdgesFusedTypedWithProp(t *testing.T) {
	measure(t, "edges/fused-typed+int-prop", probeEdges, func() any {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		key := func(i int) string { return "__cx_" + strconv.FormatUint(uint64(i), 16) }
		for i := range probeEdgeNodes {
			if err := g.AddNode(key(i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		for i := range probeEdgeNodes {
			for k := 1; k <= probeDegree; k++ {
				dst := (i*31 + k*2654435761) % probeEdgeNodes
				if err := g.AddEdgeLabeledWithProperty(
					key(i), key(dst), 1, "KNOWS", "since", lpg.Int64Value(int64(2000+k))); err != nil {
					t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
				}
			}
		}
		return g
	})
}
