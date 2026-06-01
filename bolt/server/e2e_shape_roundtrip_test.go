package server_test

// e2e_shape_roundtrip_test.go — T880: Shape categories round-trip CREATE/MATCH.
//
// For a curated set of shapes from Family 1 (degenerate), Family 2 (classic),
// and Family 6 (random), this test:
//  1. Builds the shape locally using internal/shapegen.
//  2. Seeds the server via a single Cypher CREATE statement that creates all
//     nodes and edges inline (using Cypher variable references within CREATE).
//  3. Reconstructs the graph client-side via MATCH and verifies:
//     - Node count equals Build() Order().
//     - Edge count equals Build() Size().
//     - Sorted out-degree sequence matches.
//  4. Runs at the short layer for n≤200.
//
// Known engine limitations:
//   - The Cypher engine does not support CREATE of a relationship between two
//     nodes obtained via separate MATCH clauses when those nodes are carried
//     through WITH (CreateRelationship requires integer node IDs, not NodeValue
//     variables from MATCH). All nodes and edges must therefore be created in a
//     single CREATE statement using inline Cypher variable references.
//   - All edges are stored as directed arcs. Undirected shapes store each
//     logical edge as two arcs (u→v and v→u); MATCH ()-[:E]->() counts each
//     directed arc, so edge count == Size() for both directed and undirected.
//   - Each test case uses a unique label prefix to avoid cross-case interference.
//   - Summary counters always return 0 (server does not emit "stats").

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// shapeCase describes one round-trip test case.
type shapeCase struct {
	name       string
	shape      shapegen.Shape[int, int64]
	undirected bool
}

// shortShapeCases returns the curated set for the short (n≤200) layer.
func shortShapeCases() []shapeCase {
	return []shapeCase{
		// Family 1 — Degenerate / minimal
		{name: "Path_n5", shape: shapegen.Path(5, true), undirected: false},
		{name: "Path_n10_ud", shape: shapegen.Path(10, false), undirected: true},

		// Family 2 — Classic
		{name: "Cycle_n8", shape: shapegen.Cycle(8, true), undirected: false},
		{name: "Star_n10", shape: shapegen.Star(10, true), undirected: false},
		{name: "Complete_n6", shape: shapegen.Complete(6, true), undirected: false},
		{name: "Bipartite_4x4", shape: shapegen.CompleteBipartite(4, 4), undirected: true},

		// Family 6 — Random (small, reproducible seeds)
		{name: "ER_n50_p30", shape: shapegen.ErdosRenyiNP(50, 30, 42), undirected: true},
		{name: "BA_n40_m3", shape: shapegen.BarabasiAlbert(40, 3, 7), undirected: false},
		{name: "WS_n20_k4_b30", shape: shapegen.WattsStrogatz(20, 4, 30, 99), undirected: false},
	}
}

// TestE2E_ShapeRoundtrip verifies node count, edge count, and out-degree
// sequence round-trip through the server for each curated shape case.
func TestE2E_ShapeRoundtrip(t *testing.T) {
	ctx := context.Background()

	for _, tc := range shortShapeCases() {
		tc := tc // capture
		t.Run(tc.name, func(t *testing.T) {
			driver, _ := newDriverForTest(t)
			testShapeRoundtrip(ctx, t, driver, tc)
		})
	}
}

