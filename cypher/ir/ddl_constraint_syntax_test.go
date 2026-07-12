package ir

// ddl_constraint_syntax_test.go — regression for #1906: the modern
// FOR … REQUIRE constraint grammar (Neo4j 4.x+/current) must parse to the same
// IR as the legacy ON … ASSERT form, and out-of-scope forms must be rejected
// with a specific error rather than a misleading parse failure.

import (
	"strings"
	"testing"
)

func TestParseCreateConstraint_ModernAndLegacyGrammars(t *testing.T) {
	cases := []struct {
		query    string
		wantName string
		wantLbl  string
		wantProp string
		wantKind ConstraintKind
		wantINE  bool
	}{
		// Modern FOR … REQUIRE.
		{`CREATE CONSTRAINT c1 FOR (n:User) REQUIRE n.email IS UNIQUE`, "c1", "User", "email", ConstraintUnique, false},
		{`CREATE CONSTRAINT c2 IF NOT EXISTS FOR (n:User) REQUIRE n.name IS NOT NULL`, "c2", "User", "name", ConstraintNotNull, true},
		{`CREATE CONSTRAINT FOR (n:User) REQUIRE n.email IS UNIQUE`, "user_email_unique", "User", "email", ConstraintUnique, false},
		{`CREATE CONSTRAINT FOR (n:User) REQUIRE (n.email) IS UNIQUE`, "user_email_unique", "User", "email", ConstraintUnique, false},
		// Legacy ON … ASSERT (kept as an alias).
		{`CREATE CONSTRAINT c3 ON (n:User) ASSERT n.email IS UNIQUE`, "c3", "User", "email", ConstraintUnique, false},
		{`CREATE CONSTRAINT ON (n:User) ASSERT n.tier IS NOT NULL`, "user_tier_not_null", "User", "tier", ConstraintNotNull, false},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			plan, err := ParseDDL(tc.query)
			if err != nil {
				t.Fatalf("ParseDDL: %v", err)
			}
			cc, ok := plan.(*CreateConstraint)
			if !ok {
				t.Fatalf("expected *CreateConstraint, got %T", plan)
			}
			if cc.Name != tc.wantName || cc.Label != tc.wantLbl || cc.Property != tc.wantProp ||
				cc.Kind != tc.wantKind || cc.IfNotExists != tc.wantINE {
				t.Fatalf("got {name:%q label:%q prop:%q kind:%d ine:%v}, want {name:%q label:%q prop:%q kind:%d ine:%v}",
					cc.Name, cc.Label, cc.Property, cc.Kind, cc.IfNotExists,
					tc.wantName, tc.wantLbl, tc.wantProp, tc.wantKind, tc.wantINE)
			}
		})
	}
}

func TestParseCreateConstraint_RejectsUnsupportedForms(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantSubstr string
	}{
		{"relationship", `CREATE CONSTRAINT c FOR ()-[r:KNOWS]-() REQUIRE r.since IS NOT NULL`, "relationship"},
		{"composite", `CREATE CONSTRAINT c FOR (n:User) REQUIRE (n.a, n.b) IS UNIQUE`, "composite"},
		{"node key", `CREATE CONSTRAINT c FOR (n:User) REQUIRE n.a IS NODE KEY`, "NODE KEY"},
		{"type", `CREATE CONSTRAINT c FOR (n:User) REQUIRE n.a IS :: STRING`, "type"},
		{"mismatched connective", `CREATE CONSTRAINT c FOR (n:User) ASSERT n.a IS UNIQUE`, "REQUIRE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDDL(tc.query)
			if err == nil {
				t.Fatalf("expected rejection for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
