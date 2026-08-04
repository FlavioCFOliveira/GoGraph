package mvcc

// conflict_test.go — the write-write conflict rule (rmp #2300).
//
// The rule is the exact negation of [Visible], so the table below is written as
// the four cases of the DESIGN (docs/design-write-conflict-detection.md §2)
// rather than as a restatement of the implementation. If someone changes
// Conflicts to something other than !Visible, these cases are what must still
// hold.

import (
	"errors"
	"testing"
)

func TestConflicts_TheFourCases(t *testing.T) {
	const (
		startTS = 100
		myTx    = TxIDBase + 7
		otherTx = TxIDBase + 9
	)
	for _, tc := range []struct {
		name   string
		headTS uint64
		want   bool
		why    string
	}{
		{
			name:   "no recorded version",
			headTS: 0,
			want:   false,
			why:    "nothing has written this object since the last reclamation",
		},
		{
			name:   "my own uncommitted version",
			headTS: myTx,
			want:   false,
			why:    "a transaction must be free to write the same object twice",
		},
		{
			name:   "committed before my snapshot",
			headTS: startTS - 1,
			want:   false,
			why:    "I can see it, so I may overwrite it",
		},
		{
			name:   "committed exactly at my snapshot",
			headTS: startTS,
			want:   false,
			why: "startTS is the CONTIGUOUS FRONTIER (rmp #2298), so a commit at it has finished " +
				"and is visible — this is the one boundary where GoGraph differs from Memgraph's ts < start_timestamp",
		},
		{
			name:   "committed after my snapshot",
			headTS: startTS + 1,
			want:   true,
			why:    "first-committer-wins: I may not adopt a version newer than my own snapshot",
		},
		{
			name:   "another transaction still in flight",
			headTS: otherTx,
			want:   true,
			why:    "first-updater-wins: someone got there first and has not finished",
		},
		{
			name:   "an aborted transaction's version",
			headTS: AbortedTS,
			want:   true,
			why: "an aborted head CONFLICTS since rmp #2318, and this case has now been " +
				"measured BOTH ways. rmp #2300 exempted it because refusing made the first " +
				"object any transaction aborted on PERMANENTLY UNWRITABLE — " +
				"examples/27_concurrent_txn's writers exhausted a nine-attempt retry chain. " +
				"But the exemption let the NEXT writer build on a stored value that still " +
				"held the aborted transaction's writes, and a later reader then saw them " +
				"(measured: `reader sees L=true M=true`) — an ATOMICITY violation, and for " +
				"the adjacency an unrecoverable one, because that chain holds entry " +
				"SNAPSHOTS rather than undo actions. rmp #2318 withdraws an aborted version " +
				"SYNCHRONOUSLY at abort, so the head is gone before the next writer arrives " +
				"and this branch covers only the race window — where refusing yields a " +
				"retriable serialization failure instead of a lost update",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Conflicts(tc.headTS, startTS, myTx)
			if got != tc.want {
				t.Fatalf("Conflicts(head=%d, start=%d, tx=%d) = %v, want %v — %s",
					tc.headTS, startTS, myTx, got, tc.want, tc.why)
			}
			// The rule IS the exact negation of the read-side test for every input
			// but the zero head, which has no version to be visible at all.
			// Asserting it here is what stops the two drifting apart, which is how a
			// lost update gets shipped. rmp #2318 removed the ABORTED head from this
			// exclusion: it is no longer an asymmetry, because the version is
			// withdrawn at abort rather than left to be overwritten.
			if tc.headTS != 0 {
				if got == Visible(tc.headTS, startTS, myTx) {
					t.Fatalf("Conflicts and Visible agree for head=%d; they must be exact opposites",
						tc.headTS)
				}
			}
		})
	}
	// The aborted head is invisible AND unwritable, which is no longer an asymmetry
	// at all — it is what [Conflicts] being the plain negation of [Visible] means.
	// Stated directly because the property it replaces (invisible but overwritable)
	// shipped an atomicity violation, and a reader of this file should see which one
	// is in force.
	t.Run("an aborted version is invisible AND unwritable", func(t *testing.T) {
		if Visible(AbortedTS, startTS, myTx) {
			t.Fatal("an aborted version is VISIBLE; readers would observe the work of a " +
				"transaction that never committed")
		}
		if !Conflicts(AbortedTS, startTS, myTx) {
			t.Fatal("a writer may overwrite an aborted version; it would build on a stored " +
				"value that still holds the aborted transaction's writes, and a later reader " +
				"would see them (rmp #2318). Liveness comes from withdrawing the version at " +
				"abort, not from letting the writer through")
		}
	})
}

func TestConflict_IsIdentifiableAndDescribes(t *testing.T) {
	err := NewConflict("node labels", TxIDBase+9, 100, TxIDBase+7)

	if !errors.Is(err, ErrSerializationConflict) {
		t.Fatal("a Conflict is not errors.Is(ErrSerializationConflict): a caller cannot identify it, " +
			"so the Bolt boundary cannot map it to a retriable code")
	}
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatal("errors.As did not recover the Conflict detail")
	}
	if c.Store != "node labels" {
		t.Fatalf("Store = %q, want %q", c.Store, "node labels")
	}
	if !c.ConcurrentWriter() {
		t.Fatal("a head above TxIDBase is an in-flight writer (first-updater-wins) and must report as one")
	}

	committed := NewConflict("node properties", 101, 100, TxIDBase+7)
	if committed.ConcurrentWriter() {
		t.Fatal("a head below TxIDBase is a COMMITTED version (first-committer-wins), not an in-flight writer")
	}
	if !errors.Is(committed, ErrSerializationConflict) {
		t.Fatal("a first-committer-wins conflict must be identifiable too")
	}
}
