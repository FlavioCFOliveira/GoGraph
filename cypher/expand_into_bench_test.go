package cypher_test

// expand_into_bench_test.go — evidence for expand-into (#2206).
//
// A pattern that closes a cycle, or re-uses an already-bound endpoint, produces a hop
// whose destination is fixed. The IR translator detects this (destRebinding) and expands
// into a synthetic `__anon_N_to_<var>` with an equality Selection above, so before #2206
// the operator emitted one row per NEIGHBOUR of the source — building and boxing a
// (srcID, edgeID, dstID) triplet for each — and the Selection discarded all but the one
// that landed on the bound node.
//
// The round-3 audit measured triangle counting at 107x Memgraph's and named the missing
// expand-into as the cause. These benchmarks isolate the operator-level effect: each
// closing-hop query is paired with an open-hop query of the same shape, so the difference
// is the wasted per-neighbour row construction and nothing else.
//
//	go test -run=^$ -bench='BenchmarkExpandInto' -benchmem -count=6 ./cypher/
//
// Layer: short.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// expandIntoNodes and expandIntoDegree size the fixture. A meaningful out-degree is the
// point: the wasted work before #2206 is proportional to it, so a degree-1 graph would
// measure nothing.
const (
	expandIntoNodes  = 2000
	expandIntoDegree = 16
)

// seedExpandIntoGraph builds a labelled graph where every node has expandIntoDegree
// out-edges of type K, arranged so that cycles exist: node i points at
// (i+1), (i+2), ..., (i+degree) modulo the population.
func seedExpandIntoGraph(b *testing.B) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, expandIntoNodes)
	for i := 0; i < expandIntoNodes; i++ {
		k := "n" + strconv.Itoa(i)
		keys[i] = k
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
	}
	for i := 0; i < expandIntoNodes; i++ {
		for d := 1; d <= expandIntoDegree; d++ {
			j := (i + d) % expandIntoNodes
			if err := g.AddEdge(keys[i], keys[j], 1.0); err != nil {
				b.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel(keys[i], keys[j], "K")
		}
	}
	return g
}

func benchExpandInto(b *testing.B, q string) {
	silenceBenchLogs(b)
	g := seedExpandIntoGraph(b)
	eng := cypher.NewEngine(g)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

// ── the closing hop: destination already bound ──

// BenchmarkExpandInto_TriangleClose closes a 3-cycle, so the third hop's destination is
// the already-bound `a`.
func BenchmarkExpandInto_TriangleClose(b *testing.B) {
	benchExpandInto(b, `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`)
}

// BenchmarkExpandInto_TwoCycleClose closes a 2-cycle — the shortest closing pattern.
func BenchmarkExpandInto_TwoCycleClose(b *testing.B) {
	benchExpandInto(b, `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN count(*) AS n`)
}

// ── the open controls: same shape, destination fresh ──

// BenchmarkExpandInto_TriangleOpen is the triangle without the closing hop bound, so no
// expand-into applies. It bounds how much of the pair's difference is the fixture rather
// than the operator.
func BenchmarkExpandInto_TriangleOpen(b *testing.B) {
	benchExpandInto(b, `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(d:P) RETURN count(*) AS n`)
}

func BenchmarkExpandInto_TwoHopOpen(b *testing.B) {
	benchExpandInto(b, `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P) RETURN count(*) AS n`)
}
