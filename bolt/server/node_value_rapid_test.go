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
	"reflect"
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

// TestNodeValueRapid_RoundTrip verifies that exprValueToPackstream produces a
// correct map representation of NodeValue over 200 rapid iterations.
//
// The encoded map must have:
//   - "id"         → int64(nv.ID)
//   - "labels"     → []packstream.Value with label strings in source order
//   - "properties" → map[string]packstream.Value with string values
func TestNodeValueRapid_RoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nv := genNodeValue().Draw(rt, "nv")

		got := exprValueToPackstream(nv, 5)
		m, ok := got.(map[string]packstream.Value)
		if !ok {
			rt.Fatalf("expected map[string]packstream.Value, got %T", got)
		}

		// id field.
		id, ok := m["id"].(int64)
		if !ok {
			rt.Fatalf("id field: expected int64, got %T (%v)", m["id"], m["id"])
		}
		if uint64(id) != nv.ID {
			rt.Fatalf("id: want %d, got %d", nv.ID, id)
		}

		// labels field: order must match source order.
		labels, ok := m["labels"].([]packstream.Value)
		if !ok {
			rt.Fatalf("labels field: expected []packstream.Value, got %T", m["labels"])
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

		// properties field.
		props, ok := m["properties"].(map[string]packstream.Value)
		if !ok {
			rt.Fatalf("properties field: expected map[string]packstream.Value, got %T", m["properties"])
		}
		if len(props) != len(nv.Properties) {
			rt.Fatalf("properties length: want %d, got %d", len(nv.Properties), len(props))
		}
		for k, wantVal := range nv.Properties {
			gotVal, exists := props[k]
			if !exists {
				rt.Fatalf("properties: key %q missing", k)
			}
			wantStr := string(wantVal.(expr.StringValue)) //nolint:forcetypeassert // generator always produces StringValue
			if gotVal != wantStr {
				rt.Fatalf("properties[%q]: want %q, got %v", k, wantStr, gotVal)
			}
		}

		// Verify there are no unexpected fields beyond id/labels/properties.
		if len(m) != 3 {
			rt.Fatalf("map has unexpected fields: %v", reflect.ValueOf(m).MapKeys())
		}
	})
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
	m := got.(map[string]packstream.Value)     //nolint:forcetypeassert // known type
	labels := m["labels"].([]packstream.Value) //nolint:forcetypeassert // known type
	want := []string{"Z", "A", "M", "B"}
	for i, wl := range want {
		if labels[i].(string) != wl { //nolint:forcetypeassert // known type
			t.Errorf("label[%d]: want %q, got %v", i, wl, labels[i])
		}
	}
}
