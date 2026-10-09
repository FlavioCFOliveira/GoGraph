package txn_test

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Regression tests for the ACID audit of rmp #2965: [txn.Tx.Commit] fsynced its
// WAL record and only then applied the transaction in memory, where a concurrent
// writer's claim or uncommitted version could refuse it. The caller then got
// [txn.ErrCommittedNotApplied]: durable, not visible, and not retryable. Commit
// now takes its claims (applies as an uncommitted transaction) before the WAL, so
// a refusal leaves nothing logged and a logged commit is never refused.

// openClaimsStore opens a store over a fresh on-disk WAL in dir.
func openClaimsStore(t *testing.T, dir string) (*lpg.Graph[string, float64], *txn.Store[string, float64]) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return g, st
}

// TestCommit_DirectWritersNeverLeaveADurableCommitUnapplied: four direct writers
// hammer the node a store transaction writes. Before the fix 161 of 493 commits
// returned ErrCommittedNotApplied (147 of 495 in this test's pre-fix run).
// Every Commit must now either succeed or refuse retryably, and the WAL must hold
// exactly one commit marker per successful Commit.
func TestCommit_DirectWritersNeverLeaveADurableCommitUnapplied(t *testing.T) {
	dir := t.TempDir()
	g, st := openClaimsStore(t, dir)
	seed := st.Begin()
	if err := seed.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := seed.AddNode("b"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var directOK atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				var err error
				if n%2 == 0 {
					err = g.SetNodeProperty("a", "direct", lpg.StringValue("x"))
				} else {
					err = g.AddEdge("a", "b", 1)
				}
				if err == nil {
					directOK.Add(1)
				}
			}
		}()
	}
	committed, refused, notApplied := 1, 0, 0 // the seed is committed
	var other []error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tx := st.Begin()
		if err := tx.SetNodeProperty("a", "durable", lpg.StringValue("v")); err != nil {
			t.Fatal(err)
		}
		if err := tx.AddEdge("a", "b", 2); err != nil {
			t.Fatal(err)
		}
		switch err := tx.Commit(); {
		case err == nil:
			committed++
		case errors.Is(err, txn.ErrCommittedNotApplied):
			notApplied++
		case errors.Is(err, mvcc.ErrSerializationConflict):
			refused++
		default:
			other = append(other, err)
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("store commits ok=%d refused=%d committedNotApplied=%d; direct writes ok=%d",
		committed-1, refused, notApplied, directOK.Load())
	if notApplied != 0 {
		t.Errorf("DURABILITY/VISIBILITY: %d durable commits were not applied in memory", notApplied)
	}
	for _, err := range other {
		t.Errorf("unexpected commit error: %v", err)
	}
	if committed < 2 || directOK.Load() == 0 {
		t.Fatalf("vacuous run: %d store commits, %d direct writes succeeded", committed-1, directOK.Load())
	}
	if got := len(walSeqs(t, dir)); got != committed {
		t.Errorf("WAL holds %d commit markers; want %d (one per Commit that returned nil)", got, committed)
	}
}

// TestCommit_RefusedByAnOpenExplicitTransactionLogsNothing: an explicit
// transaction holds an uncommitted write on node a. A store commit writing a is
// refused retryably with nothing durable and nothing visible, and the same
// commit succeeds once the explicit transaction has ended. Before the fix the
// commit returned ErrCommittedNotApplied with its record already fsynced.
func TestCommit_RefusedByAnOpenExplicitTransactionLogsNothing(t *testing.T) {
	dir := t.TempDir()
	g, st := openClaimsStore(t, dir)
	seed := st.Begin()
	if err := seed.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	t1 := g.BeginVersionedTx()
	if err := g.Writer(t1).SetNodeProperty("a", "x", lpg.StringValue("1")); err != nil {
		t.Fatal(err)
	}
	tx := st.Begin()
	if err := tx.SetNodeProperty("a", "y", lpg.StringValue("2")); err != nil {
		t.Fatal(err)
	}
	err := tx.Commit()
	if errors.Is(err, txn.ErrCommittedNotApplied) {
		t.Fatalf("DURABILITY/VISIBILITY: %v", err)
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("commit while an explicit transaction holds a: err = %v; want a serialization conflict", err)
	}
	if got := len(walSeqs(t, dir)); got != 1 {
		t.Errorf("WAL holds %d commit markers after the refusal; want 1 (the seed only)", got)
	}
	g.EndVersionedTx(t1)
	if _, ok := g.GetNodeProperty("a", "y"); ok {
		t.Error("the refused commit's write is visible")
	}

	retry := st.Begin()
	if err := retry.SetNodeProperty("a", "y", lpg.StringValue("2")); err != nil {
		t.Fatal(err)
	}
	if err := retry.Commit(); err != nil {
		t.Fatalf("retry once the explicit transaction ended: %v", err)
	}
	if v, ok := g.GetNodeProperty("a", "y"); !ok || v != lpg.StringValue("2") {
		t.Errorf("after the retry a.y = %v, %v; want \"2\"", v, ok)
	}
	if got := len(walSeqs(t, dir)); got != 2 {
		t.Errorf("WAL holds %d commit markers after the retry; want 2", got)
	}
}
