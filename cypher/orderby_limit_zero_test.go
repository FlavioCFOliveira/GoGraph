package cypher_test

// orderby_limit_zero_test.go — regression gate for #1801 (sprint 253):
// ORDER BY ... LIMIT 0 used to return ALL rows because the Sort+Limit->Top
// fusion treated limit 0 as "no limit" and returned the child unchanged.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func countRows1801(t *testing.T, eng *cypher.Engine, q string) int {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer res.Close()
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("query %q result: %v", q, err)
	}
	return n
}

func TestOrderByLimitZero_1801(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{`CREATE (:N {a:1})`, `CREATE (:N {a:2})`, `CREATE (:N {a:3})`} {
		r, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		r.Close()
	}
	cases := []struct {
		q    string
		want int
	}{
		{`MATCH (n) RETURN n.a AS a ORDER BY n.a LIMIT 0`, 0},        // the bug
		{`UNWIND [1,2,3] AS x RETURN x ORDER BY x LIMIT 0`, 0},       // the bug (no graph)
		{`MATCH (n) RETURN n.a AS a ORDER BY n.a LIMIT 2`, 2},        // positive limit unchanged
		{`MATCH (n) RETURN n.a AS a ORDER BY n.a`, 3},                // no limit unchanged
		{`MATCH (n) RETURN n.a AS a LIMIT 0`, 0},                     // LIMIT 0 without ORDER BY
		{`MATCH (n) RETURN n.a AS a ORDER BY n.a SKIP 0 LIMIT 0`, 0}, // SKIP+LIMIT 0
	}
	for _, c := range cases {
		if got := countRows1801(t, eng, c.q); got != c.want {
			t.Errorf("%s\n  got %d rows, want %d", c.q, got, c.want)
		}
	}
}
