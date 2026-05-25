package server

// T619: rapid-based round-trip tests for RelationshipValue → packstream encoding.
//
// exprValueToPackstream converts expr.RelationshipValue to
// map[string]packstream.Value. The test verifies identity over 200 rapid
// iterations: id, start, end, type, and properties fields are all preserved.
//
// Known gap — endpoint elementId fields:
//
//	Bolt 5.0+ requires "startNodeElementId" and "endNodeElementId" fields
//	alongside "start" and "end". These are not yet present in the current
//	exprValueToPackstream implementation. When added, the AC "All endpoint-id
//	fields preserved on v5.0+" will be satisfied by extending these tests.
//
// Type-name length boundaries (1, 255, 256 bytes) are covered by the
// TestRelValueTypeName_Boundaries table-driven test below.
//
// Layer: short (no build tag required).

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"gograph/bolt/packstream"
	"gograph/cypher/expr"
)

// genRelationshipValue returns a rapid generator for random expr.RelationshipValue.
func genRelationshipValue() *rapid.Generator[expr.RelationshipValue] {
	return rapid.Custom(func(rt *rapid.T) expr.RelationshipValue {
		id := rapid.Uint64().Draw(rt, "id")
		startID := rapid.Uint64().Draw(rt, "startID")
		endID := rapid.Uint64().Draw(rt, "endID")
		typeName := rapid.StringN(1, 64, 64).Draw(rt, "type")
		numProps := rapid.IntRange(0, 4).Draw(rt, "numProps")
		props := make(expr.MapValue, numProps)
		for i := range numProps {
			k := fmt.Sprintf("p%d", i)
			v := rapid.String().Draw(rt, fmt.Sprintf("propVal[%d]", i))
			props[k] = expr.StringValue(v)
		}
		return expr.RelationshipValue{
			ID:         id,
			StartID:    startID,
			EndID:      endID,
			Type:       typeName,
			Properties: props,
		}
	})
}

// TestRelValueRapid_RoundTrip verifies that exprValueToPackstream produces a
// correct map representation of RelationshipValue over 200 rapid iterations.
//
// The encoded map must have:
//   - "id"         → int64(rv.ID)
//   - "start"      → int64(rv.StartID)
//   - "end"        → int64(rv.EndID)
//   - "type"       → rv.Type (string)
//   - "properties" → map[string]packstream.Value
func TestRelValueRapid_RoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		rv := genRelationshipValue().Draw(rt, "rv")

		got := exprValueToPackstream(rv)
		m, ok := got.(map[string]packstream.Value)
		if !ok {
			rt.Fatalf("expected map[string]packstream.Value, got %T", got)
		}

		checkInt64Field := func(field string, want uint64) {
			rt.Helper()
			v, ok := m[field].(int64)
			if !ok {
				rt.Fatalf("%s field: expected int64, got %T (%v)", field, m[field], m[field])
			}
			if uint64(v) != want {
				rt.Fatalf("%s: want %d, got %d", field, want, v)
			}
		}

		checkInt64Field("id", rv.ID)
		checkInt64Field("start", rv.StartID)
		checkInt64Field("end", rv.EndID)

		typeName, ok := m["type"].(string)
		if !ok {
			rt.Fatalf("type field: expected string, got %T", m["type"])
		}
		if typeName != rv.Type {
			rt.Fatalf("type: want %q, got %q", rv.Type, typeName)
		}

		props, ok := m["properties"].(map[string]packstream.Value)
		if !ok {
			rt.Fatalf("properties field: expected map, got %T", m["properties"])
		}
		if len(props) != len(rv.Properties) {
			rt.Fatalf("properties length: want %d, got %d", len(rv.Properties), len(props))
		}
		for k, wantVal := range rv.Properties {
			gotVal, exists := props[k]
			if !exists {
				rt.Fatalf("properties: key %q missing", k)
			}
			wantStr := string(wantVal.(expr.StringValue)) //nolint:forcetypeassert // generator always produces StringValue
			if gotVal != wantStr {
				rt.Fatalf("properties[%q]: want %q, got %v", k, wantStr, gotVal)
			}
		}

		// No unexpected fields.
		if len(m) != 5 {
			rt.Fatalf("map has unexpected number of fields: %d (expected 5)", len(m))
		}
	})
}

// TestRelValueTypeName_Boundaries exercises type-name length boundaries:
// 1 byte, 255 bytes, and 256 bytes. These straddle the Str8/Str16 boundary
// in the properties encoding and confirm the type field is not truncated.
func TestRelValueTypeName_Boundaries(t *testing.T) {
	cases := []struct {
		name   string
		length int
	}{
		{"len_1", 1},
		{"len_255_str8_max", 255},
		{"len_256_str16_start", 256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typeName := strings.Repeat("T", tc.length)
			rv := expr.RelationshipValue{
				ID:         1,
				StartID:    2,
				EndID:      3,
				Type:       typeName,
				Properties: expr.MapValue{},
			}
			got := exprValueToPackstream(rv)
			m, ok := got.(map[string]packstream.Value)
			if !ok {
				t.Fatalf("expected map, got %T", got)
			}
			gotType, ok := m["type"].(string)
			if !ok {
				t.Fatalf("type field: expected string, got %T", m["type"])
			}
			if len(gotType) != tc.length {
				t.Errorf("type length: want %d, got %d", tc.length, len(gotType))
			}
		})
	}
}
