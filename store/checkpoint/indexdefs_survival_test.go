package checkpoint_test

// indexdefs_survival_test.go — engine-path gate for #1755, complementing the
// store-direct coverage in index_survival_test.go.
//
// A secondary index created via the public CREATE INDEX path through a
// WAL-backed Cypher engine must survive a checkpoint that folds + truncates the
// WAL, then a restart. Before the fix a CREATE INDEX op lived ONLY in the WAL
// frame: the snapshot persisted no index DEFINITION, snapshotIsSelfSufficient
// never considered indexes, so a WAL-truncating checkpoint discarded the
// OpCreateIndex frame and recovery surfaced an empty Result.Indexes — the index
// was silently gone and index seeks degraded to full scans with no error (a
// Durability/Consistency breach on committed schema DDL).
//
// With WithIndexSpecs wired the checkpoint persists the index definition into
// the snapshot's indexdefs.bin component, so the WAL prefix CAN be truncated and
// the index still recovers and is usable — the efficient path.
//
// A second test pins back-compat: a snapshot written WITHOUT indexdefs.bin (the
// pre-#1755 byte layout) still recovers cleanly with an empty index set.
//
// Layer: short. Parallel (no process-global state).
// goleak-clean (checkpointer, engine, and WAL are local and closed).

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// manifestHasIndexDefs reports whether the snapshot manifest in snapDir lists
// the indexdefs.bin component.
func manifestHasIndexDefs(t *testing.T, snapDir string) bool {
	t.Helper()
	m, err := snapshot.ReadManifestFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read snapshot manifest: %v", err)
	}
	for _, f := range m.Files {
		if f.Name == snapshot.IndexDefsFile {
			return true
		}
	}
	return false
}

// TestCheckpointer_IndexSurvivesCheckpoint_WithIndexSpecs is the #1755 engine
// path gate: CREATE INDEX through a WAL-backed Cypher engine, one checkpoint
// with WithIndexSpecs wired (so the WAL prefix that declared the index IS
// truncated and the def is persisted into indexdefs.bin), then a restart must
// recover the index definition and the index must be usable.
//
// A plain `CREATE INDEX … FOR (n:Person) ON (n.email)` (no BTREE keyword) is a
// HASH index (ir.IndexTypeHash is the default), so the recovered def carries
// txn.IndexKindHash.
func TestCheckpointer_IndexSurvivesCheckpoint_WithIndexSpecs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- Process 1: create the index + seed nodes via a Cypher engine.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := csRunOne(t, eng, `CREATE INDEX ix_person_email FOR (n:Person) ON (n.email)`); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE (n:Person {email: 'alice@example.com'})`); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE (n:Person {email: 'bob@example.com'})`); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot),
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint Trigger: %v", terr)
	}
	cp.Stop()

	// The WAL prefix holding the CREATE INDEX op must actually be gone, or index
	// survival could come from WAL replay and the test would pass vacuously
	// without exercising the snapshot indexdefs.bin path. This assertion FAILS on
	// pre-fix code: without indexdefs.bin the phase-3 self-sufficiency re-check
	// refused to truncate.
	if got := cp.Stats().WALTruncBytes; got == 0 {
		t.Fatalf("checkpoint did not truncate the WAL (WALTruncBytes = 0); the indexdefs.bin path is not exercised — #1755")
	}
	// Component-level pin: the snapshot must carry the durable index def set.
	if !manifestHasIndexDefs(t, filepath.Join(dir, "snapshot")) {
		t.Fatalf("snapshot manifest lists no %s: the checkpointer dropped the index def set (#1755)", snapshot.IndexDefsFile)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// --- Process 2: restart. The index def must be recovered and usable.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if len(res.Indexes) == 0 {
		t.Fatalf("recovery.Result.Indexes is empty after checkpoint+restart; the index def was lost (#1755 regression)")
	}
	var found bool
	for i := range res.Indexes {
		if res.Indexes[i].Name == "ix_person_email" {
			found = true
			if res.Indexes[i].Label != "Person" || res.Indexes[i].Property != "email" {
				t.Fatalf("recovered index def fields wrong: got Label=%q Property=%q, want Person/email",
					res.Indexes[i].Label, res.Indexes[i].Property)
			}
			if res.Indexes[i].Kind != txn.IndexKindHash {
				t.Fatalf("recovered index Kind = %v, want IndexKindHash (default CREATE INDEX kind)", res.Indexes[i].Kind)
			}
		}
	}
	if !found {
		t.Fatalf("recovered index defs %+v do not include ix_person_email", res.Indexes)
	}

	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("reopen wal.Open: %v", err)
	}
	defer func() {
		if cerr := w2.Close(); cerr != nil {
			t.Errorf("wal.Close (reopen): %v", cerr)
		}
	}()
	store2 := txn.NewStoreWithOptions[string, float64](res.Graph, w2, csStoreOpts())
	eng2 := cypher.NewEngineWithStoreAndSchema(store2, res.Constraints, res.Indexes)

	// Usable: the engine must list the index, the planner must choose an index
	// seek for an equality predicate on the indexed property, and the query must
	// return the right seeded node.
	var listed bool
	for _, name := range eng2.ListIndexes() {
		if name == "ix_person_email" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("post-restart engine.ListIndexes() %v does not include ix_person_email; index not re-registered", eng2.ListIndexes())
	}
	plan, err := eng2.Explain(`MATCH (n:Person) WHERE n.email = 'alice@example.com' RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain after restart: %v", err)
	}
	if !strings.Contains(strings.ToLower(plan), "index") {
		t.Fatalf("post-restart plan does not use an index seek (index def not usable):\n%s", plan)
	}
	r, err := eng2.RunInTxAny(context.Background(), `MATCH (n:Person) WHERE n.email = 'alice@example.com' RETURN n.email AS e`, nil)
	if err != nil {
		t.Fatalf("query after restart: %v", err)
	}
	var rows int
	for r.Next() {
		rows++
	}
	if rerr := r.Err(); rerr != nil {
		t.Fatalf("query drain: %v", rerr)
	}
	_ = r.Close()
	if rows != 1 {
		t.Fatalf("post-restart indexed query returned %d rows, want 1", rows)
	}
}

// TestRecovery_BackCompat_SnapshotWithoutIndexDefs pins the back-compat
// contract: a snapshot written by a checkpointer that emits NO indexdefs.bin
// (the pre-#1755 byte layout, reproduced by an index-free graph with no
// WithIndexSpecs) must still recover cleanly, with an empty recovered index set
// and no error.
func TestRecovery_BackCompat_SnapshotWithoutIndexDefs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	commitStoreDirect(t, store, func(tx *txn.Tx[string, float64]) error {
		return tx.AddNode("alice")
	})

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		// No WithIndexSpecs and no indexes declared: the snapshot carries no
		// indexdefs.bin, exactly like a pre-#1755 snapshot.
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint Trigger: %v", terr)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// The snapshot must NOT carry indexdefs.bin (it reproduces the old layout).
	if manifestHasIndexDefs(t, filepath.Join(dir, "snapshot")) {
		t.Fatalf("snapshot unexpectedly carries %s for an index-free graph; back-compat byte layout broken", snapshot.IndexDefsFile)
	}

	// Recovery must succeed and surface no indexes (and no error).
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open on a snapshot without indexdefs.bin failed (back-compat broken): %v", err)
	}
	if len(res.Indexes) != 0 {
		t.Fatalf("recovery surfaced %d indexes for an index-free snapshot, want 0", len(res.Indexes))
	}
}
