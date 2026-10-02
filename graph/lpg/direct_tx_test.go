package lpg

// direct_tx_test.go — rmp #2947: a direct Go-API write is an implicit
// single-operation transaction, all-or-nothing under every interleaving the
// audit of the rejected point-check design found (findings F1–F5).
//
// Each test drives a peer transaction's write into one window of a direct write
// and then asserts the outcome is one a serial order of the acknowledged writes
// produces: either the direct write applied whole, or it was refused with
// ErrDirectWriteConflict and changed nothing.

import (
	"errors"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func newDirectTxGraph(t *testing.T, directed bool) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func requireNoErr(t *testing.T, errs ...error) {
	t.Helper()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("setup step %d: %v", i, err)
		}
	}
}

// removalWindows names the two windows of a direct edge removal a peer write is
// driven into: just before its adjacency write and just after it.
var removalWindows = []struct {
	name  string
	after bool
}{
	{"before the adjacency write", false},
	{"after the adjacency write", true},
}

func driveRemovalWindow(g *Graph[string, float64], after bool, peer func()) {
	if after {
		setEdgeRemovalSeams(g, nil, peer)
	} else {
		setEdgeRemovalSeams(g, peer, nil)
	}
}

// TestDirectTx_RemoveEdgeIsAllOrNothing is audit finding F1: a peer append to the
// source between the removal's adjacency write and its per-pair cleanup made the
// cleanup refuse, and the direct RemoveEdge returned nil with the pair's CREATE
// counter and per-instance records still in place, so re-creating the pair
// resurrected them.
func TestDirectTx_RemoveEdgeIsAllOrNothing(t *testing.T) {
	for _, w := range removalWindows {
		t.Run(w.name, func(t *testing.T) {
			g := newDirectTxGraph(t, true)
			requireNoErr(t, g.AddNode("a"), g.AddNode("b"), g.AddNode("c"), g.AddEdge("a", "b", 1))
			g.IncEdgeCreateCount("a", "b")
			requireNoErr(t,
				g.SetEdgePropertyAt("a", "b", 1, "k", StringValue("old")),
				g.SetEdgeLabelAt("a", "b", 1, "OLD"))

			var peer WriteTx
			var peerErr error
			fired := false
			driveRemovalWindow(g, w.after, func() {
				fired = true
				peer = g.BeginVersionedTx()
				peerErr = g.Writer(peer).AddEdge("a", "c", 1)
			})
			err := g.RemoveEdge("a", "b")
			setEdgeRemovalSeams(g, nil, nil)
			if !fired {
				t.Fatal("the removal window never ran")
			}
			peerApplied := peerErr == nil && peer.Err() == nil
			g.EndVersionedTx(peer)
			g.ReclaimNow()

			if got := g.AdjList().HasEdge("a", "c"); got != peerApplied {
				t.Errorf("peer append: acknowledged=%v but the arc is present=%v", peerApplied, got)
			}
			switch {
			case err == nil:
				if g.AdjList().HasEdge("a", "b") {
					t.Fatal("RemoveEdge returned nil and the arc is still there")
				}
				requireNoErr(t, g.AddEdge("a", "b", 1))
				if n := g.IncEdgeCreateCount("a", "b"); n != 1 {
					t.Errorf("re-created pair's CREATE counter resumed at %d, want 1", n)
				}
				if p := g.EdgePropertiesAt("a", "b", 1); len(p) != 0 {
					t.Errorf("re-created pair resurrected per-instance properties %v", p)
				}
				if l := g.EdgeLabelsAt("a", "b", 1); len(l) != 0 {
					t.Errorf("re-created pair resurrected per-instance labels %v", l)
				}
			case errors.Is(err, ErrDirectWriteConflict):
				if !g.AdjList().HasEdge("a", "b") {
					t.Error("RemoveEdge was refused and the arc is gone")
				}
				if n := g.EdgeCreateCount("a", "b"); n != 1 {
					t.Errorf("refused removal changed the CREATE counter to %d", n)
				}
				if v, _ := g.EdgePropertiesAt("a", "b", 1)["k"].String(); v != "old" {
					t.Errorf("refused removal changed the per-instance property to %q", v)
				}
				if l := g.EdgeLabelsAt("a", "b", 1); !slices.Contains(l, "OLD") {
					t.Errorf("refused removal changed the per-instance labels to %v", l)
				}
			default:
				t.Fatalf("RemoveEdge: %v", err)
			}
		})
	}
}

