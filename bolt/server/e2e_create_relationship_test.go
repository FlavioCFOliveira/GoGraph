package server_test

// e2e_create_relationship_test.go — T796: CREATE relationship with properties.
//
// Known server limitations:
//   - Summary counters always return 0 (no "stats" key in PULL SUCCESS).
//     Relationship creation and property counts are verified via MATCH instead.
//   - Relationships are decoded as map[string]any by the driver.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_CreateRelationshipWithProperties creates a relationship with float64,
// string and bool properties, then verifies them via MATCH.
func TestE2E_CreateRelationshipWithProperties(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// CREATE two nodes and a relationship with three properties.
	runWrite(ctx, t, session,
		`CREATE (a:A)-[r:R {weight: $weight, label: $label, flag: $flag}]->(b:B)`,
		map[string]any{
			"weight": float64(1.5),
			"label":  "x",
			"flag":   true,
		},
	)

	// AC#1 (counter gap): server does not emit "stats"; verify via MATCH count.
	countRows := runRead(ctx, t, session, `MATCH ()-[r:R]->() RETURN count(r) AS cnt`, nil)
	if len(countRows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(countRows))
	}
	cnt, ok := countRows[0]["cnt"].(int64)
	if !ok || cnt != 1 {
		t.Errorf("relationship count: got %v (%T), want 1", countRows[0]["cnt"], countRows[0]["cnt"])
	}

	// AC#2: Property values round-trip.
	rows := runRead(ctx, t, session, `MATCH ()-[r:R]->() RETURN r`, nil)
	if len(rows) != 1 {
		t.Fatalf("MATCH returned %d rows, want 1", len(rows))
	}

	relMap, ok := rows[0]["r"].(map[string]any)
	if !ok {
		t.Fatalf("rel value: expected map[string]any, got %T", rows[0]["r"])
	}

	props, ok := relMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("rel 'properties': expected map[string]any, got %T", relMap["properties"])
	}

	if got, ok := props["weight"].(float64); !ok || got != 1.5 {
		t.Errorf("property weight: got %v (%T), want 1.5 (float64)", props["weight"], props["weight"])
	}
	if got, want := props["label"], "x"; got != want {
		t.Errorf("property label: got %v, want %v", got, want)
	}
	if got, ok := props["flag"].(bool); !ok || !got {
		t.Errorf("property flag: got %v (%T), want true (bool)", props["flag"], props["flag"])
	}
}
