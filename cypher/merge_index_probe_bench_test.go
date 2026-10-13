package cypher_test

// merge_index_probe_bench_test.go — the measurement behind rmp #2812.
//
// With a property index on (:L, k), the MERGE match phase used to walk the whole
// label posting list and re-check every node, so an indexed MERGE cost O(label
// population) per call. The probe replaces that walk with an index lookup whose
// candidates are re-checked in full, so the cost of a MERGE that matches one
// node should stay flat as the label grows.
//
// The sweep holds everything fixed except the label population and covers both
// index families and both MERGE forms:
//
//	idx=hash   string key, the default CREATE INDEX kind
//	idx=btree  integer key, served by the numeric companion btree
//	form=literal  MERGE (n:L {k: <literal>})          the closure search
//	form=unwind   UNWIND $rows AS r MERGE (n:L {k: r}) the row-aware search
//
// Every MERGE matches an existing node, so the measurement is of the match phase
// and never of node creation.
//
// Run:
//
//	go test -run '^$' -bench 'BenchmarkMergeIndexProbe' -benchmem -count=6 ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newMergeProbeBenchEngine seeds pop nodes labelled :L carrying a key k — the
// string "k<i>" when stringKey is set, the integer i otherwise — and then
// creates the index. Seeding goes through the Go API before the index exists,
// which is the documented way to populate a graph that is indexed afterwards.
func newMergeProbeBenchEngine(b *testing.B, pop int, stringKey bool, ddl string) *cypher.Engine {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < pop; i++ {
		n := fmt.Sprintf("n%d", i)
		if err := g.AddNode(n); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(n, "L"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		v := lpg.Int64Value(int64(i))
		if stringKey {
			v = lpg.StringValue(fmt.Sprintf("k%d", i))
		}
		if err := g.SetNodeProperty(n, "k", v); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	eng := cypher.NewEngine(g)
	if _, err := eng.Run(context.Background(), ddl, nil); err != nil {
		b.Fatalf("%s: %v", ddl, err)
	}
	return eng
}

func drainMergeProbe(b *testing.B, eng *cypher.Engine, q string, params map[string]any) {
	b.Helper()
	res, err := eng.RunInTxAny(context.Background(), q, params)
	if err != nil {
		b.Fatalf("%s: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		b.Fatalf("%s iterate: %v", q, err)
	}
	_ = res.Close()
}

// BenchmarkMergeIndexProbe sweeps the label population under an index on the
// merged key. Flat across the sweep means the match phase is index-driven;
// linear means it walks the label.
func BenchmarkMergeIndexProbe(b *testing.B) {
	type family struct {
		name      string
		stringKey bool
		ddl       string
		literal   string
		unwindArg any
	}
	families := []family{
		{
			name: "hash", stringKey: true,
			ddl:       "CREATE INDEX FOR (n:L) ON (n.k)",
			literal:   `MERGE (n:L {k: 'k7'}) RETURN count(n) AS c`,
			unwindArg: "k7",
		},
		{
			name: "btree", stringKey: false,
			ddl:       "CREATE INDEX FOR (n:L) ON (n.k) OPTIONS {indexType: 'btree'}",
			literal:   `MERGE (n:L {k: 7}) RETURN count(n) AS c`,
			unwindArg: int64(7),
		},
	}
	const unwind = `UNWIND $rows AS r MERGE (n:L {k: r}) RETURN count(n) AS c`
	for _, f := range families {
		for _, pop := range []int{1024, 4096, 16384} {
			eng := newMergeProbeBenchEngine(b, pop, f.stringKey, f.ddl)
			b.Run(fmt.Sprintf("idx=%s/form=literal/label=%d", f.name, pop), func(b *testing.B) {
				drainMergeProbe(b, eng, f.literal, nil) // warm the plan cache
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					drainMergeProbe(b, eng, f.literal, nil)
				}
			})
			b.Run(fmt.Sprintf("idx=%s/form=unwind/label=%d", f.name, pop), func(b *testing.B) {
				params := map[string]any{"rows": []any{f.unwindArg}}
				drainMergeProbe(b, eng, unwind, params)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					drainMergeProbe(b, eng, unwind, params)
				}
			})
		}
	}
}
