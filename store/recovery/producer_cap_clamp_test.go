package recovery

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// producer_cap_clamp_test.go — rmp #2530.
//
// The hazard, measured at HEAD before the fix: the per-transaction op cap
// exists on both sides and the two are independently configurable. Configure
// the PRODUCER looser than the REPLAYER and the oversized transaction is not
// refused — [txn.Tx.Commit] returns nil, the fsync happened, the write is
// acknowledged durable — and the next [Open] then fails the WHOLE directory
// with [ErrTransactionTooLarge]. Every transaction committed BEFORE the
// oversized one is stranded behind that fail-stop; every one committed AFTER it
// is discarded unreplayed. A raised producer bound for a bulk load turns a
// rejected write into a database that will not open.
//
// [Result.NewStoreCapped] now makes that combination inexpressible through the
// recovery handoff by clamping the producer bound down to [Result.MaxTxnOps].
// These tests pin both halves: the clamp fires, and the store stays openable.

// clampTestOpts is the codec pair the clamp tests reopen and write with.
func clampTestOpts() txn.Options[string, int64] {
	return txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
}

// commitNodes commits one transaction of n AddNode ops named prefix0..prefix<n-1>
// through st and returns the Commit error verbatim (nil means ACKNOWLEDGED
// DURABLE, which is the whole point of the assertion in these tests).
func commitNodes(t *testing.T, st *txn.Store[string, int64], prefix string, n int) error {
	t.Helper()
	tx := st.Begin()
	for i := 0; i < n; i++ {
		if err := tx.AddNode(prefix + itoa(i)); err != nil {
			t.Fatalf("AddNode(%s%d): %v", prefix, i, err)
		}
	}
	return tx.Commit()
}

