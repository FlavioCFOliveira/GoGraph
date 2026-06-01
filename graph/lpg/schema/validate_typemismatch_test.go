package schema

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSchema_TypeMismatch verifies that schema.Validate returns an error
// wrapping ErrTypeMismatch for every (declared kind, wrong value kind) pair,
// that the error message contains both the declared and observed kind, and
// that the correct value produces no error.
func TestSchema_TypeMismatch(t *testing.T) {
	t.Parallel()

	// allValues holds one canonical value for each PropertyKind, indexed
	// by the kind constant (1-based: PropString=1 … PropBytes=6).
	allValues := map[lpg.PropertyKind]lpg.PropertyValue{
		lpg.PropString:  lpg.StringValue("text"),
		lpg.PropInt64:   lpg.Int64Value(42),
		lpg.PropFloat64: lpg.Float64Value(3.14),
		lpg.PropBool:    lpg.BoolValue(true),
		lpg.PropTime:    lpg.TimeValue(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		lpg.PropBytes:   lpg.BytesValue([]byte{0xDE, 0xAD}),
	}

	// allKinds is the ordered list of all kinds used to derive the five
	// wrong values for each row.
	allKinds := []lpg.PropertyKind{
		lpg.PropString,
		lpg.PropInt64,
		lpg.PropFloat64,
		lpg.PropBool,
		lpg.PropTime,
		lpg.PropBytes,
	}

	type row struct {
		name     string
		declared lpg.PropertyKind
	}

	rows := []row{
		{"PropString", lpg.PropString},
		{"PropInt64", lpg.PropInt64},
		{"PropFloat64", lpg.PropFloat64},
		{"PropBool", lpg.PropBool},
		{"PropTime", lpg.PropTime},
		{"PropBytes", lpg.PropBytes},
	}

	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()

			s := New(nil, nil)
			if _, err := s.RegisterProperty("prop", r.declared); err != nil {
				t.Fatalf("RegisterProperty: %v", err)
			}

			// Happy path: the correct kind must produce nil.
			correct := allValues[r.declared]
			if err := s.Validate("prop", correct); err != nil {
				t.Errorf("correct value: unexpected error: %v", err)
			}

			// Rejection path: every other kind must produce ErrTypeMismatch.
			for _, wrongKind := range allKinds {
				if wrongKind == r.declared {
					continue
				}
				wrongVal := allValues[wrongKind]
				err := s.Validate("prop", wrongVal)

				if !errors.Is(err, ErrTypeMismatch) {
					t.Errorf("declared=%d wrong=%d: got %v, want errors.Is(_, ErrTypeMismatch)",
						r.declared, wrongKind, err)
					continue
				}

				// The error message must contain the numeric representation of
				// both the declared kind and the observed kind. PropertyKind has
				// no String() method, so %d is the canonical representation used
				// by schema.Validate.
				msg := err.Error()
				wantDeclared := fmt.Sprintf("%d", r.declared)
				wantObserved := fmt.Sprintf("%d", wrongKind)
				if !strings.Contains(msg, wantDeclared) {
					t.Errorf("declared=%d wrong=%d: error %q does not contain declared kind %q",
						r.declared, wrongKind, msg, wantDeclared)
				}
				if !strings.Contains(msg, wantObserved) {
					t.Errorf("declared=%d wrong=%d: error %q does not contain observed kind %q",
						r.declared, wrongKind, msg, wantObserved)
				}
			}
		})
	}
}
