package flow

import (
	"testing"
	"time"
)

// TestPushRelabel_WorstCase verifies push-relabel on a layered
// network designed to stress the height-labelling scheme.
//
// Layout: source (0) → 7 layers of 21 nodes each → sink (148).
// Total = 1 + 7*21 + 1 = 149 vertices (≈ V=150).
// Each vertex in layer i connects to every vertex in layer i+1 with
// capacity 1. Min-cut between consecutive layers = 21; max flow = 21.
//
// Assertions:
//  1. PushRelabelMaxFlow == MaxFlow (Dinic) == 21.
//  2. Both complete in under 2 s.
func TestPushRelabel_WorstCase(t *testing.T) {
	t.Parallel()

	const (
		layerCount = 7
		layerSize  = 21
		// 0 = source, 1..(layerCount*layerSize) = interior, last = sink.
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
	prGot := PushRelabelMaxFlow(build(), src, sink)
	prDur := time.Since(start)

	if prGot != wantMax {
		t.Fatalf("PushRelabelMaxFlow = %d, want %d", prGot, wantMax)
	}
	if prDur > 2*time.Second {
		t.Fatalf("PushRelabelMaxFlow took %v, want < 2s", prDur)
	}

	start = time.Now()
	dnGot := MaxFlow(build(), src, sink)
	dnDur := time.Since(start)

	if dnGot != wantMax {
		t.Fatalf("MaxFlow = %d, want %d", dnGot, wantMax)
	}
	if dnDur > 2*time.Second {
		t.Fatalf("MaxFlow took %v, want < 2s", dnDur)
	}

	if prGot != dnGot {
		t.Fatalf("PushRelabelMaxFlow=%d != MaxFlow=%d: implementations disagree", prGot, dnGot)
	}
}
