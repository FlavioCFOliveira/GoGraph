package ir_test

// ddl_show_test.go — parser tests for the SHOW CONSTRAINTS / SHOW INDEXES
// schema-introspection statements (#1922).
//
// These tests fail on the pre-change behaviour: before #1922, ir.IsShow did not
// exist, ir.IsDDL returned false for any SHOW statement, and ir.ParseDDL
// rejected SHOW as an unrecognised statement.

import (
	"reflect"
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

// TestParseDDL_ShowUnsupportedForms rejects the Neo4j BRIEF/VERBOSE suffixes and
// any unknown SHOW target with a clear error rather than silently mis-parsing.
// (YIELD / WHERE are now supported — see TestParseDDL_ShowYield.)
func TestParseDDL_ShowUnsupportedForms(t *testing.T) {
	for _, q := range []string{
		"SHOW CONSTRAINTS BRIEF",
		"SHOW CONSTRAINTS VERBOSE",
		"SHOW INDEXES BRIEF",
		"SHOW FOO",
		"SHOW DATABASES",
	} {
		if _, err := ir.ParseDDL(q); err == nil {
			t.Errorf("ParseDDL(%q) = nil error, want a rejection", q)
		}
	}
}

// showProjOf parses q and returns the ShowProjection carried by the resulting
// ShowConstraints/ShowIndexes plan, failing the test on any error or shape
// mismatch.
func showProjOf(t *testing.T, q string) *ir.ShowProjection {
	t.Helper()
	plan, err := ir.ParseDDL(q)
	if err != nil {
		t.Fatalf("ParseDDL(%q): %v", q, err)
	}
	switch p := plan.(type) {
	case *ir.ShowConstraints:
		return p.Projection
	case *ir.ShowIndexes:
		return p.Projection
	default:
		t.Fatalf("ParseDDL(%q) = %T, want a SHOW plan", q, plan)
		return nil
	}
}

// yieldOutputs returns the ordered YIELD output (aliased) names of a projection.
func yieldOutputs(proj *ir.ShowProjection) []string {
	out := make([]string, len(proj.Project))
	for i, p := range proj.Project {
		out[i] = p.Output
	}
	return out
}

// TestParseDDL_ShowPlainHasNoProjection confirms the plain and singular forms
// carry a nil Projection (unchanged behaviour).
func TestParseDDL_ShowPlainHasNoProjection(t *testing.T) {
	for _, q := range []string{
		"SHOW CONSTRAINTS", "SHOW CONSTRAINT", "SHOW INDEXES", "SHOW INDEX",
		"SHOW CONSTRAINTS ;", "  SHOW INDEXES  ",
	} {
		if proj := showProjOf(t, q); proj != nil {
			t.Errorf("ParseDDL(%q).Projection = %+v, want nil (plain form)", q, proj)
		}
	}
}

// TestParseDDL_ShowYield verifies the accepted YIELD / WHERE / RETURN forms parse
// into the expected ShowProjection shape.
func TestParseDDL_ShowYield(t *testing.T) {
	t.Run("explicit columns", func(t *testing.T) {
		proj := showProjOf(t, "SHOW CONSTRAINTS YIELD name, type")
		if got, want := yieldOutputs(proj), []string{"name", "type"}; !reflect.DeepEqual(got, want) {
			t.Errorf("yield outputs = %v, want %v", got, want)
		}
		if proj.Where != nil || proj.Return != nil {
			t.Errorf("expected no WHERE/RETURN, got where=%v return=%v", proj.Where, proj.Return)
		}
	})

	t.Run("aliases", func(t *testing.T) {
		proj := showProjOf(t, "SHOW INDEXES YIELD name AS n, type AS t")
		if got, want := yieldOutputs(proj), []string{"n", "t"}; !reflect.DeepEqual(got, want) {
			t.Errorf("yield outputs = %v, want %v", got, want)
		}
		if proj.Project[0].Source != "name" || proj.Project[1].Source != "type" {
			t.Errorf("sources = %q,%q, want name,type", proj.Project[0].Source, proj.Project[1].Source)
		}
	})

	t.Run("yield star expands to all columns", func(t *testing.T) {
		proj := showProjOf(t, "SHOW CONSTRAINTS YIELD *")
		if got := yieldOutputs(proj); !reflect.DeepEqual(got, ir.ShowConstraintColumns) {
			t.Errorf("yield * outputs = %v, want %v", got, ir.ShowConstraintColumns)
		}
	})

	t.Run("yield with where", func(t *testing.T) {
		proj := showProjOf(t, "SHOW CONSTRAINTS YIELD name, type WHERE type = 'UNIQUE'")
		if proj.Where == nil {
			t.Fatal("expected a WHERE predicate")
		}
	})

	t.Run("where without yield scopes all columns", func(t *testing.T) {
		proj := showProjOf(t, "SHOW INDEXES WHERE type = 'btree'")
		if got := yieldOutputs(proj); !reflect.DeepEqual(got, ir.ShowIndexColumns) {
			t.Errorf("where-only outputs = %v, want all %v", got, ir.ShowIndexColumns)
		}
		if proj.Where == nil {
			t.Fatal("expected a WHERE predicate")
		}
	})

	t.Run("yield with return", func(t *testing.T) {
		proj := showProjOf(t, "SHOW CONSTRAINTS YIELD name, type RETURN name AS c, type")
		if proj.Return == nil {
			t.Fatal("expected a RETURN clause")
		}
		if got, want := proj.ReturnColumns, []string{"c", "type"}; !reflect.DeepEqual(got, want) {
			t.Errorf("return columns = %v, want %v", got, want)
		}
	})

	t.Run("case-insensitive keywords", func(t *testing.T) {
		proj := showProjOf(t, "show constraints yield name, type where type = 'UNIQUE'")
		if got, want := yieldOutputs(proj), []string{"name", "type"}; !reflect.DeepEqual(got, want) {
			t.Errorf("yield outputs = %v, want %v", got, want)
		}
	})
}

// TestParseDDL_ShowYieldRejected verifies malformed or unsupported YIELD forms
// are rejected with a non-nil error at parse time.
func TestParseDDL_ShowYieldRejected(t *testing.T) {
	for _, q := range []string{
		"SHOW CONSTRAINTS YIELD bogus",                                // unknown column
		"SHOW CONSTRAINTS YIELD name WHERE type = 'UNIQUE'",           // type not yielded (scope barrier)
		"SHOW CONSTRAINTS RETURN name",                                // RETURN without YIELD
		"SHOW CONSTRAINTS YIELD * RETURN name",                        // YIELD * with RETURN
		"SHOW CONSTRAINTS YIELD name ORDER BY name",                   // YIELD-level ORDER BY
		"SHOW CONSTRAINTS YIELD name, type RETURN name ORDER BY name", // RETURN ORDER BY
		"SHOW CONSTRAINTS YIELD name, type RETURN name SKIP 1",        // RETURN SKIP
		"SHOW CONSTRAINTS YIELD name, type RETURN name LIMIT 1",       // RETURN LIMIT
		"SHOW CONSTRAINTS YIELD name, type RETURN DISTINCT name",      // RETURN DISTINCT
		"SHOW CONSTRAINTS YIELD type RETURN count(*)",                 // aggregation
		"SHOW CONSTRAINTS YIELD toUpper(name)",                        // expression in YIELD
		"SHOW CONSTRAINTS YIELD name RETURN bogus",                    // RETURN refers out of scope
	} {
		if _, err := ir.ParseDDL(q); err == nil {
			t.Errorf("ParseDDL(%q) = nil error, want a rejection", q)
		}
	}
}
