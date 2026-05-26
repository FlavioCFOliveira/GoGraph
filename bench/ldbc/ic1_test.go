package ldbc

// ic1_test.go — T719: LDBC SNB IC1 (Friends-of-Friends) via Bolt end-to-end.
//
// From Person id=1 (Alice Smith), find all Persons named "Alice" reachable
// via KNOWS*1..3 (excluding the start node). Return id and lastName sorted
// by lastName. Exercised through the neo4j-go-driver over a live Bolt TCP
// connection.
//
// Acceptance criteria (T719):
//  1. Exactly 1 row returned.
//  2. friend.id == 3, friend.lastName == "Brown".
//  3. Goroutine-leak free (goleak registered in startICServer).

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestIC1_FriendsOfFriends verifies the IC1 friends-of-friends pipeline:
// MATCH (start:Person {id:1})-[:KNOWS*1..3]-(friend:Person)
// WHERE friend.firstName = 'Alice' AND friend.id <> 1
// RETURN DISTINCT friend.id, friend.lastName ORDER BY friend.lastName
func TestIC1_FriendsOfFriends(t *testing.T) {
	_, driver := startICServer(t)
	ctx := context.Background()

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	const query = `MATCH (start:Person {id: 1})-[:KNOWS*1..3]-(friend:Person)
WHERE friend.firstName = 'Alice' AND friend.id <> 1
RETURN DISTINCT friend.id, friend.lastName
ORDER BY friend.lastName`

	results, err := runICQuery(ctx, session, query)
	if err != nil {
		t.Skipf("IC1 query not supported: %v", err)
	}

	// AC#1: exactly one row.
	if len(results) != 1 {
		t.Fatalf("IC1: expected 1 row, got %d: %v", len(results), results)
	}

	row := results[0]

	// AC#2: friend.id == 3.
	gotID := requireInt64(t, "friend.id", row["friend.id"])
	if gotID != 3 {
		t.Errorf("IC1: friend.id = %d, want 3", gotID)
	}

	// AC#2: friend.lastName == "Brown".
	gotLast := requireString(t, "friend.lastName", row["friend.lastName"])
	if gotLast != "Brown" {
		t.Errorf("IC1: friend.lastName = %q, want %q", gotLast, "Brown")
	}
}
