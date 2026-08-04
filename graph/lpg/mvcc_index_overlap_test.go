package lpg

// mvcc_index_overlap_test.go — rmp #2304 AC8, carried from rmp #2303 (MVCC B1).
//
// # The test #2303 could not write, and why it can be written now
//
// A label removal is DEFERRED: applied to the label bitmap only once the
// reclamation watermark has passed it, because a reader older than the removal must
// still find the node there or it silently loses a row. "Passed it" needs an
// instant, and until commit a00cfae8 that instant came from the graph's AMBIENT
// [mvcc.WriteStamp] slot, which names whichever transaction most recently opened a
// write bracket.
//
// rmp #2303's AC3 asked for a test verified to FAIL against the unordered build and
// could not deliver one. Its own note records why: on the barrier path the slot IS
// occupied, so the ambient read returned a transaction's record — the WRONG
// transaction's once writers overlap, but the exclusive barrier is precisely what
// guaranteed the slot had one occupant, so the collision was unproducible. Same
// situation as rmp #2300's AC5.
//
// rmp #2320 removed the barrier from the ordinary write path, so two brackets can
// now be open at once and the collision is producible. This file produces it
// deterministically, in the one order that matters: writer A opens its bracket,
// writer B opens ITS bracket (so the slot names B), and only THEN does A defer its
// removal. Against the ambient build A's removal is charged to B.
//
// # Both failure directions, and how each is observed
//
// Charging the removal to the wrong transaction fails in two opposite ways, and the
// two tests below cover one each:
//
//   - to a transaction that commits EARLIER, the removal is swept before the
//     removing transaction's own readers are done with the entry, and a reader
//     silently loses a row — the exact failure the deferral exists to prevent;
//   - to a transaction still IN FLIGHT, it carries an id no record will ever
//     publish, so `at() <= watermark` can never hold, the removal is NEVER swept,
//     and the label bitmap over-reports for the life of the process.
//
// Both are observed on the pending entry's own record rather than on a downstream
// symptom, because the record IS the ordering basis: an assertion on the sweep
// alone would pass or fail for timing reasons that have nothing to do with whose
// instant was recorded.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestDeferredIndexRemoval_IsChargedToTheRemovingTransaction_NotAConcurrentOne is
// rmp #2304 AC8's first direction: the pending removal must carry the REMOVING
// transaction's record, not the record of a writer that merely published to the
// ambient slot later.
//
// It fails against a build whose deferral reads the ambient slot: with B's bracket
// open, the entry carries B's record, so the removal becomes reclaimable at B's
// commit instant — which may precede A's — and a reader that still needs the entry
// loses a row.
func TestDeferredIndexRemoval_IsChargedToTheRemovingTransaction_NotAConcurrentOne(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("n", "L"); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	id, ok := g.AdjList().Mapper().Lookup("n")
	if !ok {
		t.Fatal("seeded node not interned")
	}
	lid := g.reg.Intern("L")

	var (
		aOpen      = make(chan struct{})
		bPublished = make(chan struct{})
		bRelease   = make(chan struct{})
		bDone      = make(chan struct{})
		aRecord    *mvcc.CommitInfo
		bRecord    *mvcc.CommitInfo
	)

	// Writer B: opens a bracket AFTER A has opened its own, so the ambient slot
	// names B while A is still writing, and holds it open. It writes one property so
	// it HAS a record — an ambient read must have something wrong to find, or the
	// test could pass for want of a candidate rather than for correctness.
	go func() {
		defer close(bDone)
		<-aOpen
		_ = g.ApplyVersioned(func(tx WriteTx) error {
			if err := g.Writer(tx).SetNodeProperty("other", "v", Int64Value(1)); err != nil {
				return err
			}
			bRecord = tx.w.tx.OpenRecord()
			close(bPublished)
			<-bRelease
			return nil
		})
	}()

	err := g.ApplyVersioned(func(tx WriteTx) error {
		close(aOpen)
		<-bPublished // the ambient slot now names B
		// The removal that defers the index entry.
		g.Writer(tx).RemoveNodeLabel("n", "L")
		aRecord = tx.w.tx.OpenRecord()
		return nil
	})
	if err != nil {
		close(bRelease)
		<-bDone
		t.Fatalf("writer A: %v", err)
	}

	got := pendingRemovalRecord(t, g, uint32(lid), id)

	close(bRelease)
	<-bDone

	if aRecord == nil {
		t.Fatal("writer A allocated no commit record; the removal recorded no version")
	}
	if bRecord == nil {
		t.Fatal("writer B allocated no commit record; an ambient read would have had " +
			"nothing wrong to find and this test would prove nothing")
	}
	if aRecord == bRecord {
		t.Fatal("the two writers share one commit record; the brackets did not overlap " +
			"as this test requires")
	}
	if got != aRecord {
		which := "a third record"
		if got == bRecord {
			which = "writer B's record — the CONCURRENT writer's"
		} else if got == nil {
			which = "no record at all"
		}
		t.Fatalf("the deferred label-index removal is charged to %s instead of to the "+
			"transaction that made it. It then becomes reclaimable at that transaction's "+
			"instant rather than its own: swept too early, a reader that still needs the "+
			"entry silently loses a row (rmp #2303 AC3 / rmp #2304 AC8).", which)
	}
}

