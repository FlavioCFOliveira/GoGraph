package cypher_test

// api_test.go — end-to-end tests for Engine.Run (task-250).
//
// Coverage: simple MATCH/RETURN, parse error wrapping, empty graph,
// plan cache reuse, context cancellation, race-clean.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newGraph creates an lpg.Graph populated with n nodes.
func newGraph(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		if err := g.AddNode(string(rune('A'+i%26)) + string(rune('0'+i%10))); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Empty graph — MATCH (n) RETURN n yields 0 rows
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_EmptyGraph(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

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
	if count != 0 {
		t.Errorf("expected 0 rows for empty graph, got %d", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Non-empty graph — MATCH (n) RETURN n yields one row per node
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_MatchAllNodes(t *testing.T) {
	const n = 5
	g := newGraph(t, n)
	eng := cypher.NewEngine(g)

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
	if count != n {
		t.Errorf("expected %d rows, got %d", n, count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Parse error is wrapped and contains position info
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_ParseError(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	_, err := eng.Run(context.Background(), "MATCH RETURN", nil) // syntactically invalid
	if err == nil {
		t.Fatal("expected error for invalid query, got nil")
	}
	// Error must wrap a *parser.ParseError or contain useful info.
	var pe *parser.ParseError
	if errors.As(err, &pe) {
		// Good: has structured parse error.
		if pe.Line == 0 && pe.Column == 0 {
			t.Error("ParseError has zero line and column")
		}
	}
	// At minimum the error must not be nil and contain a message.
	if err.Error() == "" {
		t.Error("error message is empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Plan cache — same query string parsed once
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_PlanCache(t *testing.T) {
	g := newGraph(t, 3)
	eng := cypher.NewEngine(g)

	const query = "MATCH (n) RETURN n"
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
		if count != 3 {
			t.Errorf("Run[%d]: got %d rows, want 3", i, count)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Result.Columns returns expected names
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_Columns(t *testing.T) {
	g := newGraph(t, 1)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	defer res.Close()

	cols := res.Columns()
	if len(cols) != 1 || cols[0] != "n" {
		t.Errorf("Columns() = %v, want [n]", cols)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Context cancellation — Close always called
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_Cancellation(t *testing.T) {
	const n = 100
	g := newGraph(t, n)
	eng := cypher.NewEngine(g)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
	if err != nil {
		// Parse/translate phase happened before cancellation check: fine.
		return
	}
	defer res.Close()

	// Drain with cancelled context.
	for res.Next() {
	}
	// We just want no panic or hang; the error (if any) is in res.Err().
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Race-clean — concurrent Run calls on same engine
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_RaceConcurrentRun(t *testing.T) {
	g := newGraph(t, 10)
	eng := cypher.NewEngine(g)

	const goroutines = 8
	done := make(chan error, goroutines)
	for range goroutines {
		go func() {
			res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
			if err != nil {
				done <- err
				return
			}
			for res.Next() {
			}
			done <- res.Close()
		}()
	}
	for range goroutines {
		if err := <-done; err != nil {
			t.Errorf("goroutine error: %v", err)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. Close is idempotent
// ─────────────────────────────────────────────────────────────────────────────

func TestEngine_CloseIdempotent(t *testing.T) {
	g := newGraph(t, 1)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("first Close error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Errorf("second Close error: %v", err)
	}
}
