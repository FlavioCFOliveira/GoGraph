package server_test

// e2e_return_path_test.go — T791: RETURN path shape.
//
// Known server limitations:
//   - The server sends paths as plain PackStream maps (keys: "nodes",
//     "relationships"), not as PackStream structs with tag byte 0x50. The
//     neo4j-go-driver therefore decodes them as map[string]any rather than
//     neo4j.Path.
//   - Node and relationship ElementIds are not yet available as strings; only
//     numeric "id" values exist in the sub-maps.
//
// This test uses a simple linear chain A-[R1]->B-[R2]->C and matches a path
// of length 2.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_ReturnPathShape seeds a 3-node chain, matches a path of length 2,
// and verifies the path map structure.
func TestE2E_ReturnPathShape(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)

	// Seed: (a:Start)-[:HOP]->(b:Mid)-[:HOP]->(c:End)
	runWrite(ctx, t, session,
		`CREATE (a:Start {n:0})-[:HOP]->(b:Mid {n:1})-[:HOP]->(c:End {n:2})`,
		nil,
	)

	// MATCH a path of exactly 2 hops.
	rows := runRead(ctx, t, session,
		`MATCH p=(a:Start)-[:HOP*2..2]->(c:End) RETURN p`,
		nil,
	)
	if len(rows) != 1 {
		t.Fatalf("MATCH returned %d paths, want 1", len(rows))
	}

	// Since #2189 the server sends the Bolt 'P' structure, so the driver materialises a
	// real neo4j.Path — nodes as neo4j.Node and relationships REBUILT from the path's
	// index pairs, which means this also checks that the indices this server emits are
	// the ones the driver's buildPath expects. A wrong index would surface as a wrong
	// relationship count or as endpoints that do not chain.
	pathVal := rows[0]["p"]
	path, ok := pathVal.(neo4j.Path)
	if !ok {
		t.Fatalf("path value: expected the driver to materialise a neo4j.Path, got %T: %v — "+
			"the server is not sending the Bolt Path structure (#2189)", pathVal, pathVal)
	}

	// AC#1: Path length matches seeded walk: 3 nodes, 2 relationships.
	if len(path.Nodes) != 3 {
		t.Errorf("path nodes: got %d, want 3", len(path.Nodes))
	}
	if len(path.Relationships) != 2 {
		t.Errorf("path relationships: got %d, want 2", len(path.Relationships))
	}

	// AC#2: Node ids and element ids are present and distinct.
	nodeIDs := make(map[int64]struct{}, len(path.Nodes))
	for i, n := range path.Nodes {
		id := nodeID(t, n)
		if _, dup := nodeIDs[id]; dup {
			t.Errorf("node[%d]: duplicate id %d in path nodes", i, id)
		}
		nodeIDs[id] = struct{}{}
		if n.ElementId == "" {
			t.Errorf("node[%d]: empty ElementId", i)
		}
	}

	// AC#3: Relationship ids are present and distinct, and the rebuilt relationships
	// CHAIN — each hop's start is the previous hop's end. That is the property the index
	// pairs exist to carry, so it fails if the relationship index sign or the node index
	// is wrong.
	relIDs := make(map[int64]struct{}, len(path.Relationships))
	for i, r := range path.Relationships {
		rid := r.Id //nolint:staticcheck // Id is supported until driver 6.0
		if _, dup := relIDs[rid]; dup {
			t.Errorf("path rel[%d]: duplicate id %d", i, rid)
		}
		relIDs[rid] = struct{}{}
		if r.ElementId == "" {
			t.Errorf("path rel[%d]: empty ElementId", i)
		}
		// Hop i must join node i to node i+1, in one direction or the other.
		from := nodeID(t, path.Nodes[i])
		to := nodeID(t, path.Nodes[i+1])
		//nolint:staticcheck // StartId/EndId are supported until driver 6.0
		if !(r.StartId == from && r.EndId == to) && !(r.StartId == to && r.EndId == from) {
			t.Errorf("path rel[%d] joins (%d, %d) but hop %d is between nodes %d and %d: "+
				"the path index pair is wrong", i, r.StartId, r.EndId, i, from, to)
		}
	}
}
