package cypher_test

// index_seek_param_type_test.go — regression for bug #1125.
//
// A parameterised equality on an INTEGER property that lowers to a
// NodeByIndexSeek must NOT be rejected by parameter type-checking with
// "cypher: parameter $id: expected String value, got Integer". The inferred
// parameter type follows the indexed property's key type, not a hard-coded
// String.
//
// The fix has two coordinated parts, exercised here:
//
//   - sema.InferParamTypes consults the index that backs the property: an
//     int64 hash index proves the property is Integer, so an Integer parameter
//     is accepted and a String parameter is rejected (symmetric with the
//     pre-existing rejection of an Integer parameter against a String-keyed
//     property — see TestRun_ParamTypeMismatch_Error).
//   - tryNewHashSeek declines a seek whose value kind does not match the index
//     key type, so a type-incompatible *literal* (which the parameter type
//     check never sees) falls back to scan+filter and yields zero rows per
//     openCypher, instead of failing at operator Init.
//
// The graph and index helpers (buildAgeGraph, installAgeIndex, personAgeEntry)
// live in scan_index_hash_int64_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/cypher/sema"
)

func TestIndexSeekParam_IntegerPropertyAcceptsIntegerParam(t *testing.T) {
	t.Parallel()

	persons := []personAgeEntry{
		{"Alice", 30},
		{"Bob", 25},
		{"Carol", 35},
		{"Dave", 30},
	}
	g := buildAgeGraph(t, persons)
	eng := cypher.NewEngine(g) // installs index.Manager on g
	installAgeIndex(g)         // hash.Index[int64] named "age_hash"

	const q = "MATCH (n:Person) WHERE n.age = $age RETURN n.name"
	param := map[string]expr.Value{"age": expr.IntegerValue(30)}

	// The planner must actually pick an index seek for this shape, otherwise the
	// test would not exercise the seek path. Explain must be given the parameter
	// value, since the seek can only resolve a bound parameter.
	plan, err := eng.Explain(q, param)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("expected NodeByIndexSeek in plan; got:\n%s", plan)
	}

	res, err := eng.Run(context.Background(), q, param)
	if err != nil {
		t.Fatalf("Run with integer param over integer index seek: %v", err)
	}
	defer res.Close() //nolint:errcheck // test cleanup

	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("age=30: want 2 rows (Alice, Dave), got %d", len(rows))
	}
	names := make(map[string]bool, len(rows))
	for _, row := range rows {
		sv, ok := row["n.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("n.name: expected StringValue, got %T", row["n.name"])
		}
		names[string(sv)] = true
	}
	for _, want := range []string{"Alice", "Dave"} {
		if !names[want] {
			t.Errorf("missing expected name %q; got %v", want, names)
		}
	}
}

func TestIndexSeekParam_StringPropertyStillAcceptsStringParam(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(10, true) // string "name" hash index

	const q = "MATCH (n:Person) WHERE n.name = $name RETURN n"
	param := map[string]expr.Value{"name": expr.StringValue("Alice")}

	plan, err := eng.Explain(q, param)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("expected NodeByIndexSeek in plan; got:\n%s", plan)
	}

	res, err := eng.Run(context.Background(), q, param)
	if err != nil {
		t.Fatalf("Run with string param over string index seek: %v", err)
	}
	defer res.Close() //nolint:errcheck // test cleanup

	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("name=Alice: want 1 row, got %d", len(rows))
	}
}

func TestIndexSeekParam_StringParamAgainstIntegerPropertyRejected(t *testing.T) {
	t.Parallel()

	persons := []personAgeEntry{{"Alice", 30}, {"Bob", 25}}
	g := buildAgeGraph(t, persons)
	eng := cypher.NewEngine(g)
	installAgeIndex(g)

	// The int64 index proves n.age is Integer; a String parameter is a genuine
	// type error and must be rejected — symmetric with TestRun_ParamTypeMismatch_Error,
	// which rejects an Integer parameter against a String-keyed property. This
	// engine deliberately surfaces a type-incompatible parameter as a typed
	// error rather than silently matching nothing.
	_, err := eng.Run(context.Background(),
		"MATCH (n:Person) WHERE n.age = $age RETURN n.name",
		map[string]expr.Value{"age": expr.StringValue("30")})
	if err == nil {
		t.Fatal("expected param-type error for string param against integer property")
	}
	var pte *sema.ParamTypeError
	if !errors.As(err, &pte) {
		t.Fatalf("expected *sema.ParamTypeError, got %T: %v", err, err)
	}
	if pte.Expected != expr.KindInteger {
		t.Errorf("Expected = %v, want KindInteger", pte.Expected)
	}
	if pte.Got != expr.KindString {
		t.Errorf("Got = %v, want KindString", pte.Got)
	}
}

func TestIndexSeekParam_LiteralTypeMismatchFallsBackToZeroRows(t *testing.T) {
	t.Parallel()

	persons := []personAgeEntry{{"Alice", 30}, {"Bob", 25}}
	g := buildAgeGraph(t, persons)
	eng := cypher.NewEngine(g)
	installAgeIndex(g)

	// A type-incompatible *literal* equality is not seen by the parameter type
	// check. Per openCypher, n.age (Integer) = 'thirty' (String) is false, so
	// the result is zero rows with no error. The seek declines the mismatched
	// value kind and the scan+filter produces the spec-faithful empty result.
	res, err := eng.Run(context.Background(),
		"MATCH (n:Person {age: 'thirty'}) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("literal string vs integer index: unexpected error: %v", err)
	}
	defer res.Close() //nolint:errcheck // test cleanup

	rows := collectRecords(t, res)
	if len(rows) != 0 {
		t.Errorf("literal type mismatch: want 0 rows, got %d", len(rows))
	}
}

// TestIndexSeekParam_IntegerParamNotForcedString guards the sema layer
// specifically: an integer parameter must not be rejected by a
// *sema.ParamTypeError when an int64 index types the property as Integer.
func TestIndexSeekParam_IntegerParamNotForcedString(t *testing.T) {
	t.Parallel()

	persons := []personAgeEntry{{"Alice", 30}}
	g := buildAgeGraph(t, persons)
	eng := cypher.NewEngine(g)
	installAgeIndex(g)

	_, err := eng.Run(context.Background(),
		"MATCH (n:Person) WHERE n.age = $age RETURN n.name",
		map[string]expr.Value{"age": expr.IntegerValue(30)})
	var pte *sema.ParamTypeError
	if errors.As(err, &pte) {
		t.Fatalf("integer param wrongly rejected by param type check: %v", err)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
