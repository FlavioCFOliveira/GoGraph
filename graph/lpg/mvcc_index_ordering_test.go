package lpg

// mvcc_index_ordering_test.go — the deferred label-index removal's ordering basis
// (rmp #2303, MVCC B1, audit finding E15).
//
// # The dependency that was there
//
// A label removal is DEFERRED: recorded when it happens, applied to the bitmap
// only once the reclamation watermark has passed it, because a reader older than
// the removal must still find the node in the bitmap or it silently loses a row
// (see mvcc_index.go).
//
// "Passed it" needs an instant, and [Graph.deferLabelIndexRemoval] took that
// instant from the graph's AMBIENT [mvcc.WriteStamp] slot, which names whichever
// transaction currently holds the write visibility barrier. With one writer that
// is always the removing transaction, so it was correct. With concurrent writers
// it is wrong in both directions:
//
//   - stamped with a transaction that commits EARLIER, the removal is swept before
//     the removing transaction's own readers are finished with the entry — a reader
//     silently loses a row, which is the exact failure the deferral exists to
//     prevent;
//   - stamped with a transaction still in flight, it carries an id no record will
//     ever publish, so `st.at() <= watermark` can never hold and the removal is
//     NEVER swept — the bitmap over-reports for the life of the process.
//
// Same defect class as audit finding E3, which rmp #2301 closed for the commit
// record and the version count. The deferred index removal was the last reader of
// the ambient slot on this path.
//
// # The fix, and the honest limit on what can be tested today
//
// The instant now comes from the removing transaction's own [writeCtx]. A nil tx —
// a direct Go-API write outside any transaction — still falls back to the ambient
// stamp, which is correct for it: such a write commits the instant it is made.
//
// AC3 ASKED FOR A TEST VERIFIED TO FAIL AGAINST THE UNORDERED BUILD, AND WHEN THIS
// FILE WAS WRITTEN THAT WAS NOT POSSIBLE. It is now: rmp #2320 removed the exclusive
// barrier from the ordinary write path, so two write brackets overlap and the
// collision is producible. mvcc_index_overlap_test.go produces it deterministically
// and both of its tests FAIL against `g.stamp.Stamp()`, naming the concurrent
// writer's record (rmp #2304 AC8). The paragraphs below are kept because finding out
// why the tests HERE could not discriminate is what corrected the description of the
// defect:
//
//   - on the BARRIER path ([Graph.beginWrite]) the ambient slot IS occupied, so the
//     old read returned the ambient transaction's record. That is the wrong record
//     once writers overlap — but the barrier is exactly what guarantees the slot has
//     one occupant, so no test can produce the collision until rmp #2304 removes it.
//     Same situation as rmp #2300's AC5.
//   - on the labelTx path used by these tests, [Graph.beginWriteCtx] does NOT publish
//     to the ambient slot, so the old read fell through to Stamp's untransacted
//     branch: it allocated AND IMMEDIATELY PUBLISHED a fresh commit timestamp per
//     deferred removal. That is a different defect and it IS observable — the removal
//     became reclaimable at an instant BEFORE its own transaction committed, and it
//     inflated the untracked-write counter once per deferral.
//
// TestDeferredIndexRemoval_ChargesNoUntrackedWrite pins that second one, which was
// the part of this change a test could discriminate at the time. The first is now
// pinned by mvcc_index_overlap_test.go.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestDeferredIndexRemoval_IsStampedWithItsOwnTransaction is the discriminating
// test: TWO transactions are open at once, and the one that removes a label must
// have its deferral charged to ITSELF, not to whichever transaction the graph's
// ambient slot happens to name.
//
// It is built so the two instants differ observably. B opens second, so it holds
// the ambient slot while A performs the removal. If the deferral were charged to
// B, sweeping at a watermark that has passed A but not B would leave the entry in
// the bitmap; charged to A, the sweep removes it.
//
// It passes against the previous behaviour too, for the reason given in the file
// comment above; it is a guard on the property rmp #2304 depends on, not a
// discriminator.
func TestDeferredIndexRemoval_IsStampedWithItsOwnTransaction(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id := nodeIDOf(t, g, "a")
	lid := g.reg.Intern("L")
	if !g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("the label bitmap does not carry the node it was just given a label for")
	}

	// A removes the label. B is opened AFTER A so that, under the old behaviour,
	// the ambient slot names B rather than A at the moment A defers.
	txA := g.beginLabelTx()
	txB := g.beginLabelTx()
	txA.removeNodeLabel("a", "L")

	if got := g.IndexRemovalBacklog(); got != 1 {
		t.Fatalf("IndexRemovalBacklog = %d, want 1 — the removal was not deferred at all, "+
			"so this test cannot observe whose instant it carries", got)
	}
	// Still in the bitmap: that is the whole point of deferring.
	if !g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("the entry left the bitmap immediately; a reader older than the removal " +
			"would now silently lose a row")
	}

	tsA := mustCommit(t, txA)

	// Sweep at a watermark that has passed A and NOT B. B is still in flight, so
	// its id is above any published timestamp; A's commit is at tsA.
	if n := g.applyDeferredIndexRemovals(tsA); n != 1 {
		t.Fatalf("the sweep applied %d removals at a watermark past A's commit, want 1 — "+
			"A's deferral is not charged to A, so it is waiting on a transaction that is "+
			"not the one that made it", n)
	}
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("the entry is still in the bitmap after being swept")
	}

	// B is untouched by any of this and must still commit cleanly: the deferral
	// must not have attached itself to B's fate in either direction.
	if _, err := txB.commit(); err != nil {
		t.Fatalf("B was refused although it wrote nothing: %v", err)
	}
}