// TestResultNewStoreCapped_ClampsProducerToReplayCap is the headline #2530
// regression. Recovery runs under a replay bound of 32; the caller then asks
// the handoff for an UNLIMITED producer bound — the exact "raise it for a bulk
// load" mistake. The clamp must lower it to 32, so the 33-op transaction is
// refused at commit with [txn.ErrTransactionTooLarge] instead of being made
// durable, and the directory must still reopen cleanly with the earlier
// transaction intact.
//
// PRE-FIX (NewStoreCapped passing maxTxnOps straight through): the 33-op commit
// returns nil and the reopen fails with recovery's ErrTransactionTooLarge.
func TestResultNewStoreCapped_ClampsProducerToReplayCap(t *testing.T) {
	t.Parallel()
	const replayCap = 32
	dir := t.TempDir()

	// A first, ordinary transaction, durable before the hazard is introduced.
	// It is what a fail-stop reopen would strand.
	writeCommittedTxnOfSize(t, dir, 5)

	res, err := openCappedTxnOps(t, dir, replayCap)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if !res.IsClean() {
		t.Fatalf("first Open not clean: %v", res.TailErr)
	}
	if res.MaxTxnOps != replayCap {
		t.Fatalf("Result.MaxTxnOps = %d, want %d (the bound the replay ran under)",
			res.MaxTxnOps, replayCap)
	}

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	// The mistake: ask for no producer bound at all while recovery keeps one.
	st := res.NewStoreCapped(w, clampTestOpts(), txn.MaxTxnOpsUnlimited)

	// A transaction the replayer could not replay must be REFUSED, not
	// acknowledged. This is the assertion that fails pre-fix.
	err = commitNodes(t, st, "big", replayCap+1)
	if err == nil {
		t.Fatalf("Commit(%d ops) was ACKNOWLEDGED DURABLE under a replay cap of %d: "+
			"the producer bound was not clamped, and this directory is now unopenable",
			replayCap+1, replayCap)
	}
	if !errors.Is(err, txn.ErrTransactionTooLarge) {
		t.Fatalf("Commit error = %v, want errors.Is(err, txn.ErrTransactionTooLarge)", err)
	}

	// A transaction AT the clamped bound still commits: the clamp lowered the
	// bound to the replay cap, it did not disable writing.
	if err := commitNodes(t, st, "ok", replayCap); err != nil {
		t.Fatalf("Commit(%d ops, exactly the clamped cap): %v, want nil", replayCap, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// The store is still openable, and holds BOTH transactions.
	res2, err := openCappedTxnOps(t, dir, replayCap)
	if err != nil {
		t.Fatalf("reopen after the refused oversized transaction: %v "+
			"(the whole directory became unopenable)", err)
	}
	if !res2.IsClean() {
		t.Fatalf("reopen not clean: %v", res2.TailErr)
	}
	m := res2.Graph.AdjList().Mapper()
	if _, ok := m.Lookup("n0"); !ok {
		t.Error("node n0 from the FIRST committed transaction is missing after reopen")
	}
	if _, ok := m.Lookup("ok0"); !ok {
		t.Error("node ok0 from the at-cap transaction is missing after reopen")
	}
	if _, ok := m.Lookup("big0"); ok {
		t.Error("node big0 from the REFUSED transaction is present; the refusal was not clean")
	}
}

// TestResultNewStoreCapped_ClampLeavesAgreeingCapsAlone is the negative
// control: when the requested producer bound is already <= the replay bound the
// clamp must not fire, and when the replay bound is itself unlimited there is
// nothing to clamp against.
func TestResultNewStoreCapped_ClampLeavesAgreeingCapsAlone(t *testing.T) {
	t.Parallel()

	t.Run("producer_below_replay_kept", func(t *testing.T) {
		t.Parallel()
		if got := clampProducerCapToReplay(8, 32); got != 8 {
			t.Fatalf("clamp(producer=8, replay=32) = %d, want 8 (already tighter)", got)
		}
	})
	t.Run("producer_equal_replay_kept", func(t *testing.T) {
		t.Parallel()
		if got := clampProducerCapToReplay(32, 32); got != 32 {
			t.Fatalf("clamp(32, 32) = %d, want 32", got)
		}
	})
	t.Run("producer_above_replay_lowered", func(t *testing.T) {
		t.Parallel()
		if got := clampProducerCapToReplay(64, 32); got != 32 {
			t.Fatalf("clamp(producer=64, replay=32) = %d, want 32", got)
		}
	})
	t.Run("unlimited_producer_finite_replay_lowered", func(t *testing.T) {
		t.Parallel()
		if got := clampProducerCapToReplay(txn.MaxTxnOpsUnlimited, 32); got != 32 {
			t.Fatalf("clamp(unlimited, 32) = %d, want 32", got)
		}
	})
	t.Run("unlimited_replay_clamps_nothing", func(t *testing.T) {
		t.Parallel()
		if got := clampProducerCapToReplay(txn.MaxTxnOpsUnlimited, txn.MaxTxnOpsUnlimited); got != txn.MaxTxnOpsUnlimited {
			t.Fatalf("clamp(unlimited, unlimited) = %d, want %d", got, txn.MaxTxnOpsUnlimited)
		}
		if got := clampProducerCapToReplay(0, txn.MaxTxnOpsUnlimited); got != 0 {
			t.Fatalf("clamp(default, unlimited) = %d, want 0", got)
		}
	})
	t.Run("zero_replay_is_no_information", func(t *testing.T) {
		t.Parallel()
		// A hand-built Result carries 0; there is nothing to clamp against.
		if got := clampProducerCapToReplay(txn.MaxTxnOpsUnlimited, 0); got != txn.MaxTxnOpsUnlimited {
			t.Fatalf("clamp(unlimited, no-information) = %d, want %d", got, txn.MaxTxnOpsUnlimited)
		}
	})
	t.Run("both_default_kept", func(t *testing.T) {
		t.Parallel()
		// 0 resolves to DefaultMaxTxnOps on both sides: equal, so kept verbatim,
		// which is what keeps every existing NewStore caller byte-for-byte
		// unchanged.
		if got := clampProducerCapToReplay(0, txn.DefaultMaxTxnOps); got != 0 {
			t.Fatalf("clamp(default, default) = %d, want 0 (unchanged)", got)
		}
	})
}

// TestResultMaxTxnOps_PopulatedByEveryOpenPath pins the carrier the clamp reads:
// Open must report the bound its replay ran under, in the caller-facing option
// convention, on a clean Result and on a fail-stop one alike.
func TestResultMaxTxnOps_PopulatedByEveryOpenPath(t *testing.T) {
	t.Parallel()

	t.Run("explicit_cap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCommittedTxnOfSize(t, dir, 4)
		res, err := openCappedTxnOps(t, dir, 32)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if res.MaxTxnOps != 32 {
			t.Fatalf("Result.MaxTxnOps = %d, want 32", res.MaxTxnOps)
		}
	})
	t.Run("default_cap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCommittedTxnOfSize(t, dir, 4)
		res, err := openCappedTxnOps(t, dir, 0)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if res.MaxTxnOps != txn.DefaultMaxTxnOps {
			t.Fatalf("Result.MaxTxnOps = %d, want %d", res.MaxTxnOps, txn.DefaultMaxTxnOps)
		}
	})
	t.Run("unlimited_cap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCommittedTxnOfSize(t, dir, 4)
		res, err := openCappedTxnOps(t, dir, txn.MaxTxnOpsUnlimited)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if res.MaxTxnOps != txn.MaxTxnOpsUnlimited {
			t.Fatalf("Result.MaxTxnOps = %d, want %d", res.MaxTxnOps, txn.MaxTxnOpsUnlimited)
		}
	})
	t.Run("fail_stop_result_still_reports_it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCommittedTxnOfSize(t, dir, 33)
		res, err := openCappedTxnOps(t, dir, 32)
		if !errors.Is(err, ErrTransactionTooLarge) {
			t.Fatalf("Open error = %v, want ErrTransactionTooLarge", err)
		}
		if res.MaxTxnOps != 32 {
			t.Fatalf("Result.MaxTxnOps = %d on a fail-stop Result, want 32", res.MaxTxnOps)
		}
	})
}

