package sim

// wal_writer_surface_test.go — the wiring and the falsifiability proofs for the
// WAL writer-surface oracles (rmp #2472).
//
// Every gate here is proved falsifiable as well as satisfied. A gate that only
// ever passes is indistinguishable from a gate that cannot fail, and this cycle
// has already found eight guards that existed and proved nothing — two of them
// written to this project's own specification, and one hiding inside a metric.
//
// The falsification comes in two forms, deliberately:
//
//   - a DOCTORED record handed to the pure adjudicator, which cannot flake and
//     proves the clause is wired; and
//   - where a real seam exists, a LIVE control arm through the same writer, which
//     proves the property is produced by the mechanism named rather than by
//     something incidental. The contiguity control is the important one: the same
//     workload through per-frame Append really does interleave.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// walSurfaceSeed is the master seed for the arms in this file.
const walSurfaceSeed uint64 = 0x2472_A11C_0FFE_E51E

// -----------------------------------------------------------------------------
// Part 1 — the durability watermark
// -----------------------------------------------------------------------------

// TestWALWatermark_DirectExact is the EXACT arm: the harness chose every payload,
// so the durable offset must equal the watermark AppendRun returned and the
// lifetime counters must equal the accumulation over the frames emitted. Nothing
// is inferred from a byte size the run happened to produce.
func TestWALWatermark_DirectExact(t *testing.T) {
	t.Parallel()

	ev, err := RunWALWatermarkDirect(context.Background(), walSurfaceSeed)
	if err != nil {
		t.Fatalf("RunWALWatermarkDirect: %v", err)
	}
	t.Log(ev)

	if v := checkWALWatermarkNonVacuity(ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the run proved nothing about the watermark; the verdict below would be meaningless")
	}
	for _, viol := range checkWALWatermark(ev) {
		t.Errorf("watermark: %s", viol)
	}
	if !ev.Exact {
		t.Error("the direct arm did not carry exact expectations, so its strongest clauses were skipped")
	}
}

// TestWALWatermark_EngineIsSizeAgnostic is the arm against the REAL stack, and it
// is also the standing proof that the oracle does not depend on an absolute byte
// size. The engine's commit markers encode the instant they were written, so the
// durable image is not byte-stable across runs (rmp #2521); the clauses the
// oracle applies here are monotonicity, the accepted-bytes ceiling and the
// frame-boundary relation, none of which reference a constant.
func TestWALWatermark_EngineIsSizeAgnostic(t *testing.T) {
	t.Parallel()

	ev, err := RunWALWatermarkEngine(context.Background(), walSurfaceSeed)
	if err != nil {
		t.Fatalf("RunWALWatermarkEngine: %v", err)
	}
	t.Log(ev)

	if v := checkWALWatermarkNonVacuity(ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the engine run proved nothing about the watermark")
	}
	for _, viol := range checkWALWatermark(ev) {
		t.Errorf("watermark: %s", viol)
	}
	if ev.Exact {
		t.Error("the engine arm claims exact expectations, which it cannot have: the engine composes the payloads")
	}
}

