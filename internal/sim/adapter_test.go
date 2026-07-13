package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestToExprValue_Map verifies the harness binds a string-keyed map parameter as
// an expr.MapValue, recursively converting its scalar and list elements (EN1).
func TestToExprValue_Map(t *testing.T) {
	ev, err := toExprValue(map[string]any{
		"name": "Ada",
		"age":  int64(36),
		"tags": []any{"x", int64(1)},
	})
	if err != nil {
		t.Fatalf("toExprValue(map): unexpected error: %v", err)
	}
	m, ok := ev.(expr.MapValue)
	if !ok {
		t.Fatalf("toExprValue(map): got %T, want expr.MapValue", ev)
	}
	if len(m) != 3 {
		t.Fatalf("map arity: got %d, want 3", len(m))
	}
	if s, ok := m["name"].(expr.StringValue); !ok || string(s) != "Ada" {
		t.Fatalf("map[name]: got %v (%T), want StringValue(Ada)", m["name"], m["name"])
	}
	if i, ok := m["age"].(expr.IntegerValue); !ok || int64(i) != 36 {
		t.Fatalf("map[age]: got %v (%T), want IntegerValue(36)", m["age"], m["age"])
	}
	lst, ok := m["tags"].(expr.ListValue)
	if !ok || len(lst) != 2 {
		t.Fatalf("map[tags]: got %v (%T), want ListValue len 2", m["tags"], m["tags"])
	}
}

// TestToExprValue_UnsupportedMapElement verifies a map carrying an unsupported
// element kind surfaces a loud error rather than binding a wrong value.
func TestToExprValue_UnsupportedMapElement(t *testing.T) {
	if _, err := toExprValue(map[string]any{"bad": struct{}{}}); err == nil {
		t.Fatalf("expected error for unsupported map element, got nil")
	}
}

// TestEngineAdapter_MapParamRoundTrip drives a map parameter through the real
// engine write path via the adapter: `SET n = $props` consumes the map to set a
// SET of scalar properties (openCypher forbids a map as a single property value),
// and each property must read back to its bound value (EN1 end-to-end).
func TestEngineAdapter_MapParamRoundTrip(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	ctx := context.Background()

	res, err := a.RunWrite(ctx, "CREATE (n:Person) SET n = $props",
		map[string]any{"props": map[string]any{"name": "Ada", "age": int64(36)}})
	if err != nil {
		t.Fatalf("RunWrite(SET n = $props): %v", err)
	}
	_ = res.Close()

	got, err := a.projectRowStrings(ctx, "MATCH (n:Person {name:'Ada'}) RETURN n.name, n.age", 2)
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if got == nil {
		t.Fatalf("read-back: node absent after SET n = $props")
	}
	// Values render through expr.Value.String(): strings are quoted, ints bare —
	// the same canonical form the type-coverage checker compares against.
	if got[0] != `"Ada"` || got[1] != "36" {
		t.Fatalf(`read-back: got %v, want ["Ada" 36]`, got)
	}
}
