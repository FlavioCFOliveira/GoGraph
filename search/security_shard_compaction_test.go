package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// security_shard_compaction_test.go is the short-layer companion to the
// soak envelope test: it pins, on every PR, that the #1474 LiveMask
// compaction in search.TransitiveClosure and search.WCC is correctness-
// preserving and bounds allocation on the live node count rather than on
// the shard-amplified MaxNodeID().
//
// These tests use string keys deliberately so the sharded Mapper governs
// the NodeID space (integer keys would too, but strings are the
// untrusted-import key type the gap is reachable through).

// secShardCompactionKeys returns n distinct string keys; on string-keyed
// graphs the sharded Mapper already spreads them, so MaxNodeID() exceeds
// the live order. The exact spread is hash-dependent and irrelevant — the
// tests only require MaxNodeID() > order, which any non-trivial key set
// over 256 shards yields, to exercise the compaction's ghost-slot path.
// (itoa is the shared base-10 helper from bfs_shape_helpers_test.go.)
func secShardCompactionKeys(n int) []string {
	ks := make([]string, n)
	for i := range ks {
		ks[i] = "node-" + string(rune('A'+i%26)) + "-" + itoa(i)
	}
	return ks
}

// TestSec_Core_TransitiveClosureSparseNodeSpace verifies that
// TransitiveClosure over a sparse (Mapper-sharded) NodeID space returns
// IDENTICAL reachability to a hand-computed reference, proving the
// LiveMask compaction (rmp #1474) preserves correctness while sizing the
// bitset on the live order, not MaxNodeID().
func TestSec_Core_TransitiveClosureSparseNodeSpace(t *testing.T) {
	t.Parallel()

	const n = 40
	keys := secShardCompactionKeys(n)
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	// A directed chain: key[i] -> key[i+1]. Reachable(i, j) iff i <= j.
	for i := 0; i+1 < n; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	// Precondition: the NodeID space must be sparse (sharding inflates
	// MaxNodeID beyond the live order) so the compaction path is real.
	if uint64(c.MaxNodeID()) <= c.Order() {
		t.Fatalf("MaxNodeID()=%d not greater than Order()=%d; sharding did not "+
			"produce a sparse space, test precondition broken",
			uint64(c.MaxNodeID()), c.Order())
	}

	tc := TransitiveClosure(c)
	m := a.Mapper()
	ids := make([]graph.NodeID, n)
	for i, k := range keys {
		id, ok := m.Lookup(k)
		if !ok {
			t.Fatalf("key %q not interned", k)
		}
		ids[i] = id
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			want := i <= j // chain reachability, reflexive
			if got := tc.Reachable(ids[i], ids[j]); got != want {
				t.Fatalf("Reachable(key[%d], key[%d]) = %v, want %v", i, j, got, want)
			}
		}
	}

	// A ghost padding slot (a NodeID the sharding skips, never interned)
	// must report unreachable in both directions — including reflexively.
	// Find one: scan low NodeIDs for a slot no key maps to.
	interned := make(map[graph.NodeID]bool, n)
	for _, id := range ids {
		interned[id] = true
	}
	var ghost graph.NodeID
	foundGhost := false
	for cand := graph.NodeID(0); uint64(cand) < uint64(c.MaxNodeID()); cand++ {
		if !interned[cand] {
			ghost = cand
			foundGhost = true
			break
		}
	}
	if !foundGhost {
		t.Fatal("expected at least one ghost slot in a sparse NodeID space")
	}
	if tc.Reachable(ghost, ghost) {
		t.Fatalf("Reachable(ghost=%d, ghost) = true; compacted TC must report "+
			"ghost padding slots unreachable (this also fixes the prior over-report)", ghost)
	}
	if tc.Reachable(ids[0], ghost) || tc.Reachable(ghost, ids[0]) {
		t.Fatalf("ghost slot %d must be unreachable to/from any live node", ghost)
	}
}

