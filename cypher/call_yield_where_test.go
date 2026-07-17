package cypher_test

// call_yield_where_test.go — regression test for the production-readiness audit
// backlog bug #1966.
//
// A WHERE predicate on a CALL ... YIELD sub-clause must filter the yielded rows
// (openCypher), exactly like WITH ... WHERE. The grammar parsed the WHERE onto
// the YieldItems rule but the visitor dropped it and the translator never
// applied it, so every yielded row survived. Fixed by capturing Call.Where in
// the visitor and lifting it as a Selection over the ProcedureCall.

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestCallYieldWhere_FiltersYieldedRows(t *testing.T) {
	t.Parallel()
	eng, ctx := newPlainEngine(t)
	writeMust(t, eng, `CREATE (:P {name: 'a', age: 1, city: 'x'})`)

	// WHERE on YIELD must filter to exactly the 'name' key.
	res, err := eng.RunAny(ctx, `CALL db.propertyKeys() YIELD propertyKey WHERE propertyKey = 'name' RETURN propertyKey AS k`, nil)
	if err != nil {
		t.Fatalf("RunAny: %v", err)
	}
	var got []string
	for res.Next() {
		if v, ok := res.Record()["k"].(expr.StringValue); ok {
			got = append(got, string(v))
		}
	}
	_ = res.Close()
	if len(got) != 1 || got[0] != "name" {
		t.Fatalf("CALL ... YIELD ... WHERE returned %v, want [name] (WHERE was ignored pre-fix)", got)
	}
}

// Control: without WHERE, all property keys are yielded.
func TestCallYield_NoWhere_YieldsAll(t *testing.T) {
	t.Parallel()
	eng, ctx := newPlainEngine(t)
	writeMust(t, eng, `CREATE (:P {name: 'a', age: 1, city: 'x'})`)

	res, err := eng.RunAny(ctx, `CALL db.propertyKeys() YIELD propertyKey RETURN propertyKey AS k`, nil)
	if err != nil {
		t.Fatalf("RunAny: %v", err)
	}
	var got []string
	for res.Next() {
		if v, ok := res.Record()["k"].(expr.StringValue); ok {
			got = append(got, string(v))
		}
	}
	_ = res.Close()
	sort.Strings(got)
	want := []string{"age", "city", "name"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("CALL ... YIELD (no WHERE) returned %v, want %v", got, want)
	}
}
