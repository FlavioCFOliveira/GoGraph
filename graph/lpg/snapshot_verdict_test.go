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
	s := newSharedSnapshot(startTS, ownTx, 0)

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
	if n := len(s.memo.verdict); n != 0 {
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
	if n := len(s.memo.verdict); n != 2 {
		t.Fatalf("memo holds %d records, want exactly the 2 classified in flight", n)
	}
}

// TestSnapshotVisible_OwnedSnapshotPinsWithoutAMemo covers the owned snapshot a
// direct present-state accessor reads through (rmp #2965, round 5): a record
// first classified in flight stays invisible after it commits, so a read that
// straddles a commit sees the transaction wholly or not at all, and the pin
// needs no shared memo.
func TestSnapshotVisible_OwnedSnapshotPinsWithoutAMemo(t *testing.T) {
	var s Snapshot
	s.startTS = implicitStartTS
	rec := mvcc.NewCommitInfo(mvcc.TxIDBase + 11)
	if s.visible(rec, 0, s.startTS, 0) {
		t.Fatal("an in-flight record is visible to a committed-only read")
	}
	rec.Commit(7)
	if s.visible(rec, 0, s.startTS, 0) {
		t.Fatal("a record first classified in flight became visible mid-read")
	}
	var fresh Snapshot
	fresh.startTS = implicitStartTS
	if !fresh.visible(rec, 0, fresh.startTS, 0) {
		t.Fatal("a committed record is invisible to a new committed-only read")
	}
}

// TestSnapshotVisible_LongSnapshotMemoStaysBounded covers rmp #3026: a long-lived
// shared snapshot meets thousands of transactions in flight that then commit or
// abort. The memo drops each pin once its record has resolved to the verdict it
// pinned, so it stays within [snapMemoPruneFloor], and it keeps every pin a fresh
// classification would answer differently: the snapshot's own record, and records
// that resolved below the start after being pinned invisible.
func TestSnapshotVisible_LongSnapshotMemoStaysBounded(t *testing.T) {
	const startTS, ownTx, transactions = 100, mvcc.TxIDBase + 1, 16 * snapMemoPruneFloor
	s := newSharedSnapshot(startTS, ownTx, 0)

	own := mvcc.NewCommitInfo(ownTx)
	held := make([]*commitInfo, 3) // in flight for the whole run
	for i := range held {
		held[i] = mvcc.NewCommitInfo(mvcc.TxIDBase + 2 + uint64(i))
	}
	if !s.visible(own, 0, startTS, ownTx) {
		t.Fatal("the snapshot's own in-flight record is invisible")
	}
	for _, h := range held {
		if s.visible(h, 0, startTS, ownTx) {
			t.Fatal("another transaction's in-flight record is visible")
		}
	}

	resolved := make([]*commitInfo, 0, transactions)
	peak := 0
	for i := range uint64(transactions) {
		rec := mvcc.NewCommitInfo(mvcc.TxIDBase + 100 + i)
		if s.visible(rec, 0, startTS, ownTx) {
			t.Fatalf("transaction %d: in-flight record is visible", i)
		}
		if i%3 == 0 {
			rec.Abort()
		} else {
			rec.Commit(startTS + 1 + i)
		}
		if s.visible(rec, 0, startTS, ownTx) {
			t.Fatalf("transaction %d: resolved record became visible", i)
		}
		resolved = append(resolved, rec)
		peak = max(peak, len(s.memo.verdict))
	}
	if peak > snapMemoPruneFloor {
		t.Fatalf("memo peaked at %d entries over %d resolved transactions, want at most %d",
			peak, transactions, snapMemoPruneFloor)
	}

	// The pins that must survive every prune: resolved, but a fresh classification
	// would now answer otherwise.
	own.Commit(startTS + 2*transactions)
	for _, h := range held {
		h.Commit(startTS - 1)
	}
	for i := range uint64(4 * snapMemoPruneFloor) { // force further prunes
		rec := mvcc.NewCommitInfo(mvcc.TxIDBase + 1_000_000 + i)
		_ = s.visible(rec, 0, startTS, ownTx)
		rec.Abort()
	}
	if !s.visible(own, 0, startTS, ownTx) {
		t.Fatal("the snapshot's own record became invisible after it committed")
	}
	for _, h := range held {
		if s.visible(h, 0, startTS, ownTx) {
			t.Fatal("a record pinned invisible became visible after it committed below the start")
		}
	}
	for i, rec := range resolved {
		if s.visible(rec, 0, startTS, ownTx) {
			t.Fatalf("transaction %d: answer changed after its pin was dropped", i)
		}
	}
}
