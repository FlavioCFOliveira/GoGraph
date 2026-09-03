package sim

// mvcc_isolation_regression_test.go — engine-level regressions for the four
// defects the rmp #2436 isolation checkers surfaced (rmp #2445, #2446). Each
// test is the minimal deterministic reproduction distilled from a failing
// RunMVCCSessions seed, and each FAILED against the engine before its fix:
//
//   - TestMVCCRegression_DoomedRollbackRestoresAppliedDelete — seed 10:
//     an applied DETACH DELETE followed by a refused (doomed) one, rolled
//     back, re-tombstoned the victim (graph/lpg aliveBefore, rmp #2445).
//   - TestMVCCRegression_PublishedRollbackKeepsNodeVisibleToOldReaders —
//     seed 19: a voluntary rollback of a DETACH DELETE published a died+born
//     pair over the committed birth record, and a reader pinned before the
//     rollback lost the committed node (graph/lpg aliveBefore, rmp #2445).
//   - TestMVCCRegression_ConcurrentSameNodeAppendConflicts — seeds 5/14:
//     adjacency entry snapshots embed a concurrent transaction's pending arc,
//     so the second same-node append must now be refused (rmp #2445,
//     node = unit of write-write conflict for appends, user-approved).
//   - TestMVCCRegression_WriteTxViewNeverServedToOtherReaders — seed 16: the
//     engine-scoped CSR pair / edge-type-filter caches served a write
//     transaction's own-writes view to every reader (rmp #2446).
//   - TestMVCCRegression_RefusedEdgeCreateLeavesNoArc — seed 16: a CREATE
//     refused by the post-insert endpoint cross-check left the arc physically
//     in the slot, and the next committed append on the node published it
//     (rmp #2446 family; fix in graph/lpg addEdgeInfo/addEdgeHInfo).
//   - TestMVCCRegression_DetachDeleteDoesNotWipeAConcurrentAppend — seed 29:
//     the BULK edge removal `DETACH DELETE` uses took no adjacency claim, so it
//     wiped a concurrent transaction's in-flight arc and then restored it from
//     its own undo journal — a rolled-back edge surviving both rollbacks
//     (rmp #2694; fix in graph/lpg removeAllEdgesFromInfo and the cypher
//     mutator adapters).
//   - TestMVCCRegression_RefusedRetirementPhasesStrandNothing — the SYMMETRY
//     question rmp #2725 was asked to answer: a DETACH DELETE retires an arc, a
//     label, a property and the node record in four separate phases, each of
//     which can be REFUSED, so the arc leak could have a twin on any of the
//     other three (rmp #2725).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// regressionStore opens a fresh WAL-backed SimDisk store for one regression.
func regressionStore(t *testing.T) *SimStore {
	t.Helper()
	store, err := OpenSimStore(NewSimDisk(NewSeed(99), 0), simulatorStoreConfig())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// mustExecCommit runs one statement in its own committed transaction.
func mustExecCommit(t *testing.T, sess *cypher.Session, q string, params map[string]any) {
	t.Helper()
	tx, err := sess.BeginTx(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecAny(q, params); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s: %v", q, err)
	}
}

// engineCount runs a single-scalar count query through the engine's autocommit
// read path.
func engineCount(t *testing.T, eng *cypher.Engine, q string) int64 {
	t.Helper()
	res, err := eng.Run(t.Context(), q, nil)
	if err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var c int64
	if res.Next() {
		if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			c = int64(iv)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", q, err)
	}
	return c
}

// txCountScalar runs a single-scalar count query inside an open transaction.
func txCountScalar(t *testing.T, tx *cypher.ExplicitTx, q string) int64 {
	t.Helper()
	res, err := tx.ExecAny(q, nil)
	if err != nil {
		t.Fatalf("tx count %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var c int64
	if res.Next() {
		if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			c = int64(iv)
		}
	}
	return c
}

// TestMVCCRegression_DoomedRollbackRestoresAppliedDelete replays the seed-10
// shape: transaction B applies a DETACH DELETE of x (visible), is doomed by a
// second, refused-void DETACH DELETE of y (whose newest adjacency version — an
// edge committed after B began — is invisible to B), surfaces the doom at its
// next write, and rolls back. Every read path must then serve x: before the
// aliveBefore fix the abort reclaim's cancel branch re-tombstoned the node the
// undo replay had just revived, and x vanished from the unlabeled scan
// permanently.
func TestMVCCRegression_DoomedRollbackRestoresAppliedDelete(t *testing.T) {
	store := regressionStore(t)
	eng := store.Engine()
	sa := eng.NewSession()
	sb := eng.NewSession()

	mustExecCommit(t, sa, "CREATE (n:Person {name:'x', age:1})", nil)
	mustExecCommit(t, sa, "CREATE (n:Person {name:'y', age:2})", nil)

	txB, err := sb.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txB.Rollback() }()
	// y's newest adjacency version becomes invisible to B.
	mustExecCommit(t, sa, "MATCH (a:Person {name:'y'}),(b:Person {name:'y'}) CREATE (a)-[:KNOWS]->(b)", nil)

	if _, err := txB.ExecAny("MATCH (n:Person {name:'x'}) DETACH DELETE n", nil); err != nil {
		t.Fatalf("applied delete of x: %v", err)
	}
	if _, err := txB.ExecAny("MATCH (n:Person {name:'y'}) DETACH DELETE n", nil); err != nil &&
		!errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("refused delete of y: %v", err)
	}
	// The doomed-tx contract: the refused void write surfaces at the next
	// write or at COMMIT.
	if _, err := txB.ExecAny("CREATE (n:Person {name:'b1', age:3})", nil); err != nil &&
		!errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("surfacing write: %v", err)
	}
	if err := txB.Rollback(); err != nil {
		t.Fatal(err)
	}

	unlabeled := engineCount(t, eng, "MATCH (n) RETURN count(n)")
	labeled := engineCount(t, eng, "MATCH (n:Person) RETURN count(n)")
	byName := engineCount(t, eng, "MATCH (n:Person {name:'x'}) RETURN count(n)")
	if unlabeled != 2 || labeled != 2 || byName != 1 {
		t.Fatalf("aborted doomed rollback left torn state: unlabeled=%d labeled=%d byNameX=%d, want 2,2,1",
			unlabeled, labeled, byName)
	}
}

