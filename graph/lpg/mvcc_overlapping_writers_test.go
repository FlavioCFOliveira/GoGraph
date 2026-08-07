package lpg

// mvcc_overlapping_writers_test.go — the properties that only become TESTABLE once
// two write brackets can overlap (rmp #2304, MVCC B2).
//
// Several earlier tasks left a property asserted but not discriminated, with the
// same reason recorded each time: the exclusive visibility barrier admitted one
// write bracket at a time, so the collision the property guards against could not
// be produced. rmp #2300's AC5 and rmp #2303's AC3 both say so in their own files.
//
// [Graph.ApplyVersioned] is what removes that excuse. It holds the schema barrier
// SHARED, so two brackets genuinely overlap, and the tests here drive that overlap
// deterministically — with channels rather than with sleeps, so they are neither
// flaky nor slow.
//
// # The interleaving these tests construct
//
// Every test below builds the same shape, because it is the shape that discriminates:
//
//	A: open bracket ──────────── write ─── commit ────┐
//	B:        open bracket ─────────────────────────── commit
//	                    ↑
//	          B opened SECOND, so B holds the graph's ambient
//	          slot at the moment A performs its write
//
// A resolving its own write through the AMBIENT slot therefore resolves it to B —
// a transaction that is still in flight and whose id no commit record will ever
// publish. That is the failure mode, and it is the one the assertions detect.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// overlap sequences two write brackets into the interleaving drawn in the file
// comment, and returns once both have closed.
//
// bodyA runs with B's bracket already open, which is the condition every test here
// needs. betweenAAndB runs after A has fully committed and while B is STILL OPEN —
// the only window in which "charged to a transaction that has not published" is
// distinguishable from "charged to one that has", and therefore the window that
// makes these tests discriminate rather than merely pass. bodyB runs last and
// normally does nothing. Either callback may be nil.
//
// Every body receives its own [WriteTx], so a test can write through its OWN
// transaction rather than through whichever the graph happens to name — which is
// exactly the distinction under test.
//
// It fails the test rather than returning an error, because a bracket that refuses
// to open is not a measurement.
func overlap[N comparable, W any](t *testing.T, g *Graph[N, W], bodyA func(tx WriteTx), betweenAAndB func(), bodyB func(tx WriteTx)) {
	t.Helper()
	var (
		wg       sync.WaitGroup
		aOpen    = make(chan struct{})
		bOpen    = make(chan struct{})
		aClosed  = make(chan struct{})
		betweenD = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := g.ApplyVersioned(func(tx WriteTx) error {
			close(aOpen)
			// Wait for B to open, so B — not A — is what the ambient slot names
			// while A does its work.
			<-bOpen
			if bodyA != nil {
				bodyA(tx)
			}
			return nil
		})
		// Closed AFTER the bracket returns, so A's publication has happened.
		close(aClosed)
		if err != nil {
			t.Errorf("A's bracket: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-aOpen
		err := g.ApplyVersioned(func(tx WriteTx) error {
			close(bOpen)
			// Stay open until A has committed AND the between-step has observed the
			// graph, so B is provably still in flight throughout both.
			<-aClosed
			<-betweenD
			if bodyB != nil {
				bodyB(tx)
			}
			return nil
		})
		if err != nil {
			t.Errorf("B's bracket: %v", err)
		}
	}()

	<-aClosed
	if betweenAAndB != nil {
		betweenAAndB()
	}
	close(betweenD)
	wg.Wait()
}

// TestOverlap_DeferredIndexRemovalChargedToItsOwnTransaction is rmp #2303's AC3,
// discharged: the carried acceptance criterion that could not be satisfied while
// the barrier existed (see mvcc_index_ordering_test.go, which records why the test
// there is a guard rather than a discriminator).
//
// A removes a label while B holds the ambient slot. The deferral must be charged to
// A's instant. Charged to B instead, B's transaction id is above every published
// commit timestamp, so `st.at() <= watermark` can never hold and the removal is
// NEVER swept — the label bitmap over-reports for the life of the process, which is
// the second of the two failure directions the criterion names.
//
// Unlike its predecessor this test DISCRIMINATES: reverting
// [Graph.deferralStamp] to read the ambient stamp makes it fail, because inside an
// ApplyVersioned bracket the ambient slot is occupied by a live transaction rather
// than empty.
func TestOverlap_DeferredIndexRemovalChargedToItsOwnTransaction(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id := nodeIDOf(t, g, "a")
	lid := uint32(g.reg.Intern("L"))
	if !g.nodeIdx.Has(lid, id) {
		t.Fatal("the label bitmap does not carry the node it was just given a label for")
	}

	var (
		backlogDuringA int64
		swept          int
		stillIndexed   bool
	)
	overlap(t, g,
		func(tx WriteTx) {
			// Through A's OWN transaction. The public RemoveNodeLabel passes a nil
			// transaction and would resolve through the ambient slot, which is the
			// behaviour rmp #2306 must still fix for the engine path; this test is
			// about whether a write that DOES carry its transaction is charged to it.
			g.removeNodeLabelInfo("a", "L", tx.w)
			backlogDuringA = g.IndexRemovalBacklog()
		},
		func() {
			// THE DISCRIMINATING WINDOW: A has published, B has not. The clock's read
			// timestamp is therefore at or past A's commit and cannot have reached B,
			// which has not allocated a commit timestamp at all yet — endWrite mints it
			// on the unwind.
			//
			// Sweeping here separates the two answers. Charged to A, the deferral's
			// instant is A's published commit timestamp and the sweep applies it.
			// Charged to B, its instant is B's TRANSACTION id, which lives above
			// mvcc.TxIDBase and above every commit timestamp, so `st.at() <= watermark`
			// cannot hold and the removal is not swept — which, sustained past B's own
			// commit, is the "never swept" direction of the acceptance criterion.
			//
			// Doing this AFTER both had committed is what made the first draft of this
			// test useless: by then B had published too, so a deferral charged to B was
			// swept anyway and the assertion held against the defective build. Verified
			// by reverting Graph.deferralStamp to the ambient read, which now fails here
			// and did not before.
			swept = g.applyDeferredIndexRemovals(g.mvccClock.ReadTS())
			stillIndexed = g.nodeIdx.Has(lid, id)
		},
		nil,
	)

	if backlogDuringA != 1 {
		t.Fatalf("IndexRemovalBacklog during A = %d, want 1 — the removal was not deferred, "+
			"so this test cannot observe whose instant it carries", backlogDuringA)
	}
	if swept != 1 {
		t.Fatalf("the sweep applied %d removals at a watermark past A and not past B, want 1 — "+
			"A's deferral is waiting on an instant that is not A's, which is what charging it to "+
			"a still-in-flight concurrent writer looks like", swept)
	}
	if stillIndexed {
		t.Fatal("the entry is still in the label bitmap after being swept, so a scan over-reports " +
			"a label the node no longer has")
	}
}

// TestOverlap_TransactionsDoNotStealEachOthersState is the guard on what
// [Graph.beginWrite] returns and [Graph.endWrite] consumes.
//
// Two overlapping brackets each write a property. Both writes must become visible,
// and each must carry its OWN transaction's commit timestamp. The failure this
// detects is the one [mvcc.WriteStamp.EndFor] describes: closing the transaction the
// graph's SLOT names rather than the caller's own, which makes A publish B's record
// and leaves B's versions stamped with an id nothing will ever publish — invisible
// to every reader and unreclaimable by every reclaimer, with -race silent on it
// because every field involved is atomic.
func TestOverlap_TransactionsDoNotStealEachOthersState(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %q: %v", n, err)
		}
	}

	overlap(t, g,
		func(tx WriteTx) {
			if err := g.setNodePropertyInfo("a", "v", Int64Value(1), tx.w); err != nil {
				t.Errorf("A's write: %v", err)
			}
		},
		nil,
		func(tx WriteTx) {
			if err := g.setNodePropertyInfo("b", "v", Int64Value(2), tx.w); err != nil {
				t.Errorf("B's write: %v", err)
			}
		},
	)

	// A reader starting now is after both commits and must see both writes. A
	// version whose transaction was never published is invisible to every snapshot,
	// so a stolen record shows up here as a missing value.
	snap := g.BeginRead()
	defer g.EndRead(snap)
	rv := g.ReadAt(snap)
	for _, want := range []struct {
		node string
		v    int64
	}{{"a", 1}, {"b", 2}} {
		got, ok := rv.GetNodeProperty(want.node, "v")
		if !ok {
			t.Errorf("node %q has no property v after its transaction committed; its versions "+
				"carry a transaction id no commit record published, which is what one bracket "+
				"closing another's window produces", want.node)
			continue
		}
		iv, _ := got.Int64()
		if iv != want.v {
			t.Errorf("node %q v = %d, want %d", want.node, iv, want.v)
		}
	}
}

