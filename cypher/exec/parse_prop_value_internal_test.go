package exec

// parse_prop_value_internal_test.go — package-internal unit tests for
// [parsePropValue] and [parsePropList], covering list literals (T957).

import (
	"errors"
	"testing"

	"gograph/graph/lpg"
)

// TestParsePropValue_ListLiterals verifies that parsePropValue correctly handles
// list literals of integer, string, float, boolean, mixed, and negative elements.
func TestParsePropValue_ListLiterals(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    lpg.PropertyValue
		wantErr bool
	}{
		{
			name:  "empty list",
			input: "[]",
			want:  lpg.ListValue(nil),
		},
		{
			name:  "integer list",
			input: "[1, 2, 3]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2), lpg.Int64Value(3)}),
		},
		{
			name:  "integer list no spaces",
			input: "[1,2,3,4,5,6,7]",
			want: lpg.ListValue([]lpg.PropertyValue{
				lpg.Int64Value(1), lpg.Int64Value(2), lpg.Int64Value(3),
				lpg.Int64Value(4), lpg.Int64Value(5), lpg.Int64Value(6),
				lpg.Int64Value(7),
			}),
		},
		{
			name:  "string list single quote",
			input: "['A', 'B']",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("A"), lpg.StringValue("B")}),
		},
		{
			name:  "string list double quote",
			input: `["foo", "bar"]`,
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("foo"), lpg.StringValue("bar")}),
		},
		{
			name:  "float list",
			input: "[1.5, 2.5, 3.14]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Float64Value(1.5), lpg.Float64Value(2.5), lpg.Float64Value(3.14)}),
		},
		{
			name:  "boolean list",
			input: "[true, false, true]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.BoolValue(true), lpg.BoolValue(false), lpg.BoolValue(true)}),
		},
		{
			name:  "negative integer in list",
			input: "[1, -2, 3]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(-2), lpg.Int64Value(3)}),
		},
		{
			name:  "all negative",
			input: "[-1, -2, -3]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(-1), lpg.Int64Value(-2), lpg.Int64Value(-3)}),
		},
		{
			name:  "single element list",
			input: "[42]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(42)}),
		},
		{
			name:  "null inside list is dropped",
			input: "[1, null, 3]",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(3)}),
		},
		{
			name:  "string with comma inside",
			input: `['hello, world', 'foo']`,
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("hello, world"), lpg.StringValue("foo")}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePropValue(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePropValue(%q) = %v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePropValue(%q) error: %v", tc.input, err)
			}
			if !propValueDeepEqual(got, tc.want) {
				t.Fatalf("parsePropValue(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestParsePropLiteral_ListProperty verifies that a full property-map literal
// containing a list value parses correctly via parsePropLiteral.
func TestParsePropLiteral_ListProperty(t *testing.T) {
	cases := []struct {
		name  string
		input string
		key   string
		want  lpg.PropertyValue
	}{
		{
			name:  "seasons",
			input: "{seasons: [1, 2, 3, 4, 5, 6, 7]}",
			key:   "seasons",
			want: lpg.ListValue([]lpg.PropertyValue{
				lpg.Int64Value(1), lpg.Int64Value(2), lpg.Int64Value(3),
				lpg.Int64Value(4), lpg.Int64Value(5), lpg.Int64Value(6),
				lpg.Int64Value(7),
			}),
		},
		{
			name:  "string list property",
			input: "{list: ['A', 'B']}",
			key:   "list",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("A"), lpg.StringValue("B")}),
		},
		{
			name:  "integer list no spaces",
			input: "{a: [1,2,3]}",
			key:   "a",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2), lpg.Int64Value(3)}),
		},
		{
			name:  "list alongside scalar",
			input: `{name: "Alice", scores: [10, 20, 30]}`,
			key:   "scores",
			want:  lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(10), lpg.Int64Value(20), lpg.Int64Value(30)}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePropLiteral(tc.input)
			if err != nil {
				t.Fatalf("parsePropLiteral(%q) error: %v", tc.input, err)
			}
			var found *lpg.PropertyValue
			for i := range got {
				if got[i].key == tc.key {
					v := got[i].value
					found = &v
					break
				}
			}
			if found == nil {
				t.Fatalf("key %q not found in parsed result", tc.key)
			}
			if !propValueDeepEqual(*found, tc.want) {
				t.Fatalf("key %q: got %v, want %v", tc.key, *found, tc.want)
			}
		})
	}
}

// TestParsePropValue_NonListPrimitives confirms that existing primitive parsing
// still works correctly after the list-literal change.
func TestParsePropValue_NonListPrimitives(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  lpg.PropertyValue
	}{
		{"integer", "42", lpg.Int64Value(42)},
		{"negative integer", "-7", lpg.Int64Value(-7)},
		{"float", "3.14", lpg.Float64Value(3.14)},
		{"string double quote", `"hello"`, lpg.StringValue("hello")},
		{"string single quote", "'world'", lpg.StringValue("world")},
		{"bool true", "true", lpg.BoolValue(true)},
		{"bool false", "false", lpg.BoolValue(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePropValue(tc.input)
			if err != nil {
				t.Fatalf("parsePropValue(%q) error: %v", tc.input, err)
			}
			if !propValueDeepEqual(got, tc.want) {
				t.Fatalf("parsePropValue(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestParsePropValue_NullSentinel confirms the null literal still returns
// ErrPropertyValueIsNull.
func TestParsePropValue_NullSentinel(t *testing.T) {
	_, err := parsePropValue("null")
	if !errors.Is(err, ErrPropertyValueIsNull) {
		t.Fatalf("parsePropValue(\"null\") = %v, want ErrPropertyValueIsNull", err)
	}
}

// propValueDeepEqual compares two PropertyValues for equality, including
// recursive comparison of PropList elements.
func propValueDeepEqual(a, b lpg.PropertyValue) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case lpg.PropString:
		av, _ := a.String()
		bv, _ := b.String()
		return av == bv
	case lpg.PropInt64:
		av, _ := a.Int64()
		bv, _ := b.Int64()
		return av == bv
	case lpg.PropFloat64:
		av, _ := a.Float64()
		bv, _ := b.Float64()
		return av == bv
	case lpg.PropBool:
		av, _ := a.Bool()
		bv, _ := b.Bool()
		return av == bv
	case lpg.PropList:
		ae, _ := a.List()
		be, _ := b.List()
		if len(ae) != len(be) {
			return false
		}
		for i := range ae {
			if !propValueDeepEqual(ae[i], be[i]) {
				return false
			}
		}
		return true
	}
	return false
}