// TestWALWatermark_GateDetectsEachDefect is the falsifiability proof. Each
// subtest hands the adjudicator a record that differs from a healthy one in ONE
// respect, and asserts the matching clause fires with a message that names what
// went wrong. Without this, a clause that silently stopped being evaluated would
// read exactly like a clause that keeps passing.
func TestWALWatermark_GateDetectsEachDefect(t *testing.T) {
	t.Parallel()

	// A healthy two-commit record: 30 then 60 bytes, both on frame boundaries.
	healthy := func() WALWatermarkEvidence {
		return WALWatermarkEvidence{
			Label:   "doctored",
			Commits: 4,
			Samples: []walWatermarkSample{
				{durable: 30, appended: 30, frames: 1, syncs: 1, imageLen: 30, boundary: 30, boundaryOK: true},
				{durable: 60, appended: 60, frames: 2, syncs: 2, imageLen: 60, boundary: 60, boundaryOK: true},
				{durable: 90, appended: 90, frames: 3, syncs: 3, imageLen: 90, boundary: 90, boundaryOK: true},
				{durable: 120, appended: 120, frames: 4, syncs: 4, imageLen: 120, boundary: 120, boundaryOK: true},
			},
		}
	}
	if v := checkWALWatermark(healthy()); len(v) > 0 {
		t.Fatalf("the healthy control record was rejected, so every subtest below would be meaningless:\n%v", v)
	}

	cases := []struct {
		name    string
		doctor  func(*WALWatermarkEvidence)
		wantMsg string
	}{
		{
			name:    "durable offset goes backwards",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[2].durable, e.Samples[2].boundary = 45, 45 },
			wantMsg: "WENT BACKWARDS",
		},
		{
			name:    "durable offset does not advance on an acknowledged commit",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[2].durable, e.Samples[2].boundary = 60, 60 },
			wantMsg: "did not advance past",
		},
		{
			name:    "a lifetime counter goes backwards",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[2].frames = 1 },
			wantMsg: "counter went backwards",
		},
		{
			name:    "the watermark covers bytes never appended",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[3].appended = 100 },
			wantMsg: "exceeds the",
		},
		{
			name:    "the offset is not on a frame boundary",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[3].boundary = 119 },
			wantMsg: "frame-boundary accumulation",
		},
		{
			name:    "the durable image is torn",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[3].boundaryOK = false },
			wantMsg: "clean tail",
		},
		{
			name:    "an acknowledged commit sits on a poisoned writer",
			doctor:  func(e *WALWatermarkEvidence) { e.Samples[3].poisoned = wal.ErrDurabilityFailed },
			wantMsg: "poisoned",
		},
		{
			name: "the exact watermark disagrees with AppendRun's return",
			doctor: func(e *WALWatermarkEvidence) {
				e.Exact = true
				for i := range e.Samples {
					s := &e.Samples[i]
					s.exact, s.wantDurable, s.wantAppended, s.wantFrames = true, s.durable, s.appended, s.frames
				}
				e.Samples[3].wantDurable = 121
			},
			wantMsg: "watermark 121 that AppendRun returned",
		},
		{
			name: "the exact accumulation disagrees with the payloads emitted",
			doctor: func(e *WALWatermarkEvidence) {
				e.Exact = true
				for i := range e.Samples {
					s := &e.Samples[i]
					s.exact, s.wantDurable, s.wantAppended, s.wantFrames = true, s.durable, s.appended, s.frames
				}
				e.Samples[3].wantAppended = 118
			},
			wantMsg: "payloads emitted account for exactly",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := healthy()
			tc.doctor(&ev)
			v := checkWALWatermark(ev)
			if len(v) == 0 {
				t.Fatalf("the gate accepted a record with %q: the clause cannot fail and therefore proves nothing", tc.name)
			}
			joined := violationText(v)
			if !strings.Contains(joined, tc.wantMsg) {
				t.Errorf("the violation does not explain the defect (want a mention of %q):\n%s", tc.wantMsg, joined)
			}
		})
	}
}

// TestWALWatermark_NonVacuityGateIsSeparate proves the two gates are genuinely
// independent (rmp #2470): a trivial population must be reported by the
// NON-VACUITY gate, and must not be dressed up as a durability defect.
func TestWALWatermark_NonVacuityGateIsSeparate(t *testing.T) {
	t.Parallel()

	// One sample: monotonicity is undefined over a single point, which is exactly
	// the window-quantity trap the sprint standard names.
	single := WALWatermarkEvidence{
		Label: "trivial", Commits: 1,
		Samples: []walWatermarkSample{{durable: 30, appended: 30, frames: 1, syncs: 1, imageLen: 30, boundary: 30, boundaryOK: true}},
	}
	v := checkWALWatermarkNonVacuity(single)
	if len(v) == 0 {
		t.Fatal("the non-vacuity gate accepted a one-sample run: a monotonicity claim over a single point cannot fail")
	}
	if !strings.Contains(violationText(v), "sequence, not a point") {
		t.Errorf("the non-vacuity violation does not say why one sample is not enough:\n%s", violationText(v))
	}
	// And the property gate must stay SILENT on it: uninformative is not faulty.
	if pv := checkWALWatermark(single); len(pv) > 0 {
		t.Errorf("the property gate reported %d violation(s) on a merely uninformative run, which would be read as a writer defect:\n%v", len(pv), pv)
	}

	// A run that never got a byte to the platter is likewise uninformative.
	empty := WALWatermarkEvidence{
		Label: "empty", Commits: 4,
		Samples: []walWatermarkSample{
			{boundaryOK: true}, {boundaryOK: true}, {boundaryOK: true}, {boundaryOK: true},
		},
	}
	ev := checkWALWatermarkNonVacuity(empty)
	if !strings.Contains(violationText(ev), "never left zero") {
		t.Errorf("a run whose watermark never advanced was not reported as uninformative:\n%s", violationText(ev))
	}
}

// -----------------------------------------------------------------------------
// Part 2 — per-transaction frame contiguity under concurrency
// -----------------------------------------------------------------------------

