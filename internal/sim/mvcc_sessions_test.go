package sim

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"go.uber.org/goleak"
)

// mvccTestConfig is the shared small configuration the short-layer gates run:
// enough sessions and ticks to overlap write transactions on every seed tried,
// small enough to keep the package well under the 60s short budget.
func mvccTestConfig(seed uint64) MVCCSessionsConfig {
	return MVCCSessionsConfig{
		Seed:           seed,
		Ticks:          600,
		Sessions:       6,
		MinTxOps:       2,
		MaxTxOps:       6,
		ReadTxWeight:   0.25,
		RollbackWeight: 0.10,
		CheckEvery:     10,
	}
}

// TestMVCCSessions_Deterministic is the reproducibility gate for the
// multi-session mode: two runs with the same seed must produce IDENTICAL
// results — same schedule, same outcomes, same counters — which is what makes
// a failing seed a reproducer. It also asserts the run is non-vacuous (it
// committed transactions and drove statements) and clean.
func TestMVCCSessions_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	for _, seed := range []uint64{1, 7, 42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			a, err := RunMVCCSessions(ctx, mvccTestConfig(seed))
			if err != nil {
				t.Fatalf("run A: %v", err)
			}
			b, err := RunMVCCSessions(ctx, mvccTestConfig(seed))
			if err != nil {
				t.Fatalf("run B: %v", err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("runs diverge:\nA: %+v\nB: %+v", a, b)
			}
			if !a.Clean() {
				t.Fatalf("violations=%v foldErrors=%v", a.Violations, a.FoldErrors)
			}
			if a.TxCommitted == 0 || a.Statements == 0 {
				t.Fatalf("vacuous run: %+v", a)
			}
		})
	}
}

// TestMVCCSessions_TransactionsOverlap asserts the mode actually exercises
// MVCC: at least two WRITE transactions are open at once on some tick (the
// non-vacuity guard the task's acceptance criteria demand — a mode that never
// overlaps proves nothing), and the read-only path runs too.
func TestMVCCSessions_TransactionsOverlap(t *testing.T) {
	defer goleak.VerifyNone(t)
	res, err := RunMVCCSessions(context.Background(), mvccTestConfig(7))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Clean() {
		t.Fatalf("violations=%v foldErrors=%v", res.Violations, res.FoldErrors)
	}
	if res.WriteOverlapTicks == 0 {
		t.Fatalf("no tick had two write transactions open — the mode is not exercising MVCC: %+v", res)
	}
	if res.OverlapTicks == 0 || res.MaxOpenTx < 2 {
		t.Fatalf("no transaction overlap observed: %+v", res)
	}
	if res.TxReadOnly == 0 {
		t.Fatalf("read-only transaction path never ran: %+v", res)
	}
	t.Logf("committed=%d rolledBack=%d conflicted=%d readOnly=%d overlap=%d writeOverlap=%d maxOpen=%d statements=%d",
		res.TxCommitted, res.TxRolledBack, res.TxConflicted, res.TxReadOnly,
		res.OverlapTicks, res.WriteOverlapTicks, res.MaxOpenTx, res.Statements)
}

// TestMVCCSessions_CancelledContextStops asserts the harness honours ctx
// cancellation: an already-cancelled context stops the run at the first tick
// with the context error and leaks nothing.
func TestMVCCSessions_CancelledContextStops(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunMVCCSessions(ctx, mvccTestConfig(1)); err == nil {
		t.Fatal("cancelled context did not stop the run")
	}
}
