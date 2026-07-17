package ir_test

// ddl_show_test.go — parser tests for the SHOW CONSTRAINTS / SHOW INDEXES
// schema-introspection statements (#1922).
//
// These tests fail on the pre-change behaviour: before #1922, ir.IsShow did not
// exist, ir.IsDDL returned false for any SHOW statement, and ir.ParseDDL
// rejected SHOW as an unrecognised statement.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// TestIsShow verifies the SHOW classifier recognises the plain and singular
// forms (case-insensitively) and nothing else.
func TestIsShow(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"SHOW CONSTRAINTS", true},
		{"show constraints", true},
		{"SHOW CONSTRAINT", true},
		{"SHOW INDEXES", true},
		{"show indexes", true},
		{"SHOW INDEX", true},
		{"  SHOW INDEXES  ", true}, // leading/trailing space trimmed
		{"SHOW INDEXES;", true},
		{"MATCH (n) RETURN n", false},
		{"CREATE INDEX foo FOR (n:Person) ON (n.name)", false},
		{"DROP CONSTRAINT c", false},
		{"SHOW DATABASES", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := ir.IsShow(tc.query); got != tc.want {
			t.Errorf("IsShow(%q) = %v, want %v", tc.query, got, tc.want)
		}
		// Every SHOW recognised by IsShow must also be classified as DDL so it
		// routes through the hand-written DDL path (bypassing ANTLR).
		if tc.want && !ir.IsDDL(tc.query) {
			t.Errorf("IsDDL(%q) = false, want true (SHOW must dispatch as DDL)", tc.query)
		}
	}
}

// TestParseDDL_ShowConstraints checks every accepted SHOW CONSTRAINTS form
// parses to a *ShowConstraints plan.
func TestParseDDL_ShowConstraints(t *testing.T) {
	for _, q := range []string{
		"SHOW CONSTRAINTS",
		"show constraints",
		"SHOW CONSTRAINT",
		"SHOW CONSTRAINTS ;",
		"  SHOW CONSTRAINTS  ",
	} {
		plan, err := ir.ParseDDL(q)
		if err != nil {
			t.Fatalf("ParseDDL(%q): %v", q, err)
		}
		if _, ok := plan.(*ir.ShowConstraints); !ok {
			t.Errorf("ParseDDL(%q) = %T, want *ir.ShowConstraints", q, plan)
		}
	}
}

// TestParseDDL_ShowIndexes checks every accepted SHOW INDEXES form parses to a
// *ShowIndexes plan.
func TestParseDDL_ShowIndexes(t *testing.T) {
	for _, q := range []string{
		"SHOW INDEXES",
		"show indexes",
		"SHOW INDEX",
		"SHOW INDEXES ;",
		"  SHOW INDEXES  ",
	} {
		plan, err := ir.ParseDDL(q)
		if err != nil {
			t.Fatalf("ParseDDL(%q): %v", q, err)
		}
		if _, ok := plan.(*ir.ShowIndexes); !ok {
			t.Errorf("ParseDDL(%q) = %T, want *ir.ShowIndexes", q, plan)
		}
	}
}

// TestParseDDL_ShowPlanShape confirms the SHOW plans are leaf nodes with no
// children and no variables (LogicalPlan contract).
func TestParseDDL_ShowPlanShape(t *testing.T) {
	for _, q := range []string{"SHOW CONSTRAINTS", "SHOW INDEXES"} {
		plan, err := ir.ParseDDL(q)
		if err != nil {
			t.Fatalf("ParseDDL(%q): %v", q, err)
		}
		if kids := plan.Children(); kids != nil {
			t.Errorf("ParseDDL(%q).Children() = %v, want nil", q, kids)
		}
		if vars := plan.Vars(); vars != nil {
			t.Errorf("ParseDDL(%q).Vars() = %v, want nil", q, vars)
		}
	}
}

// TestParseDDL_ShowUnsupportedForms rejects the Neo4j BRIEF/VERBOSE/YIELD/WHERE
// suffixes and any unknown SHOW target with a clear error rather than silently
// mis-parsing.
func TestParseDDL_ShowUnsupportedForms(t *testing.T) {
	for _, q := range []string{
		"SHOW CONSTRAINTS BRIEF",
		"SHOW CONSTRAINTS VERBOSE",
		"SHOW INDEXES YIELD name, type",
		"SHOW INDEXES WHERE type = 'btree'",
		"SHOW INDEXES BRIEF",
		"SHOW FOO",
		"SHOW DATABASES",
	} {
		if _, err := ir.ParseDDL(q); err == nil {
			t.Errorf("ParseDDL(%q) = nil error, want a rejection", q)
		}
	}
}
