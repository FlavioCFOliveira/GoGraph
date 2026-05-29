package txn

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

// TestIsolation_Commit_NoPartialTransactionObservable is the F3 isolation
// integration test for the txn layer (docs/isolation-design.md). It proves
// that a reader using the transaction-visibility barrier (Graph.View) never
// observes a partially-applied multi-op transaction committed via
// Tx.Commit, which now applies under Graph.ApplyAtomically.
//
// Invariant: every committed transaction sets node "a".v and node "b".v to
// the SAME value in one transaction. A reader inside Graph.View reads both
// and asserts they are equal; without transaction-atomic visibility it could
// read the new "a".v and the old "b".v. The commit count is bounded because
// each commit fsyncs the WAL; the lock-free mechanism itself is stress-tested
// without I/O in graph/lpg/TestIsolation_ApplyAtomically_View_NoPartialReads.
func TestIsolation_Commit_NoPartialTransactionObservable(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	store := NewStoreWithOptions[string, int64](g, w, Options[string, int64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewInt64WeightCodec(),
	})

	tx := store.Begin()
	mustNoErr(t, tx.SetNodeProperty("a", "v", lpg.Int64Value(0)))
	mustNoErr(t, tx.SetNodeProperty("b", "v", lpg.Int64Value(0)))
	mustNoErr(t, tx.Commit())

	const (
		commits = 300
		readers = 8
	)
	var (
		wg        sync.WaitGroup
		done      atomic.Bool
		violation atomic.Int64
		reads     atomic.Int64
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for i := int64(1); i <= commits; i++ {
			tx := store.Begin()
			if err := tx.SetNodeProperty("a", "v", lpg.Int64Value(i)); err != nil {
				violation.Add(1)
				return
			}
			if err := tx.SetNodeProperty("b", "v", lpg.Int64Value(i)); err != nil {
				violation.Add(1)
				return
			}
			if err := tx.Commit(); err != nil {
				violation.Add(1)
				return
			}
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				g.View(func() {
					va, oka := g.GetNodeProperty("a", "v")
					vb, okb := g.GetNodeProperty("b", "v")
					if !oka || !okb {
						return
					}
					ia, _ := va.Int64()
					ib, _ := vb.Int64()
					reads.Add(1)
					if ia != ib {
						violation.Add(1)
					}
				})
			}
		}()
	}

	wg.Wait()
	if v := violation.Load(); v != 0 {
		t.Fatalf("observed %d partial-transaction violations (a.v != b.v inside a pinned View)", v)
	}
	if reads.Load() == 0 {
		t.Fatal("readers never observed both properties; test did not exercise the invariant")
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tx op: %v", err)
	}
}
