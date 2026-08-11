package cypher

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// These tests pin literal hoisting (rmp #2412): a query that inlines a rotating
// string literal must collapse onto ONE plan-cache entry instead of re-parsing
// and re-planning per distinct value.
//
// They live in package cypher, not cypher_test, because the property asserted is
// the size of the unexported plan cache. Every test asserts BOTH halves —
// the cache size and the rows — because each alone is satisfied by a broken
// implementation: asserting only rows passes without the optimisation at all,
// and asserting only the cache size passes on an engine that shares a plan and
// answers every query with the first literal's row.

func hoistTestEngine(t *testing.T) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		q := fmt.Sprintf(`CREATE (:Person {sid: 'p%d', name: 'name-%d'})`, i, i)
		res, err := eng.RunInTx(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		_ = res.Close()
	}
	return eng
}

func hoistQueryOne(t *testing.T, eng *Engine, query string) string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("run %q: %v", query, err)
	}
	defer func() { _ = res.Close() }()
	cols := res.Columns()
	if len(cols) != 1 {
		t.Fatalf("run %q: got %d columns, want 1", query, len(cols))
	}
	got, n := "", 0
	for res.Next() {
		got = fmt.Sprint(res.Record()[cols[0]])
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", query, err)
	}
	if n != 1 {
		t.Fatalf("run %q: got %d rows, want 1", query, n)
	}
	return got
}

// TestLiteralHoisting_RotatingLiteralSharesOnePlan is the defect this exists to
// fix: eight distinct literals must not produce eight plans.
func TestLiteralHoisting_RotatingLiteralSharesOnePlan(t *testing.T) {
	for _, shape := range []struct {
		name  string
		query func(i int) string
	}{
		{"WHERE equality", func(i int) string {
			return fmt.Sprintf(`MATCH (p:Person) WHERE p.sid = 'p%d' RETURN p.name AS name`, i)
		}},
		{"pattern property map", func(i int) string {
			return fmt.Sprintf(`MATCH (p:Person {sid: 'p%d'}) RETURN p.name AS name`, i)
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			eng := hoistTestEngine(t)
			eng.cache.clear()

			const rounds = 8
			for i := 0; i < rounds; i++ {
				got := hoistQueryOne(t, eng, shape.query(i))
				want := expr.StringValue(fmt.Sprintf("name-%d", i)).String()
				if got != want {
					t.Fatalf("literal %d: got %s, want %s — a shared plan is answering with the wrong literal",
						i, got, want)
				}
			}
			if n := eng.cache.Len(); n != 1 {
				t.Errorf("plan cache holds %d entries after %d distinct literals, want 1", n, rounds)
			}
		})
	}
}

// TestLiteralHoisting_ProjectionLiteralIsNotHoisted pins the column name an
// unaliased projection takes from its own source text. Hoisting there would
// rename the column after the parameter and let two queries that must differ
// share a header.
func TestLiteralHoisting_ProjectionLiteralIsNotHoisted(t *testing.T) {
	eng := hoistTestEngine(t)
	for _, lit := range []string{"a", "b"} {
		q := fmt.Sprintf(`RETURN '%s'`, lit)
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("run %q: %v", q, err)
		}
		cols := res.Columns()
		if len(cols) != 1 || cols[0] != fmt.Sprintf("'%s'", lit) {
			t.Errorf("run %q: columns = %v, want [%q]", q, cols, "'"+lit+"'")
		}
		for res.Next() {
		}
		_ = res.Close()
	}
}

// TestLiteralHoisting_AgreesWithTheParameterisedForm is the differential: each
// query is run as written and again with the value supplied as a real
// parameter, and the two must agree. A hoisting bug that bound the wrong value,
// or bound it under the wrong name, surfaces here as a disagreement.
func TestLiteralHoisting_AgreesWithTheParameterisedForm(t *testing.T) {
	eng := hoistTestEngine(t)
	ctx := context.Background()

	for _, tc := range []struct {
		literal, param, bind string
	}{
		{`MATCH (p:Person {sid: 'p3'}) RETURN p.name AS name`,
			`MATCH (p:Person {sid: $k}) RETURN p.name AS name`, "p3"},
		{`MATCH (p:Person) WHERE p.sid = 'p5' RETURN p.name AS name`,
			`MATCH (p:Person) WHERE p.sid = $k RETURN p.name AS name`, "p5"},
		{`MATCH (p:Person) WHERE p.sid IN ['p1', 'p2'] RETURN count(p) AS n`,
			`MATCH (p:Person) WHERE p.sid IN [$k, 'p2'] RETURN count(p) AS n`, "p1"},
	} {
		gotLit := hoistQueryOne(t, eng, tc.literal)

		r2, err := eng.Run(ctx, tc.param, map[string]expr.Value{"k": expr.StringValue(tc.bind)})
		if err != nil {
			t.Fatalf("param form %q: %v", tc.param, err)
		}
		cols := r2.Columns()
		gotParam, n := "", 0
		for r2.Next() {
			gotParam = fmt.Sprint(r2.Record()[cols[0]])
			n++
		}
		if err := r2.Err(); err != nil {
			t.Fatalf("param form %q: %v", tc.param, err)
		}
		_ = r2.Close()
		if n != 1 {
			t.Fatalf("param form %q: got %d rows, want 1", tc.param, n)
		}
		if gotLit != gotParam {
			t.Errorf("literal and parameter forms disagree:\n  %s -> %s\n  %s -> %s",
				tc.literal, gotLit, tc.param, gotParam)
		}
	}
}

// The auto-parameter exemption in checkParamTypesCached is pinned by
// TestIndexSeekParam_LiteralTypeMismatchFallsBackToZeroRows, not here.
//
// That is deliberate. A version of the test was written in this file and turned
// out NOT to be an oracle: it created its index with a plain CREATE INDEX, which
// does not type the property as Integer, so it passed with the exemption removed
// and would have looked like protection while providing none. The existing test
// installs an int64 index, which is what makes the property Integer-typed and
// the mismatch reachable; with the exemption removed it fails with
// "parameter $  auto_0: expected Integer value, got String", verified by
// injection.
