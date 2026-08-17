package sim

// txn_oversize_test.go — the wiring and the falsifiability proofs for the
// transaction-size cap oracles (rmp #2474).
//
// Every gate here is proved falsifiable as well as satisfied. This sprint has
// already found nine guards that existed and proved nothing — two of them
// written to their author's own specification, one hiding inside a metric, one
// satisfied by definition — so a live arm that passes is only half the evidence.
// The *_ClausesFire tests perturb a hand-built control ONE field at a time and
// require the corresponding clause to fire, with the unperturbed control silent.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// txnOversizeSeed is the fixed seed the live arms run under. The scenario is
// deterministic — its sizes are boundary constants, not samples — so one seed is
// the whole population.
const txnOversizeSeed = 0x9E3779B97F4A7C15

// -----------------------------------------------------------------------------
// Live arms
// -----------------------------------------------------------------------------

// TestTxnOversize_ProducerRefusesBeforeWritingAnyFrame is the producer verdict:
// a transaction past the store's op cap must be refused with
// [txn.ErrTransactionTooLarge], the refusal must leave the durable WAL image
// BYTE-identical and the live graph unmutated, and the surviving file must
// recover clean and equal to the model.
//
// The non-vacuity gate runs first and separately, so a run that never got an
// oversize transaction in front of a non-empty WAL is reported as uninformative
// rather than as a defect in the store.
func TestTxnOversize_ProducerRefusesBeforeWritingAnyFrame(t *testing.T) {
	ev, err := RunTxnOversizeProducer(context.Background(), TxnOversizeConfig{
		Seed: txnOversizeSeed,
		Cap:  txnOversizeCap,
	})
	if err != nil {
		t.Fatalf("RunTxnOversizeProducer: %v", err)
	}
	t.Log(&ev)
	for _, a := range ev.Attempts {
		t.Logf("  %s", a)
	}

	if v := checkTxnOversizeNonVacuity(&ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the run proved nothing about the producer cap; the verdict below would be meaningless")
	}
	for _, viol := range checkTxnOversizeProducer(&ev) {
		t.Errorf("producer cap: %s", viol)
	}
}

// TestTxnOversize_BoundaryIsExclusiveAtTheCap pins what the engine actually does
// at the boundary, rather than inferring it from reading the comparison. The
// producer's guard is `len(ops) > cap`, so a transaction of EXACTLY cap ops must
// commit and one of cap+1 ops must not.
//
// The witness clauses are Logf, never Errorf: an attempt the plan did not
// produce is an unmet precondition of this pin, not a defect in the store.
func TestTxnOversize_BoundaryIsExclusiveAtTheCap(t *testing.T) {
	ev, err := RunTxnOversizeProducer(context.Background(), TxnOversizeConfig{
		Seed: txnOversizeSeed,
		Cap:  txnOversizeCap,
	})
	if err != nil {
		t.Fatalf("RunTxnOversizeProducer: %v", err)
	}
	if ev.EffectiveCap != txnOversizeCap {
		t.Fatalf("effective cap %d, want %d: the store did not resolve the configured cap",
			ev.EffectiveCap, txnOversizeCap)
	}

	byName := make(map[string]TxnOversizeAttempt, len(ev.Attempts))
	for _, a := range ev.Attempts {
		byName[a.Name] = a
	}

	atCap, ok := byName["at-cap"]
	if !ok {
		t.Logf("witness: the plan produced no at-cap attempt; the inclusive half of the boundary is unpinned")
	} else {
		if atCap.Ops != txnOversizeCap {
			t.Fatalf("at-cap attempt buffered %d ops, want exactly %d", atCap.Ops, txnOversizeCap)
		}
		if atCap.Refused {
			t.Errorf("a transaction of EXACTLY the cap (%d ops) was refused: the producer guard is "+
				"inclusive where the source says `len(ops) > cap` — %s", txnOversizeCap, atCap)
		}
		if atCap.WALAfter <= atCap.WALBefore {
			t.Errorf("the at-cap transaction committed but appended nothing (%d -> %d bytes) — %s",
				atCap.WALBefore, atCap.WALAfter, atCap)
		}
	}

	oneOver, ok := byName["one-over"]
	if !ok {
		t.Logf("witness: the plan produced no one-over attempt; the exclusive half of the boundary is unpinned")
		return
	}
	if oneOver.Ops != txnOversizeCap+1 {
		t.Fatalf("one-over attempt buffered %d ops, want exactly %d", oneOver.Ops, txnOversizeCap+1)
	}
	if !oneOver.Refused || !oneOver.Sentinel {
		t.Errorf("a transaction of cap+1 (%d ops) was not refused with txn.ErrTransactionTooLarge — %s",
			txnOversizeCap+1, oneOver)
	}
}

