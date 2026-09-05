package sim

import (
	"slices"
	"testing"
)

// seedPerson commits one Person into the parent oracle, failing the test on a
// non-committed prediction.
func seedPerson(t *testing.T, o *GraphOracle, name string, age int64) {
	t.Helper()
	res := o.ApplyCreate(tmplCreatePerson, map[string]any{"name": name, "age": age})
	if !res.Committed || res.NodesCreated != 1 {
		t.Fatalf("seed %q: %+v", name, res)
	}
}

// TestOracleTx_ReadsSnapshotPlusOwnWrites asserts a workspace read observes the
// begin-snapshot overlaid with the transaction's own pending writes, while
// the committed state stays untouched until Commit.
func TestOracleTx_ReadsSnapshotPlusOwnWrites(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "alice", 30)

	tx := o.BeginTx()
	if res := tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(20)}); !res.Committed {
		t.Fatalf("tx create: %+v", res)
	}
	if res := tx.ApplyMatch(tmplSetAge, map[string]any{"name": "alice", "age": int64(31)}); !res.Committed {
		t.Fatalf("tx set: %+v", res)
	}

	// The transaction sees both its own pending write and the updated age.
	if !tx.HasPerson("bob") {
		t.Fatal("tx does not see its own pending create")
	}
	if age, ok := tx.AgeOf("alice"); !ok || age != int64(31) {
		t.Fatalf("tx AgeOf(alice)=%v,%v; want 31,true", age, ok)
	}
	if got, want := tx.NodeNames(), []string{"alice", "bob"}; !slices.Equal(got, want) {
		t.Fatalf("tx NodeNames=%v; want %v", got, want)
	}
	if tx.NodeCount() != 2 {
		t.Fatalf("tx NodeCount=%d; want 2", tx.NodeCount())
	}

	// The committed state is unchanged: no bob, alice still 30, history unmoved.
	if o.NodeCount() != 1 {
		t.Fatalf("committed NodeCount=%d; want 1 (pending write leaked)", o.NodeCount())
	}
	if _, ok := o.byName["bob"]; ok {
		t.Fatal("pending create leaked into committed state before Commit")
	}
	if age := o.nodes[o.byName["alice"]].Properties["age"]; age != int64(30) {
		t.Fatalf("committed alice age=%v; want 30 (pending SET leaked)", age)
	}
	if len(o.Ops()) != 1 {
		t.Fatalf("committed history len=%d; want 1 (tx ops leaked)", len(o.Ops()))
	}
	tx.Abort()
}

// TestOracleTx_CommitAppliesAtomically asserts the happy-path fold publishes
// every decided effect in one step, including edges to nodes created inside
// the same transaction, and folds the tx history into the parent's.
func TestOracleTx_CommitAppliesAtomically(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "alice", 30)
	histBefore := len(o.Ops())

	tx := o.BeginTx()
	tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(20)})
	tx.ApplyMatch(tmplSetAge, map[string]any{"name": "alice", "age": int64(31)})
	if res := tx.ApplyCreateKnows(map[string]any{"a": "alice", "b": "bob"}); res.EdgesCreated != 1 {
		t.Fatalf("tx edge to own pending node: %+v", res)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if o.NodeCount() != 2 || o.EdgeCount() != 1 {
		t.Fatalf("committed nodes=%d edges=%d; want 2,1", o.NodeCount(), o.EdgeCount())
	}
	if age := o.nodes[o.byName["alice"]].Properties["age"]; age != int64(31) {
		t.Fatalf("committed alice age=%v; want 31", age)
	}
	if !o.HasKnowsByName("alice", "bob") {
		t.Fatal("committed state misses the folded edge")
	}
	if got := len(o.Ops()) - histBefore; got != 3 {
		t.Fatalf("history grew by %d; want 3 (tx ops folded)", got)
	}
	// A finished workspace refuses a second Commit.
	if err := tx.Commit(); err == nil {
		t.Fatal("second Commit on a finished tx succeeded")
	}
}

