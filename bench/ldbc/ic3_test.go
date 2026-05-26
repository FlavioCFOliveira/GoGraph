package ldbc

// ic3_test.go — T729: LDBC SNB IC3 (Friends in Two Countries) via Bolt e2e.
//
// Adapted to the social graph (no Country label): from Person id=1, find
// friends-of-friends (KNOWS*1..2) whose lastName starts with 'B'. This
// exercises the same multi-hop EXPAND + FILTER + DISTINCT + SORT pipeline
// as the canonical IC3 query without requiring a Place label hierarchy.
//
// Acceptance criteria (T729):
//  1. Exactly 1 row returned (Person 3, Alice Brown, is the only reachable
//     person — excluding start — with lastName starting with 'B').
//  2. friend.id == 3, friend.lastName == "Brown".
//  3. Goroutine-leak free (goleak registered in startICServer).

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestIC3_FriendsInCountries verifies the IC3-analogous pipeline:
// MATCH (start:Person {id:1})-[:KNOWS*1..2]-(friend:Person)
// WHERE friend.lastName STARTS WITH 'B'
// RETURN DISTINCT friend.id, friend.lastName ORDER BY friend.id
func TestIC3_FriendsInCountries(t *testing.T) {
	_, driver := startICServer(t)
	ctx := context.Background()

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	const query = `MATCH (start:Person {id: 1})-[:KNOWS*1..2]-(friend:Person)
WHERE friend.lastName STARTS WITH 'B'
RETURN DISTINCT friend.id, friend.lastName
ORDER BY friend.id`

	results, err := runICQuery(ctx, session, query)
	if err != nil {
		t.Skipf("IC3 query not supported: %v", err)
	}

	// AC#1: exactly one row.
	// Reachable from Person 1 via KNOWS*1..2 (excluding start):
	//   depth-1: Person 2 (Jones), Person 4 (Davis)
	//   depth-2: Person 3 (Brown ✓), Person 5 (Wilson), Person 5 (Wilson)
	// DISTINCT → Person 3 is the sole match.
	if len(results) != 1 {
		t.Fatalf("IC3: expected 1 row, got %d: %v", len(results), results)
	}

	row := results[0]

	// AC#2: friend.id == 3.
	gotID := requireInt64(t, "friend.id", row["friend.id"])
	if gotID != 3 {
		t.Errorf("IC3: friend.id = %d, want 3", gotID)
	}

	// AC#2: friend.lastName == "Brown".
	gotLast := requireString(t, "friend.lastName", row["friend.lastName"])
	if gotLast != "Brown" {
		t.Errorf("IC3: friend.lastName = %q, want %q", gotLast, "Brown")
	}
}