// TestTxnOversize_UnlimitedCapCommitsTheSameWorkload is the third boundary
// setting and the SENSITIVITY seam of the producer verdict at once. It drives
// the byte-identical plan through a store opened with
// [txn.MaxTxnOpsUnlimited]: every attempt — including the ones the capped arm
// refused — must now commit and survive recovery.
//
// Without it, "the over-cap transactions were refused" would be consistent with
// a store that refuses those op counts for some reason of its own. With it, the
// only difference between the two runs is the cap.
func TestTxnOversize_UnlimitedCapCommitsTheSameWorkload(t *testing.T) {
	ev, err := RunTxnOversizeProducer(context.Background(), TxnOversizeConfig{
		Seed: txnOversizeSeed,
		Cap:  txn.MaxTxnOpsUnlimited,
	})
	if err != nil {
		t.Fatalf("RunTxnOversizeProducer(unlimited): %v", err)
	}
	t.Log(&ev)

	if ev.EffectiveCap != 0 {
		t.Fatalf("effective cap %d, want 0 (no cap): txn.MaxTxnOpsUnlimited did not disable the bound",
			ev.EffectiveCap)
	}
	if v := checkTxnOversizeNonVacuity(&ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the unlimited run proved nothing; the verdict below would be meaningless")
	}
	for _, viol := range checkTxnOversizeProducer(&ev) {
		t.Errorf("unlimited: %s", viol)
	}
	if ev.Committed() != len(ev.Attempts) {
		t.Errorf("only %d of %d attempts committed under an UNLIMITED cap: the op counts the capped arm "+
			"refused are being rejected for some other reason, so that arm's refusals are not "+
			"attributable to the cap — %s", ev.Committed(), len(ev.Attempts), &ev)
	}
}

// TestTxnOversize_ReplayFailStopsOnACraftedOversizeWAL is the replay verdict: a
// hand-built WAL whose marker-less run exceeds the replay cap must stop with
// [recovery.ErrTransactionTooLarge], keep the committed prefix, and be refused
// by the harness store-open rather than appended onto.
//
// The `over-cap-unlimited` arm in the same sweep replays the byte-identical file
// with the cap disabled and recovers every op, which is what makes the fail-stop
// attributable to the cap rather than to a file the harness built wrong.
func TestTxnOversize_ReplayFailStopsOnACraftedOversizeWAL(t *testing.T) {
	ev, err := RunTxnOversizeReplay(context.Background(), txnOversizeSeed)
	if err != nil {
		t.Fatalf("RunTxnOversizeReplay: %v", err)
	}
	t.Log(ev)

	if v := checkTxnOversizeReplayNonVacuity(ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the crafted files did not have the shape the verdict assumes")
	}
	for _, viol := range checkTxnOversizeReplay(ev) {
		t.Errorf("replay cap: %s", viol)
	}
}

