package cypher_test

// Regression gate for #1788 (sprint 250): a '*' adjacent to an integer literal
// inside a list literal or list/pattern comprehension was being rewritten by
// the normalizeVarlenBounds parser pre-pass into a negative literal (intended
// only for variable-length relationship range bounds [r*N..M]), silently
// negating the multiplication result. The fix scopes that rewrite to
// relationship-detail brackets (those preceded by '-'). These tests pin the
// correct arithmetic and guard that the variable-length-path path it serves
// still works.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runScalar executes q on a fresh empty graph and returns fmt.Sprint of the
// single named column in the first row.
func runScalarCol(t *testing.T, q, col string) string {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("query %q error: %v", q, err)
	}
	defer res.Close()
	if !res.Next() {
		if err := res.Err(); err != nil {
			t.Fatalf("query %q result error: %v", q, err)
		}
		t.Fatalf("query %q returned no rows", q)
	}
	v := res.Record()[col]
	return fmt.Sprint(v)
}

func TestListLiteralMultiplication_1788(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		// The bug: these previously returned negated products.
		{`RETURN [2*10] AS r`, "[20]"},
		{`RETURN [x IN [1,2,3] | x*10] AS r`, "[10, 20, 30]"},
		{`RETURN [10, 2*5, 100] AS r`, "[10, 10, 100]"},
		{`WITH 3 AS a RETURN [a*4] AS r`, "[12]"},
		{`RETURN [2*3*4] AS r`, "[24]"},
		// Controls that were already correct — must stay correct.
		{`RETURN 2*10 AS r`, "20"},
		{`RETURN [2 * 10] AS r`, "[20]"},
		// Nested list of products.
		{`RETURN [x IN [1,2] | [x*2, x*3]] AS r`, "[[2, 3], [4, 6]]"},
	}
	for _, c := range cases {
		got := runScalarCol(t, c.query, "r")
		if got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.query, got, c.want)
		}
	}
}

// TestVarlenBoundsStillWork_1788 guards that the relationship-range path the
// normalizer was built to serve still parses and bound-checks after the fix.
func TestVarlenBoundsStillWork_1788(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	// Build a 4-node directed chain a->b->c->d.
	if res, err := eng.RunInTx(ctx, `CREATE (a:N {k:'a'})-[:R]->(b:N {k:'b'})-[:R]->(c:N {k:'c'})-[:R]->(d:N {k:'d'})`, nil); err != nil {
		t.Fatalf("setup CREATE error: %v", err)
	} else {
		res.Close()
	}
	// Each of these varlen forms must parse and execute without error.
	for _, q := range []string{
		`MATCH (a {k:'a'})-[r*2..3]->(x) RETURN count(*) AS r`,
		`MATCH (a {k:'a'})-[r*2..]->(x) RETURN count(*) AS r`,
		`MATCH (a {k:'a'})-[r*..3]->(x) RETURN count(*) AS r`,
		`MATCH (a {k:'a'})-[r*1..1]->(x) RETURN count(*) AS r`,
	} {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("varlen query %q error: %v", q, err)
		}
		res.Close()
	}
	// Spot-check a concrete count: a-[*1..1]->(x) is exactly the single direct
	// neighbour b. This pins that the bound rewrite still yields correct hops.
	if got := runVarlenCount(t, eng, ctx, `MATCH (a {k:'a'})-[*1..1]->(x) RETURN count(*) AS r`); got != "1" {
		t.Errorf("varlen 1..1 from a: got %s want 1", got)
	}
}

func runVarlenCount(t *testing.T, eng *cypher.Engine, ctx context.Context, q string) string {
	t.Helper()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("query %q error: %v", q, err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatalf("query %q no rows", q)
	}
	return fmt.Sprint(res.Record()["r"])
}