// TestProducerCapAboveReplayCap_HazardIsReal is the measurement the fix answers,
// kept as an executable record of WHY the clamp exists. It builds the producer
// store INDEPENDENTLY — the path the clamp deliberately does not cover — and
// pins the disproportion: the oversized commit is acknowledged durable, and the
// reopen then refuses the whole directory, stranding the earlier transaction
// and discarding the later one.
func TestProducerCapAboveReplayCap_HazardIsReal(t *testing.T) {
	t.Parallel()
	const replayCap = 32
	dir := t.TempDir()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	// Producer bound deliberately looser than the replay bound.
	st := txn.NewStoreWithOptionsCapped(g, w, clampTestOpts(), txn.MaxTxnOpsUnlimited)

	if err := commitNodes(t, st, "early", 5); err != nil {
		t.Fatalf("early commit: %v", err)
	}
	if err := commitNodes(t, st, "big", replayCap+1); err != nil {
		t.Fatalf("oversized commit returned %v; the premise of #2530 is that the "+
			"producer ACKNOWLEDGES it when its own bound is looser", err)
	}
	if err := commitNodes(t, st, "late", 1); err != nil {
		t.Fatalf("late commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := openCappedTxnOps(t, dir, replayCap)
	if !errors.Is(err, ErrTransactionTooLarge) {
		t.Fatalf("reopen error = %v, want ErrTransactionTooLarge (the whole directory refused)", err)
	}
	if res.IsClean() {
		t.Fatal("Result.IsClean() = true, want false: the reopen is a fail-stop")
	}
	m := res.Graph.AdjList().Mapper()
	// The earlier transaction replayed into the DIAGNOSTIC graph, but Open
	// returned an error, so a caller obeying the recover-then-append contract
	// never gets to use it: it is stranded behind the fail-stop.
	if _, ok := m.Lookup("early0"); !ok {
		t.Error("early0 missing from the diagnostic graph")
	}
	// Everything at and after the oversized transaction is gone.
	if _, ok := m.Lookup("big0"); ok {
		t.Error("big0 present: the oversized transaction must not have been applied")
	}
	if _, ok := m.Lookup("late0"); ok {
		t.Error("late0 present: a transaction committed AFTER the oversized one is discarded unreplayed")
	}
}
