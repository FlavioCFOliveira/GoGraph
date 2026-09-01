package cypher

// top_fusion_test.go — rmp #2509
//
// The gates for fusing ORDER BY … SKIP s LIMIT k into Skip(s) over Top(s+k).
//
// Every assertion here is a CONTRACT, not an observation. The instrument this
// file replaces (bench/audit352 TestPaginationPlans) rendered each pagination
// plan with t.Logf and asserted nothing, so it could not fail whatever the
// planner did; the plans below are asserted, and the RESULT parity beside them
// is asserted against the unfused spelling of the same page rather than against
// a hand-written expectation.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// orderingOpsOf extracts, in plan order, the ordering/pagination operator names
// a rendered physical plan contains. It reads the same rendering grammar
// internal/sim's orderingPlanOps does — indented "Name" or "Name [detail]"
// behind tree glyphs — so the two cannot disagree about what a plan says.
func orderingOpsOf(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, " │├└─\t")
		name, _, _ := strings.Cut(trimmed, " ")
		switch name {
		case "Sort", "Top", "Limit", "Skip":
			out = append(out, name)
		}
	}
	return out
}

func explainOpsParams(t *testing.T, eng *Engine, q string, params map[string]expr.Value) []string {
	t.Helper()
	plan, err := eng.Explain(q, params)
	if err != nil {
		t.Fatalf("Explain(%q): %v", q, err)
	}
	return orderingOpsOf(plan)
}

// TestPaginationPlansAreFused is the asserting replacement for the audit's
// log-only pagination plan probe.
//
// The two gaps #2509 closes are named individually, because they have different
// causes and a fix for one would not fix the other: a PARAMETERISED bound used
// to lose the fusion its identical literal received, and ANY SKIP used to block
// the fusion even when both clauses were literals.
func TestPaginationPlansAreFused(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 40, 7)

	five := map[string]expr.Value{"m": expr.IntegerValue(5)}
	pageParams := map[string]expr.Value{"k": expr.IntegerValue(2), "m": expr.IntegerValue(5)}

	cases := []struct {
		name   string
		query  string
		params map[string]expr.Value
		want   []string
	}{
		{
			name:  "unlimited ORDER BY stays an unbounded Sort",
			query: `MATCH (n:P) RETURN n.name AS name ORDER BY n.age DESC, name ASC`,
			want:  []string{"Sort"},
		},
		{
			name:  "ORDER BY with a bare SKIP has no bound to fuse",
			query: `MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP 2`,
			want:  []string{"Skip", "Sort"},
		},
		{
			name:  "literal LIMIT fuses (unchanged)",
			query: `MATCH (n:P) RETURN n.name AS name ORDER BY n.age DESC, name ASC LIMIT 5`,
			want:  []string{"Top"},
		},
		{
			name:   "PARAMETERISED LIMIT fuses — gap 1",
			query:  `MATCH (n:P) RETURN n.name AS name ORDER BY n.age DESC, name ASC LIMIT $m`,
			params: five,
			want:   []string{"Top"},
		},
		{
			name:  "literal SKIP + literal LIMIT fuse to Skip over Top — gap 2",
			query: `MATCH (n:P) RETURN n.name AS name ORDER BY n.age DESC, name ASC SKIP 2 LIMIT 5`,
			want:  []string{"Skip", "Top"},
		},
		{
			name:   "parameterised SKIP + LIMIT fuse to Skip over Top",
			query:  `MATCH (n:P) RETURN n.name AS name ORDER BY n.age DESC, name ASC SKIP $k LIMIT $m`,
			params: pageParams,
			want:   []string{"Skip", "Top"},
		},
		{
			name:  "SKIP 0 no longer defeats the fusion",
			query: `MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP 0 LIMIT 5`,
			want:  []string{"Skip", "Top"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := explainOpsParams(t, eng, tc.query, tc.params)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ordering operators = %v, want %v\nquery: %s", got, tc.want, tc.query)
			}
		})
	}
}

