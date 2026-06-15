package txn_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestGroupCommit_ConcurrentDurability is the correctness gate for group commit
// (#1507): with many goroutines committing distinct transactions concurrently
// through the coalesced-fsync path, every transaction that was acknowledged
// (Commit returned nil) MUST be durably present after the store is closed and
// the on-disk WAL is replayed by recovery — no acknowledged commit is lost, and
// no un-acknowledged frame is resurrected.
//
// Each goroutine commits transactions that AddEdge a pair of keys unique to
// (worker, iteration), so distinct transactions touch distinct keys: the
// recovered edge set must equal exactly the set of acknowledged commits. This
// exercises the leader/follower fsync coalescing, the sequence-ordered apply
// gate, and the durable-before-visible contract under real on-disk fsync.
func TestGroupCommit_ConcurrentDurability(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithCodec(g, w, txn.NewStringCodec())

	const (
		workers       = 32
		perWorker     = 50
		expectedAcked = workers * perWorker
	)

	// acked collects, per worker, the (src,dst) pairs whose Commit returned nil.
	// A successful Commit is a durability acknowledgement, so every pair here
	// must survive recovery.
	type pair struct{ src, dst string }
	ackedByWorker := make([][]pair, workers)
	var ackedCount atomic.Int64
	var firstErr atomic.Value // error

	var wg sync.WaitGroup
	wg.Add(workers)
	for wkr := 0; wkr < workers; wkr++ {
		go func(wkr int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				src := fmt.Sprintf("w%d-s%d", wkr, i)
				dst := fmt.Sprintf("w%d-d%d", wkr, i)
				tx := s.Begin()
				if aerr := tx.AddEdge(src, dst, 0); aerr != nil {
					_ = tx.Rollback()
					firstErr.CompareAndSwap(nil, aerr)
					return
				}
				if cerr := tx.Commit(); cerr != nil {
					firstErr.CompareAndSwap(nil, cerr)
					return
				}
				ackedByWorker[wkr] = append(ackedByWorker[wkr], pair{src, dst})
				ackedCount.Add(1)
			}
		}(wkr)
	}
	wg.Wait()

	if e := firstErr.Load(); e != nil {
		t.Fatalf("a concurrent commit failed: %v", e)
	}
	if got := ackedCount.Load(); got != expectedAcked {
		t.Fatalf("acknowledged commits = %d; want %d", got, expectedAcked)
	}

	// Close the WAL (flushes + fsyncs the acked tail) and replay the on-disk
	// directory through recovery. The recovered graph is built purely from the
	// durable WAL, with no reliance on the live in-memory graph.
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal.Close: %v", cerr)
	}
	res, err := recovery.Open[string, int64](dir,
		recovery.Options[string, int64]{Codec: txn.NewStringCodec()})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph

	// Every acknowledged edge must be present in the recovered graph.
	missing := 0
	for wkr := 0; wkr < workers; wkr++ {
		for _, p := range ackedByWorker[wkr] {
			if !rg.AdjList().HasEdge(p.src, p.dst) {
				missing++
				if missing <= 5 {
					t.Errorf("acknowledged edge %s->%s missing after recovery", p.src, p.dst)
				}
			}
		}
	}
	if missing > 0 {
		t.Fatalf("%d acknowledged edges lost after recovery (Durability violation)", missing)
	}

	// The recovered op count must equal the acknowledged-commit count exactly:
	// one AddEdge op per transaction. WALOps > acked would mean an
	// un-acknowledged frame was made durable (Atomicity/Durability violation);
	// WALOps < acked would mean an acknowledged commit was lost.
	if got, want := int64(res.WALOps), ackedCount.Load(); got != want {
		t.Fatalf("recovered WAL ops = %d; want %d (exactly one per acknowledged commit)", got, want)
	}
}
