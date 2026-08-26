package tck_test

// rel_uniqueness_error_name_test.go — rmp #2252 asserts the ERROR NAME TOKEN,
// which no other gate does for this shape.
//
// The TCK runner's own assertion (world.assertSyntaxError) accepts any error in
// the right phase and does NOT compare the openCypher error-name token, and the
// fidelity ratchet counts matches over the TCK's own scenario population — where
// the clause-scoped comma form does not appear at all (Match3 [29] covers only
// the single-path-pattern form). So without this test the fix could raise the
// WRONG token and every existing gate would stay green.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

// TestRelUniquenessErrorName_ClauseScopedFormClassifiesCorrectly runs the two
// shapes through parse + Analyse and requires both to classify to the SAME
// openCypher token the TCK expects for the single-pattern form.
func TestRelUniquenessErrorName_ClauseScopedFormClassifiesCorrectly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
	}{
		// The shape TCK Match3 scenario [29] pins.
		{"same_path_pattern", "MATCH (a)-[r]->()-[r]->(a) RETURN r"},
		// The shape rmp #2252 brought under the same check. The TCK does not
		// cover it, which is exactly why the token is asserted here.
		{"sibling_comma_pattern", "MATCH (a)-[r]->(b), (b)-[r]->(a) RETURN r"},
	}
	const want = "RelationshipUniquenessViolation"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := parser.Parse(tc.query)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			errs := sema.Analyse(q)
			if len(errs) == 0 {
				t.Fatalf("Analyse raised NO error for %q", tc.query)
			}
			// Classify every reported error and require the expected token to
			// be among them; the analyser may also report downstream errors.
			var got []string
			found := false
			for i := range errs {
				tok, ok := classifyTCKErrorType(&errs[i])
				if ok {
					got = append(got, tok)
					if tok == want {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("no error classified as %q; classified tokens: %v; errors: %v", want, got, errs)
			}
		})
	}
}
