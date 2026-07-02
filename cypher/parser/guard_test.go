package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// asParseError asserts that err is a non-nil *ParseError and returns it.
func asParseError(t *testing.T, err error) *ParseError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a *ParseError, got nil")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ParseError, got %T: %v", err, err)
	}
	return pe
}

// TestGuardRejectsDeepNesting verifies that a query nested past maxNestingDepth
// is rejected with a *ParseError and does NOT crash the process. The three
// bracket families — '(', '[' and '{' — are each exercised independently.
//
// Without the guard the recursive parser/visitor overflows the goroutine stack,
// which is a fatal error that recover() cannot catch; the test would therefore
// abort the whole `go test` process rather than fail cleanly.
func TestGuardRejectsDeepNesting(t *testing.T) {
	// One level beyond the cap is enough to trip the guard; we use a large
	// multiple to make the intent unambiguous and to stay well clear of any
	// real stack-overflow threshold even if the guard were removed.
	const n = 150000

	cases := []struct {
		name   string
		prefix string // run before the brackets
		open   byte
		close  byte
		inner  string // innermost token between open and close runs
	}{
		{name: "parens", prefix: "RETURN ", open: '(', close: ')', inner: "1"},
		{name: "lists", prefix: "RETURN ", open: '[', close: ']', inner: ""},
		{name: "maps", prefix: "RETURN ", open: '{', close: '}', inner: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString(tc.prefix)
			b.WriteString(strings.Repeat(string(tc.open), n))
			b.WriteString(tc.inner)
			b.WriteString(strings.Repeat(string(tc.close), n))
			q := b.String()

			_, err := Parse(q)
			pe := asParseError(t, err)
			if !strings.Contains(pe.Message, "nesting too deep") {
				t.Fatalf("expected a nesting-depth error, got: %v", pe)
			}
		})
	}
}

// TestGuardDeepNestingParseStrict verifies the guard fires for ParseStrict too,
// returning the depth error as the sole error in the slice.
func TestGuardDeepNestingParseStrict(t *testing.T) {
	const n = 150000
	q := "RETURN " + strings.Repeat("(", n) + "1" + strings.Repeat(")", n)

	_, errs := ParseStrict(q)
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error, got %d: %v", len(errs), errs)
	}
	pe := asParseError(t, errs[0])
	if !strings.Contains(pe.Message, "nesting too deep") {
		t.Fatalf("expected a nesting-depth error, got: %v", pe)
	}
}

// TestGuardBracketsInStringNotCounted is the false-positive guard: a query whose
// string literal contains many '(' (or other brackets) must NOT be rejected,
// because brackets inside a string carry no structural meaning. This proves the
// scanner skips string content correctly.
func TestGuardBracketsInStringNotCounted(t *testing.T) {
	// A string literal far deeper than maxNestingDepth, wrapped in benign
	// structure. The whole query nests only one real level (the RETURN).
	bigParens := strings.Repeat("(", maxNestingDepth*4)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "single_quoted",
			query: "RETURN '" + bigParens + "' AS s",
		},
		{
			name:  "double_quoted",
			query: `RETURN "` + bigParens + `" AS s`,
		},
		{
			name: "mixed_brackets_in_string",
			query: "RETURN '" +
				strings.Repeat("([{", maxNestingDepth*2) + "' AS s",
		},
		{
			name:  "escaped_quote_inside_string",
			query: `RETURN '` + bigParens + `\'` + bigParens + `' AS s`,
		},
		{
			name:  "brackets_in_line_comment",
			query: "RETURN 1 AS n // " + bigParens,
		},
		{
			name:  "brackets_in_block_comment",
			query: "RETURN /* " + bigParens + " */ 1 AS n",
		},
		{
			name:  "brackets_in_backtick_identifier",
			query: "RETURN 1 AS `" + bigParens + "`",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The guard itself must not reject it.
			if err := guardInput(tc.query); err != nil {
				t.Fatalf("guard wrongly rejected a legitimate query: %v", err)
			}
			// And the full parse must succeed (these are valid Cypher).
			if _, err := Parse(tc.query); err != nil {
				t.Fatalf("Parse failed on a legitimate query: %v", err)
			}
		})
	}
}

// TestGuardWithinBoundParses verifies a query nested right at the limit still
// parses unchanged — the guard must not be off-by-one against legitimate input.
func TestGuardWithinBoundParses(t *testing.T) {
	// maxNestingDepth nested parentheses around a literal. Exactly at the cap
	// must be accepted (the guard rejects only depth > maxNestingDepth).
	q := "RETURN " + strings.Repeat("(", maxNestingDepth) + "1" +
		strings.Repeat(")", maxNestingDepth)

	if err := guardInput(q); err != nil {
		t.Fatalf("guard rejected a query exactly at the depth cap: %v", err)
	}
	if _, err := Parse(q); err != nil {
		t.Fatalf("Parse failed on a query at the depth cap: %v", err)
	}
}

