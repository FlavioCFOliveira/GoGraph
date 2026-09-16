package cypher

// frontend_share_bench_test.go — the front end's SHARE OF A WHOLE QUERY'S
// LATENCY, measured rather than inferred (rmp #2844, sprint 362 parsing
// campaign, stage 2).
//
// # The question
//
// The campaign's ceiling is not what the parser costs; it is what fraction of a
// query the parser is. That fraction is entirely determined by how expensive
// the query's EXECUTION is, so a single number would be a fiction. This file
// measures the fraction over a range of read queries taken from the corpus —
// from an indexed point lookup, whose execution is as cheap as a real query
// gets, to a full label scan with a sort — and for both plan-cache states.
//
// # Method
//
// Two arms, both PRODUCTION entry points, so nothing here is a transcription
// that can drift:
//
//	1parse — [Engine.parseAndAnalyse] followed by [mergeAutoParams], which is
//	         verbatim the first two statements of [Engine.runRead]
//	         (cypher/api.go:2528-2529). The FRONT END, and nothing else.
//	2full  — [Engine.Run] itself, drained and closed. The WHOLE query, as a
//	         caller experiences it, including the metrics defers, the DDL
//	         check, the snapshot, the physical build and the result
//	         materialisation.
//
// share = 1parse / 2full, per query, per cache state. Both arms run in one
// process against one rig, so the ratio is not carried across sessions.
//
// # Why not runReadPrefix
//
// An earlier version of this file drove [Engine.runReadPrefix], the
// cumulative-prefix instrument read_phase_attribution_bench_test.go carries for
// rmp #2292. It FAILED on the one statement here that carries a hoistable
// string literal:
//
//	cypher: ParameterMissing: MissingParameter: expected parameter $  auto_0
//
// runReadPrefix drops the auto-parameters [Engine.parseAndAnalyse] returns
// (`entry, _, err :=`, read_phase_attribution_bench_test.go:212) whereas
// [Engine.runRead] merges them (`params = mergeAutoParams(params, autoParams)`,
// cypher/api.go:2529). Its own fidelity guard cannot see the divergence,
// because both of its queries are quote-free and therefore hoist nothing. That
// is a defect in that instrument, recorded as a finding and NOT fixed here:
// this file simply stopped depending on it.
//
// # Cache state
//
//	hit  — the entry is published before the timer starts, so every iteration
//	       resolves the key and hits. This is what a server does on nearly
//	       every statement.
//	miss — the cache is cleared inside the timed loop before every iteration,
//	       so every iteration compiles. The clear is NOT free and it is NOT
//	       part of a real miss, so it is measured on its own as the
//	       "control/cacheclear" arm and subtracted when the share is computed.
//	       Both the parse arm and the full arm pay it identically, so it
//	       cancels in the numerator and the denominator's difference but not in
//	       their ratio; the report states both the raw and the corrected share.
//
// # The rig
//
// A USER graph in example 22_cypher's shape — id, name, age, city, and KNOWS
// out-edges carrying since — with an index on USER(id) so the point lookup is a
// genuine seek and so [Engine.compilePlanCacheEntry]'s InferParamTypes path has
// a schema to resolve against. No store: durability is out of this campaign's
// scope, and adding it would put a cost in the denominator that the question
// does not ask about.
//
// # Running it
//
//	go test -run=^$ -bench='^BenchmarkFrontEndShare$' -benchmem \
//	    -benchtime=100ms -count=10 ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// shareUsers is the population. Large enough that a label scan is real
	// work against a sub-microsecond front end, small enough that the rig
	// rebuilds quickly for every sub-benchmark.
	shareUsers = 5000
	// shareDegree is the fixed KNOWS out-degree. Fixed rather than random:
	// the measured queries do not traverse, so the degree only shapes the
	// adjacency the scan walks past, and a fixed one is reproducible.
	shareDegree = 4
	// shareCities is the number of distinct city values, so the city
	// predicate in the literal-bearing arm selects a real fraction of rows.
	shareCities = 8
)

// shareUserID is the id property of user i, in the 24-hex-character shape
// example 22_cypher uses.
func shareUserID(i int) string { return fmt.Sprintf("%024x", i) }

