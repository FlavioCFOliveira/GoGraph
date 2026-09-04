package server

// T613: rapid-based round-trip tests for NodeValue → packstream encoding.
//
// exprValueToPackstream converts expr.NodeValue to map[string]packstream.Value.
// The test verifies identity of the encoded map structure over 200 rapid
// iterations: id field, label slice order, and properties are all preserved.
//
// Known gap — ElementId:
//
//	The current exprValueToPackstream implementation does not include an
//	"elementId" field in the encoded map. Bolt 5.0+ requires elementId
//	alongside id for full protocol conformance. This is tracked separately;
//	when elementId is added the AC "ElementId field present when negotiated
//	v5.0+" will be satisfied by extending these tests.
//
// Layer: short (no build tag required).

import (
	"fmt"
	"strconv"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// genNodeValue returns a rapid generator that produces random expr.NodeValue
// instances with 0–4 labels and 0–4 string-valued properties.
func genNodeValue() *rapid.Generator[expr.NodeValue] {
	return rapid.Custom(func(rt *rapid.T) expr.NodeValue {
		id := rapid.Uint64().Draw(rt, "id")
		numLabels := rapid.IntRange(0, 4).Draw(rt, "numLabels")
		labels := make([]string, numLabels)
		for i := range labels {
			labels[i] = rapid.StringN(1, 20, 20).Draw(rt, fmt.Sprintf("label[%d]", i))
		}
		numProps := rapid.IntRange(0, 4).Draw(rt, "numProps")
		props := make(expr.MapValue, numProps)
		for i := range numProps {
			k := fmt.Sprintf("prop%d", i)
			v := rapid.String().Draw(rt, fmt.Sprintf("propVal[%d]", i))
			props[k] = expr.StringValue(v)
		}
		return expr.NodeValue{
			ID:         id,
			Labels:     labels,
			Properties: props,
		}
	})
}

// TestNodeValueRapid_RoundTrip verifies that exprValueToPackstream produces a correct
// Bolt NODE STRUCTURE for a NodeValue over 200 rapid iterations.
//
// Since #2189 a node goes on the wire as the 'N' (0x4E) structure the Bolt protocol
// specifies, not as a PackStream map, so the driver can materialise it as a
// dbtype.Node. At Bolt 5 the field order is [id, labels, properties, element_id]; the
// driver asserts that exact count, so the arity check below is not incidental.
func TestNodeValueRapid_RoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nv := genNodeValue().Draw(rt, "nv")

		got := exprValueToPackstream(nv, 5)
		st, ok := got.(packstream.Struct)
		if !ok {
			rt.Fatalf("expected packstream.Struct, got %T", got)
		}
		if st.Tag != tagNode {
			rt.Fatalf("tag: want %#x (Node), got %#x", tagNode, st.Tag)
		}
		if len(st.Fields) != 4 {
			rt.Fatalf("Bolt 5 Node must carry 4 fields (the driver asserts the count), got %d", len(st.Fields))
		}

		// Field 0: id.
		id, ok := st.Fields[0].(int64)
		if !ok {
			rt.Fatalf("id field: expected int64, got %T (%v)", st.Fields[0], st.Fields[0])
		}
		if uint64(id) != nv.ID {
			rt.Fatalf("id: want %d, got %d", nv.ID, id)
		}

		// Field 1: labels, in source order.
		labels, ok := st.Fields[1].([]packstream.Value)
		if !ok {
			rt.Fatalf("labels field: expected []packstream.Value, got %T", st.Fields[1])
		}
		if len(labels) != len(nv.Labels) {
			rt.Fatalf("labels length: want %d, got %d", len(nv.Labels), len(labels))
		}
		for i, want := range nv.Labels {
			got, ok := labels[i].(string)
			if !ok {
				rt.Fatalf("label[%d]: expected string, got %T", i, labels[i])
			}
			if got != want {
				rt.Fatalf("label[%d]: want %q, got %q", i, want, got)
			}
		}

		// Field 2: properties.
		props, ok := st.Fields[2].(map[string]packstream.Value)
		if !ok {
			rt.Fatalf("properties field: expected map[string]packstream.Value, got %T", st.Fields[2])
		}
		if len(props) != len(nv.Properties) {
			rt.Fatalf("properties length: want %d, got %d", len(nv.Properties), len(props))
		}
		for k, wantVal := range nv.Properties {
			gotVal, exists := props[k]
			if !exists {
				rt.Fatalf("properties: key %q missing", k)
			}
			wantStr := string(wantVal.(expr.StringValue)) // generator always produces StringValue
			if gotVal != wantStr {
				rt.Fatalf("properties[%q]: want %q, got %v", k, wantStr, gotVal)
			}
		}

		// Field 3: element_id — the durable id in decimal, identical to what the
		// Cypher elementId() function returns for the same entity.
		eid, ok := st.Fields[3].(string)
		if !ok {
			rt.Fatalf("element_id field: expected string, got %T", st.Fields[3])
		}
		if want := strconv.FormatUint(nv.ID, 10); eid != want {
			rt.Fatalf("element_id: want %q, got %q", want, eid)
		}
	})
}

// TestNodeValueRapid_Bolt4OmitsElementID pins the version split: element_id was added in
// Bolt 5, and a Bolt 4 client asserts a THREE-field Node, so sending four would be a
// protocol error on that connection.
func TestNodeValueRapid_Bolt4OmitsElementID(t *testing.T) {
	nv := expr.NodeValue{ID: 7, Labels: []string{"A"}, Properties: expr.MapValue{}}
	st, ok := exprValueToPackstream(nv, 4).(packstream.Struct)
	if !ok {
		t.Fatalf("expected packstream.Struct")
	}
	if st.Tag != tagNode {
		t.Fatalf("tag: want %#x, got %#x", tagNode, st.Tag)
	}
	if len(st.Fields) != 3 {
		t.Fatalf("Bolt 4 Node must carry 3 fields (no element_id), got %d", len(st.Fields))
	}
}

// TestNodeValueRapid_LabelOrderPreserved verifies label order preservation
// with a fixed multi-label node to ensure the index-stable slice copy in
// exprValueToPackstream does not sort or shuffle labels.
func TestNodeValueRapid_LabelOrderPreserved(t *testing.T) {
	nv := expr.NodeValue{
		ID:         99,
		Labels:     []string{"Z", "A", "M", "B"},
		Properties: expr.MapValue{},
	}
	got := exprValueToPackstream(nv, 5)
	st := got.(packstream.Struct)               // known type
	labels := st.Fields[1].([]packstream.Value) // known type
	want := []string{"Z", "A", "M", "B"}
	for i, wl := range want {
		if labels[i].(string) != wl { // known type
			t.Errorf("label[%d]: want %q, got %v", i, wl, labels[i])
		}
	}
}