// TestOracleTx_CommitIsAllOrNothing asserts a fold that fails validation (its
// decided SET's target vanished from the committed state after the statement
// ran) applies NOTHING: not even the transaction's independent create.
func TestOracleTx_CommitIsAllOrNothing(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "alice", 30)

	tx := o.BeginTx()
	tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(20)})
	tx.ApplyMatch(tmplSetAge, map[string]any{"name": "alice", "age": int64(31)})

	// A concurrent session commits a DETACH DELETE of alice between the tx's
	// statements and its Commit — the collision the engine must refuse.
	if res := o.ApplyDelete(tmplDetachDelete, map[string]any{"name": "alice"}); !res.Committed {
		t.Fatalf("concurrent delete: %+v", res)
	}
	nodesBefore, edgesBefore, histBefore := o.NodeCount(), o.EdgeCount(), len(o.Ops())

	err := tx.Commit()
	if err == nil {
		t.Fatal("Commit folded over a vanished SET target; want validation error")
	}
	if o.NodeCount() != nodesBefore || o.EdgeCount() != edgesBefore || len(o.Ops()) != histBefore {
		t.Fatalf("failed Commit mutated the parent: nodes %d->%d edges %d->%d hist %d->%d",
			nodesBefore, o.NodeCount(), edgesBefore, o.EdgeCount(), histBefore, len(o.Ops()))
	}
	if _, ok := o.byName["bob"]; ok {
		t.Fatal("failed Commit partially applied (independent create leaked)")
	}
	// The workspace is still unfinished, so the driver can Abort it cleanly.
	tx.Abort()
}

// TestOracleTx_AbortLeavesNoTrace asserts Abort discards every pending effect
// and the transaction-local history.
func TestOracleTx_AbortLeavesNoTrace(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "alice", 30)
	histBefore := len(o.Ops())

	tx := o.BeginTx()
	tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(20)})
	tx.ApplyMerge(tmplMergePerson, map[string]any{"name": "carol"})
	tx.ApplyMatch(tmplSetAge, map[string]any{"name": "alice", "age": int64(99)})
	tx.ApplyCreateKnows(map[string]any{"a": "alice", "b": "bob"})
	tx.ApplyDelete(tmplDetachDelete, map[string]any{"name": "alice"})
	tx.Abort()

	if o.NodeCount() != 1 || o.EdgeCount() != 0 {
		t.Fatalf("abort leaked state: nodes=%d edges=%d; want 1,0", o.NodeCount(), o.EdgeCount())
	}
	if age := o.nodes[o.byName["alice"]].Properties["age"]; age != int64(30) {
		t.Fatalf("abort leaked SET: age=%v; want 30", age)
	}
	if len(o.Ops()) != histBefore {
		t.Fatalf("abort leaked history: %d ops; want %d", len(o.Ops()), histBefore)
	}
}

