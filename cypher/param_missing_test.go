package cypher_test

// param_missing_test.go — regression gate for task #1431: a query that
// references a parameter ($name) that is not present in the caller-supplied
// params map must return a *sema.ErrParamMissing error rather than silently
// resolving the missing parameter to NULL.
//
// Before the fix, the checkParamTypes pass returned nil when len(params)==0,
// and the expression evaluator returned Null for any unbound $param. After
// the fix, CollectParamNames gathers all $param references at parse time and
// checkParamPresence rejects any query whose referenced names are absent.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newStorelessEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// TestParamMissing_ReturnParam verifies that RETURN $x with no params map
// returns *sema.ErrParamMissing instead of null.
func TestParamMissing_ReturnParam(t *testing.T) {
	eng := newStorelessEngine(t)

	res, err := eng.Run(context.Background(), "RETURN $x", nil)
	if res != nil {
		for res.Next() {
		}
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("expected ErrParamMissing, got nil error")
	}
	var pe *sema.ErrParamMissing
	if !errors.As(err, &pe) {
		t.Fatalf("expected *sema.ErrParamMissing, got %T: %v", err, err)
	}
	if pe.Name != "x" {
		t.Errorf("ErrParamMissing.Name = %q, want %q", pe.Name, "x")
	}
}

// TestParamMissing_WhereParam verifies that a missing $p in a WHERE clause
// is caught before execution.
func TestParamMissing_WhereParam(t *testing.T) {
	eng := newStorelessEngine(t)

	res, err := eng.Run(context.Background(),
		"MATCH (n) WHERE n.x = $p RETURN n", nil)
	if res != nil {
		for res.Next() {
		}
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("expected ErrParamMissing, got nil error")
	}
	var pe *sema.ErrParamMissing
	if !errors.As(err, &pe) {
		t.Fatalf("expected *sema.ErrParamMissing, got %T: %v", err, err)
	}
}

// TestParamMissing_PresentParamSucceeds confirms that when the referenced
// parameter IS supplied the query executes normally.
func TestParamMissing_PresentParamSucceeds(t *testing.T) {
	eng := newStorelessEngine(t)

	params := map[string]expr.Value{"x": expr.IntegerValue(5)}
	res, err := eng.Run(context.Background(), "RETURN $x", params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = res.Close() }()

	if !res.Next() {
		t.Fatal("expected one row, got none")
	}
	rec := res.Record()
	v, ok := rec["$x"]
	if !ok {
		t.Fatalf("result record has no $x key; keys: %v", rec)
	}
	if v != expr.IntegerValue(5) {
		t.Errorf("$x = %v, want 5", v)
	}
}

// TestParamMissing_PartialParams verifies that $y is reported missing when
// $x is present but $y is absent.
func TestParamMissing_PartialParams(t *testing.T) {
	eng := newStorelessEngine(t)

	params := map[string]expr.Value{"x": expr.IntegerValue(1)}
	res, err := eng.Run(context.Background(), "RETURN $x + $y", params)
	if res != nil {
		for res.Next() {
		}
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("expected ErrParamMissing for $y, got nil error")
	}
	var pe *sema.ErrParamMissing
	if !errors.As(err, &pe) {
		t.Fatalf("expected *sema.ErrParamMissing, got %T: %v", err, err)
	}
	if pe.Name != "y" {
		t.Errorf("ErrParamMissing.Name = %q, want %q", pe.Name, "y")
	}
}

// TestParamMissing_WriteQuery verifies that a write-only query referencing
// a missing parameter is also caught.
func TestParamMissing_WriteQuery(t *testing.T) {
	eng := newStorelessEngine(t)

	res, err := eng.RunInTx(context.Background(),
		"CREATE (n:Item {v: $val})", nil)
	if res != nil {
		for res.Next() {
		}
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("expected ErrParamMissing, got nil error")
	}
	var pe *sema.ErrParamMissing
	if !errors.As(err, &pe) {
		t.Fatalf("expected *sema.ErrParamMissing, got %T: %v", err, err)
	}
}
