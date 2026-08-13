package cypher_test

// index_seek_label_guard_test.go — rmp #2423, an openCypher CONFORMANCE and ACID
// CONSISTENCY defect: an index-driven rewrite dropped the label predicate, so a node
// whose label a COMMITTED transaction had removed still matched (n:Label).
//
// # The anomaly, as measured at 9167d3d3
//
//	MATCH (n:Person) RETURN count(n)                        -> 0   correct
//	MATCH (n:Person) WHERE n.k     = 'b' RETURN count(n)    -> 0   correct
//	MATCH (n:Person) WHERE n.email = 'old' RETURN count(n)  -> 1   WRONG
//	MATCH (n:Person) WHERE n.email = 'old' RETURN labels(n) -> []  self-contradictory
//
// The engine returned a row for the pattern (n:Person) and, in that same row,
// reported the node as carrying no labels at all. The only difference between the
// correct reads and the wrong one is that `email` is INDEXED — by the UNIQUE
// constraint's backing hash index — and `k` is not.
//
// # Why the index cannot be trusted for the label
//
// The planner rewrote `Selection(n.email = 'old')` over `NodeByLabelScan(Person)`
// into a bare `NodeByIndexSeek` on the index covering (Person, email), on the
// assumption that membership in a label-scoped index implies the label. An index is
// a CANDIDATE source and over-reports: removing a label leaves the node's entries in
// that label's property indexes behind. The label SCAN never had this problem
// because it resolves through lpg's snapshot-aware LabelBitmapAsOf, which filters
// the over-reporting bitmap; the seek bypassed that path entirely.
//
// So the seek now carries a residual per-candidate label check, and a rewrite that
// cannot obtain one DECLINES rather than emitting candidates it cannot qualify.

import "testing"

// TestIndexSeek_LabelGuard_CommittedRemovalIsRespected is the reproduction. It fails
// against the pre-fix build with `count = 1` and a self-contradictory row.
func TestIndexSeek_LabelGuard_CommittedRemovalIsRespected(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, uniqueSetup...)
	t1, t2 := beginTwo(t, ctx, eng)

	// T1 removes the label and COMMITS; T2's property write on the same node
	// collides and rolls back. What matters afterwards is only that b is no longer a
	// :Person while its indexed property value is unchanged — the state in which the
	// index still lists it.
	if err := execInTx(t1, `MATCH (b:Person {k:'b'}) REMOVE b:Person`); err != nil {
		t.Fatalf("T1 REMOVE b:Person: %v", err)
	}
	_ = execInTx(t2, `MATCH (b {k:'b'}) SET b.email = 'new'`)
	if err := t1.Commit(); err != nil {
		t.Fatalf("T1 COMMIT: %v", err)
	}
	_ = t2.Rollback()

	// The premise: b exists, holds 'old', and is NOT a :Person. Asserted rather than
	// assumed, because every assertion below is meaningless if b is still labelled.
	if got := countQ(t, ctx, eng, `MATCH (n {k:'b'}) RETURN count(n) AS c`); got != 1 {
		t.Fatalf("premise: node b is gone (count=%d); the fixture no longer reproduces", got)
	}
	if got := countQ(t, ctx, eng, `MATCH (n:Person) RETURN count(n) AS c`); got != 0 {
		t.Fatalf("premise: %d :Person nodes remain after a COMMITTED removal, want 0", got)
	}

	// THE DEFECT: an indexed property predicate under the label.
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email = 'old' RETURN count(n) AS c`); got != 0 {
		t.Errorf("MATCH (n:Person) WHERE n.email = 'old' returned %d, want 0: the index-driven "+
			"seek admitted a node whose label a committed transaction removed", got)
	}
	// The inline-property form of the same pattern takes the same rewrite.
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person {email:'old'}) RETURN count(n) AS c`); got != 0 {
		t.Errorf("MATCH (n:Person {email:'old'}) returned %d, want 0", got)
	}
	// And the key-SET form (#2183), which subsumes the same Selection.
	if got := countQ(t, ctx, eng,
		`MATCH (n:Person) WHERE n.email IN ['old','absent'] RETURN count(n) AS c`); got != 0 {
		t.Errorf("MATCH (n:Person) WHERE n.email IN [...] returned %d, want 0: the key-set "+
			"seek admitted a node whose label a committed transaction removed", got)
	}

	// The UNLABELLED query must still find it: the node does hold 'old', and a fix
	// that filtered here would be removing rows the pattern asks for.
	if got := countQ(t, ctx, eng,
		`MATCH (n) WHERE n.email = 'old' RETURN count(n) AS c`); got != 1 {
		t.Errorf("MATCH (n) WHERE n.email = 'old' returned %d, want 1: the guard is filtering "+
			"rows that carry no label requirement at all", got)
	}
}

