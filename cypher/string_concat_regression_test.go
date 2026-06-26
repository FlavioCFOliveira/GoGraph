package cypher_test

// string_concat_regression_test.go — pinning gate for #1794 (sprint 250):
// string + number concatenation returns null (GoGraph requires toString() for
// mixed concatenation; openCypher leaves implicit coercion underspecified).
// This test pins the decision so the behaviour cannot drift silently.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestStringNumberConcat_ReturnsNull_1794(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	scalar := func(q string) expr.Value {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("query %q error: %v", q, err)
		}
		defer res.Close()
		if !res.Next() {
			t.Fatalf("query %q returned no rows", q)
		}
		v, _ := res.Record()["r"].(expr.Value)
		return v
	}

	for _, q := range []string{
		`RETURN 'a' + 1 AS r`,
		`RETURN 'count: ' + 5 AS r`,
		`RETURN 1 + '2' AS r`,
		`RETURN 1.5 + 'x' AS r`,
	} {
		if v := scalar(q); !expr.IsNull(v) {
			t.Errorf("%s: got %v, want null", q, v)
		}
	}

	// The supported form via toString() must concatenate.
	if v := scalar(`RETURN 'count: ' + toString(5) AS r`); v != expr.StringValue("count: 5") {
		t.Errorf("toString concat: got %v, want \"count: 5\"", v)
	}
}
