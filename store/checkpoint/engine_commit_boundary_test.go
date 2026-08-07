package checkpoint

// engine_commit_boundary_test.go — regression test for rmp #2349, an ACID
// DURABILITY defect: an acknowledged commit was lost when a checkpoint landed in
// the window between a transaction's fsync and its MVCC publish.
//
// Layer: short.
//
// # The defect
//
// Phase 1 takes two readings under the commit lock — the WAL DURABLE offset, which
// is the prefix phase 3 truncates, and an MVCC VISIBILITY instant, which is what the
// image is read at. checkpoint.go argued they always describe the same set of
// transactions because the commit serialiser drains admitted writers to zero and a
// writer stays admitted through its publish, citing store/txn.Tx.Commit deferring
// exitWriter past ApplyVersioned.
//
// THAT PREMISE WAS CHECKED ON THE WRONG PATH. The Cypher engine — the only
// production writer — commits through Tx.CommitWALOnly, which performs no in-memory
// apply and publishes no instant; its `defer exitWriter` fires the moment the fsync
// returns, while the instant is published later, when the lpg write bracket unwinds
// through Graph.endWrite (cypher/api.go commitUnderBarrier, which even names the
// state: "DURABLE, BUT NOT YET VISIBLE"). Between those two points the store's
// in-flight writer count is ZERO and the frame is already inside the durable offset.
// So the drain completed, the watermark included the commit, the instant excluded
// it, the image omitted it, and phase 3 truncated away its only record.
//
// It was found by rmp #2347's load-based search: internal/sim ST3 at seed 0xBADF00D
// under coverage with four fsync-heavy durable packages looped in parallel produced
// 2 failures in 15 iterations, each losing exactly one acknowledged commit.
//
// # What this test does that the existing boundary test cannot
//
// TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary asserts the same
// invariant and is careful to prove its oracle non-vacuous — but every one of its
// writers commits through st.Begin()/tx.Commit(), the ONE path on which the
// invariant genuinely held. It can therefore never observe this defect. This test
// reproduces the ENGINE's ordering instead: the transaction is opened outside the
// bracket, the mutation applied eagerly inside it, CommitWALOnly called inside it,
// and the bracket unwound afterwards — with the writer PARKED in the window so the
// interleaving is deterministic rather than rare.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
)

