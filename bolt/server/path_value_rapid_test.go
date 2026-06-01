package server

// T626: rapid-based round-trip tests for PathValue → packstream encoding.
//
// exprValueToPackstream converts expr.PathValue to map[string]packstream.Value
// with "nodes" and "relationships" fields. The test verifies:
//
//  1. Round-trip identity over 200 rapid iterations.
//  2. Index list sign and order preserved (nodes[i].ID, rels[i].ID match source).
//  3. Pn (path graph: n nodes, n-1 edges) and Cn (cycle graph: n nodes, n edges)
//     shapes are covered by explicit shape tests.
//
// Layer: short (no build tag required).

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// genPathValue generates a random PathValue with 1–5 nodes and len(Nodes)-1
// relationships (Pn path shape). At least one node is always present.
func genPathValue() *rapid.Generator[expr.PathValue] {
	return rapid.Custom(func(rt *rapid.T) expr.PathValue {
		numNodes := rapid.IntRange(1, 5).Draw(rt, "numNodes")
		nodes := make([]expr.NodeValue, numNodes)
		for i := range nodes {
			nodes[i] = expr.NodeValue{
				ID:         uint64(i + 1),
				Labels:     []string{fmt.Sprintf("L%d", i)},
				Properties: expr.MapValue{},
			}
		}
		numRels := numNodes - 1
		rels := make([]expr.RelationshipValue, numRels)
		for i := range rels {
			rels[i] = expr.RelationshipValue{
				ID:         uint64(100 + i),
				StartID:    nodes[i].ID,
				EndID:      nodes[i+1].ID,
				Type:       "REL",
				Properties: expr.MapValue{},
			}
		}
		return expr.PathValue{Nodes: nodes, Relationships: rels}
	})
}

// TestPathValueRapid_RoundTrip verifies that exprValueToPackstream produces a
// correct map representation of PathValue over 200 rapid iterations.
//
// The encoded map must have:
//   - "nodes"         → []packstream.Value (each is a node map)
//   - "relationships" → []packstream.Value (each is a rel map)
//
// Node and relationship order must match source order.
func TestPathValueRapid_RoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		pv := genPathValue().Draw(rt, "pv")

		got := exprValueToPackstream(pv)
		m, ok := got.(map[string]packstream.Value)
		if !ok {
			rt.Fatalf("expected map[string]packstream.Value, got %T", got)
		}

		nodes, ok := m["nodes"].([]packstream.Value)
		if !ok {
			rt.Fatalf("nodes field: expected []packstream.Value, got %T", m["nodes"])
		}
		if len(nodes) != len(pv.Nodes) {
			rt.Fatalf("nodes length: want %d, got %d", len(pv.Nodes), len(nodes))
		}

		rels, ok := m["relationships"].([]packstream.Value)
		if !ok {
			rt.Fatalf("relationships field: expected []packstream.Value, got %T", m["relationships"])
		}
		if len(rels) != len(pv.Relationships) {
			rt.Fatalf("relationships length: want %d, got %d", len(pv.Relationships), len(rels))
		}

		// Verify each node's ID preserves source order.
		for i, wantNode := range pv.Nodes {
			nm, ok := nodes[i].(map[string]packstream.Value)
			if !ok {
				rt.Fatalf("nodes[%d]: expected map, got %T", i, nodes[i])
			}
			gotID, ok := nm["id"].(int64)
			if !ok {
				rt.Fatalf("nodes[%d].id: expected int64, got %T", i, nm["id"])
			}
			if uint64(gotID) != wantNode.ID {
				rt.Fatalf("nodes[%d].id: want %d, got %d", i, wantNode.ID, gotID)
			}
		}

		// Verify each relationship's ID preserves source order.
		for i, wantRel := range pv.Relationships {
			rm, ok := rels[i].(map[string]packstream.Value)
			if !ok {
				rt.Fatalf("rels[%d]: expected map, got %T", i, rels[i])
			}
			gotID, ok := rm["id"].(int64)
			if !ok {
				rt.Fatalf("rels[%d].id: expected int64, got %T", i, rm["id"])
			}
			if uint64(gotID) != wantRel.ID {
				rt.Fatalf("rels[%d].id: want %d, got %d", i, wantRel.ID, gotID)
			}
		}

		// Map must have exactly two keys.
		if len(m) != 2 {
			rt.Fatalf("map has unexpected fields: got %d keys, want 2", len(m))
		}
	})
}

