package sim

import (
	"context"
	"testing"
	"time"

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

// TestToExprValue_Temporal pins the temporal parameter mapping (rmp #2457): the
// six temporal expr values pass through verbatim, a Go time.Time binds as a
// zoned DATETIME, and a Go time.Duration binds as a DURATION carrying only the
// seconds/nanoseconds stride. Binding the ISO-8601 STRING instead would store an
// untagged property that reads back as a plain string, which is precisely the
// degradation the type-coverage checker exists to catch.
func TestToExprValue_Temporal(t *testing.T) {
	cases := []struct {
		in   any
		want expr.Value
		name string
	}{
		{name: "date", in: expr.NewDate(2026, 7, 13), want: expr.NewDate(2026, 7, 13)},
		{
			name: "localdatetime",
			in:   expr.NewLocalDateTime(2026, 7, 13, 14, 35, 47, 0),
			want: expr.NewLocalDateTime(2026, 7, 13, 14, 35, 47, 0),
		},
		{
			name: "datetime",
			in:   expr.NewDateTime(2026, 7, 13, 14, 35, 47, 0, time.UTC),
			want: expr.NewDateTime(2026, 7, 13, 14, 35, 47, 0, time.UTC),
		},
		{name: "localtime", in: expr.NewLocalTime(14, 35, 47, 0), want: expr.NewLocalTime(14, 35, 47, 0)},
		{name: "time", in: expr.NewTime(14, 35, 0, 0, 3600), want: expr.NewTime(14, 35, 0, 0, 3600)},
		{name: "duration", in: expr.NewDuration(1, 2, 3, 4), want: expr.NewDuration(1, 2, 3, 4)},
		{
			name: "go time.Time maps to DATETIME",
			in:   time.Date(2026, 7, 13, 14, 35, 47, 0, time.UTC),
			want: expr.DateTimeValue{T: time.Date(2026, 7, 13, 14, 35, 47, 0, time.UTC)},
		},
		{
			name: "go time.Duration maps to DURATION",
			in:   90*time.Minute + 500*time.Millisecond,
			want: expr.NewDuration(0, 0, 5400, 500_000_000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toExprValue(tc.in)
			if err != nil {
				t.Fatalf("toExprValue(%T): %v", tc.in, err)
			}
			if got.Kind() != tc.want.Kind() {
				t.Fatalf("kind: got %v, want %v", got.Kind(), tc.want.Kind())
			}
			if got.String() != tc.want.String() {
				t.Fatalf("value: got %s, want %s", got.String(), tc.want.String())
			}
		})
	}
}

// TestToExprValue_TemporalIsTaggedOnWrite proves the binding path this harness
// uses actually reaches the engine's TAGGED temporal storage form: a temporal
// bound as a parameter reads back as a temporal, while the same text bound as a
// STRING reads back as a string. Without the tag both would be indistinguishable
// strings, which is the tautology rmp #2457 removed.
func TestToExprValue_TemporalIsTaggedOnWrite(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	if _, err := sm.engine.RunWrite(ctx, "CREATE (n:TagProbe {temporal:$t, text:$s})", map[string]any{
		"t": expr.NewDate(2026, 7, 13),
		"s": "2026-07-13",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := sm.engine.projectRowValues(ctx, "MATCH (n:TagProbe) RETURN n.temporal, n.text", 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read: no row")
	}
	if got[0].Kind() != expr.KindDate {
		t.Errorf("parameter-bound temporal read back as %v (%s), want Date — the storage tag was not written",
			got[0].Kind(), got[0].String())
	}
	if got[1].Kind() != expr.KindString {
		t.Errorf("string-bound value read back as %v, want String", got[1].Kind())
	}
}
