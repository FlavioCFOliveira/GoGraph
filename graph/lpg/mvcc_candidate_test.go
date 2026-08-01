package lpg

// mvcc_candidate_test.go — the candidate-set discipline (rmp #2290, MVCC P4c).
//
// The versioned stores answer what an object CONTAINS. These tests are about
// which objects a read may CONSIDER, which is a different question and has its
// own two failure directions: seeing something that did not exist yet, and
// losing something that did.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestNodeExistence_IsVersionedInBothDirections pins the pair of failures P4b
// exposed and P4c closes.
func TestNodeExistence_IsVersionedInBothDirections(t *testing.T) {
	g := mvccGraph(t)
	if err := g.ApplyAtomically(func() error { return g.AddNode("old") }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	oldID := mvccNodeID(t, g, "old")

	before := snapAt(g.readTS())

	// A node created after the reader started.
	if err := g.ApplyAtomically(func() error { return g.AddNode("new") }); err != nil {
		t.Fatalf("create: %v", err)
	}
	newID := mvccNodeID(t, g, "new")
	if g.NodeExistsAsOf(newID, before) {
		t.Error("a reader that started BEFORE a node was created can see it — a scan would emit " +
			"a row of nulls for a node that did not exist yet")
	}
	if !g.NodeExistsAsOf(newID, snapAt(g.readTS())) {
		t.Error("a reader that started after the creation cannot see the node")
	}

	// A node removed after the reader started.
	if err := g.ApplyAtomically(func() error { g.RemoveNode("old"); return nil }); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !g.NodeExistsAsOf(oldID, before) {
		t.Error("a reader that started BEFORE a node was removed has lost it — this direction " +
			"is silent, because the tombstone set is read at the present")
	}
	if g.NodeExistsAsOf(oldID, snapAt(g.readTS())) {
		t.Error("a reader that started after the removal still sees the node")
	}
}

// TestNodeExistence_RemoveThenReviveInOneTransaction pins the tie a commit
// timestamp cannot break.
//
// A rolled-back DELETE is exactly this shape: the statement tombstones the node
// and the undo log revives it, both stamped with the one shared commit record,
// so the two events carry the same instant. Taking the death unconditionally
// made the node vanish for every reader afterwards.
func TestNodeExistence_RemoveThenReviveInOneTransaction(t *testing.T) {
	g := mvccGraph(t)
	if err := g.ApplyAtomically(func() error { return g.AddNode("a") }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := mvccNodeID(t, g, "a")

	if err := g.ApplyAtomically(func() error {
		g.RemoveNode("a")
		return g.AddNode("a") // the undo log's revival
	}); err != nil {
		t.Fatalf("remove+revive: %v", err)
	}
	if !g.NodeExistsAsOf(id, snapAt(g.readTS())) {
		t.Fatal("a node removed and re-created inside ONE transaction reads as absent: the two " +
			"events share a commit record, so only their write ORDER separates them")
	}
}

// TestLabelIndex_RemovalIsDeferredAndVisibleToOlderReaders pins the
// superset-good / missing-fatal asymmetry.
func TestLabelIndex_RemovalIsDeferredAndVisibleToOlderReaders(t *testing.T) {
	g := mvccGraph(t)
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("a"); err != nil {
			return err
		}
		return g.SetNodeLabel("a", "P")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := mvccNodeID(t, g, "a")
	lid := g.reg.Intern("P")
	before := snapAt(g.readTS())

	if err := g.ApplyAtomically(func() error { g.RemoveNodeLabel("a", "P"); return nil }); err != nil {
		t.Fatalf("remove label: %v", err)
	}
	if g.IndexRemovalBacklog() == 0 {
		t.Fatal("the label-index removal was applied eagerly, so a reader older than it has " +
			"already lost the entry and this test cannot detect the loss")
	}
	if !g.LabelBitmapAsOf(lid, before).Contains(uint64(id)) {
		t.Error("a reader that started before the label was removed no longer finds the node in " +
			"the label bitmap — a silently lost row")
	}
	if g.LabelBitmapAsOf(lid, snapAt(g.readTS())).Contains(uint64(id)) {
		t.Error("a reader that started after the removal still finds the node")
	}
	// A label ADDED after the reader started is the harmless direction, and it
	// must still be filtered out rather than emitted.
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("b"); err != nil {
			return err
		}
		return g.SetNodeLabel("b", "P")
	}); err != nil {
		t.Fatalf("add later: %v", err)
	}
	laterID := mvccNodeID(t, g, "b")
	if g.LabelBitmapAsOf(lid, before).Contains(uint64(laterID)) {
		t.Error("a reader that started before a label was added can see the node under it")
	}
}