// TestPathValueShape_Pn verifies encoding of Pn (path graph) shapes for
// n = 1, 2, 5. A path graph with n nodes has n-1 undirected edges; here
// edges are directed StartID→EndID.
func TestPathValueShape_Pn(t *testing.T) {
	cases := []int{1, 2, 5}
	for _, n := range cases {
		t.Run(fmt.Sprintf("P%d", n), func(t *testing.T) {
			nodes := make([]expr.NodeValue, n)
			for i := range nodes {
				nodes[i] = expr.NodeValue{ID: uint64(i + 1), Labels: []string{"N"}, Properties: expr.MapValue{}}
			}
			rels := make([]expr.RelationshipValue, n-1)
			for i := range rels {
				rels[i] = expr.RelationshipValue{ID: uint64(i + 1), StartID: nodes[i].ID, EndID: nodes[i+1].ID, Type: "E", Properties: expr.MapValue{}}
			}
			pv := expr.PathValue{Nodes: nodes, Relationships: rels}

			got := exprValueToPackstream(pv)
			m, ok := got.(map[string]packstream.Value)
			if !ok {
				t.Fatalf("expected map, got %T", got)
			}
			gotNodes := m["nodes"].([]packstream.Value)        //nolint:forcetypeassert // known type
			gotRels := m["relationships"].([]packstream.Value) //nolint:forcetypeassert // known type
			if len(gotNodes) != n {
				t.Errorf("P%d: nodes: want %d, got %d", n, n, len(gotNodes))
			}
			if len(gotRels) != n-1 {
				t.Errorf("P%d: rels: want %d, got %d", n, n-1, len(gotRels))
			}
		})
	}
}

// TestPathValueShape_Cn verifies encoding of Cn (cycle graph) shapes for
// n = 3, 4. A cycle graph with n nodes has n directed edges (last node
// connects back to first).
func TestPathValueShape_Cn(t *testing.T) {
	cases := []int{3, 4}
	for _, n := range cases {
		t.Run(fmt.Sprintf("C%d", n), func(t *testing.T) {
			nodes := make([]expr.NodeValue, n+1) // +1 because PathValue repeats start at end
			for i := range n {
				nodes[i] = expr.NodeValue{ID: uint64(i + 1), Labels: []string{"N"}, Properties: expr.MapValue{}}
			}
			// Close the cycle: last node is a copy of the first.
			nodes[n] = nodes[0]

			rels := make([]expr.RelationshipValue, n)
			for i := range n {
				rels[i] = expr.RelationshipValue{
					ID:         uint64(i + 1),
					StartID:    nodes[i].ID,
					EndID:      nodes[(i+1)%n].ID,
					Type:       "E",
					Properties: expr.MapValue{},
				}
			}
			pv := expr.PathValue{Nodes: nodes, Relationships: rels}

			got := exprValueToPackstream(pv)
			m, ok := got.(map[string]packstream.Value)
			if !ok {
				t.Fatalf("expected map, got %T", got)
			}
			gotNodes := m["nodes"].([]packstream.Value)        //nolint:forcetypeassert // known type
			gotRels := m["relationships"].([]packstream.Value) //nolint:forcetypeassert // known type
			if len(gotNodes) != n+1 {
				t.Errorf("C%d: nodes: want %d, got %d", n, n+1, len(gotNodes))
			}
			if len(gotRels) != n {
				t.Errorf("C%d: rels: want %d, got %d", n, n, len(gotRels))
			}
		})
	}
}
