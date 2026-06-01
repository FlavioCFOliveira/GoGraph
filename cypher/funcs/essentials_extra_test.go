package funcs_test

// essentials_extra_test.go — supplementary tests to bring cypher/funcs above
// the 75% coverage gate. Targets uncovered built-in functions.

import (
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// TypeError.Error() / ArityError.Error()
// ─────────────────────────────────────────────────────────────────────────────

func TestError_TypeError_Message(t *testing.T) {
	_, err := call(t, "id", expr.StringValue("oops"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
	if te.Error() == "" {
		t.Error("TypeError.Error() should not be empty")
	}
}

func TestError_ArityError_Message(t *testing.T) {
	_, err := call(t, "abs")
	var ae *funcs.ArityError
	if !errors.As(err, &ae) {
		t.Fatalf("expected ArityError, got %T", err)
	}
	if ae.Error() == "" {
		t.Error("ArityError.Error() should not be empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// toBoolean()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToBoolean(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.BoolValue(true), expr.BoolValue(true)},
		{expr.BoolValue(false), expr.BoolValue(false)},
		{expr.StringValue("true"), expr.BoolValue(true)},
		{expr.StringValue("TRUE"), expr.BoolValue(true)},
		{expr.StringValue("false"), expr.BoolValue(false)},
		{expr.StringValue("FALSE"), expr.BoolValue(false)},
		{expr.StringValue("maybe"), expr.Null},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "toboolean", tc.input)
		if v != tc.want {
			t.Errorf("toBoolean(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_ToBoolean_TypeError(t *testing.T) {
	_, err := call(t, "toboolean", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ceil() / floor() / round() / sqrt() / sign()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Ceil(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.FloatValue(2.3), expr.FloatValue(3.0)},
		{expr.FloatValue(-1.7), expr.FloatValue(-1.0)},
		{expr.IntegerValue(5), expr.IntegerValue(5)},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "ceil", tc.input)
		if v != tc.want {
			t.Errorf("ceil(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_Ceil_TypeError(t *testing.T) {
	_, err := call(t, "ceil", expr.StringValue("x"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

func TestFn_Floor(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.FloatValue(2.9), expr.FloatValue(2.0)},
		{expr.FloatValue(-1.1), expr.FloatValue(-2.0)},
		{expr.IntegerValue(5), expr.IntegerValue(5)},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "floor", tc.input)
		if v != tc.want {
			t.Errorf("floor(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_Floor_TypeError(t *testing.T) {
	_, err := call(t, "floor", expr.StringValue("x"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

func TestFn_Round(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.FloatValue(2.5), expr.FloatValue(3.0)},
		{expr.FloatValue(2.4), expr.FloatValue(2.0)},
		{expr.IntegerValue(7), expr.IntegerValue(7)},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "round", tc.input)
		if v != tc.want {
			t.Errorf("round(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_Round_TypeError(t *testing.T) {
	_, err := call(t, "round", expr.StringValue("x"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

func TestFn_Sqrt(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.FloatValue(4.0), expr.FloatValue(2.0)},
		{expr.IntegerValue(9), expr.FloatValue(3.0)},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "sqrt", tc.input)
		if v != tc.want {
			t.Errorf("sqrt(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_Sqrt_TypeError(t *testing.T) {
	_, err := call(t, "sqrt", expr.StringValue("x"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

func TestFn_Sign(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.IntegerValue(5), expr.IntegerValue(1)},
		{expr.IntegerValue(-3), expr.IntegerValue(-1)},
		{expr.IntegerValue(0), expr.IntegerValue(0)},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "sign", tc.input)
		if v != tc.want {
			t.Errorf("sign(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_Sign_Float(t *testing.T) {
	// sign(float) returns FloatValue(±1.0) via math.Copysign.
	v := mustCall(t, "sign", expr.FloatValue(-2.5))
	fv, ok := v.(expr.FloatValue)
	if !ok {
		t.Fatalf("sign(float) = %T, want FloatValue", v)
	}
	if float64(fv) != -1.0 {
		t.Errorf("sign(-2.5) = %v, want -1.0", fv)
	}
}

func TestFn_Sign_TypeError(t *testing.T) {
	_, err := call(t, "sign", expr.StringValue("x"))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ltrim() / rtrim()
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_LTrim(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.StringValue("  hello"), expr.StringValue("hello")},
		{expr.StringValue("hello  "), expr.StringValue("hello  ")},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "ltrim", tc.input)
		if v != tc.want {
			t.Errorf("ltrim(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_LTrim_TypeError(t *testing.T) {
	_, err := call(t, "ltrim", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

func TestFn_RTrim(t *testing.T) {
	tests := []struct {
		input expr.Value
		want  expr.Value
	}{
		{expr.StringValue("hello  "), expr.StringValue("hello")},
		{expr.StringValue("  hello"), expr.StringValue("  hello")},
		{expr.Null, expr.Null},
	}
	for _, tc := range tests {
		v := mustCall(t, "rtrim", tc.input)
		if v != tc.want {
			t.Errorf("rtrim(%v) = %v, want %v", tc.input, v, tc.want)
		}
	}
}

func TestFn_RTrim_TypeError(t *testing.T) {
	_, err := call(t, "rtrim", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// keys() and properties() — remaining branches
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Keys_Node(t *testing.T) {
	node := expr.NodeValue{
		ID:         1,
		Properties: expr.MapValue{"name": expr.StringValue("Alice"), "age": expr.IntegerValue(30)},
	}
	v := mustCall(t, "keys", node)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Errorf("keys(node) = %v, want list of 2", v)
	}
}

func TestFn_Keys_NodeNilProperties(t *testing.T) {
	node := expr.NodeValue{ID: 1}
	v := mustCall(t, "keys", node)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("keys(node without props) = %v, want empty list", v)
	}
}

func TestFn_Keys_Relationship(t *testing.T) {
	rel := expr.RelationshipValue{
		ID:         1,
		Properties: expr.MapValue{"weight": expr.FloatValue(1.5)},
	}
	v := mustCall(t, "keys", rel)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 1 {
		t.Errorf("keys(rel) = %v, want list of 1", v)
	}
}

func TestFn_Keys_RelationshipNilProperties(t *testing.T) {
	rel := expr.RelationshipValue{ID: 1}
	v := mustCall(t, "keys", rel)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("keys(rel without props) = %v, want empty list", v)
	}
}

func TestFn_Keys_TypeError(t *testing.T) {
	_, err := call(t, "keys", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

func TestFn_Properties_Relationship(t *testing.T) {
	rel := expr.RelationshipValue{
		ID:         1,
		Properties: expr.MapValue{"since": expr.IntegerValue(2021)},
	}
	v := mustCall(t, "properties", rel)
	mv, ok := v.(expr.MapValue)
	if !ok || mv["since"] != expr.IntegerValue(2021) {
		t.Errorf("properties(rel) = %v, want {since: 2021}", v)
	}
}

func TestFn_Properties_NodeNilProps(t *testing.T) {
	node := expr.NodeValue{ID: 1}
	v := mustCall(t, "properties", node)
	mv, ok := v.(expr.MapValue)
	if !ok || len(mv) != 0 {
		t.Errorf("properties(node without props) = %v, want empty map", v)
	}
}

func TestFn_Properties_RelNilProps(t *testing.T) {
	rel := expr.RelationshipValue{ID: 1}
	v := mustCall(t, "properties", rel)
	mv, ok := v.(expr.MapValue)
	if !ok || len(mv) != 0 {
		t.Errorf("properties(rel without props) = %v, want empty map", v)
	}
}

func TestFn_Properties_TypeError(t *testing.T) {
	_, err := call(t, "properties", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// length() — list and string branches
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Length_List(t *testing.T) {
	v := mustCall(t, "length", expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)})
	if v != expr.IntegerValue(3) {
		t.Errorf("length(list) = %v, want 3", v)
	}
}

func TestFn_Length_String(t *testing.T) {
	v := mustCall(t, "length", expr.StringValue("hello"))
	if v != expr.IntegerValue(5) {
		t.Errorf("length(string) = %v, want 5", v)
	}
}

func TestFn_Length_Null(t *testing.T) {
	v := mustCall(t, "length", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("length(null) = %v, want null", v)
	}
}

func TestFn_Length_TypeError(t *testing.T) {
	_, err := call(t, "length", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// head() / tail() / last() — null and type-error branches
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Head_Null(t *testing.T) {
	v := mustCall(t, "head", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("head(null) = %v, want null", v)
	}
}

func TestFn_Head_TypeError(t *testing.T) {
	_, err := call(t, "head", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

func TestFn_Tail_Null(t *testing.T) {
	v := mustCall(t, "tail", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("tail(null) = %v, want null", v)
	}
}

func TestFn_Tail_TypeError(t *testing.T) {
	_, err := call(t, "tail", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

func TestFn_Tail_Empty(t *testing.T) {
	v := mustCall(t, "tail", expr.ListValue{})
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("tail([]) = %v, want empty list", v)
	}
}

func TestFn_Last_Null(t *testing.T) {
	v := mustCall(t, "last", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("last(null) = %v, want null", v)
	}
}

func TestFn_Last_TypeError(t *testing.T) {
	_, err := call(t, "last", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// reverse() — list branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Reverse_List(t *testing.T) {
	lst := expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}
	v := mustCall(t, "reverse", lst)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("reverse(list) = %v (%T), want ListValue of 3", v, v)
	}
	if lv[0] != expr.IntegerValue(3) || lv[1] != expr.IntegerValue(2) || lv[2] != expr.IntegerValue(1) {
		t.Errorf("reverse(list) = %v, want [3, 2, 1]", lv)
	}
}

func TestFn_Reverse_TypeError(t *testing.T) {
	_, err := call(t, "reverse", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

func TestFn_Reverse_Null(t *testing.T) {
	v := mustCall(t, "reverse", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("reverse(null) = %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// startNode() / endNode() — null and type-error branches
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_StartNode_Null(t *testing.T) {
	v := mustCall(t, "startnode", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("startNode(null) = %v, want null", v)
	}
}

func TestFn_StartNode_TypeError(t *testing.T) {
	_, err := call(t, "startnode", expr.NodeValue{ID: 1})
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

func TestFn_EndNode_Null(t *testing.T) {
	v := mustCall(t, "endnode", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("endNode(null) = %v, want null", v)
	}
}

func TestFn_EndNode_TypeError(t *testing.T) {
	_, err := call(t, "endnode", expr.NodeValue{ID: 1})
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// nodes() / relationships() — null and type-error branches
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Nodes_Null(t *testing.T) {
	v := mustCall(t, "nodes", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("nodes(null) = %v, want null", v)
	}
}

func TestFn_Nodes_TypeError(t *testing.T) {
	_, err := call(t, "nodes", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

func TestFn_Relationships_Null(t *testing.T) {
	v := mustCall(t, "relationships", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("relationships(null) = %v, want null", v)
	}
}

func TestFn_Relationships_TypeError(t *testing.T) {
	_, err := call(t, "relationships", expr.IntegerValue(1))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// toString() — ListValue type error branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToString_TypeError(t *testing.T) {
	_, err := call(t, "tostring", expr.ListValue{})
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError for tostring(list), got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// toInteger() — type error branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToInteger_TypeError(t *testing.T) {
	_, err := call(t, "tointeger", expr.ListValue{})
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError for tointeger(list), got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// toFloat() — type error branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToFloat_TypeError(t *testing.T) {
	_, err := call(t, "tofloat", expr.ListValue{})
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError for tofloat(list), got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// type() — null branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Type_Null(t *testing.T) {
	v := mustCall(t, "type", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("type(null) = %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// left() / right() — edge cases
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Left_Null(t *testing.T) {
	v := mustCall(t, "left", expr.Null, expr.IntegerValue(3))
	if !expr.IsNull(v) {
		t.Errorf("left(null) = %v, want null", v)
	}
}

func TestFn_Left_NegativeN(t *testing.T) {
	// Negative length → empty string.
	v := mustCall(t, "left", expr.StringValue("hello"), expr.IntegerValue(-1))
	if v != expr.StringValue("") {
		t.Errorf("left(hello, -1) = %v, want empty string", v)
	}
}

func TestFn_Left_ExceedsLength(t *testing.T) {
	// Length > string length → full string.
	v := mustCall(t, "left", expr.StringValue("hi"), expr.IntegerValue(100))
	if v != expr.StringValue("hi") {
		t.Errorf("left(hi, 100) = %v, want hi", v)
	}
}

func TestFn_Right_Null(t *testing.T) {
	v := mustCall(t, "right", expr.Null, expr.IntegerValue(3))
	if !expr.IsNull(v) {
		t.Errorf("right(null) = %v, want null", v)
	}
}

func TestFn_Right_NegativeN(t *testing.T) {
	v := mustCall(t, "right", expr.StringValue("hello"), expr.IntegerValue(-1))
	if v != expr.StringValue("") {
		t.Errorf("right(hello, -1) = %v, want empty string", v)
	}
}

func TestFn_Right_ExceedsLength(t *testing.T) {
	v := mustCall(t, "right", expr.StringValue("hi"), expr.IntegerValue(100))
	if v != expr.StringValue("hi") {
		t.Errorf("right(hi, 100) = %v, want hi", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// substring() — edge cases
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Substring_Null(t *testing.T) {
	v := mustCall(t, "substring", expr.Null, expr.IntegerValue(0))
	if !expr.IsNull(v) {
		t.Errorf("substring(null) = %v, want null", v)
	}
}

func TestFn_Substring_ZeroStart(t *testing.T) {
	v := mustCall(t, "substring", expr.StringValue("hello"), expr.IntegerValue(0))
	if v != expr.StringValue("hello") {
		t.Errorf("substring(hello, 0) = %v, want hello", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// size() — map branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Size_Map(t *testing.T) {
	m := expr.MapValue{"a": expr.IntegerValue(1), "b": expr.IntegerValue(2), "c": expr.IntegerValue(3)}
	v := mustCall(t, "size", m)
	if v != expr.IntegerValue(3) {
		t.Errorf("size(map) = %v, want 3", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// toupper() / tolower() — null branches
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToUpper_Null(t *testing.T) {
	v := mustCall(t, "toupper", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("toUpper(null) = %v, want null", v)
	}
}

func TestFn_ToLower_Null(t *testing.T) {
	v := mustCall(t, "tolower", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("toLower(null) = %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// trim() — null branch
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Trim_Null(t *testing.T) {
	v := mustCall(t, "trim", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("trim(null) = %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// range() — null argument
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_Range_NullArg(t *testing.T) {
	v := mustCall(t, "range", expr.Null, expr.IntegerValue(5))
	if !expr.IsNull(v) {
		t.Errorf("range(null, 5) = %v, want null", v)
	}
}

func TestFn_Range_TypeError(t *testing.T) {
	_, err := call(t, "range", expr.StringValue("a"), expr.IntegerValue(5))
	var te *funcs.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected TypeError, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// toString() — temporal types
// ─────────────────────────────────────────────────────────────────────────────

func TestFn_ToString_Temporal(t *testing.T) {
	tests := []struct {
		name  string
		input expr.Value
		want  string
	}{
		{
			name:  "Date",
			input: expr.NewDate(2020, 1, 15),
			want:  "2020-01-15",
		},
		{
			name:  "LocalDateTime",
			input: expr.NewLocalDateTime(2020, 1, 15, 12, 30, 45, 0),
			want:  "2020-01-15T12:30:45",
		},
		{
			name:  "LocalDateTimeWithNanos",
			input: expr.NewLocalDateTime(2020, 1, 15, 12, 30, 45, 123_000_000),
			want:  "2020-01-15T12:30:45.123",
		},
		{
			name:  "DateTimeUTC",
			input: expr.NewDateTime(2020, 1, 15, 12, 30, 45, 0, nil),
			want:  "2020-01-15T12:30:45Z",
		},
		{
			name:  "DateTimeWithOffset",
			input: expr.NewDateTime(2020, 1, 15, 12, 30, 45, 0, time.FixedZone("+01:00", 3600)),
			want:  "2020-01-15T12:30:45+01:00",
		},
		{
			name:  "LocalTime",
			input: expr.NewLocalTime(12, 30, 45, 0),
			want:  "12:30:45",
		},
		{
			name:  "LocalTimeNoSeconds",
			input: expr.NewLocalTime(9, 0, 0, 0),
			want:  "09:00",
		},
		{
			name:  "TimeUTC",
			input: expr.NewTime(12, 30, 45, 0, 0),
			want:  "12:30:45Z",
		},
		{
			name:  "TimeWithOffset",
			input: expr.NewTime(12, 30, 45, 0, 3600),
			want:  "12:30:45+01:00",
		},
		{
			name:  "DurationZero",
			input: expr.NewDuration(0, 0, 0, 0),
			want:  "PT0S",
		},
		{
			name:  "DurationYearMonth",
			input: expr.NewDuration(14, 0, 0, 0), // 1Y2M
			want:  "P1Y2M",
		},
		{
			name:  "DurationDays",
			input: expr.NewDuration(0, 3, 0, 0),
			want:  "P3D",
		},
		{
			name:  "DurationHoursMinutesSeconds",
			input: expr.NewDuration(0, 0, 4*3600+5*60+6, 0),
			want:  "PT4H5M6S",
		},
		{
			name:  "DurationFractionalSeconds",
			input: expr.NewDuration(0, 0, 6, 789_000_000),
			want:  "PT6.789S",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			v := mustCall(t, "tostring", tc.input)
			if got, ok := v.(expr.StringValue); !ok || string(got) != tc.want {
				t.Errorf("toString(%v) = %v, want %q", tc.input, v, tc.want)
			}
		})
	}
}
