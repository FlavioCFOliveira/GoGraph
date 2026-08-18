package server

// list_value_test.go — RECORD encoding of expr.ListValue (rmp #2513).
//
// A list-valued column reaches every Bolt client through
// [exprValueToPackstream]. Before #2513 that switch had no expr.ListValue arm —
// despite its own godoc claiming one — so a list fell through to the `default`
// arm and was emitted as its String() rendering: `RETURN [1,2,3]` arrived as the
// packstream String "[1, 2, 3]", and `nodes(p)` arrived as text with all node
// structure destroyed. Every list-producing construct was affected: collect(),
// labels(), keys(), nodes(), relationships(), list literals, and list-valued
// properties.
//
// These tests assert the STRUCTURAL encoding — a PackStream List whose elements
// are themselves encoded per kind, with the negotiated Bolt major threaded
// through so entity elements carry the right field count on both 4.4 and 5.x.
// A list is version-INDEPENDENT on the wire (the same List markers exist in
// PackStream v1 and v2); it is the ELEMENTS that branch on the version, which is
// exactly why boltMajor must be propagated rather than dropped.
//
// Layer: short (no build tag required).

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// asList asserts that got is a PackStream List — a []packstream.Value — and
// returns its elements. The concrete Go type IS the wire encoding: a list that
// arrives as a string is the #2513 defect even when the text looks right.
func asList(t testing.TB, got packstream.Value) []packstream.Value {
	t.Helper()
	if s, isString := got.(string); isString {
		t.Fatalf("list encoded as packstream String %q — the expr.ListValue arm is missing (#2513)", s)
	}
	l, ok := got.([]packstream.Value)
	if !ok {
		t.Fatalf("expected a PackStream List ([]packstream.Value), got %T (%v)", got, got)
	}
	return l
}

// TestListValue_Scalars is the headline case: a list of integers must encode as
// a List of Integers, not as its String() rendering.
func TestListValue_Scalars(t *testing.T) {
	got := exprValueToPackstream(
		expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}, 5)
	items := asList(t, got)
	want := []packstream.Value{int64(1), int64(2), int64(3)}
	if len(items) != len(want) {
		t.Fatalf("arity: got %d, want %d", len(items), len(want))
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("element %d: got %T %v, want %T %v", i, items[i], items[i], want[i], want[i])
		}
	}
}

// TestListValue_MixedScalars covers every scalar kind inside one list, including
// a Null element, which must be a PackStream NULL rather than the text "null".
func TestListValue_MixedScalars(t *testing.T) {
	items := asList(t, exprValueToPackstream(expr.ListValue{
		expr.StringValue("a"),
		expr.IntegerValue(-7),
		expr.FloatValue(2.5),
		expr.BoolValue(true),
		expr.Null,
	}, 5))
	want := []packstream.Value{"a", int64(-7), 2.5, true, nil}
	if len(items) != len(want) {
		t.Fatalf("arity: got %d, want %d", len(items), len(want))
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("element %d: got %T %#v, want %T %#v", i, items[i], items[i], want[i], want[i])
		}
	}
}

// TestListValue_Empty pins the empty list: an empty PackStream List (a non-nil
// []packstream.Value of length zero), never the string "[]".
func TestListValue_Empty(t *testing.T) {
	items := asList(t, exprValueToPackstream(expr.ListValue{}, 5))
	if items == nil {
		t.Fatal("empty list encoded as a nil slice; want a non-nil empty List")
	}
	if len(items) != 0 {
		t.Fatalf("empty list: got %d elements, want 0", len(items))
	}

	// A nil ListValue is the same wire value: an empty List, not NULL.
	nilItems := asList(t, exprValueToPackstream(expr.ListValue(nil), 5))
	if len(nilItems) != 0 {
		t.Fatalf("nil list: got %d elements, want 0", len(nilItems))
	}
}

