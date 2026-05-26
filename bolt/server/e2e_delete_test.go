package server_test

// e2e_delete_test.go — T807: DELETE and DETACH DELETE via neo4j-go-driver.
//
// AC#1: DELETE on a node that has relationships must return a client error.
// AC#2: DETACH DELETE deletes the node and its relationship(s).
// AC#3: The other endpoint node survives.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_DeleteDetachDelete seeds (a:A)-[:R]->(b:B), then:
//  1. Attempts DELETE a (must fail with a client error).
//  2. Runs DETACH DELETE a.
//  3. Verifies b still exists.
func TestE2E_DeleteDetachDelete(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// Seed the graph.
	runWrite(ctx, t, session,
		`CREATE (a:A {name:'alice'})-[:R]->(b:B {name:'bob'})`,
		nil,
	)

	// AC#1: DELETE a when a has relationships must return a client error.
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, `MATCH (a:A {name:'alice'}) DELETE a`, nil)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
	if err == nil {
		t.Error("DELETE on node with relationships: expected error, got nil")
	} else {
		// The server returns a Failure for this condition.
		t.Logf("DELETE on node with relationships returned error (expected): %v", err)
	}

	// The session may be in a failed state; open a new session for subsequent ops.
	session2 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session2.Close(ctx) //nolint:errcheck

	// Verify graph is unchanged after the failed DELETE.
	countA := runRead(ctx, t, session2, `MATCH (a:A {name:'alice'}) RETURN count(a) AS cnt`, nil)
	if cnt, _ := countA[0]["cnt"].(int64); cnt != 1 {
		t.Errorf("after failed DELETE: node a count = %d, want 1", cnt)
	}

	// AC#2: DETACH DELETE a.
	runWrite(ctx, t, session2,
		`MATCH (a:A {name:'alice'}) DETACH DELETE a`,
		nil,
	)

	// Verify a is gone and the relationship is gone.
	countA2 := runRead(ctx, t, session2, `MATCH (a:A {name:'alice'}) RETURN count(a) AS cnt`, nil)
	if cnt, _ := countA2[0]["cnt"].(int64); cnt != 0 {
		t.Errorf("after DETACH DELETE: node a count = %d, want 0", cnt)
	}

	countR := runRead(ctx, t, session2, `MATCH ()-[r:R]->() RETURN count(r) AS cnt`, nil)
	if cnt, _ := countR[0]["cnt"].(int64); cnt != 0 {
		t.Errorf("after DETACH DELETE: relationship count = %d, want 0", cnt)
	}

	// AC#3: Node b survives.
	countB := runRead(ctx, t, session2, `MATCH (b:B {name:'bob'}) RETURN count(b) AS cnt`, nil)
	if cnt, _ := countB[0]["cnt"].(int64); cnt != 1 {
		t.Errorf("after DETACH DELETE a: node b count = %d, want 1", cnt)
	}
}
