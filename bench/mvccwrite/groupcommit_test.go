package mvccwrite

// groupcommit_test.go — rmp #2193: fsyncs per commit must FALL as writers rise.
//
// Layer: short.
//
// [TestWALWriteScalingGate] already gates the CONSEQUENCE of group commit —
// throughput rising with writer count. It cannot say the fsyncs coalesced, because
// throughput can rise for reasons that have nothing to do with the WAL: less lock
// contention, a warmer cache, a faster disk. This file gates the MECHANISM, counted
// from wal.Writer's own counter rather than inferred from time.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// coalesceFloor is how much lower syncs-per-commit must be at [gateWriters] than
// at one writer.
//
// A leader/follower round makes ONE fsync durable for every committer that arrived
// while it was in flight, so with N writers the ideal is N times fewer. The floor is
// set well below that because the real factor depends on arrival timing, which
// depends on the host: a writer that arrives after the leader has already entered
// the syscall pays its own fsync and coalesces with nobody.
//
// Measured on the development host at the time of writing: 1 writer 1.00
// syncs/commit, 8 writers 0.20-0.28 — a factor of 3.6x-5.0x. The floor of 1.60x is
// deliberately far below the weakest of those, because this gate exists to catch
// coalescing being LOST entirely (the factor returning to ~1.00x, which is what an
// exclusive lock across the commit path produces), not to police its efficiency.
const coalesceFloor = 1.60

// TestWALGroupCommit_FsyncsPerCommitFall gates the mechanism behind rmp #2193.
//
// # What made this reachable
//
// The coalescing itself was never missing: wal.Writer.SyncGroup has been
// leader/follower since it was written, and store/txn has carried the apply-ordering
// gate since #1507. What blocked it was structural — the engine called
// Tx.CommitWALOnly from inside the EXCLUSIVE visibility barrier, so there was never
// a second committer in flight for a leader to coalesce with, and the mechanism was
// present but unreachable. Retiring the single-writer semaphore (#2296) and moving
// version ownership onto the transaction left the commit finalisation running under
// a SHARED hold instead (see cypher.ExplicitTx.Commit), which is what allows two
// committers to overlap.
//
// So this gate does not test new machinery. It pins the reachability, which is the
// part a future refactor can silently take away — exactly as it was taken away
// before.
//
// # Why syncs-per-commit and not syncs
//
// The absolute fsync count falls trivially if fewer commits happen. The RATIO is
// the invariant: one fsync per commit means every committer paid its own, which is
// what a serialised commit path produces however fast the disk is.
func TestWALGroupCommit_FsyncsPerCommitFall(t *testing.T) {
	requireCores(t)
	// Coverage instrumentation lengthens and uniformises every basic block, which is
	// exactly what determines whether a second committer arrives before the leader
	// enters its syscall. It removes the overlap this measures rather than the
	// mechanism, so the ratio compresses towards 1.0 and reports a false regression.
	testlayers.RequireUninstrumented(t, "the ratio of WAL fsyncs to commits at one "+
		"writer against many, which depends on committers overlapping in real time")

	const ops = 240

	measure := func(writers int) float64 {
		t.Helper()
		r := newRig(t, wiringWAL)
		defer func() { _ = r.close() }()
		if r.wr == nil {
			t.Fatal("the WAL wiring did not expose its writer")
		}
		ctx := context.Background()
		// Warm up so first-write costs (segment creation, buffer growth) are not
		// counted, then take the counter from a steady state.
		if err := commit(ctx, r.eng, 0, -1); err != nil {
			t.Fatalf("warm-up commit: %v", err)
		}
		before := r.wr.Stats().Syncs
		arm := mustRunArm(t, writers, ops/writers, func(writer, i int) error {
			return commit(ctx, r.eng, writer, i)
		})
		syncs := r.wr.Stats().Syncs - before
		if arm.commits == 0 {
			t.Fatal("no commits recorded")
		}
		per := float64(syncs) / float64(arm.commits)
		t.Logf("wal group commit: %d writers, %d commits, %d fsyncs => %.3f fsyncs/commit",
			writers, arm.commits, syncs, per)
		return per
	}

	// Interleaved, best-of, and in this order: the single-writer arm is the baseline
	// and must not be measured only once on a host that drifts. See the repeat
	// rationale on [measureScaling].
	best := 0.0
	for r := 0; r < gateRepeats; r++ {
		one := measure(1)
		many := measure(gateWriters)
		if many <= 0 {
			t.Fatalf("%d writers made ZERO fsyncs for %d commits, which cannot be right: "+
				"either the counter is not being read or the commits are not durable", gateWriters, ops)
		}
		if factor := one / many; factor > best {
			best = factor
		}
	}

	if best < coalesceFloor {
		t.Fatalf("WAL fsyncs are no longer coalescing: %d writers pay only %.2fx fewer fsyncs per "+
			"commit than one writer (best of %d), floor is %.2fx. A factor near 1.00x means every "+
			"committer fsynced for itself, which is what an exclusive lock across the commit path "+
			"produces — wal.Writer.SyncGroup's leader/follower round is then unreachable again, as "+
			"it was before #2193. Check that the commit finalisation still runs under a SHARED hold "+
			"(cypher.ExplicitTx.Commit) and re-run on an idle machine before touching the floor: a "+
			"softened gate is not a gate.", gateWriters, best, gateRepeats, coalesceFloor)
	}
}
