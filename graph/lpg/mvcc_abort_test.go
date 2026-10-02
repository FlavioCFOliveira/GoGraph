package lpg

// mvcc_abort_test.go — rmp #2300 AC4: a transaction that loses a write-write
// conflict ABORTS, so nothing it wrote is ever visible.
//
// # The atomicity violation this pins, measured
//
// Until [Graph.endWrite] gained its abort branch, EVERY write bracket published its
// commit record — including one whose transaction had been refused. The caller was
// told the transaction failed and part of it was visible anyway:
//
//	transaction writes n.v = 1
//	transaction hits a conflict on its second write, which is refused
//	the bracket returns mvcc.ErrSerializationConflict
//	a FRESH SNAPSHOT then reads n.v = 1        <- the violation
//
// It had not surfaced because the Cypher engine carries an undo log that physically
// restores the stored value, so on that path the chain nets out and publication is
// harmless — which is what graph/lpg/mvcc_write.go's file comment describes and why
// publishing on ROLLBACK is right. But the undo log is a cypher structure: a caller
// using [Graph.ApplyVersioned] directly has none, and the durable store's apply is
// such a caller. Rollback and abort are different things and the substrate has to be
// atomic without help.
//
// # What is deliberately NOT asserted here
//
// AC4 also asks that the aborted versions be RECLAIMABLE. They are not yet: the
// reclaimer's watermark test cannot free a chain whose head reads [mvcc.AbortedTS],
// which sits above [mvcc.TxIDBase]. That is rmp #2318 ("an aborted transaction
// retains its versions FOREVER and leaves the stored value dirty"), a separate task
// in this sprint with its own dependency on the background vacuum (rmp #2308).
// TestAbort_VersionsAreNotYetReclaimable_2318 pins the CURRENT behaviour so that
// closing #2318 has something to flip, rather than leaving the gap unrecorded.

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestAbort_ConflictedTransactionIsNeverVisible is AC4's first half. It fails
// against the build that published unconditionally, reading back the refused
// transaction's write.
func TestAbort_ConflictedTransactionIsNeverVisible(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := g.SetNodeLabel("n", "Seed"); err != nil {
		t.Fatalf("seed label: %v", err)
	}

	var record *mvcc.CommitInfo
	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		// Two writes to two DIFFERENT stores before the conflict, so the assertion
		// covers a transaction that had already spread across the substrate.
		if e := wv.SetNodeProperty("n", "v", Int64Value(1)); e != nil {
			return e
		}
		if e := wv.SetNodeLabel("n", "Added"); e != nil {
			return e
		}
		// Doom it exactly as a real conflict does: record one.
		if e := tx.w.conflictErr("node properties", ^uint64(0)); e == nil {
			t.Fatal("conflictErr returned nil; the transaction was not doomed")
		}
		record = tx.w.tx.OpenRecord()
		return tx.w.err()
	})
	if err == nil {
		t.Fatal("the bracket reported success for a transaction that hit a conflict")
	}
	if record == nil {
		t.Fatal("the transaction allocated no commit record, so it versioned nothing " +
			"and this test would prove nothing")
	}

	if ts := record.TS(); ts != mvcc.AbortedTS {
		t.Fatalf("the refused transaction's commit record reads %d, want mvcc.AbortedTS "+
			"(%d). A published record makes the transaction's partial work VISIBLE even "+
			"though its caller was told it failed (rmp #2300 AC4).", ts, uint64(mvcc.AbortedTS))
	}

	// The observable half: a snapshot taken after the failure must see the
	// pre-transaction state in every store the transaction touched.
	snap := g.BeginRead()
	view := g.ReadAt(snap)
	v, okV := view.GetNodeProperty("n", "v")
	labels := view.NodeLabels("n")
	g.EndRead(snap)

	if !okV {
		t.Fatal("the seeded property vanished")
	}
	if got, _ := v.Int64(); got != 0 {
		t.Fatalf("a snapshot taken after the refused transaction reads v=%d, want the "+
			"pre-transaction 0. Part of a failed transaction is visible — an ATOMICITY "+
			"violation (rmp #2300 AC4).", got)
	}
	for _, l := range labels {
		if l == "Added" {
			t.Fatalf("a snapshot taken after the refused transaction sees its label, "+
				"labels=%v. Part of a failed transaction is visible.", labels)
		}
	}
}

