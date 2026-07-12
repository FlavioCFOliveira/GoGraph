package checkpoint_test

// index_drop_survival_test.go — regression for F-STORE1: a DROP INDEX that has
// been folded into a WAL-truncating checkpoint must NOT be resurrected on
// restart, and a surviving index must persist. The durable-def-registry update
// is performed inside the DDL's single-writer-serialised window (before the WAL
// commit releases it), so a concurrent checkpoint can never capture a WAL
// watermark past the DROP while the def registry still lists the index —
// otherwise the stale def would land in indexdefs.bin and the truncated WAL
// would replay no DROP, resurrecting the index. This test pins the end-to-end
// durability invariant across create/drop/checkpoint/restart.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestCheckpointer_DroppedIndexNotResurrected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	// Two indexes so the surviving-index path is exercised alongside the drop.
	if err := csRunOne(t, eng, `CREATE INDEX ix_keep FOR (n:Person) ON (n.email)`); err != nil {
		t.Fatalf("CREATE INDEX ix_keep: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE INDEX ix_drop FOR (n:Person) ON (n.nickname)`); err != nil {
		t.Fatalf("CREATE INDEX ix_drop: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE (n:Person {email: 'alice@example.com', nickname: 'al'})`); err != nil {
		t.Fatalf("seed: %v", err)
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

	// Checkpoint #1: fold both CREATE INDEX ops into the snapshot.
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #1: %v", terr)
	}

	// Drop one index, then checkpoint #2 so the DROP is folded and the WAL
	// prefix truncated. A stale def captured here would resurrect ix_drop.
	if err := csRunOne(t, eng, `DROP INDEX ix_drop`); err != nil {
		t.Fatalf("DROP INDEX ix_drop: %v", err)
	}
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #2: %v", terr)
	}
	cp.Stop()

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Restart: ix_keep must survive, ix_drop must be gone.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := map[string]bool{}
	for i := range res.Indexes {
		got[res.Indexes[i].Name] = true
	}
	if !got["ix_keep"] {
		t.Errorf("surviving index ix_keep was lost across checkpoint+restart; recovered=%v", res.Indexes)
	}
	if got["ix_drop"] {
		t.Errorf("dropped index ix_drop was RESURRECTED across checkpoint+restart (#F-STORE1); recovered=%v", res.Indexes)
	}

	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("reopen wal.Open: %v", err)
	}
	defer func() { _ = w2.Close() }()
	store2 := txn.NewStoreWithOptions[string, float64](res.Graph, w2, csStoreOpts())
	eng2 := cypher.NewEngineWithStoreAndSchema(store2, res.Constraints, res.Indexes)
	listed := map[string]bool{}
	for _, name := range eng2.ListIndexes() {
		listed[name] = true
	}
	if !listed["ix_keep"] {
		t.Errorf("post-restart engine does not list ix_keep: %v", eng2.ListIndexes())
	}
	if listed["ix_drop"] {
		t.Errorf("post-restart engine lists the dropped ix_drop (resurrected): %v", eng2.ListIndexes())
	}
}

// TestCheckpointer_IndexDDLConcurrentWithCheckpoint stresses the interleaving
// the serial test above cannot: a background goroutine triggers checkpoints
// continuously while the main goroutine churns CREATE/DROP INDEX. The fix moves
// the def-registry update inside the DDL's single-writer-serialised window, so
// a checkpoint can never capture a WAL watermark past a DDL while the registry
// lags — hence after reopen the dropped probe index must be ABSENT and the
// stable keep index PRESENT, regardless of scheduling. Runs under -race in CI.
func TestCheckpointer_IndexDDLConcurrentWithCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := csRunOne(t, eng, `CREATE INDEX ix_keep FOR (n:Person) ON (n.email)`); err != nil {
		t.Fatalf("CREATE INDEX ix_keep: %v", err)
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

	// Background checkpoint pressure.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = cp.Trigger() // ErrCheckpointerStopped is fine at teardown
			}
		}
	}()

	// Churn: create then drop a probe index repeatedly, racing the checkpoints.
	const rounds = 40
	for i := 0; i < rounds; i++ {
		if err := csRunOne(t, eng, `CREATE INDEX ix_probe FOR (n:Person) ON (n.nickname)`); err != nil {
			t.Fatalf("round %d CREATE ix_probe: %v", i, err)
		}
		if err := csRunOne(t, eng, `DROP INDEX ix_probe`); err != nil {
			t.Fatalf("round %d DROP ix_probe: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()
	// One final checkpoint to fold the terminal state (ix_probe dropped).
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("final checkpoint: %v", terr)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := map[string]bool{}
	for i := range res.Indexes {
		got[res.Indexes[i].Name] = true
	}
	if !got["ix_keep"] {
		t.Errorf("ix_keep lost after concurrent checkpoint churn; recovered=%v", res.Indexes)
	}
	if got["ix_probe"] {
		t.Errorf("ix_probe RESURRECTED after concurrent checkpoint churn (#F-STORE1); recovered=%v", res.Indexes)
	}
}