// TestMVCCRegression_PublishedRollbackKeepsNodeVisibleToOldReaders replays the
// seed-19 shape: a reader pins its snapshot, then another session DETACH
// DELETEs a committed node and voluntarily ROLLS BACK — which publishes the
// undo's revive as a died+born pair over the node's committed birth record.
// The pinned reader must keep seeing the node: before the aliveBefore fix the
// neither-visible branch fell back to the chain-level primordial flag and told
// the old reader the committed node never existed.
func TestMVCCRegression_PublishedRollbackKeepsNodeVisibleToOldReaders(t *testing.T) {
	store := regressionStore(t)
	eng := store.Engine()
	sa := eng.NewSession()
	sb := eng.NewSession()

	mustExecCommit(t, sa, "CREATE (n:Person {name:'x', age:1})", nil)

	reader, err := sb.BeginReadTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	if got := txCountScalar(t, reader, "MATCH (n:Person) RETURN count(n)"); got != 1 {
		t.Fatalf("pre-rollback read: count=%d, want 1", got)
	}

	// Voluntary (published) rollback of a DETACH DELETE of x.
	txA, err := sa.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txA.ExecAny("MATCH (n:Person {name:'x'}) DETACH DELETE n", nil); err != nil {
		t.Fatal(err)
	}
	if err := txA.Rollback(); err != nil {
		t.Fatal(err)
	}

	if got := txCountScalar(t, reader, "MATCH (n:Person) RETURN count(n)"); got != 1 {
		t.Fatalf("pinned reader lost committed node x after a published rollback: count=%d, want 1", got)
	}
	if got := engineCount(t, eng, "MATCH (n:Person {name:'x'}) RETURN count(n)"); got != 1 {
		t.Fatalf("fresh reader lost committed node x after a published rollback: count=%d, want 1", got)
	}
}

