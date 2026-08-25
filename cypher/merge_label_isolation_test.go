package cypher_test

// merge_label_isolation_test.go — rmp #2365.
//
// MERGE decides whether an existing node matches its pattern by reading the
// node's labels RAW (cypher/exec/merge_search.go and merge_pattern.go call
// mutator.NodeLabels, not the transaction-resolved read exec.labelsInTx that
// rmp #2355 introduced for the constraint path). A bare shard read returns the
// NEWEST stored label set, which includes other in-flight transactions' eager,
// uncommitted writes.
//
// Reachable for the same reason as rmp #2353 and #2355: conflicts are per
// SUBSTORE, so a transaction writing the LABEL never collides with one reading it
// to make a match decision.
//
// These tests REPRODUCE the consequence before anything is changed, in both
// directions, and each is paired with a control so a fix that broke MERGE
// outright could not satisfy them.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// mergeBranch runs a MERGE that records which branch it took, inside tx, and
// returns the recorded value.
//
// The branch is recorded as a PROPERTY rather than inferred from a node count,
// because a count cannot tell ON MATCH from an ON CREATE that happened to
// produce the same total, and the property is what a caller would actually
// observe.
func mergeBranch(t *testing.T, tx *cypher.ExplicitTx, label, key string) string {
	t.Helper()
	branches := mergeBranches(t, tx, label, key)
	if len(branches) != 1 {
		t.Fatalf("the read-back matched %d node(s) with :%s {k:%q}, want exactly 1: %v. More "+
			"than one means MERGE created a DUPLICATE of a node that already existed",
			len(branches), label, key, branches)
	}
	return branches[0]
}

// mergeBranches runs the branch-recording MERGE inside tx and returns the branch
// property of EVERY node the pattern then matches.
//
// It returns a slice rather than one value because the duplicate is the symptom
// in one of the two directions: a MERGE that wrongly took ON CREATE leaves two
// nodes where one existed, and a helper that read only the first would report
// the wrong branch instead of the duplication.
func mergeBranches(t *testing.T, tx *cypher.ExplicitTx, label, key string) []string {
	t.Helper()
	q := "MERGE (n:" + label + " {k:'" + key + "'}) " +
		"ON CREATE SET n.branch = 'create' " +
		"ON MATCH SET n.branch = 'match'"
	if err := execInTx(tx, q); err != nil {
		t.Fatalf("MERGE: %v", err)
	}
	r, err := tx.Exec("MATCH (n:"+label+" {k:'"+key+"'}) RETURN n.branch AS b", nil)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer func() { _ = r.Close() }()
	var got []string
	for r.Next() {
		got = append(got, scalarString(r.Record()["b"]))
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read back drain: %v", err)
	}
	return got
}

// scalarString renders a returned value as a bare Go string.
//
// fmt.Sprint on a returned STRING renders it QUOTED, which is how the first
// version of this helper reported `"match"` against a want of `match` and made
// the engine look wrong when the harness was. The quotes are stripped here, once.
func scalarString(v any) string {
	s := fmt.Sprint(v)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// TestMerge_DoesNotMatchOnAPeersUncommittedLabel is direction A of rmp #2365.
//
// T1 eagerly adds :Target to a node that does not carry it and does NOT commit.
// T2's MERGE must not see that label: in every COMMITTED state the node is
// unlabelled, so MERGE must take ON CREATE. Seeing it means MERGE matched a node
// on a label that may never exist — if T1 rolls back, it never did.
func TestMerge_DoesNotMatchOnAPeersUncommittedLabel(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Node {k:'x'})")
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, "MATCH (n:Node {k:'x'}) SET n:Target"); err != nil {
		t.Fatalf("T1 SET n:Target: %v", err)
	}

	if got := mergeBranch(t, t2, "Target", "x"); got != "create" {
		t.Errorf("T2's MERGE took ON %s; want ON CREATE. It matched an existing node on a "+
			"label only T1's UNCOMMITTED write put there, so a T1 rollback leaves MERGE having "+
			"matched on a label that never existed in any committed state (rmp #2365)", got)
	}
}

