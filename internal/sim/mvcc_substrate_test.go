package sim

// mvcc_substrate_test.go — validation of the MVCC-substrate telemetry oracle
// (rmp #2470): the live churn and abort arms, the commit-quiescence boundary,
// and the sensitivity proofs that every clause can actually fire.
//
// The sensitivity work is deliberately in two halves, because the two kinds of
// proof answer different questions:
//
//   - LIVE proofs, driven by a real engine, that the oracle responds to real
//     pressure and does NOT cry wolf at legitimate behaviour: a run too small to
//     wake the vacuum must be REJECTED as vacuous, and a long-lived reader that
//     genuinely stalls the watermark and deepens every chain must NOT be
//     reported as a defect;
//   - FABRICATED-EVIDENCE tables that drive each clause individually, because a
//     checker whose clauses cannot be reached one at a time is one whose green
//     runs prove nothing — and the unperturbed control must stay silent, or the
//     firings would only show the oracle rejects everything.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// -----------------------------------------------------------------------------
// Live arms
// -----------------------------------------------------------------------------

// TestMVCCSubstrate_ChurnCleanAndNonVacuous is the green gate for the churn arm:
// a run that crosses the vacuum's wake threshold must adjudicate clean AND be
// shown non-vacuous — the vacuum really swept, really released records, and
// really published a chain-depth distribution with nothing pinning it.
func TestMVCCSubstrate_ChurnCleanAndNonVacuous(t *testing.T) {
	defer goleak.VerifyNone(t)
	res, err := RunMVCCSubstrateChurn(context.Background(), MVCCSubstrateConfig{Seed: 11})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Clean() {
		t.Fatalf("violations on HEAD:\n%s\nevidence: %s", renderViolations(res.Violations), res.Summary)
	}
	// The non-vacuity gate above already refuses a run that proved nothing;
	// these assertions record WHAT it measured, so a regression that quietly
	// weakens the workload shows up as a number rather than as a silent pass.
	if !res.CrossedBound {
		t.Fatalf("churn never crossed the vacuum wake threshold: %s", res.Summary)
	}
	if res.Sweeps == 0 || res.ReclaimedRecords == 0 {
		t.Fatalf("the vacuum did not sweep or released nothing: %s", res.Summary)
	}
	if res.WatermarkTo <= res.WatermarkFrom {
		t.Fatalf("the watermark did not advance: %s", res.Summary)
	}
	if res.UnpinnedChainSamples == 0 {
		t.Fatalf("no unpinned chain-depth reading, so the depth bound was applied to nothing: %s", res.Summary)
	}
	t.Logf("%s", res.Summary)
}