// TestDirectTx_RemoveEdgeByHandleIsAllOrNothing is audit finding F2: a peer's
// per-handle write after the removal's checks made RemoveEdgeByHandle report
// removed=true together with a conflict, the slot gone and the handle's records
// left behind as orphans.
func TestDirectTx_RemoveEdgeByHandleIsAllOrNothing(t *testing.T) {
	for _, w := range removalWindows {
		t.Run(w.name, func(t *testing.T) {
			g := newDirectTxGraph(t, true)
			h1, err := g.AddEdgeH("a", "b", 1)
			requireNoErr(t, err)
			_, err = g.AddEdgeH("a", "b", 2)
			requireNoErr(t, err, g.SetEdgeLabelByHandle("a", "b", h1, "X"))

			var peer WriteTx
			var peerErr error
			driveRemovalWindow(g, w.after, func() {
				peer = g.BeginVersionedTx()
				peerErr = g.Writer(peer).SetEdgePropertyByHandle("a", "b", h1, "p", Int64Value(7))
			})
			removed, err := g.RemoveEdgeByHandle("a", "b", h1)
			setEdgeRemovalSeams(g, nil, nil)
			if !peer.Valid() {
				t.Fatal("the removal window never ran")
			}
			peerApplied := peerErr == nil && peer.Err() == nil
			g.EndVersionedTx(peer)
			g.ReclaimNow()

			present := slices.Contains(g.AppendEdgeHandles("a", "b", nil), h1)
			labels := g.EdgeLabelsByHandle("a", "b", h1)
			props := g.EdgePropertiesByHandle("a", "b", h1)
			switch {
			case err == nil:
				if !removed || present {
					t.Fatalf("RemoveEdgeByHandle = (%v, nil) and the slot present=%v", removed, present)
				}
				if len(labels) != 0 {
					t.Errorf("removed instance left its labels behind: %v", labels)
				}
				// The removal committed first, so the peer's acknowledged write,
				// if any, is the only record the handle may still carry: a
				// per-handle property write does not require the slot.
				if n, _ := props["p"].Int64(); peerApplied && (len(props) != 1 || n != 7) {
					t.Errorf("the peer's acknowledged write is not what the handle carries: %v", props)
				}
				if !peerApplied && len(props) != 0 {
					t.Errorf("removed instance carries properties no committed write made: %v", props)
				}
			case errors.Is(err, ErrDirectWriteConflict):
				if removed || !present {
					t.Fatalf("refused RemoveEdgeByHandle reported removed=%v and the slot present=%v", removed, present)
				}
				if !slices.Contains(labels, "X") {
					t.Errorf("refused removal changed the instance's labels to %v", labels)
				}
			default:
				t.Fatalf("RemoveEdgeByHandle: %v", err)
			}
		})
	}
}

// TestDirectTx_RemoveEdgeDoesNotStackOnAPendingOverflow is audit finding F3: the
// side-store writes of a direct removal ran untested, so a removal that
// re-asserted a surviving sibling's type into the pair's overflow built on a
// peer's uncommitted overflow write, and that write survived the peer's abort.
func TestDirectTx_RemoveEdgeDoesNotStackOnAPendingOverflow(t *testing.T) {
	for _, w := range removalWindows {
		t.Run(w.name, func(t *testing.T) {
			g := newDirectTxGraph(t, true)
			// Two parallel a→b slots typed X and Z: removing the X slot leaves no
			// free slot for X, so the removal re-asserts it into the overflow.
			requireNoErr(t,
				g.AddEdgeLabeled("a", "b", 1, "X"),
				g.AddEdgeLabeled("a", "b", 1, "Z"))

			var peer WriteTx
			var peerErr error
			driveRemovalWindow(g, w.after, func() {
				peer = g.BeginVersionedTx()
				// No free slot, so this spills to the pair's overflow.
				peerErr = g.Writer(peer).SetEdgeLabel("a", "b", "Y")
			})
			err := g.RemoveEdge("a", "b")
			setEdgeRemovalSeams(g, nil, nil)
			if !peer.Valid() {
				t.Fatal("the removal window never ran")
			}
			// The peer ROLLS BACK: whatever it wrote must leave no trace.
			if peerErr == nil {
				peer.w.conflict.CompareAndSwap(nil, errAbortForTest)
			}
			g.EndVersionedTx(peer)
			g.ReclaimNow()

			labels := g.EdgeLabels("a", "b")
			slices.Sort(labels)
			if slices.Contains(labels, "Y") {
				t.Errorf("a rolled-back peer's overflow type survived the direct removal: labels=%v", labels)
			}
			switch {
			case err == nil:
				if want := []string{"X", "Z"}; !slices.Equal(labels, want) {
					t.Errorf("the surviving edge carries %v, want the pair's types %v", labels, want)
				}
			case errors.Is(err, ErrDirectWriteConflict):
				if n := len(g.AppendEdgeHandles("a", "b", nil)); n != 2 {
					t.Errorf("refused removal left %d slots, want 2", n)
				}
			default:
				t.Fatalf("RemoveEdge: %v", err)
			}
		})
	}
}

