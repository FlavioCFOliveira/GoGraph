//go:build gograph_crashinject

package crashinject_test

// mvcc_clock_shape_test.go — rmp #2309 (MVCC C3c), acceptance criterion 3: after
// kill -9, no transaction that was not durably committed is visible, every
// acknowledged one is, and the restored MVCC clock never re-mints a durable instant.
//
// Layer: short, and compiled only under the gograph_crashinject build tag — without
// it internal/crashpoint's Breakpoint is an empty function, the child runs to
// completion and is never killed.
//
// # The window under test, and why it is the one that matters
//
// cypher's commitUnderBarrier fsyncs the WAL and only then lets the write bracket
// unwind, which is what publishes the MVCC instant. Between the two the transaction
// is DURABLE BUT INVISIBLE: its OpCommit marker carries a timestamp no reader ever
// saw. That is the exact ordering case the derive-and-ratchet design exists to get
// right, and it cannot be reasoned about — the spec says so explicitly: "Validate by
// crash-injection, not by reasoning."
//
// Two directions must both hold, and a violation of either is silent:
//
//   - a timestamp that was made visible must NEVER be re-minted. Recovery derives
//     its floor from the largest instant in the file and adds one, so the floor must
//     sit above the crashed transaction's instant even though nobody ever saw it;
//   - a timestamp allocated but never published must not make a PHANTOM transaction
//     visible. Here the transaction is not a phantom: its fsync returned, so it is
//     committed by the module's acked-implies-durable contract and recovery must
//     apply it. What must not appear is anything the file does not carry.
//
// The assertions are on the recovered GRAPH SHAPE and on the recovered CLOCK, not on
// a clean exit code — a recovery that silently dropped the crashed transaction, or
// restarted the clock at zero, exits perfectly cleanly.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// mvccCommitScenario is the breakpoint the child crashes at: after the WAL fsync
// returned and before the commit instant is published.
const mvccCommitScenario = "mvcc.commit.post-fsync-pre-publish"

// mvccSeedKeys and mvccCrashKey mirror the helper's workload literally rather than
// importing its variables, so a change on either side has to be reconciled
// deliberately — the same discipline the sibling shape expectations use.
var mvccSeedKeys = []int64{10, 20, 30}

const mvccCrashKey int64 = 40

// TestCrashRecovery_MVCCClock_PostFsyncPrePublish is AC 3.
func TestCrashRecovery_MVCCClock_PostFsyncPrePublish(t *testing.T) {
	// SKIP the seed transactions' breakpoint hits so the crash lands on the LAST
	// commit rather than the first. Without this the child dies during transaction
	// one and the recovered graph holds a single node — which still exercises the
	// window, but leaves no published prefix to distinguish from the
	// durable-but-unpublished commit, and that distinction is the whole test.
	out, err := crashinject.Run(t, mvccCommitScenario, crashinject.Opts{
		Env: []string{"GOGRAPH_CRASH_AFTER=" + strconv.Itoa(len(mvccSeedKeys))},
	})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", mvccCommitScenario, err)
	}
	if out.TimedOut {
		t.Fatalf("child timed out instead of crashing at %s\nstdout: %s\nstderr: %s",
			mvccCommitScenario, out.Stdout, out.Stderr)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s (exit code %d)\nstdout: %s\nstderr: %s",
			mvccCommitScenario, out.ExitCode, out.Stdout, out.Stderr)
	}
	dir := out.Dir

	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open after %s: %v", mvccCommitScenario, err)
	}
	if res.Graph == nil {
		t.Fatal("recovery.Open returned a nil graph")
	}

	// ── The graph shape ──────────────────────────────────────────────────────
	// Every seed transaction was acknowledged, so every one must be present. The
	// crashing transaction's fsync RETURNED before the breakpoint, so under the
	// module's acked-implies-durable contract it is committed too and must also be
	// present — even though its instant was never published and no reader saw it.
	wantKeys := append(append([]int64(nil), mvccSeedKeys...), mvccCrashKey)
	for _, id := range wantKeys {
		if !hasAcctNode(t, res.Graph, id) {
			t.Errorf("%s: the recovered graph is missing :Acct{id:%d}. Every one of "+
				"these transactions fsynced before the crash, so every one is durable "+
				"and must replay — dropping the last is an ATOMICITY violation, since "+
				"its OpCommit marker is in the file",
				mvccCommitScenario, id)
		}
	}
	if got, want := res.Graph.LiveOrder(), uint64(len(wantKeys)); got != want {
		t.Errorf("%s: live node count = %d, want %d — an extra node means recovery "+
			"invented a transaction the file does not carry", mvccCommitScenario, got, want)
	}

	// ── The clock ────────────────────────────────────────────────────────────
	if res.MaxCommitTS == 0 {
		t.Fatalf("%s: recovery observed no commit instant in the WAL. The whole point "+
			"of this scenario is a durable marker carrying an unpublished timestamp, so "+
			"either the instant is not reaching the record or the child crashed before "+
			"writing one", mvccCommitScenario)
	}
	// THE INVARIANT: the next instant this process mints must exceed every instant
	// the file already carries. Otherwise a version committed before the crash and a
	// version committed after it share a timestamp, and a reader cannot order them.
	//
	// THIS ASSERTION IS A GUARD, NOT A DISCRIMINATOR, and that was established by
	// injecting the defect rather than assumed: removing the clock restore from
	// recovery entirely leaves it GREEN. WAL-only replay mints an instant per OP
	// while the durable maximum counts TRANSACTIONS, and ops-per-transaction is
	// always at least one — so the replayed clock necessarily overshoots and the
	// restore cannot be observed here.
	//
	// The restore becomes load-bearing only when a SNAPSHOT folds the WAL prefix, so
	// that the file's instants are high while the ops left to replay are few. The
	// discriminating coverage therefore lives in
	// TestClockRestore_NextCommitExceedsEveryPreviouslyPublishedInstant
	// (store/recovery), which manufactures that relationship directly, and it will
	// become natural here once C3d puts the captured instant in the snapshot header.
	//
	// It stays because the invariant is the one this whole task exists to protect,
	// and a crash scenario is where it would break first.
	now := res.Graph.MVCCStats().Now
	if now <= res.MaxCommitTS {
		t.Fatalf("%s: the recovered clock reads %d but the WAL already carries an "+
			"instant of %d. The next commit would RE-MINT a durable timestamp: two "+
			"different transactions would share one instant, and a reader could reach "+
			"a version that is simultaneously in its past and its future",
			mvccCommitScenario, now, res.MaxCommitTS)
	}
	if n := res.Graph.MVCCStats().InFlightCommits; n != 0 {
		t.Errorf("%s: the recovered graph reports %d in-flight commits, want 0. "+
			"Recovery has no transactions in flight by construction, and a non-zero "+
			"count holds the contiguous frontier back for every reader",
			mvccCommitScenario, n)
	}
}

// hasAcctNode reports whether the recovered graph carries the :Acct node with this
// id. The helper writes them through Cypher, so the node key is the engine's
// generated identity rather than the id property; the property is what identifies
// the transaction, so that is what is checked.
func hasAcctNode(t *testing.T, g *lpg.Graph[string, float64], id int64) bool {
	t.Helper()
	found := false
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		v, ok := g.GetNodeProperty(key, "id")
		if !ok {
			return true
		}
		if iv, ok := v.Int64(); ok && iv == id {
			found = true
			return false
		}
		return true
	})
	return found
}