// TestCheckpoint_EngineCommitOrdering_KeepsAnAckedCommit parks a transaction in the
// post-fsync-pre-publish window, runs a checkpoint against it, and asserts BOTH the
// invariant and its observable consequence:
//
//  1. INVARIANT. InFlightCommits is zero when phase 1 takes its two readings. The
//     parked transaction holds an allocated, unpublished instant, so a non-zero
//     reading here is the defect itself rather than a proxy for it.
//
//  2. CONSEQUENCE. After the checkpoint has published its snapshot and truncated the
//     WAL prefix, a fresh recovery from the artefact on disk still contains the
//     parked transaction's edge. This is the ACID Durability assertion: the commit
//     was acknowledged (CommitWALOnly returned nil), so losing it is unrecoverable.
//
// # How the interleaving is made deterministic without a sleep
//
// The parked writer is released by whichever of two things happens first, and each
// one is the signature of one of the two possible orderings:
//
//   - afterWatermarkHook fires. The readings were taken WITH the transaction still in
//     the window — the defective ordering. The writer is released so the test can
//     finish and report the loss.
//   - the graph reports a caller blocked on the frontier (MVCCStats.SessionsWaiting).
//     That is phase 1 waiting for this transaction to publish — the fixed ordering.
//     The writer is released so the wait can complete.
//
// Neither branch is a timeout, so the passing path costs nothing and the failing
// path is not a race. The deadline below exists only so a build in which NEITHER
// signal arrives fails loudly instead of hanging.
func TestCheckpoint_EngineCommitOrdering_KeepsAnAckedCommit(t *testing.T) {
	t.Parallel()
	dir, g, st, w, cp := newPairStore(t)
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	// A few ordinary transactions first, so the snapshot the checkpoint publishes is
	// real work and the WAL has a prefix worth truncating.
	const priorTxns = 8
	for n := 0; n < priorTxns; n++ {
		tx := st.Begin()
		if err := tx.AddEdge(fmt.Sprintf("pre-a%d", n), fmt.Sprintf("pre-b%d", n), 0); err != nil {
			t.Fatalf("prior txn %d: AddEdge: %v", n, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("prior txn %d: Commit: %v", n, err)
		}
	}

	var (
		// inWindowMax is the largest InFlightCommits reading taken inside the phase-1
		// locked window. The parked transaction makes a correct reading of zero a fact
		// about the ORDERING rather than about an idle counter.
		inWindowMax   atomic.Uint64
		windowSamples atomic.Int64
		// releasedBy records which signal let the parked writer go, so the run says
		// which ordering it exercised rather than leaving it to be inferred.
		releasedBy atomic.Value
	)
	hookFired := make(chan struct{})
	var hookOnce atomic.Bool
	cp.afterWatermarkHook = func() {
		s := g.MVCCStats()
		if n := s.InFlightCommits; n > inWindowMax.Load() {
			inWindowMax.Store(n)
		}
		windowSamples.Add(1)
		if hookOnce.CompareAndSwap(false, true) {
			close(hookFired)
		}
	}

	// THE ENGINE'S COMMIT SHAPE, driven by hand so the window can be held open:
	// transaction opened outside the bracket, mutation applied eagerly inside it,
	// CommitWALOnly inside it, bracket unwound (which publishes) afterwards.
	const lateSrc, lateDst = "late-a", "late-b"
	inWindow := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		tx := st.Begin()
		if err := tx.AddEdge(lateSrc, lateDst, 0); err != nil {
			_ = tx.Rollback()
			writerDone <- fmt.Errorf("late txn AddEdge: %w", err)
			return
		}
		var commitErr error
		bracketErr := g.ApplyVersionedCtx(context.Background(), func(wtx lpg.WriteTx) error {
			// Eager apply, exactly as the engine's mutators do: the graph carries the
			// write before the WAL does.
			if err := g.Writer(wtx).AddEdge(lateSrc, lateDst, 0); err != nil {
				return err
			}
			// Allocate the instant, then fsync the record carrying it. On return the
			// transaction is DURABLE and its in-flight writer registration is already
			// gone — but the instant is not published until this closure returns.
			if err := tx.CommitWALOnly(g.AllocateCommitTS(wtx)); err != nil {
				commitErr = err
				return err
			}
			close(inWindow)
			<-release
			return nil
		})
		if commitErr != nil {
			writerDone <- fmt.Errorf("late txn CommitWALOnly: %w", commitErr)
			return
		}
		if bracketErr != nil {
			writerDone <- fmt.Errorf("late txn bracket: %w", bracketErr)
			return
		}
		writerDone <- nil
	}()

	select {
	case <-inWindow:
	case err := <-writerDone:
		t.Fatalf("the late transaction never reached the window: %v", err)
	}
	// The commit is acknowledged from here on. Anything that loses it is a durability
	// violation, not a scheduling accident.

	// Release the writer on whichever signal arrives; see the doc comment.
	go func() {
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		poll := time.NewTicker(time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case <-hookFired:
				releasedBy.Store("afterWatermarkHook (phase 1 read its positions with the " +
					"transaction still unpublished)")
				close(release)
				return
			case <-poll.C:
				if g.MVCCStats().SessionsWaiting > 0 {
					releasedBy.Store("phase 1 blocked on the commit frontier")
					close(release)
					return
				}
			case <-deadline.C:
				releasedBy.Store("DEADLINE — neither signal arrived")
				close(release)
				return
			}
		}
	}()

	if err := cp.Trigger(); err != nil {
		<-writerDone
		t.Fatalf("checkpoint: %v", err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("late writer: %v", err)
	}

	if windowSamples.Load() == 0 {
		t.Fatal("the phase-1 seam never fired: this test sampled nothing")
	}
	if s, _ := releasedBy.Load().(string); s == "" {
		t.Fatal("the parked writer was never released by a recorded signal")
	} else {
		t.Logf("parked writer released by: %s", s)
	}

	if n := inWindowMax.Load(); n != 0 {
		t.Errorf("INVARIANT VIOLATED: %d commit(s) were durable but unpublished when phase 1 "+
			"took the watermark and the instant. The watermark is a DURABILITY position and "+
			"the instant is a VISIBILITY position; a transaction between its fsync and its "+
			"publish is below the first and above the second, so the image cannot carry it "+
			"and phase 3 truncates away its only record. Draining admitted writers is NOT "+
			"enough: Tx.CommitWALOnly releases its writer registration the moment the fsync "+
			"returns and the instant is published later, at bracket unwind. Phase 1 must "+
			"wait the window out (Checkpointer.awaitCommitQuiescence, rmp #2349)", n)
	}

	// THE CONSEQUENCE. Recover from the artefact on disk — snapshot plus whatever WAL
	// survived the truncation — and look for the acknowledged edge.
	cp.Stop()
	if err := w.Sync(); err != nil {
		t.Fatalf("WAL Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("WAL Close: %v", err)
	}
	rec, err := recovery.Open[string, int64](dir, capAtomicRecOpts())
	if err != nil {
		t.Fatalf("snapshot+WAL recovery: %v", err)
	}
	if !rec.SnapshotHit {
		t.Fatal("snapshot+WAL recovery: SnapshotHit = false — the checkpoint published nothing, " +
			"so this test would pass on the WAL alone and prove nothing about truncation")
	}
	adj := rec.Graph.AdjList()
	mapper := adj.Mapper()
	srcID, okSrc := mapper.Lookup(lateSrc)
	dstID, okDst := mapper.Lookup(lateDst)
	found := false
	if okSrc && okDst {
		nbrs, _ := adj.LoadEntry(srcID)
		for _, d := range nbrs {
			if d == dstID {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("ACID DURABILITY VIOLATED: the edge %q->%q was acknowledged (CommitWALOnly "+
			"returned nil) and is ABSENT after recovery from the checkpointed artefact. "+
			"Order=%d Size=%d, endpoints interned: src=%v dst=%v. A checkpoint truncated "+
			"the WAL prefix holding the only record of an acknowledged commit",
			lateSrc, lateDst, adj.Order(), adj.Size(), okSrc, okDst)
	}
	// Every transaction in this test contributes exactly two fresh nodes and one edge,
	// so the absolute oracle applies to the recovered graph as a whole.
	assertPairInvariant(t, "recovered", rec.Graph)
	if want := uint64(priorTxns + 1); adj.Size() != want {
		t.Fatalf("recovered %d edges, want %d: a transaction other than the parked one was "+
			"lost or duplicated", adj.Size(), want)
	}
	t.Logf("%d phase-1 window(s) sampled; recovered Order=%d Size=%d with the acknowledged "+
		"late commit present", windowSamples.Load(), adj.Order(), adj.Size())
}
