package parser

import (
	"testing"
)

// TestParseStrictNormalizerParity verifies that Parse and ParseStrict apply the
// same normalizer pipeline. Before the fix, ParseStrict omitted
// normalizeArithmeticMinus, normalizeZeroDotFloat, and normalizeLeadingDotFloat,
// causing valid Cypher like "RETURN 0.5" or "RETURN .5" to be falsely rejected.
//
// For each input that Parse accepts, ParseStrict must also accept it (and vice
// versa). Neither may panic.
func TestParseStrictNormalizerParity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		query      string
		wantAccept bool // true = both should succeed, false = both should fail
	}{
		{"zero dot float", "RETURN 0.5", true},
		{"leading dot float", "RETURN .5", true},
		{"leading dot float 2", "RETURN .123", true},
		{"full decimal", "RETURN 0.123", true},
		{"arithmetic minus", "RETURN 1-2", true},
		{"valid int", "RETURN 42", true},
		{"valid string", "RETURN 'hello'", true},
		{"WITH zero dot return", "WITH 0.5 AS x RETURN x", true},
		{"WITH leading dot return", "WITH .5 AS x RETURN x", true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, parseErr := Parse(tc.query)
			_, strictErrs := ParseStrict(tc.query)

			parseOK := parseErr == nil
			strictOK := len(strictErrs) == 0

			// Both must agree: either both accept or both reject.
			if parseOK != strictOK {
				t.Errorf("divergence on %q: Parse ok=%v, ParseStrict ok=%v", tc.query, parseOK, strictOK)
			}

			if tc.wantAccept && !parseOK {
				t.Errorf("Parse(%q): expected success, got %v", tc.query, parseErr)
			}
			if tc.wantAccept && !strictOK {
				t.Errorf("ParseStrict(%q): expected success, got %v", tc.query, strictErrs)
			}
		})
	}
}