// TestOracleTx_OverlaySemantics covers the overlay edge cases: MERGE sees a
// pending create, DELETE cancels a pending create and its pending edges, a
// re-create after an in-tx delete resurrects the name, and DETACH drops a
// pending edge so the fold never references a deleted endpoint.
func TestOracleTx_OverlaySemantics(t *testing.T) {
	t.Run("merge is a no-op on an own-pending name", func(t *testing.T) {
		o := NewGraphOracle()
		tx := o.BeginTx()
		tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(1)})
		if res := tx.ApplyMerge(tmplMergePerson, map[string]any{"name": "bob"}); res.NodesCreated != 0 {
			t.Fatalf("merge re-created an own-pending node: %+v", res)
		}
		tx.Abort()
	})

	t.Run("delete cancels an own-pending create and its edges", func(t *testing.T) {
		o := NewGraphOracle()
		seedPerson(t, o, "alice", 30)
		tx := o.BeginTx()
		tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(1)})
		tx.ApplyCreateKnows(map[string]any{"a": "alice", "b": "bob"})
		tx.ApplyDelete(tmplDetachDelete, map[string]any{"name": "bob"})
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if o.NodeCount() != 1 || o.EdgeCount() != 0 {
			t.Fatalf("cancelled create leaked: nodes=%d edges=%d; want 1,0", o.NodeCount(), o.EdgeCount())
		}
	})

	t.Run("re-create after in-tx delete resurrects the name", func(t *testing.T) {
		o := NewGraphOracle()
		seedPerson(t, o, "alice", 30)
		tx := o.BeginTx()
		tx.ApplyDelete(tmplDetachDelete, map[string]any{"name": "alice"})
		if tx.HasPerson("alice") {
			t.Fatal("deleted name still visible in-tx")
		}
		tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "alice", "age": int64(5)})
		if age, ok := tx.AgeOf("alice"); !ok || age != int64(5) {
			t.Fatalf("resurrected AgeOf=%v,%v; want 5,true", age, ok)
		}
		if got := tx.NodeNames(); !slices.Equal(got, []string{"alice"}) {
			t.Fatalf("resurrected NodeNames=%v; want [alice]", got)
		}
		if tx.NodeCount() != 1 {
			t.Fatalf("resurrected NodeCount=%d; want 1", tx.NodeCount())
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if age := o.nodes[o.byName["alice"]].Properties["age"]; age != int64(5) {
			t.Fatalf("committed resurrected age=%v; want 5", age)
		}
	})

	t.Run("DETACH drops a pending edge before the fold", func(t *testing.T) {
		o := NewGraphOracle()
		seedPerson(t, o, "alice", 30)
		seedPerson(t, o, "bob", 31)
		tx := o.BeginTx()
		tx.ApplyCreateKnows(map[string]any{"a": "alice", "b": "bob"})
		// Deleting bob AFTER deciding the edge drops the pending edge (DETACH),
		// so this must fold cleanly with no edge.
		tx.ApplyDelete(tmplDetachDelete, map[string]any{"name": "bob"})
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if o.EdgeCount() != 0 {
			t.Fatalf("DETACH-dropped edge folded anyway: edges=%d", o.EdgeCount())
		}
		if _, ok := o.byName["bob"]; ok {
			t.Fatal("in-tx delete of committed node did not fold")
		}
	})
}

// TestOracleTx_CommitRefusesMergeRace asserts a MERGE-decided create whose
// name has since appeared in the committed state fails validation — folding it
// would re-point the name index and orphan the concurrently-committed node —
// and that the failed fold applies nothing.
func TestOracleTx_CommitRefusesMergeRace(t *testing.T) {
	o := NewGraphOracle()
	tx := o.BeginTx()
	// MERGE decides a create: "carol" is absent when the statement runs.
	if res := tx.ApplyMerge(tmplMergePerson, map[string]any{"name": "carol"}); res.NodesCreated != 1 {
		t.Fatalf("merge on a miss must decide a create: %+v", res)
	}
	// A concurrent session commits the same name before this tx's Commit.
	seedPerson(t, o, "carol", 40)
	carolID := o.byName["carol"]

	if err := tx.Commit(); err == nil {
		t.Fatal("Commit folded a MERGE race; want validation error")
	}
	if o.NodeCount() != 1 || o.byName["carol"] != carolID {
		t.Fatalf("failed Commit disturbed the committed node: count=%d id=%d want 1,%d",
			o.NodeCount(), o.byName["carol"], carolID)
	}
	tx.Abort()
}

// TestOracleTx_DeterministicAccessorsAndFold asserts the workspace accessors
// and the Commit fold are deterministic: two identically-driven oracles end
// byte-equivalent, including allocated node ids.
func TestOracleTx_DeterministicAccessorsAndFold(t *testing.T) {
	build := func() *GraphOracle {
		o := NewGraphOracle()
		seedPerson(t, o, "p3", 3)
		seedPerson(t, o, "p1", 1)
		tx := o.BeginTx()
		// Insertion order deliberately unsorted, so a map-range fold would
		// allocate ids non-deterministically.
		tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "z9", "age": int64(9)})
		tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "a0", "age": int64(0)})
		tx.ApplyMerge(tmplMergePerson, map[string]any{"name": "m5"})
		if got, want := tx.NodeNames(), []string{"a0", "m5", "p1", "p3", "z9"}; !slices.Equal(got, want) {
			t.Fatalf("tx NodeNames=%v; want %v", got, want)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		return o
	}
	a, b := build(), build()
	if !slices.Equal(a.NodeNames(), b.NodeNames()) {
		t.Fatalf("NodeNames diverge: %v vs %v", a.NodeNames(), b.NodeNames())
	}
	for _, name := range a.NodeNames() {
		if a.byName[name] != b.byName[name] {
			t.Fatalf("id allocation diverges for %q: %d vs %d", name, a.byName[name], b.byName[name])
		}
	}
}

