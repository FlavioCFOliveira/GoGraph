package txn_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
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
					cerr = tx.CommitWALOnly(0)
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

// TestApplyGate_ADurableCommitIsNeverRefusedByConflictDetection is the property the
// apply gate exists to provide, and it is the answer to rmp #2306's instruction to
// PROVE the gate removable rather than assume it. The answer is a REFUTATION.
//
// # What the premise assumed, and what is actually protected
//
// The task states: "under MVCC the ordering it protected comes from the commit
// timestamp instead, so it should be removable". The commit timestamp orders
// VISIBILITY, and MVCC does deliver that without the gate. It does not order the
// APPLY — and the apply is where the stored value is decided.
//
// # The measurement
//
// With waitApplyTurn disabled and eight committers writing the SAME node property,
// Tx.Commit returned, for genuinely committed work:
//
//	txn: transaction committed durably but in-memory apply failed; recovery will
//	reconcile: mvcc: serialization conflict in node properties: the newest version is
//	not visible to this transaction
//
// Two of 320 commits, reproducibly. That is txn.ErrCommittedNotApplied wrapping a
// serialization conflict for a transaction whose frames and OpCommit marker are ALREADY
// FSYNCED: the WAL says committed while the in-memory apply refuses it, and the client
// has no action available — retrying would duplicate work that is not undoable.
//
// The state does NOT corrupt. A replay of the same WAL agreed with the live graph in
// every run, because recovery replays in WAL order and reconciles. Worth stating
// precisely: the hazard is an unactionable error and a temporarily stale in-memory
// view, not a lost write.
//
// # Why detection is the wrong mechanism here
//
// The apply REPLAYS work that is already durable and already serialised — the WAL
// sequence IS the serialisation order. Detection exists to decide which of two
// concurrent transactions wins, and that was decided when the frames were appended.
// Re-asking after the fact can only produce an answer nobody can act on. Recovery,
// which replays the same ops, is not subject to detection for exactly this reason.
//
// # What would make the gate removable
//
// Two changes together, and both are construction rather than deletion:
//
//  1. Exempt the apply from conflict detection, as recovery already is.
//  2. Replace GLOBAL sequencing with PER-OBJECT sequencing. The exemption alone is not
//     enough: with applies unordered, two same-object transactions race and the last
//     writer wins arbitrarily, which need not match WAL order — and then a live graph
//     and its own recovered image CAN diverge, which the present agreement rules out.
//     Disjoint objects may apply freely; same-object writes must follow the WAL.
//
// The assertion is phrased as "no durable commit is refused" rather than "the gate
// exists", so a future design that removes the gate and satisfies both points passes it
// unchanged.
func TestApplyGate_ADurableCommitIsNeverRefusedByConflictDetection(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	store := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	seed := store.Begin()
	if e := seed.SetNodeProperty("n", "v", lpg.Int64Value(0)); e != nil {
		t.Fatalf("seed: %v", e)
	}
	if e := seed.Commit(); e != nil {
		t.Fatalf("seed commit: %v", e)
	}

	// Every writer targets the SAME property, so every pair is a candidate collision.
	// Eight and forty are what reproduced the refusal against the ungated build; a
	// TWO-goroutine probe reported zero and would have "proved" the gate removable,
	// which is why the shape is recorded here rather than left to be re-derived.
	const (
		writers = 8
		rounds  = 40
	)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		refusals  []error
		otherErrs []error
	)
	for k := 0; k < writers; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				tx := store.Begin()
				if e := tx.SetNodeProperty("n", "v", lpg.Int64Value(int64(k*1000+r))); e != nil {
					mu.Lock()
					otherErrs = append(otherErrs, e)
					mu.Unlock()
					return
				}
				e := tx.Commit()
				if e == nil {
					continue
				}
				mu.Lock()
				if errors.Is(e, txn.ErrCommittedNotApplied) || errors.Is(e, mvcc.ErrSerializationConflict) {
					refusals = append(refusals, e)
				} else {
					otherErrs = append(otherErrs, e)
				}
				mu.Unlock()
				return
			}
		}(k)
	}
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("unexpected commit error: %v", e)
	}
	if len(refusals) != 0 {
		t.Fatalf("%d of %d durable commits were REFUSED at in-memory apply time; first: %v.\n"+
			"Their frames and OpCommit markers are already fsynced, so the WAL says committed "+
			"while memory says no and the client has no action available. The apply REPLAYS "+
			"already-serialised work — the WAL sequence IS the serialisation order — so it must "+
			"not be re-decided by conflict detection. See this test's comment for what removing "+
			"the apply gate requires (rmp #2306).",
			len(refusals), writers*rounds, refusals[0])
	}
}
