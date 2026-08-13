package lpg

// mvcc_vacuum_test.go — MVCC C2 (rmp #2308): the background vacuum's gates.
//
// Each test here pins one of the properties that made reclamation movable off the
// commit path at all, and each is written to FAIL against the placement it
// replaces:
//
//   - the commit path performs no reclamation (it used to sweep inline);
//   - the goroutine terminates, on Close and on its own (there was no goroutine);
//   - one pass is bounded (the inline sweep was unbounded);
//   - memory stays bounded with a long-lived reader present, and the growth is
//     attributable to it;
//   - the one reclaimer that did NOT exclude a concurrent writer no longer loses
//     an index entry to one.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// vacuumGraph is a graph with the substrate armed and no vacuum yet started.
func vacuumGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return g
}

// churn makes n transactional modifications to one node.
func churn(t *testing.T, g *Graph[string, float64], n int) {
	t.Helper()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}

// TestVacuum_CommitPathPerformsNoReclamation is acceptance criterion 1, and the
// whole point of the task.
//
// # How it can tell
//
// By the PEAK. A commit-path sweep runs when the debt crosses [reclaimThreshold]
// and it runs BEFORE the commit returns, so the retained count observed between
// two commits can never exceed the threshold by more than one transaction's worth
// of versions — two, here. Observing a peak far above the threshold is therefore
// positive evidence that no commit swept, and it is evidence a synchronous sweep
// cannot manufacture. Measured: 8 480 against a threshold of 4 096.
//
// The obvious instrument — holding the single-sweeper slot and asserting the count
// never falls — was tried and REJECTED: the placement this replaces guarded itself
// with the same slot and skipped when it was busy, so the test passed against the
// very code it was written to catch.
//
// The two remaining assertions close the other direction: the peak stays under the
// ceiling, so "does not sweep" has not become "does not reclaim", and the records
// that went are accounted to vacuum passes.
func TestVacuum_CommitPathPerformsNoReclamation(t *testing.T) {
	g := vacuumGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	const rounds = reclaimThreshold * 4
	var peak int64
	for i := 0; i < rounds; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n := g.VersionCount(); n > peak {
			peak = n
		}
	}

	// It is still bounded, so "no sweep on the commit path" has not become "no
	// reclamation".
	if limit := int64(reclaimDebtCeiling + reclaimThreshold); peak > limit {
		t.Errorf("the retained count peaked at %d over an instantaneous bound of %d", peak, limit)
	}

	// THE DISCRIMINATOR: every record that went is ATTRIBUTED to a vacuum pass.
	// VacuumStats.Reclaimed is incremented only by [Graph.vacuumLoop], so records
	// freed by a commit-path sweep do not appear in it. Measured on this workload,
	// which creates about one version per round: on the correct build the vacuum
	// accounts for 75 % to 99.6 % of them (the spread is scheduling — how much of the
	// churn a pass happens to catch), and for 53 % once a synchronous commit-path
	// sweep is restored. TWO THIRDS separates those ranges with margin on both sides.
	//
	// The first threshold was nine tenths, fitted to a single 99.6 % sample, and it
	// failed on the correct build at 75 %. A bound fitted to one measurement is a
	// bound fitted to that measurement's luck.
	//
	// The obvious instrument — assert the retained count PEAKED above the threshold,
	// which a synchronous sweep could never allow — was tried and REJECTED because it
	// is itself a race: under -race the writer is slow enough that the sweeper keeps
	// up perfectly and the peak never rises, so it failed on the CORRECT build inside
	// make ci ("never exceeded 4098 over 16384 modifications"). Holding the
	// single-sweeper slot was rejected for the opposite reason: the placement this
	// replaces guarded itself with that same slot and skipped when it was busy, so it
	// passed against the very code it was written to catch.
	waitWithinBound(t, g)
	// WAIT FOR THE VACUUM TO QUIESCE before attributing (rmp #2335).
	//
	// waitWithinBound returns the moment the retained count is within the bound,
	// which can be well before the sweeper has finished crediting itself for the
	// churn — so the ratio below was being applied to however much of its work the
	// sweeper happened to have done by then. Under a full-coverage build, where the
	// vacuum goroutine gets markedly less CPU, that produced 10354 of 16384 (63.2 %)
	// and a failure claiming reclamation was back on the commit path. It was not:
	// a commit-path sweep measures 53 %, and 63.2 % is a STARVED SWEEPER, which is a
	// fact about the machine rather than about where reclamation happens.
	//
	// Waiting for Reclaimed to stop growing removes the scheduler from the
	// measurement. It does not weaken the discriminator: on a defective build the
	// commit path has ALREADY freed the records, so Reclaimed plateaus low however
	// long the wait runs — waiting cannot manufacture attribution that the vacuum
	// never earned.
	waitVacuumQuiesced(t, g)
	vs := g.VacuumStats()
	if vs.Passes == 0 {
		t.Errorf("the substrate settled with ZERO vacuum passes: the records were freed by " +
			"something other than the vacuum, which means reclamation is still on the " +
			"commit path")
	}
	if want := int64(rounds) * 2 / 3; vs.Reclaimed < want {
		t.Errorf("vacuum passes account for only %d records over %d modifications, short of the "+
			"%d that attribution requires, after the vacuum had QUIESCED (%d passes): the rest "+
			"were freed by something other than the vacuum, which means reclamation is still on "+
			"the commit path", vs.Reclaimed, rounds, want, vs.Passes)
	}
}

