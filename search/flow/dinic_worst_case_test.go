package flow

import (
	"testing"
	"time"
)

// TestMaxFlow_WorstCase verifies Dinic's algorithm on a layered
// network whose structure maximises the number of BFS-level rebuilds.
// The key property is that the number of phases is bounded by O(V),
// so even a large network must complete well within a second.
//
// Layout: source (0) → 8 layers of 20 nodes each → sink (161).
// Total = 1 + 8*20 + 1 = 162 vertices.
// Each vertex in layer i is connected to every vertex in layer i+1
// with capacity 1. The min-cut between any two consecutive layers is
// 20, so max flow = 20.
//
// Assertions:
//  1. MaxFlow == EdmondsKarp == 20.
//  2. Both algorithms complete in under 2 s on any reasonable host.
func TestMaxFlow_WorstCase(t *testing.T) {
	t.Parallel()

	const (
		layerCount = 8
		layerSize  = 20
		// node layout: 0 = source, 1..(layerCount*layerSize) = interior,
		// layerCount*layerSize+1 = sink.
		n       = 1 + layerCount*layerSize + 1
		src     = 0
		sink    = n - 1
		wantMax = layerSize
	)

	build := func() *Network {
		g := NewNetwork(n)
		// source -> first layer
		for j := 0; j < layerSize; j++ {
			g.AddEdge(src, 1+j, 1)
		}
		// layer i -> layer i+1
		for i := 0; i < layerCount-1; i++ {
			base := 1 + i*layerSize
			next := base + layerSize
			for u := base; u < base+layerSize; u++ {
				for v := next; v < next+layerSize; v++ {
					g.AddEdge(u, v, 1)
				}
			}
		}
		// last layer -> sink
		lastBase := 1 + (layerCount-1)*layerSize
		for j := 0; j < layerSize; j++ {
			g.AddEdge(lastBase+j, sink, 1)
		}
		return g
	}

	start := time.Now()
	got := MaxFlow(build(), src, sink)
	dinicDur := time.Since(start)

	if got != wantMax {
		t.Fatalf("MaxFlow = %d, want %d", got, wantMax)
	}
	if dinicDur > 2*time.Second {
		t.Fatalf("MaxFlow took %v, want < 2s", dinicDur)
	}

	start = time.Now()
	ekGot := EdmondsKarp(build(), src, sink)
	ekDur := time.Since(start)

	if ekGot != wantMax {
		t.Fatalf("EdmondsKarp = %d, want %d", ekGot, wantMax)
	}
	if ekDur > 2*time.Second {
		t.Fatalf("EdmondsKarp took %v, want < 2s", ekDur)
	}

	if got != ekGot {
		t.Fatalf("MaxFlow=%d != EdmondsKarp=%d: implementations disagree", got, ekGot)
	}
}
