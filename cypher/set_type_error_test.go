package cypher_test

// set_type_error_test.go — regression coverage for SET clause type-error
// hardening (roadmap gograph sprint 177, tasks #1455 and #1456).
//
// These cases were surfaced by an exhaustive SET-variation audit (2026-06-13)
// and confirmed against openCypher behaviour by the cypher-expert consultant.
// The openCypher TCK does not cover them, so they are pinned here.
//
//   #1455 — SET/REMOVE of a label on a relationship is a compile-time
//           TypeError ("expected Node but was Relationship"), never a leaked
//           internal error or a silently-mislabelled node.
//   #1456 — A whole-entity SET (`SET n = …` / `SET n += …`) with a non-null,
//           non-map literal RHS is a compile-time TypeError; `SET n = null`
//           clears all properties (≡ `SET n = {}`) and `SET n += null` is a
//           no-op.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers (also used by set_map_param_type_test.go)
// ─────────────────────────────────────────────────────────────────────────────

func setEngine() *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// setExec runs a write query (params may be nil), draining the result, and
// returns any error. Late errors surfaced during iteration are included.
func setExec(t *testing.T, eng *cypher.Engine, q string, params map[string]any) error {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), q, params)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	err = res.Err()
	_ = res.Close()
	return err
}

// setMustExec fails the test if the query errors.
func setMustExec(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	if err := setExec(t, eng, q, nil); err != nil {
		t.Fatalf("setup %q: unexpected error: %v", q, err)
	}
}

// setExprToGo converts an engine record value to a plain Go value.
func setExprToGo(v any) any {
	switch x := v.(type) {
	case expr.StringValue:
		return string(x)
	case expr.IntegerValue:
		return int64(x)
	case expr.FloatValue:
		return float64(x)
	case expr.BoolValue:
		return bool(x)
	case expr.ListValue:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = setExprToGo(e)
		}
		return out
	case expr.MapValue:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = setExprToGo(e)
		}
		return out
	case nil:
		return nil
	default:
		if e, ok := v.(expr.Value); ok && expr.IsNull(e) {
			return nil
		}
		return x
	}
}

