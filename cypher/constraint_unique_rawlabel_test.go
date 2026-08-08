package cypher_test

// constraint_unique_rawlabel_test.go — regression tests for rmp #2355, an ACID
// CONSISTENCY defect: the property path decides whether a UNIQUE constraint applies
// from a RAW present-time label read, so a peer's UNCOMMITTED label removal makes the
// node look unconstrained and the value is written without a reservation.
//
// # The anomaly
//
//	CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE
//	b = (:Person {k:'b', email:'old'})
//	T1: REMOVE b:Person        -- the RAW bag now shows no :Person
//	T2: SET b.email = 'new'    -- raw label read => "not a :Person" => no reservation
//	T1: ROLLBACK               -- b is a :Person again
//	T2: COMMIT   -> nil
//	CREATE (z:Person {k:'z', email:'new'})  -> ACCEPTED
//	=> two :Person nodes share a value declared UNIQUE.
//
// The old value leaks in the same interleaving: T2 released nothing either, so 'old'
// stays reserved with no live node holding it — the phantom failure mode #1342
// records.
//
// UNIQUE is enforced by eager value-set reservations with journaled inverses, NOT by
// the commit-time scan the existence constraint uses, so rmp #2353's per-node
// constraint stamp does not cover this: that stamp is taken only when the registry
// reports a NOT NULL constraint. Reachable for the same reason as #2353 — conflicts
// are per SUBSTORE, and one transaction writes the label while the other writes the
// property.
//
// The openCypher TCK does not cover concurrent transactions, so nothing here is — or
// is claimed as — TCK coverage.

import (
	"testing"
)

// uniqueSetup declares the UNIQUE constraint and one node that holds a value under
// it.
var uniqueSetup = []string{
	`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
	`CREATE (b:Person {k:'b', email:'old'})`,
}

// TestUnique_SameTransactionDuplicateIsRejected is the control that gives the
// assertions below their meaning: it proves the UNIQUE constraint is enforced AT ALL
// in this fixture. Without it, "no duplicate was admitted" would be satisfied by a
// constraint that never fires.
func TestUnique_SameTransactionDuplicateIsRejected(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, uniqueSetup...)

	// A second :Person with the SAME value must be refused, with no concurrency
	// involved at all. Routed through runLabelConstraintTx because a UNIQUE violation
	// is reported on the RESULT, not by RunInTx's return — asserting on the return
	// alone reads every refusal as success, which is how this control first read as a
	// broken constraint.
	if err := runLabelConstraintTx(ctx, eng, `CREATE (z:Person {k:'z', email:'old'})`); err == nil {
		t.Fatal("CONTROL BROKEN: a duplicate UNIQUE value was accepted with no concurrency; " +
			"every assertion in this file is worthless until this passes")
	}
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'old' RETURN count(n) AS c`); got != 1 {
		t.Fatalf("%d :Person nodes carry 'old', want 1", got)
	}
}

