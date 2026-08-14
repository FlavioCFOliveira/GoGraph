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

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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