// TestMerge_MatchesDespiteAPeersUncommittedLabelRemoval is direction B, the
// mirror: a peer's uncommitted REMOVE must not HIDE a node that does match.
//
// Hiding it sends MERGE down ON CREATE and produces a DUPLICATE of a node that
// exists in every committed state.
func TestMerge_MatchesDespiteAPeersUncommittedLabelRemoval(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Target {k:'y'})")
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, "MATCH (n:Target {k:'y'}) REMOVE n:Target"); err != nil {
		t.Fatalf("T1 REMOVE n:Target: %v", err)
	}

	if got := mergeBranch(t, t2, "Target", "y"); got != "match" {
		t.Errorf("T2's MERGE took ON %s; want ON MATCH. A peer's UNCOMMITTED removal hid a node "+
			"that carries :Target in every committed state, so MERGE created a duplicate "+
			"(rmp #2365)", got)
	}
}

// TestMerge_PositiveControl_SingleTransactionBranches is the control required by
// the acceptance criteria: an ordinary MERGE, with no peer in sight, must still
// take ON MATCH for a node that genuinely carries the label and ON CREATE for one
// that does not.
//
// Without it, a "fix" that made MERGE never match — or always match — would
// satisfy both negatives above.
func TestMerge_PositiveControl_SingleTransactionBranches(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Target {k:'present'})")

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if got := mergeBranch(t, tx, "Target", "present"); got != "match" {
		t.Errorf("MERGE on a node that genuinely carries :Target took ON %s, want ON MATCH", got)
	}
	if got := mergeBranch(t, tx, "Target", "absent"); got != "create" {
		t.Errorf("MERGE on a node that does not exist took ON %s, want ON CREATE", got)
	}
}

// TestMerge_SeesItsOwnUncommittedLabel is acceptance criterion 3:
// read-your-own-writes. A transaction's OWN eager label write must remain
// visible to its own MERGE.
//
// This is the clause a view-resolved read could most easily break, and it is
// also MERGE's ON MATCH shape — the reason the raw read was left in place when
// rmp #2355 moved the constraint path.
func TestMerge_SeesItsOwnUncommittedLabel(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Node {k:'own'})")

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := execInTx(tx, "MATCH (n:Node {k:'own'}) SET n:Target"); err != nil {
		t.Fatalf("SET n:Target: %v", err)
	}
	if got := mergeBranch(t, tx, "Target", "own"); got != "match" {
		t.Errorf("MERGE took ON %s for a label THIS transaction added itself; want ON MATCH. "+
			"Read-your-own-writes must survive the fix", got)
	}
}

// TestMerge_MatchesDespiteAPeersUncommittedPropertyChange is the PROPERTY half
// of rmp #2365, found by probing after the label half was fixed.
//
// The exposure is identical: GraphMutator.NodeProperties is a bare shard read
// returning the newest stored value, a peer's eager uncommitted write included.
// A peer renaming the key hid the node from MERGE, which then created a
// DUPLICATE of a node that carries the committed key in every committed state.
//
// MEASURED before the fix, and the sharpest statement of the whole ticket:
//
//	T2: MATCH (n:Target {k:'old'}) RETURN count(n)  => 1
//	T2: MERGE (n:Target {k:'old'}) …                => duplicate
//
// MATCH and MERGE disagreeing inside ONE transaction is what says the defect is
// at the decision site and not in the candidate enumeration.
func TestMerge_MatchesDespiteAPeersUncommittedPropertyChange(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Target {k:'old'})")
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, "MATCH (n:Target {k:'old'}) SET n.k = 'new'"); err != nil {
		t.Fatalf("T1 SET n.k: %v", err)
	}

	// The premise, asserted rather than assumed: T2 must still SEE the node under
	// its committed key, or the MERGE assertion below would be about a node that
	// is legitimately invisible.
	if n := countInTx(t, t2, "MATCH (n:Target {k:'old'}) RETURN count(n) AS c"); n != "1" {
		t.Fatalf("premise: T2 sees %s node(s) under the committed key, want 1", n)
	}

	// The MERGE carries NO action clause, deliberately. With ON MATCH the
	// decision is correct but the ACTION then collides with T1's uncommitted
	// write to the same node's properties — a genuine serialization conflict,
	// and the right outcome, but one that hides which BRANCH was taken. An
	// action-free MERGE isolates the decision: match leaves one node, a wrong
	// ON CREATE leaves two.
	if err := execInTx(t2, "MERGE (n:Target {k:'old'})"); err != nil {
		t.Fatalf("T2 MERGE: %v", err)
	}
	if n := countInTx(t, t2, "MATCH (n:Target {k:'old'}) RETURN count(n) AS c"); n != "1" {
		t.Errorf("after T2's MERGE there are %s node(s) with :Target {k:'old'}, want 1. A peer's "+
			"UNCOMMITTED property write hid a node that MATCH can see in the SAME transaction, "+
			"so MERGE took ON CREATE and duplicated it (rmp #2365)", n)
	}
}