// testShapeRoundtrip implements the round-trip logic for a single shape case.
func testShapeRoundtrip(
	ctx context.Context,
	t *testing.T,
	driver neo4j.DriverWithContext,
	tc shapeCase,
) {
	t.Helper()

	cfg := adjlist.Config{Directed: !tc.undirected}
	g, err := tc.shape.Build(cfg)
	if err != nil {
		t.Fatalf("shape.Build: %v", err)
	}

	adj := g.AdjList()
	wantNodes := adj.Order()
	// For undirected shapes the adjlist stores two directed arcs per logical
	// edge; Neighbours yields all arcs. We count arcs (not logical edges) for
	// the seed, because we create arcs in the Cypher CREATE statement.
	// For directed shapes: arcCount == Size().
	// For undirected shapes: arcCount == Size() * 2 (both directions).
	var arcCount uint64

	// Collect all node integers (internal IDs resolve via mapper).
	maxID := uint64(adj.MaxNodeID())
	nodes := make([]int, 0, wantNodes)
	for id := uint64(0); id <= maxID; id++ {
		v, ok := adj.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nodes = append(nodes, v)
	}

	// Build a stable index: node integer → 0-based position for variable naming.
	nodeIdx := make(map[int]int, len(nodes))
	for i, v := range nodes {
		nodeIdx[v] = i
	}

	// Compute expected out-degree sequence and arc count.
	wantDegSeq := make([]int, len(nodes))
	for i, u := range nodes {
		deg := 0
		for range adj.Neighbours(u) {
			deg++
		}
		wantDegSeq[i] = deg
		arcCount += uint64(deg)
	}
	sort.Ints(wantDegSeq)

	label := tc.name // already safe (only alphanumeric + _)
	relLabel := "E_" + tc.name

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// ── Seed: single CREATE statement with all nodes and edges ──────────────
	// Build: (v0:L {nid:'0'}),(v1:L {nid:'1'}),…,(v0)-[:E]->(v1),…
	// This is required because the Cypher engine cannot create relationships
	// between nodes obtained via MATCH/WITH (engine limitation).
	var b strings.Builder
	b.WriteString("CREATE ")

	// Node clauses.
	for i, v := range nodes {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(v%d:%s {nid:'%d'})", i, label, v)
	}

	// Edge clauses: emit all arcs as yielded by Neighbours (both directions for
	// undirected shapes, matching arcCount computed above).
	edgeCount := 0
	for _, u := range nodes {
		for v := range adj.Neighbours(u) {
			srcIdx := nodeIdx[u]
			dstIdx := nodeIdx[v]
			fmt.Fprintf(&b, ",(v%d)-[:%s]->(v%d)", srcIdx, relLabel, dstIdx)
			edgeCount++
		}
	}

	createQ := b.String()
	_, err = session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, e := tx.Run(ctx, createQ, nil)
		if e != nil {
			return nil, e
		}
		_, e = r.Consume(ctx)
		return nil, e
	})
	if err != nil {
		t.Fatalf("seed graph: %v (query len=%d)", err, len(createQ))
	}

	// ── Verify node count ────────────────────────────────────────────────────
	nodeCountQ := fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS cnt", label)
	nodeRows := runRead(ctx, t, session, nodeCountQ, nil)
	if len(nodeRows) != 1 {
		t.Fatalf("node count query returned %d rows", len(nodeRows))
	}
	gotNodes, ok := nodeRows[0]["cnt"].(int64)
	if !ok {
		t.Fatalf("node count: expected int64, got %T", nodeRows[0]["cnt"])
	}
	if uint64(gotNodes) != wantNodes {
		t.Errorf("node count: got %d, want %d", gotNodes, wantNodes)
	}

	// ── Verify edge count ────────────────────────────────────────────────────
	edgeCountQ := fmt.Sprintf("MATCH (a:%s)-[:%s]->(b:%s) RETURN count(*) AS cnt", label, relLabel, label)
	edgeRows := runRead(ctx, t, session, edgeCountQ, nil)
	if len(edgeRows) != 1 {
		t.Fatalf("edge count query returned %d rows", len(edgeRows))
	}
	gotEdges, ok := edgeRows[0]["cnt"].(int64)
	if !ok {
		t.Fatalf("edge count: expected int64, got %T", edgeRows[0]["cnt"])
	}
	if uint64(gotEdges) != arcCount {
		t.Errorf("edge count: got %d, want %d (arcCount)", gotEdges, arcCount)
	}

	// ── Verify out-degree sequence ───────────────────────────────────────────
	degQ := fmt.Sprintf(
		"MATCH (a:%s) OPTIONAL MATCH (a)-[:%s]->(b:%s) WITH a, count(b) AS deg RETURN deg ORDER BY deg",
		label, relLabel, label,
	)
	degRows := runRead(ctx, t, session, degQ, nil)
	gotDegSeq := make([]int, 0, len(degRows))
	for _, row := range degRows {
		d, _ := row["deg"].(int64)
		gotDegSeq = append(gotDegSeq, int(d))
	}
	sort.Ints(gotDegSeq)

	if !intSliceEqual(gotDegSeq, wantDegSeq) {
		t.Errorf("out-degree sequence mismatch:\n  got  %v\n  want %v", gotDegSeq, wantDegSeq)
	}
}

// intSliceEqual reports whether a and b have identical contents.
func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
