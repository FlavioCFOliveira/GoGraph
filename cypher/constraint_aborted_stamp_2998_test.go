package cypher_test

// constraint_aborted_stamp_2998_test.go — rmp #2998.
//
// The NOT NULL write skew rmp #2353 closed reopened under one extra transaction.
// The per-node constraint stamp a committed label gain leaves on the node is what
// refuses an older snapshot's property removal. A third transaction that stamped
// the same node after that commit and then ABORTED overwrote the slot, and the
// abort cleared it, taking the committed stamp with it: the older snapshot's
// REMOVE found the node unstamped, its own view showed no constrained label, and
// it committed a :Acct node with no email.
//
// The interleaving is scripted with explicit transactions — no goroutines. The
// displacer aborts two ways, both reachable from Cypher: an explicit ROLLBACK, and
// an autocommit statement refused at commit by the very constraint at stake.
// Before the fix both rows commit the violation; after it the older snapshot is
// refused with the typed serialization conflict. The control row, with no aborted
// displacer, is refused on both builds, so a pass of the other rows cannot come
// from the conflict model being disarmed.
//
// Layer: short. The openCypher TCK does not cover concurrent transactions, so
// nothing here is — or is claimed as — TCK coverage.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func TestConstraint_NotNullSkewRefusedAfterAbortedDisplacer(t *testing.T) {
	cases := []struct {
		name string
		// displace stamps n1 after the label-adder's commit and aborts; nil runs
		// the control.
		displace func(t *testing.T, eng *cypher.Engine)
	}{
		{
			name: "explicit_rollback",
			displace: func(t *testing.T, eng *cypher.Engine) {
				tx, err := eng.BeginTx(t.Context())
				if err != nil {
					t.Fatalf("BeginTx (displacer): %v", err)
				}
				if err := execInTx(tx, `MATCH (n {k:'n1'}) SET n.tag = 1`); err != nil {
					t.Fatalf("displacer SET was refused, so it displaced nothing: %v", err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatalf("displacer rollback: %v", err)
				}
			},
		},
		{
			name: "autocommit_refused_by_constraint",
			displace: func(t *testing.T, eng *cypher.Engine) {
				// n1 is :Acct now, so dropping email violates NOT NULL and the
				// statement is rolled back after it stamped the node.
				if err := runLabelConstraintTx(t.Context(), eng, dropProperty); err == nil {
					t.Fatal("displacer REMOVE n.email committed on an :Acct node; " +
						"the NOT NULL constraint did not fire")
				}
			},
		},
		{name: "control"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, eng, ctx := newLabelConflictGraph(t, notNullSetup...)

			// T1's snapshot predates the label gain.
			t1, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatalf("BeginTx (T1): %v", err)
			}
			t.Cleanup(func() { _ = t1.Rollback() })
			if err := execInTx(t1, `MATCH (n {k:'n1'}) RETURN n`); err != nil {
				t.Fatalf("T1 read: %v", err)
			}

			// T2 commits the label gain, stamping n1.
			mustRunTx(t, ctx, eng, addLabel)

			if c.displace != nil {
				c.displace(t, eng)
			}
			// A reclaim while T1 is open must keep the displaced commit: the
			// watermark is pinned at or below T1's snapshot.
			g.ReclaimNow()

			t1Err := execInTx(t1, dropProperty)
			if t1Err == nil {
				// The conflict may legally surface at COMMIT instead of the statement.
				t1Err = t1.Commit()
			}
			if !errors.Is(t1Err, mvcc.ErrSerializationConflict) {
				t.Errorf("T1 REMOVE n.email over a label gain committed after its "+
					"snapshot: err=%v, want ErrSerializationConflict (rmp #2998)", t1Err)
			}
			if got := countQ(t, ctx, eng,
				`MATCH (n:Acct) WHERE n.email IS NULL RETURN count(n) AS c`); got != 0 {
				t.Fatalf("CONSISTENCY: %d :Acct node(s) committed with no email (rmp #2998)", got)
			}

			// Bounded: with every transaction finished the floor is at or below the
			// watermark, so the reclaimer drops every constraint stamp.
			g.ReclaimNow()
			if n := g.MVCCStats().ConstraintStamps; n != 0 {
				t.Fatalf("%d constraint stamps survive a reclaim with no open transaction", n)
			}
		})
	}
}

// TestConstraint_UniqueSkewRefusedAfterAbortedDisplacer is the same loss on the
// UNIQUE path (rmp #2355 widened the stamp to it). T2 commits b's label loss; T1,
// whose snapshot still shows b as a :Person, sets b.email and reserves the new
// value. Before the fix the aborted displacer erased T2's stamp, T1 committed,
// and 'new' stayed reserved with no :Person holding it — a phantom reservation
// that refuses every later node the value (rmp #1342's failure mode). After the
// fix T1 is refused and the value stays free.
func TestConstraint_UniqueSkewRefusedAfterAbortedDisplacer(t *testing.T) {
	for _, displaced := range []bool{true, false} {
		name := "explicit_rollback"
		if !displaced {
			name = "control"
		}
		t.Run(name, func(t *testing.T) {
			_, eng, ctx := newLabelConflictGraph(t, uniqueSetup...)

			t1, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatalf("BeginTx (T1): %v", err)
			}
			t.Cleanup(func() { _ = t1.Rollback() })
			if err := execInTx(t1, `MATCH (b {k:'b'}) RETURN b`); err != nil {
				t.Fatalf("T1 read: %v", err)
			}

			// T2 commits the label loss, stamping b.
			mustRunTx(t, ctx, eng, `MATCH (b:Person {k:'b'}) REMOVE b:Person`)

			if displaced {
				tx, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatalf("BeginTx (displacer): %v", err)
				}
				if err := execInTx(tx, `MATCH (b {k:'b'}) SET b.tag = 1`); err != nil {
					t.Fatalf("displacer SET was refused, so it displaced nothing: %v", err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatalf("displacer rollback: %v", err)
				}
			}

			t1Err := execInTx(t1, `MATCH (b {k:'b'}) SET b.email = 'new'`)
			if t1Err == nil {
				t1Err = t1.Commit()
			}
			if !errors.Is(t1Err, mvcc.ErrSerializationConflict) {
				t.Errorf("T1 SET b.email over a label loss committed after its snapshot: "+
					"err=%v, want ErrSerializationConflict (rmp #2998)", t1Err)
			}
			if got := countQ(t, ctx, eng,
				`MATCH (n:Person) WHERE n.email = 'new' RETURN count(n) AS c`); got != 0 {
				t.Fatalf("%d :Person node(s) hold 'new' before it was ever created", got)
			}
			if err := runLabelConstraintTx(ctx, eng,
				`CREATE (z:Person {k:'z', email:'new'})`); err != nil {
				t.Fatalf("PHANTOM: no :Person holds 'new', yet it is reserved: %v (rmp #2998)", err)
			}
		})
	}
}
