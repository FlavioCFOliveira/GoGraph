package checkpoint_test

// constraint_drop_survival_test.go — regression for the CONSTRAINT counterpart
// of the DROP-not-resurrected invariant (rmp #1920): a DROP CONSTRAINT folded
// into a WAL-truncating checkpoint must NOT be resurrected on restart, and a
// surviving constraint must persist. The constraint DROP path unregisters
// before the WAL commit (the opposite ordering to the historical INDEX DROP
// bug), so it is provably safe; this test pins that end-to-end across
// create/drop/checkpoint/restart. The INDEX side has index_drop_survival_test.go;
// the constraint side previously had only the CREATE-survival test.

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

func TestCheckpointer_DroppedConstraintNotResurrected(t *testing.T) {
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

	// Two UNIQUE constraints so the surviving path is exercised alongside the drop.
	if err := csRunOne(t, eng, `CREATE CONSTRAINT c_keep FOR (n:Person) REQUIRE n.email IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_keep: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE CONSTRAINT c_drop FOR (n:Person) REQUIRE n.nickname IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_drop: %v", err)
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

	// Checkpoint #1: fold both CREATE CONSTRAINT ops into the snapshot.
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #1: %v", terr)
	}

	// Drop one, then checkpoint #2 so the DROP is folded and the WAL prefix
	// truncated. A stale spec captured here would resurrect c_drop on restart.
	if err := csRunOne(t, eng, `DROP CONSTRAINT c_drop`); err != nil {
		t.Fatalf("DROP c_drop: %v", err)
	}
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #2: %v", terr)
	}
	cp.Stop()

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Restart: c_keep must survive, c_drop must be gone.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := map[string]bool{}
	for i := range res.Constraints {
		got[res.Constraints[i].Name] = true
	}
	if !got["c_keep"] {
		t.Errorf("surviving constraint c_keep was lost across checkpoint+restart; recovered=%v", res.Constraints)
	}
	if got["c_drop"] {
		t.Errorf("dropped constraint c_drop was RESURRECTED across checkpoint+restart; recovered=%v", res.Constraints)
	}

	// And the recovered engine must enforce only c_keep: a duplicate email is
	// rejected, a duplicate nickname (c_drop dropped) is accepted.
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("reopen wal.Open: %v", err)
	}
	defer func() { _ = w2.Close() }()
	store2 := txn.NewStoreWithOptions[string, float64](res.Graph, w2, csStoreOpts())
	eng2 := cypher.NewEngineWithStoreAndSchema(store2, res.Constraints, res.Indexes)

	if err := csRunOne(t, eng2, `CREATE (:Person {email: 'a@b.com', nickname: 'al'})`); err != nil {
		t.Fatalf("seed after restart: %v", err)
	}
	// c_keep still bites on email.
	if err := csRunOne(t, eng2, `CREATE (:Person {email: 'a@b.com'})`); err == nil {
		t.Error("c_keep must still enforce UNIQUE email after restart, but the duplicate was accepted")
	}
	// c_drop is gone: duplicate nickname is accepted.
	if err := csRunOne(t, eng2, `CREATE (:Person {nickname: 'al'})`); err != nil {
		t.Errorf("dropped c_drop must no longer enforce UNIQUE nickname, but got: %v", err)
	}
}
