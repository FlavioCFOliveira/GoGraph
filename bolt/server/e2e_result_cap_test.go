package server_test

// e2e_result_cap_test.go — task #1293: the Bolt server must surface the engine's
// result-row cap to clients as a clean, typed FAILURE instead of materialising an
// unbounded result set.
//
// The engine applies a finite MaxResultRows inside its visibility barrier during
// materialisation (cypher.Engine.RunInTx → execUnderBarrier → Result.materialize),
// so the cap trips and Result.Err() reports cypher.ErrResultRowsExceeded BEFORE
// any surplus RECORD is chunked onto the wire. This test drives that path end to
// end through the neo4j-go-driver and asserts:
//
//  1. A read query that would produce more rows than the cap fails with a typed
//     Neo4jError whose code is Neo.ClientError.General.LimitExceeded (the same
//     resource-limit code the per-connection cursor cap uses) and whose message
//     names the row limit rather than the opaque internal-error text.
//  2. The connection stays healthy: the SAME session, reused after the failure
//     (the driver issues RESET internally on the pooled connection), runs a
//     subsequent simple query successfully.
//  3. No goroutine leak (enforced by TestMain via goleak) and race-clean.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// startCappedServer starts an isolated bolt/server.Server whose engine enforces
// the given finite result-row cap, and returns its address. It mirrors
// startTestServer but injects a purpose-built engine instead of the default
// uncapped-by-test one, because the cap is the subject under test. Cleanup of the
// Serve goroutine is registered via t.Cleanup.
func startCappedServer(t *testing.T, maxRows int64) string {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: maxRows})
	return startTestServerWithEngine(t, eng, server.Options{
		ConnTimeout: 10 * time.Second,
		Auth:        server.NoAuthHandler{},
	})
}

// newDriverForAddr connects a neo4j-go-driver v5 driver to an already-running
// server at addr and registers its Close via t.Cleanup.
func newDriverForAddr(t *testing.T, addr string) neo4j.DriverWithContext {
	t.Helper()
	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 5
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Logf("driver.Close: %v", err)
		}
	})
	return driver
}

// TestE2E_ResultRowCap_MappedFailure is the acceptance test for task #1293's
// first deliverable. With an engine capped at a small MaxResultRows, a client
// RUN/PULL of a query that would yield more rows receives a typed FAILURE mapped
// from cypher.ErrResultRowsExceeded — not an unbounded materialisation and not a
// generic UnknownError — and the connection survives to serve a follow-up query.
func TestE2E_ResultRowCap_MappedFailure(t *testing.T) {
	const cap = 100
	ctx := context.Background()

	addr := startCappedServer(t, cap)
	driver := newDriverForAddr(t, addr)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// ── AC#1: a query that produces more than `cap` rows must fail cleanly. ──
	// UNWIND range(...) needs no seeded data and deterministically produces
	// cap+1 rows under the same in-barrier materialise path a whole-graph MATCH
	// would take, so the cap trips before the surplus row reaches the wire.
	result, err := session.Run(ctx,
		"UNWIND range(1, $n) AS x RETURN x",
		map[string]any{"n": cap + 1},
	)
	var failErr error
	if err != nil {
		// The driver may surface the failure already at Run (RUN+PULL are
		// pipelined) or while draining; accept either.
		failErr = err
	} else {
		for result.Next(ctx) {
		}
		failErr = result.Err()
	}
	if failErr == nil {
		t.Fatal("AC#1: expected a FAILURE for a query exceeding the result-row cap, got nil")
	}

	var neoErr *neo4j.Neo4jError
	if !errors.As(failErr, &neoErr) {
		t.Fatalf("AC#1: expected *neo4j.Neo4jError, got %T: %v", failErr, failErr)
	}
	if neoErr.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Errorf("AC#1: FAILURE code = %q, want Neo.ClientError.General.LimitExceeded", neoErr.Code)
	}
	// The message must reflect the row cap (mapped from ErrResultRowsExceeded),
	// not the generic internal-error text. ErrResultRowsExceeded.Error() is
	// "cypher: result row limit exceeded".
	if msg := neoErr.Msg; !strings.Contains(msg, "row limit") {
		t.Errorf("AC#1: FAILURE message %q does not reflect the row-cap (want substring %q)", msg, "row limit")
	}
	if strings.Contains(neoErr.Msg, "An internal error occurred") {
		t.Errorf("AC#1: row-cap FAILURE must not be reported as a generic internal error, got %q", neoErr.Msg)
	}
	t.Logf("AC#1: row-cap FAILURE code=%q msg=%q", neoErr.Code, neoErr.Msg)

	// ── AC#2: the same session recovers (driver RESETs the pooled connection). ──
	// A follow-up simple query under the cap must succeed, proving the
	// per-connection handler stayed alive and the session returned to Ready.
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result2, err := session.Run(recoverCtx, "RETURN 1 AS ok", nil)
	if err != nil {
		t.Fatalf("AC#2: same session after row-cap failure: session.Run: %v", err)
	}
	if !result2.Next(recoverCtx) {
		t.Fatal("AC#2: same session recovery: Next returned false")
	}
	v, ok := result2.Record().Get("ok")
	if !ok || v.(int64) != 1 {
		t.Errorf("AC#2: same session recovery: got %v, want 1", v)
	}
	if _, err := result2.Consume(recoverCtx); err != nil {
		t.Fatalf("AC#2: same session recovery: Consume: %v", err)
	}
	t.Log("AC#2: same session served a follow-up query after the row-cap failure — connection stayed healthy")
}