// TestTxnOversize_CraftedFileIsWellFormed is the standing proof that the
// hand-written v3 op payloads match what store/txn's unexported encoder
// produces. The layout was transcribed from source, and a transcription is a
// claim until something decodes it: the at-cap arm replays these frames through
// the REAL decoder and must recover exactly the nodes they name, which a wrong
// version tag, kind, sequence width or body layout could not do.
func TestTxnOversize_CraftedFileIsWellFormed(t *testing.T) {
	ev, err := RunTxnOversizeReplay(context.Background(), txnOversizeSeed)
	if err != nil {
		t.Fatalf("RunTxnOversizeReplay: %v", err)
	}
	found := false
	for _, a := range ev.Arms {
		if a.EffectiveCap > 0 && a.RunOps > a.EffectiveCap {
			continue // the fail-stop arms decode nothing past the cap by design
		}
		found = true
		want := ev.PriorOps + a.RunOps
		if a.Order != uint64(want) {
			t.Errorf("arm %q recovered %d node(s) from %d hand-written AddNode frames: the crafted v3 "+
				"payload does not match what the real decoder expects — %s", a.Name, a.Order, want, a)
		}
	}
	if !found {
		t.Logf("witness: the sweep drove no within-cap arm, so the payload layout is unproven here")
	}
}

// TestTxnOversize_UncappedProducerMakesAnUnreplayableTransactionDurable is the
// permanent SENSITIVITY proof of the producer oracle, and it is driven against
// the real defect rather than against fabricated evidence: the seam opens the
// store exactly as this harness did before rmp #2474 — the cap reaching the
// replayer and not the producer — and everything else in the run is unchanged.
//
// What that produces is the hazard the producer <= replay invariant exists to
// prevent, and it is worth stating plainly because it is worse than a missed
// refusal. The 33-op transaction is ACKNOWLEDGED as durable by a producer
// bounded at [txn.DefaultMaxTxnOps]; recovery, bounded at the configured 32,
// then refuses to replay the file at all. The store does not lose that
// transaction, it fails to reopen — every committed transaction in the WAL
// becomes unreachable behind a fail-stop.
//
// So this test requires two things: that the over-cap transaction was committed
// (the clause the live arm exists to catch), and that the resulting image is
// genuinely unreplayable (the consequence).
func TestTxnOversize_UncappedProducerMakesAnUnreplayableTransactionDurable(t *testing.T) {
	ev, err := RunTxnOversizeProducer(context.Background(), TxnOversizeConfig{
		Seed:                 txnOversizeSeed,
		Cap:                  txnOversizeCap,
		UncappedProducerSeam: true,
	})
	t.Logf("seam: %v", &ev)
	t.Logf("seam reopen: %v", err)

	if err == nil {
		t.Fatalf("the seam reopened cleanly: a producer bounded ABOVE the replay cap wrote a file "+
			"recovery accepted, so the two bounds are no longer coupled — %s", &ev)
	}
	if !errors.Is(err, recovery.ErrTransactionTooLarge) {
		t.Errorf("the seam failed with %v, want an error carrying recovery.ErrTransactionTooLarge: "+
			"the unreplayable-transaction hazard is not what stopped it", err)
	}

	// The clause the live arm relies on must also fire on the same evidence.
	v := checkTxnOversizeProducer(&ev)
	if !txnOversizeAnyMessageContains(v, "was COMMITTED under cap") {
		t.Errorf("the producer verdict did not fire its over-cap clause against a store that really "+
			"committed an over-cap transaction; it fired %d violation(s): %v", len(v), v)
	}

	// And the non-vacuity gate must still pass, so the failure above is reported
	// as a defect rather than as an uninformative run.
	for _, viol := range checkTxnOversizeNonVacuity(&ev) {
		t.Errorf("the seam run was judged uninformative, which would mask the defect: %s", viol)
	}
}

// -----------------------------------------------------------------------------
// Falsifiability: the producer clauses
// -----------------------------------------------------------------------------