// TestFusionRefusedWhenUnsound pins the two shapes that must NOT fuse. Both are
// soundness refusals, so each is stated with the reason it exists rather than as
// a bare expectation.
func TestFusionRefusedWhenUnsound(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 20, 5)

	t.Run("non-deterministic SKIP", func(t *testing.T) {
		t.Parallel()
		// The fused shape evaluates the offset TWICE — once for the Top bound and
		// once for the Skip above it. A SKIP that draws a fresh random number each
		// time would skip a different count than it reserved.
		q := `MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP toInteger(rand()*3) LIMIT 5`
		got := explainOpsParams(t, eng, q, nil)
		if strings.Join(got, ",") != "Limit,Skip,Sort" {
			t.Errorf("ordering operators = %v, want [Limit Skip Sort]: a non-deterministic "+
				"SKIP must not reach the fused plan", got)
		}
	})

	t.Run("non-deterministic LIMIT", func(t *testing.T) {
		t.Parallel()
		q := `MATCH (n:P) RETURN n.name AS name ORDER BY n.age LIMIT toInteger(rand()*3)`
		got := explainOpsParams(t, eng, q, nil)
		for _, op := range got {
			if op == "Top" {
				t.Errorf("ordering operators = %v: a non-deterministic LIMIT must not fuse", got)
			}
		}
	})

	t.Run("literal s+k overflows int64", func(t *testing.T) {
		t.Parallel()
		// Saturating would silently drop the LIMIT and Neo4j's Math.addExact
		// would throw, but this engine's pinned contract says a SKIP past the end
		// of the input returns zero rows and no error. Refusing to fuse leaves
		// Sort → Skip → Limit, which does exactly that.
		q := `MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP 9223372036854775800 LIMIT 100`
		got := explainOpsParams(t, eng, q, nil)
		if strings.Join(got, ",") != "Limit,Skip,Sort" {
			t.Fatalf("ordering operators = %v, want [Limit Skip Sort] on s+k overflow", got)
		}
		rows := collectNames(t, eng, q, nil)
		if len(rows) != 0 {
			t.Errorf("SKIP past the end returned %d rows, want 0 and no error", len(rows))
		}
	})
}

// TestFusedPageEqualsUnfusedPage is the RESULT half. The plan assertions above
// prove the shape changed; this proves the answer did not.
//
// The oracle is the same page computed through a plan the fusion cannot reach —
// an unbounded ORDER BY drained in full and sliced in the test — so it is not a
// second run of the code under test. The fixture ties heavily on the sort key
// (40 rows over 7 ages) and the pages are chosen to start and end INSIDE a tie
// run, which is the only place a fused Top could differ from Sort → Skip → Limit
// without differing in row count.
func TestFusedPageEqualsUnfusedPage(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 40, 7)

	full := collectNames(t, eng, `MATCH (n:P) RETURN n.name AS name ORDER BY n.age`, nil)
	if len(full) != 40 {
		t.Fatalf("oracle returned %d rows, want 40", len(full))
	}

	for _, page := range []struct{ skip, limit int }{
		{0, 1}, {0, 7}, {1, 1}, {3, 5}, {6, 9}, {12, 20}, {35, 10}, {40, 5}, {41, 5},
	} {
		page := page
		t.Run(fmt.Sprintf("skip=%d/limit=%d", page.skip, page.limit), func(t *testing.T) {
			t.Parallel()
			want := full
			if page.skip < len(want) {
				want = want[page.skip:]
			} else {
				want = nil
			}
			if page.limit < len(want) {
				want = want[:page.limit]
			}

			lit := collectNames(t, eng, fmt.Sprintf(
				`MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP %d LIMIT %d`,
				page.skip, page.limit), nil)
			par := collectNames(t, eng,
				`MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP $k LIMIT $m`,
				map[string]expr.Value{
					"k": expr.IntegerValue(int64(page.skip)),
					"m": expr.IntegerValue(int64(page.limit)),
				})
			assertSameNames(t, "literal page", lit, want)
			assertSameNames(t, "parameterised page", par, want)
		})
	}
}

