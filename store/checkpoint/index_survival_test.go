package checkpoint_test

// index_survival_test.go — store-direct gate for #1755, complementing the
// engine-path coverage in indexdefs_survival_test.go.
//
// An index created via the PUBLIC txn.Store-direct API (txn.Tx.CreateIndex +
// Tx.Commit) by an embedder that NEVER drives the graph through the cypher
// engine (so nothing populates the engine index-def registry / WithIndexSpecs)
// must NOT be silently dropped by a WAL-truncating checkpoint.
//
// Before the fix, the checkpoint fail-safe had no notion of indexes at all, so
// the snapshot was judged self-sufficient, TruncatePrefix discarded the
// OpCreateIndex frame, and the index def vanished on the next reopen (a
// Durability breach on committed schema DDL). After the fix, txn.Store's
// commit-apply path drives the graph's store-direct index count
// (Graph.AddStoreIndex), so Graph.HasIndexes is true and the phase-3
// self-sufficiency re-check refuses to truncate: the WAL is retained and
// recovery replays the OpCreateIndex op on top of the snapshot.
//
// Layer: short. Parallel (no process-global state).
// goleak-clean (checkpointer, store, and WAL are local and closed).

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpointer_StoreDirectIndex_Unwired_SkipsTruncation is the #1755
// store-direct gate: an index created via txn.Tx.CreateIndex by a store-direct
// embedder (no engine, no WithIndexSpecs), with the mapper side self-sufficient
// (WithMapperCodec) so the ONLY missing snapshot component is the index def.
// The checkpoint must retain the WAL (the fail-safe) and the index def must
// survive the reopen.
func TestCheckpointer_StoreDirectIndex_Unwired_SkipsTruncation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- Process 1: declare the index ENTIRELY through the store-direct API.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())

	commitStoreDirect(t, store, func(tx *txn.Tx[string, float64]) error {
		return tx.CreateIndex(txn.IndexKindHash, "Person", "email", "ix_person_email")
	})
	// Seed a node so the recovered graph the index rebuilds from is non-trivial.
	commitStoreDirect(t, store, func(tx *txn.Tx[string, float64]) error {
		if err := tx.AddNode("alice"); err != nil {
			return err
		}
		if err := tx.SetNodeLabel("alice", "Person"); err != nil {
			return err
		}
		return tx.SetNodeProperty("alice", "email", lpg.StringValue("alice@example.com"))
	})

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		// NO WithIndexSpecs — this is the #1755 store-direct misconfiguration.
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint Trigger: %v", terr)
	}
	cp.Stop()

	// Fail-safe: the WAL must NOT have been truncated (the un-persisted index def
	// is the reason the snapshot is not self-sufficient).
	if got := cp.Stats().WALTruncBytes; got != 0 {
		t.Fatalf("WAL was truncated (WALTruncBytes = %d) despite an un-persisted store-direct index — #1755 fail-safe did not engage; the index would be lost on reopen", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// --- Process 2: restart. Because the WAL was retained, recovery replays the
	// CREATE INDEX op and surfaces it via Result.Indexes.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if len(res.Indexes) == 0 {
		t.Fatalf("recovery.Result.Indexes is empty after checkpoint+restart; the store-direct index def was lost (#1755 regression)")
	}
	if res.Indexes[0].Name != "ix_person_email" || res.Indexes[0].Kind != txn.IndexKindHash ||
		res.Indexes[0].Label != "Person" || res.Indexes[0].Property != "email" {
		t.Fatalf("recovered index def = %+v, want {Name:ix_person_email Kind:Hash Label:Person Property:email}", res.Indexes[0])
	}
}