// TestOracleTx_SnapshotIsolationAtBegin asserts a commit that lands AFTER the
// workspace began is invisible to it for its whole lifetime — the engine's
// snapshot-isolation contract the workspace mirrors (see
// TestProbe_WriteTxSnapshotAtBegin for the engine-side evidence).
func TestOracleTx_SnapshotIsolationAtBegin(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "alice", 30)

	tx := o.BeginTx()
	// A concurrent session commits carol and mutates alice AFTER tx began.
	seedPerson(t, o, "carol", 40)
	if res := o.ApplyMatch(tmplSetAge, map[string]any{"name": "alice", "age": int64(77)}); !res.Committed {
		t.Fatalf("concurrent set: %+v", res)
	}

	if tx.HasPerson("carol") {
		t.Fatal("post-BEGIN commit visible inside the workspace (snapshot isolation broken)")
	}
	if age, ok := tx.AgeOf("alice"); !ok || age != int64(30) {
		t.Fatalf("workspace observes post-BEGIN mutation: AgeOf(alice)=%v,%v; want the begin value 30", age, ok)
	}
	if got, want := tx.NodeNames(), []string{"alice"}; !slices.Equal(got, want) {
		t.Fatalf("workspace NodeNames=%v; want %v", got, want)
	}
	if tx.NodeCount() != 1 {
		t.Fatalf("workspace NodeCount=%d; want 1", tx.NodeCount())
	}

	// An unrelated create still folds cleanly over the advanced parent.
	tx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "bob", "age": int64(1)})
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if o.NodeCount() != 3 {
		t.Fatalf("committed NodeCount=%d; want 3", o.NodeCount())
	}
	if age := o.nodes[o.byName["alice"]].Properties["age"]; age != int64(77) {
		t.Fatalf("fold disturbed the concurrent SET: age=%v; want 77", age)
	}
}

// TestOracleTx_ValuePreservingSetIsNotAWrite is the rmp #2717 regression at
// model scope. A SET that stores the value the transaction already observes
// records NO version in the engine (graph/lpg/property.go, the
// propValuesDefinitelyEqual guard in setNodePropertyInfo), so it neither
// conflicts with a concurrent DETACH DELETE nor makes the transaction one —
// and the workspace must fold cleanly over the delete. A value-CHANGING SET is
// a real write and must still be refused, because there the engine does refuse
// one of the two transactions.
//
// Before the fix the value-preserving arm refused the fold, and the
// multi-session mode reported it as an isolation finding on seeds 22 (crash
// arm), 500 and 572 (no-crash arm).
func TestOracleTx_ValuePreservingSetIsNotAWrite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setAge    int64
		wantRefus bool
	}{
		{"value preserving", 95, false},
		{"value changing", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewGraphOracle()
			seedPerson(t, o, "X", 95)

			setter := o.BeginTx()
			if res := setter.ApplyMatch(tmplSetAge, map[string]any{"name": "X", "age": tc.setAge}); !res.Committed {
				t.Fatalf("tx set: %+v", res)
			}

			// A concurrent transaction deletes X and commits FIRST.
			deleter := o.BeginTx()
			if res := deleter.ApplyDelete(tmplDetachDelete, map[string]any{"name": "X"}); !res.Committed {
				t.Fatalf("tx delete: %+v", res)
			}
			if err := deleter.Commit(); err != nil {
				t.Fatalf("deleter commit: %v", err)
			}
			if o.HasPersonName("X") {
				t.Fatal("committed model still holds X after the delete folded")
			}

			err := setter.Commit()
			switch {
			case tc.wantRefus && err == nil:
				t.Fatal("a value-CHANGING SET over a concurrently deleted node folded cleanly; " +
					"the engine refuses one of those two transactions, so the model must refuse too")
			case !tc.wantRefus && err != nil:
				t.Fatalf("a value-PRESERVING SET must not be modelled as a write: %v", err)
			}
			// Either way the committed model must not have resurrected X.
			if o.HasPersonName("X") {
				t.Fatal("fold resurrected a deleted node")
			}
		})
	}
}

