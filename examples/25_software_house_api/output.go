package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"gograph/cypher/expr"
	"gograph/graph"
)

// writeJSON marshals payload and writes it with the given status code and
// a JSON content type. Marshalling failures degrade to a 500 with a fixed
// error body rather than a partial response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"response encoding failed","kind":"runtime"}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// errorBody is the typed error envelope returned by every failing handler.
type errorBody struct {
	Error string `json:"error"`
	Kind  string `json:"kind"`
}

// writeError emits the typed error envelope with the given status.
func writeError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Kind: kind})
}

// jsonValue converts a Cypher record cell into a value ready for
// encoding/json. The cell may be:
//
//   - nil — emitted as JSON null.
//   - An [expr.Value] (the runtime value model): IntegerValue, FloatValue,
//     StringValue, BoolValue, ListValue, MapValue, NodeValue,
//     RelationshipValue, PathValue, or the Null singleton.
//   - A [graph.NodeID] — emitted as a JSON number (uint64).
//   - A native Go scalar (string, bool, integer, float) — passed through.
//
// Types with no obvious JSON form fall back to their String()
// representation so a row remains encodable rather than failing.
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
// RelationshipValue use the leading-underscore convention of the
// neo4j-go-driver so consumers can distinguish graph metadata from
// regular property maps.
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