// TestGuardOverLengthRejected verifies an over-length query is rejected with a
// *ParseError before any parsing work is done.
func TestGuardOverLengthRejected(t *testing.T) {
	// Pad a trivially-valid query with whitespace past the byte cap.
	q := "RETURN 1 AS n" + strings.Repeat(" ", maxQueryBytes)

	_, err := Parse(q)
	pe := asParseError(t, err)
	if !strings.Contains(pe.Message, "too large") {
		t.Fatalf("expected an over-length error, got: %v", pe)
	}
}

// TestGuardInputBoundaries exercises maxBracketDepth directly on small inputs to
// pin down the counting and skipping behaviour.
func TestMaxBracketDepth(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		depth int
	}{
		{name: "empty", in: "", depth: 0},
		{name: "no_brackets", in: "RETURN 1 AS n", depth: 0},
		{name: "flat_pairs", in: "()()()", depth: 1},
		{name: "nested_three", in: "((()))", depth: 3},
		{name: "mixed_families", in: "([{}])", depth: 3},
		{name: "interleaved", in: "(a[b{c}])", depth: 3},
		{name: "unbalanced_close_ignored", in: ")))(((", depth: 3},
		{name: "single_quoted_skipped", in: "'((((('", depth: 0},
		{name: "double_quoted_skipped", in: `"((((("`, depth: 0},
		{name: "backtick_skipped", in: "`(((((`", depth: 0},
		{name: "line_comment_skipped", in: "// (((((\n", depth: 0},
		{name: "block_comment_skipped", in: "/* ((((( */", depth: 0},
		{name: "escaped_quote_keeps_string", in: `'\'((' `, depth: 0},
		{name: "structure_around_string", in: "( '(((' )", depth: 1},
		{name: "structure_after_comment", in: "// x\n((", depth: 2},
		{name: "div_not_comment", in: "(a/b)", depth: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxBracketDepth(tc.in); got != tc.depth {
				t.Fatalf("maxBracketDepth(%q) = %d, want %d", tc.in, got, tc.depth)
			}
		})
	}
}

// TestGuardRejectsDeepCASENesting is the gate test for task #1383.
// A query with more than maxCASEKeywords CASE keywords must be rejected with a
// *ParseError before any recursive parsing begins.
//
// Gate semantics:
//
//	Before fix: countCASEKeywords is absent; the query has bracket depth 0 and
//	            is under 1 MiB, so guardInput returns nil → Parse succeeds →
//	            the test's expectation of a *ParseError FAILS.
//	After fix:  countCASEKeywords fires; Parse returns *ParseError → PASSES.
func TestGuardRejectsDeepCASENesting(t *testing.T) {
	// Build: RETURN CASE WHEN CASE WHEN … THEN 1 END THEN 2 END (300 levels)
	// 300 > maxCASEKeywords (256) so the guard must fire.
	const depth = 300
	var b strings.Builder
	b.WriteString("RETURN ")
	for i := 0; i < depth; i++ {
		b.WriteString("CASE WHEN ")
	}
	b.WriteString("true")
	for i := 0; i < depth; i++ {
		b.WriteString(" THEN 1 END")
	}

	q := b.String()
	_, err := Parse(q)
	pe := asParseError(t, err)
	if !strings.Contains(pe.Message, "CASE") {
		t.Fatalf("expected a CASE-depth error, got: %v", pe)
	}
}

// TestGuardRejectsLongOperatorChain is the gate test for task #1383.
// A query with more than maxBinaryOpTokens binary operators must be rejected.
//
// Gate semantics:
//
//	Before fix: countBinaryOpTokens absent; no error → Parse succeeds →
//	            test FAILS.
//	After fix:  countBinaryOpTokens fires → *ParseError → PASSES.
func TestGuardRejectsLongOperatorChain(t *testing.T) {
	// Build: RETURN true AND true AND true AND … (600 AND operators)
	// 600 > maxBinaryOpTokens (512) so the guard must fire.
	const n = 600
	var b strings.Builder
	b.WriteString("RETURN true")
	for i := 0; i < n; i++ {
		b.WriteString(" AND true")
	}

	q := b.String()
	_, err := Parse(q)
	pe := asParseError(t, err)
	if !strings.Contains(pe.Message, "operator") {
		t.Fatalf("expected an operator-count error, got: %v", pe)
	}
}

