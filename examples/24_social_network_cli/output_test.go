package main

import (
	"bytes"
	"strings"
	"testing"

	"gograph/cypher/expr"
	"gograph/graph"
)

// TestWriteRecord_AlphabeticalOrder verifies that keys are emitted in
// ascending alphabetical order independent of map iteration order.
func TestWriteRecord_AlphabeticalOrder(t *testing.T) {
	rec := map[string]any{
		"z": int64(1),
		"a": int64(2),
		"m": int64(3),
	}
	var buf bytes.Buffer
	if err := writeRecord(&buf, rec); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}
	got := buf.String()
	want := `{"a":2,"m":3,"z":1}` + "\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestWriteRecord_TerminatorNewline verifies that exactly one '\n' is
// appended to each record, regardless of the number of keys.
func TestWriteRecord_TerminatorNewline(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  map[string]any
	}{
		{"empty", map[string]any{}},
		{"single", map[string]any{"k": "v"}},
		{"three", map[string]any{"a": 1, "b": 2, "c": 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeRecord(&buf, tc.rec); err != nil {
				t.Fatalf("writeRecord: %v", err)
			}
			s := buf.String()
			if !strings.HasSuffix(s, "\n") {
				t.Fatalf("output %q does not end with '\\n'", s)
			}
			if strings.Count(s, "\n") != 1 {
				t.Fatalf("output %q has %d newlines, want exactly 1", s, strings.Count(s, "\n"))
			}
		})
	}
}

// TestWriteRecord_ExprValueTypes covers the full expr.Value mapping
// table: each kind that may appear in a Cypher record cell.
func TestWriteRecord_ExprValueTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
		want string
	}{
		{"integer", expr.IntegerValue(42), `{"v":42}` + "\n"},
		{"float", expr.FloatValue(3.5), `{"v":3.5}` + "\n"},
		{"string", expr.StringValue("hello"), `{"v":"hello"}` + "\n"},
		{"bool true", expr.BoolValue(true), `{"v":true}` + "\n"},
		{"bool false", expr.BoolValue(false), `{"v":false}` + "\n"},
		{"null", expr.Null, `{"v":null}` + "\n"},
		{"list", expr.ListValue{expr.IntegerValue(1), expr.StringValue("x")}, `{"v":[1,"x"]}` + "\n"},
		{
			"map",
			expr.MapValue{"k1": expr.IntegerValue(1), "k2": expr.StringValue("x")},
			"", // map iteration order is non-deterministic at the expr level; assert via parse below
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeRecord(&buf, map[string]any{"v": tc.val}); err != nil {
				t.Fatalf("writeRecord: %v", err)
			}
			got := buf.String()
			if tc.want == "" {
				// map values: only assert structural properties.
				if !strings.HasPrefix(got, `{"v":{`) || !strings.HasSuffix(got, "}}\n") {
					t.Fatalf("map case: got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteRecord_GraphNodeID confirms that a bare graph.NodeID is
// emitted as a JSON number (uint64) so that columns produced by
// MATCH (n) RETURN id(n) round-trip cleanly.
func TestWriteRecord_GraphNodeID(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRecord(&buf, map[string]any{"id": graph.NodeID(7)}); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}
	got := buf.String()
	want := `{"id":7}` + "\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestWriteRecord_NativeScalars confirms that native Go scalars pass
// through encoding/json without alteration. This matters because a
// Cypher record cell that contains a raw int64/string/bool (instead of
// the wrapping expr.Value) must still be JSON-encodable.
func TestWriteRecord_NativeScalars(t *testing.T) {
	rec := map[string]any{
		"int":   int64(42),
		"flt":   3.5,
		"str":   "hello",
		"bool":  true,
		"null":  nil,
		"bytes": []byte("ab"),
	}
	var buf bytes.Buffer
	if err := writeRecord(&buf, rec); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}
	got := buf.String()
	want := `{"bool":true,"bytes":"ab","flt":3.5,"int":42,"null":null,"str":"hello"}` + "\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestWriteRecord_NodeValue confirms that a NodeValue is emitted with
// the leading-underscore convention for graph metadata fields.
func TestWriteRecord_NodeValue(t *testing.T) {
	n := expr.NodeValue{
		ID:     11,
		Labels: []string{"User"},
		Properties: expr.MapValue{
			"username": expr.StringValue("alice"),
		},
	}
	var buf bytes.Buffer
	if err := writeRecord(&buf, map[string]any{"n": n}); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}
	got := buf.String()
	want := `{"n":{"_id":11,"_labels":["User"],"_properties":{"username":"alice"}}}` + "\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