// TestDeferredIndexRemoval_MisChargingWouldMoveTheSweepInstantBothWays is AC8's
// second half: it shows that charging the removal to the concurrent writer is a real
// MIS-ORDERING in both directions, not a harmless alias.
//
// The AC states the second direction as "charged to one still in flight, it carries
// an id no record will ever publish and is NEVER swept". That is NOT what the
// ambient read would actually produce here, and the correction is worth recording:
// both the threaded and the ambient resolution store a *[mvcc.CommitInfo], and
// [lifeStamp.at] resolves through it, so a removal charged to a live writer is swept
// at THAT writer's commit instant rather than stranded. Stranding needs
// [lifeStamp.info] to be nil with an in-flight id in [lifeStamp.ts], which
// [Graph.deferralStamp] produces only if the transaction's window has already been
// retracted — unreachable while its bracket is open.
//
// So the harm is mis-ORDERING, and this test proves the two instants are genuinely
// different and orderable EITHER WAY: with the concurrent writer committing first
// the removal would be swept too EARLY (before the removing transaction's own
// readers are done with the entry, which silently loses a row), and with it
// committing second, too LATE (the bitmap over-reports until that unrelated writer
// happens to finish). Run in both orders, because a test that only ever produced one
// order would leave the other direction asserted by argument rather than measured.
func TestDeferredIndexRemoval_MisChargingWouldMoveTheSweepInstantBothWays(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bFirst   bool
		wantSign string
	}{
		{name: "concurrent_writer_commits_first", bFirst: true, wantSign: "earlier"},
		{name: "concurrent_writer_commits_second", bFirst: false, wantSign: "later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New[string, float64](adjlist.Config{Directed: true})
			if err := g.SetNodeLabel("n", "L"); err != nil {
				t.Fatalf("seed label: %v", err)
			}
			id, ok := g.AdjList().Mapper().Lookup("n")
			if !ok {
				t.Fatal("seeded node not interned")
			}
			lid := uint32(g.reg.Intern("L"))

			var (
				aOpen      = make(chan struct{})
				bPublished = make(chan struct{})
				bRelease   = make(chan struct{})
				bDone      = make(chan struct{})
				aRecord    *mvcc.CommitInfo
				bRecord    *mvcc.CommitInfo
			)
			go func() {
				defer close(bDone)
				<-aOpen
				_ = g.ApplyVersioned(func(tx WriteTx) error {
					if err := g.Writer(tx).SetNodeProperty("other", "v", Int64Value(1)); err != nil {
						return err
					}
					bRecord = tx.w.tx.OpenRecord()
					close(bPublished)
					<-bRelease
					return nil
				})
			}()

			aDeferred := make(chan struct{})
			aDone := make(chan struct{})
			var aErr error
			go func() {
				defer close(aDone)
				aErr = g.ApplyVersioned(func(tx WriteTx) error {
					close(aOpen)
					<-bPublished // the ambient slot names B from here on
					g.Writer(tx).RemoveNodeLabel("n", "L")
					aRecord = tx.w.tx.OpenRecord()
					close(aDeferred)
					if tc.bFirst {
						// Let B commit before A does.
						close(bRelease)
						<-bDone
					}
					return nil
				})
			}()
			<-aDeferred
			charged := pendingRemovalRecord(t, g, lid, id)
			<-aDone
			if !tc.bFirst {
				close(bRelease)
				<-bDone
			}
			if aErr != nil {
				t.Fatalf("writer A: %v", aErr)
			}

			if aRecord == nil || bRecord == nil || aRecord == bRecord {
				t.Fatalf("the two writers did not overlap with distinct records "+
					"(a=%p b=%p); this test would prove nothing", aRecord, bRecord)
			}
			if charged != aRecord {
				t.Fatalf("the removal is charged to %s instead of to the transaction that "+
					"made it", recordName(charged, aRecord, bRecord))
			}

			// Both have committed now, so both instants are real timestamps and the
			// mis-charge is quantifiable rather than argued.
			aAt, bAt := aRecord.TS(), bRecord.TS()
			if aAt >= mvcc.TxIDBase || bAt >= mvcc.TxIDBase {
				t.Fatalf("a record is still in flight (a=%d b=%d); both writers must have "+
					"committed before the instants can be compared", aAt, bAt)
			}
			switch tc.wantSign {
			case "earlier":
				if bAt >= aAt {
					t.Fatalf("the concurrent writer was supposed to commit FIRST but its "+
						"instant %d is not before the removing transaction's %d; this arm "+
						"is not exercising the swept-too-early direction", bAt, aAt)
				}
			case "later":
				if bAt <= aAt {
					t.Fatalf("the concurrent writer was supposed to commit SECOND but its "+
						"instant %d is not after the removing transaction's %d; this arm "+
						"is not exercising the swept-too-late direction", bAt, aAt)
				}
			}
			t.Logf("removal charged to its own transaction at %d; the concurrent writer's "+
				"instant is %d, so a mis-charge would have moved the sweep %s", aAt, bAt, tc.wantSign)
		})
	}
}

// recordName describes which of the two known records got is, for a failure message
// that names the defect instead of printing a pointer.
func recordName(got, a, b *mvcc.CommitInfo) string {
	switch got {
	case nil:
		return "no record at all"
	case a:
		return "the removing transaction's record"
	case b:
		return "the CONCURRENT writer's record"
	default:
		return "a third, unknown record"
	}
}

// pendingRemovalRecord returns the commit record the pending deferred removal of
// (lid, id) carries, failing the test when there is no pending removal.
//
// It reads g.idxDeferred directly, which is why these tests live in package lpg:
// the record IS the ordering basis, and asserting on it is what makes the failure
// message name the defect rather than a downstream symptom.
func pendingRemovalRecord(t *testing.T, g *Graph[string, float64], lid uint32, id graph.NodeID) *mvcc.CommitInfo {
	t.Helper()
	g.idxDeferred.mu.Lock()
	defer g.idxDeferred.mu.Unlock()
	st, ok := g.idxDeferred.pending[idxEntry{id: id, lid: lid}]
	if !ok {
		t.Fatal("no deferred index removal is pending; the label removal did not defer " +
			"one, so this test would prove nothing")
	}
	return st.info
}
