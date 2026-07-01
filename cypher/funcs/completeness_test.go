package funcs_test

// completeness_test.go — unit tests for the openCypher completeness built-ins
// added in audit finding F5 (#1832): elementId, isNaN, and the toXList family.
// timestamp() and randomUUID() are exercised end-to-end (statement-freezing and
// UUID shape) in the cypher package.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestFn_ElementID(t *testing.T) {
	t.Run("node", func(t *testing.T) {
		v := mustCall(t, "elementId", expr.NodeValue{ID: 42})
		if v != expr.StringValue("42") {
			t.Errorf("elementId(node 42) = %v, want \"42\"", v)
		}
	})
	t.Run("relationship", func(t *testing.T) {
		v := mustCall(t, "elementId", expr.RelationshipValue{ID: 7})
		if v != expr.StringValue("7") {
			t.Errorf("elementId(rel 7) = %v, want \"7\"", v)
		}
	})
	t.Run("null", func(t *testing.T) {
		if v := mustCall(t, "elementId", expr.Null); !expr.IsNull(v) {
			t.Errorf("elementId(null) = %v, want null", v)
		}
	})
	t.Run("type_error", func(t *testing.T) {
		if _, err := call(t, "elementId", expr.IntegerValue(1)); err == nil {
			t.Error("elementId(integer) should be a type error")
		}
	})
}

func TestFn_IsNaN(t *testing.T) {
	nan := expr.FloatValue(math.NaN())
	cases := []struct {
		name string
		in   expr.Value
		want expr.Value
	}{
		{"nan", nan, expr.BoolValue(true)},
		{"finite_float", expr.FloatValue(1.5), expr.BoolValue(false)},
		{"integer", expr.IntegerValue(3), expr.BoolValue(false)},
		{"null", expr.Null, expr.Null},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := mustCall(t, "isNaN", tc.in); v != tc.want {
				t.Errorf("isNaN(%v) = %v, want %v", tc.in, v, tc.want)
			}
		})
	}
	t.Run("type_error", func(t *testing.T) {
		if _, err := call(t, "isNaN", expr.StringValue("x")); err == nil {
			t.Error("isNaN(string) should be a type error")
		}
	})
}

func TestFn_ToIntegerList(t *testing.T) {
	in := expr.ListValue{expr.IntegerValue(1), expr.StringValue("2"), expr.StringValue("x"), expr.FloatValue(3.9)}
	v := mustCall(t, "toIntegerList", in)
	got, ok := v.(expr.ListValue)
	if !ok {
		t.Fatalf("toIntegerList returned %T, want ListValue", v)
	}
	want := []expr.Value{expr.IntegerValue(1), expr.IntegerValue(2), expr.Null, expr.IntegerValue(3)}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("elem %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFn_ToStringList(t *testing.T) {
	in := expr.ListValue{expr.IntegerValue(1), expr.BoolValue(true), expr.ListValue{expr.IntegerValue(9)}}
	v := mustCall(t, "toStringList", in)
	got := v.(expr.ListValue)
	// A nested list is not convertible to string -> null (per openCypher).
	want := []expr.Value{expr.StringValue("1"), expr.StringValue("true"), expr.Null}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("elem %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFn_ToFloatBooleanList(t *testing.T) {
	fv := mustCall(t, "toFloatList", expr.ListValue{expr.IntegerValue(2), expr.StringValue("bad")}).(expr.ListValue)
	if fv[0] != expr.FloatValue(2) || !expr.IsNull(fv[1]) {
		t.Errorf("toFloatList = %v, want [2.0, null]", fv)
	}
	bv := mustCall(t, "toBooleanList", expr.ListValue{expr.StringValue("true"), expr.StringValue("nope")}).(expr.ListValue)
	if bv[0] != expr.BoolValue(true) || !expr.IsNull(bv[1]) {
		t.Errorf("toBooleanList = %v, want [true, null]", bv)
	}
}

func TestFn_ToIntegerList_Null(t *testing.T) {
	if v := mustCall(t, "toIntegerList", expr.Null); !expr.IsNull(v) {
		t.Errorf("toIntegerList(null) = %v, want null", v)
	}
}

func TestFn_ToIntegerList_NonListTypeError(t *testing.T) {
	if _, err := call(t, "toIntegerList", expr.IntegerValue(1)); err == nil {
		t.Error("toIntegerList(integer) should be a type error")
	}
}
