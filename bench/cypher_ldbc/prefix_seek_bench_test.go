package cypher_ldbc_test

// prefix_seek_bench_test.go — benchmark for the STARTS WITH prefix range seek
// (#2127, measured under #2129).
//
// A prefix predicate IS a range predicate, but before #2127 the planner did not
// recognise the shape: `p.name STARTS WITH 'name002'` walked every :PfxBench
// node and refiltered each row, while the semantically identical
// `>= 'name002' AND < 'name003'` descended the bound btree. This benchmark pairs
// the rewrite ENABLED against DISABLED on one fixture, and includes the
// range-equivalent form as the reference target the prefix form should now match.
//
// The fixture is the one the round-2 planner audit measured
// (docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md §2.1): 50 000 nodes with a
// sortable string property, a btree index on it, and a prefix selecting 100 rows
// — 0.2% of the population, comfortably inside the seek's 10% gate.
//
// TestPrefixSeekBenchRowCountsAgree asserts all three variants return the SAME
// 100 rows before any timing is believed, so the benchmark compares like with
// like rather than comparing a fast wrong answer against a slow right one.
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkPrefixSeek -benchmem -count=10 ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pfxBenchPop is the :PfxBench population — the audit's 50 000, far above the
// seek's 1024-node floor so scanning it dominates a selective descent.
const pfxBenchPop = 50_000

const (
	// pfxBenchQuery selects the 100 names beginning "name002" (name00200 …
	// name00299) — 0.2% of the population.
	pfxBenchQuery = `MATCH (p:PfxBench) WHERE p.name STARTS WITH "name002" RETURN p.name AS name`
	// pfxBenchRangeQuery is the hand-written range equivalent: the same 100 rows,
	// and the cost the prefix form is meant to reach.
	pfxBenchRangeQuery = `MATCH (p:PfxBench) WHERE p.name >= "name002" AND p.name < "name003" RETURN p.name AS name`
	// pfxBenchRangeQuery3 adds a third, non-narrowing conjunct to the range form.
	// Same 100 rows — but NOT the same plan, which is the point it now records.
	//
	// It was introduced to isolate the cost of the residual predicate's ARITY
	// (the hypothesis for why the one-conjunct prefix form beats the two-conjunct
	// range form) by holding the access path fixed. Measurement refuted the
	// premise: extractStringRangePred recognises only an exact TWO-way AND, so a
	// third conjunct makes it decline and the index seek is LOST, whereupon the
	// all-comparison predicate becomes ColumnarFilter-eligible. The arm therefore
	// compares three different plans, not one plan with three residual sizes:
	//
	//   1 conjunct  STARTS WITH   Filter        → NodeByIndexRangeScan     30.67 µs / 368 allocs
	//   2 conjuncts >= AND <      Filter        → NodeByIndexRangeScan     38.63 µs / 566 allocs
	//   3 conjuncts >= AND < AND  ColumnarFilter → NodeByLabelScan       3566.12 µs / 104 allocs
	//
	// Keep it: it pins a real and costly planner cliff — one extra conjunct on a
	// range predicate gives up the seek and runs 92× slower (rmp backlog #2245) —
	// and it is the evidence for docs/benchmarks/prefix-range-seek-2026-07-28.md §3.
	pfxBenchRangeQuery3 = `MATCH (p:PfxBench) WHERE p.name >= "name002" AND p.name < "name003" AND p.name <> "zzz" RETURN p.name AS name`
	// pfxBenchExpectedRows is the row count every variant must return.
	pfxBenchExpectedRows = 100
)

// buildPrefixSeekBenchEngine seeds a large :PfxBench label keyed by a sortable
// "name" string and creates the bound btree through the engine's CREATE INDEX
// path — the only way to obtain a self-maintaining, backfilled btree.
func buildPrefixSeekBenchEngine(tb testing.TB, disablePrefix bool) *cypher.Engine {
	tb.Helper()
	// Directed + Multigraph is the openCypher storage model. It is also what keeps
	// the engine from logging its non-directed / non-multigraph warnings, which
	// would interleave with the benchmark output and make benchstat silently
	// discard the affected samples.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < pfxBenchPop; i++ {
		key := fmt.Sprintf("p%05d", i)
		_ = g.AddNode(key)
		_ = g.SetNodeLabel(key, "PfxBench")
		_ = g.SetNodeProperty(key, "name", lpg.StringValue(fmt.Sprintf("name%05d", i)))
	}
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisablePrefixIndexSeek: disablePrefix,
		MaxResultRows:          cypher.MaxResultRowsUnlimited,
	})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:PfxBench) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
		tb.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// countPrefixBenchRows drains q and returns the row count.
func countPrefixBenchRows(tb testing.TB, eng *cypher.Engine, q string) int {
	tb.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatalf("Run: %v", err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err: %v", err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close: %v", err)
	}
	return n
}

// TestPrefixSeekBenchRowCountsAgree is the companion correctness gate: all three
// benchmarked variants must return the same 100 rows. Without it the benchmark
// could report a large "win" that is really a wrong answer.
func TestPrefixSeekBenchRowCountsAgree(t *testing.T) {
	engOn := buildPrefixSeekBenchEngine(t, false)
	engOff := buildPrefixSeekBenchEngine(t, true)

	for _, tc := range []struct {
		name  string
		eng   *cypher.Engine
		query string
	}{
		{"prefix_seek_enabled", engOn, pfxBenchQuery},
		{"prefix_seek_disabled", engOff, pfxBenchQuery},
		{"range_equivalent", engOn, pfxBenchRangeQuery},
		{"range_equivalent_three_conjuncts", engOn, pfxBenchRangeQuery3},
	} {
		if got := countPrefixBenchRows(t, tc.eng, tc.query); got != pfxBenchExpectedRows {
			t.Fatalf("%s: %d rows, want %d", tc.name, got, pfxBenchExpectedRows)
		}
	}
}

func benchPrefixSeek(b *testing.B, eng *cypher.Engine, query string) {
	b.Helper()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, query, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkPrefixSeek_IndexSeek measures the rewritten prefix plan.
func BenchmarkPrefixSeek_IndexSeek(b *testing.B) {
	benchPrefixSeek(b, buildPrefixSeekBenchEngine(b, false), pfxBenchQuery)
}

// BenchmarkPrefixSeek_LabelScan measures the same prefix predicate with the
// rewrite disabled — the NodeByLabelScan + Filter plan it replaces.
func BenchmarkPrefixSeek_LabelScan(b *testing.B) {
	benchPrefixSeek(b, buildPrefixSeekBenchEngine(b, true), pfxBenchQuery)
}

// BenchmarkPrefixSeek_RangeEquivalent measures the hand-written range form on
// the same fixture — the reference cost the prefix rewrite targets. The prefix
// seek should land on this number, not merely improve on the scan.
func BenchmarkPrefixSeek_RangeEquivalent(b *testing.B) {
	benchPrefixSeek(b, buildPrefixSeekBenchEngine(b, false), pfxBenchRangeQuery)
}

// BenchmarkPrefixSeek_RangeEquivalent3 adds a third, non-narrowing conjunct to
// the range form. It was meant to hold the access path fixed and vary only the
// residual predicate's arity; measurement showed it does not — the third conjunct
// costs the seek outright. See the pfxBenchRangeQuery3 comment for the three
// plans and the numbers. It is retained as the regression witness for that cliff.
func BenchmarkPrefixSeek_RangeEquivalent3(b *testing.B) {
	benchPrefixSeek(b, buildPrefixSeekBenchEngine(b, false), pfxBenchRangeQuery3)
}