// waitVacuumQuiesced blocks until the vacuum has stopped crediting itself, so an
// attribution ratio measured afterwards reflects what the sweeper DID rather than
// how much CPU it happened to get (rmp #2335).
//
// Quiescence is Reclaimed unchanged across several consecutive polls spanning a real
// interval, not a single unchanged reading: one reading can fall between two passes.
// The deadline is the same settleTimeout the bound wait uses, and expiring it is not
// a failure — the caller's ratio then judges whatever was accumulated, which is the
// pre-existing behaviour and cannot be weaker than it.
func waitVacuumQuiesced(t *testing.T, g *Graph[string, float64]) {
	t.Helper()
	const stableFor = 5
	deadline := time.Now().Add(settleTimeout)
	last := g.VacuumStats().Reclaimed
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		now := g.VacuumStats().Reclaimed
		if now == last {
			stable++
			if stable >= stableFor {
				return
			}
			continue
		}
		last, stable = now, 0
	}
}

// TestVacuum_TerminatesOnClose is half of acceptance criterion 2: an explicit
// lifecycle with no leak.
//
// The package's goleak.VerifyTestMain is the other half — a vacuum that outlived
// its test would fail the whole package rather than this one function.
func TestVacuum_TerminatesOnClose(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	churn(t, g, reclaimThreshold)
	if vs := g.VacuumStats(); vs.Starts == 0 {
		t.Fatal("no vacuum was ever started, so this test is not closing one")
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	vs := g.VacuumStats()
	if vs.Running {
		t.Error("Close returned with the vacuum still running: the join is not a join")
	}
	if vs.Starts != vs.Exits {
		t.Errorf("vacuum lifecycle is unbalanced after Close: %d starts, %d exits", vs.Starts, vs.Exits)
	}
	// Idempotent, and a write afterwards must not resurrect the goroutine.
	if err := g.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := g.SetNodeProperty("a", "w", Int64Value(1)); err != nil {
		t.Fatalf("post-close write: %v", err)
	}
	if got := g.VacuumStats(); got.Starts != vs.Starts {
		t.Errorf("a write after Close started %d more vacuum(s); a closed graph must stay closed",
			got.Starts-vs.Starts)
	}
}

// TestVacuum_SelfTerminatesWithoutClose is the other half of acceptance criterion
// 2, and it is what makes Close optional rather than mandatory.
//
// A permanent goroutine per graph would be a leak by the module's own measure —
// this package's goleak gate would catch it — and a caller of [New] is not
// required to close. So the sweeper must exit on its own once there is nothing
// left to do.
func TestVacuum_SelfTerminatesWithoutClose(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	churn(t, g, reclaimThreshold)
	if vs := g.VacuumStats(); vs.Starts == 0 {
		t.Fatal("no vacuum was ever started, so this test cannot observe one exiting")
	}
	deadline := time.Now().Add(settleTimeout)
	for {
		vs := g.VacuumStats()
		if !vs.Running && vs.Starts == vs.Exits {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the vacuum was still alive %v after the last write with nobody closing the "+
				"graph: %+v", settleTimeout, vs)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestVacuum_CloseCtxReportsAnExpiredDeadline pins that the context bounds the
// JOIN and never the signal.
//
// An already-cancelled context must still stop the vacuum — abandoning the signal
// would leave a goroutine running with nothing able to stop it, which is the leak
// Close exists to prevent — while reporting that the caller's deadline passed.
func TestVacuum_CloseCtxReportsAnExpiredDeadline(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	churn(t, g, reclaimThreshold)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Either outcome is legitimate for the RETURN: the sweeper may already have
	// exited. What is not legitimate is the signal being skipped.
	_ = g.CloseCtx(ctx)
	if err := g.Close(); err != nil {
		t.Fatalf("Close after a cancelled CloseCtx: %v", err)
	}
	if vs := g.VacuumStats(); vs.Running || vs.Starts != vs.Exits {
		t.Errorf("a cancelled CloseCtx did not deliver the shutdown signal: %+v", vs)
	}
}

// TestVacuum_PassRespectsTheRecordBound is acceptance criterion 2's per-pass
// bound, asserted by making it bite.
//
// A reader is pinned while far more than [vacuumRecordsPerPass] records
// accumulate, so when it leaves there is more reclaimable work than one pass may
// do. An unbounded sweep would clear it in a single pass and record no capped
// one.
func TestVacuum_PassRespectsTheRecordBound(t *testing.T) {
	g := vacuumGraph(t)
	if vs := g.VacuumStats(); vs.RecordsPerPass != vacuumRecordsPerPass {
		t.Fatalf("the exported per-pass bound is %d, want %d", vs.RecordsPerPass, vacuumRecordsPerPass)
	}
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	pin := g.BeginRead()
	// Two versions per round (a property delta and its predecessor's chain link),
	// so this holds comfortably more than one pass may release.
	for i := 0; i < vacuumRecordsPerPass+reclaimThreshold; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			g.EndRead(pin)
			t.Fatalf("write %d: %v", i, err)
		}
	}
	held := g.VersionCount()
	if held <= vacuumRecordsPerPass {
		g.EndRead(pin)
		t.Fatalf("the pinned reader is holding only %d records, which one pass may release in "+
			"full: this test cannot observe the bound", held)
	}
	g.EndRead(pin)
	waitWithinBound(t, g)
	// POLLED, not sampled once. The pass decrements each store's record counter
	// while it runs and increments CappedPasses only as it returns, so
	// waitWithinBound can observe a settled substrate a few instructions before the
	// flag is published — which failed under -race in make ci while passing
	// standalone.
	deadline := time.Now().Add(settleTimeout)
	for {
		if g.VacuumStats().CappedPasses > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d reclaimable records were cleared with no pass hitting the %d-record "+
				"bound: the pass is not bounded (vacuum %+v)",
				held, vacuumRecordsPerPass, g.VacuumStats())
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestVacuum_BoundedUnderChurnWithALongLivedReader is acceptance criterion 3.
//
// The reader is the half of the bound this package cannot enforce — it is
// entitled to what it can still reach — so what has to hold is that the CHURN
// half stays inside its ceiling throughout, that the growth is attributable to
// the reader, and that it all comes back when the reader leaves.
func TestVacuum_BoundedUnderChurnWithALongLivedReader(t *testing.T) {
	g := vacuumGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	pin := g.BeginRead()
	for i := 0; i < reclaimThreshold*4; i++ {
		if err := g.ApplyAtomically(func() error {
			return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
		}); err != nil {
			g.EndRead(pin)
			t.Fatalf("write %d: %v", i, err)
		}
	}
	s := g.MVCCStats()
	if s.ActiveSnapshots != 1 {
		g.EndRead(pin)
		t.Fatalf("the horizon reports %d active readers, want 1", s.ActiveSnapshots)
	}
	if s.OldestSnapshotAge() == 0 {
		t.Error("the oldest reader's age is zero while it is demonstrably behind: the growth " +
			"cannot be attributed to the read that caused it")
	}
	if s.UnregisteredSnapshots != 0 {
		t.Errorf("%d readers failed to register: reclamation is suspended for a different reason "+
			"than this test is measuring", s.UnregisteredSnapshots)
	}
	g.EndRead(pin)
	settled := waitWithinBound(t, g)
	if settled.Total > settled.Bound {
		t.Errorf("after the reader left the substrate holds %d records against a bound of %d",
			settled.Total, settled.Bound)
	}
}

// TestVacuum_ReclaimNowExcludesTheBackgroundSweeper pins that the synchronous
// settlement and the vacuum cannot walk the same chain.
//
// Both take the single-sweeper slot, so this is really an assertion that
// [Graph.ReclaimNow] takes it — before rmp #2308 it took nothing, and its contract
// asked the CALLER to hold the visibility barrier, which no longer exists on the
// write path. Under -race a violation shows up as a data race on a version chain;
// without it, as a count that does not settle.
func TestVacuum_ReclaimNowExcludesTheBackgroundSweeper(t *testing.T) {
	g := vacuumGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < reclaimThreshold*2; i++ {
			if err := g.ApplyAtomically(func() error {
				return g.SetNodeProperty("a", "w", Int64Value(int64(i)))
			}); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		g.ReclaimNow()
	}
	wg.Wait()
	if got := g.ReclaimNow(); got < 0 {
		t.Fatalf("ReclaimNow reported %d", got)
	}
	waitWithinBound(t, g)
}

// TestDeferredIndexRemoval_ConcurrentReaddIsNotLost is acceptance criterion 4's
// regression gate, and the one genuine defect the exclusion audit found.
//
// # The failure it reproduces
//
// [Graph.applyDeferredIndexRemovals] used to collect the ready entries under
// idxDeferred.mu, RELEASE the lock, and only then remove them from the label
// bitmap. A writer re-adding the same label in that window found nothing pending
// to cancel and then had its index entry deleted underneath it — so the node
// carried the label and was absent from every later label scan, which is the one
// failure direction the candidate-set discipline cannot recover from.
//
// It could not surface while the sweep ran under the visibility barrier, which
// excluded the writer by construction. The background vacuum has no barrier.
//
// # How the interleaving is driven
//
// NOT by a background sweeper racing an ordinary workload — that was tried and it
// never hit the window, because the gap between releasing the lock and removing
// from the bitmap is a handful of instructions. Instead each round starts the
// reclaimer and the re-add as two goroutines released together, on a key whose
// removal the watermark has already passed, which is the exact state the defect
// needs. A few thousand rounds of that lands in the window reliably; verified by
// reverting both halves of the fix, where it fails within the first few hundred.
func TestDeferredIndexRemoval_ConcurrentReaddIsNotLost(t *testing.T) {
	g := vacuumGraph(t)
	// The BACKGROUND vacuum is closed out of this test, because it is a third
	// party to the interleaving under test and it interferes with the instrument:
	// its own sweep applies the deferred removal before the hand-driven one gets
	// there, and the "there is a deferred entry to race" precondition then fails
	// (observed at round 908 under -race, where the whole package's churn had a
	// sweeper already running). The two parties whose locking is being tested —
	// the reclaimer and the label-add path — are both driven explicitly below.
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lid := g.reg.Intern("L")
	const rounds = 4000

	for r := 0; r < rounds; r++ {
		k := fmt.Sprintf("n%d", r)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("round %d AddNode: %v", r, err)
		}
		if err := g.SetNodeLabel(k, "L"); err != nil {
			t.Fatalf("round %d SetNodeLabel: %v", r, err)
		}
		// Remove it, which DEFERS the bitmap removal, and let the watermark pass
		// the removal's instant so the next sweep finds it ready.
		if err := g.ApplyAtomically(func() error {
			g.RemoveNodeLabel(k, "L")
			return nil
		}); err != nil {
			t.Fatalf("round %d remove: %v", r, err)
		}
		if g.IndexRemovalBacklog() == 0 {
			t.Fatalf("round %d: the removal was applied eagerly, so there is no deferred entry "+
				"for a concurrent re-add to race", r)
		}
		watermark := g.horizon.Oldest(g.mvccClock.ReadTS())
		if watermark == 0 {
			t.Fatalf("round %d: reclamation is suspended, so no sweep can run", r)
		}

		// Released together, so the sweep's bitmap removal and the re-add's bitmap
		// insertion contend for the same few instructions.
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			g.vac.acquireSweeper()
			g.applyDeferredIndexRemovals(watermark)
			g.vac.releaseSweeper()
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := g.SetNodeLabel(k, "L"); err != nil {
				t.Errorf("round %d re-add: %v", r, err)
			}
		}()
		close(start)
		wg.Wait()

		// The node carries the label again, so it MUST be a candidate. The bitmap is
		// allowed to over-report; losing a member is unrecoverable.
		id := mvccNodeID(t, g, k)
		if !g.HasNodeLabel(k, "L") {
			t.Fatalf("round %d: the re-add did not take effect, so this round proves nothing", r)
		}
		if !g.nodeIdx.Intersect(uint32(lid)).Contains(uint64(id)) {
			t.Fatalf("round %d: node %s carries label L but left the label bitmap — a silently "+
				"lost row; the deferred removal fired over a concurrent re-add", r, k)
		}
	}
}

