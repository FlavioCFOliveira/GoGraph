package ir

// ddl_leading_comment_test.go — regression coverage for the DDL dispatch defect
// found by the documentation gate (task #2227).
//
// DDL dispatch is a prefix test on the raw query text, not on a token stream.
// IsDDL trimmed whitespace only, so a statement opening with a comment was not
// recognised as DDL and was routed to the ANTLR grammar, which rejected it —
// `// named` + `CREATE INDEX …` failed with `unexpected "person_email"`. Five
// commented DDL examples in docs/cypher.md were therefore broken, and nothing
// noticed, because no gate executed a documented example.
//
// The lexer sends both `//` and `/* … */` to a hidden channel, so the DML path
// has always accepted a leading comment. These tests pin that the DDL path
// agrees.

import "testing"

// TestIsDDL_SkipsLeadingComments asserts that a leading comment does not hide a
// DDL statement from dispatch.
func TestIsDDL_SkipsLeadingComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		query  string
		wantOK bool
		show   bool // also expected to satisfy IsShow
	}{
		{"plain create index", `CREATE INDEX i FOR (n:P) ON (n.x)`, true, false},
		{"line comment", "// named\nCREATE INDEX i FOR (n:P) ON (n.x)", true, false},
		{"two line comments", "// one\n// two\nCREATE INDEX i FOR (n:P) ON (n.x)", true, false},
		{"line comment indented", "   // named\n   CREATE INDEX i FOR (n:P) ON (n.x)", true, false},
		{"block comment", "/* named */ CREATE INDEX i FOR (n:P) ON (n.x)", true, false},
		{"multiline block comment", "/* named\n   and explained */\nCREATE INDEX i FOR (n:P) ON (n.x)", true, false},
		{"mixed comments", "// first\n/* second */\nDROP INDEX i", true, false},
		{"drop constraint", "// tidy up\nDROP CONSTRAINT c", true, false},
		{"create constraint", "// uniqueness\nCREATE CONSTRAINT c FOR (n:P) REQUIRE n.x IS UNIQUE", true, false},
		{"show constraints", "// list them\nSHOW CONSTRAINTS", true, true},
		{"show indexes", "/* list */ SHOW INDEXES", true, true},

		// Non-DDL must stay non-DDL.
		{"match is not ddl", `MATCH (n) RETURN n`, false, false},
		{"commented match is not ddl", "// a read\nMATCH (n) RETURN n", false, false},
		{"comment only", "// nothing follows", false, false},
		{"unterminated block comment", "/* never closed CREATE INDEX i FOR (n:P) ON (n.x)", false, false},
		{"create node is not ddl", "// build\nCREATE (n:P)", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsDDL(tc.query); got != tc.wantOK {
				t.Errorf("IsDDL(%q) = %v, want %v", tc.query, got, tc.wantOK)
			}
			if got := IsShow(tc.query); got != tc.show {
				t.Errorf("IsShow(%q) = %v, want %v", tc.query, got, tc.show)
			}
		})
	}
}

// TestParseDDL_SkipsLeadingComments asserts that ParseDDL parses a commented
// statement into the same plan as the uncommented form, so the comment is
// genuinely ignored rather than merely surviving dispatch.
func TestParseDDL_SkipsLeadingComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		commented  string
		equivalent string
	}{
		{
			name:       "create index",
			commented:  "// named\nCREATE INDEX person_email FOR (n:Person) ON (n.email)",
			equivalent: `CREATE INDEX person_email FOR (n:Person) ON (n.email)`,
		},
		{
			name:       "create index block comment",
			commented:  "/* named */\nCREATE INDEX person_email FOR (n:Person) ON (n.email)",
			equivalent: `CREATE INDEX person_email FOR (n:Person) ON (n.email)`,
		},
		{
			name:       "drop index",
			commented:  "// tidy\nDROP INDEX person_email",
			equivalent: `DROP INDEX person_email`,
		},
		{
			name:       "create constraint",
			commented:  "// uniqueness constraint\nCREATE CONSTRAINT person_email_unique FOR (n:Person) REQUIRE n.email IS UNIQUE",
			equivalent: `CREATE CONSTRAINT person_email_unique FOR (n:Person) REQUIRE n.email IS UNIQUE`,
		},
		{
			name:       "drop constraint",
			commented:  "// tidy\nDROP CONSTRAINT person_email_unique",
			equivalent: `DROP CONSTRAINT person_email_unique`,
		},
		{
			name:       "show constraints",
			commented:  "// list them\nSHOW CONSTRAINTS",
			equivalent: `SHOW CONSTRAINTS`,
		},
		{
			name:       "show indexes yield",
			commented:  "// project two columns\nSHOW INDEXES YIELD name, type",
			equivalent: `SHOW INDEXES YIELD name, type`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotCommented, err := ParseDDL(tc.commented)
			if err != nil {
				t.Fatalf("ParseDDL(%q): %v", tc.commented, err)
			}
			gotPlain, err := ParseDDL(tc.equivalent)
			if err != nil {
				t.Fatalf("ParseDDL(%q): %v", tc.equivalent, err)
			}
			// The plans must be of the same concrete type and render identically;
			// the comment must not leak into a parsed name or predicate.
			if a, b := Explain(gotCommented), Explain(gotPlain); a != b {
				t.Errorf("commented and plain forms differ:\n commented: %s\n plain:     %s", a, b)
			}
		})
	}
}