// txnOversizeControl builds a hand-made evidence value that every producer
// clause accepts. Perturbing one field of it must fire exactly the clause that
// field belongs to.
func txnOversizeControl() TxnOversizeEvidence {
	return TxnOversizeEvidence{
		Cap:          txnOversizeCap,
		EffectiveCap: txnOversizeCap,
		Attempts: []TxnOversizeAttempt{
			{
				Name: "warmup", Ops: txnOversizeWarmupOps, Keys: []string{"warmup-0000"},
				WALBefore: 0, WALAfter: 240, OrderBefore: 0, OrderAfter: 4,
			},
			{
				Name: "one-over", Ops: txnOversizeOneOverOps, Keys: []string{"one-over-0000"},
				Refused: true, Sentinel: true, Err: "txn: transaction exceeds the per-transaction op cap",
				WALBefore: 240, WALAfter: 240, WALIdentical: true, OrderBefore: 4, OrderAfter: 4,
			},
			{
				Name: "at-cap", Ops: txnOversizeCap, Keys: []string{"at-cap-0000"},
				WALBefore: 240, WALAfter: 900, OrderBefore: 4, OrderAfter: 20,
			},
		},
		PreOversizeWALBytes: 240,
		MaxAttemptOps:       txnOversizeOneOverOps,
		ReopenClean:         true,
		RecoveredOrder:      2,
		ModelKeys:           []string{"at-cap-0000", "warmup-0000"},
		RefusedKeys:         []string{"one-over-0000"},
	}
}

// TestTxnOversize_ProducerClausesFire perturbs the control one field at a time
// and requires the producer verdict to fire, with the unperturbed control
// silent.
func TestTxnOversize_ProducerClausesFire(t *testing.T) {
	control := txnOversizeControl()
	if v := checkTxnOversizeProducer(&control); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("control: %s", viol)
		}
		t.Fatal("the unperturbed control must be accepted, or the perturbations below prove nothing")
	}

	tests := []struct {
		name    string
		perturb func(*TxnOversizeEvidence)
		want    string
	}{
		{"over-cap committed", func(e *TxnOversizeEvidence) {
			e.Attempts[1].Refused = false
		}, "was COMMITTED under cap"},
		{"refused without the typed sentinel", func(e *TxnOversizeEvidence) {
			e.Attempts[1].Sentinel = false
		}, "NOT txn.ErrTransactionTooLarge"},
		{"refusal changed the WAL image", func(e *TxnOversizeEvidence) {
			e.Attempts[1].WALIdentical = false
			e.Attempts[1].WALAfter = 260
		}, "durable WAL image CHANGED"},
		{"refusal mutated the live graph", func(e *TxnOversizeEvidence) {
			e.Attempts[1].OrderAfter = 20
		}, "live graph MUTATED"},
		{"within-cap refused", func(e *TxnOversizeEvidence) {
			e.Attempts[2].Refused = true
		}, "WITHIN-cap transaction"},
		{"commit appended nothing", func(e *TxnOversizeEvidence) {
			e.Attempts[2].WALAfter = e.Attempts[2].WALBefore
		}, "durable WAL did not grow"},
		{"reopen found corruption", func(e *TxnOversizeEvidence) {
			e.ReopenClean = false
		}, "reported genuine corruption"},
		{"committed key lost", func(e *TxnOversizeEvidence) {
			e.MissingKeys = []string{"at-cap-0000"}
		}, "did not survive recovery"},
		{"refused key resurrected", func(e *TxnOversizeEvidence) {
			e.ResurrectedKeys = []string{"one-over-0000"}
		}, "came back after recovery"},
		{"recovered order disagrees with the model", func(e *TxnOversizeEvidence) {
			e.RecoveredOrder = 99
		}, "the model holds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := txnOversizeControl()
			tc.perturb(&ev)
			v := checkTxnOversizeProducer(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q fired no clause: the verdict does not adjudicate it", tc.name)
			}
			if !txnOversizeAnyMessageContains(v, tc.want) {
				t.Errorf("perturbation %q fired %d violation(s), none mentioning %q: %v",
					tc.name, len(v), tc.want, v)
			}
		})
	}
}

