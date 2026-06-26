package cypher_test

// numeric_order_regression_test.go — regression gate for #1789 (sprint 250):
// ORDER BY and min()/max() must treat Integer and Float as one Number tier
// (CIP2016-06-14). Before the fix, all floats sorted below all integers, so a
// column mixing the two ordered wrong and min/max returned the wrong extreme.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func runRowsCol(t *testing.T, q, col string) []string {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("query %q error: %v", q, err)
	}
	defer res.Close()
	var out []string
	for res.Next() {
		out = append(out, fmt.Sprint(res.Record()[col]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("query %q result error: %v", q, err)
	}
	return out
}

func TestMixedNumericOrderBy_1789(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{`UNWIND [1, 1.5, 2] AS x RETURN x ORDER BY x`, []string{"1", "1.5", "2"}},
		{`UNWIND [3.0, 2, 1.0] AS x RETURN x ORDER BY x`, []string{"1", "2", "3"}},
		{`UNWIND [2, 1.5, 1, 0.5] AS x RETURN x ORDER BY x`, []string{"0.5", "1", "1.5", "2"}},
		{`UNWIND [2, 1.5, 1, 0.5] AS x RETURN x ORDER BY x DESC`, []string{"2", "1.5", "1", "0.5"}},
		// Single-type ordering must remain correct.
		{`UNWIND [3, 1, 2] AS x RETURN x ORDER BY x`, []string{"1", "2", "3"}},
		{`UNWIND [1.5, 1.3, 999.99] AS x RETURN x ORDER BY x`, []string{"1.3", "1.5", "999.99"}},
	}
	for _, c := range cases {
		got := runRowsCol(t, c.query, "x")
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s\n  got  %v\n  want %v", c.query, got, c.want)
		}
	}
}

func TestMixedNumericMinMax_1789(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{`UNWIND [10.5, 3] AS x RETURN max(x) AS m`, "10.5"},
		{`UNWIND [2, 100.0] AS x RETURN min(x) AS m`, "2"},
		{`UNWIND [5, 5.9] AS x RETURN max(x) AS m`, "5.9"},
		{`UNWIND [5, 5.9] AS x RETURN min(x) AS m`, "5"},
		{`UNWIND [1, 2.5, 0, 3] AS x RETURN max(x) AS m`, "3"},
		{`UNWIND [1, 2.5, 0.1, 3] AS x RETURN min(x) AS m`, "0.1"},
	}
	for _, c := range cases {
		got := runRowsCol(t, c.query, "m")
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s\n  got  %v\n  want %s", c.query, got, c.want)
		}
	}
}
