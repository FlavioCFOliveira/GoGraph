package lpg

// edge_source_slots_bench_test.go — per-read cost of the per-pair reads with
// and without the ReadView slot index, on example 26's measured read pattern
// (rmp #3089): every source read once per slot, in slot order, by a fresh view
// per pass, degree ~325, one slot per pair.
//
//	go test -run=^$ -bench=BenchmarkPairReads -benchmem ./graph/lpg/

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func pairReadsGraph(b *testing.B) (*Graph[string, float64], []graph.NodeID) {
	b.Helper()
	const sources, targets, deg = 64, 20000, 325
	g := New[string, float64](adjlist.Config{})
	for i := 0; i < targets; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			b.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(1))
	srcs := make([]graph.NodeID, 0, sources)
	for s := 0; s < sources; s++ {
		src := fmt.Sprintf("n%d", s)
		seen := map[int]bool{}
		for len(seen) < deg {
			d := rng.Intn(targets)
			if seen[d] {
				continue
			}
			seen[d] = true
			if err := g.AddEdgeLabeledWithProperty(src, fmt.Sprintf("n%d", d), 1, "FRIEND", "since", Int64Value(int64(d))); err != nil {
				b.Fatal(err)
			}
		}
		id, _ := g.adj.Mapper().Lookup(src)
		srcs = append(srcs, id)
	}
	return g, srcs
}

func BenchmarkPairReads(b *testing.B) {
	g, srcs := pairReadsGraph(b)
	m := g.adj.Mapper()
	type pr struct {
		sk, dk string
		s, d   graph.NodeID
	}
	var pairs []pr
	for _, s := range srcs {
		sk, _ := m.Resolve(s)
		for _, d := range g.EntryViewAsOf(s, nil).Neighbours {
			dk, _ := m.Resolve(d)
			pairs = append(pairs, pr{sk, dk, s, d})
		}
	}
	run := func(name string, read func(v *ReadView[string, float64], p pr)) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v := g.ReadAt(nil)
				for _, p := range pairs {
					read(v, p)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(pairs)), "ns/read")
		})
	}
	run("labels/scan", func(v *ReadView[string, float64], p pr) { _ = g.EdgeLabelsByIDAsOf(p.s, p.d, nil) })
	run("labels/view", func(v *ReadView[string, float64], p pr) { _ = v.EdgeLabelsByID(p.s, p.d) })
	run("prop/scan", func(v *ReadView[string, float64], p pr) { _, _ = g.GetEdgePropertyAsOf(p.sk, p.dk, "since", nil) })
	run("prop/view", func(v *ReadView[string, float64], p pr) { _, _ = v.GetEdgeProperty(p.sk, p.dk, "since") })
	run("pres/scan", func(v *ReadView[string, float64], p pr) { _ = g.EdgeHasPropertyAsOf(p.sk, p.dk, "since", nil) })
	run("pres/view", func(v *ReadView[string, float64], p pr) { _ = v.EdgeHasProperty(p.sk, p.dk, "since") })
}