// TestListValue_Nested proves the arm recurses: a list inside a list stays a
// List at every level.
func TestListValue_Nested(t *testing.T) {
	outer := asList(t, exprValueToPackstream(expr.ListValue{
		expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)},
		expr.ListValue{},
		expr.IntegerValue(3),
	}, 5))
	if len(outer) != 3 {
		t.Fatalf("outer arity: got %d, want 3", len(outer))
	}
	inner := asList(t, outer[0])
	if len(inner) != 2 || inner[0] != int64(1) || inner[1] != int64(2) {
		t.Errorf("inner list: got %#v, want [1 2]", inner)
	}
	if empty := asList(t, outer[1]); len(empty) != 0 {
		t.Errorf("nested empty list: got %#v, want []", empty)
	}
	if outer[2] != int64(3) {
		t.Errorf("scalar sibling: got %T %v, want int64 3", outer[2], outer[2])
	}
}

// TestListValue_InsideMap covers the composition the map arm already had but
// could not complete: a Map whose value is a List.
func TestListValue_InsideMap(t *testing.T) {
	got := exprValueToPackstream(expr.MapValue{
		"xs": expr.ListValue{expr.IntegerValue(7)},
	}, 5)
	m, ok := got.(map[string]packstream.Value)
	if !ok {
		t.Fatalf("expected a PackStream Map, got %T", got)
	}
	items := asList(t, m["xs"])
	if len(items) != 1 || items[0] != int64(7) {
		t.Fatalf("map value list: got %#v, want [7]", items)
	}
}

// TestListValue_OfNodes is the nodes(p) case: every element must be a Bolt 'N'
// structure, with the field count the negotiated version specifies. This is
// where dropping boltMajor would be invisible on 5.x and wrong on 4.4.
func TestListValue_OfNodes(t *testing.T) {
	list := expr.ListValue{
		expr.NodeValue{ID: 1, Labels: []string{"A"}, Properties: expr.MapValue{"k": expr.IntegerValue(1)}},
		expr.NodeValue{ID: 2, Labels: []string{"B", "C"}},
	}
	for _, tc := range []struct {
		name       string
		boltMajor  uint8
		wantFields int
	}{
		{"bolt 4.4", 4, 3},
		{"bolt 5", 5, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := asList(t, exprValueToPackstream(list, tc.boltMajor))
			if len(items) != 2 {
				t.Fatalf("arity: got %d, want 2", len(items))
			}
			for i, item := range items {
				s, ok := item.(packstream.Struct)
				if !ok {
					t.Fatalf("element %d: expected a Node Struct, got %T (%v)", i, item, item)
				}
				if s.Tag != tagNode {
					t.Errorf("element %d: tag 0x%02X, want 0x%02X ('N')", i, s.Tag, tagNode)
				}
				if len(s.Fields) != tc.wantFields {
					t.Errorf("element %d: %d fields, want %d", i, len(s.Fields), tc.wantFields)
				}
			}
			// The first node's labels are themselves a List, and its property
			// map a Map — structure preserved all the way down.
			first, _ := items[0].(packstream.Struct)
			if labels := asList(t, first.Fields[1]); len(labels) != 1 || labels[0] != "A" {
				t.Errorf("labels: got %#v, want [A]", labels)
			}
			if props, ok := first.Fields[2].(map[string]packstream.Value); !ok || props["k"] != int64(1) {
				t.Errorf("properties: got %T %#v, want map with k=1", first.Fields[2], first.Fields[2])
			}
		})
	}
}

// TestListValue_OfRelationships is the relationships(p) case: Bolt 'R'
// structures carrying their endpoints, again with a version-dependent arity.
func TestListValue_OfRelationships(t *testing.T) {
	list := expr.ListValue{
		expr.RelationshipValue{ID: 10, StartID: 1, EndID: 2, Type: "KNOWS"},
	}
	for _, tc := range []struct {
		name       string
		boltMajor  uint8
		wantFields int
	}{
		{"bolt 4.4", 4, 5},
		{"bolt 5", 5, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := asList(t, exprValueToPackstream(list, tc.boltMajor))
			s, ok := items[0].(packstream.Struct)
			if !ok {
				t.Fatalf("expected a Relationship Struct, got %T (%v)", items[0], items[0])
			}
			if s.Tag != tagRelationship {
				t.Errorf("tag 0x%02X, want 0x%02X ('R')", s.Tag, tagRelationship)
			}
			if len(s.Fields) != tc.wantFields {
				t.Errorf("%d fields, want %d", len(s.Fields), tc.wantFields)
			}
			if s.Fields[3] != "KNOWS" {
				t.Errorf("type field: got %#v, want \"KNOWS\"", s.Fields[3])
			}
		})
	}
}

