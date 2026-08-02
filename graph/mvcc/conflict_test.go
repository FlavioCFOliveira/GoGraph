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
			why:    "an aborted version is not visible, so it is not overwritable through this path either",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Conflicts(tc.headTS, startTS, myTx)
			if got != tc.want {
				t.Fatalf("Conflicts(head=%d, start=%d, tx=%d) = %v, want %v — %s",
					tc.headTS, startTS, myTx, got, tc.want, tc.why)
			}
			// The rule IS the negation of the read-side test, for every input
			// but the zero head. Asserting it here is what stops the two
			// drifting apart, which is how a lost update gets shipped.
			if tc.headTS != 0 {
				if got == Visible(tc.headTS, startTS, myTx) {
					t.Fatalf("Conflicts and Visible agree for head=%d; they must be exact opposites",
						tc.headTS)
				}
			}
		})
	}
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
