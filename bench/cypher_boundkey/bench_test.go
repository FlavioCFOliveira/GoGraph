// Package cypher_boundkey_test benchmarks the access path a query gets when its
// index key is BOUND rather than written inline, and the cost gate that decides
// when seeking such a key stops paying.
//
// It is the regression gate for tasks #2182 and #2183, and the measurement task
// #2184 asked for. The round-3 comparative audit attributed its headline load
// deficit — 35 m 33 s to load 20 000 nodes and 200 000 edges, against Memgraph's
// 977 ms and Neo4j's 2.39 s — to a bound key never reaching the index. The #2181
// spike confirmed the gap (3 038× at a label population of 20 000) and corrected
// two premises by measurement. This benchmark holds the result.
//
// # Two things this file gets right on purpose
//
// **Entity projection.** Every case returns the node (`RETURN a`), never a scalar
// (`RETURN a.id`). The spike measured the inline-literal seek at 204/415/895 µs
// across N = 5k/10k/20k on a scalar projection — growing linearly despite the plan
// showing NodeByIndexSeek — because the columnar scan-and-filter path claims a
// scalar-projecting Selection and never consults the seek (#2204). A
// scalar-projection benchmark would measure #2204 twice and this work not at all.
//
// **Key count, not only N.** The gain is N/rows per query, so a single-key
// benchmark reports ~1 400× and a 300-key benchmark ~20×. Reporting one without the
// other would misrepresent the change, so BenchmarkBoundKeySet sweeps the key count
// and includes a case past the cost gate, which must decline to a scan rather than
// regress.
//
// Run with:
//
//	go test -bench=. -benchmem -count=6 ./bench/cypher_boundkey/...
package cypher_boundkey_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The label populations the spike measured, so the figures are comparable.
var populations = []int{5000, 10000, 20000}

// newEngine builds a :P label of n nodes with a unique string name and a hash
// index over it, so one key's posting list holds exactly one node.
//
// The graph is a MULTIGRAPH because the engine warns on a simple graph, and the
// warning interleaves with benchmark output.
func newEngine(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		fmt.Sprintf(`UNWIND range(1, %d) AS i CREATE (:P {id: i, name: 'name-' + toString(i)})`, n),
		`CREATE INDEX p_name FOR (n:P) ON (n.name)`,
	} {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			tb.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			tb.Fatalf("seed %q: %v", q, err)
		}
		if err := res.Close(); err != nil {
			tb.Fatalf("seed close: %v", err)
		}
	}
	return eng
}

// drain runs q to completion, which is what the benchmark times.
func drain(tb testing.TB, eng *cypher.Engine, q string, params map[string]any) {
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		tb.Fatalf("run: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("run: %v", err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("close: %v", err)
	}
}

// keyList renders a Cypher list literal of n distinct existing keys.
func keyList(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "'name-%d'", i)
	}
	b.WriteByte(']')
	return b.String()
}

// path reports which access path q actually gets, so each benchmark name states
// it and a plan regression shows up as a renamed benchmark rather than as a
// silently slower one.
//
// params must be the params the query will actually RUN with. Explain resolves an
// absent parameter to NULL, which declines a seek — so calling this with nil for a
// parameterised query labels a seeking case "scan". That mislabelling is the reason
// this helper takes params at all.
func path(tb testing.TB, eng *cypher.Engine, q string, params map[string]any) string {
	ev, err := cypher.BindParams(params)
	if err != nil {
		tb.Fatalf("BindParams: %v", err)
	}
	plan, err := eng.Explain(q, ev)
	if err != nil {
		tb.Fatalf("Explain: %v", err)
	}
	switch {
	case strings.Contains(plan, "NodeByIndexSeekSet"):
		return "seekset"
	case strings.Contains(plan, "NodeByIndexSeek"):
		return "seek"
	default:
		return "scan"
	}
}

// BenchmarkSingleBoundKey is the #2182 gate: a key bound by WITH must cost what an
// inline literal costs, and both must be FLAT in the label population.
//
// Before #2182 the WITH-bound case was linear in N — 2.72 / 5.54 / 13.37 ms at
// N = 5k/10k/20k — because the equality was left as a residual filter over a full
// label scan. A reappearance of that linearity is what this benchmark exists to
// catch.
func BenchmarkSingleBoundKey(b *testing.B) {
	for _, n := range populations {
		eng := newEngine(b, n)
		params := map[string]any{"p": "name-1"}
		cases := []struct {
			name, query string
			params      map[string]any
		}{
			{name: "inline", query: `MATCH (a:P {name: 'name-1'}) RETURN a`},
			{name: "withbound", query: `WITH 'name-1' AS k MATCH (a:P {name: k}) RETURN a`},
			{name: "withbound_param", query: `WITH $p AS k MATCH (a:P {name: k}) RETURN a`, params: params},
		}
		for _, tc := range cases {
			b.Run(fmt.Sprintf("n=%d/%s/%s", n, tc.name, path(b, eng, tc.query, tc.params)), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					drain(b, eng, tc.query, tc.params)
				}
			})
		}
	}
}