// TestListValue_OfPaths covers collect(p): each element is a Bolt 'P' structure
// whose own node and relationship lists survive as Lists of Structs.
func TestListValue_OfPaths(t *testing.T) {
	p := expr.PathValue{
		Nodes: []expr.NodeValue{
			{ID: 1, Labels: []string{"A"}},
			{ID: 2, Labels: []string{"B"}},
		},
		Relationships: []expr.RelationshipValue{
			{ID: 10, StartID: 1, EndID: 2, Type: "R"},
		},
	}
	items := asList(t, exprValueToPackstream(expr.ListValue{p}, 5))
	s, ok := items[0].(packstream.Struct)
	if !ok {
		t.Fatalf("expected a Path Struct, got %T (%v)", items[0], items[0])
	}
	if s.Tag != tagPath {
		t.Fatalf("tag 0x%02X, want 0x%02X ('P')", s.Tag, tagPath)
	}
	if len(s.Fields) != 3 {
		t.Fatalf("%d fields, want 3", len(s.Fields))
	}
	if nodes := asList(t, s.Fields[0]); len(nodes) != 2 {
		t.Errorf("path nodes: got %d, want 2", len(nodes))
	}
	if rels := asList(t, s.Fields[1]); len(rels) != 1 {
		t.Errorf("path relationships: got %d, want 1", len(rels))
	}
	if idx := asList(t, s.Fields[2]); len(idx) != 2 {
		t.Errorf("path indices: got %d, want 2", len(idx))
	}
}

// TestListValue_OfTemporals proves the version threading reaches temporal
// elements too: a DateTime inside a list takes tag 0x49 on Bolt 5 and 0x46 on
// Bolt 4.4, exactly as it does at top level.
func TestListValue_OfTemporals(t *testing.T) {
	list := expr.ListValue{
		expr.NewDate(2024, 1, 15),
		expr.NewDuration(1, 2, 3, 4),
		expr.NewDateTime(2024, 1, 15, 10, 30, 0, 0, time.FixedZone("test", 3600)),
	}
	for _, tc := range []struct {
		name            string
		boltMajor       uint8
		wantDateTimeTag byte
	}{
		{"bolt 4.4", 4, 0x46},
		{"bolt 5", 5, 0x49},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := asList(t, exprValueToPackstream(list, tc.boltMajor))
			if len(items) != 3 {
				t.Fatalf("arity: got %d, want 3", len(items))
			}
			wantTags := []byte{0x44, 0x45, tc.wantDateTimeTag}
			for i, wantTag := range wantTags {
				s, ok := items[i].(packstream.Struct)
				if !ok {
					t.Fatalf("element %d: expected a temporal Struct, got %T (%v)", i, items[i], items[i])
				}
				if s.Tag != wantTag {
					t.Errorf("element %d: tag 0x%02X, want 0x%02X", i, s.Tag, wantTag)
				}
			}
		})
	}
}

