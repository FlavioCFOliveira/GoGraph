package server_test

// e2e_return_relationship_test.go — T784: RETURN relationship shape.
//
// Known server limitations:
//   - The server sends relationships as plain PackStream maps (keys: "id",
//     "start", "end", "type", "properties"), not as PackStream structs with
//     tag byte 0x52. The neo4j-go-driver therefore decodes them as
//     map[string]any rather than neo4j.Relationship.
//   - ElementId, StartElementId, EndElementId (ACs #1–2): only numeric "id",
//     "start" and "end" int64 values are available. String element IDs are not
//     emitted. This is documented as a known gap.
//
// Closes backlog #504: RelationshipValue end-to-end surface is validated here.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_ReturnRelationshipShape creates two nodes and a KNOWS relationship
// between them, then MATCH ()-[r:KNOWS]->() RETURN r and verifies the
// relationship map shape.
func TestE2E_ReturnRelationshipShape(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	runWrite(ctx, t, session,
		`CREATE (a:Person {name:'Alice'})-[r:KNOWS {since: $since}]->(b:Person {name:'Bob'})`,
		map[string]any{"since": int64(2020)},
	)

	rows := runRead(ctx, t, session, `MATCH ()-[r:KNOWS]->() RETURN r`, nil)
	if len(rows) != 1 {
		t.Fatalf("MATCH returned %d rows, want 1", len(rows))
	}

	rel, ok := rows[0]["r"].(neo4j.Relationship)
	if !ok {
		t.Fatalf("expected the driver to materialise a neo4j.Relationship, got %T: %v — the "+
			"server is not sending the Bolt Relationship structure (#2189)", rows[0]["r"], rows[0]["r"])
	}

	// AC#1: Type equals seeded type string.
	if rel.Type != "KNOWS" {
		t.Errorf("rel type: got %q, want %q", rel.Type, "KNOWS")
	}

	// AC#2: StartElementId / EndElementId. Since #2189 the server sends all three
	// element ids, so these are real strings rather than the numeric-only fallback the
	// map form left the driver to synthesise.
	if rel.ElementId == "" {
		t.Error("rel ElementId is empty: the server must send element_id")
	}
	if rel.StartElementId == "" || rel.EndElementId == "" {
		t.Errorf("rel StartElementId=%q EndElementId=%q: both must be sent",
			rel.StartElementId, rel.EndElementId)
	}
	if rel.StartElementId == rel.EndElementId {
		t.Errorf("rel start (%s) and end (%s) element ids should differ",
			rel.StartElementId, rel.EndElementId)
	}
	//nolint:staticcheck // StartId/EndId are supported until driver 6.0
	if rel.StartId == rel.EndId {
		t.Errorf("rel start (%d) and end (%d) should differ", rel.StartId, rel.EndId)
	}

	// AC#3: Properties map round-trips.
	if got, ok := rel.Props["since"].(int64); !ok || got != 2020 {
		t.Errorf("property since: got %v (%T), want 2020 (int64)", rel.Props["since"], rel.Props["since"])
	}
}