// TestTxnOversizeNonVacuity_ClausesFire proves the SEPARATE coverage gate is
// falsifiable too. It must accept the control and reject each degenerate shape.
func TestTxnOversizeNonVacuity_ClausesFire(t *testing.T) {
	control := txnOversizeControl()
	if v := checkTxnOversizeNonVacuity(&control); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("control: %s", viol)
		}
		t.Fatal("the unperturbed control must be accepted by the non-vacuity gate")
	}

	tests := []struct {
		name    string
		perturb func(*TxnOversizeEvidence)
		want    string
	}{
		{"no attempt at all", func(e *TxnOversizeEvidence) {
			e.Attempts = nil
		}, "no commit was attempted"},
		{"nothing exceeded the reference cap", func(e *TxnOversizeEvidence) {
			e.MaxAttemptOps = txnOversizeCap
		}, "does not exceed the reference cap"},
		{"the WAL was empty underneath", func(e *TxnOversizeEvidence) {
			e.PreOversizeWALBytes = 0
		}, "satisfied by definition"},
		{"nothing committed", func(e *TxnOversizeEvidence) {
			for i := range e.Attempts {
				e.Attempts[i].Refused = true
			}
		}, "never exercised the durable write path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := txnOversizeControl()
			tc.perturb(&ev)
			v := checkTxnOversizeNonVacuity(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q fired no clause: the gate would accept an uninformative run", tc.name)
			}
			if !txnOversizeAnyMessageContains(v, tc.want) {
				t.Errorf("perturbation %q fired %d violation(s), none mentioning %q: %v",
					tc.name, len(v), tc.want, v)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Falsifiability: the replay clauses
// -----------------------------------------------------------------------------

// txnOversizeReplayControl builds a hand-made replay evidence value every clause
// accepts: one within-cap arm that replayed whole, one over-cap arm that
// fail-stopped, and one unlimited arm over the same run size that replayed whole.
func txnOversizeReplayControl() TxnOversizeReplayEvidence {
	const prior = txnOversizeReplayPriorOps
	return TxnOversizeReplayEvidence{
		PriorOps: prior,
		Arms: []TxnOversizeReplayArm{
			{
				Name: "at-cap", RunOps: txnOversizeReplayCap, Cap: txnOversizeReplayCap,
				EffectiveCap: txnOversizeReplayCap, Bytes: 900, Clean: true,
				WALOps: prior + txnOversizeReplayCap, Order: uint64(prior + txnOversizeReplayCap),
			},
			{
				Name: "over-cap", RunOps: txnOversizeReplayCap + 1, Cap: txnOversizeReplayCap,
				EffectiveCap: txnOversizeReplayCap, Bytes: 940, Clean: false, Sentinel: true,
				TailErr: "recovery: v3 transaction exceeds the per-transaction op cap",
				WALOps:  prior, Order: uint64(prior),
				HarnessRefused: true, HarnessSentinel: true,
			},
			{
				Name: "over-cap-unlimited", RunOps: txnOversizeReplayCap + 1, Cap: txn.MaxTxnOpsUnlimited,
				EffectiveCap: 0, Bytes: 940, Clean: true,
				WALOps: prior + txnOversizeReplayCap + 1, Order: uint64(prior + txnOversizeReplayCap + 1),
			},
		},
	}
}

// TestTxnOversizeReplay_ClausesFire perturbs the replay control one field at a
// time and requires the replay verdict to fire, with the control silent.
func TestTxnOversizeReplay_ClausesFire(t *testing.T) {
	if v := checkTxnOversizeReplay(txnOversizeReplayControl()); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("control: %s", viol)
		}
		t.Fatal("the unperturbed replay control must be accepted")
	}

	tests := []struct {
		name    string
		perturb func(*TxnOversizeReplayEvidence)
		want    string
	}{
		{"over-cap replayed clean", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].Clean = true
		}, "replayed as CLEAN"},
		{"stop is not the typed sentinel", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].Sentinel = false
		}, "NOT recovery.ErrTransactionTooLarge"},
		{"fail-stop applied part of the run", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].WALOps = txnOversizeReplayPriorOps + 3
		}, "want exactly the committed prefix"},
		{"fail-stop graph disagrees with the prefix", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].Order = 99
		}, "want the prefix's"},
		{"harness accepted a fail-stopped file", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].HarnessRefused = false
		}, "ACCEPTED a file recovery fail-stopped on"},
		{"harness refusal is untyped", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].HarnessSentinel = false
		}, "does not carry recovery.ErrTransactionTooLarge"},
		{"within-cap rejected as corrupt", func(e *TxnOversizeReplayEvidence) {
			e.Arms[0].Clean = false
		}, "WITHIN-cap file was rejected as corrupt"},
		{"within-cap replayed partially", func(e *TxnOversizeReplayEvidence) {
			e.Arms[0].WALOps = 1
		}, "did not replay whole"},
		{"within-cap graph disagrees", func(e *TxnOversizeReplayEvidence) {
			e.Arms[0].Order = 1
		}, "the op count and the graph"},
		{"harness refused a within-cap file", func(e *TxnOversizeReplayEvidence) {
			e.Arms[0].HarnessRefused = true
		}, "REFUSED a within-cap file"},
		{"the unlimited arm fail-stopped anyway", func(e *TxnOversizeReplayEvidence) {
			e.Arms[2].Clean = false
		}, "WITHIN-cap file was rejected as corrupt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := txnOversizeReplayControl()
			tc.perturb(&ev)
			v := checkTxnOversizeReplay(ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q fired no clause: the verdict does not adjudicate it", tc.name)
			}
			if !txnOversizeAnyMessageContains(v, tc.want) {
				t.Errorf("perturbation %q fired %d violation(s), none mentioning %q: %v",
					tc.name, len(v), tc.want, v)
			}
		})
	}
}

