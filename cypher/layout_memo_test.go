package cypher

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// layoutMemoShapes are TCK-style read shapes that cross every memoised site: a
// Filter predicate, a Projection's input snapshot and its general-path snapshot,
// secondary expression keys (Return4 [3]), WITH chains, aggregation, expansion,
// OPTIONAL MATCH, an index seek, and a disjoint-component swap.
var layoutMemoShapes = []struct {
	q       string
	params  map[string]expr.Value
	ordered bool
	swap    bool
}{
	{q: "MATCH (n:Big {k: $p}) RETURN n.k AS k", params: map[string]expr.Value{"p": expr.IntegerValue(7)}},
	{q: "MATCH (n:Big) WHERE n.k > 10 AND n.k < 30 RETURN n.k AS k, n.k * 2 AS d"},
	{q: "MATCH (n:Big) RETURN n.k AS n, n.k ORDER BY n", ordered: true},
	{q: "MATCH (n:Big) WITH n.k AS x WHERE x % 3 = 0 RETURN x, x + 1 AS y, [x, x] AS l ORDER BY x", ordered: true},
	{q: "MATCH (n:Big) RETURN count(n) AS c, sum(n.k) AS s, collect(n.k % 5)[0..1] AS h"},
	{q: "MATCH (a:Big)-[r]->(b) WHERE b.k <> a.k RETURN a.k AS ak, b.k AS bk, toString(a.k) + '-' + toString(b.k) AS s"},
	{q: "MATCH (a:Small) OPTIONAL MATCH (a)-[]->(b) RETURN a.k AS ak, b.k AS bk"},
	{q: "MATCH (n:Med) WITH n, n.k AS k WHERE k > 0 RETURN n.k AS a, k, CASE WHEN k > 3 THEN 'hi' ELSE 'lo' END AS c"},
	{q: "MATCH (a:Big), (b:Small) RETURN a.k AS ak, b.k AS bk", swap: true},
}

func layoutMemoEngine(t *testing.T) *Engine {
	t.Helper()
	g := buildReorderGraph(t, map[string]int{"Big": 80, "Med": 12, "Small": 4})
	for i := 0; i+1 < 96; i += 3 {
		if err := g.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), 1); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(g)
	if _, err := e.RunInTx(context.Background(), "CREATE INDEX FOR (n:Big) ON (n.k)", nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func layoutMemoRows(t *testing.T, e *Engine, q string, params map[string]expr.Value, ordered bool) string {
	t.Helper()
	r, err := e.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var rows []string
	for r.Next() {
		var b strings.Builder
		for i := range r.Columns() {
			fmt.Fprintf(&b, "%v|", r.ValueAt(i))
		}
		rows = append(rows, b.String())
	}
	if err := r.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if !ordered {
		sort.Strings(rows)
	}
	return strings.Join(r.Columns(), ",") + "\n" + strings.Join(rows, "\n")
}

// TestLayoutMemo_CachedMatchesRebuilt pins rmp #3097: executions answered from the
// plan-cache entry's layout memo return exactly what a fresh build returns, the
// memo is consulted on every non-swap shape, and an execution where a swap gate
// fired does not touch it.
func TestLayoutMemo_CachedMatchesRebuilt(t *testing.T) {
	cached := layoutMemoEngine(t)
	for _, tc := range layoutMemoShapes {
		t.Run(tc.q, func(t *testing.T) {
			// A fresh engine's first execution builds every snapshot from scratch.
			want := layoutMemoRows(t, layoutMemoEngine(t), tc.q, tc.params, tc.ordered)
			entry, _, err := cached.parseAndAnalyse(tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if tc.swap {
				labelSrc := &lpgLabelResolver{g: cached.g.ReadAt(nil), eng: cached, ctx: context.Background()}
				swaps, err := computeReorderSwaps(context.Background(), entry.reorderCandidates, labelSrc, tc.params, int64(cached.g.LiveOrderStored()))
				if err != nil || len(swaps) == 0 {
					t.Fatalf("the swap shape did not fire a swap (err=%v): the test would not cover it", err)
				}
			}
			for i := 0; i < 3; i++ {
				if got := layoutMemoRows(t, cached, tc.q, tc.params, tc.ordered); got != want {
					t.Fatalf("execution %d differs from a rebuilt plan:\ngot:\n%s\nwant:\n%s", i, got, want)
				}
			}
			hits, misses := entry.layouts.hits.Load(), entry.layouts.misses.Load()
			if tc.swap {
				if hits != 0 || misses != 0 {
					t.Fatalf("a swap-firing execution consulted the memo: hits=%d misses=%d", hits, misses)
				}
				return
			}
			if hits == 0 {
				t.Fatalf("memo never hit (misses=%d): the cached path is not exercised", misses)
			}
		})
	}
}

// TestLayoutMemo_ContentMismatchGetsFreshSnapshot pins the exactness rule: a
// stored snapshot is returned only for an identical schema.
func TestLayoutMemo_ContentMismatchGetsFreshSnapshot(t *testing.T) {
	var lm layoutMemo
	key := new(int)
	a := lm.rowSchemaAt(&lm.filter, key, map[string]int{"n": 0, "m": 1})
	b := lm.rowSchemaAt(&lm.filter, key, map[string]int{"n": 0, "m": 1})
	if !sameLayout(a.cols, b.cols) || lm.hits.Load() != 1 {
		t.Fatalf("equal schema not answered from the memo: hits=%d", lm.hits.Load())
	}
	c := lm.rowSchemaAt(&lm.filter, key, map[string]int{"n": 1, "m": 0})
	if c.cols["n"] != 1 || c.cols["m"] != 0 {
		t.Fatalf("different schema answered with the stored snapshot: %v", c.cols)
	}
	if b.cols["n"] != 0 {
		t.Fatal("the stored snapshot was mutated")
	}
	if sameLayout(map[string]int{"a": 0}, map[string]int{"a": 0, "b": 0}) || sameLayout(map[string]int{"a": 0}, map[string]int{"b": 0}) {
		t.Fatal("sameLayout accepted different schemas")
	}
}

// TestLayoutMemo_ConcurrentExecutions runs 64 goroutines over the same cached
// plans, so under -race any write to a shared snapshot is reported, and checks
// every answer.
func TestLayoutMemo_ConcurrentExecutions(t *testing.T) {
	e := layoutMemoEngine(t)
	want := make([]string, 0, 3)
	shapes := layoutMemoShapes[:3]
	for _, tc := range shapes {
		want = append(want, layoutMemoRows(t, e, tc.q, tc.params, tc.ordered))
	}
	const workers, iters = 64, 20
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := (w + i) % len(shapes)
				tc := shapes[k]
				r, err := e.Run(context.Background(), tc.q, tc.params)
				if err != nil {
					errs <- err.Error()
					return
				}
				var rows []string
				for r.Next() {
					var b strings.Builder
					for c := range r.Columns() {
						fmt.Fprintf(&b, "%v|", r.ValueAt(c))
					}
					rows = append(rows, b.String())
				}
				if !tc.ordered {
					sort.Strings(rows)
				}
				if got := strings.Join(r.Columns(), ",") + "\n" + strings.Join(rows, "\n"); got != want[k] {
					errs <- fmt.Sprintf("worker %d: %s answered differently", w, tc.q)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	entry, _, err := e.parseAndAnalyse(shapes[0].q)
	if err != nil {
		t.Fatal(err)
	}
	if entry.layouts.hits.Load() == 0 {
		t.Fatal("the concurrent executions never hit the memo")
	}
}
