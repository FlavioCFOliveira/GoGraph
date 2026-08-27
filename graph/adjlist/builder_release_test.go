package adjlist

import (
	"context"
	"testing"
)

// retainedBuilders counts the shards still holding a private builder.
func retainedBuilders[W any](a *AdjList[int, W]) int {
	n := 0
	for i := range a.shards {
		s := &a.shards[i]
		s.mu.Lock()
		if s.building != nil {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

func buildBracketed(t *testing.T) *AdjList[int, float64] {
	t.Helper()
	a := New[int, float64](Config{Directed: true, Multigraph: true})
	const nodes = 512
	for i := 0; i < nodes; i++ {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
	}
	a.BeginExclusiveBuild()
	for i := 0; i < nodes; i++ {
		for j := 1; j <= 8; j++ {
			if err := a.AddEdge(i, (i+j)%nodes, 1.0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	a.EndExclusiveBuild()
	return a
}

// The contract on adjShard.building says "the window end freezes it by clearing
// this field". Until rmp #2628 nothing did: the field was released only lazily,
// by storeEntry, when a write presenting a DIFFERENT owner happened to touch the
// same shard, so a shard no later transaction touched pinned its builder for the
// lifetime of the graph.
func TestEndExclusiveBuildReleasesBuilders(t *testing.T) {
	a := buildBracketed(t)
	if got := retainedBuilders(a); got != 0 {
		t.Errorf("after EndExclusiveBuild: %d shards still hold a builder, want 0", got)
	}
}

// Compact is the operation that turns a retained builder into a measurable cost:
// it replaces slotsRef, so a leftover builder keeps the shard's UNTRIMMED array
// alive next to the trimmed one that replaced it, and the call that exists to
// give memory back roughly doubles the resident adjacency instead. Measured at
// 3M edges before the fix: 159.9 MiB unbracketed vs 361.9 MiB bracketed.
func TestCompactReleasesBuilders(t *testing.T) {
	a := buildBracketed(t)
	// Re-open a window and touch shards again, so builders exist at Compact time
	// even if the window close had failed to clear them.
	a.BeginExclusiveBuild()
	for i := 0; i < 512; i += 2 {
		if err := a.AddEdge(i, (i+9)%512, 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	a.Compact(context.Background())
	if got := retainedBuilders(a); got != 0 {
		t.Errorf("after Compact: %d shards still hold a builder, want 0", got)
	}
	a.EndExclusiveBuild()
}

// Releasing the builder must not lose a write: storeEntry publishes the builder
// into slotsRef on the shard's first touch, so everything the window wrote is
// already reachable through the published pointer.
func TestBuilderReleasePreservesEveryEdge(t *testing.T) {
	a := buildBracketed(t)
	a.Compact(context.Background())
	for i := 0; i < 512; i++ {
		for j := 1; j <= 8; j++ {
			if !a.HasEdge(i, (i+j)%512) {
				t.Fatalf("edge %d->%d lost", i, (i+j)%512)
			}
		}
	}
}
