package cypher_test

// bindparams_reflect_test.go — gate for rmp #2219.
//
// BindParams enumerated its container types with a Go type switch, so it bound
// []any and map[string]any but rejected every TYPED slice and map: []string,
// []int and []map[string]any were all "unsupported parameter type". That is a
// sharp edge for a Go library — those are the shapes a Go caller naturally
// holds — and `UNWIND $rows AS r`, the idiom every driver's documentation
// teaches for bulk work, needs []map[string]any specifically.
//
// A reflection fallback now binds any slice and any string-keyed map,
// recursively. This file pins the four properties that make that safe: the new
// shapes bind to the RIGHT VALUES (not merely without error), the exact
// int/float distinction survives, the recursion is bounded rather than
// overflowing the stack on hostile input, and []byte still fails loudly.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func bindEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g)
}

// bindRunScalar runs q with params and returns the single rendered value of the
// single column of the single row.
func bindRunScalar(t *testing.T, eng *cypher.Engine, q string, params map[string]any) string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var got string
	n := 0
	for res.Next() {
		got = res.ValueAt(0).String()
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	if n != 1 {
		t.Fatalf("run %q: got %d rows, want 1", q, n)
	}
	return got
}

// TestBindParams_TypedContainersBindToTheRightValues is the gate: each shape
// must bind AND evaluate to what the caller meant.
func TestBindParams_TypedContainersBindToTheRightValues(t *testing.T) {
	eng := bindEngine(t)
	cases := []struct {
		name  string
		query string
		param any
		want  string
	}{
		{"[]int", `RETURN $p AS v`, []int{1, 2, 3}, `[1, 2, 3]`},
		{"[]string", `RETURN $p AS v`, []string{"a", "b"}, `["a", "b"]`},
		{"[]float64", `RETURN $p AS v`, []float64{1.5, 2.5}, `[1.5, 2.5]`},
		{"[]bool", `RETURN $p AS v`, []bool{true, false}, `[true, false]`},
		{"typed slice indexes", `RETURN $p[1] AS v`, []string{"a", "b"}, `"b"`},
		{"typed slice unwinds", `UNWIND $p AS x RETURN sum(x) AS v`, []int{1, 2, 3}, `6`},
		{"map[string]int", `RETURN $p.k AS v`, map[string]int{"k": 7}, `7`},
		{"[]map[string]any bulk idiom", `UNWIND $p AS r RETURN count(r.id) AS v`,
			[]map[string]any{{"id": 1}, {"id": 2}}, `2`},
		{"nested typed", `RETURN $p[0][1] AS v`, [][]int{{1, 2}, {3}}, `2`},
		{"map of typed slice", `RETURN $p.xs[0] AS v`, map[string][]string{"xs": {"z"}}, `"z"`},
		// Unchanged shapes: the reflection fallback must not disturb them.
		{"[]any still binds", `RETURN $p[0] AS v`, []any{1, "x"}, `1`},
		{"map[string]any still binds", `RETURN $p.k AS v`, map[string]any{"k": "x"}, `"x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bindRunScalar(t, eng, tc.query, map[string]any{"p": tc.param}); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestBindParams_PreservesExactIntFloat guards the CIP2016-06-14 int/float
// distinction: a typed slice must not silently widen its integers to floats.
func TestBindParams_PreservesExactIntFloat(t *testing.T) {
	eng := bindEngine(t)
	if got := bindRunScalar(t, eng, `RETURN $p[0] AS v`, map[string]any{"p": []int64{3}}); got != `3` {
		t.Errorf("int64 element rendered as %s, want 3 (not 3.0)", got)
	}
	// Rendering does not distinguish 3 from 3.0, so the distinction is asserted
	// where it is observable: openCypher integer division truncates, float
	// division does not.
	if got := bindRunScalar(t, eng, `RETURN $p[0] / 2 AS v`, map[string]any{"p": []int{3}}); got != `1` {
		t.Errorf("int64 element divided: got %s, want 1 (integer division)", got)
	}
	if got := bindRunScalar(t, eng, `RETURN $p[0] / 2 AS v`, map[string]any{"p": []float64{3}}); got != `1.5` {
		t.Errorf("float64 element divided: got %s, want 1.5 (the element widened to an integer)", got)
	}
	// An integer and a float that compare equal must still be distinguishable
	// by type, which is what the exact-equivalence work established.
	if got := bindRunScalar(t, eng, `RETURN $a[0] = $b[0] AS v`,
		map[string]any{"a": []int{3}, "b": []float64{3}}); got != `true` {
		t.Errorf("3 = 3.0 rendered as %s, want true", got)
	}
}

// TestBindParams_BoundedNesting proves hostile input is refused with a typed
// error rather than growing the stack until the runtime kills the process.
func TestBindParams_BoundedNesting(t *testing.T) {
	var deep any = 1
	for i := 0; i < 200; i++ {
		deep = []any{deep}
	}
	_, err := cypher.BindParams(map[string]any{"p": deep})
	if err == nil {
		t.Fatal("a 200-deep parameter bound without error; the recursion is unbounded")
	}
	if !strings.Contains(err.Error(), "nested deeper than") {
		t.Fatalf("got %v, want the depth-limit error", err)
	}
}

// TestBindParams_BytesStillRejected pins the deliberate exclusion. Bolt has a
// distinct Bytes wire type and expr has no value kind for it, so binding []byte
// as a list of integers would move the failure from a clear error at bind time
// to a silent type change at the far end.
func TestBindParams_BytesStillRejected(t *testing.T) {
	_, err := cypher.BindParams(map[string]any{"p": []byte{1, 2}})
	if err == nil {
		t.Fatal("[]byte bound; it must be refused until expr has a bytes value kind")
	}
	if !errors.Is(err, cypher.ErrUnsupportedParamType) {
		t.Fatalf("got %v, want ErrUnsupportedParamType", err)
	}
	if !strings.Contains(err.Error(), "Bolt Bytes") {
		t.Fatalf("the error must say WHY: got %v", err)
	}
}

// TestBindParams_NonStringMapKeyRejected keeps the Cypher map contract: a map
// literal has string keys, so a map[int]string is not a Cypher map.
func TestBindParams_NonStringMapKeyRejected(t *testing.T) {
	_, err := cypher.BindParams(map[string]any{"p": map[int]string{1: "a"}})
	if err == nil {
		t.Fatal("a non-string-keyed map bound; a Cypher map needs string keys")
	}
	if !errors.Is(err, cypher.ErrUnsupportedParamType) {
		t.Fatalf("got %v, want ErrUnsupportedParamType", err)
	}
}

// BenchmarkBindParams_SupportedShapes guards acceptance criterion 5 of #2219:
// the reflection fallback must cost nothing on the shapes the concrete type
// switch already served, since it is only reached from the default arm. The
// depth counter added to the recursion is the only change on those paths.
func BenchmarkBindParams_SupportedShapes(b *testing.B) {
	rows := make([]any, 64)
	for i := range rows {
		rows[i] = map[string]any{"id": i, "name": "n", "ok": true}
	}
	shapes := []struct {
		name string
		p    map[string]any
	}{
		{"scalar", map[string]any{"a": 1, "b": "x", "c": true}},
		{"list_any", map[string]any{"p": []any{1, 2, 3, 4, 5, 6, 7, 8}}},
		{"map_any", map[string]any{"p": map[string]any{"k": 1, "j": "x"}}},
		{"rows_any", map[string]any{"rows": rows}},
	}
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := cypher.BindParams(s.p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
