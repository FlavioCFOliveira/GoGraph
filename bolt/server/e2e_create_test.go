package server_test

// e2e_create_test.go — T767: CREATE single node via neo4j-go-driver session.
//
// Known server limitations:
//   - Summary counters (NodesCreated, LabelsAdded, PropertiesSet) always
//     return 0 because the server's PULL SUCCESS does not emit a "stats" key.
//     Write effects are verified via a subsequent MATCH query instead.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_Create drives a CREATE (n:Person {name: 'Alice'}) through the
// neo4j-go-driver and verifies:
//
//  1. ExecuteWrite succeeds without error.
//  2. A subsequent MATCH returns exactly one row containing the created node.
//  3. The returned node map carries the expected label and property.
func TestE2E_Create(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// CREATE the node via a managed write transaction.
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, `CREATE (n:Person {name: $name}) RETURN n`, map[string]any{
			"name": "Alice",
		})
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
	if err != nil {
		t.Fatalf("ExecuteWrite CREATE: %v", err)
	}

	// MATCH to verify the node was persisted.
	rows := runRead(ctx, t, session, `MATCH (n:Person {name: $name}) RETURN n`, map[string]any{
		"name": "Alice",
	})

	if len(rows) != 1 {
		t.Fatalf("MATCH returned %d rows, want 1", len(rows))
	}

	nodeMap := asNodeMap(t, rows[0]["n"])

	labels := nodeLabels(t, nodeMap)
	ls := labelSet(labels)
	if _, ok := ls["Person"]; !ok {
		t.Errorf("node labels %v do not contain 'Person'", labels)
	}

	props := nodeProps(t, nodeMap)
	if got, want := props["name"], "Alice"; got != want {
		t.Errorf("node property name: got %v, want %v", got, want)
	}
}
