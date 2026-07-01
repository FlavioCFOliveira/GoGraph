package cypher

// completeness_engine_test.go — end-to-end tests for the completeness built-ins
// (audit finding F5, #1832) that need the full engine: timestamp()
// statement-freezing (via the per-query now-aware registry), randomUUID() shape
// and uniqueness, and elementId() over live entities.

import (
	"context"
	"regexp"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func runScalarRows(t *testing.T, e *Engine, q string, col string) []expr.Value {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	var out []expr.Value
	for res.Next() {
		out = append(out, res.Record()[col].(expr.Value))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	return out
}

func TestEngine_Timestamp_StatementFrozen(t *testing.T) {
	e := NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true}))
	// Five rows in one statement must all observe the SAME timestamp — the
	// per-query now-aware registry freezes the instant. Without freezing,
	// successive time.Now() reads could diverge across rows.
	vals := runScalarRows(t, e, `UNWIND range(1, 5) AS x RETURN timestamp() AS t`, "t")
	if len(vals) != 5 {
		t.Fatalf("got %d rows, want 5", len(vals))
	}
	first, ok := vals[0].(expr.IntegerValue)
	if !ok {
		t.Fatalf("timestamp() returned %T, want IntegerValue", vals[0])
	}
	if int64(first) <= 0 {
		t.Fatalf("timestamp() = %d, want a positive epoch-milliseconds value", int64(first))
	}
	for i, v := range vals {
		if v != first {
			t.Fatalf("timestamp() row %d = %v, not statement-frozen (row 0 = %v)", i, v, first)
		}
	}
}

func TestEngine_RandomUUID_ShapeAndUniqueness(t *testing.T) {
	e := NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true}))
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	vals := runScalarRows(t, e, `UNWIND range(1, 200) AS x RETURN randomUUID() AS u`, "u")
	if len(vals) != 200 {
		t.Fatalf("got %d rows, want 200", len(vals))
	}
	seen := make(map[string]struct{}, len(vals))
	for i, v := range vals {
		s, ok := v.(expr.StringValue)
		if !ok {
			t.Fatalf("randomUUID() row %d returned %T, want StringValue", i, v)
		}
		if !v4.MatchString(string(s)) {
			t.Fatalf("randomUUID() = %q, not a canonical v4 UUID", string(s))
		}
		if _, dup := seen[string(s)]; dup {
			t.Fatalf("randomUUID() produced a duplicate: %q", string(s))
		}
		seen[string(s)] = struct{}{}
	}
}

func TestEngine_ElementID_LiveNode(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(g)
	vals := runScalarRows(t, e, `MATCH (n) RETURN elementId(n) AS eid`, "eid")
	if len(vals) != 1 {
		t.Fatalf("got %d rows, want 1", len(vals))
	}
	if _, ok := vals[0].(expr.StringValue); !ok {
		t.Fatalf("elementId(n) returned %T, want StringValue", vals[0])
	}
}
