package bulkimport

// bulkimport_bench_test.go — rmp #2178.
//
// Records the ingest rate at the round-3 audit's own dataset shape, 20 000 nodes
// and 200 000 edges, so the figure the #2177 spike measured (2.72 M edges/s) is
// held by a standing benchmark rather than by a note in a design document.
//
//	go test -run x -bench BenchmarkImport -benchmem -count=6 ./store/bulkimport/
//
// The reference point that matters is not GoGraph's own before-state but the
// incumbents': on this dataset Memgraph loads in 977 ms and Neo4j in 2.39 s,
// against the Cypher write path's 35 m 33 s.
//
// Layer: short (bench-only).

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	benchNodes = 20_000
	benchEdges = 200_000
)

// buildBenchFixture generates the audit's shape: every node carries a label and
// an integer property, every edge a type and a property, because an ingest path
// that carried only adjacency would not be comparable with what Memgraph and
// Neo4j were asked to load.
func buildBenchFixture() ([]Node, []Edge[int64]) {
	key := func(i int) string { return "n" + strconv.Itoa(i) }
	nodes := make([]Node, benchNodes)
	for i := 0; i < benchNodes; i++ {
		nodes[i] = Node{
			Key:        key(i),
			Labels:     []string{"Person"},
			Properties: map[string]lpg.PropertyValue{"id": lpg.Int64Value(int64(i))},
		}
	}
	edges := make([]Edge[int64], benchEdges)
	for i := 0; i < benchEdges; i++ {
		edges[i] = Edge[int64]{
			Src: key(i % benchNodes), Dst: key((i*7919 + 13) % benchNodes),
			Weight: int64(i), Type: "KNOWS",
			Properties: map[string]lpg.PropertyValue{"since": lpg.Int64Value(int64(i % 2000))},
		}
	}
	return nodes, edges
}

// BenchmarkImport_LabelsAndProperties measures the whole in-memory build and
// reports edges/s directly, so a regression is visible as a rate rather than as
// a duration that has to be divided by hand.
func BenchmarkImport_LabelsAndProperties(b *testing.B) {
	nodes, edges := buildBenchFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builder := New[int64](Options{Directed: true, Multigraph: true, ExpectNodes: benchNodes})
		if err := builder.AddNodes(nodes); err != nil {
			b.Fatal(err)
		}
		if err := builder.AddEdges(edges); err != nil {
			b.Fatal(err)
		}
		st, err := builder.Finish()
		if err != nil {
			b.Fatal(err)
		}
		if st.Edges != benchEdges {
			b.Fatalf("ingested %d edges, want %d", st.Edges, benchEdges)
		}
	}
	b.StopTimer()
	// edges/s at the measured per-iteration cost.
	if b.Elapsed() > 0 {
		perOp := b.Elapsed().Seconds() / float64(b.N)
		b.ReportMetric(float64(benchEdges)/perOp/1e6, "Medges/s")
	}
}

// BenchmarkImport_NodesOnly isolates the node half, so a future regression can be
// attributed to node or edge ingest rather than to the total.
func BenchmarkImport_NodesOnly(b *testing.B) {
	nodes, _ := buildBenchFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builder := New[int64](Options{Directed: true, Multigraph: true, ExpectNodes: benchNodes})
		if err := builder.AddNodes(nodes); err != nil {
			b.Fatal(err)
		}
		if _, err := builder.Finish(); err != nil {
			b.Fatal(err)
		}
	}
}