// TestMerge_ConflictsRatherThanDuplicating_OnAPeersUncommittedPropertyWrite is
// the other face of the same fix, and the better outcome.
//
// With the raw read, a MERGE whose ON MATCH writes the same node as an
// uncommitted peer silently produced a DUPLICATE. Resolved through the
// transaction view it correctly takes ON MATCH and the write then meets a real
// serialization conflict in the node-properties store — refused, not duplicated.
//
// Refusing is what the ACID contract asks for; duplicating is what it forbids.
func TestMerge_ConflictsRatherThanDuplicating_OnAPeersUncommittedPropertyWrite(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Target {k:'old'})")
	t1, t2 := beginTwo(t, ctx, eng)

	if err := execInTx(t1, "MATCH (n:Target {k:'old'}) SET n.k = 'new'"); err != nil {
		t.Fatalf("T1 SET n.k: %v", err)
	}

	err := execInTx(t2, "MERGE (n:Target {k:'old'}) ON MATCH SET n.branch = 'match'")
	if err == nil {
		t.Fatalf("T2's MERGE succeeded while a peer holds an uncommitted write to the same " +
			"node's properties; want a serialization conflict. Succeeding means it either " +
			"wrote over the peer or created a duplicate (rmp #2365)")
	}
	assertPropertyStoreConflict(t, err)
	if n := countInTx(t, t2, "MATCH (n:Target) RETURN count(n) AS c"); n != "1" {
		t.Errorf("the refused MERGE left %s :Target node(s), want 1: a conflict must leave no "+
			"partially-created node behind", n)
	}
}

// assertPropertyStoreConflict asserts err is a serialization conflict attributed
// to the node-properties store. Checking only that an error occurred would pass
// for a parse failure, so the attribution is asserted too.
func assertPropertyStoreConflict(t *testing.T, err error) {
	t.Helper()
	if !strings.Contains(err.Error(), "serialization conflict") {
		t.Fatalf("error = %v; want a serialization conflict", err)
	}
	if !strings.Contains(err.Error(), "node properties") {
		t.Errorf("error = %v; want the conflict attributed to the node-properties store, so "+
			"this cannot pass on a conflict raised somewhere else", err)
	}
}

// TestMerge_SeesItsOwnUncommittedPropertyChange is read-your-own-writes for the
// property half: a transaction's own eager property write must remain visible to
// its own MERGE, exactly as its own label write must.
func TestMerge_SeesItsOwnUncommittedPropertyChange(t *testing.T) {
	t.Parallel()
	eng, ctx := newLabelConflictEngine(t, "CREATE (:Target {k:'before'})")

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := execInTx(tx, "MATCH (n:Target {k:'before'}) SET n.k = 'after'"); err != nil {
		t.Fatalf("SET n.k: %v", err)
	}
	if got := mergeBranch(t, tx, "Target", "after"); got != "match" {
		t.Errorf("MERGE took ON %s for a key THIS transaction wrote itself; want ON MATCH", got)
	}
}

// countInTx runs a counting query inside tx and returns the rendered count.
func countInTx(t *testing.T, tx *cypher.ExplicitTx, q string) string {
	t.Helper()
	r, err := tx.Exec(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = r.Close() }()
	var got string
	for r.Next() {
		got = scalarString(r.Record()["c"])
	}
	if err := r.Err(); err != nil {
		t.Fatalf("%s drain: %v", q, err)
	}
	return got
}