// TestOverlap_WriterViewOfSeesOwnWriteNotTheOthers pins read-your-own-writes under
// overlap, which is the property [Graph.WriterViewOf] exists for.
//
// A must see its own uncommitted write and must NOT see B's, and the only way to
// get that right is to resolve the snapshot from the transaction the caller HOLDS.
// Resolving it from the graph's slot — which is what [Graph.WriterView] does, and
// what the whole write path did before rmp #2304 — hands A the snapshot of whichever
// bracket opened last, so A sees neither its own work nor a consistent graph. That
// is the same mistake rmp #2301 made one level down, where reading the writer's
// identity off the graph produced a FALSE conflict between goroutines writing
// disjoint nodes.
func TestOverlap_WriterViewOfSeesOwnWriteNotTheOthers(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %q: %v", n, err)
		}
	}

	var (
		sawOwn   bool
		sawOther bool
	)
	// B writes FIRST — inside its own bracket, before A reads — so that by the time
	// A looks there is something of B's to be wrongly visible. The overlap helper
	// runs bodyA while B's bracket is open, so B's write has to happen at bracket
	// open rather than in bodyB.
	var wg sync.WaitGroup
	var (
		aOpen   = make(chan struct{})
		bWrote  = make(chan struct{})
		aClosed = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := g.ApplyVersioned(func(tx WriteTx) error {
			close(aOpen)
			<-bWrote
			if err := g.setNodePropertyInfo("a", "v", Int64Value(1), tx.w); err != nil {
				return err
			}
			rv := g.WriterViewOf(tx)
			_, sawOwn = rv.GetNodeProperty("a", "v")
			_, sawOther = rv.GetNodeProperty("b", "v")
			return nil
		}); err != nil {
			t.Errorf("A's bracket: %v", err)
		}
		close(aClosed)
	}()
	go func() {
		defer wg.Done()
		<-aOpen
		if err := g.ApplyVersioned(func(tx WriteTx) error {
			if err := g.setNodePropertyInfo("b", "v", Int64Value(2), tx.w); err != nil {
				return err
			}
			close(bWrote)
			<-aClosed
			return nil
		}); err != nil {
			t.Errorf("B's bracket: %v", err)
		}
	}()
	wg.Wait()

	if !sawOwn {
		t.Error("A could not see its OWN uncommitted write through WriterViewOf; the write path " +
			"applies eagerly and a later clause of the same statement must observe an earlier one")
	}
	if sawOther {
		t.Error("A saw B's UNCOMMITTED write through WriterViewOf — a dirty read. B may still " +
			"roll back, in which case A has matched a value that never existed")
	}
}
