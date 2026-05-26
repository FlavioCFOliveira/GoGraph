package expr_test

// value_test.go — unit tests for the Cypher runtime value model (task-233).
//
// Coverage targets:
//   - All 10 Kind values instantiated and round-trip through Kind().
//   - NULL singleton identity (Null == Null at the pointer level).
//   - Equality returns NULL per 3VL when either operand is Null.
//   - Cross-kind equality always returns false (never NULL for non-null values).
//   - Ordering: NULLs sort last; intra-type ordering is correct.
//   - Hash consistency: equal values have equal hashes.

import (
	"sort"
	"testing"

	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. Kind coverage — all 10 value kinds
// ─────────────────────────────────────────────────────────────────────────────

func TestValue_KindCoverage(t *testing.T) {
	cases := []struct {
		name string
		v    expr.Value
		kind expr.Kind
	}{
		{"Null", expr.Null, expr.KindNull},
		{"Integer", expr.IntegerValue(42), expr.KindInteger},
		{"Float", expr.FloatValue(3.14), expr.KindFloat},
		{"String", expr.StringValue("hello"), expr.KindString},
		{"Bool", expr.BoolValue(true), expr.KindBool},
		{"List", expr.ListValue{expr.IntegerValue(1)}, expr.KindList},
		{"Map", expr.MapValue{"k": expr.IntegerValue(1)}, expr.KindMap},
		{"Node", expr.NodeValue{ID: 1}, expr.KindNode},
		{"Relationship", expr.RelationshipValue{ID: 1}, expr.KindRelationship},
		{"Path", expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}}, expr.KindPath},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Kind(); got != tc.kind {
				t.Errorf("Kind() = %v, want %v", got, tc.kind)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. NULL singleton identity
// ─────────────────────────────────────────────────────────────────────────────

func TestNull_Singleton(t *testing.T) {
	// Two references to the Null sentinel must be the same interface value.
	n1 := expr.Null
	n2 := expr.Null
	if n1 != n2 {
		t.Error("Null singleton: n1 != n2")
	}
	if !expr.IsNull(n1) {
		t.Error("IsNull(Null) = false, want true")
	}
	if expr.IsNull(expr.IntegerValue(0)) {
		t.Error("IsNull(IntegerValue(0)) = true, want false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. 3VL equality — NULL operands always return NULL
// ─────────────────────────────────────────────────────────────────────────────

func TestEqual_NullPropagation(t *testing.T) {
	nonNullValues := []expr.Value{
		expr.IntegerValue(1),
		expr.FloatValue(1.0),
		expr.StringValue("x"),
		expr.BoolValue(true),
		expr.ListValue{},
		expr.MapValue{},
		expr.NodeValue{ID: 1},
		expr.RelationshipValue{ID: 1},
		expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}},
	}

	for _, v := range nonNullValues {
		t.Run(v.Kind().String()+"_eq_null", func(t *testing.T) {
			r := v.Equal(expr.Null)
			if !expr.IsNull(r) {
				t.Errorf("%v.Equal(Null) = %v, want Null", v, r)
			}
		})
		t.Run("null_eq_"+v.Kind().String(), func(t *testing.T) {
			r := expr.Null.Equal(v)
			if !expr.IsNull(r) {
				t.Errorf("Null.Equal(%v) = %v, want Null", v, r)
			}
		})
	}

	// NULL == NULL is also NULL per 3VL.
	t.Run("null_eq_null", func(t *testing.T) {
		nullCopy := expr.Null // use a separate variable to avoid gocritic dupArg
		if !expr.IsNull(expr.Null.Equal(nullCopy)) {
			t.Error("Null.Equal(Null) should be Null")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Equality — same-type and cross-type
// ─────────────────────────────────────────────────────────────────────────────

func TestEqual_SameType(t *testing.T) {
	cases := []struct {
		name string
		a, b expr.Value
		want bool
	}{
		{"int equal", expr.IntegerValue(7), expr.IntegerValue(7), true},
		{"int not equal", expr.IntegerValue(7), expr.IntegerValue(8), false},
		{"float equal", expr.FloatValue(1.5), expr.FloatValue(1.5), true},
		{"float not equal", expr.FloatValue(1.5), expr.FloatValue(2.5), false},
		{"string equal", expr.StringValue("a"), expr.StringValue("a"), true},
		{"string not equal", expr.StringValue("a"), expr.StringValue("b"), false},
		{"bool T==T", expr.BoolValue(true), expr.BoolValue(true), true},
		{"bool F==F", expr.BoolValue(false), expr.BoolValue(false), true},
		{"bool T!=F", expr.BoolValue(true), expr.BoolValue(false), false},
		{"list equal", expr.ListValue{expr.IntegerValue(1)}, expr.ListValue{expr.IntegerValue(1)}, true},
		{"list not equal", expr.ListValue{expr.IntegerValue(1)}, expr.ListValue{expr.IntegerValue(2)}, false},
		{"map equal", expr.MapValue{"k": expr.IntegerValue(1)}, expr.MapValue{"k": expr.IntegerValue(1)}, true},
		{"map not equal", expr.MapValue{"k": expr.IntegerValue(1)}, expr.MapValue{"k": expr.IntegerValue(2)}, false},
		{"node equal", expr.NodeValue{ID: 5}, expr.NodeValue{ID: 5}, true},
		{"node not equal", expr.NodeValue{ID: 5}, expr.NodeValue{ID: 6}, false},
		{"rel equal", expr.RelationshipValue{ID: 3}, expr.RelationshipValue{ID: 3}, true},
		{"rel not equal", expr.RelationshipValue{ID: 3}, expr.RelationshipValue{ID: 4}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.a.Equal(tc.b)
			if got := expr.IsTruthy(r); got != tc.want {
				t.Errorf("%v.Equal(%v) = %v (truthy=%v), want truthy=%v", tc.a, tc.b, r, got, tc.want)
			}
		})
	}
}

func TestEqual_CrossType(t *testing.T) {
	// Different non-null types must always return false (not null).
	a := expr.IntegerValue(1)
	b := expr.StringValue("1")
	r := a.Equal(b)
	if expr.IsNull(r) {
		t.Errorf("cross-type equal returned Null, want false")
	}
	if expr.IsTruthy(r) {
		t.Errorf("cross-type equal returned true, want false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. List equality with embedded NULL propagation
// ─────────────────────────────────────────────────────────────────────────────

func TestEqual_ListNullPropagation(t *testing.T) {
	a := expr.ListValue{expr.IntegerValue(1), expr.Null}
	b := expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}
	r := a.Equal(b)
	if !expr.IsNull(r) {
		t.Errorf("list equal with embedded null should return Null, got %v", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Hash consistency — equal values must have equal hashes
// ─────────────────────────────────────────────────────────────────────────────

func TestHash_Consistency(t *testing.T) {
	pairs := []struct {
		name string
		a, b expr.Value
	}{
		{"integer", expr.IntegerValue(99), expr.IntegerValue(99)},
		{"float", expr.FloatValue(2.718), expr.FloatValue(2.718)},
		{"string", expr.StringValue("gograph"), expr.StringValue("gograph")},
		{"bool", expr.BoolValue(false), expr.BoolValue(false)},
		{"list", expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}, expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}},
		{"map", expr.MapValue{"x": expr.IntegerValue(1)}, expr.MapValue{"x": expr.IntegerValue(1)}},
		{"node", expr.NodeValue{ID: 7}, expr.NodeValue{ID: 7}},
		{"rel", expr.RelationshipValue{ID: 8}, expr.RelationshipValue{ID: 8}},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			if ha, hb := tc.a.Hash(), tc.b.Hash(); ha != hb {
				t.Errorf("hash mismatch for equal %v values: %d != %d", tc.name, ha, hb)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Ordering — NULLs sort last
// ─────────────────────────────────────────────────────────────────────────────

func TestCompare_NullLast(t *testing.T) {
	values := []expr.Value{
		expr.Null,
		expr.IntegerValue(1),
		expr.Null,
		expr.StringValue("z"),
		expr.Null,
	}
	sort.Slice(values, func(i, j int) bool {
		return expr.Compare(values[i], values[j]) < 0
	})
	// Last three must be Null.
	for i := 2; i < 5; i++ {
		if !expr.IsNull(values[i]) {
			t.Errorf("position %d expected Null after sort, got %v", i, values[i])
		}
	}
}

func TestCompare_IntraType(t *testing.T) {
	cases := []struct {
		name string
		a, b expr.Value
		want int
	}{
		{"int less", expr.IntegerValue(1), expr.IntegerValue(2), -1},
		{"int equal", expr.IntegerValue(2), expr.IntegerValue(2), 0},
		{"int greater", expr.IntegerValue(3), expr.IntegerValue(2), 1},
		{"float less", expr.FloatValue(1.0), expr.FloatValue(2.0), -1},
		{"float equal", expr.FloatValue(2.0), expr.FloatValue(2.0), 0},
		{"string less", expr.StringValue("a"), expr.StringValue("b"), -1},
		{"string equal", expr.StringValue("b"), expr.StringValue("b"), 0},
		{"bool false<true", expr.BoolValue(false), expr.BoolValue(true), -1},
		{"bool equal", expr.BoolValue(true), expr.BoolValue(true), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expr.Compare(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("Compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompare_CrossType_Order(t *testing.T) {
	// openCypher 9 cross-type canonical order (non-null):
	// Path < Node < Relationship < Map < List < String < Boolean < Float < Integer
	path := expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}}
	node := expr.NodeValue{ID: 1}
	rel := expr.RelationshipValue{ID: 1}
	mapV := expr.MapValue{"a": expr.IntegerValue(1)}
	list := expr.ListValue{expr.IntegerValue(1)}
	str := expr.StringValue("x")
	boolV := expr.BoolValue(true)
	flt := expr.FloatValue(1.0)
	intV := expr.IntegerValue(1)

	// openCypher cross-type sort order: Map < Node < Relationship <
	// List < Path < String < Boolean < Float < Integer.
	ordered := []expr.Value{mapV, node, rel, list, path, str, boolV, flt, intV}
	for i := range ordered {
		for j := i + 1; j < len(ordered); j++ {
			if c := expr.Compare(ordered[i], ordered[j]); c >= 0 {
				t.Errorf("expected Compare(%v, %v) < 0, got %d", ordered[i].Kind(), ordered[j].Kind(), c)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. IsTruthy
// ─────────────────────────────────────────────────────────────────────────────

func TestIsTruthy(t *testing.T) {
	cases := []struct {
		v    expr.Value
		want bool
	}{
		{expr.BoolValue(true), true},
		{expr.BoolValue(false), false},
		{expr.Null, false},
		{expr.IntegerValue(1), false}, // non-bool is never truthy
		{expr.StringValue("true"), false},
	}
	for _, tc := range cases {
		if got := expr.IsTruthy(tc.v); got != tc.want {
			t.Errorf("IsTruthy(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. Path value — String and Equal
// ─────────────────────────────────────────────────────────────────────────────

func TestPathValue_Equal(t *testing.T) {
	n1 := expr.NodeValue{ID: 1}
	n2 := expr.NodeValue{ID: 2}
	r1 := expr.RelationshipValue{ID: 10, StartID: 1, EndID: 2}

	p1 := expr.PathValue{Nodes: []expr.NodeValue{n1, n2}, Relationships: []expr.RelationshipValue{r1}}
	p2 := expr.PathValue{Nodes: []expr.NodeValue{n1, n2}, Relationships: []expr.RelationshipValue{r1}}
	p3 := expr.PathValue{Nodes: []expr.NodeValue{n1}, Relationships: nil}

	if r := p1.Equal(p2); !expr.IsTruthy(r) {
		t.Errorf("equal paths: Equal = %v, want true", r)
	}
	if r := p1.Equal(p3); expr.IsTruthy(r) {
		t.Errorf("unequal paths: Equal = %v, want false", r)
	}
	if r := p1.Equal(expr.Null); !expr.IsNull(r) {
		t.Errorf("path.Equal(Null) = %v, want Null", r)
	}
}
