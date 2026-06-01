package cypher_test

// api_ctx_cancel_test.go — #1236 (security hardening I5)
//
// The engine's query entrypoints (Engine.Run / Engine.RunInTx, and therefore
// the RunAny / RunInTxAny that delegate to them) consult the caller's context
// BEFORE any synchronous parse/plan/Begin work. A caller whose context is
// already cancelled or whose deadline has elapsed is answered promptly with a
// context error — it never pays for an expensive-to-parse-but-valid query whose
// worst case the parser's length/nesting guards only bound, not eliminate.
//
// These tests assert (a) an already-cancelled / already-expired context returns
// promptly with a matchable context error and does NOT proceed to parse or
// execute, and (b) a live context still runs a normal query unchanged.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"testing"
	"time"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newCtxTestEngine builds a small in-memory engine for the cancellation tests.
func newCtxTestEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// TestRun_CancelledContext_ReturnsPromptlyBeforeParse asserts that Engine.Run
// with an already-cancelled context returns a context.Canceled error before any
// parse/plan/execute work. The query is deliberately malformed: if Run reached
// the parser it would surface a "cypher: parse" error rather than the context
// error, so observing context.Canceled proves the entry guard short-circuited
// ahead of parseAndAnalyse.
func TestRun_CancelledContext_ReturnsPromptlyBeforeParse(t *testing.T) {
	t.Parallel()
	eng := newCtxTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call

	// A syntactically invalid query: only an entry-guard short-circuit (not the
	// parser) can return a context error for this input.
	res, err := eng.Run(ctx, `MATCH (n RETURN`, nil)
	if res != nil {
		t.Fatalf("Run on cancelled ctx returned a non-nil Result; want nil")
	}
	if err == nil {
		t.Fatalf("Run on cancelled ctx returned nil error; want context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on cancelled ctx: err = %v; want errors.Is(err, context.Canceled)", err)
	}
}

// TestRunInTx_CancelledContext_ReturnsPromptlyBeforeBegin asserts the same guard
// for the write entrypoint: an already-cancelled context returns context.Canceled
// before parseAndAnalyse and before any txn.Store.Begin, so no write transaction
// is opened. The query is a write that would otherwise be parsed and planned.
func TestRunInTx_CancelledContext_ReturnsPromptlyBeforeBegin(t *testing.T) {
	t.Parallel()
	eng := newCtxTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call

	res, err := eng.RunInTx(ctx, `CREATE (n:Person {name: 'never'})`, nil)
	if res != nil {
		t.Fatalf("RunInTx on cancelled ctx returned a non-nil Result; want nil")
	}
	if err == nil {
		t.Fatalf("RunInTx on cancelled ctx returned nil error; want context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunInTx on cancelled ctx: err = %v; want errors.Is(err, context.Canceled)", err)
	}
}

// TestRun_ExpiredDeadline_ReturnsDeadlineExceeded asserts the deadline variant:
// a context whose deadline has already elapsed returns context.DeadlineExceeded
// from the entry guard.
func TestRun_ExpiredDeadline_ReturnsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	eng := newCtxTestEngine(t)

	// A deadline in the past: ctx.Err() reports DeadlineExceeded immediately.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	res, err := eng.Run(ctx, `MATCH (n) RETURN n`, nil)
	if res != nil {
		t.Fatalf("Run on expired ctx returned a non-nil Result; want nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run on expired ctx: err = %v; want errors.Is(err, context.DeadlineExceeded)", err)
	}
}

// TestRunAny_CancelledContext_InheritsGuard asserts that the entry guard is
// inherited by the delegating RunAny entrypoint (read path here, since the query
// has no writing clause): an already-cancelled context returns context.Canceled.
func TestRunAny_CancelledContext_InheritsGuard(t *testing.T) {
	t.Parallel()
	eng := newCtxTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := eng.RunAny(ctx, `MATCH (n) RETURN n`, nil)
	if res != nil {
		t.Fatalf("RunAny on cancelled ctx returned a non-nil Result; want nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAny on cancelled ctx: err = %v; want errors.Is(err, context.Canceled)", err)
	}
}

// TestRun_LiveContext_NormalQueryUnaffected asserts a normal (non-cancelled)
// query still parses, executes, and returns its row unchanged after the entry
// guard was added. The query is constant-folding only (no graph data needed),
// so it must yield exactly one row whose iteration succeeds with no error.
func TestRun_LiveContext_NormalQueryUnaffected(t *testing.T) {
	t.Parallel()
	eng := newCtxTestEngine(t)

	res, err := eng.Run(context.Background(), `RETURN 1 AS one`, nil)
	if err != nil {
		t.Fatalf("Run on live ctx: unexpected error: %v", err)
	}
	if res == nil {
		t.Fatalf("Run on live ctx returned nil Result; want a result")
	}

	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Run on live ctx: result error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Run on live ctx: close: %v", err)
	}
	if rows != 1 {
		t.Fatalf("Run on live ctx: got %d rows; want 1", rows)
	}
}

// TestRunInTx_LiveContext_NormalWriteUnaffected asserts a normal (non-cancelled)
// write query still parses, opens its transaction, and commits unchanged after
// the entry guard was added.
func TestRunInTx_LiveContext_NormalWriteUnaffected(t *testing.T) {
	t.Parallel()
	eng := newCtxTestEngine(t)

	res, err := eng.RunInTx(context.Background(), `CREATE (n:Person {name: 'live'}) RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("RunInTx on live ctx: unexpected error: %v", err)
	}
	if res == nil {
		t.Fatalf("RunInTx on live ctx returned nil Result; want a result")
	}

	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("RunInTx on live ctx: result error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("RunInTx on live ctx: close: %v", err)
	}
	if rows != 1 {
		t.Fatalf("RunInTx on live ctx: got %d rows; want 1", rows)
	}
}
