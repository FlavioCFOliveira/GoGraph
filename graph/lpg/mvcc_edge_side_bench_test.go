package lpg

// mvcc_edge_side_bench_test.go — what versioning the per-edge side stores costs
// a read that never needs a version (rmp #2291).
//
// The claim under test is the fast-path claim every phase of this programme has
// had to make and measure: a graph holding no live version of a store must read
// it for one uncontended atomic load more than before. If that is not true the
// design fails on the no-regression mandate, because the overwhelming majority
// of reads are on objects no concurrent writer has touched.
//
// Each benchmark reads a pair that HAS the state being measured — a second
// relationship type in overflow, a per-handle type, a per-handle property —
// after reclamation has driven the version count to zero. That is the shape of
// a steady-state read: the data is there, the history is not.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// edgeSideFixture builds pairs carrying overflow types and per-handle metadata,
// then sweeps the version chains so the benchmark measures the fast path.
func edgeSideFixture(b *testing.B, pairs int) (*Graph[string, float64], []graph.NodeID, []graph.NodeID) {
	b.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	srcs := make([]graph.NodeID, pairs)
	dsts := make([]graph.NodeID, pairs)
	for i := 0; i < pairs; i++ {
		s := "s" + strconv.Itoa(i)
		d := "d" + strconv.Itoa(i)
		if err := g.ApplyAtomically(func() error {
			if err := g.AddNode(s); err != nil {
				return err
			}
			if err := g.AddNode(d); err != nil {
				return err
			}
			if err := g.AddEdge(s, d, 1); err != nil {
				return err
			}
			// Two types: the second has nowhere to go but overflow.
			g.SetEdgeLabel(s, d, "KNOWS")
			g.SetEdgeLabel(s, d, "LIKES")
			g.SetEdgeLabelByHandle(s, d, 1, "KNOWS")
			return g.SetEdgePropertyByHandle(s, d, 1, "since", Int64Value(2020))
		}); err != nil {
			b.Fatalf("fixture %d: %v", i, err)
		}
		srcs[i], _ = g.adj.Mapper().Lookup(s)
		dsts[i], _ = g.adj.Mapper().Lookup(d)
	}
	// Steady state: the data is present, the history is swept.
	if err := g.ApplyAtomically(func() error { g.ReclaimNow(); return nil }); err != nil {
		b.Fatalf("ReclaimNow: %v", err)
	}
	if n := g.VersionCount(); n != 0 {
		b.Fatalf("fixture left %d live versions, so this measures the SLOW path", n)
	}
	return g, srcs, dsts
}

func BenchmarkEdgeSideRead_LabelsByID(b *testing.B) {
	g, srcs, dsts := edgeSideFixture(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(srcs)
		if out := g.EdgeLabelsByID(srcs[j], dsts[j]); len(out) != 2 {
			b.Fatalf("got %d labels, want 2", len(out))
		}
	}
}

func BenchmarkEdgeSideRead_LabelsByHandle(b *testing.B) {
	g, srcs, dsts := edgeSideFixture(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(srcs)
		if out := g.EdgeLabelsByHandleID(srcs[j], dsts[j], 1); len(out) != 1 {
			b.Fatalf("got %d labels, want 1", len(out))
		}
	}
}

func BenchmarkEdgeSideRead_PropertiesByHandle(b *testing.B) {
	g, srcs, dsts := edgeSideFixture(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(srcs)
		if out := g.EdgePropertiesByHandleID(srcs[j], dsts[j], 1); len(out) != 1 {
			b.Fatalf("got %d properties, want 1", len(out))
		}
	}
}

// The AsOf forms with a nil snapshot: what the Cypher read path will call once
// P4b threads a snapshot through the physical build, and what a writer inside
// the barrier calls today. They skip the compatibility delegation frame that
// the plain accessors above keep for the published Go API.

func BenchmarkEdgeSideRead_LabelsByHandleAsOf(b *testing.B) {
	g, srcs, dsts := edgeSideFixture(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(srcs)
		if out := g.EdgeLabelsByHandleIDAsOf(srcs[j], dsts[j], 1, nil); len(out) != 1 {
			b.Fatalf("got %d labels, want 1", len(out))
		}
	}
}

func BenchmarkEdgeSideRead_PropertiesByHandleAsOf(b *testing.B) {
	g, srcs, dsts := edgeSideFixture(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(srcs)
		if out := g.EdgePropertiesByHandleIDAsOf(srcs[j], dsts[j], 1, nil); len(out) != 1 {
			b.Fatalf("got %d properties, want 1", len(out))
		}
	}
}
