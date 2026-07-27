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

// TestApplyGate_NoLostWakeup_MixedCommitPaths is the regression guard for the
// sequence-ordered apply gate's one-waiter handoff.
//
// The gate used to wake every parked committer with a sync.Cond Broadcast on
// each commit; it now wakes EXACTLY the holder of seq+1 through a per-sequence
// parking slot. That makes the handoff O(1) instead of O(N), but it moves the
// gate from "wake everyone, let each re-check" to "wake precisely one", which
// introduces two ways to wedge the store permanently:
//
//   - A LOST WAKEUP. A committer registers its slot under applyMu only after
//     observing appliedSeq != seq-1 under that same lock. If the two steps
//     could interleave with a predecessor's advance, the successor would park
//     with nobody left to wake it and every higher sequence would queue behind
//     it forever.
//   - A GAP IN THE CHAIN. Sequences are dense, so a sequence whose turn is
//     taken but never advanced wedges all its successors. Both [Tx.Commit] and
//     [Tx.CommitWALOnly] mint and consume sequences, and CommitWALOnly advances
//     the gate while applying nothing, so the two paths must interleave without
//     leaving a hole.
//
// Either defect manifests as a permanent hang, which the test surfaces as a
// timeout rather than a wrong answer. Both commit paths are driven concurrently
// with far more writers than cores, so committers really do park.
//
// It also re-asserts the durability contract the gate must not weaken: every
// acknowledged commit — by either path — is present after the WAL is replayed.
func TestApplyGate_NoLostWakeup_MixedCommitPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithCodec(g, w, txn.NewStringCodec())

	// Deliberately far more writers than cores, so the apply gate is genuinely
	// contended and committers park rather than taking the fast path.
	const (
		workers   = 256
		perWorker = 8
	)

	type pair struct{ src, dst string }
	ackedByWorker := make([][]pair, workers)
	var acked atomic.Int64
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
				// Alternate the two paths that consume a sequence and advance
				// the gate. CommitWALOnly applies nothing in memory but must
				// still take its turn, so a chain gap would wedge the store.
				var cerr error
				if i%2 == 0 {
					cerr = tx.Commit()
				} else {
					cerr = tx.CommitWALOnly()
				}
				if cerr != nil {
					firstErr.CompareAndSwap(nil, cerr)
					return
				}
				ackedByWorker[wkr] = append(ackedByWorker[wkr], pair{src, dst})
				acked.Add(1)
			}
		}(wkr)
	}
	wg.Wait()

	if e := firstErr.Load(); e != nil {
		t.Fatalf("a concurrent commit failed: %v", e)
	}
	if got, want := acked.Load(), int64(workers*perWorker); got != want {
		t.Fatalf("acknowledged commits = %d; want %d", got, want)
	}

	// Durability: replay the on-disk WAL and require every acknowledged commit,
	// from either path, to be present.
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal.Close: %v", cerr)
	}
	res, err := recovery.Open[string, int64](dir,
		recovery.Options[string, int64]{Codec: txn.NewStringCodec()})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}

	missing := 0
	for wkr := 0; wkr < workers; wkr++ {
		for _, p := range ackedByWorker[wkr] {
			if !res.Graph.AdjList().HasEdge(p.src, p.dst) {
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
	if got, want := int64(res.WALOps), acked.Load(); got != want {
		t.Fatalf("recovered WAL ops = %d; want %d (exactly one per acknowledged commit)", got, want)
	}
}

// TestApplyGate_SingleWriterTakesFastPath asserts the gate adds no parking on
// the uncontended path: a lone committer always finds appliedSeq == seq-1 and
// returns without registering a slot.
//
// This is the acceptance criterion that the O(1) handoff must not be paid for
// with single-writer latency. It is observable rather than asserted: a
// serialised sequence of commits must leave no residual parking slots behind,
// which is only true if every one of them took the fast path.
func TestApplyGate_SingleWriterTakesFastPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithCodec(g, w, txn.NewStringCodec())

	for i := 0; i < 32; i++ {
		tx := s.Begin()
		if aerr := tx.AddEdge(fmt.Sprintf("s%d", i), fmt.Sprintf("d%d", i), 0); aerr != nil {
			t.Fatalf("AddEdge: %v", aerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			t.Fatalf("Commit(%d): %v", i, cerr)
		}
	}
	if got := s.ApplyWaiterCountForTest(); got != 0 {
		t.Fatalf("apply-gate parking slots still registered = %d; want 0 "+
			"(a single writer must never park on the gate)", got)
	}
}