// errAbortForTest dooms a peer transaction so its end aborts it — the outcome of
// a transaction refused by a conflict, whose versions are withdrawn rather than
// published.
var errAbortForTest = mvcc.NewConflict(mvcc.StoreNodeLabels, 0, 0, 0)

// TestDirectTx_RemoveNodeVsPeerLabelRemovalRollback is audit finding F4: a peer
// removed the node's label while a direct RemoveNode was retiring it and then
// rolled back, and the dead node stayed in the label's bitmap for good — the
// peer's deferred index removal replaced the retirement's, and the peer's abort
// withdrew both.
func TestDirectTx_RemoveNodeVsPeerLabelRemovalRollback(t *testing.T) {
	for _, window := range []string{"after the claims", "between the strip and the flip"} {
		t.Run(window, func(t *testing.T) {
			g := newDirectTxGraph(t, true)
			requireNoErr(t, g.SetNodeLabel("n", "L"), g.SetNodeProperty("n", "k", StringValue("v")))
			id, _ := g.adj.Mapper().Lookup("n")
			lid := g.reg.intern("L")

			var peer WriteTx
			peerWrite := func() {
				peer = g.BeginVersionedTx()
				_ = g.Writer(peer).RemoveNodeLabel("n", "L")
			}
			if window == "after the claims" {
				setNodeRemovalSeam(g, peerWrite)
			} else {
				g.retireStripFlipHookForTest = func() {
					g.retireStripFlipHookForTest = nil
					peerWrite()
				}
			}
			err := g.RemoveNode("n")
			setNodeRemovalSeam(g, nil)
			g.retireStripFlipHookForTest = nil
			if !peer.Valid() {
				t.Fatal("the removal window never ran")
			}
			// The peer rolls back as the engine does: its undo log re-asserts the
			// label inside its own transaction, then its end aborts it.
			peer.EnterUndo()
			_ = g.Writer(peer).SetNodeLabel("n", "L")
			peer.ExitUndo()
			peer.w.conflict.CompareAndSwap(nil, errAbortForTest)
			g.EndVersionedTx(peer)
			g.ReclaimNow()

			dead := g.IsTombstoned(id)
			inScan := g.LabelBitmapAsOf(lid, nil).Contains(uint64(id))
			switch {
			case err == nil:
				if !dead {
					t.Fatal("RemoveNode returned nil and the node is alive")
				}
				if inScan {
					t.Error("a dead node is in its label's present-time scan after a peer's rolled-back label removal")
				}
			case errors.Is(err, ErrDirectWriteConflict):
				if dead {
					t.Fatal("RemoveNode was refused and the node is dead")
				}
				if !inScan || !g.HasNodeLabel("n", "L") {
					t.Error("refused removal and rolled-back peer lost the node's label")
				}
			default:
				t.Fatalf("RemoveNode: %v", err)
			}
		})
	}
}