// newShareRig builds the USER/KNOWS population and the USER(id) index.
func newShareRig(tb testing.TB) *Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	for i := 0; i < shareUsers; i++ {
		key := fmt.Sprintf("u%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "USER"); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "id", lpg.StringValue(shareUserID(i))); err != nil {
			tb.Fatalf("SetNodeProperty id %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(fmt.Sprintf("user-%05d", i))); err != nil {
			tb.Fatalf("SetNodeProperty name %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(18+i%60))); err != nil {
			tb.Fatalf("SetNodeProperty age %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "city", lpg.StringValue(fmt.Sprintf("city-%d", i%shareCities))); err != nil {
			tb.Fatalf("SetNodeProperty city %s: %v", key, err)
		}
	}
	for i := 0; i < shareUsers; i++ {
		for d := 1; d <= shareDegree; d++ {
			src, dst := fmt.Sprintf("u%d", i), fmt.Sprintf("u%d", (i+d)%shareUsers)
			if err := g.AddEdgeLabeledWithProperty(src, dst, 1, "KNOWS",
				"since", lpg.StringValue("2020-01-01")); err != nil {
				tb.Fatalf("AddEdge %s->%s: %v", src, dst, err)
			}
		}
	}
	// The index is created AFTER the population, not before it. With the order
	// reversed the seek returns ZERO ROWS on this rig: the raw
	// [lpg.Graph.SetNodeProperty] writes above do not reach the index the
	// engine's DDL created, whereas CREATE INDEX over an already-populated
	// graph backfills from it. Measured, not assumed — the order was reversed
	// here and TestFrontEndShareRigValid failed on 0 rows. See the round-1
	// report's out-of-scope observations.
	if _, err := eng.RunInTx(context.Background(),
		"CREATE INDEX user_id FOR (u:USER) ON (u.id)", nil); err != nil {
		tb.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// shareQuery is one measured statement. wantRows is the oracle: a query that
// silently stopped matching would make every share in this file describe a
// query nobody runs.
type shareQuery struct {
	name     string
	corpus   string // the corpus id it came from, or "derived from ..."
	text     string
	params   map[string]expr.Value
	wantRows int
}

// shareQueries spans the execution-cost range on one rig. Three come from the
// corpus verbatim; the fourth is derived, because no 22_cypher read statement
// carries a hoistable string literal and the strip's end-to-end weight is
// exactly what stage 1 flagged as the campaign's largest opportunity.
var shareQueries = []shareQuery{
	{
		name:     "q06_indexed_point_lookup",
		corpus:   "22_cypher/06",
		text:     "MATCH (u:USER {id:$id}) RETURN u.age AS c",
		params:   map[string]expr.Value{"id": expr.StringValue(shareUserID(4242))},
		wantRows: 1,
	},
	{
		name:     "q02_scan_filter_count",
		corpus:   "22_cypher/02",
		text:     "MATCH (u:USER) WHERE u.age > $min RETURN count(u) AS c",
		params:   map[string]expr.Value{"min": expr.IntegerValue(40)},
		wantRows: 1,
	},
	{
		name:     "q16_scan_sort_limit",
		corpus:   "22_cypher/16",
		text:     "MATCH (u:USER) RETURN u.name AS name, u.age AS age ORDER BY age DESC, name ASC LIMIT 5",
		params:   nil,
		wantRows: 5,
	},
	{
		name:   "qLit_scan_strliteral",
		corpus: "derived from 22_cypher/02 over the example's city property",
		text:   "MATCH (u:USER) WHERE u.city = 'city-3' RETURN count(u) AS c",
		params: nil,
		// A count always returns one row; the row's VALUE is what proves the
		// literal matched, and TestFrontEndShareRigValid checks it.
		wantRows: 1,
	},
}

// runShareQuery executes q end to end through [Engine.Run] and returns the row
// count, so every timed iteration carries its own oracle: a query that silently
// stopped matching cannot be reported as a latency.
func runShareQuery(e *Engine, ctx context.Context, q shareQuery) (int, error) {
	res, err := e.Run(ctx, q.text, q.params)
	if err != nil {
		return 0, err
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		return rows, err
	}
	return rows, res.Close()
}

// Sinks, so the compiler cannot delete the front-end arm's work.
var (
	shareSinkParams map[string]expr.Value
	shareSinkErr    error
)

// BenchmarkFrontEndShare reports the front end and the whole query for every
// share query under both cache states, plus the cache-clear control the miss
// arms must be corrected by.
func BenchmarkFrontEndShare(b *testing.B) {
	ctx := context.Background()
	for _, q := range shareQueries {
		b.Run(q.name, func(b *testing.B) {
			for _, cache := range []string{"hit", "miss"} {
				b.Run(cache, func(b *testing.B) {
					b.Run("1parse", func(b *testing.B) {
						eng := newShareRig(b)
						if rows, err := runShareQuery(eng, ctx, q); err != nil || rows != q.wantRows {
							b.Fatalf("warm %s: rows=%d err=%v", q.name, rows, err)
						}
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if cache == "miss" {
								eng.cache.clear()
							}
							// Verbatim the first two statements of runRead.
							entry, auto, err := eng.parseAndAnalyse(q.text)
							if err != nil {
								b.Fatalf("%s: %v", q.name, err)
							}
							shareSinkParams = mergeAutoParams(q.params, auto)
							if entry == nil {
								b.Fatalf("%s: nil entry", q.name)
							}
						}
					})
					b.Run("2full", func(b *testing.B) {
						eng := newShareRig(b)
						if rows, err := runShareQuery(eng, ctx, q); err != nil || rows != q.wantRows {
							b.Fatalf("warm %s: rows=%d err=%v", q.name, rows, err)
						}
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if cache == "miss" {
								eng.cache.clear()
							}
							rows, err := runShareQuery(eng, ctx, q)
							if err != nil {
								b.Fatalf("%s: %v", q.name, err)
							}
							if rows != q.wantRows {
								b.Fatalf("%s returned %d rows, want %d", q.name, rows, q.wantRows)
							}
						}
					})
				})
			}
		})
	}
	// The control. Whatever this costs is charged to BOTH miss arms and to
	// neither hit arm, so the corrected miss share subtracts it from each.
	b.Run("control/cacheclear", func(b *testing.B) {
		eng := newShareRig(b)
		q := shareQueries[0]
		if rows, err := runShareQuery(eng, ctx, q); err != nil || rows != q.wantRows {
			b.Fatalf("warm: rows=%d err=%v", rows, err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			eng.cache.clear()
		}
	})
}

// TestFrontEndShareRigValid guards every premise the share numbers rest on.
// Without it the benchmark would still produce plausible ratios while measuring
// a query that matches nothing, or a "miss" arm that quietly hits.
func TestFrontEndShareRigValid(t *testing.T) {
	eng := newShareRig(t)
	ctx := context.Background()

	for _, q := range shareQueries {
		res, err := eng.Run(ctx, q.text, q.params)
		if err != nil {
			t.Fatalf("%s (%s): %v", q.name, q.corpus, err)
		}
		rows := 0
		var lastCount any
		for res.Next() {
			rows++
			if v, ok := res.Record()["c"]; ok {
				lastCount = v
			}
		}
		if err := res.Err(); err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("%s close: %v", q.name, err)
		}
		if rows != q.wantRows {
			t.Errorf("%s returned %d rows, want %d: the benchmark would describe a query "+
				"nobody runs", q.name, rows, q.wantRows)
		}
		// A count of zero is a query that matched nothing dressed up as one row.
		if lastCount != nil {
			iv, ok := lastCount.(expr.IntegerValue)
			if !ok {
				t.Errorf("%s: count column is %T, not an expr.IntegerValue", q.name, lastCount)
			} else if iv == 0 {
				t.Errorf("%s: count is 0 — the query matched no rows, so its execution cost "+
					"is not the cost of a query that returns data", q.name)
			}
		}
	}

	// The literal-bearing arm must really hoist, or it is not measuring the
	// strip's end-to-end weight at all.
	litFound := false
	for _, q := range shareQueries {
		if q.name != "qLit_scan_strliteral" {
			continue
		}
		litFound = true
		if _, hoisted, ok := parser.StripLiterals(q.text); !ok || len(hoisted) == 0 {
			t.Errorf("%s: StripLiterals hoists nothing from it, so the arm does not "+
				"carry the warm-path cost it exists to carry", q.name)
		}
	}
	if !litFound {
		t.Error("the literal-bearing share query is gone; the warm-path arm is unmeasured")
	}

	// The miss arm must really miss, and the front-end arm must reproduce what
	// runRead does with the auto-parameters — the exact point on which
	// runReadPrefix diverges.
	for _, q := range shareQueries {
		eng.cache.clear()
		if n := eng.cache.Len(); n != 0 {
			t.Fatalf("cache holds %d entries after clear: the miss arm would be a hit arm", n)
		}
		entry, auto, err := eng.parseAndAnalyse(q.text)
		if err != nil {
			t.Fatalf("%s: post-clear parse: %v", q.name, err)
		}
		merged := mergeAutoParams(q.params, auto)
		if err := checkParamPresence(entry.paramRefs, merged); err != nil {
			t.Errorf("%s: the front-end arm leaves a parameter unbound (%v); it is not "+
				"reproducing what runRead does", q.name, err)
		}
		if n := eng.cache.Len(); n != 1 {
			t.Fatalf("%s: cache holds %d entries after a post-clear parse, want 1: the "+
				"miss arm did not compile", q.name, n)
		}
	}
}
