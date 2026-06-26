package cypher_test

// arith_minus_regression_test.go — regression gates for sprint 251:
//   #1796: compact subtraction on an identifier/key ending in e/E (age-1).
//   #1797: compact subtraction after a closing ) ] } ((5)-1, [5][0]-1, (5)-0x1).
//   #1798: compact subtraction after a hex/oct literal ending in e/E (0x1E-1).
// All three were the parser text pre-pass mis-discriminating a binary '-' as a
// float-exponent sign or a unary-minus radix prefix.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func amScalar(t *testing.T, q string) string {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("query %q error: %v", q, err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatalf("query %q no rows", q)
	}
	return fmt.Sprint(res.Record()["r"])
}

func TestArithMinus_IdentEndingInE_1796(t *testing.T) {
	cases := []struct{ q, want string }{
		{`RETURN [age IN [5,6] | age-1] AS r`, "[4, 5]"},
		{`WITH 3 AS scope RETURN scope-1 AS r`, "2"},
		{`WITH 3 AS node RETURN node-1 AS r`, "2"},
		{`WITH {age: 7} AS m RETURN m.age-1 AS r`, "6"},
		{`WITH 10 AS price RETURN price-5 AS r`, "5"},
		// Control: identifier not ending in e/E (already worked).
		{`WITH 7 AS abc RETURN abc-1 AS r`, "6"},
		// Controls: genuine decimal-float exponents must keep working.
		{`RETURN 1.5e-3 AS r`, "0.0015"},
		{`RETURN 1e-3 AS r`, "0.001"},
		{`RETURN 2E-01 AS r`, "0.2"},
	}
	for _, c := range cases {
		if got := amScalar(t, c.q); got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.q, got, c.want)
		}
	}
}

func TestArithMinus_AfterClosingBracket_1797(t *testing.T) {
	cases := []struct{ q, want string }{
		{`RETURN (5)-1 AS r`, "4"},
		{`RETURN [5][0]-1 AS r`, "4"},
		{`RETURN (5)-0x1 AS r`, "4"},
		{`RETURN (7)-0o2 AS r`, "5"},
		// Control: unary minus after an operator still negates the radix literal.
		{`RETURN 5 + -0x1 AS r`, "4"},
		{`RETURN -0x1F AS r`, "-31"},
	}
	for _, c := range cases {
		if got := amScalar(t, c.q); got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.q, got, c.want)
		}
	}
}

func TestArithMinus_HexEndingInE_1798(t *testing.T) {
	cases := []struct{ q, want string }{
		{`RETURN 0x1E-1 AS r`, "29"},  // 0x1E = 30
		{`RETURN 0xae-1 AS r`, "173"}, // 0xae = 174
		// Control: hex not ending in e/E already worked.
		{`RETURN 0x1F-1 AS r`, "30"},
	}
	for _, c := range cases {
		if got := amScalar(t, c.q); got != c.want {
			t.Errorf("%s\n  got  %s\n  want %s", c.q, got, c.want)
		}
	}
}
