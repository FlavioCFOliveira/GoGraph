package anomaly_test

// check_test.go — the checker validated on constructed histories whose
// classification is known in advance (rmp #2341 AC3).
//
// Every phenomenon the package claims to detect gets a history built to exhibit
// it and nothing else, and the checker must name THAT one. A checker validated
// only on histories it happens to pass is not validated at all, so each case
// asserts the exact phenomenon rather than merely "some violation".
//
// The SI boundary — lost update forbidden, write skew permitted — is the case
// the task calls the substance of the work, and it is asserted from both sides.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/anomaly"
)

// rd and wr build read and write operations.
func rd(key string, v anomaly.Version) anomaly.Op {
	return anomaly.Op{Kind: anomaly.Read, Key: key, Ver: v}
}
func wt(key string, v anomaly.Version) anomaly.Op {
	return anomaly.Op{Kind: anomaly.Write, Key: key, Ver: v}
}

// tx builds a committed transaction.
func tx(id anomaly.TxID, start, commit uint64, ops ...anomaly.Op) anomaly.Txn {
	return anomaly.Txn{ID: id, Start: start, Commit: commit, Ops: ops}
}

// aborted builds an aborted transaction.
func aborted(id anomaly.TxID, start uint64, ops ...anomaly.Op) anomaly.Txn {
	return anomaly.Txn{ID: id, Start: start, Aborted: true, Ops: ops}
}

// lostUpdate is P4: T1 and T2 both read x at its initial version and both write
// it. In the DSG that is T1 -rw-> T2 (T1 read x0, T2 installed x2) and
// T2 -ww-> T1 (x2 precedes x3)… so the cycle carries EXACTLY ONE
// anti-dependency and is G-single, which snapshot isolation must forbid.
func lostUpdate() *anomaly.History {
	return &anomaly.History{Txns: []anomaly.Txn{
		tx(1, 10, 30, rd("x", 1), wt("x", 3)),
		tx(2, 10, 20, rd("x", 1), wt("x", 2)),
		tx(9, 1, 1, wt("x", 1)), // installs the version both read
	}}
}

// writeSkew is A5B: T1 and T2 each read BOTH x and y, then each writes a
// DIFFERENT one. The cycle is T1 -rw-> T2 -rw-> T1: two anti-dependencies, and
// they are adjacent, so it is G2-item but NOT G-nonadjacent — permitted under
// snapshot isolation.
func writeSkew() *anomaly.History {
	return &anomaly.History{Txns: []anomaly.Txn{
		tx(9, 1, 1, wt("x", 1), wt("y", 1)),
		tx(1, 10, 20, rd("x", 1), rd("y", 1), wt("x", 2)),
		tx(2, 10, 21, rd("x", 1), rd("y", 1), wt("y", 2)),
	}}
}