// TestDirectTx_LabelSetBetweenStripAndFlip is audit finding F5: a NEW label set
// between a retirement's strip and its tombstone flip went into the bitmap while
// the node was alive and was never retired, so the dead node stayed in that
// label's present-time scan.
func TestDirectTx_LabelSetBetweenStripAndFlip(t *testing.T) {
	for _, bracket := range []bool{true, false} {
		name := "two direct writes"
		if bracket {
			name = "one exclusive bracket"
		}
		t.Run(name, func(t *testing.T) {
			g, _, id := retireFixture(t)
			var setErr error
			fired := false
			// The bracket's transaction once it is open; the zero value makes
			// the hook's label a direct write of its own.
			var hookTx WriteTx
			g.retireStripFlipHookForTest = func() {
				g.retireStripFlipHookForTest = nil
				fired = true
				setErr = g.Writer(hookTx).SetNodeLabel("a", "M")
			}
			var err error
			if bracket {
				err = g.ApplyAtomicallyTx(func(tx WriteTx) error {
					hookTx = tx
					if !g.Writer(tx).RemoveNode("a") {
						t.Errorf("RemoveNode inside the bracket was refused: %v", tx.Err())
					}
					return nil
				})
			} else {
				err = g.RemoveNode("a")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the seam between the strip and the flip never ran")
			}
			// Inside one bracket the label is written in the retirement's
			// transaction; as a second direct write it waits on the retirement
			// and is refused.
			if bracket && setErr != nil {
				t.Fatalf("label inside the bracket: %v", setErr)
			}
			if !bracket && setErr != nil && !errors.Is(setErr, ErrDirectWriteConflict) {
				t.Fatalf("label as a second direct write: %v", setErr)
			}
			g.ReclaimNow()
			mid := g.reg.intern("M")
			if !g.IsTombstoned(id) {
				t.Fatal("setup: the node is not removed")
			}
			if g.LabelBitmapAsOf(mid, nil).Contains(uint64(id)) {
				t.Error("a dead node is in the present-time scan of a label set during its retirement")
			}
		})
	}
}

// TestDirectTx_RefusedAppendDoesNotLeaveADeadEndpoint pins the one effect of a
// refused append that the abort cannot withdraw by itself: the endpoint it
// CREATED stays interned, tombstoned by the abort so that no reader ever saw it.
// An append does not revive a tombstoned key, so the retried AddEdge used to link
// the edge to a dead node. A key whose only existence was an aborted creation is
// now created again by the next append (rmp #2947).
func TestDirectTx_RefusedAppendDoesNotLeaveADeadEndpoint(t *testing.T) {
	g := newDirectTxGraph(t, true)
	requireNoErr(t, g.AddEdge("a", "b", 1))
	// A peer's pending write on a's entry that took no claim: an append claims
	// a's adjacency without meeting it, creates x, and is refused by the entry
	// itself. Since rmp #2966 every lpg writer that rebuilds an entry claims the
	// node first, so the peer writes the adjacency directly to reach this shape.
	peer := g.BeginVersionedTx()
	aID, _ := g.adj.Mapper().Lookup("a")
	if _, err := g.adj.Writer(peer.w.adjTx()).UpdateEntryAux(aID, func(cur adjlist.AuxColumn, _ []graph.NodeID) (adjlist.AuxColumn, bool) {
		return cur, true
	}); err != nil {
		t.Fatalf("setup: the peer's adjacency write: %v", err)
	}
	if err := g.AddEdge("a", "x", 1); !errors.Is(err, ErrDirectWriteConflict) {
		t.Fatalf("append over a pending entry returned %v, want ErrDirectWriteConflict", err)
	}
	g.EndVersionedTx(peer)
	g.ReclaimNow()
	id, ok := g.adj.Mapper().Lookup("x")
	if !ok {
		t.Fatal("setup: the refused append created no endpoint to test")
	}
	if !g.IsTombstoned(id) {
		t.Fatal("the refused append's endpoint is visible: the abort did not withdraw it")
	}
	requireNoErr(t, g.AddEdge("a", "x", 1))
	if g.IsTombstoned(id) {
		t.Fatal("the retried append linked the edge to a dead endpoint")
	}
	snap := g.BeginRead()
	defer g.EndRead(snap)
	if !g.NodeExistsAsOf(id, snap) {
		t.Fatal("the retried append's endpoint does not exist for a new reader")
	}
}

// f7Fixture is audit finding F7's setup: an explicit transaction holds an
// uncommitted write on x, so an exclusive bracket that then writes x is doomed;
// z is a node nothing else touches.
func f7Fixture(t *testing.T) (*Graph[string, float64], WriteTx) {
	t.Helper()
	g := newDirectTxGraph(t, true)
	requireNoErr(t, g.SetNodeProperty("x", "v", Int64Value(0)), g.AddNode("z"))
	peer := g.BeginVersionedTx()
	requireNoErr(t, g.Writer(peer).SetNodeProperty("x", "v", Int64Value(1)))
	return g, peer
}

