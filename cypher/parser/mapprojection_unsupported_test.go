package parser

// mapprojection_unsupported_test.go — gate for the 2026-06-25 round-2 audit
// (#1770). Map projection (n{.name, .*, k: expr}) has AST/eval/sema support but
// no grammar production, so the parser MUST reject it with a parse error rather
// than appear to work. This pins that contract: if map projection is ever wired
// (grammar + ANTLR regen), this test will start failing and must be replaced by
// a positive map-projection test. See docs/tck/DIVERGENCES.md.

import (
	"errors"
	"testing"
)

func TestMapProjection_Unsupported_ParseError(t *testing.T) {
	t.Parallel()
	queries := []string{
		"MATCH (n:Person) RETURN n{.name, .age} AS m",
		"MATCH (n:Person) RETURN n{.*} AS m",
		"WITH {a:1,b:2} AS m RETURN m{.a, .b} AS x",
		"MATCH (n) RETURN n{.name, extra: 1} AS m",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(q)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded; map projection is not wired and must be a parse error (#1770)", q)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Parse(%q) err = %T (%v), want a *ParseError (SyntaxError)", q, err, err)
			}
		})
	}
}
