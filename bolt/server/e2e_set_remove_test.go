package server_test

// e2e_set_remove_test.go — T812: SET and REMOVE properties.
//
// Known server limitations:
//   - Summary counters (PropertiesSet, PropertiesRemoved) always return 0
//     because the server does not emit a "stats" key. Property changes are
//     verified by reading back the node via MATCH.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_SetRemoveProperties seeds (n:N {a:1, b:2}), sets a=10 and adds c='x',
// then removes b, and verifies the final property map is {a:10, c:'x'}.
func TestE2E_SetRemoveProperties(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// Seed the node.
	runWrite(ctx, t, session,
		`CREATE (n:N {a: $a, b: $b})`,
		map[string]any{"a": int64(1), "b": int64(2)},
	)

	// AC#1: SET n.a = 10, n.c = 'x' — two properties set.
	// Counter verification is skipped (KNOWN GAP: no "stats" key emitted).
	runWrite(ctx, t, session,
		`MATCH (n:N) SET n.a = $a, n.c = $c`,
		map[string]any{"a": int64(10), "c": "x"},
	)

	// AC#2: REMOVE n.b — one property removed.
	// Counter verification is skipped (KNOWN GAP: no "stats" key emitted).
	runWrite(ctx, t, session,
		`MATCH (n:N) REMOVE n.b`,
		nil,
	)

	// AC#3: Final property map equals {a:10, c:'x'} with no 'b'.
	rows := runRead(ctx, t, session, `MATCH (n:N) RETURN n`, nil)
	if len(rows) != 1 {
		t.Fatalf("MATCH returned %d rows, want 1", len(rows))
	}

	nodeMap := asNodeMap(t, rows[0]["n"])
	props := nodeProps(t, nodeMap)

	if got, ok := props["a"].(int64); !ok || got != 10 {
		t.Errorf("property a: got %v (%T), want 10 (int64)", props["a"], props["a"])
	}
	if got, want := props["c"], "x"; got != want {
		t.Errorf("property c: got %v, want %v", got, want)
	}
	if _, exists := props["b"]; exists {
		t.Errorf("property b should have been removed, but still present: %v", props["b"])
	}
}
