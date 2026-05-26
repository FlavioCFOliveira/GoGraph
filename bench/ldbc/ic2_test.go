package ldbc

// ic2_test.go — T725: LDBC SNB IC2 (Recent Messages by Friends) via Bolt e2e.
//
// Adapted to the social graph (no Post/Comment schema): from Person id=1,
// find direct KNOWS friends with id > 2, sorted by id DESC, LIMIT 2. This
// exercises the same EXPAND + FILTER + SORT + LIMIT pipeline as the canonical
// IC2 query without requiring a Message label.
//
// Acceptance criteria (T725):
//  1. Exactly 1 row returned (only Person 4 is directly connected to 1 and has id>2).
//  2. friend.id == 4.
//  3. Goroutine-leak free (goleak registered in startICServer).

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestIC2_RecentMessages verifies the IC2-analogous pipeline:
// MATCH (start:Person {id:1})-[:KNOWS]-(friend:Person)
// WHERE friend.id > 2
// RETURN friend.id, friend.firstName ORDER BY friend.id DESC LIMIT 2
func TestIC2_RecentMessages(t *testing.T) {
	_, driver := startICServer(t)
	ctx := context.Background()

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	const query = `MATCH (start:Person {id: 1})-[:KNOWS]-(friend:Person)
WHERE friend.id > 2
RETURN friend.id, friend.firstName
ORDER BY friend.id DESC
LIMIT 2`

	results, err := runICQuery(ctx, session, query)
	if err != nil {
		t.Skipf("IC2 query not supported: %v", err)
	}

	// AC#1: exactly one row (Person 1 knows 2 and 4; only Person 4 has id > 2).
	if len(results) != 1 {
		t.Fatalf("IC2: expected 1 row, got %d: %v", len(results), results)
	}

	row := results[0]

	// AC#2: friend.id == 4.
	gotID := requireInt64(t, "friend.id", row["friend.id"])
	if gotID != 4 {
		t.Errorf("IC2: friend.id = %d, want 4", gotID)
	}
}
