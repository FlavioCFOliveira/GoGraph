//go:build soak || nightly

package sim

// bolt_tx_registry_soak_test.go — the long-running arms of the Bolt
// transaction-registry surface (rmp #2482).
//
// The short layer proves each clause works and that it can fail; it cannot prove
// the registry and the per-principal quota stay CORRECT at scale or over
// sustained churn, because those are questions about a run long enough for an
// ordering defect to become visible and for a leaked slot to accumulate. These
// two arms answer them:
//
//   - the SCALE arm opens 64 abandoned transactions at once, which is an order of
//     magnitude past the five the short arm uses, and holds the listing to
//     oldest-first order and the reaper to draining every one of them;
//   - the CHURN arm cycles a principal to its cap and back hundreds of times, so
//     a quota slot that were released only sometimes — or a registry entry left
//     behind on one teardown path in a hundred — stops the run.
//
// Runs under the soak layer only (docs/test-layers.md).

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// txSoakAbandonCount is how many transactions the scale arm opens at once.
const txSoakAbandonCount = 64

// TestBoltTxRegistry_ScaleAbandoned is the scale arm: 64 transactions opened one
// advance apart, every one of them abandoned, and the whole registry drained by
// the idle reaper at the ordinals the independent model predicts.
//
// The idle bound is widened along with the count, and that is not incidental. The
// short arm's ten-step bound would put 54 of the 64 opens already past their
// deadline by the time the measured sequence began, so they would all be reaped
// on the first advance and the run would stop distinguishing a correct reaper
// from one that empties the registry on its first fire. Seventy steps against 64
// staggered opens keeps every predicted ordinal DISTINCT, which is what makes the
// ordering and drain claims below load-bearing.
func TestBoltTxRegistry_ScaleAbandoned(t *testing.T) {
	defer goleak.VerifyNone(t)

	opts := defaultBoltTxAbandonOptions()
	opts.Count = txSoakAbandonCount
	opts.ServerIdleBound = 70 * txAbandonStep
	opts.ModelIdleBound = opts.ServerIdleBound

	start := time.Now()
	ev, err := runBoltTxAbandoned(context.Background(), txAbandonDefaultSeed, &opts)
	if err != nil {
		t.Fatalf("scale run: %v", err)
	}
	elapsed := time.Since(start)

	for _, v := range checkBoltTxRegistry(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	// The non-vacuity gate is written for the SHORT arm's five-transaction plan, so
	// its plan-size clause necessarily fires here. Every OTHER clause must still
	// hold: the gate stays live at scale rather than being switched off wholesale.
	for _, v := range checkBoltTxRegistryNonVacuity(ev) {
		if v.Op == txOp(ev.Arm, "nonvacuity-plan") {
			continue
		}
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}

	// Ordering, asserted on the instants rather than only through the adjudicator:
	// txRegistry.list documents oldest first, and at this size a sort that was
	// merely nearly right would still pass a spot check.
	if len(ev.Listing) != txSoakAbandonCount {
		t.Fatalf("the registry listed %d of %d transactions", len(ev.Listing), txSoakAbandonCount)
	}
	for i := 1; i < len(ev.Listing); i++ {
		if ev.Listing[i].StartedAt <= ev.Listing[i-1].StartedAt {
			t.Errorf("listing entry %d (%s) started at %s, not strictly after entry %d (%s) at %s: the listing is "+
				"not oldest-first", i, ev.Listing[i].IDSuffix, ev.Listing[i].StartedAt,
				i-1, ev.Listing[i-1].IDSuffix, ev.Listing[i-1].StartedAt)
			break
		}
	}
	// Drain to zero: every transaction left, and each at its own ordinal.
	if ev.Reaped != txSoakAbandonCount {
		t.Errorf("%d of %d transactions left the registry", ev.Reaped, txSoakAbandonCount)
	}
	if slices.Contains(ev.ObservedReapOrdinals, reapNever) {
		t.Errorf("at least one transaction was never reaped: observed ordinals %v", ev.ObservedReapOrdinals)
	}
	distinct := make(map[int]bool, len(ev.PredictedReapOrdinals))
	for _, ord := range ev.PredictedReapOrdinals {
		distinct[ord] = true
	}
	if len(distinct) != txSoakAbandonCount {
		t.Errorf("the geometry predicts %d distinct reap ordinals for %d transactions: without one ordinal each, a "+
			"reaper that emptied the whole registry at once would pass", len(distinct), txSoakAbandonCount)
	}
	t.Logf("scale arm: %d transactions, %d advances, wall %s (%s of simulated time)",
		txSoakAbandonCount, ev.Advances, elapsed, ev.SimElapsed)
}

// txSoakChurnRounds is how many cap-and-release cycles the churn arm drives.
const txSoakChurnRounds = 200

// TestBoltTxRegistry_QuotaChurn is the churn arm: one principal is driven to its
// cap and back, hundreds of times, on two connections it REUSES.
//
// Reusing the connections is the point. Every round ends with the idle reaper
// taking both transactions, so each round exercises the reap teardown path —
// Session.txClosed, which releases the quota slot and unregisters the entry
// together — rather than the connection teardown that a fresh connection per
// round would hide behind. A slot released only on some paths, or an entry left
// behind on one teardown in a hundred, stops the run: the next round's BEGIN is
// refused, or the registry never drains.
func TestBoltTxRegistry_QuotaChurn(t *testing.T) {
	defer goleak.VerifyNone(t)

	const (
		idleBound  = 1 * time.Second
		totalBound = 10 * time.Minute
	)
	ctx := context.Background()
	probe := newTxClockProbe()
	srv, err := NewSimServerTxRegistry(SimEngineForServer(), clock.Real(), probe,
		idleBound, totalBound, txQuotaLimit)
	if err != nil {
		t.Fatalf("NewSimServerTxRegistry: %v", err)
	}
	defer func() {
		if cerr := srv.Close(); cerr != nil {
			t.Errorf("SimServer.Close: %v", cerr)
		}
	}()

	// The cap's worth of connections, plus one that only ever gets refused. They are
	// closed by one deferred sweep rather than one defer each, so the loop does not
	// accumulate deferred calls.
	holders := make([]*WireClient, 0, txQuotaLimit)
	defer func() { closeWireClients(holders) }()
	for range txQuotaLimit {
		holders = append(holders, dialAsPrincipal(ctx, t, srv, txQuotaPrincipalA))
	}
	over := dialAsPrincipal(ctx, t, srv, txQuotaPrincipalA)
	defer func() { _ = over.Close() }()

	wantRefusal := txQuotaRefusalMessage(txQuotaPrincipalA, txQuotaLimit)
	start := time.Now()
	for round := range txSoakChurnRounds {
		for i, c := range holders {
			if berr := soakBegin(c, "r"); berr != nil {
				t.Fatalf("round %d: holder %d could not BEGIN: %v — a quota slot from an earlier round was never "+
					"released", round, i, berr)
			}
		}
		if werr := waitForTxRegistry(func() bool { return len(srv.Server().Transactions()) == txQuotaLimit },
			fmt.Sprintf("round %d: the registry never listed the principal's %d transactions", round, txQuotaLimit)); werr != nil {
			t.Fatalf("%v", werr)
		}
		// The cap must still refuse, every round.
		if err := armTxReadDeadline(over); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		resp, rerr := over.BeginMode("r")
		if rerr != nil {
			t.Fatalf("round %d: over-cap BEGIN: %v", round, rerr)
		}
		if isSuccess(resp) {
			t.Fatalf("round %d: the BEGIN over the cap was ACCEPTED; the principal held %d slots",
				round, txQuotaLimit)
		}
		if got := failureMessage(resp); got != wantRefusal {
			t.Fatalf("round %d: refused with %q, want %q", round, got, wantRefusal)
		}
		if err := clearTxReadDeadline(over); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// A refused BEGIN leaves the session READY (rmp #2561), so `over` is reusable
		// as-is next round; a session that had been moved to FAILED would need a RESET
		// and this loop would fail on the round after.

		// Release every slot through the idle reaper, and require the registry to
		// drain completely before the next round begins.
		probe.Advance(idleBound)
		if werr := waitForTxRegistry(func() bool { return len(srv.Server().Transactions()) == 0 },
			fmt.Sprintf("round %d: the registry never drained after one advance of the idle bound", round)); werr != nil {
			t.Fatalf("%v", werr)
		}
		// The reap left each holder FAILED with a pending termination failure; RESET
		// is what clears both so the next round can BEGIN.
		for i, c := range holders {
			if rerr := soakReset(c); rerr != nil {
				t.Fatalf("round %d: holder %d could not RESET: %v", round, i, rerr)
			}
		}
	}
	elapsed := time.Since(start)

	// The instrument must have been live throughout: one timer per BEGIN, and the
	// reaper armed on the injected clock rather than on wall time.
	if want := int64(txSoakChurnRounds * txQuotaLimit); probe.Timers() != want {
		t.Errorf("the server armed %d timer(s) on the injected clock over %d rounds, want %d (one per accepted BEGIN; "+
			"a refused BEGIN arms none)", probe.Timers(), txSoakChurnRounds, want)
	}
	if probe.Tickers() != 0 {
		t.Errorf("the server registered %d ticker(s) on the injected clock, want 0", probe.Tickers())
	}
	t.Logf("churn arm: %d rounds x %d slots = %d BEGINs plus %d refusals, wall %s (%s of simulated time)",
		txSoakChurnRounds, txQuotaLimit, txSoakChurnRounds*txQuotaLimit, txSoakChurnRounds,
		elapsed, time.Duration(txSoakChurnRounds)*idleBound)
}

// TestBoltTxRegistry_ListingCostAtScale MEASURES what txRegistry.list costs as
// the registry grows, under BOTH arrangements of its input, and reports the
// figures. It asserts nothing about them.
//
// # Why two arrangements, and why the first one alone would have been a lie
//
// list() ranges a Go map — whose iteration order is randomised — and then
// insertion-sorts the result by StartedAt, swapping only on a strict Before
// (bolt/server/txregistry.go:188-194). The cost therefore depends entirely on
// whether the entries' instants are DISTINCT:
//
//   - when every entry shares one instant the inner condition is never true, so
//     the sort makes ZERO swaps and degenerates to a linear scan;
//   - when the instants are distinct the map's random order is a genuine
//     permutation and the insertion sort is quadratic in expectation.
//
// The first arrangement is what a fake-clock harness produces by default, because
// the registry stamps StartedAt from the SERVER clock and a harness that never
// advances it registers everything at one instant. The first version of this
// measurement did exactly that, and reported a per-entry cost that FELL with n —
// a linear shape that looked like evidence the sort was cheap and was in fact
// evidence the sort had nothing to do. The staggered arrangement is the one that
// measures the sort, and a production server, whose clock is real, is always in
// it.
//
// MEASURED on this machine, one Transactions() call, no -race:
//
//	open   same-instant        distinct-instants
//	   8   326ns   (40ns/e)    314ns    (39ns/e)
//	  64   2.954µs (46ns/e)    11.839µs (184ns/e)
//	 256   8.628µs (33ns/e)    143.153µs (559ns/e)
//	 512   16.74µs (32ns/e)    599.715µs (1.171µs/e)
//
// The same-instant column is flat per entry; the distinct-instants column rises
// linearly per entry, i.e. the call is QUADRATIC in the number of open
// transactions, and 256 -> 512 costs 4.19x for twice the input. Recorded here and
// left alone: fixing it is not this task's scope.
func TestBoltTxRegistry_ListingCostAtScale(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, stagger := range []bool{false, true} {
		arrangement := "same-instant"
		if stagger {
			arrangement = "distinct-instants"
		}
		for _, n := range []int{8, 64, 256, 512} {
			t.Run(fmt.Sprintf("%s/open=%d", arrangement, n), func(t *testing.T) {
				per := measureTxListCost(t, n, stagger)
				t.Logf("Transactions() over %d open transactions (%s): %s per call (%s per entry)",
					n, arrangement, per, per/time.Duration(n))
			})
		}
	}
}

// txListStaggerStep is the virtual time between two opens in the staggered
// arrangement. Any positive value makes the instants distinct; it is far below
// the ten-minute bounds, so nothing is ever reaped mid-measurement.
const txListStaggerStep = time.Millisecond

// measureTxListCost opens n transactions and returns the mean cost of one
// Server.Transactions call over them. When stagger is set, the fake server clock
// is advanced between opens so every entry carries a DISTINCT StartedAt.
func measureTxListCost(t *testing.T, n int, stagger bool) time.Duration {
	t.Helper()
	ctx := context.Background()
	probe := newTxClockProbe()
	srv, err := NewSimServerTxRegistry(SimEngineForServer(), clock.Real(), probe,
		10*time.Minute, 10*time.Minute, 0)
	if err != nil {
		t.Fatalf("NewSimServerTxRegistry: %v", err)
	}
	defer func() {
		if cerr := srv.Close(); cerr != nil {
			t.Errorf("SimServer.Close: %v", cerr)
		}
	}()
	conns := make([]*WireClient, 0, n)
	defer func() { closeWireClients(conns) }()
	for i := range n {
		c := dialAsPrincipal(ctx, t, srv, fmt.Sprintf("churn-%d", i))
		conns = append(conns, c)
		if berr := soakBegin(c, "r"); berr != nil {
			t.Fatalf("BEGIN %d: %v", i, berr)
		}
		want := i + 1
		if werr := waitForTxRegistry(func() bool { return len(srv.Server().Transactions()) == want },
			fmt.Sprintf("the registry never listed %d transaction(s)", want)); werr != nil {
			t.Fatalf("%v", werr)
		}
		if stagger {
			// Advanced only once the entry is registered, so each open lands on its own
			// instant rather than racing the one before it.
			probe.Advance(txListStaggerStep)
		}
	}
	// The arrangement itself, verified rather than assumed: a "distinct" run whose
	// instants had collided would measure the same degenerate sort as the other one
	// and the comparison would be meaningless.
	txs := srv.Server().Transactions()
	seen := make(map[time.Time]bool, len(txs))
	for i := range txs {
		seen[txs[i].StartedAt] = true
	}
	if stagger && len(seen) != n {
		t.Fatalf("the staggered arrangement produced %d distinct instants over %d entries, want %d: the sort would "+
			"not be doing the work this case exists to measure", len(seen), n, n)
	}
	if !stagger && len(seen) != 1 {
		t.Fatalf("the same-instant arrangement produced %d distinct instants, want 1", len(seen))
	}

	const calls = 200
	start := time.Now()
	for range calls {
		if got := len(srv.Server().Transactions()); got != n {
			t.Fatalf("Transactions() returned %d entries, want %d", got, n)
		}
	}
	return time.Since(start) / calls
}

// closeWireClients closes every client in cs, ignoring the errors: these are
// teardown paths on a server the test is about to shut down anyway.
func closeWireClients(cs []*WireClient) {
	for _, c := range cs {
		_ = c.Close()
	}
}

// dialAsPrincipal opens a connection and authenticates it as principal.
func dialAsPrincipal(ctx context.Context, t *testing.T, srv *SimServer, principal string) *WireClient {
	t.Helper()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	resp, err := c.ConnectAs(ctx, principal, "")
	if err != nil {
		t.Fatalf("ConnectAs(%q): %v", principal, err)
	}
	if !isSuccess(resp) {
		t.Fatalf("ConnectAs(%q) refused: %s %s", principal, failureCode(resp), failureMessage(resp))
	}
	return c
}

// soakBegin sends BEGIN in the given mode and requires a SUCCESS.
func soakBegin(c *WireClient, mode string) error {
	if err := armTxReadDeadline(c); err != nil {
		return err
	}
	resp, err := c.BeginMode(mode)
	if err != nil {
		return err
	}
	if !isSuccess(resp) {
		return fmt.Errorf("BEGIN refused: %s %s", failureCode(resp), failureMessage(resp))
	}
	return clearTxReadDeadline(c)
}

// soakReset sends RESET and requires a SUCCESS. It is what clears the FAILED
// state and the pending termination failure the reaper armed.
func soakReset(c *WireClient) error {
	if err := armTxReadDeadline(c); err != nil {
		return err
	}
	resp, err := c.Reset()
	if err != nil {
		return err
	}
	if !isSuccess(resp) {
		return fmt.Errorf("RESET refused: %s %s", failureCode(resp), failureMessage(resp))
	}
	return clearTxReadDeadline(c)
}
