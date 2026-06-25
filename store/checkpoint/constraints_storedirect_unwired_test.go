package checkpoint_test

// constraints_storedirect_unwired_test.go — gate for #1756: a UNIQUE
// constraint declared through the PUBLIC txn.Store-direct API
// (txn.Tx.CreateConstraint + Tx.Commit), by an embedder that NEVER drives the
// graph through the cypher engine (so nothing calls SetActiveConstraintCount)
// and does NOT wire checkpoint.WithConstraintSpecs, must NOT be silently
// dropped by a WAL-truncating checkpoint.
//
// Before the fix, the checkpoint fail-safe sourced needConstraints from
// Graph.HasConstraints, which was driven ONLY by the engine's
// SetActiveConstraintCount. A store-direct embedder left HasConstraints false,
// so the snapshot was wrongly judged self-sufficient, TruncatePrefix discarded
// the OpCreateConstraint frame, no constraints.bin was written, and the
// constraint vanished on the next reopen — a Consistency breach (duplicate keys
// then accepted). After the fix, txn.Store's commit-apply path drives the
// graph's store-direct constraint count (Graph.AddStoreConstraint), so
// HasConstraints is true and the existing #1464 fail-safe engages: the WAL is
// retained and recovery replays the constraint op on top of the snapshot.
//
// Layer: short. NOT parallel: it installs a process-global metrics backend.
// goleak-clean (checkpointer, store, engine, and WAL are local and closed).

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
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// commitStoreDirect runs fn against a fresh store transaction and commits it
// through the FULL apply path (Tx.Commit, not CommitWALOnly), so the in-memory
// apply of a schema-DDL op runs — the path that drives the store-direct
// constraint count.
func commitStoreDirect(t *testing.T, store *txn.Store[string, float64], fn func(tx *txn.Tx[string, float64]) error) {
	t.Helper()
	tx := store.Begin()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("buffer store-direct op: %v", err)
	}
	if cerr := tx.Commit(); cerr != nil {
		t.Fatalf("store-direct Commit: %v", cerr)
	}
}

// TestCheckpointer_StoreDirectConstraint_Unwired_SkipsTruncation is the #1756
// gate: a constraint declared via txn.Tx.CreateConstraint by a store-direct
// embedder (no engine, no SetActiveConstraintCount), with WithConstraintSpecs
// NOT wired and the mapper side self-sufficient (WithMapperCodec), so the ONLY
// missing snapshot component is constraints.bin. The checkpoint must retain the
// WAL and the constraint must survive the reopen.
func TestCheckpointer_StoreDirectConstraint_Unwired_SkipsTruncation(t *testing.T) {
	// Not parallel: process-global metrics backend.
	mb := &countingMetrics{counter: map[string]uint64{}}
	metrics.SetBackend(mb)
	defer metrics.SetBackend(nil)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- Process 1: declare the constraint and seed one node ENTIRELY through
	// the store-direct API. No cypher engine ever touches this graph, so nothing
	// calls SetActiveConstraintCount.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())

	// Declare UNIQUE (Person).name via the public txn.Tx API + full Commit.
	commitStoreDirect(t, store, func(tx *txn.Tx[string, float64]) error {
		return tx.CreateConstraint(txn.ConstraintUnique, "Person", "name", "u_name")
	})
	// Seed one node so the post-recovery engine's UNIQUE value-set is reseeded
	// from real data and a duplicate is provably rejected.
	commitStoreDirect(t, store, func(tx *txn.Tx[string, float64]) error {
		if err := tx.AddNode("alice"); err != nil {
			return err
		}
		if err := tx.SetNodeLabel("alice", "Person"); err != nil {
			return err
		}
		return tx.SetNodeProperty("alice", "name", lpg.StringValue("alice"))
	})

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		// NO WithConstraintSpecs — this is the #1756 misconfiguration.
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint Trigger: %v", terr)
	}
	cp.Stop()

	// Fail-safe: the WAL must NOT have been truncated (the un-persisted
	// constraint is the reason the snapshot is not self-sufficient), and the
	// degrade must be surfaced via the metric.
	if got := cp.Stats().WALTruncBytes; got != 0 {
		t.Fatalf("WAL was truncated (WALTruncBytes = %d) despite an un-persisted store-direct constraint — #1756 fail-safe did not engage; the constraint would be lost on reopen", got)
	}
	if got := mb.count("store.checkpoint.truncate_skipped_not_self_sufficient"); got != 1 {
		t.Fatalf("truncate-skipped metric = %d, want 1 — the degraded mode was not surfaced", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// --- Process 2: restart from disk. Because the WAL was retained, recovery
	// replays the CREATE CONSTRAINT op and surfaces it via Result.Constraints.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if len(res.Constraints) == 0 {
		t.Fatalf("recovery.Result.Constraints is empty after checkpoint+restart; the store-direct UNIQUE constraint was lost (#1756 regression)")
	}

	// Enforcement check: wire an engine from the recovered constraint set and
	// confirm a duplicate is rejected while a distinct value is accepted. (The
	// acceptance criterion permits observing enforcement through an engine; the
	// store layer itself holds no constraint registry.)
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
		t.Fatalf("duplicate insert after checkpoint+restart was accepted; the store-direct UNIQUE constraint did not survive (#1756)")
	}
	if !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("duplicate insert after checkpoint+restart: got %v, want one wrapping exec.ErrConstraintViolation", dupErr)
	}
	if err := csRunOne(t, eng2, `CREATE (n:Person {name: 'bob'})`); err != nil {
		t.Fatalf("distinct insert after checkpoint+restart rejected: %v", err)
	}
}