// TestWALContiguity_AlternatingPerFrameSplitsDeterministically is the CONTROL,
// and it is a construction rather than a hope: two committers hand a token back
// and forth, so exactly one is eligible to append at any instant and the durable
// image is fully determined by the protocol. Per-frame [wal.Writer.Append]
// releases the writer mutex between frames, so the partner's frame really does
// land in the middle and every transaction ends up in exactly FramesPerTx
// fragments.
//
// It replaces a concurrent control that asserted a SCHEDULING outcome. That one
// passed on an idle machine (31 of 96 transactions split) and failed under the
// coverage step with the suite running in parallel, where all four retries
// measured 7 committer switches and zero splits — the scheduler simply never
// overlapped the committers. An assertion the machine may refuse to satisfy
// measures machine load, not the module (rmp #2517).
//
// The whole layout is asserted, not merely "at least one split": a lost frame, a
// skipped handoff, or an append path that stopped releasing the mutex between
// frames each change one of the numbers.
func TestWALContiguity_AlternatingPerFrameSplitsDeterministically(t *testing.T) {
	t.Parallel()

	ev, err := RunWALContiguityAlternating(context.Background(), walSurfaceSeed, true)
	if err != nil {
		t.Fatalf("RunWALContiguityAlternating(perFrame): %v", err)
	}
	t.Log(ev)

	for _, viol := range checkWALContiguityAlternatingLayout(&ev) {
		t.Errorf("constructed layout: %s", viol)
	}

	// The constructed image must FALSIFY the contiguity gate — that is what the
	// control exists for.
	v := checkWALContiguity(&ev)
	if len(v) == 0 {
		t.Fatalf("the contiguity gate PASSED a deterministically interleaved image (%s): it cannot fail and therefore proves nothing", ev)
	}
	if msg := violationText(v); !strings.Contains(msg, "NOT contiguous") || !strings.Contains(msg, "orphaned") {
		t.Errorf("the contiguity violation does not explain what breaks (recovery dropping orphaned ops):\n%s", msg)
	}
	if v[0].Kind != ViolationACIDAtomicity {
		t.Errorf("an interleaved transaction was classified %s; it is an Atomicity violation", v[0].Kind)
	}
}

// TestWALContiguity_AlternatingAppendRunStaysContiguous is the other half of the
// pair, and it is what attributes the contiguity to [wal.Writer.AppendRun]. The
// handoff is identical to the control's; only the append API changes. A signals
// that it is INSIDE its run and B immediately attempts its own append — and
// cannot get in, because AppendRun holds the writer mutex across the whole run.
// The resulting two-run image is decided by the MUTEX, not by the scheduler, so
// this arm is as deterministic as the control.
//
// The signal is one-way on purpose: a full ping-pong would deadlock here, since A
// would be waiting for B while holding the mutex B needs. That deadlock IS the
// mechanism under test, so the protocol must not depend on it resolving.
func TestWALContiguity_AlternatingAppendRunStaysContiguous(t *testing.T) {
	t.Parallel()

	ev, err := RunWALContiguityAlternating(context.Background(), walSurfaceSeed, false)
	if err != nil {
		t.Fatalf("RunWALContiguityAlternating(AppendRun): %v", err)
	}
	t.Log(ev)

	for _, viol := range checkWALContiguityAlternatingLayout(&ev) {
		t.Errorf("constructed layout: %s", viol)
	}
	for _, viol := range checkWALContiguity(&ev) {
		t.Errorf("contiguity: %s", viol)
	}
	// Stated explicitly: the partner had its chance and was excluded.
	if ev.Runs != 2 || ev.SplitTransactions != 0 {
		t.Errorf("the second committer's frames entered the first's run (%s): AppendRun did not hold the writer across its run", ev)
	}
}

// TestWALContiguity_AppendRunIsContiguousUnderConcurrency asserts the property
// [wal.Writer.AppendRun] claims, under real concurrency: with 8 committers every
// transaction's frames form ONE contiguous run in the durable file.
//
// This is the claim that was previously unverified. Before commit 9eee3b18 the
// contiguity came from the store's single-writer semaphore two layers up, and
// store/recovery discards a transaction's buffered prefix as orphaned on the
// stated ground that frames never interleave — so a violation here is a real
// Atomicity finding, not a harness complaint.
//
// The verdict is asserted UNCONDITIONALLY: a split is a defect however little
// concurrency produced it. Whether the machine actually granted concurrency is a
// separate question, reported by [checkWALContiguityConcurrencyWitness] as
// uninformative rather than as a failure — it measures the scheduler, and the
// deterministic evidence lives in the two alternating arms above.
func TestWALContiguity_AppendRunIsContiguousUnderConcurrency(t *testing.T) {
	t.Parallel()

	cfg := WALContiguityConfig{Seed: walSurfaceSeed, Committers: walContiguityMinCommitters}
	ev, err := RunWALContiguity(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWALContiguity: %v", err)
	}
	t.Log(ev)

	wantTx := walContiguityMinCommitters * walContiguityDefaultTx
	if v := checkWALContiguityNonVacuity(&ev, wantTx); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the run proved nothing about contiguity; the verdict below would be meaningless")
	}
	// UNCONDITIONAL: a split fails here whatever the scheduler did.
	for _, viol := range checkWALContiguity(&ev) {
		t.Errorf("contiguity: %s", viol)
	}
	// Reported, never asserted: this one measures the machine.
	for _, viol := range checkWALContiguityConcurrencyWitness(&ev) {
		t.Logf("UNINFORMATIVE (not a failure): %s", viol)
	}
}

