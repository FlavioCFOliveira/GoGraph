package cypher_test

// nested_aggregation_test.go — regression gate for #1804 (sprint 253): an
// aggregate nested inside another aggregate (count(count(n))) must raise a
// compile-time error (openCypher TCK Return6 [14]: SyntaxError NestedAggregation)
// rather than silently returning a result.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestNestedAggregation_1804(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	// Seed so the bound-variable forms have rows to (wrongly) aggregate.
	for _, q := range []string{`CREATE (:N)`, `CREATE (:N)`} {
		r, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		r.Close()
	}
	reject := []string{
		`MATCH (n) RETURN count(count(n))`,
		`MATCH (n) RETURN sum(count(n))`,
		`MATCH (n) RETURN collect(count(n))`,
		`RETURN count(count(*))`,
		`MATCH (n) RETURN 1 + count(count(n))`,
		`MATCH (n) WITH count(count(n)) AS c RETURN c`,
		`MATCH (n) RETURN count(n) AS c ORDER BY count(count(n))`,
	}
	for _, q := range reject {
		res, err := eng.Run(context.Background(), q, nil)
		if err == nil {
			res.Close()
			t.Errorf("%s: expected NestedAggregation compile error, got none", q)
			continue
		}
		if !strings.Contains(err.Error(), "NESTED_AGGREGATION") &&
			!strings.Contains(strings.ToLower(err.Error()), "nest") {
			t.Errorf("%s: expected nested-aggregation error, got %v", q, err)
		}
	}
	// Controls: single-level aggregation must still work.
	for _, q := range []string{
		`MATCH (n) RETURN count(n)`,
		`MATCH (n) RETURN count(*) + 1`,
		`MATCH (n) RETURN n.x, count(n)`,
	} {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Errorf("%s: single-level aggregation must be accepted, got %v", q, err)
			continue
		}
		res.Close()
	}
}
