package csr_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestCSR_NeighboursByID_ZeroAllocs asserts that iterating
// NeighboursByID on the hot path causes zero heap allocations and
// returns deterministic results on consecutive calls.
//
// t.Parallel() is intentionally omitted from the alloc sub-tests:
// testing.AllocsPerRun relies on runtime GC accounting that is
// unreliable under concurrent goroutine scheduling.
func TestCSR_NeighboursByID_ZeroAllocs(t *testing.T) {
	type shapeEntry struct {
		name    string
		buildFn func() (*csr.CSR[int64], graph.NodeID)
	}

	entries := []shapeEntry{
		{
			name: "EmptyGraph",
			buildFn: func() (*csr.CSR[int64], graph.NodeID) {
				g, err := shapegen.EmptyGraph().Build(adjlist.Config{Directed: true})
				if err != nil {
					panic(err)
				}
				c := csr.BuildFromAdjList(g.AdjList())
				return c, 0
			},
		},
		{
			name: "SingleEdge",
			buildFn: func() (*csr.CSR[int64], graph.NodeID) {
				g, err := shapegen.SingleEdge(true, false, false).Build(adjlist.Config{Directed: true})
				if err != nil {
					panic(err)
				}
				a := g.AdjList()
				c := csr.BuildFromAdjList(a)
				src := c.MaxNodeID() / 2
				return c, src
			},
		},
		{
			name: "Cycle10",
			buildFn: func() (*csr.CSR[int64], graph.NodeID) {
				g, err := shapegen.Cycle(10, true).Build(adjlist.Config{Directed: true})
				if err != nil {
					panic(err)
				}
				a := g.AdjList()
				c := csr.BuildFromAdjList(a)
				src := c.MaxNodeID() / 2
				return c, src
			},
		},
		{
			name: "Complete8",
			buildFn: func() (*csr.CSR[int64], graph.NodeID) {
				g, err := shapegen.Complete(8, true).Build(adjlist.Config{Directed: true})
				if err != nil {
					panic(err)
				}
				a := g.AdjList()
				c := csr.BuildFromAdjList(a)
				src := c.MaxNodeID() / 2
				return c, src
			},
		},
		{
			name: "BarabasiAlbert100",
			buildFn: func() (*csr.CSR[int64], graph.NodeID) {
				g, err := shapegen.BarabasiAlbert(100, 3, 7).Build(adjlist.Config{Directed: true})
				if err != nil {
					panic(err)
				}
				a := g.AdjList()
				c := csr.BuildFromAdjList(a)
				src := c.MaxNodeID() / 2
				return c, src
			},
		},
	}

	for _, e := range entries {
		e := e
		t.Run(e.name, func(t *testing.T) {
			// Do NOT call t.Parallel() here: testing.AllocsPerRun
			// requires serial execution for accurate GC accounting.
			c, src := e.buildFn()

			// Warmup: ensure the hot path is JIT-compiled and any
			// one-time initialisation is complete before measuring.
			for i := 0; i < 100; i++ {
				var n int
				for range c.NeighboursByID(src) {
					n++
				}
				_ = n
			}

			// Zero-alloc assertion.
			var sinkN int
			allocs := testing.AllocsPerRun(100, func() {
				for range c.NeighboursByID(src) {
					sinkN++
				}
			})
			_ = sinkN
			if allocs != 0 {
				t.Errorf("NeighboursByID allocs = %v, want 0", allocs)
			}

			// Determinism: two consecutive calls must return identical
			// neighbour sequences.
			var first, second []graph.NodeID
			for nb := range c.NeighboursByID(src) {
				first = append(first, nb)
			}
			for nb := range c.NeighboursByID(src) {
				second = append(second, nb)
			}
			if len(first) != len(second) {
				t.Fatalf("determinism: first call len=%d second call len=%d",
					len(first), len(second))
			}
			for i := range first {
				if first[i] != second[i] {
					t.Errorf("determinism: index %d: first=%v second=%v",
						i, first[i], second[i])
				}
			}
		})
	}
}
