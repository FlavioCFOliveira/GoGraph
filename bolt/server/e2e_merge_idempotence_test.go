package server_test

// e2e_merge_idempotence_test.go — T802: MERGE idempotence over the Bolt wire.
//
// The cypher engine's MERGE operator matches an existing node via
// NewMergeSearchFnFromPattern (cypher/api.go), so a second identical MERGE is a
// no-op rather than a duplicate-create. This test verifies that idempotence
// end-to-end over Bolt: AC#1 first MERGE creates one node, AC#2 second MERGE
// creates zero, AC#3 final count is exactly 1. (The skip that previously hid
// AC#2/AC#3 was stale — the MERGE search function is implemented; #1761.)

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_MergeIdempotence verifies MERGE idempotence over Bolt:
// AC#1 first MERGE creates the node, AC#2 second MERGE creates zero nodes,
// AC#3 final count is exactly 1.
func TestE2E_MergeIdempotence(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	// First MERGE — must create one node (AC#1).
	s1 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer s1.Close(ctx) //nolint:errcheck

	runWrite(ctx, t, s1, `MERGE (n:Person {email: $email})`, map[string]any{
		"email": "a@b",
	})

	// Verify the node was created.
	rows1 := runRead(ctx, t, s1, `MATCH (n:Person {email: $email}) RETURN count(n) AS cnt`, map[string]any{
		"email": "a@b",
	})
	if len(rows1) != 1 {
		t.Fatalf("count query after first MERGE returned %d rows, want 1", len(rows1))
	}
	cnt1, _ := rows1[0]["cnt"].(int64)
	if cnt1 < 1 {
		t.Errorf("after first MERGE: count = %d, want >= 1", cnt1)
	}

	// Second MERGE — must match the existing node (idempotence). The engine's
	// MERGE search function is implemented (NewMergeSearchFnFromPattern), so a
	// second identical MERGE is a no-op: AC#2 (zero nodes created) and AC#3
	// (final count = 1) are asserted over the live Bolt wire (#1761).
	s2 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer s2.Close(ctx) //nolint:errcheck

	runWrite(ctx, t, s2, `MERGE (n:Person {email: $email})`, map[string]any{
		"email": "a@b",
	})

	rows2 := runRead(ctx, t, s2, `MATCH (n:Person {email: $email}) RETURN count(n) AS cnt`, map[string]any{
		"email": "a@b",
	})
	if len(rows2) != 1 {
		t.Fatalf("count query after second MERGE returned %d rows, want 1", len(rows2))
	}
	cnt2, _ := rows2[0]["cnt"].(int64)
	if cnt2 != 1 {
		t.Errorf("after second MERGE: count = %d, want 1 (idempotence)", cnt2)
	}
}