// BenchmarkBoundKeySet is the #2183 gate: it sweeps the key count at a fixed
// population so the N/rows shape of the gain is visible, and it includes a set
// past the cost gate.
//
// The gate's ceiling is 10 % of the label population, so at N = 20 000 a set of
// 2 000 unique keys sits exactly at the budget and 2 001 is past it. The
// past-budget case must appear as "scan" in its name: that is the gate declining,
// and its timing must stay level with the pre-#2183 plan rather than regress. It
// does so because an unclaimed seek hint is dropped at build time, leaving the
// declined plan structurally identical to the plan that existed before the
// rewrite — without that, the same case measured 2 952 ms instead of 19.4 ms.
func BenchmarkBoundKeySet(b *testing.B) {
	const n = 20000
	eng := newEngine(b, n)

	// 1 key exercises the single-key seek; 2 000 sits exactly at the gate's
	// budget; 2 001 is one key past it and must decline.
	for _, keys := range []int{1, 30, 300, 2000, 2001} {
		q := `UNWIND ` + keyList(keys) + ` AS k MATCH (a:P {name: k}) RETURN a`
		b.Run(fmt.Sprintf("n=%d/keys=%d/%s", n, keys, path(b, eng, q, nil)), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				drain(b, eng, q, nil)
			}
		})
	}
}

// BenchmarkRuntimeBoundKey measures the key forms that are NOT served, so the
// boundary is a recorded number rather than a claim.
//
// Both are the shapes a batched bulk load actually writes, and both still scan:
//
//   - `UNWIND $keys AS k` — a runtime list, whose elements cannot be enumerated
//     when the plan is built.
//   - `UNWIND $rows AS r MATCH (a:P {name: r.name})` — the audit's own load query,
//     whose key is a PROPERTY ACCESS on the unwound row rather than a bare bound
//     variable.
//
// The second is why the audit's attribution of its load deficit to this gap does
// not survive measurement: neither #2182 nor #2183 changes that query's plan. See
// docs/benchmarks/bound-key-seek-2026-07-26.md.
func BenchmarkRuntimeBoundKey(b *testing.B) {
	const n = 20000
	eng := newEngine(b, n)

	rows := make([]any, 0, 30)
	keys := make([]any, 0, 30)
	for i := 1; i <= 30; i++ {
		rows = append(rows, map[string]any{"name": fmt.Sprintf("name-%d", i)})
		keys = append(keys, fmt.Sprintf("name-%d", i))
	}
	// A 2 001-key runtime list is the SAME key count as the past-budget literal
	// case in BenchmarkBoundKeySet. Because a runtime list is never served, this is
	// the pre-#2183 plan by construction, so the two timings together are the
	// empirical proof that a declined gate does not regress: they must match.
	wide := make([]any, 0, 2001)
	for i := 1; i <= 2001; i++ {
		wide = append(wide, fmt.Sprintf("name-%d", i))
	}

	cases := []struct {
		name   string
		query  string
		params map[string]any
	}{
		{
			name:   "param_list",
			query:  `UNWIND $keys AS k MATCH (a:P {name: k}) RETURN a`,
			params: map[string]any{"keys": keys},
		},
		{
			name:   "row_property",
			query:  `UNWIND $rows AS r MATCH (a:P {name: r.name}) RETURN a`,
			params: map[string]any{"rows": rows},
		},
		{
			// The AC-(3) reference: 2 001 keys on the never-served runtime path,
			// which is the pre-#2183 plan, to compare against the same key count on
			// the declined-gate path.
			name:   "param_list_2001",
			query:  `UNWIND $keys AS k MATCH (a:P {name: k}) RETURN a`,
			params: map[string]any{"keys": wide},
		},
	}
	for _, tc := range cases {
		b.Run(fmt.Sprintf("n=%d/%s/%s", n, tc.name, path(b, eng, tc.query, tc.params)), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				drain(b, eng, tc.query, tc.params)
			}
		})
	}
}

// TestAccessPathsAreWhatTheBenchmarkNamesClaim guards the benchmark itself.
//
// Every benchmark name above embeds the access path its query gets. If a planner
// change silently moved a case onto a different path, the timings would still be
// reported — under a name that no longer described them, which is worse than a
// failure. This asserts each expected path explicitly, so the benchmark's own
// labelling has a gate.
func TestAccessPathsAreWhatTheBenchmarkNamesClaim(t *testing.T) {
	eng := newEngine(t, 20000)

	rows := []any{map[string]any{"name": "name-1"}}
	keys := []any{"name-1", "name-2"}
	cases := []struct {
		query  string
		want   string
		params map[string]any
	}{
		{query: `MATCH (a:P {name: 'name-1'}) RETURN a`, want: "seek"},
		{query: `WITH 'name-1' AS k MATCH (a:P {name: k}) RETURN a`, want: "seek"},
		{query: `UNWIND ` + keyList(30) + ` AS k MATCH (a:P {name: k}) RETURN a`, want: "seekset"},
		{query: `UNWIND ` + keyList(2000) + ` AS k MATCH (a:P {name: k}) RETURN a`, want: "seekset"},
		// Past the 10 % gate: the set must decline to a scan.
		{query: `UNWIND ` + keyList(2001) + ` AS k MATCH (a:P {name: k}) RETURN a`, want: "scan"},
		// Not served: a runtime list, and a property access on the unwound row.
		{query: `UNWIND $keys AS k MATCH (a:P {name: k}) RETURN a`, want: "scan",
			params: map[string]any{"keys": keys}},
		{query: `UNWIND $rows AS r MATCH (a:P {name: r.name}) RETURN a`, want: "scan",
			params: map[string]any{"rows": rows}},
	}
	for _, tc := range cases {
		if got := path(t, eng, tc.query, tc.params); got != tc.want {
			t.Errorf("path = %q, want %q, for %.80s", got, tc.want, tc.query)
		}
	}
}
