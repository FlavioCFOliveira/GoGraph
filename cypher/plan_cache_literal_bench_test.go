package cypher_test

// plan_cache_literal_bench_test.go — what a plan-cache MISS costs on this
// project (rmp #2393).
//
// The plan cache keys on the query text verbatim (Engine.parseAndAnalyse →
// e.cache.get(query)), and there is no literal extraction, stripping or
// auto-parameterisation anywhere in cypher/. So two statements differing only in
// an inlined literal are two distinct entries: a workload that inlines literals
// instead of binding parameters misses on EVERY statement and additionally churns
// the 1024-entry LRU. Both Memgraph and Neo4j strip literals before keying.
//
// #2393 is explicitly measurement-first: the size of the win is unquantified and
// must not be assumed, so this benchmark measures the miss/hit ratio BEFORE any
// design work. If a miss is cheap the task closes on this evidence with no code
// change.
//
// Method. The two arms differ in exactly ONE respect — whether the varying value
// arrives as a parameter (cache hit) or inlined in the text (cache miss). Both
// vary the value, both do the same engine work, both run in one process so they
// are interleaved by `go test -bench` rather than compared across sessions.
//
// The miss arm cycles through planCacheLiteralVariants distinct texts, which must
// EXCEED DefaultPlanCacheCapacity (1024): with more distinct texts than the LRU
// holds, a text is always evicted before it comes round again, so every lookup is
// a genuine miss. With fewer, the arm would silently become a hit arm — the kind
// of harness bug that makes a measurement meaningless.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkPlanCache -benchmem -count=5 ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// planCacheLiteralVariants is the number of distinct query texts the miss arm
// cycles through. It must exceed cypher.DefaultPlanCacheCapacity so every lookup
// misses; the assertion below enforces that rather than trusting the constant.
const planCacheLiteralVariants = 2048

// planCacheSeedNodes is deliberately small. The point of measurement is the FRONT
// END (parse, semantic analysis, IR construction, plan build), so execution must
// not dominate: a large graph would bury the very difference being measured.
const planCacheSeedNodes = 64

// newPlanCacheEngine builds a small :Account graph for the literal benchmarks.
func newPlanCacheEngine(tb testing.TB) *cypher.Engine {
	tb.Helper()
	// Multigraph, not newBenchGraph()'s plain directed graph: this fixture CREATEs
	// nodes, and the engine warns once per construction otherwise — a warning that
	// lands ON the benchmark result line and hides the ns/op figure.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for i := range planCacheSeedNodes {
		res, err := eng.RunInTx(context.Background(),
			fmt.Sprintf("CREATE (n:Account {id: %d})", i), nil)
		if err != nil {
			tb.Fatalf("seed %d: %v", i, err)
		}
		for res.Next() { // intentional full drain
		}
		if err := res.Close(); err != nil {
			tb.Fatalf("seed close %d: %v", i, err)
		}
	}
	return eng
}

// runPlanCacheQuery runs one query to completion, failing on any error.
func runPlanCacheQuery(b *testing.B, eng *cypher.Engine, q string, params map[string]expr.Value) {
	res, err := eng.RunInTx(context.Background(), q, params)
	if err != nil {
		b.Fatalf("run %q: %v", q, err)
	}
	for res.Next() { // intentional full drain
	}
	if err := res.Err(); err != nil {
		b.Fatalf("result %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		b.Fatalf("close %q: %v", q, err)
	}
}

// BenchmarkPlanCacheHit_Parameterised is the HIT arm: one query text, the value
// bound as a parameter, so every lookup after the first hits the cache.
func BenchmarkPlanCacheHit_Parameterised(b *testing.B) {
	eng := newPlanCacheEngine(b)
	const q = "MATCH (n:Account {id: $id}) RETURN n.id"
	// Warm the entry so the first timed iteration is not charged the only miss
	// this arm ever takes.
	runPlanCacheQuery(b, eng, q, map[string]expr.Value{"id": expr.IntegerValue(0)})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runPlanCacheQuery(b, eng, q, map[string]expr.Value{
			"id": expr.IntegerValue(int64(i % planCacheSeedNodes)),
		})
	}
}

