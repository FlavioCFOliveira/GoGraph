package cypher_test

// set_map_param_type_test.go — regression coverage for SET map-value and
// parameter type validation (roadmap gograph sprint 177, tasks #1457 and
// #1458). Surfaced by the SET-variation audit (2026-06-13); openCypher
// behaviour confirmed by the cypher-expert consultant.
//
//   #1457 — A map, or a list of maps, used as a property value inside a
//           whole-entity SET map raises InvalidPropertyType (matching the
//           single-property form `SET n.k = {…}`, Set1[10]) instead of being
//           silently dropped.
//   #1458 — A non-null, non-map parameter as a whole-entity SET source raises
//           a TypeError instead of silently clearing the target's properties;
//           a null parameter clears for `=` / no-ops for `+=`.

import (
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// #1457 — map / list-of-maps as a property value
// ─────────────────────────────────────────────────────────────────────────────

func TestSet_MapValueInWholeEntityMap_IsInvalidPropertyType(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, q string }{
		{"replace_nested_map", `MATCH (n:L) SET n = {m: {a: 1}}`},
		{"merge_nested_map", `MATCH (n:L) SET n += {m: {a: 1}}`},
		{"replace_list_of_maps", `MATCH (n:L) SET n = {lst: [{a: 1}]}`},
		{"merge_list_of_maps", `MATCH (n:L) SET n += {lst: [{a: 1}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := setEngine()
			setMustExec(t, eng, `CREATE (:L {keep: 1})`)
			err := setExec(t, eng, tc.q, nil)
			if err == nil {
				t.Fatalf("%q: expected InvalidPropertyType, got nil", tc.q)
			}
			if !strings.Contains(err.Error(), "InvalidPropertyType") {
				t.Fatalf("%q: error = %q, want InvalidPropertyType", tc.q, err.Error())
			}
			// Statement failed at build → target untouched (no partial write,
			// no silent clear).
			if v := setNodeProp(t, eng, "(n:L)", "keep"); v != int64(1) {
				t.Errorf("%q: property keep = %v, want 1 (must be unchanged)", tc.q, v)
			}
		})
	}
}

// Normal SET maps with primitive / list-of-primitive values stay unaffected.
func TestSet_WholeEntityMap_PrimitiveValues_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setEngine()
	setMustExec(t, eng, `CREATE (:L)`)
	setMustExec(t, eng, `MATCH (n:L) SET n = {a: 1, b: 'x', c: true, d: [1, 2, 3]}`)
	if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "a", "b", "c", "d") {
		t.Fatalf("keys=%v want [a b c d]", k)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #1458 — non-map parameter as a whole-entity SET source
// ─────────────────────────────────────────────────────────────────────────────

func TestSet_NonMapParamSource_IsTypeError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		q      string
		params map[string]any
	}{
		{"replace_scalar", `MATCH (n:L) SET n = $p`, map[string]any{"p": 5}},
		{"merge_scalar", `MATCH (n:L) SET n += $p`, map[string]any{"p": "x"}},
		{"replace_bool", `MATCH (n:L) SET n = $p`, map[string]any{"p": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := setEngine()
			setMustExec(t, eng, `CREATE (:L {a: 1, b: 2})`)
			err := setExec(t, eng, tc.q, tc.params)
			if err == nil {
				t.Fatalf("%q: expected a TypeError, got nil", tc.q)
			}
			if !strings.Contains(err.Error(), "TypeError") {
				t.Fatalf("%q: error = %q, want a TypeError", tc.q, err.Error())
			}
			// CRITICAL: the target's properties must NOT have been cleared.
			if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "a", "b") {
				t.Errorf("%q: keys=%v, want [a b] — properties must NOT be cleared (no silent data loss)", tc.q, k)
			}
		})
	}
}

// A map-typed parameter source remains fully functional.
func TestSet_MapParamSource_Works(t *testing.T) {
	t.Parallel()

	t.Run("merge", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L {a: 1})`)
		if err := setExec(t, eng, `MATCH (n:L) SET n += $p`,
			map[string]any{"p": map[string]any{"b": 2, "c": 3}}); err != nil {
			t.Fatalf("SET n += $mapParam: %v", err)
		}
		if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "a", "b", "c") {
			t.Fatalf("keys=%v want [a b c]", k)
		}
	})

	t.Run("replace", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L {a: 1, old: 9})`)
		if err := setExec(t, eng, `MATCH (n:L) SET n = $p`,
			map[string]any{"p": map[string]any{"b": 2}}); err != nil {
			t.Fatalf("SET n = $mapParam: %v", err)
		}
		if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "b") {
			t.Fatalf("keys=%v want [b] (replace)", k)
		}
	})

	// A list-of-primitives is a legal property value and must be kept (not
	// silently dropped) when it arrives as a parameter-map entry.
	t.Run("list_of_primitives_value_kept", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L)`)
		if err := setExec(t, eng, `MATCH (n:L) SET n += $p`,
			map[string]any{"p": map[string]any{"tags": []any{"a", "b", "c"}}}); err != nil {
			t.Fatalf("SET n += {tags:[...]} via param: %v", err)
		}
		if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "tags") {
			t.Fatalf("keys=%v want [tags] (list value must be kept)", k)
		}
		lst, ok := setNodeProp(t, eng, "(n:L)", "tags").([]any)
		if !ok || len(lst) != 3 {
			t.Fatalf("tags = %v, want a 3-element list", setNodeProp(t, eng, "(n:L)", "tags"))
		}
	})
}

// A null parameter behaves like the null literal: clear for `=`, no-op for `+=`.
func TestSet_NullParamSource(t *testing.T) {
	t.Parallel()

	t.Run("replace_null_clears", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L {a: 1, b: 2})`)
		if err := setExec(t, eng, `MATCH (n:L) SET n = $p`, map[string]any{"p": nil}); err != nil {
			t.Fatalf("SET n = $nullParam: %v", err)
		}
		if k := setNodeKeys(t, eng, "(n:L)"); len(k) != 0 {
			t.Fatalf("keys=%v want [] (null param clears for =)", k)
		}
	})

	t.Run("merge_null_noop", func(t *testing.T) {
		t.Parallel()
		eng := setEngine()
		setMustExec(t, eng, `CREATE (:L {a: 1, b: 2})`)
		if err := setExec(t, eng, `MATCH (n:L) SET n += $p`, map[string]any{"p": nil}); err != nil {
			t.Fatalf("SET n += $nullParam: %v", err)
		}
		if k := setNodeKeys(t, eng, "(n:L)"); !setStrsEqual(k, "a", "b") {
			t.Fatalf("keys=%v want [a b] (null param no-op for +=)", k)
		}
	})
}