// TestGuardRejectsTightArithmeticChain is the gate test for audit finding F4
// (#1831): a byte-tight arithmetic chain of '-' (or '*') used to bypass the
// pre-parse operator guard entirely (the symbols were excluded), forcing the
// ANTLR parser + visitor to build a ~500k-node AST (~0.9 s CPU, ~1.2 GB
// transient) before the sema depth backstop fired — uninterruptible by any
// deadline. The guard now counts arithmetic-context '-'/'*', so the chain is
// rejected in O(n) before any AST is built.
func TestGuardRejectsTightArithmeticChain(t *testing.T) {
	t.Run("minus-chain", func(t *testing.T) {
		// RETURN 1-1-1-…-1 with 600 '-' operators (> maxBinaryOpTokens 512).
		var b strings.Builder
		b.WriteString("RETURN 1")
		for i := 0; i < 600; i++ {
			b.WriteString("-1")
		}
		_, err := Parse(b.String())
		pe := asParseError(t, err)
		if !strings.Contains(pe.Message, "operator") {
			t.Fatalf("expected an operator-count error for a tight '-' chain, got: %v", pe)
		}
	})
	t.Run("times-chain", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("RETURN 2")
		for i := 0; i < 600; i++ {
			b.WriteString("*2")
		}
		_, err := Parse(b.String())
		pe := asParseError(t, err)
		if !strings.Contains(pe.Message, "operator") {
			t.Fatalf("expected an operator-count error for a tight '*' chain, got: %v", pe)
		}
	})
}

// TestGuardRejectsTightComparisonChain is the gate test for the 2026-07-02
// audit finding (unfixed sibling of #1831): the pre-parse operator guard counted
// only arithmetic/boolean operators, so a byte-tight chain of comparison or
// string/list/null predicate operators (=, <, >, <=, >=, <>, =~, IN, CONTAINS,
// STARTS WITH, ENDS WITH, IS) still bypassed it — a ~2-byte-per-op '=' chain
// packed ~490k operators into a sub-1 MiB query and forced the ANTLR parser +
// visitor to build the whole AST (~0.9 s CPU, ~1.2 GB transient) uninterruptibly.
// The guard now counts the comparison/predicate class, so each chain is rejected
// in O(n) before any AST is built.
func TestGuardRejectsTightComparisonChain(t *testing.T) {
	// Each case builds "RETURN <seed><op>…" with 600 operators (> the 512 cap).
	cases := []struct {
		name string
		seed string
		op   string
	}{
		{name: "equals-chain", seed: "1", op: "=1"},
		{name: "lt-chain", seed: "1", op: "<1"},
		{name: "gt-chain", seed: "1", op: ">1"},
		{name: "lte-chain", seed: "1", op: "<=1"},
		{name: "gte-chain", seed: "1", op: ">=1"},
		{name: "neq-chain", seed: "1", op: "<>1"},
		{name: "regex-chain", seed: "'a'", op: "=~'a'"},
		{name: "in-chain", seed: "1", op: " IN[1]"},
		{name: "contains-chain", seed: "'a'", op: " CONTAINS 'a'"},
		{name: "starts-with-chain", seed: "'a'", op: " STARTS WITH 'a'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("RETURN ")
			b.WriteString(tc.seed)
			for i := 0; i < 600; i++ {
				b.WriteString(tc.op)
			}
			_, err := Parse(b.String())
			pe := asParseError(t, err)
			if !strings.Contains(pe.Message, "operator") {
				t.Fatalf("expected an operator-count error for a tight %q chain, got: %v", tc.op, pe)
			}
		})
	}
}

// TestGuardAllowsPatternDenseQuery is the false-positive guard for audit finding
// F4: a legitimate query with hundreds of relationship arrows and a
// variable-length pattern (far more '-' and '*' bytes than a rejected
// arithmetic chain) must NOT be rejected, proving the arithmetic-context rule
// never miscounts pattern tokens.
func TestGuardAllowsPatternDenseQuery(t *testing.T) {
	// A long chain of relationship hops: (n0)-[r1]->(n1)-[r2]->…-(n300).
	var b strings.Builder
	b.WriteString("MATCH (n0)")
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "-[r%d:R*1..3]->(n%d)", i, i)
	}
	b.WriteString(" RETURN n0")
	q := b.String()
	if err := guardInput(q); err != nil {
		t.Fatalf("guard rejected a legitimate pattern-dense query (%d '-' and 300 '*' bytes): %v", strings.Count(q, "-"), err)
	}
	if c := countBinaryOpTokens(q); c != 0 {
		t.Fatalf("countBinaryOpTokens miscounted pattern tokens as %d arithmetic operators; want 0", c)
	}
}