// TestLabelIndex_DeferredRemovalIsCancelledByReAdd pins the rollback path.
//
// A failed statement strips a node's labels and the undo log puts them back. If
// the strip's deferred removal survived, the next sweep would delete an entry
// that is legitimately present again and the node would vanish from every label
// scan afterwards.
func TestLabelIndex_DeferredRemovalIsCancelledByReAdd(t *testing.T) {
	g := mvccGraph(t)
	if err := g.ApplyAtomically(func() error {
		if err := g.AddNode("a"); err != nil {
			return err
		}
		return g.SetNodeLabel("a", "P")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := mvccNodeID(t, g, "a")
	lid := g.reg.Intern("P")

	if err := g.ApplyAtomically(func() error {
		g.RemoveNodeLabel("a", "P")
		return g.SetNodeLabel("a", "P") // the undo log's inverse
	}); err != nil {
		t.Fatalf("strip+restore: %v", err)
	}
	if n := g.IndexRemovalBacklog(); n != 0 {
		t.Fatalf("%d deferred index removals survived the re-add; the next sweep would delete an "+
			"entry that is present again", n)
	}
	if err := g.ApplyAtomically(func() error { g.ReclaimNow(); return nil }); err != nil {
		t.Fatalf("ReclaimNow: %v", err)
	}
	if !g.LabelBitmapAsOf(lid, snapAt(g.readTS())).Contains(uint64(id)) {
		t.Fatal("the node lost its label-index entry after a sweep, although the label was " +
			"restored before the sweep ran")
	}
}

// TestCandidateFilter_DoesNotDeadlockUnderConcurrentReaders is the regression
// test for a deadlock the eight-reader cell of the fairness soak found.
//
// The bitmap corrector used to hold a label shard's READ lock while visiting a
// suspect, and visiting a suspect resolves its label bag — which read-locks the
// SAME shard. Go's sync.RWMutex is not re-entrant: a writer arriving between
// the outer acquire and the inner one is queued ahead of the inner acquire, and
// the inner acquire then waits for a writer that waits for the outer reader.
// Every reader wedged at zero CPU.
//
// The watchdog is what makes this a TEST rather than a hang: without it a
// regression reports as a timeout twenty minutes later with no attribution.
func TestCandidateFilter_DoesNotDeadlockUnderConcurrentReaders(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	const n = 512
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	lid := g.reg.Intern("P")

	done := make(chan struct{})
	var wg sync.WaitGroup

	// A writer that keeps label history — and therefore the suspect set — alive.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			k := fmt.Sprintf("n%d", i%n)
			_ = g.ApplyAtomically(func() error {
				g.RemoveNodeLabel(k, "P")
				return g.SetNodeLabel(k, "P")
			})
		}
	}()

	// Readers resolving the label bitmap as of their own instant, which is what
	// drives the corrector.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				s := g.BeginRead()
				_ = g.LabelBitmapAsOf(lid, s)
				g.EndRead(s)
			}
		}()
	}

	finished := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Second)
		close(done)
		wg.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("readers resolving a label bitmap against a concurrent label writer did not " +
			"finish within 30 s: the candidate filter is holding a shard lock across a read " +
			"that re-acquires it, which deadlocks under Go's non-re-entrant RWMutex")
	}
}

var _ = graph.NodeID(0)