// setNodeKeys returns the sorted property-key set of the single node matched
// by pattern (e.g. "(n:L)").
func setNodeKeys(t *testing.T, eng *cypher.Engine, pattern string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), "MATCH "+pattern+" RETURN keys(n) AS k", nil)
	if err != nil {
		t.Fatalf("keys query: %v", err)
	}
	defer res.Close()
	var out []string
	if res.Next() {
		if lst, ok := res.Record()["k"].(expr.ListValue); ok {
			for _, e := range lst {
				if s, ok := setExprToGo(e).(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// setNodeLabels returns the sorted label set of the single node matched by
// pattern.
func setNodeLabels(t *testing.T, eng *cypher.Engine, pattern string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), "MATCH "+pattern+" RETURN labels(n) AS l", nil)
	if err != nil {
		t.Fatalf("labels query: %v", err)
	}
	defer res.Close()
	var out []string
	if res.Next() {
		if lst, ok := res.Record()["l"].(expr.ListValue); ok {
			for _, e := range lst {
				if s, ok := setExprToGo(e).(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// setNodeProp returns a single property value of the node matched by pattern.
func setNodeProp(t *testing.T, eng *cypher.Engine, pattern, key string) any {
	t.Helper()
	res, err := eng.Run(context.Background(), "MATCH "+pattern+" RETURN n."+key+" AS v", nil)
	if err != nil {
		t.Fatalf("prop query: %v", err)
	}
	defer res.Close()
	if res.Next() {
		return setExprToGo(res.Record()["v"])
	}
	return nil
}

func setStrsEqual(got []string, want ...string) bool {
	gc := append([]string(nil), got...)
	sort.Strings(gc)
	sort.Strings(want)
	return strings.Join(gc, "\x00") == strings.Join(want, "\x00")
}

// ─────────────────────────────────────────────────────────────────────────────
// #1455 — label on a relationship is a TypeError
// ─────────────────────────────────────────────────────────────────────────────

func TestSet_LabelOnRelationship_IsTypeError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		q    string
	}{
		{"set", `MATCH (a)-[r:R]->(b) SET r:Ghost`},
		{"remove", `MATCH (a)-[r:R]->(b) REMOVE r:Ghost`},
		{"merge_on_create", `MERGE (a)-[r:R2]->(b) ON CREATE SET r:Ghost`},
		{"merge_on_match", `MATCH (a)-[r:R]->(b) MERGE (a)-[r2:R]->(b) ON MATCH SET r2:Ghost`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := setEngine()
			setMustExec(t, eng, `CREATE (:A)-[:R]->(:B)`)
			err := setExec(t, eng, tc.q, nil)
			if err == nil {
				t.Fatalf("%q: expected a TypeError, got nil", tc.q)
			}
			if !strings.Contains(err.Error(), "TypeError") ||
				!strings.Contains(err.Error(), "expected Node but was Relationship") {
				t.Fatalf("%q: error = %q, want a TypeError mentioning 'expected Node but was Relationship'", tc.q, err.Error())
			}
			// The internal leak must be gone, and no node may be mislabelled.
			if strings.Contains(err.Error(), "cannot resolve NodeID") {
				t.Fatalf("%q: leaked internal error: %v", tc.q, err)
			}
			res, qerr := eng.Run(context.Background(), `MATCH (n:Ghost) RETURN count(n) AS c`, nil)
			if qerr != nil {
				t.Fatalf("count query: %v", qerr)
			}
			if res.Next() {
				if c, ok := setExprToGo(res.Record()["c"]).(int64); ok && c != 0 {
					t.Errorf("%q: %d node(s) silently mislabelled :Ghost", tc.q, c)
				}
			}
			res.Close()
		})
	}
}

// Node label-set / label-remove must remain unaffected.
func TestSet_LabelOnNode_StillWorks(t *testing.T) {
	t.Parallel()
	eng := setEngine()
	setMustExec(t, eng, `CREATE (:A)`)
	setMustExec(t, eng, `MATCH (n:A) SET n:Foo:Bar`)
	if l := setNodeLabels(t, eng, "(n:A)"); !setStrsEqual(l, "A", "Bar", "Foo") {
		t.Fatalf("labels=%v want [A Bar Foo]", l)
	}
	setMustExec(t, eng, `MATCH (n:A) REMOVE n:Bar`)
	if l := setNodeLabels(t, eng, "(n:A)"); !setStrsEqual(l, "A", "Foo") {
		t.Fatalf("labels after REMOVE=%v want [A Foo]", l)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #1456 — whole-entity SET RHS type rules
// ─────────────────────────────────────────────────────────────────────────────

func TestSet_WholeEntity_NonMapLiteralRHS_IsTypeError(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, q string }{
		{"int", `MATCH (n:L) SET n = 5`},
		{"float", `MATCH (n:L) SET n = 1.5`},
		{"string", `MATCH (n:L) SET n = 'x'`},
		{"bool", `MATCH (n:L) SET n = true`},
		{"list", `MATCH (n:L) SET n = [1, 2]`},
		{"merge_int", `MATCH (n:L) SET n += 5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := setEngine()
			setMustExec(t, eng, `CREATE (:L {a: 1})`)
			err := setExec(t, eng, tc.q, nil)
			if err == nil {
				t.Fatalf("%q: expected a TypeError, got nil", tc.q)
			}
			if !strings.Contains(err.Error(), "TypeError") {
				t.Fatalf("%q: error = %q, want a TypeError", tc.q, err.Error())
			}
			// The internal-leak message must be gone.
			if strings.Contains(err.Error(), "enclosed in {}") {
				t.Fatalf("%q: leaked internal parse-map error: %v", tc.q, err)
			}
			// The property must be untouched (the statement failed at compile).
			if v := setNodeProp(t, eng, "(n:L)", "a"); v != int64(1) {
				t.Errorf("%q: property a = %v, want 1 (must be unchanged)", tc.q, v)
			}
		})
	}
}

func TestSet_WholeEntity_NullLiteral(t *testing.T) {
	t.Parallel()

	t.Run("replace_null_clears_all", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L {a: 1, b: 2})`)
		setMustExec(t, eng, `MATCH (n:L) SET n = null`)
		if k := setNodeKeys(t, eng, "(n:L)"); len(k) != 0 {
			t.Fatalf("SET n = null: keys=%v, want [] (all properties cleared)", k)
		}
	})

	t.Run("merge_null_is_noop", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L {a: 1, b: 2})`)
		setMustExec(t, eng, `MATCH (n:L) SET n += null`)
		if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "a", "b") {
			t.Fatalf("SET n += null: keys=%v, want [a b] (no-op)", k)
		}
	})
}

// Whole-entity forms that remain legal: empty/non-empty map, and entity copy.
func TestSet_WholeEntity_LegalForms_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setEngine()
	setMustExec(t, eng, `CREATE (:L {a: 1, old: 9})`)
	setMustExec(t, eng, `MATCH (n:L) SET n = {a: 10, b: 20}`)
	if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "a", "b") {
		t.Fatalf("SET n = {a,b}: keys=%v want [a b] (replace removes 'old')", k)
	}
	setMustExec(t, eng, `MATCH (n:L) SET n = {}`)
	if k := setNodeKeys(t, eng, "(n:L)"); len(k) != 0 {
		t.Fatalf("SET n = {}: keys=%v want [] (clears)", k)
	}
}