// TestWALContiguity_NonVacuityRejectsSingleFrameTransactions pins the trap that
// would make the whole census worthless: a transaction of ONE frame is
// contiguous by definition, so a run shaped that way yields a permanently green
// gate. The non-vacuity gate must reject the shape rather than report a pass.
func TestWALContiguity_NonVacuityRejectsSingleFrameTransactions(t *testing.T) {
	t.Parallel()

	cfg := WALContiguityConfig{
		Seed: walSurfaceSeed, Committers: walContiguityMinCommitters,
		TxPerCommitter: 4, FramesPerTx: 1,
	}
	ev, err := RunWALContiguity(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWALContiguity (single-frame): %v", err)
	}
	t.Log(ev)

	// The property gate is satisfied — vacuously, which is the point.
	if pv := checkWALContiguity(&ev); len(pv) > 0 {
		t.Fatalf("a single-frame run produced contiguity violations, which is impossible:\n%v", pv)
	}
	v := checkWALContiguityNonVacuity(&ev, walContiguityMinCommitters*4)
	if !strings.Contains(violationText(v), "contiguous by definition") {
		t.Errorf("the non-vacuity gate did not reject a single-frame-per-transaction shape:\n%s", violationText(v))
	}
}

// TestWALContiguity_ConcurrencyWitnessIsSeparateFromTheVerdict proves the third
// gate is genuinely independent, and independent in BOTH directions. That
// separation is the fix for the failure this arm actually suffered: under the
// coverage step the machine ran eight committers sequentially (7 switches), and
// a gate that treated that as a defect reddened a green tree (rmp #2517).
//
//   - A run the machine denied concurrency is reported by the WITNESS as
//     uninformative, and neither the non-vacuity gate nor the verdict says
//     anything about it.
//   - A run that DID interleave and still split is a defect the verdict reports
//     regardless — the witness never excuses a split.
func TestWALContiguity_ConcurrencyWitnessIsSeparateFromTheVerdict(t *testing.T) {
	t.Parallel()

	// The real shape observed under coverage: sound population, no concurrency.
	denied := WALContiguityEvidence{
		Committers: walContiguityMinCommitters, FramesPerTx: 4,
		Transactions: 96, Frames: 384, Runs: 96, CommitterSwitches: 7,
	}
	w := checkWALContiguityConcurrencyWitness(&denied)
	if len(w) == 0 {
		t.Fatal("a run with 7 committer switches across 96 transactions was not reported as uninformative about concurrency")
	}
	if msg := violationText(w); !strings.Contains(msg, "essentially") || !strings.Contains(msg, "verdict still stands") {
		t.Errorf("the witness does not say that the verdict is unaffected:\n%s", msg)
	}
	if v := checkWALContiguityNonVacuity(&denied, 96); len(v) > 0 {
		t.Errorf("the non-vacuity gate rejected a run whose only shortfall is the machine's scheduling:\n%v", v)
	}
	if pv := checkWALContiguity(&denied); len(pv) > 0 {
		t.Errorf("the verdict reported a defect on a contiguous image:\n%v", pv)
	}

	// The other direction: no concurrency, but a split — still a defect.
	deniedAndSplit := denied
	deniedAndSplit.Runs, deniedAndSplit.SplitTransactions, deniedAndSplit.WorstFragments = 98, 2, 2
	if pv := checkWALContiguity(&deniedAndSplit); len(pv) == 0 {
		t.Error("a split image passed the verdict because the run saw little concurrency: a split is a defect however it arose")
	}

	// And a healthy concurrent run must make the witness silent.
	healthy := denied
	healthy.CommitterSwitches = walContiguityMinSwitches
	if w := checkWALContiguityConcurrencyWitness(&healthy); len(w) > 0 {
		t.Errorf("the witness fired on a run that did interleave:\n%v", w)
	}
}