func TestPhenomenaAreNamedIndividually(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		h     *anomaly.History
		level anomaly.Level
		want  anomaly.Phenomenon
	}{
		{
			// G1a: T2 reads a version T1 wrote, and T1 aborted.
			name: "G1a aborted read",
			h: &anomaly.History{Txns: []anomaly.Txn{
				aborted(1, 10, wt("x", 5)),
				tx(2, 11, 20, rd("x", 5)),
			}},
			level: anomaly.ReadCommitted,
			want:  anomaly.G1a,
		},
		{
			// G1b: T1 writes x twice; T2 reads the FIRST, which no committed
			// state ever held.
			name: "G1b intermediate read",
			h: &anomaly.History{Txns: []anomaly.Txn{
				tx(1, 10, 30, wt("x", 5), wt("x", 6)),
				tx(2, 11, 20, rd("x", 5)),
			}},
			level: anomaly.ReadCommitted,
			want:  anomaly.G1b,
		},
		{
			// G0: T1 and T2 both write x and y, and their versions are ordered
			// oppositely on the two keys — a write cycle with no reads at all.
			name: "G0 write cycle",
			h: &anomaly.History{Txns: []anomaly.Txn{
				tx(1, 10, 20, wt("x", 1), wt("y", 2)),
				tx(2, 10, 21, wt("x", 2), wt("y", 1)),
			}},
			level: anomaly.ReadUncommitted,
			want:  anomaly.G0,
		},
		{
			// G1c: each transaction reads what the other wrote — information
			// flows in a circle, with no anti-dependency involved.
			name: "G1c circular information flow",
			h: &anomaly.History{Txns: []anomaly.Txn{
				tx(1, 10, 20, wt("x", 1), rd("y", 1)),
				tx(2, 10, 21, wt("y", 1), rd("x", 1)),
			}},
			level: anomaly.ReadCommitted,
			want:  anomaly.G1c,
		},
		{
			name:  "G-single lost update",
			h:     lostUpdate(),
			level: anomaly.SnapshotIsolation,
			want:  anomaly.GSingle,
		},
		{
			name:  "G2-item write skew",
			h:     writeSkew(),
			level: anomaly.Serializable,
			want:  anomaly.G2Item,
		},
		{
			// Not a phenomenon — an INCOMPLETE history. It must be surfaced,
			// because a clean verdict computed from missing data is the worst
			// output this package could give.
			name: "unwritten version read",
			h: &anomaly.History{Txns: []anomaly.Txn{
				tx(1, 10, 20, rd("x", 77)),
			}},
			level: anomaly.SnapshotIsolation,
			want:  anomaly.Unwritten,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep, err := anomaly.Check(tc.h, tc.level)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(rep.Violations) == 0 {
				t.Fatalf("expected %s, got a clean report:\n%s", tc.want, rep)
			}
			found := false
			for _, v := range rep.Violations {
				if v.Type == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("expected %s; report named something else:\n%s", tc.want, rep)
			}
		})
	}
}

// TestSnapshotIsolationBoundary is the case the task calls the substance of the
// work, asserted from BOTH sides on the same checker at the same level.
//
// A checker that flagged legal write skew would be worse than none: every clean
// run would carry a false violation, and the real ones would stop being read.
func TestSnapshotIsolationBoundary(t *testing.T) {
	t.Parallel()

	t.Run("write skew is CLEAN under snapshot isolation", func(t *testing.T) {
		t.Parallel()
		rep, err := anomaly.Check(writeSkew(), anomaly.SnapshotIsolation)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !rep.Clean() {
			t.Fatalf("write skew was reported as a violation under snapshot isolation, "+
				"which PERMITS it (Berenson SIGMOD 1995 A5B; Adya PL-SI):\n%s", rep)
		}
		// It must be SEEN and reported as permitted, not merely absent. A
		// checker that overlooked the cycle entirely would also be "clean" here,
		// and the two must not be confused.
		if len(rep.Permitted) == 0 {
			t.Errorf("write skew was not even DETECTED; a clean verdict that saw nothing is "+
				"indistinguishable from one that saw a legal anomaly:\n%s", rep)
		}
		for _, p := range rep.Permitted {
			if p.Type == anomaly.G2Item {
				return
			}
		}
		t.Errorf("the permitted anomaly is not G2-item:\n%s", rep)
	})

	t.Run("write skew IS a violation under serializable", func(t *testing.T) {
		t.Parallel()
		rep, err := anomaly.Check(writeSkew(), anomaly.Serializable)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if rep.Clean() {
			t.Errorf("write skew was clean under serializability, which forbids G2:\n%s", rep)
		}
	})

	t.Run("lost update IS a violation under snapshot isolation", func(t *testing.T) {
		t.Parallel()
		rep, err := anomaly.Check(lostUpdate(), anomaly.SnapshotIsolation)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if rep.Clean() {
			t.Fatalf("lost update was clean under snapshot isolation, which prevents it "+
				"by first-committer-wins:\n%s", rep)
		}
		if got := rep.Violations[0].Type; got != anomaly.GSingle {
			t.Errorf("lost update classified as %s, want G-single", got)
		}
	})

	t.Run("lost update is CLEAN under read committed", func(t *testing.T) {
		t.Parallel()
		rep, err := anomaly.Check(lostUpdate(), anomaly.ReadCommitted)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !rep.Clean() {
			t.Errorf("lost update was a violation under read committed, which permits it:\n%s", rep)
		}
	})
}