// TestListValue_DeeplyNestedMixture drives lists, maps and entities interleaved
// several levels down, so a partial fix that only handled the top level fails.
func TestListValue_DeeplyNestedMixture(t *testing.T) {
	v := expr.ListValue{
		expr.MapValue{
			"inner": expr.ListValue{
				expr.ListValue{
					expr.MapValue{"n": expr.NodeValue{ID: 9, Labels: []string{"Deep"}}},
				},
			},
		},
	}
	l1 := asList(t, exprValueToPackstream(v, 5))
	m1, ok := l1[0].(map[string]packstream.Value)
	if !ok {
		t.Fatalf("level 1: expected a Map, got %T", l1[0])
	}
	l2 := asList(t, m1["inner"])
	l3 := asList(t, l2[0])
	m2, ok := l3[0].(map[string]packstream.Value)
	if !ok {
		t.Fatalf("level 4: expected a Map, got %T", l3[0])
	}
	s, ok := m2["n"].(packstream.Struct)
	if !ok {
		t.Fatalf("level 5: expected a Node Struct, got %T (%v)", m2["n"], m2["n"])
	}
	if s.Tag != tagNode {
		t.Errorf("deep node tag 0x%02X, want 0x%02X", s.Tag, tagNode)
	}
	if len(s.Fields) != 4 {
		t.Errorf("deep node: %d fields, want 4 (Bolt 5)", len(s.Fields))
	}
}

