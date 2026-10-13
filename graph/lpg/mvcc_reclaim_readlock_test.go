package lpg

// mvcc_reclaim_readlock_test.go — rmp #3108: the property sweep decides under
// the shard's READ lock whether a shard has anything to unlink, and takes the
// write lock only when it does.
//
// Layer: short.
//
// Three properties are pinned. The skip itself: a shard with history but
// nothing reclaimable is swept while another goroutine holds its read lock,
// which the write-locked sweep could not do. Visibility: a pinned reader keeps
// every version it can see across reclaims that take either path. Abort
// Atomicity: an ABORTED head the sweep finds is withdrawn in full, restoring
// the pre-image to the stored bag.

import (
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestReclaimPropVersions_SkipsShardWithNothingToReclaim fails on the
// write-locked sweep: the test holds the shard's read lock, so a sweep that
// takes the write lock on a shard with nothing to unlink blocks until released.
func TestReclaimPropVersions_SkipsShardWithNothingToReclaim(t *testing.T) {
	g := reclaimGraph(t)
	id, _ := g.adj.Mapper().Lookup("a")
	old := g.readTS()
	if err := g.SetNodeProperty("a", "w", Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if g.PropDeltaCount() != 1 {
		t.Fatalf("one property write left %d deltas, want 1", g.PropDeltaCount())
	}

	sh := g.nodePropShardFor(id)
	sh.mu.RLock()
	done := make(chan int, 1)
	go func() { done <- g.reclaimPropVersions(old) }()
	select {
	case n := <-done:
		sh.mu.RUnlock()
		if n != 0 {
			t.Fatalf("freed %d records below a watermark older than every version", n)
		}
	case <-time.After(5 * time.Second):
		sh.mu.RUnlock()
		<-done
		t.Fatal("the sweep waited for the write lock on a shard with nothing to reclaim")
	}
	if g.PropDeltaCount() != 1 {
		t.Fatalf("%d deltas remain after a no-op sweep, want 1", g.PropDeltaCount())
	}
}

// TestReclaimPropVersions_PinnedReaderAcrossBothPaths drives one sweep that
// unlinks (write-locked path) and a second at the same watermark that has
// nothing left to unlink (read-locked skip), and checks after each that a reader
// pinned at the watermark still sees its version and that both paths report the
// identical depth distribution.
func TestReclaimPropVersions_PinnedReaderAcrossBothPaths(t *testing.T) {
	g := reclaimGraph(t)
	const nodes = 64 // spread across many of the 256 shards
	keyID := g.propKeys().intern("w")
	ids := make([]graph.NodeID, nodes)
	for i := range nodes {
		name := fmt.Sprintf("n%d", i)
		if err := g.AddNode(name); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		id, _ := g.adj.Mapper().Lookup(name)
		ids[i] = id
		for v := int64(1); v <= 2; v++ {
			if err := g.SetNodeProperty(name, "w", Int64Value(v)); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
	}
	pinned := g.readTS() // the reader sees w=2 everywhere
	for i := range nodes {
		if err := g.SetNodeProperty(fmt.Sprintf("n%d", i), "w", Int64Value(3)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}

	check := func(pass string) {
		t.Helper()
		for _, id := range ids {
			bag := g.propBagAsOf(id, pinned, 0)
			if v, ok := bag.get(keyID); !ok || v.v != int64(2) {
				t.Fatalf("%s: the pinned reader reads %v/%v, want 2: a version it can see was reclaimed",
					pass, v.v, ok)
			}
			now := g.propBagAsOf(id, g.readTS(), 0)
			if v, ok := now.get(keyID); !ok || v.v != int64(3) {
				t.Fatalf("%s: the current value reads %v/%v, want 3", pass, v.v, ok)
			}
		}
	}

	if n := g.reclaimPropVersions(pinned); n != 2*nodes {
		t.Fatalf("first sweep freed %d, want %d (the two versions at or below the pinned reader)", n, 2*nodes)
	}
	first := g.depth(depthNodeProps).Load()
	check("unlinking sweep")

	if n := g.reclaimPropVersions(pinned); n != 0 {
		t.Fatalf("second sweep at the same watermark freed %d, want 0", n)
	}
	second := g.depth(depthNodeProps).Load()
	check("skipping sweep")
	if first != second {
		t.Fatalf("the read-locked skip reported depths %+v, the write-locked walk %+v: they must be identical",
			second, first)
	}
	if g.PropDeltaCount() != nodes {
		t.Fatalf("%d deltas remain, want %d (one per node, held by the pinned reader)", g.PropDeltaCount(), nodes)
	}
}

// TestReclaimPropVersions_AbortedHeadFullyWithdrawn plants an ABORTED head the
// abort's own write-set withdrawal did not reach (the shape the sweep exists
// for), on a shard whose every other version is above the watermark, and checks
// the sweep still takes the write path and restores the pre-image.
func TestReclaimPropVersions_AbortedHeadFullyWithdrawn(t *testing.T) {
	g := reclaimGraph(t)
	id, _ := g.adj.Mapper().Lookup("a")
	old := g.readTS()
	if err := g.SetNodeProperty("a", "w", Int64Value(1)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetNodeProperty("a", "w", Int64Value(2)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	// Mark the newest version ABORTED: its write (w=2) was never committed.
	sh := g.nodePropShardFor(id)
	sh.mu.Lock()
	head := sh.d[id]
	head.info, head.ts = nil, mvcc.AbortedTS
	sh.mu.Unlock()

	// Watermark below every version: only the aborted head is reclaimable.
	if n := g.reclaimPropVersions(old); n != 1 {
		t.Fatalf("the sweep freed %d, want 1 (the aborted head)", n)
	}
	if v, ok := g.GetNodeProperty("a", "w"); !ok || v.v != int64(1) {
		t.Fatalf("after withdrawal w reads %v/%v, want the committed 1", v.v, ok)
	}
	if g.PropDeltaCount() != 1 {
		t.Fatalf("%d deltas remain, want 1 (the committed w=1 version)", g.PropDeltaCount())
	}
	sh.mu.RLock()
	h := sh.d[id]
	sh.mu.RUnlock()
	if h == nil || h.stampTS() == mvcc.AbortedTS {
		t.Fatal("an aborted head survived the sweep")
	}
}
