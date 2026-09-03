package sim

// mvcc_sessions_crash_test.go — validation of rmp #2438: crashes landing
// while explicit MVCC transactions are open, recovery through the real WAL
// path, and atomicity adjudicated at TRANSACTION granularity.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// mvccCrashConfig is the shared crashing configuration: aggressive enough to
// land well over ten crashes per run while transactions overlap, small enough
// for the short layer.
func mvccCrashConfig(seed uint64) MVCCSessionsConfig {
	cfg := mvccTestConfig(seed)
	cfg.Ticks = 1200
	cfg.Crash = CrashConfig{Enabled: true, CrashProb: 1.0 / 40.0, StabilityWindow: 20}
	return cfg
}

// TestMVCCSessionsCrash_Deterministic: same seed, same crash points, same
// outcome — byte for byte — and the run is clean and non-vacuous.
func TestMVCCSessionsCrash_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for _, seed := range []uint64{1, 7, 42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			a, err := RunMVCCSessions(ctx, mvccCrashConfig(seed))
			if err != nil {
				t.Fatalf("run A: %v", err)
			}
			b, err := RunMVCCSessions(ctx, mvccCrashConfig(seed))
			if err != nil {
				t.Fatalf("run B: %v", err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("runs diverge:\nA: %+v\nB: %+v", a, b)
			}
			if !a.Clean() {
				t.Fatalf("violations=%v foldErrors=%v", a.Violations, a.FoldErrors)
			}
			if a.Crashes == 0 || a.TxCommitted == 0 {
				t.Fatalf("vacuous crash run: %+v", a)
			}
		})
	}
}

// TestMVCCSessionsCrash_TenCrashesOverlapping is the rmp #2438 acceptance
// gate: at least 10 crashes in one run, transactions genuinely overlapping,
// crashes landing while transactions were OPEN (TxCrashed > 0 — the property
// the auto-commit durability bracket structurally cannot produce), committed
// pairs surviving recovery whole, and zero violations on HEAD.
func TestMVCCSessionsCrash_TenCrashesOverlapping(t *testing.T) {
	defer goleak.VerifyNone(t)
	res, err := RunMVCCSessions(context.Background(), mvccCrashConfig(7))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Clean() {
		t.Fatalf("violations=%v foldErrors=%v", res.Violations, res.FoldErrors)
	}
	if res.Crashes < 10 {
		t.Fatalf("only %d crashes; the acceptance gate needs at least 10", res.Crashes)
	}
	if res.WriteOverlapTicks == 0 {
		t.Fatalf("no write-transaction overlap: %+v", res)
	}
	if res.TxCrashed == 0 {
		t.Fatal("no crash ever landed while a transaction was open — the mode is not exercising open-transaction crashes")
	}
	if res.PairsCommitted == 0 {
		t.Fatal("no committed pair crossed a crash — the torn-transaction sweep never had a subject")
	}
	t.Logf("crashes=%d txCrashed=%d replayedOps=%d committed=%d conflicted=%d pairs=%d",
		res.Crashes, res.TxCrashed, res.ReplayedOps, res.TxCommitted, res.TxConflicted, res.PairsCommitted)
}

// TestMVCCSessionsCrash_TornTransactionInjection proves the post-recovery
// adjudication FIRES on a torn transaction: exactly one member of a pair the
// bookkeeping holds as committed exists on the engine — half a multi-statement
// transaction, the state a broken replay would produce.
func TestMVCCSessionsCrash_TornTransactionInjection(t *testing.T) {
	defer goleak.VerifyNone(t)
	h, s := injectionHarness(t)
	mustExecCommit(t, s.sess, "CREATE (n:Person {name:'mv-s0-pa0', age:1})", nil)
	h.pairs = append(h.pairs, mvccPair{a: "mv-s0-pa0", b: "mv-s0-pb0", foldSeq: 0})

	v := h.crashPairSweep(1)
	if len(v) == 0 {
		t.Fatal("crash pair sweep did not fire on a torn transaction")
	}
	if !strings.Contains(v[0].Message, "TORN TRANSACTION") {
		t.Fatalf("wrong finding: %v", v[0])
	}
	if v[0].Kind != ViolationACIDAtomicity {
		t.Fatalf("wrong kind: %v", v[0].Kind)
	}
}

