package funcs_test

// essentials_test.go — golden tests for the built-in function registry (task-248).
//
// Tests cover: all 15+ essential functions, type errors, arity errors, NULL
// propagation, coalesce 3VL, range with negative step.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper
// ─────────────────────────────────────────────────────────────────────────────

func call(t *testing.T, name string, args ...expr.Value) (expr.Value, error) {
	t.Helper()
	fn, ok := funcs.DefaultRegistry.Resolve(name)
	if !ok {
		t.Fatalf("function %q not found in registry", name)
	}
	return fn(args)
}

func mustCall(t *testing.T, name string, args ...expr.Value) expr.Value {
	t.Helper()
	v, err := call(t, name, args...)
	if err != nil {
		t.Fatalf("%s(%v) unexpected error: %v", name, args, err)
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// id()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ID_Node(t *testing.T) {
	v := mustCall(t, "id", expr.NodeValue{ID: 42})
	if v != expr.IntegerValue(42) {
		t.Errorf("got %v, want 42", v)
	}
}

func TestFn_ID_Relationship(t *testing.T) {
	v := mustCall(t, "id", expr.RelationshipValue{ID: 7})
	if v != expr.IntegerValue(7) {
		t.Errorf("got %v, want 7", v)
	}
}

func TestFn_ID_Null(t *testing.T) {
	v := mustCall(t, "id", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("got %v, want null", v)
	}
}

func TestFn_ID_TypeError(t *testing.T) {
	_, err := call(t, "id", expr.StringValue("oops"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// labels()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Labels(t *testing.T) {
	node := expr.NodeValue{ID: 1, Labels: []string{"Person", "Employee"}}
	v := mustCall(t, "labels", node)
	lv, ok := v.(expr.ListValue)
	if !ok {
		t.Fatalf("got %T, want ListValue", v)
	}
	if len(lv) != 2 {
		t.Errorf("got %d labels, want 2", len(lv))
	}
}

func TestFn_Labels_Null(t *testing.T) {
	v := mustCall(t, "labels", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("got %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// type()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Type(t *testing.T) {
	rel := expr.RelationshipValue{ID: 1, Type: "KNOWS"}
	v := mustCall(t, "type", rel)
	if v != expr.StringValue("KNOWS") {
		t.Errorf("got %v, want KNOWS", v)
	}
}

func TestFn_Type_TypeError(t *testing.T) {
	_, err := call(t, "type", expr.NodeValue{ID: 1})
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// startNode() / endNode()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_StartNode(t *testing.T) {
	rel := expr.RelationshipValue{ID: 1, StartID: 10, EndID: 20}
	v := mustCall(t, "startnode", rel)
	nv, ok := v.(expr.NodeValue)
	if !ok || nv.ID != 10 {
		t.Errorf("got %v, want node#10", v)
	}
}

func TestFn_EndNode(t *testing.T) {
	rel := expr.RelationshipValue{ID: 1, StartID: 10, EndID: 20}
	v := mustCall(t, "endnode", rel)
	nv, ok := v.(expr.NodeValue)
	if !ok || nv.ID != 20 {
		t.Errorf("got %v, want node#20", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// keys()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Keys_Map(t *testing.T) {
	m := expr.MapValue{"a": expr.IntegerValue(1), "b": expr.IntegerValue(2)}
	v := mustCall(t, "keys", m)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Errorf("got %v, want list of 2 keys", v)
	}
}

func TestFn_Keys_Null(t *testing.T) {
	v := mustCall(t, "keys", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("got %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// properties()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Properties_Map(t *testing.T) {
	m := expr.MapValue{"x": expr.IntegerValue(1)}
	v := mustCall(t, "properties", m)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["x"] != expr.IntegerValue(1) {
		t.Errorf("x = %v, want 1", mv["x"])
	}
}

func TestFn_Properties_Node(t *testing.T) {
	node := expr.NodeValue{
		ID:         1,
		Properties: expr.MapValue{"name": expr.StringValue("Alice")},
	}
	v := mustCall(t, "properties", node)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["name"] != expr.StringValue("Alice") {
		t.Errorf("name = %v, want Alice", mv["name"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// coalesce() — 3VL
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Coalesce(t *testing.T) {
	t.Run("first_non_null", func(t *testing.T) {
		v := mustCall(t, "coalesce", expr.Null, expr.IntegerValue(42), expr.StringValue("x"))
		if v != expr.IntegerValue(42) {
			t.Errorf("got %v, want 42", v)
		}
	})
	t.Run("all_null", func(t *testing.T) {
		v := mustCall(t, "coalesce", expr.Null, expr.Null)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
	t.Run("empty_args_null", func(t *testing.T) {
		v := mustCall(t, "coalesce")
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
	t.Run("first_arg_non_null", func(t *testing.T) {
		v := mustCall(t, "coalesce", expr.BoolValue(false))
		if v != expr.BoolValue(false) {
			t.Errorf("got %v, want false (not null)", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// toString() / toInteger() / toFloat()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToString(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.IntegerValue(42), expr.StringValue("42")},
		{expr.FloatValue(3.14), expr.StringValue("3.14")},
		{expr.BoolValue(true), expr.StringValue("true")},
		{expr.StringValue("already"), expr.StringValue("already")},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "tostring", tc.input)
		if v != tc.want {
			t.Errorf("toString(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_ToInteger(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.IntegerValue(7), expr.IntegerValue(7)},
		{expr.FloatValue(3.9), expr.IntegerValue(3)},
		{expr.StringValue("42"), expr.IntegerValue(42)},
		{expr.StringValue("bad"), expr.Null},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "tointeger", tc.input)
		if v != tc.want {
			t.Errorf("toInteger(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_ToInteger_OverflowBoundary(t *testing.T) {
	t.Parallel()

	// float64(math.MaxInt64) rounds UP to 2^63, so the old `f > float64(MaxInt64)`
	// guard admitted 2^63 silently.  The fix uses `f >= 2^63`.

	t.Run("float_2^63_overflows", func(t *testing.T) {
		t.Parallel()
		const twoPow63 = 9223372036854775808.0 // 2^63
		_, err := call(t, "tointeger", expr.FloatValue(twoPow63))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow for toInteger(2^63 float), got nil")
		}
		var e *expr.EvalError
		if !errors.As(err, &e) {
			t.Fatalf("expected *expr.EvalError, got %T: %v", err, err)
		}
	})

	t.Run("float_MinInt64_valid", func(t *testing.T) {
		t.Parallel()
		// float64(math.MinInt64) == -2^63 exactly; int64(-2^63) == MinInt64. Must succeed.
		const minInt64Float = -9223372036854775808.0
		v, err := call(t, "tointeger", expr.FloatValue(minInt64Float))
		if err != nil {
			t.Fatalf("unexpected error for toInteger(-2^63 float): %v", err)
		}
		got, ok := v.(expr.IntegerValue)
		if !ok {
			t.Fatalf("expected IntegerValue, got %T", v)
		}
		const minInt64 = -9223372036854775808
		if int64(got) != minInt64 {
			t.Fatalf("expected MinInt64 (%d), got %d", int64(minInt64), int64(got))
		}
	})

	t.Run("string_2^63_overflows", func(t *testing.T) {
		t.Parallel()
		// "9223372036854775808" is 2^63 — fails ParseInt, falls to ParseFloat which
		// rounds to 2^63; the fixed guard catches it.
		_, err := call(t, "tointeger", expr.StringValue("9223372036854775808"))
		if err == nil {
			t.Fatal("expected ArithmeticOverflow for toInteger('2^63 string'), got nil")
		}
		var e *expr.EvalError
		if !errors.As(err, &e) {
			t.Fatalf("expected *expr.EvalError, got %T: %v", err, err)
		}
	})

	t.Run("string_MaxInt64_valid", func(t *testing.T) {
		t.Parallel()
		// "9223372036854775807" == MaxInt64 — succeeds via ParseInt fast path.
		v, err := call(t, "tointeger", expr.StringValue("9223372036854775807"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		const maxInt64 = 9223372036854775807
		if got := int64(v.(expr.IntegerValue)); got != maxInt64 {
			t.Fatalf("expected MaxInt64 (%d), got %d", int64(maxInt64), got)
		}
	})
}

func TestFn_ToFloat(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.FloatValue(2.5), expr.FloatValue(2.5)},
		{expr.IntegerValue(3), expr.FloatValue(3.0)},
		{expr.StringValue("1.5"), expr.FloatValue(1.5)},
		{expr.StringValue("bad"), expr.Null},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "tofloat", tc.input)
		if v != tc.want {
			t.Errorf("toFloat(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// abs()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Abs(t *testing.T) {
	t.Run("positive_int", func(t *testing.T) {
		v := mustCall(t, "abs", expr.IntegerValue(5))
		if v != expr.IntegerValue(5) {
			t.Errorf("got %v, want 5", v)
		}
	})
	t.Run("negative_int", func(t *testing.T) {
		v := mustCall(t, "abs", expr.IntegerValue(-5))
		if v != expr.IntegerValue(5) {
			t.Errorf("got %v, want 5", v)
		}
	})
	t.Run("float", func(t *testing.T) {
		v := mustCall(t, "abs", expr.FloatValue(-3.14))
		if v != expr.FloatValue(3.14) {
			t.Errorf("got %v, want 3.14", v)
		}
	})
	t.Run("null", func(t *testing.T) {
		v := mustCall(t, "abs", expr.Null)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// range()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Range(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		v := mustCall(t, "range", expr.IntegerValue(1), expr.IntegerValue(5))
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 5 {
			t.Errorf("range(1,5) length = %d, want 5", len(lv))
		}
		for i, elem := range lv {
			want := expr.IntegerValue(int64(i + 1))
			if elem != want {
				t.Errorf("[%d] got %v, want %v", i, elem, want)
			}
		}
	})

	t.Run("with_step", func(t *testing.T) {
		v := mustCall(t, "range", expr.IntegerValue(0), expr.IntegerValue(10), expr.IntegerValue(2))
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 6 {
			t.Errorf("range(0,10,2) length = %d, want 6", len(lv))
		}
	})

	t.Run("negative_step", func(t *testing.T) {
		v := mustCall(t, "range", expr.IntegerValue(5), expr.IntegerValue(1), expr.IntegerValue(-1))
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 5 {
			t.Errorf("range(5,1,-1) length = %d, want 5", len(lv))
		}
		// Should be [5,4,3,2,1].
		for i, elem := range lv {
			want := expr.IntegerValue(int64(5 - i))
			if elem != want {
				t.Errorf("[%d] got %v, want %v", i, elem, want)
			}
		}
	})

	t.Run("empty_range", func(t *testing.T) {
		v := mustCall(t, "range", expr.IntegerValue(5), expr.IntegerValue(1)) // start > end, step=1
		lv := v.(expr.ListValue)                                              //nolint:forcetypeassert // test only
		if len(lv) != 0 {
			t.Errorf("range(5,1) length = %d, want 0 (empty)", len(lv))
		}
	})

	t.Run("zero_step_error", func(t *testing.T) {
		_, err := call(t, "range", expr.IntegerValue(1), expr.IntegerValue(5), expr.IntegerValue(0))
		if err == nil {
			t.Fatal("expected error for zero step")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// head() / tail() / last()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Head_Tail_Last(t *testing.T) {
	lst := expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}

	t.Run("head", func(t *testing.T) {
		v := mustCall(t, "head", lst)
		if v != expr.IntegerValue(1) {
			t.Errorf("got %v, want 1", v)
		}
	})

	t.Run("tail", func(t *testing.T) {
		v := mustCall(t, "tail", lst)
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 2 {
			t.Errorf("tail length = %d, want 2", len(lv))
		}
		if lv[0] != expr.IntegerValue(2) || lv[1] != expr.IntegerValue(3) {
			t.Errorf("tail = %v, want [2,3]", lv)
		}
	})

	t.Run("last", func(t *testing.T) {
		v := mustCall(t, "last", lst)
		if v != expr.IntegerValue(3) {
			t.Errorf("got %v, want 3", v)
		}
	})

	t.Run("head_empty_null", func(t *testing.T) {
		v := mustCall(t, "head", expr.ListValue{})
		if !expr.IsNull(v) {
			t.Errorf("head([]) = %v, want null", v)
		}
	})

	t.Run("last_empty_null", func(t *testing.T) {
		v := mustCall(t, "last", expr.ListValue{})
		if !expr.IsNull(v) {
			t.Errorf("last([]) = %v, want null", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// size()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Size(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		v := mustCall(t, "size", expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)})
		if v != expr.IntegerValue(2) {
			t.Errorf("got %v, want 2", v)
		}
	})
	t.Run("string", func(t *testing.T) {
		v := mustCall(t, "size", expr.StringValue("hello"))
		if v != expr.IntegerValue(5) {
			t.Errorf("got %v, want 5", v)
		}
	})
	t.Run("null", func(t *testing.T) {
		v := mustCall(t, "size", expr.Null)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
	t.Run("type_error", func(t *testing.T) {
		_, err := call(t, "size", expr.IntegerValue(1))
		var te *funcs.TypeError
		if !errors.As(err, &te) {
			t.Fatalf("expected TypeError, got %T: %v", err, err)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// length()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Length(t *testing.T) {
	path := expr.PathValue{
		Nodes:         []expr.NodeValue{{ID: 1}, {ID: 2}},
		Relationships: []expr.RelationshipValue{{ID: 1}},
	}
	v := mustCall(t, "length", path)
	if v != expr.IntegerValue(1) {
		t.Errorf("got %v, want 1", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// nodes() / relationships()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Nodes_Relationships(t *testing.T) {
	path := expr.PathValue{
		Nodes:         []expr.NodeValue{{ID: 1}, {ID: 2}, {ID: 3}},
		Relationships: []expr.RelationshipValue{{ID: 10}, {ID: 11}},
	}

	t.Run("nodes", func(t *testing.T) {
		v := mustCall(t, "nodes", path)
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 3 {
			t.Errorf("nodes length = %d, want 3", len(lv))
		}
	})

	t.Run("relationships", func(t *testing.T) {
		v := mustCall(t, "relationships", path)
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 2 {
			t.Errorf("relationships length = %d, want 2", len(lv))
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// String functions
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_StringFunctions(t *testing.T) {
	t.Run("trim", func(t *testing.T) {
		v := mustCall(t, "trim", expr.StringValue("  hello  "))
		if v != expr.StringValue("hello") {
			t.Errorf("got %v, want hello", v)
		}
	})
	t.Run("toupper", func(t *testing.T) {
		v := mustCall(t, "toupper", expr.StringValue("hello"))
		if v != expr.StringValue("HELLO") {
			t.Errorf("got %v, want HELLO", v)
		}
	})
	t.Run("tolower", func(t *testing.T) {
		v := mustCall(t, "tolower", expr.StringValue("HELLO"))
		if v != expr.StringValue("hello") {
			t.Errorf("got %v, want hello", v)
		}
	})
	t.Run("substring", func(t *testing.T) {
		v := mustCall(t, "substring", expr.StringValue("hello world"), expr.IntegerValue(6))
		if v != expr.StringValue("world") {
			t.Errorf("got %v, want world", v)
		}
	})
	t.Run("substring_with_length", func(t *testing.T) {
		v := mustCall(t, "substring", expr.StringValue("hello"), expr.IntegerValue(1), expr.IntegerValue(3))
		if v != expr.StringValue("ell") {
			t.Errorf("got %v, want ell", v)
		}
	})
	t.Run("replace", func(t *testing.T) {
		v := mustCall(t, "replace", expr.StringValue("hello world"), expr.StringValue("world"), expr.StringValue("there"))
		if v != expr.StringValue("hello there") {
			t.Errorf("got %v, want hello there", v)
		}
	})
	t.Run("split", func(t *testing.T) {
		v := mustCall(t, "split", expr.StringValue("a,b,c"), expr.StringValue(","))
		lv := v.(expr.ListValue) //nolint:forcetypeassert // test only
		if len(lv) != 3 {
			t.Errorf("split length = %d, want 3", len(lv))
		}
	})
	t.Run("reverse_string", func(t *testing.T) {
		v := mustCall(t, "reverse", expr.StringValue("hello"))
		if v != expr.StringValue("olleh") {
			t.Errorf("got %v, want olleh", v)
		}
	})
	t.Run("left", func(t *testing.T) {
		v := mustCall(t, "left", expr.StringValue("hello"), expr.IntegerValue(3))
		if v != expr.StringValue("hel") {
			t.Errorf("got %v, want hel", v)
		}
	})
	t.Run("right", func(t *testing.T) {
		v := mustCall(t, "right", expr.StringValue("hello"), expr.IntegerValue(3))
		if v != expr.StringValue("llo") {
			t.Errorf("got %v, want llo", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Arity errors
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ArityError(t *testing.T) {
	_, err := call(t, "abs") // requires exactly 1 argument
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("expected ArityError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// count() stub
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Count(t *testing.T) {
	t.Run("non_null", func(t *testing.T) {
		v := mustCall(t, "count", expr.IntegerValue(1))
		if v != expr.IntegerValue(1) {
			t.Errorf("got %v, want 1", v)
		}
	})
	t.Run("null", func(t *testing.T) {
		v := mustCall(t, "count", expr.Null)
		if v != expr.IntegerValue(0) {
			t.Errorf("got %v, want 0", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry.Register panics on duplicate
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistry_Duplicate_Panics(t *testing.T) {
	r := funcs.NewRegistry()
	r.Register("myfn", func(_ []expr.Value) (expr.Value, error) { return expr.Null, nil })
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	r.Register("myfn", func(_ []expr.Value) (expr.Value, error) { return expr.Null, nil })
}