// TestGuardAllowsLegitimateComplexQuery verifies the guard does NOT reject a
// legitimate query with a small number of CASE expressions and operators.
func TestGuardAllowsLegitimateComplexQuery(t *testing.T) {
	// 5 CASE + 10 AND operators — well under any limit.
	q := `RETURN CASE WHEN true THEN 1 ELSE 2 END AND
                  CASE WHEN false THEN 3 ELSE 4 END AND
                  1 = 1 AND 2 = 2 AND 3 = 3 AS result`
	if err := guardInput(q); err != nil {
		t.Fatalf("guard rejected a legitimate query: %v", err)
	}
}

// TestCountCASEKeywords exercises countCASEKeywords on small inputs.
func TestCountCASEKeywords(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		count int
	}{
		{name: "empty", in: "", count: 0},
		{name: "no_case", in: "RETURN 1", count: 0},
		{name: "one_case", in: "RETURN CASE WHEN true THEN 1 END", count: 1},
		{name: "two_cases", in: "RETURN CASE WHEN CASE WHEN true THEN 1 END THEN 2 END", count: 2},
		{name: "case_lowercase", in: "return case when true then 1 end", count: 1},
		{name: "case_mixed", in: "RETURN Case WHEN true THEN 1 End", count: 1},
		{name: "case_in_string_skipped", in: "RETURN 'CASE' AS s", count: 0},
		{name: "case_in_double_string_skipped", in: `RETURN "CASE" AS s`, count: 0},
		{name: "case_in_line_comment_skipped", in: "RETURN 1 // CASE\n AS n", count: 0},
		{name: "case_in_block_comment_skipped", in: "RETURN /* CASE */ 1", count: 0},
		{name: "case_in_backtick_skipped", in: "RETURN 1 AS `CASE`", count: 0},
		{name: "notcase_prefix_skipped", in: "RETURN LOWERCASE", count: 0},
		{name: "notcase_suffix_skipped", in: "RETURN CASEWORK", count: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countCASEKeywords(tc.in); got != tc.count {
				t.Fatalf("countCASEKeywords(%q) = %d, want %d", tc.in, got, tc.count)
			}
		})
	}
}