// TestMVCCRegression_ConcurrentSameNodeAppendConflicts asserts the contract
// the user chose for rmp #2445: the node is the unit of write-write conflict
// for adjacency APPENDS too. A second transaction's edge create on a node
// carrying another transaction's in-flight append stamp is refused with the
// typed serialization conflict — because adjacency entries are immutable
// snapshots that would otherwise embed the first transaction's pending arc.
func TestMVCCRegression_ConcurrentSameNodeAppendConflicts(t *testing.T) {
	store := regressionStore(t)
	eng := store.Engine()
	s1 := eng.NewSession()
	s2 := eng.NewSession()

	mustExecCommit(t, s1, "CREATE (n:Person {name:'a', age:1})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'b', age:2})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'c', age:3})", nil)

	tx1, err := s1.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback() }()
	if _, err := tx1.ExecAny("MATCH (a:Person {name:'a'}),(b:Person {name:'b'}) CREATE (a)-[:KNOWS]->(b)", nil); err != nil {
		t.Fatalf("tx1 edge: %v", err)
	}

	tx2, err := s2.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback() }()
	_, err = tx2.ExecAny("MATCH (a:Person {name:'a'}),(c:Person {name:'c'}) CREATE (a)-[:KNOWS]->(c)", nil)
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("concurrent same-node append: got %v, want ErrSerializationConflict", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := engineCount(t, eng, "MATCH ()-[r]->() RETURN count(r)"); got != 0 {
		t.Fatalf("after both rollbacks: edge count=%d, want 0", got)
	}
}

