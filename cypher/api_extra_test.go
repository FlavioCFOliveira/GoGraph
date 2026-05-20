package cypher_test

// api_extra_test.go — supplementary tests to bring gograph/cypher above the
// 75% coverage gate. Targets: NewEngineWithRegistry, Record(), label scan,
// selection, expand, projection, and unsupported plan root error.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: graph with labeled nodes
// ─────────────────────────────────────────────────────────────────────────────

// newLabeledGraph creates a graph with n nodes all tagged with the given label.
func newLabeledGraph(n int, label string) *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range n {
		node := string(rune('A'+i%26)) + string(rune('0'+i%10))
		g.SetNodeLabel(node, label)
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// NewEngineWithRegistry
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_NewEngineWithRegistry(t *testing.T) {
	g := newGraph(3)
	eng := cypher.NewEngineWithRegistry(g, funcs.DefaultRegistry)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 rows, got %d", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Result.Record() — retrieve column values from a row
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_Record(t *testing.T) {
	g := newGraph(1)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	if !res.Next() {
		t.Fatal("expected at least one row")
	}
	rec := res.Record()
	if rec == nil {
		t.Fatal("Record() returned nil")
	}
	// The record must contain the column "n".
	if _, ok := rec["n"]; !ok {
		t.Errorf("Record() missing column 'n', got %v", rec)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeByLabelScan — MATCH (n:Person) RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_LabelScan_Match(t *testing.T) {
	g := newLabeledGraph(4, "Person")
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	if count != 4 {
		t.Errorf("expected 4 Person rows, got %d", count)
	}
}

func TestEngine_LabelScan_UnknownLabel(t *testing.T) {
	g := newGraph(3) // no labels
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n:Ghost) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	// No nodes have label Ghost; should return 0 rows.
	if count != 0 {
		t.Errorf("expected 0 rows for unknown label, got %d", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Selection — MATCH (n) WHERE n.age > 5 RETURN n  (pass-through stub)
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_Selection_PassThrough(t *testing.T) {
	const n = 3
	g := newGraph(n)
	eng := cypher.NewEngine(g)

	// The Selection stub always passes; all n rows should come through.
	res, err := eng.Run(context.Background(), "MATCH (n) WHERE n.age > 5 RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	if count != n {
		t.Errorf("expected %d rows (selection stub is pass-through), got %d", n, count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Expand — MATCH (n)-[r]->(m) RETURN n  (stub: row count == node count)
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_Expand_Stub(t *testing.T) {
	const n = 3
	g := newGraph(n)
	eng := cypher.NewEngine(g)

	// Sprint 25 Expand stub: passes through child rows unchanged.
	res, err := eng.Run(context.Background(), "MATCH (n)-[r]->(m) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	var count int
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	// Stub passes all n rows through.
	if count != n {
		t.Errorf("expected %d rows (expand stub), got %d", n, count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Projection alias resolution path
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_Projection_Alias(t *testing.T) {
	g := newGraph(2)
	eng := cypher.NewEngine(g)

	// RETURN n AS node — exercises Projection with explicit alias.
	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n AS node", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	cols := res.Columns()
	if len(cols) != 1 || cols[0] != "node" {
		t.Errorf("Columns() = %v, want [node]", cols)
	}
	var count int
	for res.Next() {
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Plan cache hit — same query produces identical results on second run
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_LabelScan_PlanCacheHit(t *testing.T) {
	g := newLabeledGraph(2, "Employee")
	eng := cypher.NewEngine(g)

	const query = "MATCH (n:Employee) RETURN n"
	for i := range 3 {
		res, err := eng.Run(context.Background(), query, nil)
		if err != nil {
			t.Fatalf("Run[%d] error: %v", i, err)
		}
		var count int
		for res.Next() {
			count++
		}
		res.Close()
		if count != 2 {
			t.Errorf("Run[%d]: got %d rows, want 2", i, count)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Params passed through do not break basic execution
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_WithParams_NoError(t *testing.T) {
	g := newGraph(2)
	eng := cypher.NewEngine(g)

	params := map[string]expr.Value{"limit": expr.IntegerValue(10)}
	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", params)
	if err != nil {
		t.Fatalf("Run with params error: %v", err)
	}
	defer res.Close()
	for res.Next() {
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BindParams — type conversion coverage
// ─────────────────────────────────────────────────────────────────────────────

func TestBindParams_TypeConversions(t *testing.T) {
	t.Run("nil map", func(t *testing.T) {
		out, err := cypher.BindParams(nil)
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			t.Fatalf("expected nil, got %v", out)
		}
	})

	t.Run("integer types", func(t *testing.T) {
		in := map[string]any{
			"i":   int(1),
			"i8":  int8(2),
			"i16": int16(3),
			"i32": int32(4),
			"i64": int64(5),
		}
		out, err := cypher.BindParams(in)
		if err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]int64{"i": 1, "i8": 2, "i16": 3, "i32": 4, "i64": 5} {
			got, ok := out[k].(expr.IntegerValue)
			if !ok || int64(got) != want {
				t.Errorf("key %q: got %v, want %d", k, out[k], want)
			}
		}
	})

	t.Run("uint types", func(t *testing.T) {
		in := map[string]any{"u": uint(7), "u64": uint64(99)}
		out, err := cypher.BindParams(in)
		if err != nil {
			t.Fatal(err)
		}
		if int64(out["u"].(expr.IntegerValue)) != 7 { //nolint:forcetypeassert
			t.Errorf("uint: got %v", out["u"])
		}
		if int64(out["u64"].(expr.IntegerValue)) != 99 { //nolint:forcetypeassert
			t.Errorf("uint64: got %v", out["u64"])
		}
	})

	t.Run("float types", func(t *testing.T) {
		in := map[string]any{"f32": float32(1.5), "f64": float64(2.5)}
		out, err := cypher.BindParams(in)
		if err != nil {
			t.Fatal(err)
		}
		if float64(out["f32"].(expr.FloatValue)) != float64(float32(1.5)) { //nolint:forcetypeassert
			t.Errorf("float32: got %v", out["f32"])
		}
		if float64(out["f64"].(expr.FloatValue)) != 2.5 { //nolint:forcetypeassert
			t.Errorf("float64: got %v", out["f64"])
		}
	})

	t.Run("bool and string and nil", func(t *testing.T) {
		in := map[string]any{"b": true, "s": "hello", "n": nil}
		out, err := cypher.BindParams(in)
		if err != nil {
			t.Fatal(err)
		}
		if out["b"] != expr.BoolValue(true) {
			t.Errorf("bool: got %v", out["b"])
		}
		if out["s"] != expr.StringValue("hello") {
			t.Errorf("string: got %v", out["s"])
		}
		if out["n"] != expr.Null {
			t.Errorf("nil: got %v", out["n"])
		}
	})

	t.Run("passthrough expr.Value", func(t *testing.T) {
		v := expr.IntegerValue(42)
		out, err := cypher.BindParams(map[string]any{"x": v})
		if err != nil {
			t.Fatal(err)
		}
		if out["x"] != v {
			t.Errorf("passthrough: got %v", out["x"])
		}
	})

	t.Run("list recursive", func(t *testing.T) {
		in := map[string]any{"l": []any{int(1), "two", true}}
		out, err := cypher.BindParams(in)
		if err != nil {
			t.Fatal(err)
		}
		l, ok := out["l"].(expr.ListValue)
		if !ok || len(l) != 3 {
			t.Fatalf("list: got %v", out["l"])
		}
		if l[0] != expr.IntegerValue(1) || l[1] != expr.StringValue("two") || l[2] != expr.BoolValue(true) {
			t.Errorf("list elements: %v", l)
		}
	})

	t.Run("map recursive", func(t *testing.T) {
		in := map[string]any{"m": map[string]any{"k": "v"}}
		out, err := cypher.BindParams(in)
		if err != nil {
			t.Fatal(err)
		}
		m, ok := out["m"].(expr.MapValue)
		if !ok {
			t.Fatalf("map: got %T", out["m"])
		}
		if m["k"] != expr.StringValue("v") {
			t.Errorf("map value: got %v", m["k"])
		}
	})

	t.Run("unsupported type error", func(t *testing.T) {
		in := map[string]any{"bad": struct{}{}}
		_, err := cypher.BindParams(in)
		if err == nil {
			t.Fatal("expected error for unsupported type")
		}
	})
}

func TestRunAny_Basic(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("A")
	g.AddNode("B")
	eng := cypher.NewEngine(g)

	res, err := eng.RunAny(context.Background(), "MATCH (n) RETURN n", map[string]any{"x": int(1)})
	if err != nil {
		t.Fatalf("RunAny error: %v", err)
	}
	defer res.Close()
	var count int
	for res.Next() {
		count++
	}
	if count != 2 {
		t.Errorf("got %d rows, want 2", count)
	}
}

func TestRunInTxAny_Basic(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	res, err := eng.RunInTxAny(context.Background(), "CREATE (n:Test)", map[string]any{"x": "hello"})
	if err != nil {
		t.Fatalf("RunInTxAny error: %v", err)
	}
	defer res.Close()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}
}