// TestMVCCSubstrate_ChurnWithCheckpoints drives the commit-quiescence boundary:
// every checkpoint runs phase 1 under closed writer admission and waits out the
// durable-but-unpublished window, so the reading taken immediately after it
// returns must show no commit allocated and unpublished.
func TestMVCCSubstrate_ChurnWithCheckpoints(t *testing.T) {
	defer goleak.VerifyNone(t)
	res, err := RunMVCCSubstrateChurn(context.Background(), MVCCSubstrateConfig{
		Seed: 23, Checkpoints: 3,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Clean() {
		t.Fatalf("violations on HEAD:\n%s\nevidence: %s", renderViolations(res.Violations), res.Summary)
	}
	if res.CheckpointsRun != 3 {
		t.Fatalf("checkpoints run = %d, want 3 — the quiescence boundary was not exercised as intended", res.CheckpointsRun)
	}
	t.Logf("%s", res.Summary)
}

// TestMVCCSubstrate_AbortArmWithdrawsVersions is the abort-heavy arm: forced
// serialization conflicts, with the substrate read at the drain point. The
// refused transactions' versions must not be in the live count.
func TestMVCCSubstrate_AbortArmWithdrawsVersions(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for _, seed := range []uint64{1, 7, 42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			res, cont, err := RunMVCCSubstrateAborts(ctx, MVCCContentionConfig{
				Seed: seed, Ticks: 600, Sessions: 6, Counters: 2,
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !res.Clean() {
				t.Fatalf("violations on HEAD:\n%s\nevidence: %s", renderViolations(res.Violations), res.Summary)
			}
			// Non-vacuity: the arm must really have contended.
			if cont.TxConflicted == 0 {
				t.Fatalf("no serialization conflict was produced, so nothing was aborted: %+v", cont)
			}
			if res.Aborts == 0 || res.Conflicts == 0 {
				t.Fatalf("the substrate counted no abort/conflict against %d client refusals: %s",
					cont.TxConflicted, res.Summary)
			}
			t.Logf("client refusals=%d | %s", cont.TxConflicted, res.Summary)
		})
	}
}

// -----------------------------------------------------------------------------
// Live sensitivity
// -----------------------------------------------------------------------------

// TestMVCCSubstrate_TooSmallRunIsRejectedAsVacuous is the live proof that the
// non-vacuity gate is really wired into the run, and is the specific trap this
// oracle was warned about: a workload that never crosses the vacuum's wake
// threshold satisfies every reclamation clause by never asking one.
//
// Measured: 200 committed writes leave ~200 live version records against a wake
// threshold of 4096, so the vacuum never starts, `passes=0, reclaimed=0`, and
// the ADJUDICATION is clean — it is only the non-vacuity gate that refuses it.
func TestMVCCSubstrate_TooSmallRunIsRejectedAsVacuous(t *testing.T) {
	defer goleak.VerifyNone(t)
	res, err := RunMVCCSubstrateChurn(context.Background(), MVCCSubstrateConfig{
		Seed: 31, Rounds: 200, SampleEvery: 10,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Clean() {
		t.Fatalf("a run that never woke the vacuum was accepted as proving something: %s", res.Summary)
	}
	if res.CrossedBound {
		t.Fatalf("the undersized run crossed the bound after all, so it proves nothing about the gate: %s", res.Summary)
	}
	// It must be the NON-VACUITY gate that fired, not the adjudication: a small
	// run is uninformative, not faulty.
	for _, v := range res.Violations {
		if v.Kind != ViolationOracleDeviation {
			t.Fatalf("an undersized run produced a substantive violation, so the oracle reports a defect where"+
				" there is none: %+v", v)
		}
	}
	if !containsViolation(res.Violations, "MVCC substrate telemetry non-vacuity",
		"never put under reclamation pressure") {
		t.Fatalf("the wake-threshold clause did not fire:\n%s", renderViolations(res.Violations))
	}
	t.Logf("correctly rejected as vacuous: %s", res.Summary)
}

// TestMVCCSubstrate_PinnedReaderIsNotAViolation is the live SPECIFICITY proof.
//
// A long-lived reader legitimately stalls the reclamation watermark and deepens
// every retained chain — depth is measured after truncation below the
// watermark, so a reader that holds the watermark back holds every version it
// can still reach. An oracle that reported that as a defect would be unusable
// in exactly the scenarios it was built for, and an oracle whose depth clause
// fired here would be measuring the wrong thing.
//
// The run therefore asserts three things at once: the stall is REAL (the
// watermark does not move while the clock does), the chains really do deepen
// past the bound, and the oracle nevertheless reports nothing — because the
// registered snapshot explains both.
func TestMVCCSubstrate_PinnedReaderIsNotAViolation(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	disk := NewSimDisk(NewSeed(77), 0)
	st, err := OpenSimStore(disk, fullStackStoreConfig())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	sess := st.Engine().NewSession()
	for i := 0; i < 4; i++ {
		if err := substrateExec(ctx, sess, tmplSubstrateSeed,
			map[string]any{"name": substrateObjectName(i), "v": int64(0)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	ev := newMVCCSubstrateEvidence("pinned-reader specificity")
	ev.observe(st.Graph(), "before pin", true)

	// Pin the watermark with a reader that stays open across the whole churn.
	reader, err := sess.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin reader: %v", err)
	}
	if _, err := reader.ExecAny("MATCH (n:Person) RETURN count(n)", nil); err != nil {
		t.Fatalf("reader read: %v", err)
	}
	pinnedAt := st.Graph().MVCCStats().Watermark

	writer := st.Engine().NewSession()
	for r := 0; r < 6000; r++ {
		if err := substrateExec(ctx, writer, tmplSubstrateChurn,
			map[string]any{"name": substrateObjectName(r % 4), "v": int64(r)}); err != nil {
			t.Fatalf("churn %d: %v", r, err)
		}
		if (r+1)%50 == 0 {
			ev.observe(st.Graph(), fmt.Sprintf("pinned churn %d", r+1), false)
		}
	}
	final := ev.observe(st.Graph(), "pinned terminal", true)

	// The stall must be real, or this proves nothing about the guard.
	if final.watermark != pinnedAt {
		t.Fatalf("the reader did not pin the watermark (%d -> %d), so no stall was produced",
			pinnedAt, final.watermark)
	}
	if final.now <= pinnedAt {
		t.Fatalf("the clock did not advance past the pin (now=%d pinned=%d), so there was no stall to observe",
			final.now, pinnedAt)
	}
	if ev.maxActiveSnapshots == 0 {
		t.Fatalf("no snapshot was ever observed registered, so the guard was not exercised: %s", ev.summary())
	}
	// The chains really did deepen past the bound — otherwise the depth clause's
	// guard is untested, because there would be nothing for it to suppress.
	if ev.deepest <= maxRetainedChainDepth {
		t.Fatalf("retained depth reached only %d, not past the bound of %d: the pinned run did not produce the"+
			" deep chains whose suppression is the point of this test (%s)",
			ev.deepest, maxRetainedChainDepth, ev.summary())
	}

	if v := checkMVCCSubstrate(1, ev); len(v) != 0 {
		t.Fatalf("legitimate reader-pinned retention was reported as a defect:\n%s\nevidence: %s",
			renderViolations(v), ev.summary())
	}
	_ = reader.Rollback()
	t.Logf("stall pinned at watermark=%d while clock reached %d; retained depth %d (unpinned %d); no violation: %s",
		pinnedAt, final.now, ev.deepest, ev.deepestUnpinned, ev.summary())
}

// -----------------------------------------------------------------------------
// Clause-firing tables
// -----------------------------------------------------------------------------

// healthySubstrateEvidence fabricates the folded evidence of a well-behaved run
// that crossed the vacuum's wake threshold. It is the control for the
// perturbation tables below.
func healthySubstrateEvidence() *mvccSubstrateEvidence {
	base := mvccSubstrateSample{
		label: "fabricated", quiescent: true,
		total: 32, bound: 4096, ceiling: 16384,
		watermark: 100, now: 100,
		capacity: 1024,
		write:    mvcc.WriteCounts{Commits: 10},
	}
	e := newMVCCSubstrateEvidence("fabricated healthy run")
	e.fold(&base)

	// A mid-run reading that crosses the bound and carries a sweep's depth
	// distribution, with nothing registered.
	mid := base
	mid.label, mid.quiescent = "fabricated mid-churn", false
	mid.total, mid.backlog = 5000, 5000
	mid.watermark, mid.now = 3000, 3000
	mid.write = mvcc.WriteCounts{Commits: 3000}
	mid.passes, mid.reclaimed = 40, 4000
	mid.depth = mvcc.Depths{Buckets: [mvcc.DepthBuckets]uint64{7: 0, 0: 12}, Deepest: 1}
	e.fold(&mid)

	last := base
	last.label = "fabricated terminal"
	last.watermark, last.now = 6000, 6000
	last.write = mvcc.WriteCounts{Commits: 6000}
	last.passes, last.reclaimed = 80, 6000
	e.fold(&last)
	return e
}

// TestMVCCSubstrate_ClausesFire drives every clause of [checkMVCCSubstrate]
// from fabricated evidence, one perturbation at a time. A checker whose clauses
// cannot be reached individually is one whose green runs prove nothing.
func TestMVCCSubstrate_ClausesFire(t *testing.T) {
	if v := checkMVCCSubstrate(1, healthySubstrateEvidence()); len(v) != 0 {
		t.Fatalf("the healthy control reports violations, so every firing below would only show the oracle"+
			" rejects everything: %s", renderViolations(v))
	}
	tests := []struct {
		name    string
		mutate  func(*mvccSubstrateEvidence)
		wantSub string
		wantKnd ViolationKind
	}{
		{
			name:    "never sampled",
			mutate:  func(e *mvccSubstrateEvidence) { *e = *newMVCCSubstrateEvidence("empty") },
			wantSub: "never sampled",
			wantKnd: ViolationACIDConsistency,
		},
		{
			name:    "no quiescent reading",
			mutate:  func(e *mvccSubstrateEvidence) { e.quiesced = 0 },
			wantSub: "no reading was taken at a declared quiescent point",
			wantKnd: ViolationACIDConsistency,
		},
		{
			name: "commit allocated and never published",
			mutate: func(e *mvccSubstrateEvidence) {
				e.quiesceInFlightBreaches, e.worstQuiesceInFlight = 1, 3
			},
			wantSub: "allocated and unpublished",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name:    "watermark moved backwards",
			mutate:  func(e *mvccSubstrateEvidence) { e.watermarkRegressed = true },
			wantSub: "moved BACKWARDS",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name: "watermark stalled with nothing pinning it",
			mutate: func(e *mvccSubstrateEvidence) {
				e.last.watermark = e.first.watermark
				e.maxActiveSnapshots = 0
			},
			wantSub: "watermark stalled",
			wantKnd: ViolationACIDConsistency,
		},
		{
			name:    "substrate reported a watermark regression",
			mutate:  func(e *mvccSubstrateEvidence) { e.wmRegressions = 2 },
			wantSub: "watermark regression",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name:    "horizon released a stale leaf",
			mutate:  func(e *mvccSubstrateEvidence) { e.staleLeaves = 1 },
			wantSub: "stale leaf release",
			wantKnd: ViolationACIDIsolation,
		},
		{
			name:    "a reader could not get a horizon slot",
			mutate:  func(e *mvccSubstrateEvidence) { e.unregistered = 4 },
			wantSub: "could not get a horizon slot",
			wantKnd: ViolationACIDConsistency,
		},
		{
			name: "version memory past the published ceiling",
			mutate: func(e *mvccSubstrateEvidence) {
				e.ceilingBreaches, e.worstCeilingExcess, e.maxTotal = 2, 500, 16884
			},
			wantSub: "exceeded the published ceiling",
			wantKnd: ViolationACIDConsistency,
		},
		{
			name: "vacuum woken but reclaimed nothing",
			mutate: func(e *mvccSubstrateEvidence) {
				e.first.reclaimed, e.last.reclaimed = 0, 0
			},
			wantSub: "released NOTHING",
			wantKnd: ViolationACIDConsistency,
		},
		{
			name: "retained chain depth past the bound, unpinned",
			mutate: func(e *mvccSubstrateEvidence) {
				e.deepestUnpinned, e.unpinnedChainSamples = maxRetainedChainDepth+1, 3
			},
			wantSub: "retained version-chain depth reached",
			wantKnd: ViolationACIDConsistency,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := healthySubstrateEvidence()
			tc.mutate(e)
			v := checkMVCCSubstrate(1, e)
			assertFired(t, v, tc.wantSub, tc.wantKnd)
		})
	}
}

// TestMVCCSubstrateNonVacuity_ClausesFire drives every clause of
// [checkMVCCSubstrateNonVacuity]. This is the gate that stops a run which
// proved nothing from being read as a run that proved something, so its own
// clauses must each be reachable.
func TestMVCCSubstrateNonVacuity_ClausesFire(t *testing.T) {
	healthy := func() *mvccSubstrateEvidence {
		e := healthySubstrateEvidence()
		e.unpinnedChainSamples = 2
		return e
	}
	if v := checkMVCCSubstrateNonVacuity(1, healthy()); len(v) != 0 {
		t.Fatalf("the healthy control is reported vacuous: %s", renderViolations(v))
	}
	tests := []struct {
		name    string
		mutate  func(*mvccSubstrateEvidence)
		wantSub string
	}{
		{"never sampled", func(e *mvccSubstrateEvidence) { *e = *newMVCCSubstrateEvidence("empty") }, "never sampled"},
		{"single reading", func(e *mvccSubstrateEvidence) { e.n = 1 }, "only 1 reading was folded"},
		{"no quiescent reading", func(e *mvccSubstrateEvidence) { e.quiesced = 0 }, "no quiescent reading"},
		{"nothing committed", func(e *mvccSubstrateEvidence) {
			e.last.write.Commits = 0
		}, "no transaction committed"},
		{"never woke the vacuum", func(e *mvccSubstrateEvidence) {
			e.crossedBound = false
			e.first.passes, e.last.passes = 0, 0
		}, "never put under reclamation pressure"},
		{"no sweep ran", func(e *mvccSubstrateEvidence) { e.first.passes, e.last.passes = 0, 0 }, "ran no sweep"},
		{"nothing reclaimed", func(e *mvccSubstrateEvidence) { e.first.reclaimed, e.last.reclaimed = 0, 0 }, "released no record"},
		{"no depth distribution at all", func(e *mvccSubstrateEvidence) {
			e.chainSamples, e.unpinnedChainSamples = 0, 0
		}, "non-empty chain-depth distribution"},
		{"every depth reading was pinned", func(e *mvccSubstrateEvidence) {
			e.unpinnedChainSamples = 0
		}, "was taken with a snapshot registered"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := healthy()
			tc.mutate(e)
			assertFired(t, checkMVCCSubstrateNonVacuity(1, e), tc.wantSub, ViolationOracleDeviation)
		})
	}
}

// TestMVCCAbortWithdrawal_ClausesFire drives every clause of
// [checkMVCCAbortWithdrawal] from fabricated evidence.
func TestMVCCAbortWithdrawal_ClausesFire(t *testing.T) {
	healthyCont := func() *MVCCContentionResult {
		return &MVCCContentionResult{TxCommitted: 59, TxConflicted: 29, TypedConflicts: 29, Sessions: 6, Counters: 2}
	}
	healthyEv := func() *mvccSubstrateEvidence {
		e := newMVCCSubstrateEvidence("fabricated abort arm")
		drain := mvccSubstrateSample{
			label: "drain", quiescent: true, total: 34, bound: 4096, ceiling: 16384,
			watermark: 500, now: 500, capacity: 1024,
			write: mvcc.WriteCounts{Commits: 59, Aborts: 29, Conflicts: 29},
		}
		e.fold(&drain)
		return e
	}
	if v := checkMVCCAbortWithdrawal(1, healthyEv(), healthyCont()); len(v) != 0 {
		t.Fatalf("the healthy control reports violations: %s", renderViolations(v))
	}
	tests := []struct {
		name    string
		ev      func() *mvccSubstrateEvidence
		cont    func() *MVCCContentionResult
		wantSub string
		wantKnd ViolationKind
	}{
		{
			name:    "never read at the drain point",
			ev:      func() *mvccSubstrateEvidence { return newMVCCSubstrateEvidence("empty") },
			cont:    healthyCont,
			wantSub: "never read at the drain point",
			wantKnd: ViolationACIDAtomicity,
		},
		{
			name: "no conflict was produced",
			ev:   healthyEv,
			cont: func() *MVCCContentionResult {
				c := healthyCont()
				c.TxConflicted = 0
				return c
			},
			wantSub: "NO serialization conflict",
			wantKnd: ViolationOracleDeviation,
		},
		{
			name: "client saw refusals the substrate did not count",
			ev: func() *mvccSubstrateEvidence {
				e := healthyEv()
				e.last.write.Aborts = 0
				return e
			},
			cont:    healthyCont,
			wantSub: "counted ZERO aborts",
			wantKnd: ViolationACIDAtomicity,
		},
		{
			name: "aborts attributed to no conflict store",
			ev: func() *mvccSubstrateEvidence {
				e := healthyEv()
				e.last.write.Conflicts = 0
				return e
			},
			cont:    healthyCont,
			wantSub: "attributed NONE of them",
			wantKnd: ViolationOracleDeviation,
		},
		{
			name: "aborted versions look retained",
			ev: func() *mvccSubstrateEvidence {
				e := healthyEv()
				// One retained version per abort on top of the working set.
				e.last.total = int64(8 + 29 + 1)
				return e
			},
			cont:    healthyCont,
			wantSub: "look RETAINED rather than withdrawn",
			wantKnd: ViolationACIDAtomicity,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertFired(t, checkMVCCAbortWithdrawal(1, tc.ev(), tc.cont()), tc.wantSub, tc.wantKnd)
		})
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// assertFired requires that exactly the expected clause fired, by message
// substring and violation kind.
func assertFired(t *testing.T, got []Violation, wantSub string, wantKind ViolationKind) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("no violation fired; want one containing %q", wantSub)
	}
	for _, v := range got {
		if strings.Contains(v.Message, wantSub) {
			if v.Kind != wantKind {
				t.Fatalf("violation kind = %q, want %q (message %q)", v.Kind, wantKind, v.Message)
			}
			return
		}
	}
	t.Fatalf("no violation contained %q; got:\n%s", wantSub, renderViolations(got))
}

// renderViolations formats violations one per line for a failure message.
func renderViolations(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "  [%s] %s: %s\n", v.Kind, v.Op, v.Message)
	}
	return b.String()
}
