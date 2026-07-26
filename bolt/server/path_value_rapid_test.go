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

// TestPathValueRapid_RoundTrip verifies that exprValueToPackstream produces a correct
// Bolt PATH STRUCTURE for a PathValue over 200 rapid iterations.
//
// Since #2189 a path goes on the wire as the 'P' (0x50) structure the Bolt protocol
// specifies: [nodes, unbound_relationships, indices]. Nodes are 'N' structures and the
// relationships are UNBOUND ('r') — a path's relationships carry no endpoints, because
// the indices supply them. This encoder emits both lists in path order, so hop i uses
// node index i+1 and relationship index ±(i+1).
func TestPathValueRapid_RoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		pv := genPathValue().Draw(rt, "pv")

		p := decodePath(rt, exprValueToPackstream(pv, 5))
		if len(p.Nodes) != len(pv.Nodes) {
			rt.Fatalf("nodes length: want %d, got %d", len(pv.Nodes), len(p.Nodes))
		}
		if len(p.Rels) != len(pv.Relationships) {
			rt.Fatalf("relationships length: want %d, got %d", len(pv.Relationships), len(p.Rels))
		}

		// Each node is an 'N' structure and preserves source order.
		for i, wantNode := range pv.Nodes {
			gotNode := decodeNode(rt, p.Nodes[i])
			if uint64(gotNode.ID) != wantNode.ID {
				rt.Fatalf("nodes[%d].id: want %d, got %d", i, wantNode.ID, gotNode.ID)
			}
		}

		// Each relationship is an UNBOUND 'r' structure and preserves source order.
		for i, wantRel := range pv.Relationships {
			gotID, gotType, _, _ := decodeUnboundRel(rt, p.Rels[i])
			if uint64(gotID) != wantRel.ID {
				rt.Fatalf("rels[%d].id: want %d, got %d", i, wantRel.ID, gotID)
			}
			if gotType != wantRel.Type {
				rt.Fatalf("rels[%d].type: want %q, got %q", i, wantRel.Type, gotType)
			}
		}

		// One (relationship, node) pair per hop, and the relationship index's SIGN
		// must record whether the hop traverses the relationship forwards.
		hops := len(pv.Relationships)
		if hops > len(pv.Nodes)-1 {
			hops = len(pv.Nodes) - 1 // a malformed draw emits only the supportable pairs
		}
		if hops < 0 {
			hops = 0
		}
		if len(p.Indices) != 2*hops {
			rt.Fatalf("indices: want %d entries for %d hops, got %d", 2*hops, hops, len(p.Indices))
		}
		for i := 0; i < hops; i++ {
			wantRelIdx := int64(i + 1)
			if pv.Relationships[i].StartID != pv.Nodes[i].ID {
				wantRelIdx = -wantRelIdx
			}
			if p.Indices[2*i] != wantRelIdx {
				rt.Fatalf("hop %d relationship index: want %d, got %v", i, wantRelIdx, p.Indices[2*i])
			}
			if p.Indices[2*i+1] != int64(i+1) {
				rt.Fatalf("hop %d node index: want %d, got %v", i, i+1, p.Indices[2*i+1])
			}
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

			p := decodePath(t, exprValueToPackstream(pv, 5))
			if len(p.Nodes) != n {
				t.Errorf("P%d: nodes: want %d, got %d", n, n, len(p.Nodes))
			}
			if len(p.Rels) != n-1 {
				t.Errorf("P%d: rels: want %d, got %d", n, n-1, len(p.Rels))
			}
			// Every hop runs forwards along the chain, so every relationship index is
			// positive and one-based.
			if len(p.Indices) != 2*(n-1) {
				t.Fatalf("P%d: indices: want %d, got %d", n, 2*(n-1), len(p.Indices))
			}
			for i := 0; i < n-1; i++ {
				if p.Indices[2*i] != int64(i+1) {
					t.Errorf("P%d hop %d: relationship index = %v, want %d", n, i, p.Indices[2*i], i+1)
				}
				if p.Indices[2*i+1] != int64(i+1) {
					t.Errorf("P%d hop %d: node index = %v, want %d", n, i, p.Indices[2*i+1], i+1)
				}
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

			p := decodePath(t, exprValueToPackstream(pv, 5))
			if len(p.Nodes) != n+1 {
				t.Errorf("C%d: nodes: want %d, got %d", n, n+1, len(p.Nodes))
			}
			if len(p.Rels) != n {
				t.Errorf("C%d: rels: want %d, got %d", n, n, len(p.Rels))
			}
			// A cycle closes by repeating the start node at the end, so the node list is
			// deliberately NOT deduplicated and the last hop's node index points at that
			// repeated entry. Every hop still runs forwards.
			if len(p.Indices) != 2*n {
				t.Fatalf("C%d: indices: want %d, got %d", n, 2*n, len(p.Indices))
			}
			for i := 0; i < n; i++ {
				if p.Indices[2*i] != int64(i+1) {
					t.Errorf("C%d hop %d: relationship index = %v, want %d", n, i, p.Indices[2*i], i+1)
				}
				if p.Indices[2*i+1] != int64(i+1) {
					t.Errorf("C%d hop %d: node index = %v, want %d", n, i, p.Indices[2*i+1], i+1)
				}
			}
		})
	}
}