// TestUnique_PeerUncommittedLabelRemovalStillReserves is the defect: a peer's
// uncommitted REMOVE of the constrained label must not let this transaction write a
// constrained value without reserving it.
func TestUnique_PeerUncommittedLabelRemovalStillReserves(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, uniqueSetup...)
	t1, t2 := beginTwo(t, ctx, eng)

	// T1 strips the constrained label. Its EAGER write makes the raw bag show no
	// :Person, which is what T2's decision would read.
	if err := execInTx(t1, `MATCH (b:Person {k:'b'}) REMOVE b:Person`); err != nil {
		t.Fatalf("T1 REMOVE b:Person: %v", err)
	}
	// T2 writes the constrained property. In its OWN view b is still a :Person, so
	// the value must be reserved.
	t2Write := execInTx(t2, `MATCH (b {k:'b'}) SET b.email = 'new'`)

	// T1 rolls back, so b is a :Person again in every view.
	if err := t1.Rollback(); err != nil {
		t.Fatalf("t1.Rollback: %v", err)
	}
	t2Commit := t2.Commit()
	t.Logf("T2 SET err = %v; T2 COMMIT err = %v", t2Write, t2Commit)

	// THE INVARIANT. Whether T2 was refused or committed is not specified — either
	// is a legal serialisation — but the committed state must never let two :Person
	// nodes share a value declared UNIQUE. So the probe is a SECOND writer trying to
	// take the same value.
	dupErr := runLabelConstraintTx(ctx, eng, `CREATE (z:Person {k:'z', email:'new'})`)

	dup := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'new' RETURN count(n) AS c`)
	if dup > 1 {
		t.Errorf("CONSISTENCY: %d :Person nodes carry the UNIQUE value 'new' "+
			"(second CREATE err = %v)", dup, dupErr)
	}
	if t2Commit == nil && dupErr == nil && dup == 1 {
		// T2 committed 'new' and the duplicate CREATE reported success yet only one
		// node holds the value: that means the CREATE silently did nothing, which is
		// a different defect and must not be read as a pass.
		t.Errorf("the duplicate CREATE returned nil but only %d node holds 'new': "+
			"a write reported success and applied nothing", dup)
	}
}

// TestUnique_PeerCommittedLabelRemovalDoesNotLeakTheNewValue is the MIRROR of the
// interleaving below: the peer COMMITS its label removal instead of rolling back.
//
// Measured before it was fixed: b ended up not a :Person at all, yet 'new' stayed
// reserved — a reservation held by nobody, the same phantom from the other side. It
// is a separate test because a fix aimed only at the rollback ordering would leave
// this one standing.
func TestUnique_PeerCommittedLabelRemovalDoesNotLeakTheNewValue(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, uniqueSetup...)
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, `MATCH (b:Person {k:'b'}) REMOVE b:Person`); err != nil {
		t.Fatalf("T1 REMOVE b:Person: %v", err)
	}
	t2Write := execInTx(t2, `MATCH (b {k:'b'}) SET b.email = 'new'`)
	c1 := t1.Commit()
	c2 := t2.Commit()
	t.Logf("T2 SET err = %v; T1 COMMIT err = %v; T2 COMMIT err = %v", t2Write, c1, c2)

	// One of them must have been refused; both committing is the anomaly.
	if c1 == nil && c2 == nil {
		t.Error("both transactions committed: the label half and the property half of a " +
			"declared invariant did not collide")
	}
	// Whatever the outcome, a value must be reserved only while a live :Person holds
	// it. Probe both values: the one T2 wrote and the one it moved away from.
	for _, v := range []string{"new", "old"} {
		holders := countQ(t, ctx, eng,
			`MATCH (n:Person) WHERE n.email = '`+v+`' RETURN count(n) AS c`)
		reuse := runLabelConstraintTx(ctx, eng,
			`CREATE (p:Person {k:'probe_`+v+`', email:'`+v+`'})`)
		if holders == 0 && reuse != nil {
			t.Errorf("PHANTOM on %q: no live :Person holds it, yet it is still reserved: %v", v, reuse)
		}
		if holders > 0 && reuse == nil {
			after := countQ(t, ctx, eng,
				`MATCH (n:Person) WHERE n.email = '`+v+`' RETURN count(n) AS c`)
			if after > 1 {
				t.Errorf("CONSISTENCY on %q: %d :Person nodes now hold it", v, after)
			}
		}
	}
}

// TestUnique_DisjointNodesUnderUniqueDoNotConflict is the PROPORTIONALITY control for
// widening the per-node constraint stamp to cover UNIQUE.
//
// The stamp conflicts at NODE granularity, and rmp #2355 widened the writes that take
// it to include a label loss and a property set. That is a real increase in the
// conflict surface for a schema declaring UNIQUE, so the bound has to be pinned:
// transactions working on DIFFERENT nodes must still both commit, or the widening
// turned the constraint into a global lock.
func TestUnique_DisjointNodesUnderUniqueDoNotConflict(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'a@x'})`,
		`CREATE (b:Person {k:'b', email:'b@x'})`)
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, `MATCH (n:Person {k:'a'}) SET n.email = 'a2@x'`); err != nil {
		t.Fatalf("T1 on node a: %v", err)
	}
	if err := execInTx(t2, `MATCH (n:Person {k:'b'}) SET n.email = 'b2@x'`); err != nil {
		t.Fatalf("T2 on node b must not conflict with T1 on node a: %v", err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("T1 commit: %v", err)
	}
	if err := t2.Commit(); err != nil {
		t.Fatalf("T2 commit: %v", err)
	}
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email IN ['a2@x','b2@x'] RETURN count(n) AS c`); got != 2 {
		t.Fatalf("both disjoint transactions should have committed: count=%d, want 2", got)
	}
	// And each vacated value is reusable, so the releases happened.
	for _, v := range []string{"a@x", "b@x"} {
		if err := runLabelConstraintTx(ctx, eng,
			`CREATE (p:Person {k:'p_`+v+`', email:'`+v+`'})`); err != nil {
			t.Errorf("PHANTOM: %q was vacated but is still reserved: %v", v, err)
		}
	}
}

// TestUnique_PeerUncommittedLabelRemovalDoesNotLeakTheOldValue is the other half of
// the same interleaving: if the reservation for the OLD value is not released when it
// should be, that value is unusable for the life of the process even though no live
// node holds it — the phantom rmp #1342 records.
//
// It is a separate test because it fails independently: a fix that reserves the new
// value but still skips the release of the old one passes the test above.
func TestUnique_PeerUncommittedLabelRemovalDoesNotLeakTheOldValue(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, uniqueSetup...)
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, `MATCH (b:Person {k:'b'}) REMOVE b:Person`); err != nil {
		t.Fatalf("T1 REMOVE b:Person: %v", err)
	}
	t2Write := execInTx(t2, `MATCH (b {k:'b'}) SET b.email = 'new'`)
	if err := t1.Rollback(); err != nil {
		t.Fatalf("t1.Rollback: %v", err)
	}
	t2Commit := t2.Commit()
	t.Logf("T2 SET err = %v; T2 COMMIT err = %v", t2Write, t2Commit)

	// If T2 committed, b no longer holds 'old', so 'old' must be reusable by another
	// node. If T2 was refused, b still holds 'old' and it must NOT be reusable.
	holdsOld := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'old' RETURN count(n) AS c`)
	reuseErr := runLabelConstraintTx(ctx, eng, `CREATE (z:Person {k:'z', email:'old'})`)

	switch {
	case holdsOld == 0 && reuseErr != nil:
		t.Errorf("PHANTOM: no live :Person holds 'old', yet the value is still "+
			"reserved and cannot be reused: %v", reuseErr)
	case holdsOld == 1 && reuseErr == nil:
		after := countQ(t, ctx, eng,
			`MATCH (n:Person) WHERE n.email = 'old' RETURN count(n) AS c`)
		if after > 1 {
			t.Errorf("CONSISTENCY: b still holds 'old' and a second node took it too: count=%d", after)
		}
	}
}
