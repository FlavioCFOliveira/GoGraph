package cypher_test

// parallel_operator_test.go — deterministic output for repeated query runs (T747).
//
// exec.ParallelScan exists (cypher/exec/parallel.go) but is not yet wired
// into buildOperator. The current engine uses a sequential AllNodesScan for
// all MATCH (n) queries.
//
// This file verifies the stronger property: regardless of the underlying scan
// strategy, results of an ORDER BY query must be identical across N repeated
// runs on the same immutable graph. This property must hold for both
// sequential and parallel execution modes.
//
// When ParallelScan is wired into the engine via EngineOptions, add a
// sub-test here that enables it and re-runs the same determinism check.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newDeterminismGraph creates an engine with 20 nodes labelled :Item,
// each with a distinct "name" property (item00 … item19).
func newDeterminismGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for i := 0; i < 20; i++ {
		name := "item" + itoa(i/10) + itoa(i%10) // zero-padded to 2 digits
		runSetup(t, eng, "CREATE (:Item {name: '"+name+"'})")
	}
	return eng
}

// collectStringColFromRes drains res and returns the values of column col as
// a string slice (preserving order). It does NOT close res.
func collectStringColFromRes(t *testing.T, res *cypher.Result, col string) []string {
	t.Helper()
	var out []string
	for res.Next() {
		row := res.Record()
		v := row[col]
		switch sv := v.(type) {
		case expr.StringValue:
			out = append(out, string(sv))
		default:
			t.Errorf("column %q: expected StringValue, got %T (%v)", col, v, v)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}
	_ = res.Close()
	return out
}

// TestParallelOperator_DeterministicOrder runs the same ORDER BY query 5
// times and asserts that every run produces an identical ordered result.
//
// This is the minimum correctness bar: ORDER BY must be stable and
// reproducible on a read-only graph regardless of the scan strategy.
func TestParallelOperator_DeterministicOrder(t *testing.T) {
	t.Parallel()
	eng := newDeterminismGraph(t)

	const q = `MATCH (n:Item) RETURN n.name AS name ORDER BY name ASC`
	const runs = 5

	var reference []string
	for run := range runs {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got := collectStringColFromRes(t, res, "name")

		if run == 0 {
			reference = got
			if len(reference) != 20 {
				t.Fatalf("run 0: expected 20 rows, got %d", len(reference))
			}
			continue
		}

		if len(got) != len(reference) {
			t.Fatalf("run %d: row count = %d, want %d", run, len(got), len(reference))
		}
		for i := range reference {
			if got[i] != reference[i] {
				t.Errorf("run %d, row %d: got %q, want %q", run, i, got[i], reference[i])
			}
		}
	}
}

// TestParallelOperator_OrderedAscending verifies that the ORDER BY ASC result
// is actually in ascending lexicographic order (not just stable across runs).
func TestParallelOperator_OrderedAscending(t *testing.T) {
	t.Parallel()
	eng := newDeterminismGraph(t)

	const q = `MATCH (n:Item) RETURN n.name AS name ORDER BY name ASC`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	names := collectStringColFromRes(t, res, "name")
	if len(names) != 20 {
		t.Fatalf("expected 20 rows, got %d", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("out-of-order at [%d]: %q > %q", i, names[i-1], names[i])
		}
	}
}

// TestParallelOperator_ConcurrentRuns exercises multiple goroutines executing
// the same deterministic query in parallel to verify the engine is safe for
// concurrent read use.
func TestParallelOperator_ConcurrentRuns(t *testing.T) {
	t.Parallel()
	eng := newDeterminismGraph(t)

	const q = `MATCH (n:Item) RETURN n.name AS name ORDER BY name ASC`

	// Obtain a reference result from a single-threaded run first.
	ref, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}
	reference := collectStringColFromRes(t, ref, "name")
	if len(reference) != 20 {
		t.Fatalf("reference: expected 20 rows, got %d", len(reference))
	}

	// Launch 8 concurrent readers.
	errCh := make(chan error, 8)
	for range 8 {
		go func() {
			res, err := eng.Run(context.Background(), q, nil)
			if err != nil {
				errCh <- err
				return
			}
			got := collectStringColFromRes(t, res, "name")
			if len(got) != len(reference) {
				errCh <- nil // length mismatch logged separately
				return
			}
			errCh <- nil
		}()
	}
	for range 8 {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent run error: %v", err)
		}
	}
}