// TestCleanHistoryIsClean is the positive control for the whole table: a
// serial, non-overlapping history must be clean at EVERY level.
//
// Without it, a checker that reported a violation for any input would pass every
// detection case above.
func TestCleanHistoryIsClean(t *testing.T) {
	t.Parallel()
	h := &anomaly.History{Txns: []anomaly.Txn{
		tx(1, 1, 2, wt("x", 1), wt("y", 1)),
		tx(2, 3, 4, rd("x", 1), rd("y", 1)),
		tx(3, 5, 6, rd("x", 1), wt("x", 2)),
		tx(4, 7, 8, rd("x", 2), rd("y", 1)),
	}}
	for _, level := range []anomaly.Level{
		anomaly.ReadUncommitted, anomaly.ReadCommitted,
		anomaly.SnapshotIsolation, anomaly.Serializable,
	} {
		rep, err := anomaly.Check(h, level)
		if err != nil {
			t.Fatalf("Check at %s: %v", level, err)
		}
		if !rep.Clean() {
			t.Errorf("a serial history was not clean at %s:\n%s", level, rep)
		}
	}
}

// TestCheckIsDeterministic pins the property the report's value rests on: the
// same history classified twice must produce the same text, because the report
// is the evidence a future sighting is compared against.
func TestCheckIsDeterministic(t *testing.T) {
	t.Parallel()
	h := writeSkew()
	first, err := anomaly.Check(h, anomaly.Serializable)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := first.String()
	for range 25 {
		got, err := anomaly.Check(h, anomaly.Serializable)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if got.String() != want {
			t.Fatalf("classification is not deterministic:\n--- first ---\n%s\n--- later ---\n%s", want, got)
		}
	}
}

// TestValidateRejectsAmbiguousHistories covers the input guard. Each of these
// would otherwise yield a confident, wrong classification — and a wrong answer
// from a checker is worse than no checker, because it is believed.
func TestValidateRejectsAmbiguousHistories(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		h    *anomaly.History
		want string
	}{
		{"duplicate transaction id", &anomaly.History{Txns: []anomaly.Txn{
			tx(1, 1, 2, wt("x", 1)), tx(1, 3, 4, wt("y", 1)),
		}}, "appears twice"},
		{"two writers of one version", &anomaly.History{Txns: []anomaly.Txn{
			tx(1, 1, 2, wt("x", 1)), tx(2, 3, 4, wt("x", 1)),
		}}, "version order is ambiguous"},
		{"aborted but committed", &anomaly.History{Txns: []anomaly.Txn{
			{ID: 1, Aborted: true, Commit: 9, Ops: []anomaly.Op{wt("x", 1)}},
		}}, "marked aborted but carries commit instant"},
		{"write at the initial version", &anomaly.History{Txns: []anomaly.Txn{
			tx(1, 1, 2, wt("x", anomaly.InitVersion)),
		}}, "initial version"},
		{"operation with no key", &anomaly.History{Txns: []anomaly.Txn{
			{ID: 1, Commit: 2, Ops: []anomaly.Op{{Kind: anomaly.Write, Ver: 1}}},
		}}, "no key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := anomaly.Check(tc.h, anomaly.SnapshotIsolation)
			if err == nil {
				t.Fatalf("Check accepted a malformed history (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain the problem\n got: %v\nwant it to contain: %q", err, tc.want)
			}
		})
	}
}