// TestWALContiguity_AlternatingLayoutGateDetectsDrift proves the constructed
// layout is asserted rather than merely recorded: if the handoff ever stopped
// producing the exact image its protocol determines, the arm must say so instead
// of quietly adjudicating a different picture.
func TestWALContiguity_AlternatingLayoutGateDetectsDrift(t *testing.T) {
	t.Parallel()

	for _, perFrame := range []bool{true, false} {
		want := walContiguityAlternatingWant(perFrame)
		if v := checkWALContiguityAlternatingLayout(&want); len(v) > 0 {
			t.Fatalf("perFrame=%t: the expected layout was rejected by its own gate:\n%v", perFrame, v)
		}
		drifted := want
		drifted.Runs++
		v := checkWALContiguityAlternatingLayout(&drifted)
		if len(v) == 0 {
			t.Errorf("perFrame=%t: the layout gate accepted a run count that the protocol cannot produce", perFrame)
		}
		if msg := violationText(v); !strings.Contains(msg, "maximal runs") {
			t.Errorf("perFrame=%t: the layout violation does not name the field that drifted:\n%s", perFrame, msg)
		}
	}
}

// TestWALContiguity_GateDetectsASplitTransaction is the deterministic
// falsifiability proof, kept alongside the live control so the gate's wiring
// does not depend on the scheduler cooperating.
func TestWALContiguity_GateDetectsASplitTransaction(t *testing.T) {
	t.Parallel()

	split := WALContiguityEvidence{
		Committers: walContiguityMinCommitters, FramesPerTx: 4,
		Transactions: 96, Frames: 384, Runs: 97, SplitTransactions: 1, WorstFragments: 2,
		CommitterSwitches: 30,
	}
	if v := checkWALContiguityNonVacuity(&split, 96); len(v) > 0 {
		t.Fatalf("the doctored record is not a sound population, so the verdict below is meaningless:\n%v", v)
	}
	v := checkWALContiguity(&split)
	if len(v) != 1 {
		t.Fatalf("the gate reported %d violation(s) for one split transaction; want exactly 1:\n%v", len(v), v)
	}
	if !strings.Contains(v[0].Message, "1 of 96") {
		t.Errorf("the violation does not quantify the split:\n%s", v[0].Message)
	}
}

// -----------------------------------------------------------------------------
// Part 3 — whole-file Truncate and the poisoned-writer contract
// -----------------------------------------------------------------------------

// TestWALLifecycle_TruncateAndPoisonContract exercises the whole-file
// [wal.Writer.Truncate] and the poisoned-writer surface, and holds both to the
// contract as MEASURED. Two members of that contract are undocumented —
// SyncBuffered and Truncate on a poisoned writer — and are pinned here precisely
// so a change surfaces as a failure to be judged rather than as silent drift.
func TestWALLifecycle_TruncateAndPoisonContract(t *testing.T) {
	t.Parallel()

	r, err := RunWALLifecycle(context.Background(), walSurfaceSeed)
	if err != nil {
		t.Fatalf("RunWALLifecycle: %v", err)
	}
	t.Log(r)

	for _, viol := range checkWALLifecycle(&r) {
		t.Errorf("lifecycle: %s", viol)
	}

	// The two measured-not-documented facts, asserted explicitly here as well as
	// through the adjudicator, so a reader of this test sees them stated.
	if r.SyncBufferedAfterPoison != nil {
		t.Errorf("SyncBuffered on a poisoned writer returned %v; measured contract is nil", r.SyncBufferedAfterPoison)
	}
	if r.TruncateOnPoisonedErr != nil {
		t.Errorf("Truncate on a poisoned writer returned %v; measured contract is a successful empty", r.TruncateOnPoisonedErr)
	}
	if r.StillPoisonedAfterTruncate == nil {
		t.Error("the writer stopped being poisoned after Truncate: the fail-stop is what makes the successful truncate safe")
	}
	if !errors.Is(r.PoisonedReported, wal.ErrDurabilityFailed) {
		t.Errorf("Poisoned() reported %v, which does not carry wal.ErrDurabilityFailed", r.PoisonedReported)
	}
}

