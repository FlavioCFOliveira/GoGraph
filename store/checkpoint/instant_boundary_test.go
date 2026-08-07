package checkpoint

// instant_boundary_test.go — rmp #2310: the two invariants that let a checkpoint
// run at a transactional instant while writers commit.
//
// Layer: short.
//
// Phase 1 no longer excludes writers for the duration of the capture; it holds the
// commit lock only long enough to take two O(1) readings — the durable WAL offset
// and the MVCC instant — and then serialises the whole image outside the lock. Two
// properties make that safe, and neither is visible in the artefact the other
// checkpoint tests inspect, so both are asserted directly here through white-box
// seams:
//
//   - THE BOUNDARY (AC3). The watermark is a DURABILITY position and the instant is
//     a VISIBILITY position. If a transaction could be durable below the watermark
//     while its commit instant was still unpublished, the image would not carry it
//     and phase 3 would truncate away its only record — an acknowledged commit lost.
//     The commit serialiser drains admitted writers to zero, and a writer stays
//     admitted through its MVCC publish, so no such transaction exists at that point.
//
//   - THE HORIZON (AC5). An open snapshot pins reclamation. The capture's snapshot
//     must be released when the bytes exist and BEFORE phase 2's disk I/O, so the
//     horizon is pinned for a bounded, memory-only window rather than across a
//     multi-second snapshot write.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary asserts AC3's mechanism:
// at the moment phase 1 takes the durable-offset watermark and the MVCC instant, no
// transaction is between its fsync and its commit publish.
//
// # IT COVERS ONE COMMIT PATH ONLY, AND NOT THE ENGINE'S (rmp #2349)
//
// Every writer below commits through st.Begin()/tx.Commit(). That is the path on
// which the invariant held even before rmp #2349, because [txn.Tx.Commit] defers its
// exitWriter past ApplyVersioned and therefore stays admitted through its own MVCC
// publish. This test could never have observed the defect, and its passing was read
// for one sprint as coverage it did not have.
//
// The PRODUCTION writer is the Cypher engine, which commits through
// [txn.Tx.CommitWALOnly] — no in-memory apply, no publish, and its writer
// registration released the moment the fsync returns while the instant is published
// later at bracket unwind. That ordering is covered by
// TestCheckpoint_EngineCommitOrdering_KeepsAnAckedCommit in
// engine_commit_boundary_test.go, which parks a transaction inside the window so the
// interleaving is deterministic rather than rare. Read the two together: this one
// says the invariant holds across a busy store-path workload, that one says it holds
// against the interleaving that actually broke it.
//
// # Why this is asserted here and not only through the artefact
//
// TestCheckpoint_CaptureIsAtomic_SnapshotPlusWALArtefact already proves recovery from
// the truncated WAL is EXACT, which is the observable consequence. But that test
// would also pass if the two positions disagreed and the surviving WAL suffix
// happened to heal it — and it would fail only on the interleaving that actually
// loses a commit, which is rare. This asserts the invariant itself, on every
// checkpoint, so a change that drains less is caught by construction rather than by
// luck.
//
// # The oracle, and why it is not vacuous
//
// InFlightCommits counts timestamps allocated but not yet published. It is asserted
// zero inside the locked window; the test separately records that it was NON-zero at
// least once while the writers ran, so a reading of zero is a fact about the window
// and not about a counter that is always zero.
func TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary(t *testing.T) {
	t.Parallel()
	_, g, st, w, cp := newPairStore(t)
	defer func() { _ = w.Close() }()

	var (
		// inWindowMax is the largest InFlightCommits reading taken INSIDE the locked
		// window. It must stay zero.
		inWindowMax atomic.Uint64
		// observedInFlight records that the counter can be non-zero at all, sampled
		// from outside the window while the writers run. Without it a zero inside the
		// window says nothing.
		observedInFlight atomic.Bool
		windowSamples    atomic.Int64
		// snapshotsInWindow records that the capture's own MVCC snapshot is registered
		// with the horizon at this point, which the horizon test below relies on.
		snapshotsInWindow atomic.Int64
	)
	cp.afterWatermarkHook = func() {
		s := g.MVCCStats()
		if n := s.InFlightCommits; n > inWindowMax.Load() {
			inWindowMax.Store(n)
		}
		if int64(s.ActiveSnapshots) > snapshotsInWindow.Load() {
			snapshotsInWindow.Store(int64(s.ActiveSnapshots))
		}
		windowSamples.Add(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	var (
		stop      atomic.Bool
		committed atomic.Int64
		writerErr atomic.Pointer[error]
	)
	const writers = 4
	var wg sync.WaitGroup
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				src := fmt.Sprintf("w%d-a%d", id, n)
				dst := fmt.Sprintf("w%d-b%d", id, n)
				tx := st.Begin()
				if err := tx.AddEdge(src, dst, 0); err != nil {
					e := err
					writerErr.Store(&e)
					_ = tx.Rollback()
					return
				}
				if err := tx.Commit(); err != nil {
					e := err
					writerErr.Store(&e)
					return
				}
				committed.Add(1)
				// Sampled from OUTSIDE any locked window: this is the control that says
				// the counter is capable of being non-zero on this workload.
				if g.MVCCStats().InFlightCommits > 0 {
					observedInFlight.Store(true)
				}
			}
		}(wi)
	}

	for c := 0; c < 30; c++ {
		if err := cp.Trigger(); err != nil {
			stop.Store(true)
			wg.Wait()
			cp.Stop()
			t.Fatalf("checkpoint %d: %v", c, err)
		}
	}
	stop.Store(true)
	wg.Wait()
	cp.Stop()

	if p := writerErr.Load(); p != nil {
		t.Fatalf("writer failed: %v", *p)
	}
	if committed.Load() == 0 {
		t.Fatal("no transaction committed: the checkpoints did not overlap any writer")
	}
	if windowSamples.Load() == 0 {
		t.Fatal("the phase-1 seam never fired: this test sampled nothing")
	}
	if n := inWindowMax.Load(); n != 0 {
		t.Errorf("%d commit(s) were allocated but unpublished inside the phase-1 window. The "+
			"watermark is a durability position and the instant is a visibility position: a "+
			"transaction durable below the watermark whose instant the image cannot see is "+
			"truncated away by phase 3 and lost. Phase 1 must wait the window out before it "+
			"reads either position (Checkpointer.awaitCommitQuiescence, rmp #2349)", n)
	}
	if !observedInFlight.Load() {
		t.Logf("note: InFlightCommits never observed non-zero outside the window over %d commits; "+
			"the in-window zero is still asserted but its discriminating power was not "+
			"demonstrated on this run", committed.Load())
	}
	if snapshotsInWindow.Load() == 0 {
		t.Error("the capture's MVCC snapshot was not registered with the horizon inside the " +
			"phase-1 window, so the image is not being read at a registered instant and the " +
			"horizon-release assertion below would be vacuous")
	}
	t.Logf("%d phase-1 windows sampled over %d commits: InFlightCommits stayed at 0 inside "+
		"every one, peak ActiveSnapshots %d",
		windowSamples.Load(), committed.Load(), snapshotsInWindow.Load())
}