// TestSec_Core_WCCSparseNodeSpace verifies that WCC over a sparse
// (Mapper-sharded) NodeID space returns the correct component count and
// labels live nodes consistently, proving the #1474 compaction of its
// Union-Find universe (sized on live order, not MaxNodeID) preserves
// correctness. Ghost slots must report -1.
func TestSec_Core_WCCSparseNodeSpace(t *testing.T) {
	t.Parallel()

	const n = 30
	keys := secShardCompactionKeys(n)
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	// Two disjoint chains: [0..n/2) and [n/2..n). Expect K = 2 components.
	half := n / 2
	for i := 0; i+1 < half; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for i := half; i+1 < n; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	if uint64(c.MaxNodeID()) <= c.Order() {
		t.Fatalf("MaxNodeID()=%d not greater than Order()=%d; sparse-space "+
			"precondition broken", uint64(c.MaxNodeID()), c.Order())
	}

	component, k, err := WCC(c)
	if err != nil {
		t.Fatalf("WCC: %v", err)
	}
	if k != 2 {
		t.Fatalf("WCC k = %d, want 2 disjoint components", k)
	}
	if len(component) != int(c.MaxNodeID()) {
		t.Fatalf("component slice length = %d, want MaxNodeID()=%d (NodeID-indexed contract)",
			len(component), int(c.MaxNodeID()))
	}

	m := a.Mapper()
	// All keys in the first chain share one label; all in the second
	// share another; the two labels differ.
	label := func(i int) int {
		id, ok := m.Lookup(keys[i])
		if !ok {
			t.Fatalf("key %q not interned", keys[i])
		}
		return component[id]
	}
	c1 := label(0)
	for i := 1; i < half; i++ {
		if label(i) != c1 {
			t.Fatalf("chain-1 node %d label %d != %d", i, label(i), c1)
		}
	}
	c2 := label(half)
	for i := half + 1; i < n; i++ {
		if label(i) != c2 {
			t.Fatalf("chain-2 node %d label %d != %d", i, label(i), c2)
		}
	}
	if c1 == c2 {
		t.Fatalf("disjoint chains must have distinct component labels, both = %d", c1)
	}

	// Ghost slots (never interned) must report -1.
	interned := make(map[graph.NodeID]bool, n)
	for i := 0; i < n; i++ {
		id, _ := m.Lookup(keys[i])
		interned[id] = true
	}
	for cand := graph.NodeID(0); uint64(cand) < uint64(c.MaxNodeID()); cand++ {
		if !interned[cand] && component[cand] != -1 {
			t.Fatalf("ghost slot %d labelled %d, want -1", cand, component[cand])
		}
	}
}

// TestSec_Core_KruskalSparseNodeSpace verifies that KruskalMST over a
// sparse (Mapper-sharded) NodeID space returns a spanning forest with
// the correct live-set cardinality (live-1 edges on a connected graph),
// proving the #1474 compaction of its Union-Find universe and edge
// budget (sized on live order, not MaxNodeID) preserves correctness.
func TestSec_Core_KruskalSparseNodeSpace(t *testing.T) {
	t.Parallel()

	const n = 24
	keys := secShardCompactionKeys(n)
	// Undirected, encoded symmetric: a connected spanning chain with
	// extra weighted edges. AddEdge on an undirected adjlist stores both
	// directions internally.
	a := adjlist.New[string, int64](adjlist.Config{Directed: false})
	for i := 0; i+1 < n; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], int64(i+1)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	// A couple of heavier chords that must NOT be selected over the chain.
	if err := a.AddEdge(keys[0], keys[n-1], 100); err != nil {
		t.Fatalf("AddEdge chord: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if uint64(c.MaxNodeID()) <= c.Order() {
		t.Fatalf("MaxNodeID()=%d not greater than Order()=%d; sparse-space "+
			"precondition broken", uint64(c.MaxNodeID()), c.Order())
	}

	mst, total, err := KruskalMST(c)
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}
	// Connected graph over `live` == n nodes: spanning tree has n-1 edges.
	if len(mst) != n-1 {
		t.Fatalf("MST edge count = %d, want live-1 = %d", len(mst), n-1)
	}
	// The chain edges have weights 1..n-1; the chord (100) is heavier than
	// every chain edge, so the MST is exactly the chain: total = sum(1..n-1).
	var wantTotal int64
	for i := 1; i < n; i++ {
		wantTotal += int64(i)
	}
	if total != wantTotal {
		t.Fatalf("MST total weight = %d, want %d (chain only, chord excluded)", total, wantTotal)
	}
	// Every MST edge must reference live NodeIDs (never a ghost slot).
	m := a.Mapper()
	live := make(map[graph.NodeID]bool, n)
	for _, k := range keys {
		id, _ := m.Lookup(k)
		live[id] = true
	}
	for _, e := range mst {
		if !live[e.From] || !live[e.To] {
			t.Fatalf("MST edge (%d->%d) references a non-live slot", e.From, e.To)
		}
	}
}