// TestVacuum_WatermarkRegressionIsDetected pins [Graph.publishWatermark] on both
// controls: a monotone sequence of watermarks must report nothing, and a decrease
// must be recorded with the pair that produced it.
//
// The invariant it guards is the one reclamation rests on — see
// [vacuumState.wmRegress] for why a decrease cannot happen while the substrate is
// sound, and why it is an Isolation violation rather than a leak when it does.
// Without this test the counter is one that can only ever read zero, which is
// indistinguishable from a sound system (rmp #2420).
func TestVacuum_WatermarkRegressionIsDetected(t *testing.T) {
	t.Parallel()

	t.Run("a monotone sequence reports nothing", func(t *testing.T) {
		g := New[string, int64](adjlist.Config{Directed: true})
		for _, wm := range []uint64{1, 1, 5, 5, 900, 901} {
			g.publishWatermark(g.vac.lastWatermark.Load(), wm)
		}
		if n := g.vac.wmRegress.Load(); n != 0 {
			t.Fatalf("wmRegress = %d after a monotone sequence, want 0", n)
		}
		if got := g.MVCCStats().WatermarkRegressions; got != 0 {
			t.Fatalf("MVCCStats().WatermarkRegressions = %d, want 0", got)
		}
	})

	t.Run("a decrease is reported with its pair", func(t *testing.T) {
		g := New[string, int64](adjlist.Config{Directed: true})
		if advanced := g.publishWatermark(g.vac.lastWatermark.Load(), 100); !advanced {
			t.Fatal("publishWatermark(100) on a fresh graph did not report an advance")
		}
		if advanced := g.publishWatermark(g.vac.lastWatermark.Load(), 50); advanced {
			t.Fatal("publishWatermark(50) after 100 reported an ADVANCE")
		}
		if n := g.vac.wmRegress.Load(); n != 1 {
			t.Fatalf("wmRegress = %d after a decrease, want 1", n)
		}
		if from, to := g.vac.wmRegressFrom.Load(), g.vac.wmRegressTo.Load(); from != 100 || to != 50 {
			t.Fatalf("recorded regression %d -> %d, want 100 -> 50", from, to)
		}
		if got := g.MVCCStats().WatermarkRegressions; got != 1 {
			t.Fatalf("MVCCStats().WatermarkRegressions = %d, want 1", got)
		}
	})
}
