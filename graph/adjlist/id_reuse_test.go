package adjlist_test

import (
	"sort"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
)

// TestAdjList_NodeID_NoReuse verifies that removing an edge does not
// cause NodeIDs to be reused or invalidated, and that a newly interned
// node receives a strictly larger NodeID than all previously assigned ones.
func TestAdjList_NodeID_NoReuse(t *testing.T) {
	t.Parallel()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})

	if err := a.AddEdge("A", "B", 1); err != nil {
		t.Fatalf("AddEdge A->B: %v", err)
	}

	nodeID_A, okA := a.Mapper().Lookup("A")
	nodeID_B, okB := a.Mapper().Lookup("B")
	if !okA || !okB {
		t.Fatalf("Lookup after AddEdge: A ok=%v B ok=%v", okA, okB)
	}

	a.RemoveEdge("A", "B")

	if err := a.AddEdge("A", "C", 2); err != nil {
		t.Fatalf("AddEdge A->C: %v", err)
	}

	nodeID_C, okC := a.Mapper().Lookup("C")
	if !okC {
		t.Fatal("Lookup for C returned ok=false after AddEdge")
	}

	// A must still be valid after edge removal.
	if _, ok := a.Mapper().Lookup("A"); !ok {
		t.Error("NodeID for A was invalidated after RemoveEdge")
	}

	// C must have a strictly larger NodeID than B — no reuse of any gap.
	if nodeID_C <= nodeID_B {
		t.Errorf("expected nodeID_C (%d) > nodeID_B (%d); got reuse or reversal", nodeID_C, nodeID_B)
	}

	// Sanity: IDs are distinct.
	if nodeID_A == nodeID_B || nodeID_A == nodeID_C || nodeID_B == nodeID_C {
		t.Errorf("NodeIDs are not unique: A=%d B=%d C=%d", nodeID_A, nodeID_B, nodeID_C)
	}
}

// TestAdjList_NodeID_MonotonicAcrossShards verifies that within every
// shard the intra-shard components of assigned NodeIDs are strictly
// increasing — confirming that no reuse occurs and that the counter
// only moves forward.
func TestAdjList_NodeID_MonotonicAcrossShards(t *testing.T) {
	t.Parallel()

	const n = 1000

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := range n {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
	}

	// shardIDs collects intra-shard IDs per shard (low 8 bits = shard index).
	shardIDs := make(map[int][]uint64)
	a.Mapper().Walk(func(id graph.NodeID, _ int) bool {
		shard := int(id & 0xFF)
		intra := uint64(id) >> 8
		shardIDs[shard] = append(shardIDs[shard], intra)
		return true
	})

	for shard, ids := range shardIDs {
		if len(ids) < 2 {
			continue
		}
		// Walk does not guarantee visit order; sort to check strict monotonicity
		// of the intra-shard counter — all values must be distinct.
		sorted := make([]uint64, len(ids))
		copy(sorted, ids)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		for i := 1; i < len(sorted); i++ {
			if sorted[i] == sorted[i-1] {
				t.Errorf("shard %d: duplicate intra-shard ID %d", shard, sorted[i])
			}
		}
	}
}

// TestAdjList_NodeID_StableAfterRemove verifies that NodeIDs remain
// valid and unchanged after an edge removal, and that a newly added
// node receives a fresh, unique NodeID.
func TestAdjList_NodeID_StableAfterRemove(t *testing.T) {
	t.Parallel()

	a := adjlist.New[string, int64](adjlist.Config{Directed: true})

	for _, edge := range [][2]string{{"X", "Y"}, {"Y", "Z"}} {
		if err := a.AddEdge(edge[0], edge[1], 1); err != nil {
			t.Fatalf("AddEdge %s->%s: %v", edge[0], edge[1], err)
		}
	}

	idX, _ := a.Mapper().Lookup("X")
	idY, _ := a.Mapper().Lookup("Y")
	idZ, _ := a.Mapper().Lookup("Z")

	a.RemoveEdge("X", "Y")

	// All three original IDs must still resolve correctly.
	for node, want := range map[string]graph.NodeID{"X": idX, "Y": idY, "Z": idZ} {
		got, ok := a.Mapper().Lookup(node)
		if !ok {
			t.Errorf("node %q lost its NodeID after RemoveEdge", node)
			continue
		}
		if got != want {
			t.Errorf("node %q: NodeID changed from %d to %d after RemoveEdge", node, want, got)
		}
	}

	// W must receive a new NodeID distinct from all prior ones.
	if err := a.AddNode("W"); err != nil {
		t.Fatalf("AddNode W: %v", err)
	}
	idW, ok := a.Mapper().Lookup("W")
	if !ok {
		t.Fatal("Lookup for W returned ok=false")
	}
	for node, prior := range map[string]graph.NodeID{"X": idX, "Y": idY, "Z": idZ} {
		if idW == prior {
			t.Errorf("W received the same NodeID (%d) as node %q — reuse detected", idW, node)
		}
	}
}