// TestOracleTx_ValuePreservingSetStillFoldsARealChange guards the fix against
// over-reach: the value-equality short-circuit must not swallow a SET whose
// value differs, nor a SET on a node carrying no age at all (the engine's
// `!had` arm, which always records a version).
func TestOracleTx_ValuePreservingSetStillFoldsARealChange(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "X", 95)
	// A MERGE-created node carries no age, so a later SET on it is a real write.
	merge := o.BeginTx()
	if res := merge.ApplyMerge(tmplMergePerson, map[string]any{"name": "Y"}); !res.Committed {
		t.Fatalf("merge: %+v", res)
	}
	if err := merge.Commit(); err != nil {
		t.Fatalf("merge commit: %v", err)
	}

	tx := o.BeginTx()
	if res := tx.ApplyMatch(tmplSetAge, map[string]any{"name": "X", "age": int64(96)}); !res.Committed {
		t.Fatalf("set X: %+v", res)
	}
	if res := tx.ApplyMatch(tmplSetAge, map[string]any{"name": "Y", "age": int64(1)}); !res.Committed {
		t.Fatalf("set Y: %+v", res)
	}
	// A second SET back to the ORIGINAL committed value is still a pending
	// write, because the value the transaction now observes is 96, not 95.
	if res := tx.ApplyMatch(tmplSetAge, map[string]any{"name": "X", "age": int64(95)}); !res.Committed {
		t.Fatalf("set X back: %+v", res)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committedAge := func(name string) any {
		t.Helper()
		id, ok := o.byName[name]
		if !ok {
			t.Fatalf("committed model lost %q", name)
		}
		return o.nodes[id].Properties["age"]
	}
	if got := committedAge("X"); got != int64(95) {
		t.Fatalf("committed X age=%v; want 95", got)
	}
	if got := committedAge("Y"); got != int64(1) {
		t.Fatalf("committed Y age=%v; want 1", got)
	}
}

// TestOracleTx_ValuePreservingSetDoesNotClobberANewerCommit is the apply-side
// half of the rmp #2717 fix. Recording a value-preserving SET as a pending
// update made the fold WRITE that stale value over a newer one a concurrent
// transaction had committed in the meantime — silently corrupting the
// committed model, because the engine records no version for the
// value-preserving write and therefore lets the other writer through
// unconflicted and keeps ITS value.
//
// The workspace must end with the other transaction's value, not its own.
func TestOracleTx_ValuePreservingSetDoesNotClobberANewerCommit(t *testing.T) {
	o := NewGraphOracle()
	seedPerson(t, o, "X", 95)

	// A transaction re-asserts the age X already carries: not a write.
	stale := o.BeginTx()
	if res := stale.ApplyMatch(tmplSetAge, map[string]any{"name": "X", "age": int64(95)}); !res.Committed {
		t.Fatalf("stale set: %+v", res)
	}
	// A concurrent transaction changes it for real and commits first.
	fresh := o.BeginTx()
	if res := fresh.ApplyMatch(tmplSetAge, map[string]any{"name": "X", "age": int64(50)}); !res.Committed {
		t.Fatalf("fresh set: %+v", res)
	}
	if err := fresh.Commit(); err != nil {
		t.Fatalf("fresh commit: %v", err)
	}
	if err := stale.Commit(); err != nil {
		t.Fatalf("stale commit: %v", err)
	}

	id, ok := o.byName["X"]
	if !ok {
		t.Fatal("committed model lost X")
	}
	if got := o.nodes[id].Properties["age"]; got != int64(50) {
		t.Fatalf("committed X age=%v; want 50 — a value-preserving SET clobbered a newer committed value", got)
	}
}