// TestFusedTopRejectsHostileSkip is the SECURITY gate.
//
// Lifting the literal-only restriction is what makes it necessary: with the
// bound always a small literal, exec.Top could not be asked to retain more than
// the query text said. With a parameter it can, and the operator had neither a
// row cap nor a byte budget — so a $skip in the billions was an out-of-memory
// kill rather than a typed error. Both dimensions are asserted, because either
// one alone leaves a hole: a byte budget does not bound a stream of tiny rows,
// and a row cap does not bound a handful of enormous ones.
func TestFusedTopRejectsHostileSkip(t *testing.T) {
	t.Parallel()

	t.Run("byte budget", func(t *testing.T) {
		t.Parallel()
		// A tiny MaxResultBytes with a huge $skip: the fused Top must stop
		// buffering with the same sentinel Sort uses, not grow without bound.
		eng := seedPeopleWithOptions(t, 400, 7, &EngineOptions{MaxResultBytes: 512})
		q := `MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP $k LIMIT 1`
		params := map[string]expr.Value{"k": expr.IntegerValue(4_000_000_000)}

		// Non-vacuity first: the query must actually reach the fused operator,
		// or the budget below would be enforced by a plan this task never
		// changed and the test would prove nothing about Top.
		if ops := explainOpsParams(t, eng, q, params); strings.Join(ops, ",") != "Skip,Top" {
			t.Fatalf("ordering operators = %v, want [Skip Top]: the hostile bound did not "+
				"reach the fused operator, so this test does not exercise it", ops)
		}

		res, err := eng.Run(context.Background(), q, params)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		rows := 0
		for res.Next() {
			rows++
		}
		got := res.Err()
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		if !errors.Is(got, exec.ErrSortMemoryExceeded) {
			t.Fatalf("drained %d rows, Result.Err() = %v, want exec.ErrSortMemoryExceeded — "+
				"the fused Top is not bounded by MaxResultBytes, so a hostile $skip is an OOM",
				rows, got)
		}
	})

	t.Run("row cap", func(t *testing.T) {
		t.Parallel()
		// The row cap is exercised at the operator, where a test can lower it;
		// the engine's default is ten million and no unit test may buffer that.
		// The bound is deliberately enormous AND above the cap, which is the
		// hostile-$skip shape: n is far larger than maxRows, so the operator can
		// never select down to it and must stop at the cap.
		src := &countingSource{rows: 64}
		op, err := exec.NewTop(src, []exec.SortKey{{ColIdx: 0, Ascending: true}}, 4_000_000_000, 16)
		if err != nil {
			t.Fatalf("NewTop: %v", err)
		}
		if _, derr := exec.Drain(context.Background(), op); !errors.Is(derr, exec.ErrSortMemoryExceeded) {
			t.Fatalf("Drain = %v, want exec.ErrSortMemoryExceeded at maxRows=16", derr)
		}
	})

	t.Run("a bound below the cap never trips it", func(t *testing.T) {
		t.Parallel()
		// The counterpart the cap must NOT break: a modest bound over a large
		// input retains only the bound, so the same cap that fires above must
		// stay silent here. Without this the row cap could be "enforced" by
		// erroring on every query.
		src := &countingSource{rows: 10_000}
		op, err := exec.NewTop(src, []exec.SortKey{{ColIdx: 0, Ascending: true}}, 8, 16)
		if err != nil {
			t.Fatalf("NewTop: %v", err)
		}
		rows, derr := exec.Drain(context.Background(), op)
		if derr != nil {
			t.Fatalf("Drain: %v", derr)
		}
		if len(rows) != 8 {
			t.Fatalf("got %d rows, want 8", len(rows))
		}
	})
}

