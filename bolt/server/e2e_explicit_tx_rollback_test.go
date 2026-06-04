package server_test

// e2e_explicit_tx_rollback_test.go — T825 / #1280: Explicit transaction rollback.
//
// With true engine-level explicit transactions ([cypher.ExplicitTx]), a Bolt
// transaction's writes accumulate in one engine transaction and are unwound
// together by ROLLBACK via the in-memory undo log (#1282). The previously
// skipped acceptance criteria now hold:
//
//   - AC#1: after ROLLBACK the same session observes zero rows for the
//     rolled-back writes.
//   - AC#2: another session observes zero rows for the same writes.
//
// The test server is wired with the store-less engine (cypher.NewEngine), so
// durability does not apply (nothing is persisted); ROLLBACK's in-memory undo is
// what these assertions exercise. The WAL-backed durable-rollback path (a fresh
// recovery.Open observing the rolled-back node absent) is covered by the engine
// regression test cypher.TestExplicitTx_Rollback_DurableAbsent.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_ExplicitTxRollback opens an explicit transaction, executes writes,
// and rolls back, then asserts neither the rolling-back session nor a second
// session observes the rolled-back nodes.
func TestE2E_ExplicitTxRollback(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	tx, err := session.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}

	// Two writes inside the transaction.
	r1, err := tx.Run(ctx, `CREATE (n:RollbackNode {v: 1})`, nil)
	if err != nil {
		t.Fatalf("tx.Run (1): %v", err)
	}
	if _, err := r1.Consume(ctx); err != nil {
		t.Fatalf("r1.Consume: %v", err)
	}

	r2, err := tx.Run(ctx, `CREATE (n:RollbackNode {v: 2})`, nil)
	if err != nil {
		t.Fatalf("tx.Run (2): %v", err)
	}
	if _, err := r2.Consume(ctx); err != nil {
		t.Fatalf("r2.Consume: %v", err)
	}

	// Rollback — must succeed without error.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("tx.Rollback: %v", err)
	}

	// AC#1: the same session sees zero rows after rollback — the two eager writes
	// were unwound by the engine's accumulated undo log.
	rows1 := runRead(ctx, t, session, `MATCH (n:RollbackNode) RETURN count(n) AS cnt`, nil)
	if cnt, _ := rows1[0]["cnt"].(int64); cnt != 0 {
		t.Errorf("same-session post-rollback: count = %d, want 0", cnt)
	}

	// AC#2: a second session also sees zero rows.
	session2 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session2.Close(ctx) //nolint:errcheck

	rows2 := runRead(ctx, t, session2, `MATCH (n:RollbackNode) RETURN count(n) AS cnt`, nil)
	if cnt, _ := rows2[0]["cnt"].(int64); cnt != 0 {
		t.Errorf("other-session post-rollback: count = %d, want 0", cnt)
	}
}