// TestIndexSeek_LabelGuard_RowIsSelfConsistent is the sharper form of the same
// question, and the one that admits no interpretation: whatever rows the engine
// returns for (n:Person), every one of them must report Person among labels(n).
//
// Before the fix this returned one row whose labels(n) was the EMPTY LIST.
func TestIndexSeek_LabelGuard_RowIsSelfConsistent(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t, uniqueSetup...)
	t1, t2 := beginTwo(t, ctx, eng)
	if err := execInTx(t1, `MATCH (b:Person {k:'b'}) REMOVE b:Person`); err != nil {
		t.Fatalf("T1 REMOVE b:Person: %v", err)
	}
	_ = execInTx(t2, `MATCH (b {k:'b'}) SET b.email = 'new'`)
	if err := t1.Commit(); err != nil {
		t.Fatalf("T1 COMMIT: %v", err)
	}
	_ = t2.Rollback()

	res, err := eng.Run(ctx, `MATCH (n:Person) WHERE n.email = 'old' RETURN size(labels(n)) AS c`, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer func() { _ = res.Close() }()
	rows := 0
	for res.Next() {
		rows++
		rec := res.Record()
		t.Errorf("a row matched (n:Person) while reporting %v labels — one row contradicting "+
			"itself", rec["c"])
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d row(s) returned for a label no node carries", rows)
	}
}

// TestIndexSeek_LabelGuard_StillSeeksWhenSound is the PROPORTIONALITY control: the
// guard must not disable index seeks, only qualify them.
//
// Without it, "no wrong answers" would be satisfied by a fix that stopped using the
// index at all — or worse, by one that dropped correct rows.
func TestIndexSeek_LabelGuard_StillSeeksWhenSound(t *testing.T) {
	eng, ctx := newLabelConflictEngine(t,
		`CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE`,
		`CREATE (a:Person {k:'a', email:'a@x'})`,
		`CREATE (b:Person {k:'b', email:'b@x'})`,
		`CREATE (c:Other  {k:'c', email:'c@x'})`)

	for _, tc := range []struct {
		q    string
		want int64
	}{
		{`MATCH (n:Person) WHERE n.email = 'a@x' RETURN count(n) AS c`, 1},
		{`MATCH (n:Person {email:'b@x'}) RETURN count(n) AS c`, 1},
		{`MATCH (n:Person) WHERE n.email IN ['a@x','b@x'] RETURN count(n) AS c`, 2},
		{`MATCH (n:Person) WHERE n.email = 'nobody' RETURN count(n) AS c`, 0},
		// The value exists but under a DIFFERENT label: the seek must not return it
		// for :Person, and must return it with no label constraint.
		{`MATCH (n:Person) WHERE n.email = 'c@x' RETURN count(n) AS c`, 0},
		{`MATCH (n) WHERE n.email = 'c@x' RETURN count(n) AS c`, 1},
	} {
		if got := countQ(t, ctx, eng, tc.q); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.q, got, tc.want)
		}
	}
}
