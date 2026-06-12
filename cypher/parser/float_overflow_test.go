package parser

import (
	"errors"
	"strings"
	"testing"
)

// TestFloatOverflowRaisesError asserts that exponent-form float literals that
// overflow IEEE-754 double precision produce a *SemaError with a message that
// indicates overflow — not a *ast.Variable with a numeric name, which was the
// pre-fix behaviour.
//
// The openCypher TCK Literals5 [27] covers "RETURN 1.34E999" and expects a
// FloatingPointOverflow compile-time error. The additional cases below cover
// the Symbol-branch path in VisitAtom that was the primary bug site.
func TestFloatOverflowRaisesError(t *testing.T) {
	t.Parallel()

	overflowing := []string{
		"RETURN 1e309",
		"RETURN 2e308",
		"RETURN 1e400",
		"RETURN 1.34E999",
	}

	for _, q := range overflowing {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(q)
			if err == nil {
				t.Fatalf("Parse(%q): expected error, got nil", q)
			}
			var se *SemaError
			if !errors.As(err, &se) {
				t.Fatalf("Parse(%q): expected *SemaError, got %T: %v", q, err, err)
			}
			// The message must signal overflow, not an unrelated variable error.
			if !strings.Contains(se.Message, "out of range") && !strings.Contains(se.Message, "overflow") {
				t.Errorf("Parse(%q): SemaError.Message=%q; want 'out of range' or 'overflow'", q, se.Message)
			}
		})
	}
}

// TestFloatOverflowMustReturnError asserts that Parse always returns a non-nil
// error for overflowing exponent literals. Before the fix, RETURN 1e309
// succeeded (returning an AST that evaluated the literal as the variable
// "1e309"), which is a silent wrong-result bug.
func TestFloatOverflowMustReturnError(t *testing.T) {
	t.Parallel()

	cases := []string{"1e309", "2e308", "1e400", "1.34E999"}

	for _, lit := range cases {
		lit := lit
		t.Run(lit, func(t *testing.T) {
			t.Parallel()
			q := "RETURN " + lit
			_, err := Parse(q)
			if err == nil {
				t.Errorf("Parse(%q): expected error (overflow), got nil — literal was silently misinterpreted", q)
			}
		})
	}
}

// TestValidLargeFloatParses verifies that float literals within IEEE-754 double
// range still parse successfully. The value 1.23456789e308 is taken from the
// openCypher TCK Literals5 set of accepted float literals.
func TestValidLargeFloatParses(t *testing.T) {
	t.Parallel()

	valid := []string{
		"RETURN 1.23456789e308",
		"RETURN 1.5e308",
		"RETURN 1.79e308",
	}

	for _, q := range valid {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(q)
			if err != nil {
				t.Errorf("Parse(%q): unexpected error %v", q, err)
			}
		})
	}
}
