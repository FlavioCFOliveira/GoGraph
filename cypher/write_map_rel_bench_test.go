package cypher_test

// write_map_rel_bench_test.go — the measurement behind rmp #2960.
//
// The per-row evaluators of the write-path maps (SET x = {…} / SET x += {…},
// CREATE and MERGE property maps, MERGE ON CREATE / ON MATCH actions) build a
// row context for every input row. Resolving a bound relationship to its value
// there adds per-row reads (endpoint keys, stored orientation, type and
// property bag). These benchmarks drive each evaluator over a fixed 256-row
// input, with the relationship read, bound but unread, and absent.
//
// Run:
//
//	go test -run '^$' -bench 'BenchmarkWriteMapRel' -benchmem ./cypher/

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newWriteMapRelEngine(b *testing.B) *cypher.Engine {
	b.Helper()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
	runWriteMapRel(b, eng, `UNWIND range(1, 256) AS i CREATE (:A {id: i})-[:T {p: i}]->(:B {id: i})`)
	runWriteMapRel(b, eng, `CREATE (:X {id: 1})`)
	return eng
}

func runWriteMapRel(b *testing.B, eng *cypher.Engine, query string) {
	b.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		b.Fatalf("%q: %v", query, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		b.Fatalf("%q drain: %v", query, err)
	}
	res.Close()
}

func BenchmarkWriteMapRel(b *testing.B) {
	shapes := []struct{ name, query string }{
		{"SetMap/RelProp", `MATCH (:A)-[r:T]->(:B), (x:X) SET x += {u: r.p}`},
		{"SetMap/RelBoundUnread", `MATCH (a:A)-[r:T]->(:B), (x:X) SET x += {u: a.id}`},
		{"SetMap/NoRel", `MATCH (a:A), (x:X) SET x += {u: a.id}`},
		{"MergeAction/RelProp", `MATCH (:A)-[r:T]->(:B) MERGE (x:X {id: 1}) ON MATCH SET x.u = r.p`},
		{"MergeAction/RelBoundUnread", `MATCH (a:A)-[r:T]->(:B) MERGE (x:X {id: 1}) ON MATCH SET x.u = a.id`},
	}
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			eng := newWriteMapRelEngine(b)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				runWriteMapRel(b, eng, s.query)
			}
		})
	}
}