// TestListValue_WireRoundTrip encodes a list-bearing RECORD through the real
// packstream Encoder and decodes it again, proving the BYTES on the wire are a
// PackStream List. Asserting the intermediate tree alone would not: a value that
// the encoder cannot write is a protocol error, not a passing test.
func TestListValue_WireRoundTrip(t *testing.T) {
	cells := []packstream.Value{
		exprValueToPackstream(expr.ListValue{
			expr.IntegerValue(1), expr.StringValue("a"), expr.ListValue{expr.BoolValue(false)},
		}, 5),
	}

	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := encodeRecordFast(enc, cells); err != nil {
		t.Fatalf("encodeRecordFast: %v", err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	v, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	rec, ok := v.(packstream.Struct)
	if !ok || rec.Tag != proto.TagRecord {
		t.Fatalf("expected a RECORD Struct, got %T %#v", v, v)
	}
	fields := asList(t, rec.Fields[0])
	if len(fields) != 1 {
		t.Fatalf("RECORD: got %d columns, want 1", len(fields))
	}
	items := asList(t, fields[0])
	if len(items) != 3 {
		t.Fatalf("decoded list: got %d elements, want 3", len(items))
	}
	if items[0] != int64(1) || items[1] != "a" {
		t.Errorf("decoded scalars: got %#v, %#v", items[0], items[1])
	}
	nested := asList(t, items[2])
	if len(nested) != 1 || nested[0] != false {
		t.Errorf("decoded nested list: got %#v, want [false]", nested)
	}
}

// TestListValue_LargeListIsNotTruncated pins the container-size contract on the
// write side: a list past the tiny/8-bit header boundaries takes a wider length
// prefix and every element survives. Silent truncation is the failure this
// guards against.
func TestListValue_LargeListIsNotTruncated(t *testing.T) {
	for _, n := range []int{15, 16, 255, 256, 65535, 65536} {
		lv := make(expr.ListValue, n)
		for i := range lv {
			lv[i] = expr.IntegerValue(i)
		}
		items := asList(t, exprValueToPackstream(lv, 5))
		if len(items) != n {
			t.Fatalf("n=%d: encoded %d elements", n, len(items))
		}

		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		if err := enc.WriteValue(packstream.Value(items)); err != nil {
			t.Fatalf("n=%d: WriteValue: %v", n, err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatalf("n=%d: Flush: %v", n, err)
		}
		dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
		back, err := dec.ReadValue()
		if err != nil {
			t.Fatalf("n=%d: ReadValue: %v", n, err)
		}
		decoded, ok := back.([]packstream.Value)
		if !ok {
			t.Fatalf("n=%d: decoded %T, want a List", n, back)
		}
		if len(decoded) != n {
			t.Fatalf("n=%d: decoded %d elements — the list was TRUNCATED", n, len(decoded))
		}
		if decoded[n-1] != int64(n-1) {
			t.Fatalf("n=%d: last element %#v, want %d", n, decoded[n-1], n-1)
		}
	}
}

// TestListValue_OverDeepNestingIsATypedError pins the other half of the
// container contract. expr.MaxValueDepth (1000) is deliberately more generous
// than the PackStream wire cap (128), so a legally constructed but over-deep
// list CAN reach the encoder. It must be refused with the typed
// packstream.ErrNestingTooDeep — a clean per-query error — never truncated to
// fit and never a stack overflow.
func TestListValue_OverDeepNestingIsATypedError(t *testing.T) {
	var v expr.Value = expr.IntegerValue(1)
	for range 300 { // comfortably past packstream's 128-level wire cap
		v = expr.ListValue{v}
	}

	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	err := enc.WriteValue(exprValueToPackstream(v, 5))
	if err == nil {
		t.Fatal("an over-deep list was accepted; want packstream.ErrNestingTooDeep")
	}
	if !errors.Is(err, packstream.ErrNestingTooDeep) {
		t.Fatalf("got %v, want packstream.ErrNestingTooDeep", err)
	}

	// Control: a list nested well inside the cap still encodes cleanly, so the
	// guard above is the depth bound firing and not a blanket rejection.
	var shallow expr.Value = expr.IntegerValue(1)
	for range 100 {
		shallow = expr.ListValue{shallow}
	}
	buf.Reset()
	enc = packstream.NewEncoder(&buf)
	if err := enc.WriteValue(exprValueToPackstream(shallow, 5)); err != nil {
		t.Fatalf("a 100-level list was refused: %v", err)
	}
}

// TestListValue_IsNotAFastScalar pins the streaming fast path: a list column
// must never be treated as a raw scalar, or [encodeRecordFast] would hand a raw
// expr value to packstream.WriteValue, which cannot encode it.
func TestListValue_IsNotAFastScalar(t *testing.T) {
	if isFastScalar(expr.ListValue{expr.IntegerValue(1)}) {
		t.Fatal("expr.ListValue reported as a fast scalar")
	}
	if isFastScalar(expr.ListValue{}) {
		t.Fatal("an empty expr.ListValue reported as a fast scalar")
	}
}

// TestExprValueToPackstream_NoKindStringifies generalises the #2513 defect
// class. The bug was not that lists are special: it was that a value kind with
// no arm falls through to `default` and is emitted as its String() rendering —
// text that looks plausible while destroying the wire type, which no driver can
// recover. Only expr.StringValue may encode to a Go string; every other kind
// must reach a native PackStream type or Struct.
//
// A new expr.Value kind added without an arm in exprValueToPackstream is caught
// here as soon as it is listed below — and it must be listed, because an
// unlisted kind is exactly how #2513 survived a green suite.
func TestExprValueToPackstream_NoKindStringifies(t *testing.T) {
	cases := []struct {
		name       string
		v          expr.Value
		wantString bool // only the String kind may encode to a Go string
	}{
		{"Null", expr.Null, false},
		{"Integer", expr.IntegerValue(1), false},
		{"Float", expr.FloatValue(1.5), false},
		{"String", expr.StringValue("s"), true},
		{"Bool", expr.BoolValue(true), false},
		{"List", expr.ListValue{expr.IntegerValue(1)}, false},
		{"Map", expr.MapValue{"k": expr.IntegerValue(1)}, false},
		{"Node", expr.NodeValue{ID: 1}, false},
		{"Relationship", expr.RelationshipValue{ID: 1, Type: "T"}, false},
		{"Path", expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}}, false},
		{"Date", expr.NewDate(2024, 1, 15), false},
		{"LocalTime", expr.LocalTimeValue{Nanos: 1}, false},
		{"Time", expr.TimeValue{Nanos: 1, OffsetSec: 3600}, false},
		{"LocalDateTime", expr.NewLocalDateTime(2024, 1, 15, 1, 2, 3, 4), false},
		{"DateTime", expr.NewDateTime(2024, 1, 15, 1, 2, 3, 4, time.UTC), false},
		{"Duration", expr.NewDuration(1, 2, 3, 4), false},
	}
	for _, c := range cases {
		for _, boltMajor := range []uint8{4, 5} {
			got := exprValueToPackstream(c.v, boltMajor)
			_, isString := got.(string)
			if isString != c.wantString {
				t.Errorf("%s (bolt %d): encoded as %T %#v — a non-String kind must not reach the "+
					"stringifying default arm", c.name, boltMajor, got, got)
			}
		}
	}
}
