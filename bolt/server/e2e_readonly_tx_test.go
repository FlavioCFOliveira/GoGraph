package server_test

// e2e_readonly_tx_test.go — task #1573: a Bolt session that opens a read-only
// transaction (BEGIN with mode="r", driven by neo4j.AccessModeRead) can read,
// and is refused on a write with a Failure carrying the
// "Neo.ClientError.Request.Invalid" code. The read-only path routes through
// cypher.Engine.BeginReadTx, which holds no writer lock or barrier and rejects
// any writing/DDL statement before execution.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_ReadOnlyTx_ReadsAndRefusesWrites opens a read-mode session, seeds data
// via a separate write session, then verifies the read-mode transaction reads
// successfully and a write inside it is rejected with the request-invalid code.
func TestE2E_ReadOnlyTx_ReadsAndRefusesWrites(t *testing.T) {
	ctx := context.Background()
	driver, addr := newDriverForTest(t)

	// Seed one node through an ordinary (write) session.
	seedSession := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer seedSession.Close(ctx) //nolint:errcheck
	runWrite(ctx, t, seedSession, `CREATE (:RO {v: $v})`, map[string]any{"v": int64(42)})

	// A read-mode session: the driver tags BEGIN with mode="r".
	readSession := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer readSession.Close(ctx) //nolint:errcheck

	tx, err := readSession.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction (read mode): %v", err)
	}

	// AC: a read succeeds inside the read-only transaction.
	res, err := tx.Run(ctx, `MATCH (n:RO) RETURN n.v AS v`, nil)
	if err != nil {
		t.Fatalf("read tx.Run: %v", err)
	}
	rows, err := res.Collect(ctx)
	if err != nil {
		t.Fatalf("read Collect: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read returned %d rows, want 1", len(rows))
	}
	if got, _ := rows[0].AsMap()["v"].(int64); got != 42 {
		t.Fatalf("read v = %v, want 42", rows[0].AsMap()["v"])
	}

	// AC: a write inside the read-only transaction is refused with the
	// request-invalid code. The error may surface from Run or from draining the
	// cursor depending on driver buffering; capture both.
	wres, werr := tx.Run(ctx, `CREATE (:Forbidden)`, nil)
	var failErr error
	if werr != nil {
		failErr = werr
	} else {
		for wres.Next(ctx) {
		}
		failErr = wres.Err()
	}
	if failErr == nil {
		t.Fatal("write inside read-only tx was not refused")
	}

	var neoErr *neo4j.Neo4jError
	if !errors.As(failErr, &neoErr) {
		t.Fatalf("expected *neo4j.Neo4jError, got %T: %v", failErr, failErr)
	}
	if neoErr.Code != "Neo.ClientError.Request.Invalid" {
		t.Fatalf("write-refusal code = %q, want Neo.ClientError.Request.Invalid", neoErr.Code)
	}
	if !strings.Contains(strings.ToLower(neoErr.Msg), "read-only") {
		t.Errorf("write-refusal message %q does not mention read-only", neoErr.Msg)
	}

	// The transaction is now FAILED; roll it back to release the handle cleanly.
	_ = tx.Rollback(ctx)

	// The graph must be unchanged: the forbidden write never executed. Use a
	// FRESH driver for the check so its connection pool cannot hand back the
	// read-mode connection that was just driven into a failed/reset state.
	verifyDriver := newDriverForAddr(t, addr)
	verifySession := verifyDriver.NewSession(ctx, neo4j.SessionConfig{})
	defer verifySession.Close(ctx) //nolint:errcheck
	check := runRead(ctx, t, verifySession, `MATCH (n:Forbidden) RETURN count(n) AS c`, nil)
	if len(check) != 1 {
		t.Fatalf("verify returned %d rows, want 1", len(check))
	}
	if got, _ := check[0]["c"].(int64); got != 0 {
		t.Fatalf("Forbidden node count = %v, want 0 (write must not have executed)", check[0]["c"])
	}
}
