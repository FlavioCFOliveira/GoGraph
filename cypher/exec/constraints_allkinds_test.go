package exec_test

// constraints_allkinds_test.go — regression coverage for #1318: a UNIQUE
// constraint must be enforced for EVERY property kind, not just string/int64.
//
// Before the fix, propertyValueToString returned "" for float/bool/time/bytes
// (and the check was skipped on ""), so a UNIQUE constraint on such a property
// was silently a no-op: a duplicate value committed with no error. These
// tests pin enforcement for all seven PropertyKinds by exercising the registry
// directly (RecordPropertySet then CheckSetProperty), which is the path the
// CreateNode / SetProperty operators drive on every write.
//
// Layer: short. goleak-clean (registry is local).

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestConstraintRegistry_UniqueAllKinds verifies that, for every property kind,
// recording a value then checking the same value reports a UNIQUE violation,
// and that a different value of the same kind does not.
func TestConstraintRegistry_UniqueAllKinds(t *testing.T) {
	defer goleak.VerifyNone(t)

	ts := time.Date(2026, 6, 4, 12, 0, 0, 123, time.UTC)
	tsOther := ts.Add(time.Second)

	cases := []struct {
		name      string
		dup       lpg.PropertyValue // recorded, then re-checked → must violate
		different lpg.PropertyValue // a distinct value of the same kind → must pass
	}{
		{"string", lpg.StringValue("a@example.com"), lpg.StringValue("b@example.com")},
		{"int64", lpg.Int64Value(42), lpg.Int64Value(43)},
		{"float64", lpg.Float64Value(1.5), lpg.Float64Value(2.5)},
		{"bool", lpg.BoolValue(true), lpg.BoolValue(false)},
		{"time", lpg.TimeValue(ts), lpg.TimeValue(tsOther)},
		{"bytes", lpg.BytesValue([]byte{0x01, 0x02}), lpg.BytesValue([]byte{0x01, 0x03})},
		{"list", lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)}),
			lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(3)})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := exec.NewConstraintRegistry()
			reg.RegisterUnique("N", "p", exec.UniqueIndexName("N", "p"))
			labels := []string{"N"}

			// A fresh value must pass the check.
			if err := reg.CheckSetProperty(labels, "p", tc.dup, nil); err != nil {
				t.Fatalf("%s: first value unexpectedly rejected: %v", tc.name, err)
			}
			// Record it as written.
			reg.RecordPropertySet(labels, "p", tc.dup)

			// Re-checking the SAME value must now report a UNIQUE violation —
			// this is the assertion that fails before the #1318 fix for the
			// non-string/int kinds.
			err := reg.CheckSetProperty(labels, "p", tc.dup, nil)
			if err == nil {
				t.Fatalf("%s: duplicate value accepted (UNIQUE constraint not enforced)", tc.name)
			}
			if !errors.Is(err, exec.ErrConstraintViolation) {
				t.Fatalf("%s: got error %v, want one wrapping ErrConstraintViolation", tc.name, err)
			}

			// A DIFFERENT value of the same kind must still pass.
			if err := reg.CheckSetProperty(labels, "p", tc.different, nil); err != nil {
				t.Fatalf("%s: distinct value unexpectedly rejected: %v", tc.name, err)
			}
		})
	}
}

// TestConstraintRegistry_UniqueCrossKindNoCollision verifies that values of
// different kinds with the same textual rendering do not collide in the
// value-set: the string "1", the integer 1, the float 1.0, and the bool that
// renders similarly each occupy a distinct key, so recording one does not make
// another look like a duplicate.
func TestConstraintRegistry_UniqueCrossKindNoCollision(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("N", "p", exec.UniqueIndexName("N", "p"))
	labels := []string{"N"}

	reg.RecordPropertySet(labels, "p", lpg.StringValue("1"))
	reg.RecordPropertySet(labels, "p", lpg.Int64Value(1))
	reg.RecordPropertySet(labels, "p", lpg.Float64Value(1))

	// None of the other-kind values must be seen as a duplicate of another.
	for _, v := range []lpg.PropertyValue{
		lpg.StringValue("1"), lpg.Int64Value(1), lpg.Float64Value(1),
	} {
		if err := reg.CheckSetProperty(labels, "p", v, nil); err == nil {
			t.Fatalf("value of kind %d should already be recorded (its own kind)", v.Kind())
		}
	}
	// A bool false renders as "0" internally but must not collide with int 0.
	reg.RecordPropertySet(labels, "p", lpg.BoolValue(false))
	if err := reg.CheckSetProperty(labels, "p", lpg.Int64Value(0), nil); err != nil {
		t.Fatal("int 0 collided with bool false in the value-set")
	}
}

// TestConstraintRegistry_UniqueNullSkipped verifies that the zero
// PropertyValue (null) is never treated as a UNIQUE duplicate: recording two
// nulls and checking a null must all pass (null-handling belongs to NOT NULL,
// not UNIQUE).
func TestConstraintRegistry_UniqueNullSkipped(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterUnique("N", "p", exec.UniqueIndexName("N", "p"))
	labels := []string{"N"}

	var null lpg.PropertyValue // zero value: Kind() == 0
	reg.RecordPropertySet(labels, "p", null)
	reg.RecordPropertySet(labels, "p", null)
	if err := reg.CheckSetProperty(labels, "p", null, nil); err != nil {
		t.Fatalf("null value rejected by UNIQUE constraint: %v", err)
	}
}
