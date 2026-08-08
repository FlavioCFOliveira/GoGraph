package anomaly_test

// perturb_test.go — does attaching the checker change the system it observes?
// (rmp #2341 AC5), and the standing rmp #2336 classification attempt (AC6).
//
// # Why AC5 is not a formality
//
// The module's own record is that a probe can hide the defect it was added to
// find: a single instrumentation print inside the code under test turned a
// reproducing failure into a passing one, 8/8 FAIL to 8/8 PASS. A checker that
// suppressed the anomaly it exists to classify would be worse than useless — it
// would look like a fix.
//
// So the comparison is made on the DEFECTIVE build, where there is a real
// failure rate to move. On a healthy build both arms report zero and the
// measurement would establish nothing: comparing 0 with 0 is not evidence that
// the instrument is inert, it is evidence that the experiment had no signal.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/anomaly"
)

// perturbRounds and perturbReps size the experiment. The product is what the
// rate is computed over; both arms get identical values so nothing about the
// shape of the workload differs between them.
const (
	perturbRounds = 40
	perturbReps   = 30
)

// arm accumulates one measurement arm's outcome.
type arm struct {
	reads int64
	torn  int64
}

func (a arm) rate() float64 { return float64(a.torn) / float64(a.reads) }

// tornRates runs the defective workload with and without the recorder, ARMS
// INTERLEAVED, and returns both.
//
// Interleaved because the first version of this measurement ran all twelve
// recorded repetitions and then all twelve unrecorded ones, which confounds the
// difference with everything that changes between the halves of a run — cache
// warmth, heap size, what else the machine was doing. Measured that way the
// ratio came out 0.95, 0.74, 1.27, 0.73, 0.86 across five runs: it straddles
// 1.0, so there was no effect to find, but the design could not have told a
// small real effect from the drift. Alternating the arms removes the ordering
// entirely.
func tornRates(t *testing.T) (with, without arm) {
	t.Helper()
	const writers, readers = 6, 6
	one := func(rec *anomaly.Recorder) int64 {
		b := newBank(t, rec, false) // defective (non-atomic) reader
		b.run(t, writers, readers, perturbRounds)
		return b.torn.Load()
	}
	for range perturbReps {
		with.torn += one(&anomaly.Recorder{})
		with.reads += int64(readers * perturbRounds)
		without.torn += one(nil)
		without.reads += int64(readers * perturbRounds)
	}
	return with, without
}

// TestRecordingDoesNotSuppressTheFailure is AC5.
//
// The claim under test is narrow and falsifiable: attaching the recorder must
// not measurably change how often the defect manifests. It is asserted on the
// RATE rather than on wall-clock time because the rate is what a probe destroys
// when it perturbs timing — the recorded lesson is not "the run got slower", it
// is "the failure stopped happening".
func TestRecordingDoesNotSuppressTheFailure(t *testing.T) {
	t.Parallel()

	withArm, withoutArm := tornRates(t)
	withRate, withoutRate := withArm.rate(), withoutArm.rate()
	withTorn, withoutTorn := withArm.torn, withoutArm.torn

	t.Logf("with    recorder: %d/%d reads torn = %.4f", withTorn, withArm.reads, withRate)
	t.Logf("without recorder: %d/%d reads torn = %.4f", withoutTorn, withoutArm.reads, withoutRate)
	t.Logf("ratio with/without = %.3f", withRate/withoutRate)

	// BOTH ARMS MUST HAVE A SIGNAL. Without this the test would pass trivially
	// on a build where the defect stopped manifesting at all, which is exactly
	// the failure mode it exists to detect.
	if withTorn == 0 || withoutTorn == 0 {
		t.Fatalf("one arm observed NO torn totals (with=%d, without=%d); with no signal in both arms "+
			"this comparison establishes nothing about perturbation", withTorn, withoutTorn)
	}

	// The tolerance is generous because the quantity is a race outcome on a
	// shared machine, and because the failure this guards against is not a
	// wobble but a collapse: the recorded precedent turned 8/8 FAIL into 8/8
	// PASS, i.e. the rate went to zero. A factor-of-two band catches that with
	// room to spare and does not make the test a flake detector for the host.
	ratio := withRate / withoutRate
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("attaching the recorder changed the failure rate by %.2fx (%.4f vs %.4f); "+
			"the observation is perturbing the system it observes", ratio, withRate, withoutRate)
	}
	if math.IsNaN(ratio) {
		t.Errorf("failure-rate ratio is NaN (with=%.4f without=%.4f)", withRate, withoutRate)
	}
}

