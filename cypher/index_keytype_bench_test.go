package cypher_test

// index_keytype_bench_test.go — string versus numeric indexed point lookup
// (task #2226).
//
// The round-4 three-way comparison measured an indexed STRING point lookup at
// 6 µs — the fastest of the three engines by 47× — and an indexed NUMERIC point
// lookup at 762 µs on the same data at the same scale. 127× between two lookups
// that differ only in the type of the key.
//
// #2169 made numeric equality CORRECT by rewriting `n.prop = v` into the
// degenerate closed range [v, v] over a unified float64 companion btree. This
// benchmark asks whether it is also FAST, and pins the ratio so it cannot
// silently regress.
//
// Both arms are built identically: same node count, one index, a point lookup on
// a key that exists, warmed once so the plan cache is not being measured. The
// only difference is the property's type.
//
// Run:
//
//	go test -run '^$' -bench 'BenchmarkIndexedPointLookup' -benchmem ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newKeyTypeEngine seeds n nodes labelled :K, each carrying both a string key
// `sk` and a numeric key `nk` derived from the same i, then creates a btree
// index on the property named by indexed. Seeding goes through the Go API: a
// Cypher seed would dominate the setup.
func newKeyTypeEngine(b *testing.B, n int, indexed, idxType string) *cypher.Engine {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := g.AddNode(key); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "K"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "sk", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			b.Fatalf("SetNodeProperty sk: %v", err)
		}
		if err := g.SetNodeProperty(key, "nk", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty nk: %v", err)
		}
	}
	eng := cypher.NewEngine(g)
	ddl := fmt.Sprintf("CREATE INDEX k_%s FOR (n:K) ON (n.%s) OPTIONS {indexType: '%s'}", indexed, indexed, idxType)
	res, err := eng.RunInTx(context.Background(), ddl, nil)
	if err != nil {
		b.Fatalf("CREATE INDEX %s: %v", indexed, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		b.Fatalf("CREATE INDEX %s: %v", indexed, err)
	}
	_ = res.Close()
	return eng
}

// runLookup executes query once and drains it.
func runLookup(b *testing.B, eng *cypher.Engine, query string, params map[string]expr.Value) {
	b.Helper()
	res, err := eng.Run(context.Background(), query, params)
	if err != nil {
		b.Fatalf("Run %q: %v", query, err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		b.Fatalf("iterate %q: %v", query, err)
	}
	_ = res.Close()
	if rows != 1 {
		b.Fatalf("%q returned %d rows, want exactly 1; the lookup must hit", query, rows)
	}
}

// BenchmarkIndexedPointLookup is the head-to-head matrix: each key type against
// each index type. The hash index is the DEFAULT (ir.IndexTypeHash), so the
// hash rows are what a user gets from a plain CREATE INDEX.
func BenchmarkIndexedPointLookup(b *testing.B) {
	type arm struct{ prop, idxType string }
	arms := []arm{
		{"sk", "hash"},
		{"sk", "btree"},
		{"nk", "hash"},
		{"nk", "btree"},
	}
	for _, n := range []int{5000, 20000} {
		for _, a := range arms {
			kind := "string"
			if a.prop == "nk" {
				kind = "numeric"
			}
			b.Run(fmt.Sprintf("%s/%s/n=%d", kind, a.idxType, n), func(b *testing.B) {
				eng := newKeyTypeEngine(b, n, a.prop, a.idxType)
				var q string
				if a.prop == "sk" {
					q = fmt.Sprintf(`MATCH (a:K {sk: 's%d'}) RETURN a`, n/2)
				} else {
					q = fmt.Sprintf(`MATCH (a:K {nk: %d}) RETURN a`, n/2)
				}
				runLookup(b, eng, q, nil)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					runLookup(b, eng, q, nil)
				}
			})
		}
	}
}

// BenchmarkIndexedNumericRange checks whether the companion a hash index now
// carries (#2226) also serves a numeric RANGE, not just an equality. If it
// does, the default CREATE INDEX accelerates both, and the documented table of
// "which kind accelerates which predicate" changes for numeric values.
func BenchmarkIndexedNumericRange(b *testing.B) {
	const n = 20000
	for _, idxType := range []string{"hash", "btree"} {
		b.Run(idxType, func(b *testing.B) {
			eng := newKeyTypeEngine(b, n, "nk", idxType)
			// A narrow range: 10 of 20000 rows, so a seek and a scan differ sharply.
			q := fmt.Sprintf(`MATCH (a:K) WHERE a.nk >= %d AND a.nk < %d RETURN count(a) AS c`, n/2, n/2+10)
			res, err := eng.Run(context.Background(), q, nil)
			if err != nil {
				b.Fatalf("warm: %v", err)
			}
			for res.Next() {
			}
			_ = res.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := eng.Run(context.Background(), q, nil)
				if err != nil {
					b.Fatalf("range: %v", err)
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					b.Fatalf("range iterate: %v", err)
				}
				_ = res.Close()
			}
		})
	}
}