// TestMVCCSessionsCrash_LostCommittedTransactionInjection proves the
// durability half fires across the crash boundary: the folded oracle holds a
// whole transaction the engine does not (an acknowledged commit lost by
// recovery).
func TestMVCCSessionsCrash_LostCommittedTransactionInjection(t *testing.T) {
	defer goleak.VerifyNone(t)
	h, _ := injectionHarness(t)
	// The oracle acknowledges a transaction the engine never saw.
	h.oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "ghost", "age": int64(1)})

	v := h.checker.CheckDurability(1, h.oracle, h.adapter)
	if len(v) == 0 {
		t.Fatal("durability check did not fire on a lost acknowledged transaction")
	}
	found := false
	for _, x := range v {
		if x.Kind == ViolationACIDDurability {
			found = true
		}
	}
	if !found {
		t.Fatalf("no durability-kind finding: %v", v)
	}
}

// TestMVCCSessionsCrash_ValuePreservingSetSeeds is the rmp #2717 end-to-end
// regression: the three seeds a 1000-seed sweep of the Ticks=1200,Sessions=6
// shape found refusing a fold, each on the same shape — a session SET an age
// equal to the one the node already carried, another session DETACH DELETEd
// that node and committed first, and the engine acknowledged both. The engine
// records no version for a value-preserving property write
// (graph/lpg/property.go, propValuesDefinitelyEqual), so both commits are
// correct and the refusal was the workspace's, not the engine's.
//
// Seed 22 runs the crash arm (which is where the finding was first seen);
// seeds 500 and 572 run the SAME shape with crash injection OFF, which is what
// proves the finding was never crash-specific.
func TestMVCCSessionsCrash_ValuePreservingSetSeeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  MVCCSessionsConfig
	}{
		{"crash/seed=22", mvccCrashConfig(22)},
		{"nocrash/seed=500", mvccDeepConfig(500)},
		{"nocrash/seed=572", mvccDeepConfig(572)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := RunMVCCSessions(ctx, tc.cfg)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(res.FoldErrors) > 0 {
				t.Fatalf("fold refusal returned: %v", res.FoldErrors)
			}
			if !res.Clean() {
				t.Fatalf("violations=%v", res.Violations)
			}
			// Non-vacuity: the run must have reached the far end of its tick
			// budget, not stopped early at a finding.
			if res.TxCommitted == 0 || res.Statements < 400 {
				t.Fatalf("run did not go deep enough to reach the reproducer: %+v", res)
			}
		})
	}
}

// mvccDeepConfig is [mvccTestConfig] at the crash configuration's tick depth
// with crash injection OFF — the arm that separates "needs a crash" from
// "needs depth".
func mvccDeepConfig(seed uint64) MVCCSessionsConfig {
	cfg := mvccTestConfig(seed)
	cfg.Ticks = 1200
	return cfg
}

// TestMVCCSessionsCrash_SplitLifePairSeeds is the seed gate for rmp #2724: the
// five seeds of the 1000-seed sweep whose snapshot-stability checker fired on
// the split life pair — a rolled-back DETACH DELETE overwriting a node's
// committed birth, made observable by any later unrelated delete.
//
// Two arms, because the finding is not crash-specific: 815 and 875 reproduce
// with crash injection OFF at the crash configuration's tick depth, and 486,
// 699 and 932 reproduce with it on. All five were unclean at `83657c2d` and are
// the named reproducers the diagnosis (docs/mvcc-life-record-defects-2026-09-03.md)
// left behind; the mechanism itself is pinned at the layer that owns it by
// graph/lpg TestNodeLife_RolledBackDeleteSurvivesAnUnrelatedDelete.
//
// NO TICK IS ASSERTED. `graph/lpg/mvcc_vacuum.go` runs its sweep on a
// background goroutine with wall-clock backoff, so the tick at which a
// reclamation-sensitive finding becomes countable moves between processes even
// though the defect is stable. The seeds here are byte-identical over repeats,
// but the gate must not depend on that for a mode that can reach reclamation.
func TestMVCCSessionsCrash_SplitLifePairSeeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  MVCCSessionsConfig
	}{
		{"nocrash/seed=815", mvccDeepConfig(815)},
		{"nocrash/seed=875", mvccDeepConfig(875)},
		{"crash/seed=486", mvccCrashConfig(486)},
		{"crash/seed=699", mvccCrashConfig(699)},
		{"crash/seed=932", mvccCrashConfig(932)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := RunMVCCSessions(ctx, tc.cfg)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(res.FoldErrors) > 0 {
				t.Fatalf("fold refusal returned: %v", res.FoldErrors)
			}
			if !res.Clean() {
				t.Fatalf("violations=%v", res.Violations)
			}
			// Non-vacuity: the run must have reached the far end of its tick
			// budget rather than stopping early, and it must actually have
			// rolled transactions back — a rolled-back DELETE is the whole
			// mechanism, so a schedule with no rollbacks cannot exercise it.
			if res.TxCommitted == 0 || res.TxRolledBack == 0 || res.Statements < 400 {
				t.Fatalf("run did not exercise the reproducer: %+v", res)
			}
		})
	}
}
