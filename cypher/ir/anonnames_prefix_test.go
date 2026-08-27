package ir

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// TestAnonSubqueryPrefixIsRefinement pins the relationship between the two
// synthetic-name prefixes, which Go cannot assert at compile time because a
// string-prefix test is not a constant expression.
//
// [IsSyntheticVar] — and therefore [UserNamed], and therefore the fast-path
// recognisers in degree_shape.go and labelled_hop_count.go — tests only
// [anonVarPrefix]. If [ast.SyntheticSubqueryVarPrefix] stopped starting with it,
// every name the subquery pre-naming pass mints would read as USER-named, and
// those recognisers would silently stop firing for subquery shapes: no error, no
// wrong answer, just a fall back to driving an inner plan per outer row. That is
// exactly the regression rmp #2508's fix caused once and had to repair.
func TestAnonSubqueryPrefixIsRefinement(t *testing.T) {
	if !strings.HasPrefix(anonSubqueryVarPrefix, anonVarPrefix) {
		t.Fatalf("anonSubqueryVarPrefix %q must start with anonVarPrefix %q, or IsSyntheticVar stops recognising it",
			anonSubqueryVarPrefix, anonVarPrefix)
	}
	if anonSubqueryVarPrefix == anonVarPrefix {
		t.Fatalf("anonSubqueryVarPrefix must be a STRICT refinement of %q, otherwise the two namespaces collide",
			anonVarPrefix)
	}
	// anonVarOrdinal must NOT read these names as ordinals: the digits do not
	// follow anonVarPrefix immediately. If it did, reserveAnonVars would consume
	// ordinals from a namespace it does not own.
	if n, ok := anonVarOrdinal(anonSubqueryVarPrefix + "3"); ok {
		t.Errorf("anonVarOrdinal(%q) = (%d, true), want ok=false", anonSubqueryVarPrefix+"3", n)
	}
	// The names must be recognised as synthetic, and must not be user-named.
	name := anonSubqueryVarPrefix + "0"
	if !IsSyntheticVar(name) {
		t.Errorf("IsSyntheticVar(%q) = false, want true", name)
	}
	if UserNamed(&name) {
		t.Errorf("UserNamed(%q) = true, want false", name)
	}
	// A control: a name the user could actually write must be user-named, so the
	// assertions above are not passing because the predicates always answer the
	// same way.
	user := "friend"
	if IsSyntheticVar(user) || !UserNamed(&user) {
		t.Errorf("predicates misclassify the user name %q as synthetic", user)
	}
}

// TestSyntheticSubqueryNameIsNotRendered pins the rendering contract at the AST
// level: a name minted under [ast.SyntheticSubqueryVarPrefix] must not appear in a
// pattern's rendered form, because that rendering is the default result-column
// name for an un-aliased projection.
//
// The plain `__anon_` names are asserted to STILL render. That asymmetry is
// deliberate: they predate the pass and are already part of renderings that other
// code depends on — ir.NewMerge uses a MERGE pattern's rendering as the operator's
// identity — so suppressing them would change behaviour instead of preserving it.
func TestSyntheticSubqueryNameIsNotRendered(t *testing.T) {
	synthetic := ast.SyntheticSubqueryVarPrefix + "0"
	translatorMinted := "__anon_0"
	user := "n"

	t.Run("node_pattern", func(t *testing.T) {
		cases := []struct {
			name string
			v    *string
			want string
		}{
			{"synthetic_is_hidden", &synthetic, "(:P)"},
			{"translator_minted_still_renders", &translatorMinted, "(__anon_0:P)"},
			{"user_name_renders", &user, "(n:P)"},
			{"nil_renders_bare", nil, "(:P)"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := (&ast.NodePattern{Variable: tc.v, Labels: []string{"P"}}).String()
				if got != tc.want {
					t.Errorf("String() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("relationship_pattern", func(t *testing.T) {
		cases := []struct {
			name string
			v    *string
			want string
		}{
			{"synthetic_is_hidden", &synthetic, "-[:K]->"},
			{"translator_minted_still_renders", &translatorMinted, "-[__anon_0:K]->"},
			{"user_name_renders", &user, "-[n:K]->"},
			{"nil_renders_bare", nil, "-[:K]->"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := (&ast.RelationshipPattern{
					Variable:  tc.v,
					Types:     []string{"K"},
					Direction: ast.RelDirectionOutgoing,
				}).String()
				if got != tc.want {
					t.Errorf("String() = %q, want %q", got, tc.want)
				}
			})
		}
	})
}
