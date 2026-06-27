package cypher_test

// agg_grouping_key_test.go — regression gate for #1803 (sprint 253): a
// non-aliased compound grouping-key expression (e.g. n.a + 1) in a RETURN with
// aggregation rendered as null, because the final projection re-evaluated the
// AST against the post-aggregation row (where the source variable is gone).

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestCompoundGroupingKey_1803(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{`CREATE (:N {a:1})`, `CREATE (:N {a:1})`, `CREATE (:N {a:2})`} {
		r, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		r.Close()
	}
	// Collect "<key>=<count>" pairs, sorted, so row order is irrelevant.
	pairs := func(q, keyCol, cntCol string) []string {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer res.Close()
		var out []string
		for res.Next() {
			rec := res.Record()
			out = append(out, fmt.Sprintf("%v=%v", rec[keyCol], rec[cntCol]))
		}
		sort.Strings(out)
		return out
	}

	got := pairs(`MATCH (n) RETURN n.a + 1, count(*)`, "n.a + 1", "count(*)")
	want := []string{"2=2", "3=1"} // a=1 -> key 2 (count 2); a=2 -> key 3 (count 1)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("RETURN n.a + 1, count(*): got %v, want %v", got, want)
	}

	// Post-WITH compound key.
	got2 := pairs(`MATCH (n) WITH n.a AS a RETURN a + 1, count(*)`, "a + 1", "count(*)")
	if fmt.Sprint(got2) != fmt.Sprint(want) {
		t.Errorf("WITH n.a AS a RETURN a + 1, count(*): got %v, want %v", got2, want)
	}

	// Controls that already worked must stay correct.
	if g := pairs(`MATCH (n) RETURN n.a, count(*)`, "n.a", "count(*)"); fmt.Sprint(g) != fmt.Sprint([]string{"1=2", "2=1"}) {
		t.Errorf("bare property control: got %v", g)
	}
	if g := pairs(`MATCH (n) RETURN n.a + 1 AS k, count(*) AS c`, "k", "c"); fmt.Sprint(g) != fmt.Sprint(want) {
		t.Errorf("aliased control: got %v", g)
	}
}
