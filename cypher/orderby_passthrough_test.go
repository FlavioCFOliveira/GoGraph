package cypher_test

// orderby_passthrough_test.go — regression gate for #1805 (sprint 253): a
// variable added only to satisfy an ORDER BY reference (a hidden passthrough)
// leaked into the result columns, e.g. RETURN n.a ORDER BY n.a yielded columns
// [n.a, n] instead of [n.a].

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestOrderByPassthroughColumns_1805(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{`CREATE (:N {a:2})`, `CREATE (:N {a:1})`} {
		r, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		r.Close()
	}
	cols := func(q string) []string {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer res.Close()
		return append([]string(nil), res.Columns()...)
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		q    string
		want []string
	}{
		{`MATCH (n) RETURN n.a ORDER BY n.a`, []string{"n.a"}},    // the bug
		{`MATCH (n) RETURN n.a AS x ORDER BY n.a`, []string{"x"}}, // the bug (aliased)
		{`MATCH (n) RETURN n.a AS a ORDER BY a`, []string{"a"}},   // control: order by alias
		{`MATCH (n) RETURN n ORDER BY n.a`, []string{"n"}},        // control: n is a real column
	}
	for _, c := range cases {
		if got := cols(c.q); !eq(got, c.want) {
			t.Errorf("%s\n  columns %v, want %v", c.q, got, c.want)
		}
	}
}
