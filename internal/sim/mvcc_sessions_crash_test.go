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
