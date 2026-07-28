package cypher_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestStringOrdering_IsCodePointOrder pins the string collation GoGraph
// implements, so it cannot change silently (rmp #2224).
//
// GoGraph orders strings by Unicode CODE POINT; Neo4j orders by UTF-16 code unit,
// because that is what Java's String.compareTo does. The openCypher 2024.3 grammar
// specifies no collation, so neither is non-conformant and no TCK scenario
// discriminates them — which is exactly why this test exists: without it, nothing
// in the project would notice a change to the rule, and nothing in a query result
// reveals which rule is in force.
//
// The documented behaviour lives in docs/cypher.md ("Declared divergence: string
// ordering is by code point"). The documentation and this test are a pair: if one
// changes, the other must.
func TestStringOrdering_IsCodePointOrder(t *testing.T) {
	t.Parallel()

	// The worked example from the documentation, seeded in an order that is
	// neither the expected output nor its reverse, so a no-op sort cannot pass.
	seed := []string{"ﬁ", "a", "😀", "Z", "é", "e", "z"}

	// Code-point order. The last two are the whole point: U+1F600 (128512) sorts
	// AFTER U+FB01 (64257) because 1F600 > FB01. Neo4j yields the opposite,
	// because U+1F600's leading UTF-16 surrogate is U+D83D (55357), which is below
	// U+FB01.
	want := []string{"Z", "a", "e", "z", "é", "ﬁ", "😀"}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for _, v := range seed {
		res, err := eng.RunInTxAny(ctx, "CREATE (:S {v: $v})", map[string]any{"v": v})
		if err != nil {
			t.Fatalf("seed %q: %v", v, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("seed close: %v", cerr)
		}
	}

	got := orderedStrings(t, eng, "MATCH (n:S) RETURN n.v AS v ORDER BY n.v")
	assertOrder(t, "ORDER BY ascending", got, want)

	// DESC must be the exact reverse, so the rule is one comparator rather than
	// two independent code paths.
	rev := make([]string, len(want))
	for i := range want {
		rev[i] = want[len(want)-1-i]
	}
	gotDesc := orderedStrings(t, eng, "MATCH (n:S) RETURN n.v AS v ORDER BY n.v DESC")
	assertOrder(t, "ORDER BY descending", gotDesc, rev)
}

// TestStringOrdering_SupplementaryPlaneBoundary isolates the ONE condition under
// which the two engines disagree, so a regression is reported as a boundary
// failure rather than as a long list mismatch.
//
// The condition, stated in docs/cypher.md: a supplementary-plane character
// (U+10000 and above) compared against a BMP character in U+E000–U+FFFF. Below
// U+E000 the two rules agree, because no UTF-16 surrogate value (U+D800–U+DBFF)
// can be reached.
func TestStringOrdering_SupplementaryPlaneBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		lo, hi     string // lo must sort BEFORE hi under code-point order
		neo4jAgree bool   // whether Neo4j would give the same relative order
	}{
		{
			name:       "supplementary vs BMP in the divergent range",
			lo:         "ﬁ",          // U+FB01, in E000..FFFF
			hi:         "\U0001F600", // U+1F600, supplementary
			neo4jAgree: false,        // Neo4j puts U+1F600 first (lead surrogate U+D83D)
		},
		{
			name:       "supplementary vs BMP below the surrogate range",
			lo:         "é",          // U+00E9
			hi:         "\U0001F600", // U+1F600
			neo4jAgree: true,         // both engines agree: nothing reaches D800..DBFF
		},
		{
			name:       "two supplementary characters",
			lo:         "\U0001F600", // U+1F600
			hi:         "\U0001F601", // U+1F601
			neo4jAgree: true,         // same lead surrogate, trail decides, same relative order
		},
		{
			name:       "two BMP characters in the divergent range",
			lo:         "",
			hi:         "ﬁ",
			neo4jAgree: true, // neither is a surrogate pair
		},
		{
			name:       "ASCII, where the rules always agree",
			lo:         "Z",
			hi:         "a",
			neo4jAgree: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			eng := cypher.NewEngine(g)
			ctx := context.Background()
			// Seed hi first, so the expected order is not the insertion order.
			for _, v := range []string{tc.hi, tc.lo} {
				res, err := eng.RunInTxAny(ctx, "CREATE (:B {v: $v})", map[string]any{"v": v})
				if err != nil {
					t.Fatalf("seed: %v", err)
				}
				if cerr := res.Close(); cerr != nil {
					t.Fatalf("seed close: %v", cerr)
				}
			}

			got := orderedStrings(t, eng, "MATCH (n:B) RETURN n.v AS v ORDER BY n.v")
			assertOrder(t, "ORDER BY", got, []string{tc.lo, tc.hi})

			// The comparison operators must agree with ORDER BY: one comparator,
			// not two.
			if !stringLess(t, eng, tc.lo, tc.hi) {
				t.Errorf("%q < %q must be true under code-point order", tc.lo, tc.hi)
			}
			if stringLess(t, eng, tc.hi, tc.lo) {
				t.Errorf("%q < %q must be false under code-point order", tc.hi, tc.lo)
			}

			// Document the divergence in the failure surface: when Neo4j disagrees,
			// say so, so a reader of a future failure knows this row is the
			// deliberately divergent one rather than a bug.
			if !tc.neo4jAgree {
				t.Logf("this ordering is the DECLARED divergence from Neo4j: it compares "+
					"UTF-16 code units and would place %q before %q", tc.hi, tc.lo)
			}
		})
	}
}

// orderedStrings runs a read query and returns its single column in result order.
func orderedStrings(t *testing.T, eng *cypher.Engine, cy string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), cy, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", cy, err)
	}
	var out []string
	for res.Next() {
		out = append(out, unquote(fmt.Sprint(res.Record()["v"])))
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("Run(%q): drain: %v", cy, rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("Run(%q): close: %v", cy, cerr)
	}
	return out
}

// stringLess evaluates `$a < $b` through the engine's own comparison operator, so
// the test pins the comparator rather than a sort-only code path.
func stringLess(t *testing.T, eng *cypher.Engine, a, b string) bool {
	t.Helper()
	bound, err := cypher.BindParams(map[string]any{"a": a, "b": b})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	res, err := eng.Run(context.Background(), "RETURN $a < $b AS v", bound)
	if err != nil {
		t.Fatalf("Run comparison: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("close: %v", cerr)
		}
	}()
	if !res.Next() {
		t.Fatalf("comparison produced no row: %v", res.Err())
	}
	return fmt.Sprint(res.Record()["v"]) == "true"
}

// unquote decodes the string renderer's display form back to the underlying
// value, so the comparison is against the value rather than how it prints.
//
// strconv.Unquote rather than trimming the quotes: the renderer ESCAPES any
// non-printable or private-use character, so a value of U+E000 arrives as the
// six-byte text `\ue000`. Trimming quotes alone would compare that escape
// sequence against the real character and fail for exactly the boundary cases
// this file exists to cover.
func unquote(s string) string {
	if v, err := strconv.Unquote(s); err == nil {
		return v
	}
	return s
}

// assertOrder compares an observed ordering against the expected one, reporting
// the first divergence with code points so a failure is diagnosable.
func assertOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d rows, want %d\n got %q\nwant %q", what, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			gotRune, _ := utf8.DecodeRuneInString(got[i])
			wantRune, _ := utf8.DecodeRuneInString(want[i])
			t.Fatalf("%s diverges at position %d: got %q (U+%04X), want %q (U+%04X)\n got %q\nwant %q",
				what, i, got[i], gotRune, want[i], wantRune, got, want)
		}
	}
}