// TestCountBinaryOpTokens exercises countBinaryOpTokens on small inputs.
func TestCountBinaryOpTokens(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		count int
	}{
		{name: "empty", in: "", count: 0},
		{name: "no_ops", in: "RETURN 1 AS n", count: 0},
		{name: "plus", in: "RETURN 1 + 2", count: 1},
		// '-' and '*' are counted ONLY in tight arithmetic context (both
		// neighbours operand bytes, '*' also outside '[...]' or digit-left). Spaced
		// forms are not counted (a space is not an operand byte) — acceptable, as
		// the guard targets the maximally dense byte-tight chain a hostile client
		// uses; and no relationship/VLE pattern token is ever counted.
		{name: "minus_spaced_not_counted", in: "RETURN 1 - 2", count: 0},
		{name: "times_spaced_not_counted", in: "RETURN 1 * 2", count: 0},
		{name: "minus_tight_counted", in: "RETURN 1-2", count: 1},
		{name: "times_tight_counted", in: "RETURN 2*3", count: 1},
		{name: "minus_ident_counted", in: "RETURN a-b", count: 1},
		{name: "minus_property_counted", in: "RETURN n.x-n.y", count: 1},
		{name: "minus_chain_counted", in: "RETURN 1-1-1-1", count: 3},
		{name: "rel_arrow_dash_not_counted", in: "MATCH (a)-[r]->(b) RETURN a", count: 0},
		{name: "rel_undirected_dash_not_counted", in: "MATCH (a)-[r]-(b) RETURN a", count: 0},
		{name: "rel_bare_dash_not_counted", in: "MATCH (a)-->(b) RETURN a", count: 0},
		{name: "vle_star_not_counted", in: "MATCH (a)-[:R*1..5]->(b) RETURN a", count: 0},
		{name: "vle_anon_star_not_counted", in: "MATCH (a)-[*2..4]->(b) RETURN a", count: 0},
		{name: "vle_var_star_not_counted", in: "MATCH p=(a)-[r*]->(b) RETURN p", count: 0},
		{name: "count_star_not_counted", in: "RETURN count(*)", count: 0},
		{name: "return_star_not_counted", in: "MATCH (n) RETURN *", count: 0},
		{name: "map_projection_star_not_counted", in: "MATCH (n) RETURN n{.*}", count: 0},
		{name: "times_in_list_digit_counted", in: "RETURN [1*2]", count: 1},
		{name: "divide", in: "RETURN 1 / 2", count: 1},
		{name: "mod", in: "RETURN 1 % 2", count: 1},
		{name: "power", in: "RETURN 2 ^ 3", count: 1},
		{name: "and", in: "RETURN true AND false", count: 1},
		{name: "or", in: "RETURN true OR false", count: 1},
		{name: "xor", in: "RETURN true XOR false", count: 1},
		{name: "not", in: "RETURN NOT true", count: 1},
		{name: "and_lowercase", in: "RETURN true and false", count: 1},
		{name: "chain_three_and", in: "RETURN a AND b AND c", count: 2},
		{name: "and_in_string_skipped", in: "RETURN 'AND' AS s", count: 0},
		{name: "or_in_comment_skipped", in: "RETURN 1 // OR\n AS n", count: 0},
		{name: "divide_comment_not_counted", in: "RETURN /* divide */ 1", count: 0},
		{name: "not_word_boundary_skipped", in: "RETURN ANDROID", count: 0},
		{name: "div_not_comment", in: "RETURN 4 / 2", count: 1},
		{name: "line_comment_div", in: "// a / b\nRETURN 1", count: 0},
		// Comparison operators: single '='/'<'/'>' counted only in comparison
		// context; the two-byte forms '<='/'>='/'<>'/'=~' are unambiguous.
		{name: "equals_tight_counted", in: "RETURN 1=1", count: 1},
		{name: "equals_chain_counted", in: "RETURN 1=1=1", count: 2},
		{name: "equals_spaced_not_counted", in: "RETURN 1 = 1", count: 0},
		{name: "lt_counted", in: "RETURN a<b", count: 1},
		{name: "gt_counted", in: "RETURN a>b", count: 1},
		{name: "lt_chain_counted", in: "RETURN 1<2<3", count: 2},
		{name: "lte_counted", in: "RETURN 1<=2", count: 1},
		{name: "gte_counted", in: "RETURN 1>=2", count: 1},
		{name: "neq_counted", in: "RETURN 1<>2", count: 1},
		{name: "regex_counted", in: "RETURN x=~'a'", count: 1},
		// Comparison operators must never miscount relationship arrows or a path
		// assignment / SET map (right neighbour '(' or '{').
		{name: "incoming_arrow_not_counted", in: "MATCH (a)<-[r]-(b) RETURN a", count: 0},
		{name: "bare_incoming_arrow_not_counted", in: "MATCH (a)<--(b) RETURN a", count: 0},
		{name: "outgoing_arrow_gt_not_counted", in: "MATCH (a)-[r]->(b) RETURN a", count: 0},
		{name: "path_assign_arrow_not_counted", in: "MATCH p=(a)-->(b) RETURN p", count: 0},
		{name: "set_map_not_counted", in: "MATCH (n) SET n={x:1} RETURN n", count: 0},
		// Predicate keywords: IN, CONTAINS, STARTS WITH, ENDS WITH, IS.
		{name: "in_counted", in: "RETURN 1 IN[2]", count: 1},
		{name: "in_lowercase_counted", in: "RETURN 1 in [2]", count: 1},
		{name: "contains_counted", in: "RETURN s CONTAINS 't'", count: 1},
		{name: "starts_with_counted", in: "RETURN s STARTS WITH 'a'", count: 1},
		{name: "ends_with_counted", in: "RETURN s ENDS WITH 'a'", count: 1},
		{name: "is_null_counted", in: "RETURN x IS NULL", count: 1},
		{name: "is_not_null_counted", in: "RETURN x IS NOT NULL", count: 2},
		// Predicate keywords must respect word boundaries (identifiers that merely
		// start with a keyword are not operators).
		{name: "in_prefix_ident_not_counted", in: "RETURN inner", count: 0},
		{name: "is_prefix_ident_not_counted", in: "RETURN island", count: 0},
		{name: "starts_prefix_ident_not_counted", in: "RETURN starts", count: 0},
		{name: "contains_prefix_ident_not_counted", in: "RETURN containsfoo", count: 0},
		{name: "keyword_in_string_not_counted", in: "RETURN 'a STARTS WITH b' AS s", count: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countBinaryOpTokens(tc.in); got != tc.count {
				t.Fatalf("countBinaryOpTokens(%q) = %d, want %d", tc.in, got, tc.count)
			}
		})
	}
}

// TestItoa pins the dependency-free integer formatter used in guard messages.
func TestItoa(t *testing.T) {
	cases := map[int]string{
		0:       "0",
		7:       "7",
		256:     "256",
		1 << 20: "1048576",
	}
	for n, want := range cases {
		if got := itoa(n); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}