// TestCheckpoint_ReleasesTheHorizonBeforeWritingTheSnapshot asserts AC5: the capture's
// MVCC snapshot is released once the bytes exist and BEFORE phase 2's disk I/O.
//
// # Why the release POINT and not just "it is released"
//
// A held snapshot pins reclamation, so version memory grows by whatever concurrent
// writers produce for as long as it is open. Releasing it eventually is not enough:
// the bound has to be a window whose duration is knowable. Released before phase 2,
// the bound is the in-memory serialisation — O(V+E), no disk I/O. Released after, the
// bound would be a multi-second snapshot write on an arbitrarily slow disk, and the
// horizon would be pinned across it on every checkpoint.
//
// The seam fires at the START of phase 2, so a snapshot still registered there is a
// release that has been pushed past the I/O. The companion test above establishes
// that ActiveSnapshots is non-zero inside phase 1, which is what makes a zero here a
// release rather than a gauge that never moves.
func TestCheckpoint_ReleasesTheHorizonBeforeWritingTheSnapshot(t *testing.T) {
	t.Parallel()
	_, g, st, w, cp := newPairStore(t)
	defer func() { _ = w.Close() }()

	// The test itself opens no reader, and the writers below hold a write snapshot
	// only for the duration of their own commit — so a reading taken at the start of
	// phase 2 can exceed zero transiently through a concurrent writer. What must not
	// happen is the CHECKPOINT's snapshot outliving the capture, and that is a
	// persistent pin rather than a transient one: it would be held across every
	// phase-2 sample of a single checkpoint. The quiet period below removes the
	// writers entirely for the final checkpoints, making the reading unambiguous.
	var maxAtPhase2 atomic.Int64
	var phase2Samples atomic.Int64
	cp.afterCaptureHook = func() {
		if n := int64(g.MVCCStats().ActiveSnapshots); n > maxAtPhase2.Load() {
			maxAtPhase2.Store(n)
		}
		phase2Samples.Add(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	var (
		stop      atomic.Bool
		committed atomic.Int64
		writerErr atomic.Pointer[error]
	)
	var wg sync.WaitGroup
	for wi := 0; wi < 4; wi++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				tx := st.Begin()
				if err := tx.AddEdge(fmt.Sprintf("w%d-a%d", id, n), fmt.Sprintf("w%d-b%d", id, n), 0); err != nil {
					e := err
					writerErr.Store(&e)
					_ = tx.Rollback()
					return
				}
				if err := tx.Commit(); err != nil {
					e := err
					writerErr.Store(&e)
					return
				}
				committed.Add(1)
			}
		}(wi)
	}
	// Checkpoints WITH writers running: this is the shape the property is about, and
	// it also builds a graph big enough that a capture is real work.
	for c := 0; c < 20; c++ {
		if err := cp.Trigger(); err != nil {
			stop.Store(true)
			wg.Wait()
			cp.Stop()
			t.Fatalf("busy checkpoint %d: %v", c, err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if p := writerErr.Load(); p != nil {
		cp.Stop()
		t.Fatalf("writer failed: %v", *p)
	}
	if committed.Load() == 0 {
		cp.Stop()
		t.Fatal("no transaction committed: the checkpoints did not overlap any writer")
	}

	// QUIET PERIOD. With every writer stopped and no reader of the test's own, the
	// only snapshot that could be registered at the start of phase 2 is the capture's.
	// A non-zero reading now is unambiguous.
	maxAtPhase2.Store(0)
	phase2Samples.Store(0)
	for c := 0; c < 5; c++ {
		if err := cp.Trigger(); err != nil {
			cp.Stop()
			t.Fatalf("quiet checkpoint %d: %v", c, err)
		}
	}
	cp.Stop()

	if phase2Samples.Load() == 0 {
		t.Fatal("the phase-2 seam never fired during the quiet period: this test sampled nothing")
	}
	if n := maxAtPhase2.Load(); n != 0 {
		t.Errorf("%d MVCC snapshot(s) were still registered at the start of phase 2 with no "+
			"writer and no reader running: the capture's snapshot is being held across the "+
			"snapshot WRITE. That pins the reclamation horizon for the duration of a disk "+
			"I/O of unbounded length instead of for the in-memory serialisation, so version "+
			"memory grows with checkpoint duration rather than with graph size", n)
	}
	t.Logf("%d quiet-period phase-2 samples, all with the horizon released; %d transactions "+
		"committed during the busy phase", phase2Samples.Load(), committed.Load())
}