// TestFusedTopStillFiresWrites is the openCypher §3.6.2 gate: the write side
// effects of a query must all occur regardless of how many rows the projection
// finally returns.
//
// It matters here because the fusion changed WHICH operator sits over the write
// subtree. The unfused shape put an Eager barrier under the Limit precisely so a
// truncated projection could not truncate the writes; the fused shape has no
// Limit at all, and relies instead on Top being a pipeline breaker that drains
// its child in full. That is a different argument for the same guarantee, so it
// needs its own test.
func TestFusedTopStillFiresWrites(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)

	res, err := eng.RunInTx(context.Background(),
		`UNWIND [5,3,8,1,9,2,7] AS x
		 CREATE (n:W {v: x})
		 RETURN n.v AS v ORDER BY n.v SKIP 2 LIMIT 3`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	var page []int64
	for res.Next() {
		v := res.Record()["v"]
		iv, ok := v.(expr.IntegerValue)
		if !ok {
			t.Fatalf("v is %T, want IntegerValue", v)
		}
		page = append(page, int64(iv))
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("drain: %v", rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	// The page is the 3rd, 4th and 5th smallest of the seven created values.
	if len(page) != 3 || page[0] != 3 || page[1] != 5 || page[2] != 7 {
		t.Errorf("page = %v, want [3 5 7]", page)
	}

	// Every UNWIND element must have been created, not just the three returned.
	created := collectNames(t, eng, `MATCH (n:W) RETURN toString(n.v) AS name ORDER BY n.v`, nil)
	if len(created) != 7 {
		t.Fatalf("the fused plan created %d nodes, want 7 — writes below Top were truncated:\n%s",
			len(created), explainOK(t, eng, `MATCH (n:W) RETURN n.v AS v ORDER BY n.v SKIP 2 LIMIT 3`))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// seedPeopleWithOptions is seedPeople with explicit EngineOptions on the engine
// the test finally queries. The seeding engine keeps the defaults, so a lowered
// result budget cannot make the fixture itself fail to build.
func seedPeopleWithOptions(t *testing.T, n, buckets int, opts *EngineOptions) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	seed := NewEngine(g)
	for i := 0; i < n; i++ {
		r, err := seed.RunInTx(context.Background(),
			fmt.Sprintf("CREATE (:P {age: %d, name: 'n%d'})", i%buckets, i), nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if cerr := r.Close(); cerr != nil {
			t.Fatalf("seed close %d: %v", i, cerr)
		}
	}
	return NewEngineWithOptions(g, *opts)
}

// collectNames drains a query that projects a single "name" column.
func collectNames(t *testing.T, eng *Engine, q string, params map[string]expr.Value) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	var out []string
	for res.Next() {
		v, present := res.Record()["name"]
		if !present {
			t.Fatalf("Run(%q): row has no 'name' column", q)
		}
		sv, ok := v.(expr.StringValue)
		if !ok {
			t.Fatalf("Run(%q): name is %T, want StringValue", q, v)
		}
		out = append(out, string(sv))
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("Run(%q) drain: %v", q, rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("Run(%q) close: %v", q, cerr)
	}
	return out
}

func assertSameNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows %v, want %d rows %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d = %q, want %q (got %v, want %v)", what, i, got[i], want[i], got, want)
		}
	}
}

// countingSource emits `rows` single-column rows whose value descends, so every
// arrival is better than the current worst and the operator takes its
// replacement path on every row after the first.
type countingSource struct {
	rows int
	idx  int
}

func (s *countingSource) Init(_ context.Context) error { s.idx = 0; return nil }
func (s *countingSource) Close() error                 { return nil }
func (s *countingSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= s.rows {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(int64(s.rows - s.idx))}
	s.idx++
	return true, nil
}

// TestFusedTopRebindsParametersPerExecution is the plan-cache gate.
//
// Carrying the SKIP and LIMIT expressions on ir.Top rather than resolving them
// at plan time is what lets a PARAMETERISED bound reach the fused operator at
// all, and it only works if the bound is resolved on every execution. If it were
// captured once, the FIRST page a query served would be the only page it could
// ever serve — the same query text, the same cache entry, a different answer
// silently withheld.
//
// The same Engine runs the same query text six times with six different
// (skip, limit) pairs, so every run after the first is served from the plan
// cache, and each is checked against the oracle's slice of the full ordering.
func TestFusedTopRebindsParametersPerExecution(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 40, 7)

	const q = `MATCH (n:P) RETURN n.name AS name ORDER BY n.age SKIP $k LIMIT $m`
	full := collectNames(t, eng, `MATCH (n:P) RETURN n.name AS name ORDER BY n.age`, nil)
	if len(full) != 40 {
		t.Fatalf("oracle returned %d rows, want 40", len(full))
	}

	for _, page := range []struct{ skip, limit int }{
		{0, 3}, {3, 3}, {6, 12}, {0, 40}, {37, 3}, {5, 1},
	} {
		want := full[min(page.skip, len(full)):]
		if page.limit < len(want) {
			want = want[:page.limit]
		}
		got := collectNames(t, eng, q, map[string]expr.Value{
			"k": expr.IntegerValue(int64(page.skip)),
			"m": expr.IntegerValue(int64(page.limit)),
		})
		assertSameNames(t, fmt.Sprintf("cached plan, skip=%d limit=%d", page.skip, page.limit), got, want)
	}
}