// TestDeferredIndexRemoval_UntransactedWriteStillSweeps pins the nil-tx fallback.
//
// A direct Go-API removal has no transaction, so it falls back to the ambient
// stamp. That is correct — the write is committed the instant it is made — but it
// has to keep working, because the public mutators are a supported entry point and
// a deferral that never swept would leak an over-reporting bitmap entry for the
// life of the process.
func TestDeferredIndexRemoval_UntransactedWriteStillSweeps(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "L"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	id := nodeIDOf(t, g, "a")
	lid := g.reg.Intern("L")

	// No transaction: the public mutator.
	g.RemoveNodeLabel("a", "L")

	// Either it was removed immediately (versioning disarmed) or deferred and then
	// swept. Both are correct; what is not correct is remaining in the bitmap with
	// nothing pending, which would be a leak.
	g.ReclaimNow()
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatalf("the entry survives in the bitmap with a backlog of %d after an "+
			"untransacted removal and a sweep: the deferral is charged to an instant no "+
			"record will publish, so it can never be applied", g.IndexRemovalBacklog())
	}
}

// TestDeferredIndexRemoval_ChargesNoUntrackedWrite is the discriminating half.
//
// Before this change, a deferred removal inside a transaction fell through to
// [mvcc.WriteStamp.Stamp]'s untransacted branch, because beginWriteCtx does not
// publish to the ambient slot. That branch allocates a commit timestamp AND
// PUBLISHES IT IMMEDIATELY, then counts the write as untracked.
//
// Two consequences, and the counter is the observable one: the removal became
// reclaimable at an instant BEFORE its own transaction had committed — or aborted —
// and every deferral was accounted as a write outside any transaction, which is the
// figure the substrate reports for exactly the opposite thing.
//
// Verified: with `g.stamp.Stamp()` restored in deferLabelIndexRemoval, this test
// fails with one untracked write per deferred removal.
func TestDeferredIndexRemoval_ChargesNoUntrackedWrite(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
		if err := g.SetNodeLabel(n, "L"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", n, err)
		}
	}
	// Drain whatever the untransacted set-up accounted for, so the count below is
	// attributable to the deferrals alone.
	g.stamp.TakeUntracked()

	tx := g.beginLabelTx()
	for _, n := range []string{"a", "b", "c"} {
		tx.removeNodeLabel(n, "L")
	}
	if got := g.IndexRemovalBacklog(); got != 3 {
		t.Fatalf("IndexRemovalBacklog = %d, want 3 — the removals were not deferred, so "+
			"this test cannot observe what they were charged to", got)
	}
	if got := g.stamp.TakeUntracked(); got != 0 {
		t.Errorf("%d untracked writes were charged by %d deferred removals inside a "+
			"transaction: each took a commit timestamp of its own and published it, so the "+
			"removal is reclaimable before its own transaction has committed or aborted",
			got, 3)
	}
	if _, err := tx.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