// TestAbort_ASuccessfulTransactionStillPublishes is the negative control for the
// test above: the abort branch must fire ONLY for a transaction that was refused.
// Without this, returning AbortedTS unconditionally would satisfy the first test and
// make every write invisible.
func TestAbort_ASuccessfulTransactionStillPublishes(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var record *mvcc.CommitInfo
	err := g.ApplyVersioned(func(tx WriteTx) error {
		if e := g.Writer(tx).SetNodeProperty("n", "v", Int64Value(7)); e != nil {
			return e
		}
		record = tx.w.tx.OpenRecord()
		return nil
	})
	if err != nil {
		t.Fatalf("bracket: %v", err)
	}
	if record == nil {
		t.Fatal("the transaction allocated no commit record")
	}
	if ts := record.TS(); ts >= mvcc.TxIDBase {
		t.Fatalf("a SUCCESSFUL transaction's record reads %d, which is not a commit "+
			"timestamp (>= mvcc.TxIDBase). The abort branch is firing for a transaction "+
			"that was never refused, so no write would ever become visible.", ts)
	}

	snap := g.BeginRead()
	v, ok := g.ReadAt(snap).GetNodeProperty("n", "v")
	g.EndRead(snap)
	if !ok {
		t.Fatal("the property vanished")
	}
	if got, _ := v.Int64(); got != 7 {
		t.Fatalf("a snapshot after a successful transaction reads v=%d, want 7", got)
	}
}

// TestAbort_AnAbortedObjectStaysWritable is the liveness half, and it is the
// regression test for a bug the abort branch itself created.
//
// Marking a refused transaction's record [mvcc.AbortedTS] fixed visibility and broke
// writability: AbortedTS sits above [mvcc.TxIDBase], so the plain
// "conflict = not visible" rule refused EVERY later writer to that object, forever.
// Measured immediately — examples/27_concurrent_txn's writers exhausted a
// nine-attempt retry chain on the first account any transfer aborted on, and
// `make ci` went red on a workload that had been green.
//
// [mvcc.Conflicts] therefore exempts an aborted head. This test drives the whole
// sequence through the substrate: abort on an object, then write it again, twice, and
// read the result back.
func TestAbort_AnAbortedObjectStaysWritable(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Abort on "n".
	_ = g.ApplyVersioned(func(tx WriteTx) error {
		if e := g.Writer(tx).SetNodeProperty("n", "v", Int64Value(1)); e != nil {
			return e
		}
		_ = tx.w.conflictErr("node properties", ^uint64(0))
		return tx.w.err()
	})

	// Two further transactions must both succeed. Two, not one: a single success
	// could come from the aborted version having been displaced rather than
	// exempted, which would leave the SECOND writer facing it again.
	for round, want := range []int64{5, 6} {
		err := g.ApplyVersioned(func(tx WriteTx) error {
			return g.Writer(tx).SetNodeProperty("n", "v", Int64Value(want))
		})
		if err != nil {
			t.Fatalf("write %d after an abort on the same object was REFUSED: %v.\n"+
				"An aborted version heads the chain, and mvcc.AbortedTS sits above "+
				"mvcc.TxIDBase, so the plain not-visible rule refuses every later writer "+
				"and the object is permanently unwritable (rmp #2300).", round+1, err)
		}
	}

	snap := g.BeginRead()
	v, ok := g.ReadAt(snap).GetNodeProperty("n", "v")
	g.EndRead(snap)
	if !ok {
		t.Fatal("the property vanished")
	}
	if got, _ := v.Int64(); got != 6 {
		t.Fatalf("v = %d after two writes following an abort, want 6", got)
	}
}

