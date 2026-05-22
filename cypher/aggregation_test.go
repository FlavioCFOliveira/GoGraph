package cypher_test

// aggregation_test.go — integration tests for EagerAggregation wiring
// (tasks #371). Tests exercise MATCH … RETURN count(*), count(n), and
// group-by aggregation end-to-end through the Engine.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// collectRecords drains a Result into a slice of exec.Record maps.
func collectRecords(t *testing.T, res *cypher.Result) []map[string]interface{} {
	t.Helper()
	defer res.Close()
	var rows []map[string]interface{}
	for res.Next() {
		rec := res.Record()
		cp := make(map[string]interface{}, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		rows = append(rows, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	return rows
}

// countRows drains a Result and returns the number of rows produced.
func countRows(t *testing.T, res *cypher.Result) int {
	t.Helper()
	defer res.Close()
	var n int
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. count(*) on 3-node graph → single row with IntegerValue(3)
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_CountStar(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("c"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(*) AS cnt", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["cnt"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 3 {
		t.Errorf("count(*) = %d, want 3", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. count(*) on empty graph → no rows (EagerAggregation emits one row per
//    group; with zero input rows, zero groups are formed)
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_CountStar_EmptyGraph(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(*) AS cnt", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	// openCypher semantics: a group-by-less aggregation over zero input rows
	// emits exactly one output row with each aggregate evaluated against the
	// empty multiset (count → 0, sum → 0, min/max/avg/collect → NULL).
	// GlobalAggregateAdapter synthesises that row on top of EagerAggregation
	// (which on its own emits zero rows in this case).
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for empty graph aggregate, got %d", len(rows))
	}
	raw := rows[0]["cnt"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 0 {
		t.Errorf("count(*) on empty graph = %d, want 0", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. count(n) — counts non-null values of n (should equal node count)
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_CountN(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(n) AS cnt", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["cnt"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 2 {
		t.Errorf("count(n) = %d, want 2", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. count(*) via WITH — tests EagerAggregation in a WITH clause pipeline
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_WithCountStar(t *testing.T) {
	g := newNNodeGraph(t, 5)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) WITH count(*) AS total RETURN total", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	raw := rows[0]["total"]
	got, ok := raw.(expr.IntegerValue)
	if !ok {
		t.Fatalf("total: expected IntegerValue, got %T (%v)", raw, raw)
	}
	if int64(got) != 5 {
		t.Errorf("total = %d, want 5", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. count(property) — property access as aggregate argument
// ─────────────────────────────────────────────────────────────────────────────

// TestAggregation_CountProperty verifies that count(n.num) sees actual property
// values (not the raw node id) so that NULLs (missing properties) are skipped.
// Matches openCypher TCK Aggregation1[1].
func TestAggregation_CountProperty(t *testing.T) {
	eng, _ := newAggGraph(t, []map[string]any{
		{"name": "a", "num": int64(33)},
		{"name": "a"}, // no num → count skips
		{"name": "b", "num": int64(42)},
	})

	res, err := eng.Run(context.Background(), `MATCH (n) RETURN count(n.num) AS c`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got, ok := rows[0]["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("c: expected IntegerValue, got %T", rows[0]["c"])
	}
	if int64(got) != 2 {
		t.Errorf("count(n.num) = %d, want 2", int64(got))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Group-by property — verifies non-aggregate property access becomes
//    a real grouping key (matches openCypher TCK Aggregation1[1]).
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_GroupByProperty(t *testing.T) {
	eng, _ := newAggGraph(t, []map[string]any{
		{"name": "a", "num": int64(33)},
		{"name": "a"},
		{"name": "b", "num": int64(42)},
	})

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name AS name, count(n.num) AS c`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (one per distinct name), got %d: %#v", len(rows), rows)
	}
	counts := map[string]int64{}
	for _, r := range rows {
		name, ok := r["name"].(expr.StringValue)
		if !ok {
			t.Fatalf("name: expected StringValue, got %T (%v)", r["name"], r["name"])
		}
		c, ok := r["c"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("c: expected IntegerValue, got %T", r["c"])
		}
		counts[string(name)] = int64(c)
	}
	if counts["a"] != 1 || counts["b"] != 1 {
		t.Errorf("group counts = %v, want {a:1, b:1}", counts)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. sum(n.num), avg(n.num), min(n.num), max(n.num), collect(n.num) — all
//    property-access argument forms in a single grouped query.
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_AllScalarsByProperty(t *testing.T) {
	eng, _ := newAggGraph(t, []map[string]any{
		{"group": "g", "num": int64(2)},
		{"group": "g", "num": int64(4)},
		{"group": "g", "num": int64(6)},
	})

	res, err := eng.Run(context.Background(), `
		MATCH (n)
		RETURN
		  n.group AS g,
		  sum(n.num) AS s,
		  avg(n.num) AS a,
		  min(n.num) AS lo,
		  max(n.num) AS hi,
		  count(n.num) AS c,
		  collect(n.num) AS xs
	`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	mustInt(t, "s", row["s"], 12)
	mustFloat(t, "a", row["a"], 4.0)
	mustInt(t, "lo", row["lo"], 2)
	mustInt(t, "hi", row["hi"], 6)
	mustInt(t, "c", row["c"], 3)
	xs, ok := row["xs"].(expr.ListValue)
	if !ok {
		t.Fatalf("xs: expected ListValue, got %T", row["xs"])
	}
	if len(xs) != 3 {
		t.Errorf("collect length = %d, want 3", len(xs))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. Empty graph + RETURN sum/avg/min/max/collect — verifies the
//    GlobalAggregateAdapter synthesises one row with neutral results.
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_EmptyInputNeutralRow(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN sum(n.x) AS s, avg(n.x) AS a, min(n.x) AS lo, max(n.x) AS hi, collect(n.x) AS xs, count(*) AS c`,
		nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 neutral row, got %d", len(rows))
	}
	row := rows[0]
	// On empty input, the GlobalAggregateAdapter constructs a fresh aggregator
	// per slot and calls Result(). The per-aggregator identity element is:
	//   count(*) / count(x) → IntegerValue(0)
	//   sum / avg / min / max → expr.Null (no rows ever observed)
	//   collect → empty ListValue
	mustInt(t, "c", row["c"], 0)
	if v := row["s"]; !expr.IsNull(v.(expr.Value)) {
		t.Errorf("sum on empty = %v, want NULL", v)
	}
	if v := row["a"]; !expr.IsNull(v.(expr.Value)) {
		t.Errorf("avg on empty = %v, want NULL", v)
	}
	if v := row["lo"]; !expr.IsNull(v.(expr.Value)) {
		t.Errorf("min on empty = %v, want NULL", v)
	}
	if v := row["hi"]; !expr.IsNull(v.(expr.Value)) {
		t.Errorf("max on empty = %v, want NULL", v)
	}
	if xs, ok := row["xs"].(expr.ListValue); !ok || len(xs) != 0 {
		t.Errorf("collect on empty = %v (%T), want empty ListValue", row["xs"], row["xs"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. stDev / stDevp — newly detected aggregate functions.
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregation_StDevDetection(t *testing.T) {
	eng, _ := newAggGraph(t, []map[string]any{
		{"x": int64(2)},
		{"x": int64(4)},
		{"x": int64(4)},
		{"x": int64(4)},
		{"x": int64(5)},
		{"x": int64(5)},
		{"x": int64(7)},
		{"x": int64(9)},
	})

	res, err := eng.Run(context.Background(), `MATCH (n) RETURN stDev(n.x) AS s, stDevp(n.x) AS sp`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// We don't pin exact float values — just verify the aggregator path ran
	// and produced a non-NULL FloatValue rather than treating stDev as a
	// scalar (which would have failed during projection eval).
	if _, ok := rows[0]["s"].(expr.FloatValue); !ok {
		t.Errorf("stDev(n.x): want FloatValue, got %T (%v)", rows[0]["s"], rows[0]["s"])
	}
	if _, ok := rows[0]["sp"].(expr.FloatValue); !ok {
		t.Errorf("stDevp(n.x): want FloatValue, got %T (%v)", rows[0]["sp"], rows[0]["sp"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newAggGraph creates an empty graph populated by running a chain of CREATE
// statements built from props. Each map becomes a node with the given
// properties; node names are auto-generated as n0, n1, …. Returns the engine
// (so callers can run queries) and the graph (so callers can introspect).
func newAggGraph(t *testing.T, props []map[string]any) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	if len(props) == 0 {
		return eng, g
	}
	var sb strings.Builder
	for i, p := range props {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "CREATE (n%d {", i)
		first := true
		for k, v := range p {
			if !first {
				sb.WriteString(", ")
			}
			first = false
			sb.WriteString(k)
			sb.WriteString(": ")
			switch vv := v.(type) {
			case string:
				sb.WriteString("'")
				sb.WriteString(vv)
				sb.WriteString("'")
			case int64:
				fmt.Fprintf(&sb, "%d", vv)
			case int:
				fmt.Fprintf(&sb, "%d", vv)
			case float64:
				fmt.Fprintf(&sb, "%g", vv)
			default:
				t.Fatalf("newAggGraph: unsupported property type %T for key %q", v, k)
			}
		}
		sb.WriteString("})")
	}
	res, err := eng.RunInTxAny(context.Background(), sb.String(), nil)
	if err != nil {
		t.Fatalf("newAggGraph setup: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("newAggGraph drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("newAggGraph close: %v", err)
	}
	return eng, g
}

func mustInt(t *testing.T, name string, v any, want int64) {
	t.Helper()
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Errorf("%s: expected IntegerValue, got %T (%v)", name, v, v)
		return
	}
	if int64(iv) != want {
		t.Errorf("%s = %d, want %d", name, int64(iv), want)
	}
}

func mustFloat(t *testing.T, name string, v any, want float64) {
	t.Helper()
	fv, ok := v.(expr.FloatValue)
	if !ok {
		t.Errorf("%s: expected FloatValue, got %T (%v)", name, v, v)
		return
	}
	if float64(fv) != want {
		t.Errorf("%s = %g, want %g", name, float64(fv), want)
	}
}