// startByteCappedServer starts an isolated bolt/server.Server whose engine
// enforces the given finite aggregate-byte budget (with the row cap left at its
// finite default, so only the byte budget can trip). It is the byte-budget
// analogue of startCappedServer (#1328).
func startByteCappedServer(t *testing.T, maxBytes int64) string {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultBytes: maxBytes})
	return startTestServerWithEngine(t, eng, server.Options{
		ConnTimeout: 10 * time.Second,
		Auth:        server.NoAuthHandler{},
	})
}

// TestE2E_ResultByteBudget_MappedFailure is the byte-budget analogue of
// TestE2E_ResultRowCap_MappedFailure (#1328). With an engine whose aggregate-byte
// budget is small but whose row cap is the finite default, a client RUN/PULL of a
// query that produces a modest number of WIDE rows (each a long list, so the
// aggregate byte estimate exceeds the budget while the row COUNT stays well under
// the row cap) receives a typed FAILURE mapped from cypher.ErrResultBytesExceeded
// — the same Neo.ClientError.General.LimitExceeded code the row cap uses — and the
// connection survives to serve a follow-up query. This proves the new byte budget
// reaches the wire as a clean typed limit, not an unbounded materialisation.
func TestE2E_ResultByteBudget_MappedFailure(t *testing.T) {
	const budget = 8 * 1024 // 8 KiB: far below the wide result, far above one row
	ctx := context.Background()

	addr := startByteCappedServer(t, budget)
	driver := newDriverForAddr(t, addr)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// 50 rows, each a 200-element list (~160 KiB aggregate estimate) — needs no
	// seeded data and stays far under the default row cap, so only the byte budget
	// can trip, exercising the new guard rather than the row cap.
	result, err := session.Run(ctx,
		"UNWIND range(1, 50) AS x RETURN [i IN range(1, 200) | i] AS big", nil)
	var failErr error
	if err != nil {
		failErr = err
	} else {
		for result.Next(ctx) {
		}
		failErr = result.Err()
	}
	if failErr == nil {
		t.Fatal("expected a FAILURE for a query exceeding the result-byte budget, got nil")
	}

	var neoErr *neo4j.Neo4jError
	if !errors.As(failErr, &neoErr) {
		t.Fatalf("expected *neo4j.Neo4jError, got %T: %v", failErr, failErr)
	}
	if neoErr.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Errorf("FAILURE code = %q, want Neo.ClientError.General.LimitExceeded", neoErr.Code)
	}
	// The message must reflect the byte budget (mapped from ErrResultBytesExceeded:
	// "cypher: result byte budget exceeded"), not the generic internal-error text.
	if msg := neoErr.Msg; !strings.Contains(msg, "byte budget") {
		t.Errorf("FAILURE message %q does not reflect the byte budget (want substring %q)", msg, "byte budget")
	}
	if strings.Contains(neoErr.Msg, "An internal error occurred") {
		t.Errorf("byte-budget FAILURE must not be reported as a generic internal error, got %q", neoErr.Msg)
	}
	t.Logf("byte-budget FAILURE code=%q msg=%q", neoErr.Code, neoErr.Msg)

	// The same session must recover and serve a follow-up query under the budget.
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result2, err := session.Run(recoverCtx, "RETURN 1 AS ok", nil)
	if err != nil {
		t.Fatalf("same session after byte-budget failure: session.Run: %v", err)
	}
	if !result2.Next(recoverCtx) {
		t.Fatal("same session recovery: Next returned false")
	}
	v, ok := result2.Record().Get("ok")
	if !ok || v.(int64) != 1 {
		t.Errorf("same session recovery: got %v, want 1", v)
	}
	if _, err := result2.Consume(recoverCtx); err != nil {
		t.Fatalf("same session recovery: Consume: %v", err)
	}
}

// TestE2E_ResultRowCap_UnderCapSucceeds is the negative control: a query whose
// row count is at or below the cap must stream every row successfully, proving
// the cap rejects only genuine over-budget queries and is not a blanket failure.
func TestE2E_ResultRowCap_UnderCapSucceeds(t *testing.T) {
	const cap = 100
	ctx := context.Background()

	addr := startCappedServer(t, cap)
	driver := newDriverForAddr(t, addr)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	result, err := session.Run(ctx,
		"UNWIND range(1, $n) AS x RETURN x",
		map[string]any{"n": cap},
	)
	if err != nil {
		t.Fatalf("under-cap query: session.Run: %v", err)
	}
	records, err := result.Collect(ctx)
	if err != nil {
		t.Fatalf("under-cap query: Collect: %v", err)
	}
	if len(records) != cap {
		t.Errorf("under-cap query: got %d rows, want %d", len(records), cap)
	}
}
