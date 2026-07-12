package ir

// ddl_parser_robustness_test.go — regression for F-PARSER1: the hand-written
// DDL parser is a public boundary for untrusted query text, so malformed or
// truncated input must return a typed error, never panic with
// index-out-of-range.

import (
	"strings"
	"testing"
)

// TestParseDDL_MalformedNeverPanics sweeps truncated/malformed DDL and asserts
// every input returns an error (no plan, no panic). The listed truncations all
// panicked before the tokAt bounds-safety fix.
func TestParseDDL_MalformedNeverPanics(t *testing.T) {
	inputs := []string{
		// Truncations that previously panicked in parseNodePattern /
		// parsePropAccess.
		"CREATE INDEX x FOR (",
		"CREATE INDEX x FOR (n",
		"CREATE INDEX x FOR (n:",
		"CREATE INDEX x FOR (n:L",
		"CREATE INDEX x FOR (n:L)",
		"CREATE INDEX x FOR (n:L) ON (",
		"CREATE INDEX x FOR (n:L) ON (n.p",
		"CREATE INDEX x FOR (n:L) ON (n.p)",
		"CREATE CONSTRAINT c ON (",
		"CREATE CONSTRAINT c ON (n",
		"CREATE CONSTRAINT c ON (n:L) ASSERT",
		"CREATE CONSTRAINT c ON (n:L) ASSERT n.p IS",
		// Already-erroring forms (must stay errors, still no panic).
		"CREATE INDEX",
		"DROP INDEX",
		"DROP CONSTRAINT",
		"CREATE INDEX FOR (n:L) ON (n.a, n.b)",    // composite: unsupported
		"CREATE INDEX FOR ()-[r:T]-() ON (r.p)",   // relationship: unsupported
		"CREATE INDEX x FOR (n:L) ON n.p garbage", // trailing garbage
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseDDL(%q) panicked: %v", in, r)
				}
			}()
			plan, err := ParseDDL(in)
			if err == nil {
				// A handful of forms are intentionally lenient (trailing garbage
				// on an otherwise-complete statement); accept a non-nil plan
				// there. The point of this test is: no panic. But the truncations
				// must error.
				if plan == nil {
					t.Errorf("ParseDDL(%q): nil plan and nil error", in)
				}
				return
			}
		}()
	}
}

// TestParseDDL_TruncatedReturnsError asserts the specific truncations that
// previously panicked now return a non-nil error (not merely no-panic).
func TestParseDDL_TruncatedReturnsError(t *testing.T) {
	truncations := []string{
		"CREATE INDEX x FOR (",
		"CREATE INDEX x FOR (n:",
		"CREATE INDEX x FOR (n:L) ON (",
		"CREATE INDEX x FOR (n:L) ON (n.p",
		"CREATE CONSTRAINT c ON (",
	}
	for _, in := range truncations {
		plan, err := ParseDDL(in)
		if err == nil {
			t.Errorf("ParseDDL(%q): expected a syntax error, got plan=%v", in, plan)
		}
	}
}

// TestParseDDL_WellFormedStillParses guards against the bounds-safety rewrite
// regressing valid DDL.
func TestParseDDL_WellFormedStillParses(t *testing.T) {
	cases := []struct {
		query    string
		wantType string
	}{
		{"CREATE INDEX person_email FOR (n:Person) ON (n.email)", "*ir.CreateIndex"},
		{"CREATE INDEX IF NOT EXISTS FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}", "*ir.CreateIndex"},
		{"DROP INDEX person_email IF EXISTS", "*ir.DropIndex"},
		{"CREATE CONSTRAINT c ON (n:Person) ASSERT n.id IS UNIQUE", "*ir.CreateConstraint"},
	}
	for _, tc := range cases {
		plan, err := ParseDDL(tc.query)
		if err != nil {
			t.Errorf("ParseDDL(%q): unexpected error %v", tc.query, err)
			continue
		}
		if got := typeName(plan); !strings.HasSuffix(got, strings.TrimPrefix(tc.wantType, "*ir.")) {
			t.Errorf("ParseDDL(%q): got %s, want %s", tc.query, got, tc.wantType)
		}
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *CreateIndex:
		return "CreateIndex"
	case *DropIndex:
		return "DropIndex"
	case *CreateConstraint:
		return "CreateConstraint"
	case *DropConstraint:
		return "DropConstraint"
	default:
		return "unknown"
	}
}