// TestWALLifecycle_GateDetectsEachDefect is the falsifiability proof for the
// lifecycle adjudicator. The healthy control is the record a real run produces,
// so a clause that stopped matching reality shows up here as a rejected control
// rather than as a quietly weakened gate.
func TestWALLifecycle_GateDetectsEachDefect(t *testing.T) {
	t.Parallel()

	live, err := RunWALLifecycle(context.Background(), walSurfaceSeed)
	if err != nil {
		t.Fatalf("RunWALLifecycle: %v", err)
	}
	if v := checkWALLifecycle(&live); len(v) > 0 {
		t.Fatalf("the live control record was rejected, so every subtest below would be meaningless:\n%v", v)
	}

	cases := []struct {
		name    string
		doctor  func(*WALLifecycleResult)
		wantMsg string
	}{
		{
			name:    "Truncate under-reports the bytes it freed",
			doctor:  func(r *WALLifecycleResult) { r.TruncateReturned-- },
			wantMsg: "documented as the bytes in the file at truncation",
		},
		{
			name:    "Truncate leaves a non-empty file",
			doctor:  func(r *WALLifecycleResult) { r.ImageAfterTruncate = 14 },
			wantMsg: "both must be zero",
		},
		{
			name:    "Truncate resets the lifetime counters",
			doctor:  func(r *WALLifecycleResult) { r.StatsAfter = wal.Stats{} },
			wantMsg: "documented as not reset",
		},
		{
			name:    "the post-truncate append does not restart at zero",
			doctor:  func(r *WALLifecycleResult) { r.PostTruncateMark += 30 },
			wantMsg: "freshly-empty file",
		},
		{
			name:    "the truncate did not discard the previous WAL",
			doctor:  func(r *WALLifecycleResult) { r.PostTruncateFrames = 6 },
			wantMsg: "did not discard the previous WAL",
		},
		{
			name:    "the failed commit's error lacks the durability class",
			doctor:  func(r *WALLifecycleResult) { r.PoisonSyncIsClass = false },
			wantMsg: "wal.ErrDurabilityFailed",
		},
		{
			name:    "Poisoned reports healthy after a failed fsync",
			doctor:  func(r *WALLifecycleResult) { r.PoisonedReported = nil },
			wantMsg: "reports healthy after a failed fsync",
		},
		{
			name:    "an append after the poison is accepted",
			doctor:  func(r *WALLifecycleResult) { r.AppendAfterPoisonIsSticky = false },
			wantMsg: "did not return the sticky error",
		},
		{
			name:    "a discarded committer is acknowledged",
			doctor:  func(r *WALLifecycleResult) { r.SyncGroupLostMarkIsSticky = false },
			wantMsg: "thrown away would be acknowledged",
		},
		{
			name:    "an already-durable committer is failed by a later poison",
			doctor:  func(r *WALLifecycleResult) { r.SyncGroupDurableMarkErr = wal.ErrDurabilityFailed },
			wantMsg: "rmp #2322",
		},
		{
			name:    "the discarded suffix stayed durable",
			doctor:  func(r *WALLifecycleResult) { r.DurableAtPoison += walLifecycleFrameBytes },
			wantMsg: "un-synced suffix was discarded",
		},
		{
			name:    "the poison cleared the fail-stop",
			doctor:  func(r *WALLifecycleResult) { r.StillPoisonedAfterTruncate = nil },
			wantMsg: "no longer poisoned after Truncate",
		},
		{
			name:    "SyncBuffered on a poisoned writer changed behaviour",
			doctor:  func(r *WALLifecycleResult) { r.SyncBufferedAfterPoison = wal.ErrDurabilityFailed },
			wantMsg: "MEASURED behaviour is nil",
		},
		{
			name:    "Truncate on a poisoned writer changed behaviour",
			doctor:  func(r *WALLifecycleResult) { r.TruncateOnPoisonedErr = wal.ErrDurabilityFailed },
			wantMsg: "MEASURED behaviour is a successful empty",
		},
		{
			name:    "a closed writer no longer reports ErrWriterClosed",
			doctor:  func(r *WALLifecycleResult) { r.AppendAfterCloseIsClosed = false },
			wantMsg: "closed check is documented to precede",
		},
		{
			name:    "a closed writer forgets why it died",
			doctor:  func(r *WALLifecycleResult) { r.PoisonedAfterClose = nil },
			wantMsg: "no longer tell why the handle died",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := live
			tc.doctor(&r)
			v := checkWALLifecycle(&r)
			if len(v) == 0 {
				t.Fatalf("the gate accepted a record with %q: the clause cannot fail and therefore proves nothing", tc.name)
			}
			joined := violationText(v)
			if !strings.Contains(joined, tc.wantMsg) {
				t.Errorf("the violation does not explain the defect (want a mention of %q):\n%s", tc.wantMsg, joined)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Part 4 — the two guards SimDisk cannot express
// -----------------------------------------------------------------------------

// TestWALGuards_LockAndSymlinkRefusal drives [wal.ErrWALLocked] and the
// O_NOFOLLOW symlink refusal against a REAL temporary directory, because
// [SimDisk] has neither advisory locks nor links and so cannot express either
// guard. See [WALGuardResult] for why a real directory is the honest route and
// for the explicit statement that these two guards remain outside every seeded,
// crash-injecting scenario in this package.
func TestWALGuards_LockAndSymlinkRefusal(t *testing.T) {
	t.Parallel()

	r, err := RunWALRealFSGuards(t.TempDir())
	if err != nil {
		t.Fatalf("RunWALRealFSGuards: %v", err)
	}
	t.Log(r)
	if r.Skipped {
		if r.LockGuardsAttempted || r.SymlinkWALAttempted {
			t.Fatalf("a whole-record skip was raised LATE, discarding measured verdicts: %s", r)
		}
		t.Skipf("the platform cannot express the WAL guards: %s", r.SkipReason)
	}
	// Every axis must have run on a platform that did not declare a whole-record
	// skip. Asserting this — rather than only reading the verdicts — is what
	// stops a silently unexercised axis from reading as a clean one.
	if !r.LockGuardsAttempted || !r.SymlinkWALAttempted || !r.SymlinkLockAttempted || !r.VictimChecked {
		t.Fatalf("an axis was not exercised on a platform that declared no skip: %s", r)
	}

	for _, viol := range checkWALRealFSGuards(&r) {
		t.Errorf("guards: %s", viol)
	}
	// Stated explicitly as well as adjudicated: the lock must be the named
	// sentinel and not merely "some error", or a caller cannot distinguish
	// "already in use" from "the path is broken".
	if !errors.Is(r.SecondOpenErr, wal.ErrWALLocked) {
		t.Errorf("the second open returned %v, want errors.Is wal.ErrWALLocked", r.SecondOpenErr)
	}
}

// TestWALGuards_GateDetectsEachDefect is the falsifiability proof for the guard
// adjudicator, and it needs no filesystem: the adjudicator is a pure function of
// what was observed, so each guard's clause is proved wired by a doctored record.
// That also keeps the proof valid on a platform where the live arm skips.
func TestWALGuards_GateDetectsEachDefect(t *testing.T) {
	t.Parallel()

	healthy := WALGuardResult{
		LockGuardsAttempted: true, SymlinkWALAttempted: true,
		SymlinkLockAttempted: true, VictimChecked: true,
		SecondOpenErr: wal.ErrWALLocked, SecondOpenIsLocked: true,
		SymlinkedWALErr:  errors.New("too many levels of symbolic links"),
		SymlinkedLockErr: errors.New("too many levels of symbolic links"),
		VictimIntact:     true,
	}
	if v := checkWALRealFSGuards(&healthy); len(v) > 0 {
		t.Fatalf("the healthy control record was rejected:\n%v", v)
	}

	cases := []struct {
		name    string
		doctor  func(*WALGuardResult)
		wantMsg string
	}{
		{
			name:    "a second writer is admitted",
			doctor:  func(r *WALGuardResult) { r.SecondOpenIsLocked, r.SecondOpenErr = false, nil },
			wantMsg: "interleave their frames",
		},
		{
			name:    "the lock is not released by its owner",
			doctor:  func(r *WALGuardResult) { r.ReopenAfterCloseErr = wal.ErrWALLocked },
			wantMsg: "strands the WAL",
		},
		{
			name:    "a symlinked WAL path is followed",
			doctor:  func(r *WALGuardResult) { r.SymlinkedWALErr = nil },
			wantMsg: "FOLLOWED a symlinked WAL path",
		},
		{
			name:    "a symlinked LOCK sentinel is followed",
			doctor:  func(r *WALGuardResult) { r.SymlinkedLockErr = nil },
			wantMsg: "FOLLOWED a symlinked LOCK sentinel",
		},
		{
			name:    "the victim file outside the directory was written",
			doctor:  func(r *WALGuardResult) { r.VictimIntact = false },
			wantMsg: "was MODIFIED through a symlink",
		},
		{
			name:    "the first open failed",
			doctor:  func(r *WALGuardResult) { r.FirstOpenErr = errors.New("permission denied") },
			wantMsg: "the first wal.Open failed",
		},
		{
			name: "no axis ran at all and no skip was declared",
			doctor: func(r *WALGuardResult) {
				r.LockGuardsAttempted, r.SymlinkWALAttempted = false, false
				r.SymlinkLockAttempted, r.VictimChecked = false, false
			},
			wantMsg: "exercised NO guard",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := healthy
			tc.doctor(&r)
			v := checkWALRealFSGuards(&r)
			if len(v) == 0 {
				t.Fatalf("the gate accepted a record with %q: the clause cannot fail and therefore proves nothing", tc.name)
			}
			if joined := violationText(v); !strings.Contains(joined, tc.wantMsg) {
				t.Errorf("the violation does not explain the defect (want a mention of %q):\n%s", tc.wantMsg, joined)
			}
		})
	}
}

// TestWALGuards_SkippedRunMakesNoClaim pins that an unsupported platform is
// reported as uninformative rather than as a passing guard. Without this the
// adjudicator returning nil on a skip would be indistinguishable from it
// returning nil on a healthy run.
func TestWALGuards_SkippedRunMakesNoClaim(t *testing.T) {
	t.Parallel()

	// A record in which EVERY guard failed, but which is marked skipped.
	broken := WALGuardResult{
		Skipped: true, SkipReason: "synthetic",
		SecondOpenIsLocked: false, VictimIntact: false,
	}
	if v := checkWALRealFSGuards(&broken); len(v) > 0 {
		t.Errorf("the adjudicator made a claim about a skipped run:\n%v", v)
	}
	if !strings.Contains(broken.String(), "SKIPPED") {
		t.Errorf("a skipped result does not render as skipped: %s", broken.String())
	}
}

// TestWALGuards_UnavailableLockAxisKeepsTheCWE59Verdict is the regression proof
// for rmp #2745: one unavailable axis must no longer discard the axes already
// measured, and above all not the CWE-59 symlink detection for the WAL PATH.
//
// # How the axis is made unavailable for real
//
// No mocking: the LOCK sentinel's link is made impossible by pre-creating a
// plain file at the sentinel path, so os.Symlink returns EEXIST — the same class
// of failure the arm handles on a platform where linking is unprivileged. The
// WAL-path symlink refusal is exercised BEFORE that point, so before this fix
// the late whole-record skip threw its verdict away and checkWALRealFSGuards
// returned nil having judged nothing at all.
func TestWALGuards_UnavailableLockAxisKeepsTheCWE59Verdict(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocker := filepath.Join(dir, guardLockBaseName+guardLockSuffix)
	if err := os.WriteFile(blocker, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("pre-create the LOCK sentinel path: %v", err)
	}

	r, err := RunWALRealFSGuards(dir)
	if err != nil {
		t.Fatalf("RunWALRealFSGuards: %v", err)
	}
	t.Log(r)
	// A whole-record skip is legitimate ONLY before any axis has run. Raised
	// later it discards verdicts already measured, which is the defect itself.
	if r.Skipped {
		if r.LockGuardsAttempted || r.SymlinkWALAttempted {
			t.Fatalf("a whole-record skip was raised LATE, after measured axes (lock=%t symlinkWAL=%t): it discards their verdicts, including the CWE-59 detection — %s",
				r.LockGuardsAttempted, r.SymlinkWALAttempted, r)
		}
		t.Skipf("the platform cannot express the WAL guards at all: %s", r.SkipReason)
	}

	// The unavailable axis is reported as unmeasured, not as clean.
	if r.SymlinkLockAttempted {
		t.Fatalf("the LOCK-sentinel link was expected to be uncreatable here: %s", r)
	}
	if len(r.Unmeasured) == 0 || !strings.Contains(strings.Join(r.Unmeasured, "; "), "LOCK-sentinel") {
		t.Errorf("the unavailable LOCK-sentinel axis was not reported as unmeasured: %s", r)
	}

	// The four verdicts measured before it SURVIVE.
	if !r.LockGuardsAttempted {
		t.Errorf("the single-writer lock verdicts were discarded: %s", r)
	}
	if !r.SecondOpenIsLocked {
		t.Errorf("the second open was not refused with wal.ErrWALLocked: %s", r)
	}
	if r.ReopenAfterCloseErr != nil {
		t.Errorf("reopen after close failed: %v", r.ReopenAfterCloseErr)
	}
	if !r.SymlinkWALAttempted {
		t.Fatalf("the CWE-59 WAL-path symlink detection was DISCARDED by the unavailable LOCK axis: %s", r)
	}
	if r.SymlinkedWALErr == nil {
		t.Errorf("wal.Open followed a symlinked WAL path (CWE-59): %s", r)
	}
	if !r.VictimChecked || !r.VictimIntact {
		t.Errorf("the victim-integrity verdict did not survive: %s", r)
	}

	// The honest record passes.
	if v := checkWALRealFSGuards(&r); len(v) > 0 {
		t.Fatalf("the adjudicator rejected an honest partially-measured record:\n%s", violationText(v))
	}

	// MUTATION: the surviving CWE-59 verdict is genuinely ADJUDICATED, not merely
	// stored. Feed the adjudicator the same partially-measured record with the
	// symlink followed, and it must fail.
	followed := r
	followed.SymlinkedWALErr = nil
	v := checkWALRealFSGuards(&followed)
	if len(v) == 0 {
		t.Fatalf("the adjudicator ACCEPTED a followed symlinked WAL path on a partially-measured record: the CWE-59 clause cannot fail and proves nothing")
	}
	if joined := violationText(v); !strings.Contains(joined, "FOLLOWED a symlinked WAL path") {
		t.Errorf("the violation does not name the CWE-59 defect:\n%s", joined)
	}
}

// violationText joins violations into one string for a containment assertion.
func violationText(v []Violation) string {
	parts := make([]string, 0, len(v))
	for _, viol := range v {
		parts = append(parts, viol.String())
	}
	return strings.Join(parts, "\n")
}