// TestClassifyTheStanding2336Sighting is AC6: attempt the classification of the
// 2026-08-06 torn-total sighting, and record the outcome whether or not it
// reproduces.
//
// # What the sighting was
//
// On 2026-08-06 a full `make ci` reported ONE torn total in
// examples/27_concurrent_txn: a reader's sum was 941 758 low against an expected
// 46 625 986 168. rmp #2336 already established, and this does not redo, that
// the delta is NOT one transfer amount — none of the 600 planned amounts is
// 941 758 — so "one debit observed without its credit" is false, and the leading
// hypothesis on the ticket is that ONE ACCOUNT RESOLVED AT A DIFFERENT INSTANT
// FROM THE REST, whose delta would be that account's net flow. It has never
// recurred; ~245M observations are clean.
//
// # What this test contributes
//
// Two things, and it is careful not to claim a third.
//
//  1. THE HYPOTHESIS NOW HAS A NAME. A reader that resolves one account at a
//     different instant from the rest observes a transaction's write to one key
//     and misses its write to another. In Adya's model that is a read-dependency
//     INTO the reader and an anti-dependency OUT of it, closing a cycle with
//     exactly one anti-dependency: G-single, which snapshot isolation forbids.
//     TestDefectiveBuildIsDetectedAndNamed demonstrates the classifier
//     producing exactly that name from a build with exactly that defect, so the
//     next sighting is classifiable rather than merely countable.
//
//  2. THE HEALTHY ENGINE IS CLEAN UNDER THE SAME INSTRUMENT. The run below is
//     the #2336 shape — concurrent transfers under concurrent whole-graph
//     readers — with the checker attached, and it reports no violation.
//
// It does NOT claim the sighting is explained. It did not reproduce here either,
// and a non-reproduction is not a resolution — that is the standing-search
// lesson already recorded on #2336, and the reason this test states its own
// negative result in the log rather than passing silently.
func TestClassifyTheStanding2336Sighting(t *testing.T) {
	t.Parallel()
	rec := &anomaly.Recorder{}
	b := newBank(t, rec, true) // the HEALTHY, atomic reader — the shipped build
	const writers, readers, rounds = 8, 8, 60
	b.run(t, writers, readers, rounds)

	h := rec.History()
	rep, err := anomaly.Check(&h, anomaly.SnapshotIsolation)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	observations := int64(readers * rounds)

	t.Logf("rmp #2336 classification attempt: %d reader observations over %d transactions, %d dependency edges",
		observations, rep.Txns, rep.Edges)
	t.Logf("domain invariant (the pre-existing evidence): %d torn totals", b.torn.Load())
	t.Logf("phenomenon checker (the new evidence): %s", oneLine(rep))

	if rep.Truncated {
		t.Fatalf("the cycle search was truncated, so this attempt is INCONCLUSIVE and must not be "+
			"recorded as a clean result:\n%s", rep)
	}
	if !rep.Clean() {
		// If this ever fires it is the sighting reproduced AND classified, which
		// is the outcome #2336 has been waiting for. Fail loudly with the full
		// report rather than logging it.
		t.Fatalf("THE #2336 SHAPE REPRODUCED AND IS NOW CLASSIFIED:\n%s", rep)
	}
	t.Logf("OUTCOME: NOT REPRODUCED in this run. %d observations clean, which adds to the ~245M already "+
		"recorded on #2336 and resolves nothing on its own. What HAS changed is that the ticket's leading "+
		"hypothesis — one account resolved at a different instant from the rest — now has a name the "+
		"checker produces: G-single, demonstrated in TestDefectiveBuildIsDetectedAndNamed.", observations)
}

// oneLine renders a report's verdict compactly for a log line.
func oneLine(r *anomaly.Report) string {
	if r.Truncated {
		return "INCONCLUSIVE (cycle search truncated)"
	}
	if len(r.Violations) == 0 {
		if len(r.Permitted) > 0 {
			return "CLEAN under snapshot isolation (with legal anomalies present)"
		}
		return "CLEAN under snapshot isolation"
	}
	return r.Violations[0].Type.String()
}