// TestMVCCRegression_WriteTxViewNeverServedToOtherReaders pins the rmp #2446
// contract: a write transaction that queries after its own edge create builds
// CSR/type-filter structures through its OWN view — which sees the pending
// arc — and the engine-scoped caches, keyed without transaction identity,
// served that private view to every other reader: committed edges lost their
// types and pending arcs became visible. A fresh reader must never observe
// the pending edge, however many typed queries the writing transaction runs
// first.
//
// This synthetic shape guards the contract; the defect's authoritative
// fail-on-unfixed reproduction is seed 16 of
// [TestMVCCSessions_IsolationGreen20Seeds], whose exact interleaving
// (pinned reader + cache primed mid-generation) is what surfaced it.
func TestMVCCRegression_WriteTxViewNeverServedToOtherReaders(t *testing.T) {
	store := regressionStore(t)
	eng := store.Engine()
	s1 := eng.NewSession()
	s2 := eng.NewSession()

	mustExecCommit(t, s1, "CREATE (n:Person {name:'a', age:1})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'b', age:2})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'c', age:3})", nil)
	mustExecCommit(t, s1, "MATCH (a:Person {name:'a'}),(b:Person {name:'b'}) CREATE (a)-[:KNOWS]->(b)", nil)

	tx, err := s2.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// The pending arc rides on a DIFFERENT source node so the append-append
	// conflict rule (also rmp #2445) does not refuse it.
	if _, err := tx.ExecAny("MATCH (b:Person {name:'b'}),(c:Person {name:'c'}) CREATE (b)-[:KNOWS]->(c)", nil); err != nil {
		t.Fatalf("pending edge: %v", err)
	}
	// The write transaction now runs typed queries through its own view —
	// exactly what primed the shared caches with its private CSR pair.
	if got := txCountScalar(t, tx, "MATCH (:Person)-[r:KNOWS]->(:Person) RETURN count(r)"); got != 2 {
		t.Fatalf("write tx own view: typed count=%d, want 2 (own pending edge visible to itself)", got)
	}

	// A fresh reader at the same instant must see ONLY the committed edge —
	// on the typed path, the by-name seek, AND the raw pattern scan (the raw
	// scan is where a cached own-writes CSR pair surfaces the pending arc as
	// a phantom).
	if got := engineCount(t, eng, "MATCH (:Person)-[r:KNOWS]->(:Person) RETURN count(r)"); got != 1 {
		t.Fatalf("fresh reader served the write tx's own-writes view: typed count=%d, want 1", got)
	}
	if got := engineCount(t, eng, "MATCH (a:Person {name:'a'})-[r:KNOWS]->(b:Person {name:'b'}) RETURN count(r)"); got != 1 {
		t.Fatalf("committed edge lost its type behind the write tx's cached view: count=%d, want 1", got)
	}
	if got := engineCount(t, eng, "MATCH (x)-[r]->(y) RETURN count(r)"); got != 1 {
		t.Fatalf("fresh reader observed the write tx's pending arc: raw count=%d, want 1", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// TestMVCCRegression_RefusedEdgeCreateLeavesNoArc replays the seed-16
// AddEdge-atomicity leak: a CREATE whose endpoint cross-check fires AFTER the
// physical insert (a pending property write by another transaction on the
// source node) must leave the adjacency exactly as it found it. Before the
// fix the refused arc stayed in the slot invisibly — no undo was recorded for
// the "failed" statement — and the NEXT committed append on the node embedded
// and published it as a phantom committed edge no statement ever created.
func TestMVCCRegression_RefusedEdgeCreateLeavesNoArc(t *testing.T) {
	store := regressionStore(t)
	eng := store.Engine()
	s1 := eng.NewSession()
	s2 := eng.NewSession()
	s3 := eng.NewSession()

	mustExecCommit(t, s1, "CREATE (n:Person {name:'a', age:1})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'b', age:2})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'c', age:3})", nil)
	mustExecCommit(t, s1, "CREATE (n:Person {name:'v', age:4})", nil)

	// Doom tx1 first: a SET refused by another transaction's pending SET.
	// The endpoints of the CREATE below carry no adjacency stamps, so the
	// pre-insert checkAppend cannot refuse a doomed transaction (it early
	// returns on a stampless node) — only the POST-insert cross-check can,
	// which is exactly the window that leaked the arc.
	txP, err := s3.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txP.Rollback() }()
	if _, err := txP.ExecAny("MATCH (n:Person {name:'v'}) SET n.age=$age", map[string]any{"age": int64(9)}); err != nil {
		t.Fatal(err)
	}
	tx1, err := s1.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx1.ExecAny("MATCH (n:Person {name:'v'}) SET n.age=$age", map[string]any{"age": int64(8)}); !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("dooming SET: got %v, want ErrSerializationConflict", err)
	}
	_, err = tx1.ExecAny("MATCH (a:Person {name:'a'}),(b:Person {name:'b'}) CREATE (a)-[:KNOWS]->(b)", nil)
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("edge create on a doomed tx: got %v, want ErrSerializationConflict", err)
	}
	if err := tx1.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := txP.Rollback(); err != nil {
		t.Fatal(err)
	}

	// A later committed append on the same source must not resurrect the
	// refused arc by embedding the dirty slot.
	tx2, err := s2.BeginTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.ExecAny("MATCH (a:Person {name:'a'}),(c:Person {name:'c'}) CREATE (a)-[:KNOWS]->(c)", nil); err != nil {
		t.Fatalf("later append: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	total := engineCount(t, eng, "MATCH ()-[r]->() RETURN count(r)")
	ab := engineCount(t, eng, "MATCH (a:Person {name:'a'})-[r:KNOWS]->(b:Person {name:'b'}) RETURN count(r)")
	ac := engineCount(t, eng, "MATCH (a:Person {name:'a'})-[r:KNOWS]->(c:Person {name:'c'}) RETURN count(r)")
	if total != 1 || ab != 0 || ac != 1 {
		t.Fatalf("refused edge create leaked an arc: total=%d a->b=%d a->c=%d, want 1,0,1", total, ab, ac)
	}
}

// TestMVCCRegression_DetachDeleteDoesNotWipeAConcurrentAppend is seed 29 of the
// multi-session mode, at the two tick counts whose terminal drain exposed it
// (rmp #2694).
//
// The drain rolls back every still-open transaction in session order, and this
// seed leaves two of them interleaved on the same node: session 0 holds an
// uncommitted `CREATE (a)-[:KNOWS]->(b)` out of `mv-s0-m2`, session 2 an
// uncommitted `DETACH DELETE` of that same node. Rolling them back in that
// order left the created edge in the graph:
//
//	[ACID_CONSISTENCY] tick=60 op="edge count": edge-count mismatch: oracle=3 engine=4
//
// The mechanism, and the four-way outcome matrix that shows it is not confined
// to rollback, are pinned at the layers that own it —
// graph/lpg TestConflict_AdjacencyBulkRemovalRefusedByConcurrentAppend and
// cypher TestMVCCDetachDeleteRollback_DoesNotResurrectPeerArc. This test is the
// end-to-end witness: it is the schedule the DST actually found, and it is what
// makes the two synthetic reproductions above answerable to a real workload.
//
// Both ticks are run because the divergence heals: the seed is clean at 55-59
// and again at 62-72, so a sweep that sampled only round numbers would miss it.
func TestMVCCRegression_DetachDeleteDoesNotWipeAConcurrentAppend(t *testing.T) {
	for _, ticks := range []int{60, 61} {
		res, err := RunMVCCSessions(context.Background(),
			MVCCSessionsConfig{Seed: 29, Ticks: ticks, Sessions: 4})
		if err != nil {
			t.Fatalf("ticks=%d: %v", ticks, err)
		}
		if !res.Clean() {
			t.Errorf("ticks=%d: violations=%v foldErrors=%v",
				ticks, res.Violations, res.FoldErrors)
		}
		// Non-vacuity: the schedule must actually have rolled transactions back
		// and committed others, or "clean" would mean "nothing happened". These
		// are the counters the defect was recorded with.
		if res.TxCommitted != 11 || res.TxRolledBack != 3 {
			t.Errorf("ticks=%d: committed=%d rolledBack=%d, want 11 and 3 — the "+
				"schedule this regression pins has changed and the test no "+
				"longer exercises the interleaving it was written for",
				ticks, res.TxCommitted, res.TxRolledBack)
		}
	}
}

// TestMVCCRegression_SplitLifePairKeepsNodeVisibleToOldReaders is the
// SPLIT-PAIR case #2445 left uncovered (rmp #2724, diagnosed in #2723).
//
// #2445's own regression above stops at the rollback, and that is exactly the
// blind spot: the life store is one record deep per direction, a published
// rollback of a DETACH DELETE leaves a died+born pair over the node's committed
// birth, and `aliveBefore(born, died) = died.seq < born.seq` reads that pair
// correctly only while BOTH halves still belong to the rolled-back transaction.
// A later, unrelated delete replaces the died half; the surviving pair then
// reads born-then-died, and every reader older than the rollback loses a node
// whose birth committed before it began.
//
// The two arms differ in ONE factor — whether the doomed delete rolls back
// before or after the reader pins its snapshot — because that factor is what
// decides whether the reader's snapshot predates the overwritten birth. The
// second delete never commits: a reader that loses the node while it is merely
// PENDING has taken a dirty read as well as a moved snapshot.
func TestMVCCRegression_SplitLifePairKeepsNodeVisibleToOldReaders(t *testing.T) {
	for _, arm := range []struct {
		name         string
		rollbackable bool // roll the doomed delete back BEFORE the reader pins
	}{
		{"doomed rollback before the reader pins", true},
		{"doomed rollback after the reader pins", false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			store := regressionStore(t)
			eng := store.Engine()
			seeder := eng.NewSession()
			mustExecCommit(t, seeder, "CREATE (n:Person {name:'x', age:1})", nil)
			mustExecCommit(t, seeder, "CREATE (n:Person {name:'y', age:2})", nil)

			doomed, err := eng.NewSession().BeginTx(t.Context())
			if err != nil {
				t.Fatalf("begin doomed: %v", err)
			}
			if _, err := doomed.ExecAny("MATCH (n:Person {name:'x'}) DETACH DELETE n", nil); err != nil {
				t.Fatalf("doomed delete: %v", err)
			}
			if arm.rollbackable {
				if err := doomed.Rollback(); err != nil {
					t.Fatalf("doomed rollback: %v", err)
				}
			}

			reader, err := eng.NewSession().BeginReadTx(t.Context())
			if err != nil {
				t.Fatalf("begin reader: %v", err)
			}
			defer func() { _ = reader.Rollback() }()
			if got := txCountScalar(t, reader, "MATCH (n:Person) RETURN count(n)"); got != 2 {
				t.Fatalf("reader at BEGIN: count=%d, want 2", got)
			}
			if !arm.rollbackable {
				if err := doomed.Rollback(); err != nil {
					t.Fatalf("doomed rollback: %v", err)
				}
			}
			if got := txCountScalar(t, reader, "MATCH (n:Person) RETURN count(n)"); got != 2 {
				t.Fatalf("reader after the doomed rollback: count=%d, want 2", got)
			}

			// The unrelated later delete. It replaces the died half of the pair
			// the rollback left behind; the reader must not notice, whether it
			// is pending or committed.
			second, err := eng.NewSession().BeginTx(t.Context())
			if err != nil {
				t.Fatalf("begin second delete: %v", err)
			}
			defer func() { _ = second.Rollback() }()
			if _, err := second.ExecAny("MATCH (n:Person {name:'x'}) DETACH DELETE n", nil); err != nil {
				t.Fatalf("second delete: %v", err)
			}
			if got := txCountScalar(t, reader, "MATCH (n:Person) RETURN count(n)"); got != 2 {
				t.Fatalf("reader lost a committed node to a PENDING unrelated delete "+
					"(dirty read): count=%d, want 2", got)
			}
			if err := second.Commit(); err != nil {
				t.Fatalf("commit second delete: %v", err)
			}
			if got := txCountScalar(t, reader, "MATCH (n:Person) RETURN count(n)"); got != 2 {
				t.Fatalf("reader lost a committed node to a COMMITTED unrelated delete "+
					"(snapshot moved): count=%d, want 2", got)
			}
		})
	}
}

// TestMVCCRegression_RefusedRetirementPhasesStrandNothing answers the symmetry
// question rmp #2725 was asked alongside the arc leak: the phases of a
// `DETACH DELETE` are structurally alike, so can the ordering that stranded an
// ARC also strand a node's PROPERTY, its LABEL, or the node record itself?
//
// It can not, and the arms below are what establishes that rather than
// inspection. Each puts a peer's PENDING write on the store the retiring phase
// must claim, so the phase is refused; then the two transactions are rolled
// back in the order that stranded the arc — peer FIRST, refused deleter SECOND
// — which is the order in which a wrongly-journalled inverse has nothing left
// in front of it.
//
// # Why the shape is present but the defect is not
//
// The journalling shape IS the same: every one of these adapters gates its
// inverse on a PRESENT-STATE probe rather than on the write having applied
// ([lpgMutatorAdapter.RemoveNodeLabel] on hadLabel, DelNodeProperty on had,
// RemoveNode on wasLive). What differs is the REPRESENTATION the inverse writes
// into. The node property, node label and node life stores all keep a per-object
// version chain, so an inverse replayed by a doomed transaction stays
// attributable to it, resolves as aborted for every reader, and is withdrawn by
// the abort machinery. The adjacency does not: an entry is an immutable snapshot
// built from the node's current slot, so a LATER transaction's entry physically
// EMBEDS whatever the slot held — including an aborted transaction's arc — and
// publishes it under its own visible instant. That asymmetry is the same one
// rmp #2445 recorded in graph/lpg/mvcc_adjversion.go, and it is why the arc
// could be laundered into a committed version and the other three cannot.
//
// So this test is a GUARD, not a reproduction: nothing here failed at
// `cd91a8bc`. It fails the day a retirement store gains a representation that
// can launder an aborted write, or an inverse starts creating an object rather
// than restoring a value.
//
// The PHYSICAL store is read as well as the engine, because that is precisely
// where the arc leak was real while every Cypher read still looked clean.
func TestMVCCRegression_RefusedRetirementPhasesStrandNothing(t *testing.T) {
	for _, arm := range []struct {
		name     string
		peer     string // A's pending statement, which claims the store
		retire   string // B's retiring statement, refused by that claim
		query    string // what the engine must report afterwards
		want     string
		wantAge  int64  // the physical property value that must survive
		wantLbls string // the physical label set that must survive
	}{
		{
			name:     "property/REMOVE",
			peer:     "MATCH (n:Person {name:'x'}) SET n.age = 99",
			retire:   "MATCH (n:Person {name:'x'}) REMOVE n.age",
			query:    "MATCH (n:Person {name:'x'}) RETURN n.age",
			want:     "[1]",
			wantAge:  1,
			wantLbls: "[Person]",
		},
		{
			name:     "label/REMOVE",
			peer:     "MATCH (n:Person {name:'x'}) SET n:Marked",
			retire:   "MATCH (n:Person {name:'x'}) REMOVE n:Person",
			query:    "MATCH (n {name:'x'}) RETURN labels(n)",
			want:     `[["Person"]]`,
			wantAge:  1,
			wantLbls: "[Person]",
		},
		{
			name:     "property/DETACH DELETE",
			peer:     "MATCH (n:Person {name:'x'}) SET n.age = 99",
			retire:   "MATCH (n:Person {name:'x'}) DETACH DELETE n",
			query:    "MATCH (n:Person {name:'x'}) RETURN n.age",
			want:     "[1]",
			wantAge:  1,
			wantLbls: "[Person]",
		},
		{
			name:     "label/DETACH DELETE",
			peer:     "MATCH (n:Person {name:'x'}) SET n:Marked",
			retire:   "MATCH (n:Person {name:'x'}) DETACH DELETE n",
			query:    "MATCH (n {name:'x'}) RETURN labels(n)",
			want:     `[["Person"]]`,
			wantAge:  1,
			wantLbls: "[Person]",
		},
		{
			name:     "node/DETACH DELETE",
			peer:     "MATCH (n:Person {name:'x'}) SET n.age = 99",
			retire:   "MATCH (n:Person {name:'x'}) DETACH DELETE n",
			query:    "MATCH (n:Person) RETURN n.name",
			want:     `["x" "y"]`,
			wantAge:  1,
			wantLbls: "[Person]",
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			store := regressionStore(t)
			eng := store.Engine()
			seeder := eng.NewSession()
			mustExecCommit(t, seeder, "CREATE (n:Person {name:'x', age:1})", nil)
			mustExecCommit(t, seeder, "CREATE (n:Person {name:'y', age:2})", nil)

			peer, err := eng.NewSession().BeginTx(t.Context())
			if err != nil {
				t.Fatalf("begin peer: %v", err)
			}
			if _, err := peer.ExecAny(arm.peer, nil); err != nil {
				t.Fatalf("peer write: %v", err)
			}
			deleter, err := eng.NewSession().BeginTx(t.Context())
			if err != nil {
				t.Fatalf("begin deleter: %v", err)
			}
			if res, err := deleter.ExecAny(arm.retire, nil); err == nil {
				for res.Next() {
				}
				_ = res.Close()
			}
			// Peer FIRST, refused deleter SECOND.
			if err := peer.Rollback(); err != nil {
				t.Fatalf("peer rollback: %v", err)
			}
			// NON-VACUITY: the deleter must actually have been REFUSED. One that
			// simply applied its retirement and undid it would exercise nothing.
			if err := deleter.Commit(); err == nil {
				t.Fatal("the retiring transaction COMMITTED; this arm does not " +
					"exercise a refused retirement phase and proves nothing")
			}

			if got := regressionRows(t, eng, arm.query); got != arm.want {
				t.Errorf("engine reports %s, want %s", got, arm.want)
			}
			key := regressionKeyOfNamed(t, store, "x")
			if v, ok := store.Graph().GetNodeProperty(key, "age"); !ok {
				t.Errorf("physical store lost x.age entirely")
			} else if n, isInt := v.Int64(); !isInt || n != arm.wantAge {
				t.Errorf("physical x.age = %v, want %d — a refused retirement "+
					"stranded a value in the store it never wrote to", v, arm.wantAge)
			}
			if got := fmt.Sprint(store.Graph().NodeLabels(key)); got != arm.wantLbls {
				t.Errorf("physical labels(x) = %s, want %s", got, arm.wantLbls)
			}
		})
	}
}

// regressionRows renders one projected column of an engine query, as a string,
// so an arm can compare against a literal.
func regressionRows(t *testing.T, eng *cypher.Engine, q string) string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		out = append(out, res.ValueAt(0).String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("query %q drain: %v", q, err)
	}
	_ = res.Close()
	return fmt.Sprint(out)
}

// regressionKeyOfNamed resolves the lpg node key carrying name, so a test can
// read the PHYSICAL stores. The Cypher layer mints its own opaque node keys, so
// the graph cannot be addressed by the workload's own names.
func regressionKeyOfNamed(t *testing.T, store *SimStore, name string) string {
	t.Helper()
	key := ""
	store.Graph().AdjList().Mapper().Walk(func(_ graph.NodeID, k string) bool {
		v, ok := store.Graph().GetNodeProperty(k, "name")
		if !ok {
			return true
		}
		if s, isStr := v.String(); isStr && s == name {
			key = k
			return false
		}
		return true
	})
	if key == "" {
		t.Fatalf("no lpg node carries name=%q", name)
	}
	return key
}