// TestDirectTx_ForeignWriteBeforeTheBracketIsDoomed is audit finding F7: a direct
// write from an UNRELATED goroutine, made while an exclusive bracket was open,
// joined the bracket's transaction. It was acknowledged, and then the bracket was
// doomed and aborted and took the write with it. A direct write now never joins
// a bracket, so it commits on its own (rmp #2947).
func TestDirectTx_ForeignWriteBeforeTheBracketIsDoomed(t *testing.T) {
	g, peer := f7Fixture(t)
	inside := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		<-inside
		written <- g.SetNodeProperty("z", "k", StringValue("acked"))
	}()
	bErr := g.ApplyAtomicallyTx(func(tx WriteTx) error {
		close(inside)
		// The foreign write returns before the bracket's own write dooms it.
		fe := <-written
		written <- fe
		_ = g.Writer(tx).SetNodeProperty("x", "v", Int64Value(2))
		return nil
	})
	fe := <-written
	g.EndVersionedTx(peer)
	g.ReclaimNow()
	if !errors.Is(bErr, mvcc.ErrSerializationConflict) {
		t.Fatalf("setup: the bracket returned %v, want a serialization conflict", bErr)
	}
	if fe != nil {
		t.Fatalf("the foreign direct write on an unrelated node was refused: %v", fe)
	}
	if v, ok := g.GetNodeProperty("z", "k"); !ok || v != StringValue("acked") {
		t.Fatalf("an acknowledged direct write on an unrelated node was lost with the doomed "+
			"exclusive bracket: z.k = %v present=%v", v, ok)
	}
}

// TestDirectTx_ForeignWriteAfterTheBracketIsDoomed is F7's other order: the
// foreign direct write lands after the bracket is doomed. Joining the bracket made
// it share the bracket's refusal, so a write on a node no transaction had touched
// was refused, and named a conflict it never had (rmp #2947).
func TestDirectTx_ForeignWriteAfterTheBracketIsDoomed(t *testing.T) {
	g, peer := f7Fixture(t)
	inside := make(chan struct{})
	release := make(chan struct{})
	foreign := make(chan error, 1)
	go func() {
		<-inside
		foreign <- g.SetNodeProperty("z", "k", StringValue("acked"))
		close(release)
	}()
	bErr := g.ApplyAtomicallyTx(func(tx WriteTx) error {
		_ = g.Writer(tx).SetNodeProperty("x", "v", Int64Value(2))
		close(inside)
		<-release
		return nil
	})
	fe := <-foreign
	g.EndVersionedTx(peer)
	g.ReclaimNow()
	if !errors.Is(bErr, mvcc.ErrSerializationConflict) {
		t.Fatalf("setup: the bracket returned %v, want a serialization conflict", bErr)
	}
	if fe != nil {
		t.Fatalf("a direct write on a node no transaction touched was refused: %v", fe)
	}
	if v, ok := g.GetNodeProperty("z", "k"); !ok || v != StringValue("acked") {
		t.Fatalf("the foreign direct write did not land: z.k = %v present=%v", v, ok)
	}
}

// TestDirectTx_DirectCallInsideABracketIsItsOwnTransaction pins the contract that
// replaced joining (rmp #2947): inside an exclusive bracket, a direct call is an
// implicit transaction of its own. It is visible to a new snapshot before the
// bracket ends, it survives the bracket's abort, and the bracket's own uncommitted
// write refuses it like any other transaction's.
func TestDirectTx_DirectCallInsideABracketIsItsOwnTransaction(t *testing.T) {
	g := newDirectTxGraph(t, true)
	requireNoErr(t, g.AddNode("a"), g.AddNode("z"))
	errAbort := errors.New("abort the bracket")
	var visibleInside bool
	var ownErr error
	err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
		requireNoErr(t, g.Writer(tx).SetNodeProperty("a", "v", Int64Value(1)))
		requireNoErr(t, g.SetNodeProperty("z", "k", Int64Value(7)))
		snap := g.BeginRead()
		_, visibleInside = g.GetNodePropertyAsOf("z", "k", snap)
		g.EndRead(snap)
		ownErr = g.SetNodeProperty("a", "v", Int64Value(2))
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("bracket returned %v, want its own error", err)
	}
	if !visibleInside {
		t.Error("a direct write inside a bracket was not visible to a new snapshot before the " +
			"bracket ended: it joined the bracket's transaction")
	}
	if !errors.Is(ownErr, ErrDirectWriteConflict) {
		t.Errorf("a direct write over the bracket's own uncommitted write returned %v, want "+
			"ErrDirectWriteConflict", ownErr)
	}
	if v, ok := g.GetNodeProperty("z", "k"); !ok || v != Int64Value(7) {
		t.Errorf("the direct write did not survive the bracket's rollback: z.k = %v present=%v", v, ok)
	}
}
