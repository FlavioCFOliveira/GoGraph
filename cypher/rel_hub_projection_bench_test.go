package cypher_test

// rel_hub_projection_bench_test.go — relationship projection out of ONE
// high-degree source (rmp #3089). Per-pair type and property reads used to scan
// every slot of the source per row, O(deg) per row and O(deg²) per source; the
// Go-API arms exercise that path, the Cypher arm (by-handle records) is the
// control that never took it.
//
//	go test -run=^$ -bench=BenchmarkRelHubProjection -benchmem -count=10 ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const relHubDegree = 2000

func relHubGoAPI(b *testing.B) *cypher.Engine {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	must := func(err error) {
		if err != nil {
			b.Fatal(err)
		}
	}
	must(g.AddNode("h"))
	must(g.SetNodeLabel("h", "H"))
	for i := 0; i < relHubDegree; i++ {
		k := fmt.Sprintf("t%d", i)
		must(g.AddNode(k))
		must(g.AddEdgeLabeledWithProperty("h", k, 1, "FRIEND", "since", lpg.Int64Value(int64(i))))
	}
	return cypher.NewEngine(g)
}

func relHubCypher(b *testing.B) *cypher.Engine {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.RunInTx(context.Background(), fmt.Sprintf(
		"CREATE (h:H) WITH h UNWIND range(1, %d) AS i CREATE (h)-[:FRIEND {since: i}]->(:T)", relHubDegree), nil)
	if err != nil {
		b.Fatal(err)
	}
	for res.Next() { // intentional full drain
	}
	if err := res.Err(); err != nil {
		b.Fatal(err)
	}
	_ = res.Close()
	return eng
}

func runRelHub(b *testing.B, eng *cypher.Engine, q string) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			b.Fatal(err)
		}
		_ = res.Close()
		if n != relHubDegree {
			b.Fatalf("rows = %d, want %d", n, relHubDegree)
		}
	}
}

func BenchmarkRelHubProjection(b *testing.B) {
	const (
		lazyQ  = "MATCH (:H)-[r]->(x) RETURN r.since"
		eagerQ = "MATCH (:H)-[r]->(x) RETURN type(r), properties(r)"
		presQ  = "MATCH (:H)-[r]->(x) RETURN r.since IS NOT NULL"
	)
	b.Run("GoAPI/lazy", func(b *testing.B) { runRelHub(b, relHubGoAPI(b), lazyQ) })
	b.Run("GoAPI/eager", func(b *testing.B) { runRelHub(b, relHubGoAPI(b), eagerQ) })
	b.Run("GoAPI/presence", func(b *testing.B) { runRelHub(b, relHubGoAPI(b), presQ) })
	b.Run("Cypher/eager", func(b *testing.B) { runRelHub(b, relHubCypher(b), eagerQ) })
}