// TestAbort_VersionsAreWithdrawnAtAbort is the INVERSION this test asked for.
//
// It used to pin the gap: a version whose commit record reads [mvcc.AbortedTS] can
// never satisfy a reclaimer's `at() <= watermark` test, because AbortedTS sits above
// [mvcc.TxIDBase], so an aborted transaction's versions were retained for the life of
// the process. Its failure message said "invert this test and close #2318 rather than
// restoring the old behaviour", and rmp #2318 did exactly that.
//
// The withdrawal is SYNCHRONOUS, at abort, so the assertion is on what abort itself
// leaves behind rather than on what a later sweep can reach; see
// [Graph.withdrawAbortedNow] for why a present-time read leaves no correct
// asynchronous option.
func TestAbort_VersionsAreWithdrawnAtAbort(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()
	if err := g.SetNodeProperty("n", "v", Int64Value(0)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = g.ReclaimNow()
	base := g.VersionCount()

	_ = g.ApplyVersioned(func(tx WriteTx) error {
		if e := g.Writer(tx).SetNodeProperty("n", "v", Int64Value(1)); e != nil {
			return e
		}
		_ = tx.w.conflictErr("node properties", ^uint64(0))
		return tx.w.err()
	})

	if left := g.VersionCount() - base; left != 0 {
		t.Errorf("the aborted transaction left %d version record(s) behind, want 0", left)
	}
	// And the write it was refused for is not visible, which is what makes the
	// withdrawal a withdrawal rather than a deletion of the mask.
	v, ok := g.GetNodeProperty("n", "v")
	if !ok {
		t.Fatal("the property vanished with the aborted transaction's version")
	}
	if got, _ := v.Int64(); got != 0 {
		t.Errorf("v = %d after an aborted write, want the pre-transaction 0", got)
	}
}

// adjAbortState renders the adjacency over keys as both readers see it: every
// node's present out-neighbours with their weights, its present in-neighbours
// from the reverse index, the edge count, and every ordered pair's existence in a
// fresh snapshot.
func adjAbortState(g *Graph[string, float64], keys ...string) string {
	var sb strings.Builder
	a := g.AdjList()
	snap := g.BeginRead()
	defer g.EndRead(snap)
	fmt.Fprintf(&sb, "size=%d\n", a.Size())
	for _, k := range keys {
		var out []string
		for dst, w := range a.Neighbours(k) {
			out = append(out, fmt.Sprintf("%s:%g", dst, w))
		}
		slices.Sort(out)
		var in []uint64
		if id, ok := a.Mapper().Lookup(k); ok {
			for _, src := range a.InNeighbourIDs(id) {
				in = append(in, uint64(src))
			}
		}
		slices.Sort(in)
		fmt.Fprintf(&sb, "%s out=%v in=%v snap=", k, out, in)
		for _, d := range keys {
			fmt.Fprintf(&sb, "%t,", g.HasEdgeAsOf(k, d, snap))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestAbort_AdjacencyWritesAreWithdrawn is rmp #2965: a bracket that changes the
// adjacency and is then doomed by a conflict on ANOTHER store aborted without
// withdrawing its adjacency writes. The node-property store withdrew its version,
// but the adjacency kept the bracket's entry as the stored value: a removed edge
// stayed removed, an added edge stayed present, for every present-time reader and
// for the next write, which built on it. A bracket with no undo log —
// [Graph.ApplyVersioned] as the durable store's apply runs it, or
// [Graph.ApplyAtomicallyTx] — has nothing else to put the entry back.
//
// After the abort the adjacency must be exactly as before, in the forward
// entries, the reverse index, the edge count and a fresh snapshot; and a later
// committed write on the same entries must land on the pre-image, which the
// reference graph — the same writes without the aborted bracket — pins.
func TestAbort_AdjacencyWritesAreWithdrawn(t *testing.T) {
	brackets := map[string]func(*Graph[string, float64], func(WriteTx) error) error{
		"ApplyVersioned":    (*Graph[string, float64]).ApplyVersioned,
		"ApplyAtomicallyTx": (*Graph[string, float64]).ApplyAtomicallyTx,
	}
	ops := map[string]func(*testing.T, WriteView[string, float64]){
		"add": func(t *testing.T, wv WriteView[string, float64]) {
			if err := wv.AddEdge("a", "c", 3); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		},
		"remove": func(t *testing.T, wv WriteView[string, float64]) {
			if !wv.RemoveEdge("a", "b") {
				t.Fatal("RemoveEdge was refused before the bracket was doomed")
			}
		},
	}
	keys := []string{"a", "b", "c", "d"}
	seed := func(t *testing.T, directed bool) *Graph[string, float64] {
		g := New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
		t.Cleanup(func() { _ = g.Close() })
		for _, err := range []error{
			g.AddEdge("a", "b", 1), g.AddEdge("b", "c", 2), g.AddNode("d"),
			g.SetNodeProperty("x", "v", Int64Value(0)),
		} {
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		return g
	}
	later := func(t *testing.T, g *Graph[string, float64]) {
		if err := g.ApplyVersioned(func(tx WriteTx) error {
			_ = g.Writer(tx).AddEdge("a", "d", 4)
			return tx.Err()
		}); err != nil {
			t.Fatalf("later write: %v", err)
		}
	}
	for _, directed := range []bool{true, false} {
		for bn, bracket := range brackets {
			for on, op := range ops {
				t.Run(fmt.Sprintf("directed=%v/%s/%s", directed, bn, on), func(t *testing.T) {
					g := seed(t, directed)
					before := adjAbortState(g, keys...)
					peer := g.BeginVersionedTx()
					if err := g.Writer(peer).SetNodeProperty("x", "v", Int64Value(1)); err != nil {
						t.Fatalf("peer: %v", err)
					}
					err := bracket(g, func(tx WriteTx) error {
						wv := g.Writer(tx)
						op(t, wv)
						_ = wv.SetNodeProperty("x", "v", Int64Value(2)) // dooms the bracket
						return tx.Err()
					})
					g.EndVersionedTx(peer)
					if !errors.Is(err, mvcc.ErrSerializationConflict) {
						t.Errorf("the doomed bracket returned %v, want a serialization conflict", err)
					}
					if got := adjAbortState(g, keys...); got != before {
						t.Errorf("the aborted bracket's adjacency write is still applied\nbefore:\n%safter:\n%s", before, got)
					}
					later(t, g)
					ref := seed(t, directed)
					later(t, ref)
					if got, want := adjAbortState(g, keys...), adjAbortState(ref, keys...); got != want {
						t.Errorf("a later write built on the aborted entry\ngot:\n%swant:\n%s", got, want)
					}
				})
			}
		}
	}
}

// doomForTest dooms tx as a refused write would, so its end aborts it.
func doomForTest(tx WriteTx) { _ = tx.w.conflictErr(mvcc.StoreAdjacency, mvcc.AbortedTS) }

func requireAdjInvariants(t *testing.T, g *Graph[string, float64], what string) {
	t.Helper()
	if err := g.adj.CheckInvariants(); err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

func adjEntryString(g *Graph[string, float64], n string) string {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return "<none>"
	}
	nbs, _, hs := g.adj.LoadEntryH(id)
	if len(nbs) == 0 {
		return "[]"
	}
	return fmt.Sprintf("%v/%v", nbs, hs)
}

func adjNodeID(g *Graph[string, float64], n string) graph.NodeID {
	id, _ := g.adj.Mapper().Lookup(n)
	return id
}

// TestAbort_AdjacencyWithdrawalIsExact is the audit's withdrawal probe (rmp
// #2965): an aborted explicit transaction's appends, removals, self-loop removal
// and an append creating a new source are withdrawn exactly — every entry, the
// reverse index and the edge count — on both graph shapes, also while a pinned
// snapshot forces the restoration onto a cloned slot array, and the next write
// builds on the pre-image.
func TestAbort_AdjacencyWithdrawalIsExact(t *testing.T) {
	for _, directed := range []bool{true, false} {
		for _, pin := range []bool{false, true} {
			g := New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
			t.Cleanup(func() { _ = g.Close() })
			for _, e := range []error{g.AddEdge("a", "b", 1), g.AddEdge("a", "c", 1), g.AddEdge("a", "a", 1), g.AddEdge("b", "c", 1)} {
				if e != nil {
					t.Fatal(e)
				}
			}
			keys := []string{"a", "b", "c", "d"}
			before := map[string]string{}
			for _, n := range keys {
				before[n] = adjEntryString(g, n)
			}
			size0 := g.adj.Size()
			if pin {
				_ = g.adj.PinSnapshot()
			}
			tx := g.BeginVersionedTx()
			wv := g.Writer(tx)
			if err := wv.AddEdge("a", "b", 2); err != nil {
				t.Fatal(err)
			}
			wv.RemoveEdge("a", "c")
			if err := wv.AddEdge("d", "a", 3); err != nil {
				t.Fatal(err)
			}
			wv.RemoveEdge("a", "a")
			doomForTest(tx)
			g.EndVersionedTx(tx)
			for _, n := range keys {
				if got := adjEntryString(g, n); got != before[n] && (before[n] != "<none>" || got != "[]") {
					t.Errorf("directed=%v pin=%v: %s entry %s, want %s", directed, pin, n, got, before[n])
				}
			}
			if g.adj.Size() != size0 {
				t.Errorf("directed=%v pin=%v: edge count %d, want %d", directed, pin, g.adj.Size(), size0)
			}
			requireAdjInvariants(t, g, fmt.Sprintf("directed=%v pin=%v after the abort", directed, pin))
			g.ReclaimNow()
			if err := g.AddEdge("a", "e", 1); err != nil {
				t.Fatal(err)
			}
			if g.adj.HasEdge("d", "a") || !g.adj.HasEdge("a", "c") || !g.adj.HasEdge("a", "a") {
				t.Errorf("directed=%v pin=%v: the aborted change leaked into the next write: %s", directed, pin, adjEntryString(g, "a"))
			}
			requireAdjInvariants(t, g, "after the next write")
		}
	}
}

// TestAbort_EdgeWriteCannotStackOnAnUncommittedEntry is rmp #2966: an explicit
// transaction's edge-property write rebuilt the entry another explicit
// transaction had not committed, so the first's arc was embedded in the
// second's entry. When the first aborted, its arc was committed by the second,
// or left as an aborted stored value that the next direct write committed.
// Every entry rebuilder now claims the source node, so the second write is
// refused and no order leaves the aborted arc behind.
func TestAbort_EdgeWriteCannotStackOnAnUncommittedEntry(t *testing.T) {
	writes := map[string]func(WriteView[string, float64]) error{
		"SetEdgeProperty": func(wv WriteView[string, float64]) error { return wv.SetEdgeProperty("a", "b", "p", Int64Value(1)) },
		"DelEdgeProperty": func(wv WriteView[string, float64]) error { return wv.DelEdgeProperty("a", "b", "k") },
		"SetEdgeLabel":    func(wv WriteView[string, float64]) error { return wv.SetEdgeLabel("a", "b", "T") },
		"RemoveEdgeLabel": func(wv WriteView[string, float64]) error { return wv.RemoveEdgeLabel("a", "b", "L") },
	}
	for wn, write := range writes {
		for _, order := range []string{"T1abort-T2commit", "T1abort-T2abort", "T2abort-T1abort"} {
			t.Run(wn+"/"+order, func(t *testing.T) {
				g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
				t.Cleanup(func() { _ = g.Close() })
				requireNoErr(t, g.AddEdge("a", "b", 1), g.SetEdgeProperty("a", "b", "k", Int64Value(0)), g.SetEdgeLabel("a", "b", "L"))
				t1 := g.BeginVersionedTx()
				requireNoErr(t, g.Writer(t1).AddEdge("a", "c", 1))
				t2 := g.BeginVersionedTx()
				_ = write(g.Writer(t2))
				if t2.Err() == nil {
					t.Errorf("an explicit %s over another transaction's uncommitted entry was admitted", wn)
				}
				switch order {
				case "T1abort-T2commit":
					doomForTest(t1)
					g.EndVersionedTx(t1)
					g.EndVersionedTx(t2)
				case "T1abort-T2abort":
					doomForTest(t1)
					g.EndVersionedTx(t1)
					doomForTest(t2)
					g.EndVersionedTx(t2)
				case "T2abort-T1abort":
					doomForTest(t2)
					g.EndVersionedTx(t2)
					doomForTest(t1)
					g.EndVersionedTx(t1)
				}
				g.ReclaimNow()
				if err := g.SetEdgeProperty("a", "b", "q", Int64Value(2)); err != nil {
					t.Fatalf("a direct write after both ended: %v", err)
				}
				g.ReclaimNow()
				if g.adj.HasEdge("a", "c") {
					t.Errorf("an aborted transaction's append is in the stored adjacency: %s", adjEntryString(g, "a"))
				}
				snap := g.BeginRead()
				committed, _, _ := g.adj.LoadEntryHAt(adjNodeID(g, "a"), adjlist.At{Versioned: true, StartTS: snap.StartTS()})
				g.EndRead(snap)
				if len(committed) != 1 {
					t.Errorf("the committed adjacency of a is %v, want only a->b", committed)
				}
				requireAdjInvariants(t, g, order)
			})
		}
	}
}

// TestAbort_UndoThenAbortDoesNotReinstateTheArc is the audit's CRITICAL
// regression: T1 appends, T2 rebuilds T1's uncommitted entry through an
// edge-property write, T1's undo removes its arc and T1 is then doomed. The
// withdrawal restored the version's prev, which was T2's entry still holding
// T1's arc, so the abort reinstated the arc the undo had removed. T2's write is
// now refused, so the undo and the withdrawal both see T1's own pre-image.
func TestAbort_UndoThenAbortDoesNotReinstateTheArc(t *testing.T) {
	for _, t2commit := range []bool{true, false} {
		g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		t.Cleanup(func() { _ = g.Close() })
		requireNoErr(t, g.AddEdge("a", "b", 1))
		t1 := g.BeginVersionedTx()
		h, err := g.Writer(t1).AddEdgeH("a", "c", 1)
		requireNoErr(t, err)
		t2 := g.BeginVersionedTx()
		_ = g.Writer(t2).SetEdgeProperty("a", "b", "p", Int64Value(1))
		t1.EnterUndo()
		_ = g.Writer(t1).RemoveEdgeByHandle("a", "c", h)
		t1.ExitUndo()
		doomForTest(t1)
		g.EndVersionedTx(t1)
		if g.adj.HasEdge("a", "c") {
			t.Errorf("t2commit=%v: the abort reinstated the arc the undo removed", t2commit)
		}
		if !t2commit {
			doomForTest(t2)
		}
		g.EndVersionedTx(t2)
		g.ReclaimNow()
		snap := g.BeginRead()
		nbs, _, _ := g.adj.LoadEntryHAt(adjNodeID(g, "a"), adjlist.At{Versioned: true, StartTS: snap.StartTS()})
		g.EndRead(snap)
		if len(nbs) != 1 {
			t.Errorf("t2commit=%v: the committed adjacency of a is %v, want only a->b", t2commit, nbs)
		}
		requireAdjInvariants(t, g, "undo then abort")
	}
}

// TestAbort_UndirectedStackingLeavesNoHalfEdge is #2966 on an undirected graph:
// a withdrawal that could restore one endpoint's entry and not the other's left
// half an edge (the audit measured "asym [140 242] 1 vs 0").
func TestAbort_UndirectedStackingLeavesNoHalfEdge(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: false, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	requireNoErr(t, g.AddEdge("a", "b", 1), g.AddNode("c"))
	t1 := g.BeginVersionedTx()
	requireNoErr(t, g.Writer(t1).AddEdge("a", "c", 1))
	t2 := g.BeginVersionedTx()
	_ = g.Writer(t2).SetEdgeProperty("a", "b", "p", Int64Value(1))
	doomForTest(t1)
	g.EndVersionedTx(t1)
	g.EndVersionedTx(t2)
	g.ReclaimNow()
	requireAdjInvariants(t, g, "after the abort")
	if g.adj.HasEdge("a", "c") || g.adj.HasEdge("c", "a") {
		t.Errorf("an aborted undirected edge survives: a=%s c=%s", adjEntryString(g, "a"), adjEntryString(g, "c"))
	}
	if err := g.RemoveEdge("a", "b"); err != nil {
		t.Fatal(err)
	}
	requireAdjInvariants(t, g, "after a later removal")
}

// TestAbort_WithdrawalDoesNotShareTheAbortedBackingArray is audit finding 2: the
// aborted entry extended the pre-image's backing arrays in place, the
// withdrawal re-published the pre-image with its spare capacity, and the next
// append wrote into memory a reader of the aborted entry still held.
func TestAbort_WithdrawalDoesNotShareTheAbortedBackingArray(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	for _, d := range []string{"b", "c", "d"} {
		requireNoErr(t, g.AddEdge("a", d, 1))
	}
	requireNoErr(t, g.AddNode("x"), g.AddNode("y"))
	a := adjNodeID(g, "a")
	tx := g.BeginVersionedTx()
	requireNoErr(t, g.Writer(tx).AddEdge("a", "x", 1))
	held, _ := g.adj.LoadEntry(a)
	before := fmt.Sprint(held)
	doomForTest(tx)
	g.EndVersionedTx(tx)
	requireNoErr(t, g.AddEdge("a", "y", 1))
	if after := fmt.Sprint(held); after != before {
		t.Errorf("an adjacency entry a reader holds changed under it: %s became %s", before, after)
	}
}

// TestAbort_WithdrawalRacesNoReader is finding 2 under the race detector: a
// lock-free reader of the entry while aborts and appends alternate on it.
func TestAbort_WithdrawalRacesNoReader(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	for _, d := range []string{"b", "c", "d"} {
		requireNoErr(t, g.AddEdge("a", d, 1))
	}
	requireNoErr(t, g.AddNode("x"), g.AddNode("y"), g.AddNode("q"))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = g.adj.HasEdge("a", "q")
			}
		}
	}()
	for i := 0; i < 500; i++ {
		tx := g.BeginVersionedTx()
		_ = g.Writer(tx).AddEdge("a", "x", 1)
		doomForTest(tx)
		g.EndVersionedTx(tx)
		_ = g.AddEdge("a", "y", 1)
		_ = g.RemoveEdge("a", "y")
	}
	close(stop)
	<-done
	requireAdjInvariants(t, g, "after the race")
}

// TestAbort_OnlyAdjacencyWritesReachTheAdjacency is audit finding 4's mechanism:
// the abort visits the transaction's own adjacency write set, so a transaction
// that wrote only node properties records none, and one that wrote k entries
// records exactly k.
func TestAbort_OnlyAdjacencyWritesReachTheAdjacency(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: false, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	requireNoErr(t, g.AddEdge("a", "b", 1))
	props := g.BeginVersionedTx()
	requireNoErr(t, g.Writer(props).SetNodeProperty("a", "k", Int64Value(1)))
	if n := len(props.w.tx.AdjacencyWrites()); n != 0 {
		t.Errorf("a property-only transaction recorded %d adjacency writes, want 0", n)
	}
	g.EndVersionedTx(props)
	edge := g.BeginVersionedTx()
	requireNoErr(t, g.Writer(edge).AddEdge("a", "c", 1), g.Writer(edge).AddEdge("a", "d", 1))
	// a, c and d: a's entry once however often it was rebuilt.
	if n := len(edge.w.tx.AdjacencyWrites()); n != 3 {
		t.Errorf("a transaction that wrote three entries recorded %d adjacency writes, want 3", n)
	}
	doomForTest(edge)
	g.EndVersionedTx(edge)
	requireAdjInvariants(t, g, "after the abort")
}

// TestAbort_RefusedDirectAppendAllocatesNoRecord is finding 4's hub shape: a
// direct append whose destination's claim is refused used to have stamped its
// source first, which allocates the commit record, so the refusal had to abort
// a transaction holding one. Both endpoints are now claimed in one step.
func TestAbort_RefusedDirectAppendAllocatesNoRecord(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	requireNoErr(t, g.AddNode("x"), g.AddNode("hub"))
	peer := g.BeginVersionedTx()
	requireNoErr(t, g.Writer(peer).AddEdge("hub", "y", 1))
	w := g.acquireImplicit()
	var h uint64
	_, err := g.appendEdgeInfo("x", "hub", 1, 0, nil, &h, w)
	info, _ := w.tx.Retract()
	g.implicitCtx.Put(w)
	g.EndVersionedTx(peer)
	if err == nil {
		t.Fatal("setup: the append to a node another transaction holds was admitted")
	}
	if info != nil {
		t.Error("a refused direct append allocated a commit record, so its refusal costs an abort")
	}
}

// TestDirect_PanicSettlesTheImplicitTransaction is audit finding 5: a panic in a
// direct write's operation must not leave its implicit transaction in flight,
// where its record would refuse every later write on what it touched.
func TestDirect_PanicSettlesTheImplicitTransaction(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	requireNoErr(t, g.SetNodeProperty("n", "k", Int64Value(0)))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_ = g.direct(func(tx *writeCtx) error {
			if err := g.setNodePropertyInfo("n", "k", Int64Value(1), tx); err != nil {
				return err
			}
			panic("operation panicked")
		})
	}()
	if v, _ := g.GetNodeProperty("n", "k"); v != Int64Value(0) {
		t.Errorf("the panicked write is visible: n.k = %v", v)
	}
	if err := g.SetNodeProperty("n", "k", Int64Value(2)); err != nil {
		t.Errorf("a write after the panic was refused: %v", err)
	}
}

// TestAbort_AdjacencyInvariantsUnderMixedLoad is the audit's mixed stress: direct
// appends, removals, bulk removals and entry rebuilds, explicit transactions
// and ApplyVersioned brackets of which half are doomed, a reader pinning the
// horizon and a reclaimer, on both shapes. After quiescence the stored adjacency
// must equal the latest committed view, and the reverse index and the edge
// count must agree with it. Run under -race in the battery.
func TestAbort_AdjacencyInvariantsUnderMixedLoad(t *testing.T) {
	for _, directed := range []bool{true, false} {
		t.Run(fmt.Sprintf("directed=%v", directed), func(t *testing.T) {
			adjMixedLoad(t, directed, 500*time.Millisecond)
		})
	}
}

func adjMixedLoad(t *testing.T, directed bool, dur time.Duration) {
	g := New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	const nodes = 10
	name := func(i int) string { return fmt.Sprintf("n%d", i) }
	for i := 0; i < nodes; i++ {
		requireNoErr(t, g.AddNode(name(i)))
	}
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	worker := func(seed uint64, f func(r *rand.Rand)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, 7)) //nolint:gosec // a reproducible test workload, not a secret
			for time.Now().Before(deadline) {
				f(r)
			}
		}()
	}
	pick := func(r *rand.Rand) string { return name(r.IntN(nodes)) }
	for w := uint64(0); w < 2; w++ {
		worker(10*w+1, func(r *rand.Rand) { _ = g.AddEdge(pick(r), pick(r), 1) })
		worker(10*w+2, func(r *rand.Rand) { _ = g.RemoveEdge(pick(r), pick(r)) })
		worker(10*w+3, func(r *rand.Rand) { _ = g.RemoveAllEdgesFrom(pick(r)) })
		worker(10*w+4, func(r *rand.Rand) {
			a, b := pick(r), pick(r)
			_ = g.SetEdgeLabel(a, b, "T")
			_ = g.SetEdgeProperty(a, b, "p", Int64Value(r.Int64N(5)))
		})
		worker(10*w+5, func(r *rand.Rand) {
			tx := g.BeginVersionedTx()
			wv := g.Writer(tx)
			_ = wv.AddEdge(pick(r), pick(r), 1)
			wv.RemoveEdge(pick(r), pick(r))
			_ = wv.SetEdgeProperty(pick(r), pick(r), "q", Int64Value(1))
			if r.IntN(3) == 0 {
				wv.RemoveAllEdgesFrom(pick(r))
			}
			time.Sleep(time.Duration(r.IntN(100)) * time.Microsecond)
			if r.IntN(2) == 0 {
				doomForTest(tx)
			}
			g.EndVersionedTx(tx)
		})
		worker(10*w+6, func(r *rand.Rand) {
			_ = g.ApplyVersioned(func(tx WriteTx) error {
				wv := g.Writer(tx)
				_ = wv.AddEdge(pick(r), pick(r), 1)
				wv.RemoveEdge(pick(r), pick(r))
				if r.IntN(2) == 0 {
					doomForTest(tx)
				}
				return tx.Err()
			})
		})
	}
	worker(100, func(r *rand.Rand) {
		s := g.BeginRead()
		time.Sleep(time.Duration(r.IntN(2000)) * time.Microsecond)
		g.EndRead(s)
	})
	worker(101, func(*rand.Rand) { g.ReclaimNow(); time.Sleep(500 * time.Microsecond) })
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(dur + 60*time.Second):
		t.Fatal("the workers did not finish: deadlock or livelock")
	}
	g.ReclaimNow()
	snap := g.BeginRead()
	for i := 0; i < nodes; i++ {
		id := adjNodeID(g, name(i))
		pn, _, ph := g.adj.LoadEntryH(id)
		cn, _, ch := g.adj.LoadEntryHAt(id, adjlist.At{Versioned: true, StartTS: snap.StartTS()})
		if !slices.Equal(pn, cn) || !slices.Equal(ph, ch) {
			t.Errorf("%s: stored %v/%v differs from the latest committed %v/%v", name(i), pn, ph, cn, ch)
		}
	}
	g.EndRead(snap)
	requireAdjInvariants(t, g, "after the mixed load")
}

// BenchmarkAbortCost is one abort of a transaction that wrote one node property
// while a reader holds adjacency history (audit finding 4): it must not scale
// with that history.
func BenchmarkAbortCost(b *testing.B) {
	for _, held := range []int{0, 100000} {
		b.Run(fmt.Sprintf("history=%d", held), func(b *testing.B) {
			g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			defer func() { _ = g.Close() }()
			_ = g.AddNode("n")
			rd := g.BeginRead()
			defer g.EndRead(rd)
			for i := 0; i < held; i++ {
				_ = g.AddEdge(fmt.Sprintf("s%d", i), "n", 1)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tx := g.BeginVersionedTx()
				_ = g.Writer(tx).SetNodeProperty("n", "k", Int64Value(int64(i)))
				doomForTest(tx)
				g.EndVersionedTx(tx)
			}
		})
	}
}