// BenchmarkPlanCacheMiss_InlinedLiteral is the MISS arm: the same shape and the
// same varying value, but inlined into the text, so each statement is a distinct
// cache key and every lookup misses.
func BenchmarkPlanCacheMiss_InlinedLiteral(b *testing.B) {
	eng := newPlanCacheEngine(b)

	// Pre-render the texts so string formatting is not charged to the engine.
	// Values stay within the seeded range so both arms match the same rows; the
	// variant index only makes the TEXT distinct.
	texts := make([]string, planCacheLiteralVariants)
	for i := range texts {
		texts[i] = fmt.Sprintf("MATCH (n:Account {id: %d}) RETURN n.id /*%d*/",
			i%planCacheSeedNodes, i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runPlanCacheQuery(b, eng, texts[i%planCacheLiteralVariants], nil)
	}
}

// BenchmarkPlanCacheMiss_InlinedLiteralNoComment is the miss arm without the
// trailing comment: the literal itself is what varies, exactly as a generated or
// ad-hoc workload would emit it. It is the honest shape of the problem, kept
// alongside the comment variant so the comment cannot be blamed for the result.
//
// It cycles only planCacheSeedNodes distinct texts (64 < 1024), so after the
// first pass every lookup HITS. That is the point: it isolates how much of the
// miss arm's cost is the distinct-text count rather than the inlining itself.
func BenchmarkPlanCacheMiss_InlinedLiteralNoComment(b *testing.B) {
	eng := newPlanCacheEngine(b)
	texts := make([]string, planCacheSeedNodes)
	for i := range texts {
		texts[i] = fmt.Sprintf("MATCH (n:Account {id: %d}) RETURN n.id", i)
	}
	for _, q := range texts { // warm all 64 entries
		runPlanCacheQuery(b, eng, q, nil)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runPlanCacheQuery(b, eng, texts[i%planCacheSeedNodes], nil)
	}
}

// TestPlanCacheLiteralArmsAreValid guards the two premises the benchmark's
// conclusion rests on. Without it the miss arm could silently degrade into a hit
// arm and still report a plausible number — the failure mode that makes a
// measurement worse than none.
func TestPlanCacheLiteralArmsAreValid(t *testing.T) {
	// 1. The miss arm must cycle MORE distinct texts than the cache can hold,
	//    or its lookups are hits.
	if planCacheLiteralVariants <= cypher.DefaultPlanCacheCapacity {
		t.Fatalf("miss arm cycles %d distinct texts against a %d-entry cache: every "+
			"lookup would HIT after the first pass, so the arm would not measure a miss",
			planCacheLiteralVariants, cypher.DefaultPlanCacheCapacity)
	}
	// 2. Both arms must return the same rows, or they are not the same workload.
	eng := newPlanCacheEngine(t)
	ctx := context.Background()
	countRows := func(q string, params map[string]expr.Value) int {
		res, err := eng.RunInTx(ctx, q, params)
		if err != nil {
			t.Fatalf("run %q: %v", q, err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("result %q: %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("close %q: %v", q, err)
		}
		return n
	}
	got := countRows("MATCH (n:Account {id: $id}) RETURN n.id", map[string]expr.Value{"id": expr.IntegerValue(7)})
	want := countRows("MATCH (n:Account {id: 7}) RETURN n.id", nil)
	if got != want {
		t.Errorf("parameterised arm returned %d rows, inlined arm %d: the arms are not "+
			"running the same workload, so their costs are not comparable", got, want)
	}
	if want == 0 {
		t.Error("both arms returned 0 rows: the benchmark would measure a query that " +
			"matches nothing, and a plan for no rows is not the plan being studied")
	}
}
