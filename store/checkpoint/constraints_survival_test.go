package checkpoint_test

// constraints_survival_test.go — gate for #1334: the PRODUCTION background
// checkpointer must persist the engine's schema constraints into the
// snapshot's constraints.bin component before truncating the WAL.
//
// The pre-existing cypher/constraint_durability_test.go covers the manual
// snapshot path (calling WriteSnapshotFullWithMapperCodecAndConstraints
// directly). This test goes through checkpoint.Checkpointer.Trigger — the
// code path real deployments run — wired with WithConstraintSpecs. Before
// the fix, writeSnapshot used the constraint-unaware writers: one
// checkpoint truncated the WAL prefix holding the CREATE CONSTRAINT op, no
// constraints.bin was emitted, and after a restart the UNIQUE constraint
// was silently unenforced (a duplicate insert succeeded).
//
// Layer: short. goleak-clean (checkpointer, engines, and WAL are local and
// closed; package TestMain verifies).

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func csStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func csRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

// csRunOne runs one autocommit query and returns its terminal error (build
// error, drain error, or commit error from Close). It never calls t.Fatal so
// the caller can inspect the error of a deliberately-violating query.
func csRunOne(t *testing.T, eng *cypher.Engine, q string) error {
	t.Helper()
	r, err := eng.RunInTxAny(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for r.Next() { //nolint:revive // drain to run the write to completion
	}
	rerr := r.Err()
	if cerr := r.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	return rerr
}

// TestCheckpointer_ConstraintsSurviveCheckpointRestart drives the #1334
// acceptance criterion end-to-end through the production checkpointer:
//
//  1. Declare a UNIQUE constraint and insert one node via a WAL-backed
//     Cypher engine.
//  2. Run one checkpoint through Checkpointer.Trigger (which truncates the
//     WAL prefix that first declared the constraint).
//  3. "Restart": recovery.Open + a fresh engine re-registering the
//     recovered constraints.
//  4. The recovered constraint set must be non-empty, the snapshot manifest
//     must list constraints.bin, and a duplicate insert must be rejected
//     with exec.ErrConstraintViolation while a distinct value is accepted.
func TestCheckpointer_ConstraintsSurviveCheckpointRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- Process 1: declare the constraint, insert one node, checkpoint.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := csRunOne(t, eng, `CREATE CONSTRAINT u_name ON (n:Person) ASSERT n.name IS UNIQUE`); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE (n:Person {name: 'alice'})`); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint Trigger: %v", terr)
	}
	cp.Stop()
	// The WAL prefix holding the CREATE CONSTRAINT op must actually be gone;
	// otherwise constraint survival could come from WAL replay and the test
	// would pass vacuously without exercising the snapshot path.
	if got := cp.Stats().WALTruncBytes; got == 0 {
		t.Fatalf("checkpoint did not truncate the WAL (WALTruncBytes = 0); the test cannot exercise the snapshot-only constraint path")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Component-level pin: the production checkpointer's snapshot must carry
	// the durable constraint set.
	m, err := snapshot.ReadManifestFile(filepath.Join(dir, "snapshot", "manifest.json"))
	if err != nil {
		t.Fatalf("read snapshot manifest: %v", err)
	}
	hasConstraints := false
	for _, f := range m.Files {
		if f.Name == snapshot.ConstraintsFile {
			hasConstraints = true
			break
		}
	}
	if !hasConstraints {
		t.Fatalf("snapshot manifest lists no %s: the checkpointer dropped the constraint set (files=%+v)", snapshot.ConstraintsFile, m.Files)
	}

	// --- Process 2: restart from disk and assert the constraint is enforced.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if len(res.Constraints) == 0 {
		t.Fatalf("recovery.Result.Constraints is empty after checkpoint+restart; the UNIQUE constraint was lost")
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
	eng2 := cypher.NewEngineWithStoreAndConstraints(store2, res.Constraints)

	dupErr := csRunOne(t, eng2, `CREATE (n:Person {name: 'alice'})`)
	if dupErr == nil {
		t.Fatalf("duplicate insert after checkpoint+restart was accepted; the UNIQUE constraint did not survive the checkpoint")
	}
	if !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("duplicate insert after checkpoint+restart: got %v, want one wrapping exec.ErrConstraintViolation", dupErr)
	}
	// A genuinely new value must still be accepted — enforcement, not a
	// blanket reject.
	if err := csRunOne(t, eng2, `CREATE (n:Person {name: 'bob'})`); err != nil {
		t.Fatalf("distinct insert after checkpoint+restart rejected: %v", err)
	}
}