// TestTxnOversizeReplayNonVacuity_ClausesFire proves the replay coverage gate is
// falsifiable: it must reject a sweep whose files could not have exercised a cap.
func TestTxnOversizeReplayNonVacuity_ClausesFire(t *testing.T) {
	if v := checkTxnOversizeReplayNonVacuity(txnOversizeReplayControl()); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("control: %s", viol)
		}
		t.Fatal("the unperturbed replay control must be accepted by the non-vacuity gate")
	}

	tests := []struct {
		name    string
		perturb func(*TxnOversizeReplayEvidence)
		want    string
	}{
		{"no arm at all", func(e *TxnOversizeReplayEvidence) {
			e.Arms = nil
		}, "no arm was driven"},
		{"no committed prefix", func(e *TxnOversizeReplayEvidence) {
			e.PriorOps = 0
		}, "no committed prefix"},
		{"an empty file", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].Bytes = 0
		}, "EMPTY file"},
		{"no arm exceeds its own cap", func(e *TxnOversizeReplayEvidence) {
			e.Arms[1].EffectiveCap = e.Arms[1].RunOps
		}, "never presented"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := txnOversizeReplayControl()
			tc.perturb(&ev)
			v := checkTxnOversizeReplayNonVacuity(ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q fired no clause: the gate would accept a sweep that proved nothing",
					tc.name)
			}
			if !txnOversizeAnyMessageContains(v, tc.want) {
				t.Errorf("perturbation %q fired %d violation(s), none mentioning %q: %v",
					tc.name, len(v), tc.want, v)
			}
		})
	}
}

// txnOversizeAnyMessageContains reports whether any violation's message contains
// want. It keeps a perturbation honest: firing SOME clause is not enough, it must
// fire the one the perturbation targets.
func txnOversizeAnyMessageContains(v []Violation, want string) bool {
	for _, viol := range v {
		if strings.Contains(viol.Message, want) {
			return true
		}
	}
	return false
}
