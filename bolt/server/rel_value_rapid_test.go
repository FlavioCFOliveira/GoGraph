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
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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

// TestRelValueRapid_RoundTrip verifies that exprValueToPackstream produces a correct
// Bolt RELATIONSHIP STRUCTURE for a RelationshipValue over 200 rapid iterations.
//
// Since #2189 a relationship goes on the wire as the 'R' (0x52) structure the Bolt
// protocol specifies. At Bolt 5 the field order is [id, start_id, end_id, type,
// properties, element_id, start_element_id, end_element_id] and the driver asserts that
// exact count of eight, so the arity check below is not incidental.
func TestRelValueRapid_RoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		rv := genRelationshipValue().Draw(rt, "rv")

		r := decodeRel(rt, exprValueToPackstream(rv, 5))

		checkInt64Field := func(field string, got int64, want uint64) {
			rt.Helper()
			if uint64(got) != want {
				rt.Fatalf("%s: want %d, got %d", field, want, got)
			}
		}
		checkInt64Field("id", r.ID, rv.ID)
		checkInt64Field("start_id", r.StartID, rv.StartID)
		checkInt64Field("end_id", r.EndID, rv.EndID)

		if r.Type != rv.Type {
			rt.Fatalf("type: want %q, got %q", rv.Type, r.Type)
		}

		if len(r.Props) != len(rv.Properties) {
			rt.Fatalf("properties length: want %d, got %d", len(rv.Properties), len(r.Props))
		}
		for k, wantVal := range rv.Properties {
			gotVal, exists := r.Props[k]
			if !exists {
				rt.Fatalf("properties: key %q missing", k)
			}
			wantStr := string(wantVal.(expr.StringValue)) //nolint:forcetypeassert // generator always produces StringValue
			if gotVal != wantStr {
				rt.Fatalf("properties[%q]: want %q, got %v", k, wantStr, gotVal)
			}
		}

		// The three element ids are the corresponding durable ids in decimal, the same
		// strings the Cypher elementId() function returns.
		if want := strconv.FormatUint(rv.ID, 10); r.ElementID != want {
			rt.Fatalf("element_id: want %q, got %q", want, r.ElementID)
		}
		if want := strconv.FormatUint(rv.StartID, 10); r.StartEID != want {
			rt.Fatalf("start_element_id: want %q, got %q", want, r.StartEID)
		}
		if want := strconv.FormatUint(rv.EndID, 10); r.EndEID != want {
			rt.Fatalf("end_element_id: want %q, got %q", want, r.EndEID)
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
			r := decodeRel(t, exprValueToPackstream(rv, 5))
			if len(r.Type) != tc.length {
				t.Errorf("type length: want %d, got %d", tc.length, len(r.Type))
			}
		})
	}
}
