package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"gograph/cypher/expr"
	"gograph/graph"
)

// writeRecord serialises rec as a single line of JSON terminated by '\n'.
// Keys are emitted in ascending alphabetical order so the byte stream is
// deterministic across runs and golden-file comparable.
//
// Values are translated into JSON via [jsonValue]; see that helper for
// the full type-mapping table.
func writeRecord(w io.Writer, rec map[string]any) error {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.Grow(64 + 32*len(keys))
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kJSON, err := json.Marshal(k)
		if err != nil {
			return fmt.Errorf("marshal key %q: %w", k, err)
		}
		buf.Write(kJSON)
		buf.WriteByte(':')
		vJSON, err := json.Marshal(jsonValue(rec[k]))
		if err != nil {
			return fmt.Errorf("marshal value for key %q: %w", k, err)
		}
		buf.Write(vJSON)
	}
	buf.WriteByte('}')
	buf.WriteByte('\n')
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	return nil
}

// jsonValue converts a Cypher record cell into a value ready for
// encoding/json. The cell may be:
//
//   - nil — emitted as JSON null.
//   - An [expr.Value] (the runtime value model used by Engine.RunInTx):
//     IntegerValue, FloatValue, StringValue, BoolValue, ListValue,
//     MapValue, NodeValue, RelationshipValue, PathValue, or the Null
//     singleton. Each is mapped to its natural JSON shape.
//   - A [graph.NodeID] — emitted as a JSON number (uint64).
//   - A native Go scalar (string, bool, integer, float) — passed through.
//
// Types that have no obvious JSON form (temporal kinds, paths, opaque
// driver values) fall back to their String() representation so the
// record remains encodable rather than failing the whole row.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case expr.Value:
		return jsonExprValue(x)
	case graph.NodeID:
		return uint64(x)
	case int, int8, int16, int32, int64:
		return x
	case uint, uint8, uint16, uint32, uint64:
		return x
	case float32, float64:
		return x
	case string:
		return x
	case bool:
		return x
	case []byte:
		return string(x)
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}

// jsonExprValue maps an expr.Value to its JSON form. NodeValue and
// RelationshipValue are serialised with the leading-underscore convention
// used by neo4j-go-driver so consumers can distinguish graph metadata
// from regular property maps.
func jsonExprValue(v expr.Value) any {
	if expr.IsNull(v) {
		return nil
	}
	switch x := v.(type) {
	case expr.IntegerValue:
		return int64(x)
	case expr.FloatValue:
		return float64(x)
	case expr.StringValue:
		return string(x)
	case expr.BoolValue:
		return bool(x)
	case expr.ListValue:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = jsonExprValue(item)
		}
		return out
	case expr.MapValue:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = jsonExprValue(item)
		}
		return out
	case expr.NodeValue:
		props := make(map[string]any, len(x.Properties))
		for k, item := range x.Properties {
			props[k] = jsonExprValue(item)
		}
		return map[string]any{
			"_id":         x.ID,
			"_labels":     x.Labels,
			"_properties": props,
		}
	case expr.RelationshipValue:
		props := make(map[string]any, len(x.Properties))
		for k, item := range x.Properties {
			props[k] = jsonExprValue(item)
		}
		return map[string]any{
			"_id":         x.ID,
			"_type":       x.Type,
			"_start":      x.StartID,
			"_end":        x.EndID,
			"_properties": props,
		}
	default:
		return v.String()
	}
}
