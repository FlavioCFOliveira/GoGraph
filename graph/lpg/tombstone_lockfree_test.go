package lpg

import (
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestTombstoneLockfree_LivenessUnchanged pins the observable liveness
// semantics of the copy-on-write tombstone representation (rmp #2039): a
// removed node must be reported tombstoned, excluded from LiveOrder, and listed
// (ascending) by TombstonedIDs, and reviving it must restore all three exactly
// as the previous map-backed representation did.
func TestTombstoneLockfree_LivenessUnchanged(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})

	const n = 6
	ids := make(map[string]graph.NodeID, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%q): %v", key, err)
		}
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			t.Fatalf("node %q not mapped", key)
		}
		ids[key] = id
	}

	if got := g.LiveOrder(); got != n {
		t.Fatalf("LiveOrder on fresh graph = %d, want %d", got, n)
	}
	if got := g.TombstoneCount(); got != 0 {
		t.Fatalf("TombstoneCount on fresh graph = %d, want 0", got)
	}
	for key, id := range ids {
		if g.IsTombstoned(id) {
			t.Fatalf("node %q (id %d) reported tombstoned before any delete", key, id)
		}
	}

	// Remove two nodes.
	g.RemoveNode("k1")
	g.RemoveNode("k3")

	if got := g.TombstoneCount(); got != 2 {
		t.Fatalf("TombstoneCount after 2 removes = %d, want 2", got)
	}
	if got := g.LiveOrder(); got != n-2 {
		t.Fatalf("LiveOrder after 2 removes = %d, want %d", got, n-2)
	}
	if !g.IsTombstoned(ids["k1"]) || !g.IsTombstoned(ids["k3"]) {
		t.Fatal("removed nodes not reported tombstoned")
	}
	if g.IsTombstoned(ids["k0"]) || g.IsTombstoned(ids["k2"]) {
		t.Fatal("surviving node wrongly reported tombstoned")
	}

	// TombstonedIDs must be ascending and contain exactly the removed ids.
	dead := g.TombstonedIDs()
	if len(dead) != 2 {
		t.Fatalf("TombstonedIDs len = %d, want 2 (%v)", len(dead), dead)
	}
	if dead[0] > dead[1] {
		t.Fatalf("TombstonedIDs not ascending: %v", dead)
	}
	want := map[graph.NodeID]bool{ids["k1"]: true, ids["k3"]: true}
	for _, id := range dead {
		if !want[id] {
			t.Fatalf("TombstonedIDs has unexpected id %d (%v)", id, dead)
		}
	}

	// Revive k1 by re-creating it: liveness must fully restore.
	if err := g.AddNode("k1"); err != nil {
		t.Fatalf("revive AddNode(k1): %v", err)
	}
	if g.IsTombstoned(ids["k1"]) {
		t.Fatal("revived node still reported tombstoned")
	}
	if got := g.TombstoneCount(); got != 1 {
		t.Fatalf("TombstoneCount after revive = %d, want 1", got)
	}
	if got := g.LiveOrder(); got != n-1 {
		t.Fatalf("LiveOrder after revive = %d, want %d", got, n-1)
	}
	if dead := g.TombstonedIDs(); len(dead) != 1 || dead[0] != ids["k3"] {
		t.Fatalf("TombstonedIDs after revive = %v, want [%d]", dead, ids["k3"])
	}

	// Reviving the last tombstone must drop the set back to empty, and the
	// lock-free gate must report the graph as clean again.
	if err := g.AddNode("k3"); err != nil {
		t.Fatalf("revive AddNode(k3): %v", err)
	}
	if got := g.TombstoneCount(); got != 0 {
		t.Fatalf("TombstoneCount after full revive = %d, want 0", got)
	}
	if got := g.LiveOrder(); got != n {
		t.Fatalf("LiveOrder after full revive = %d, want %d", got, n)
	}
	if dead := g.TombstonedIDs(); len(dead) != 0 {
		t.Fatalf("TombstonedIDs after full revive = %v, want empty", dead)
	}
}

// TestTombstoneLockfree_ConcurrentScanDuringDelete drives many reader
// goroutines sweeping the lock-free tombstone readers (IsTombstoned, LiveOrder,
// TombstonedIDs) while a writer removes and revives a rotating window of nodes.
// It is the copy-on-write publish vs lock-free read race check for rmp #2039:
// run under -race, a data race between the writer's atomic.Store and a reader's
// atomic.Load — or a mutation of an already-published bitmap — fails the test.
func TestTombstoneLockfree_ConcurrentScanDuringDelete(t *testing.T) {
	t.Parallel()
	const (
		n       = 4000
		readers = 8
		rounds  = 3000
	)
	g := New[string, int64](adjlist.Config{Directed: true})
	ids := make([]graph.NodeID, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		id, ok := g.AdjList().Mapper().Lookup(key)
		if !ok {
			t.Fatalf("node %q not mapped", key)
		}
		ids[i] = id
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Exercise every lock-free reader on the tombstone set.
				live := 0
				for _, id := range ids {
					if !g.IsTombstoned(id) {
						live++
					}
				}
				_ = live
				_ = g.LiveOrder()
				_ = g.TombstonedIDs()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for round := 0; round < rounds; round++ {
			key := fmt.Sprintf("n%d", round%n)
			g.RemoveNode(key)
			// Re-create (revive) so the working set churns both directions,
			// forcing repeated copy-on-write publishes of a non-trivial set.
			if err := g.AddNode(key); err != nil {
				t.Errorf("revive AddNode(%q): %v", key, err)
				return
			}
		}
	}()

	wg.Wait()

	// After the writer has revived everything it removed, the graph must be
	// back to fully live.
	if got := g.TombstoneCount(); got != 0 {
		t.Fatalf("TombstoneCount after churn = %d, want 0", got)
	}
	if got := g.LiveOrder(); got != n {
		t.Fatalf("LiveOrder after churn = %d, want %d", got, n)
	}
}
