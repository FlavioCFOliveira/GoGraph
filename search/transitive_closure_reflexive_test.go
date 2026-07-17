package search

// transitive_closure_reflexive_test.go — regression test for the production-
// readiness audit finding [G1] (rmp #2040).
//
// TransitiveClosure.Reachable(v, v) returned false for a present but isolated
// (degree-0) node, contradicting FloydWarshall.At / JohnsonAPSP.At /
// Dijkstra.Distance / BellmanFord.Distance, which all report (0, true) for the
// self-query, and violating reflexive reachability. The compaction on the live
// set (#1474) dropped the reflexive self-bit for non-live slots without the
// i==j early-return that Floyd-Warshall's At carries.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestTransitiveClosure_ReflexiveIsolatedNode(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddNode(2); err != nil { // present but isolated (degree 0)
		t.Fatalf("AddNode: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	iso, _ := a.Mapper().Lookup(2)
	n0, _ := a.Mapper().Lookup(0)

	tc := TransitiveClosure(c)

	// The fix: an isolated present node reaches itself.
	if !tc.Reachable(iso, iso) {
		t.Fatalf("TC.Reachable(isolated, self) = false, want true (reflexive)")
	}
	// Cross-oracle agreement on the self-query (all report (0, true)).
	if d, ok := FloydWarshall(c).At(iso, iso); !ok || d != 0 {
		t.Fatalf("FloydWarshall.At(iso,iso) = (%v,%v), want (0,true)", d, ok)
	}
	if dj, _ := Dijkstra(c, iso); dj != nil {
		if d, ok := dj.Distance(iso); !ok || d != 0 {
			t.Fatalf("Dijkstra.Distance(iso) = (%v,%v), want (0,true)", d, ok)
		}
	}
	if bf, _ := BellmanFord(c, iso); bf != nil {
		if d, ok := bf.Distance(iso); !ok || d != 0 {
			t.Fatalf("BellmanFord.Distance(iso) = (%v,%v), want (0,true)", d, ok)
		}
	}

	// No spurious cross-reachability: the isolated node reaches no other node,
	// and no other node reaches it.
	if tc.Reachable(iso, n0) {
		t.Errorf("isolated node must not reach a distinct node")
	}
	if tc.Reachable(n0, iso) {
		t.Errorf("a distinct node must not reach the isolated node")
	}
	// A live node still reaches itself, and real edges still hold.
	if !tc.Reachable(n0, n0) {
		t.Errorf("live node must reach itself")
	}
	n1, _ := a.Mapper().Lookup(1)
	if !tc.Reachable(n0, n1) {
		t.Errorf("0->1 edge must be reachable")
	}

	// Out-of-range NodeID is still false (defensive boundary).
	if tc.Reachable(iso, iso+100) || tc.Reachable(iso+100, iso) {
		t.Errorf("out-of-range NodeID must be unreachable")
	}
}
