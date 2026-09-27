package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// snapshot_verdict_test.go — rmp #2888: the snapshot pins the verdict of a record
// it first classifies IN FLIGHT, and only that. A committed or aborted record's
// stamp is final, so pinning it changed no answer and only grew the memo — once
// per distinct transaction a read touched.

// TestSnapshotVisible_PinsOnlyInFlightRecords drives the three states of a
// commit record through one snapshot and asserts both the verdicts and what the
// memo holds.
func TestSnapshotVisible_PinsOnlyInFlightRecords(t *testing.T) {
	const startTS, ownTx = 100, mvcc.TxIDBase + 7
	s := &Snapshot{startTS: startTS, txID: ownTx}

	older := mvcc.NewCommittedInfo(startTS - 1)
	newer := mvcc.NewCommittedInfo(startTS + 1)
	aborted := mvcc.NewCommitInfo(mvcc.TxIDBase + 3)
	aborted.Abort()
	for _, c := range []struct {
		name string
		info *commitInfo
		want bool
	}{
		{"committed at or below startTS", older, true},
		{"committed above startTS", newer, false},
		{"aborted", aborted, false},
	} {
		for range 3 {
			if got := s.visible(c.info, 0, startTS, ownTx); got != c.want {
				t.Fatalf("%s: visible = %v, want %v", c.name, got, c.want)
			}
		}
	}
	if n := len(s.verdict); n != 0 {
		t.Fatalf("memo holds %d terminal records, want 0: a final stamp needs no pin", n)
	}

	// Another transaction's record, first seen in flight: invisible, pinned, and
	// STILL invisible once it commits — the rule the memo exists for.
	other := mvcc.NewCommitInfo(mvcc.TxIDBase + 9)
	if s.visible(other, 0, startTS, ownTx) {
		t.Fatal("another transaction's in-flight record is visible")
	}
	other.Commit(startTS + 5)
	if s.visible(other, 0, startTS, ownTx) {
		t.Fatal("a record first classified in flight became visible after it committed")
	}
	// The snapshot's OWN record, first seen in flight: visible, and still visible
	// after its commit stamps it above startTS.
	own := mvcc.NewCommitInfo(ownTx)
	if !s.visible(own, 0, startTS, ownTx) {
		t.Fatal("the snapshot's own in-flight record is invisible")
	}
	own.Commit(startTS + 6)
	if !s.visible(own, 0, startTS, ownTx) {
		t.Fatal("the snapshot's own record became invisible after it committed")
	}
	if n := len(s.verdict); n != 2 {
		t.Fatalf("memo holds %d records, want exactly the 2 classified in flight", n)
	}
}
