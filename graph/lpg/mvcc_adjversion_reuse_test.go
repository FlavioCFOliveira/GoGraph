package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestAdjStamps_EmptiedMapKeptOnlyWhileSmall pins the map lifecycle of rmp
// #3025: a shard emptied by the vacuum keeps its map for the next write while
// that map has never held more than [adjKeepEntries] entries, and releases it
// once it has, so an idle graph retains at most one smallest map per shard.
func TestAdjStamps_EmptiedMapKeptOnlyWhileSmall(t *testing.T) {
	// ids that all hash to shard 0, enough to grow its map past one group.
	var av adjVersions
	ids := make([]graph.NodeID, 0, adjKeepEntries+1)
	for id := graph.NodeID(1); len(ids) < cap(ids); id++ {
		if av.shardIndex(id) == 0 {
			ids = append(ids, id)
		}
	}
	sh := &av.shards[0]
	fill := func(n int) {
		sh.mu.Lock()
		for _, id := range ids[:n] {
			sh.entryLocked(id)
		}
		sh.mu.Unlock()
	}

	// Small: emptied by the sweep, the map is kept for reuse.
	fill(adjKeepEntries)
	if freed := av.truncate(1); freed != adjKeepEntries {
		t.Fatalf("truncate freed %d, want %d", freed, adjKeepEntries)
	}
	if sh.d == nil {
		t.Fatal("an emptied map that never grew past one group was released; the next write reallocates it")
	}
	if av.len() != 0 {
		t.Fatalf("len = %d after the sweep, want 0", av.len())
	}

	// Grown: emptied by the sweep, the map is released, since maps never shrink.
	fill(adjKeepEntries + 1)
	if freed := av.truncate(1); freed != adjKeepEntries+1 {
		t.Fatalf("truncate freed %d, want %d", freed, adjKeepEntries+1)
	}
	if sh.d != nil || sh.grown {
		t.Fatalf("an emptied map that grew past one group was kept (nil=%v grown=%v): an idle graph retains its peak", sh.d == nil, sh.grown)
	}
}
