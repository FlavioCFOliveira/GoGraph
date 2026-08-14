package sim

// mvcc_contention_test.go — validation of the rmp #2437 lost-update scenario:
// determinism, contention non-vacuity, typed-refusal accounting, and the two
// injections the acceptance criteria demand (a silently-dropped conflicting
// write and a phantom apply must both fire the adjudication).

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// contTestConfig is the shared gate configuration: few counters and enough
// sessions and ticks to collide on every seed tried.
func contTestConfig(seed uint64) MVCCContentionConfig {
	return MVCCContentionConfig{Seed: seed, Ticks: 600, Sessions: 6, Counters: 2}
}

// TestMVCCContention_DeterministicAndClean is the reproducibility and green
// gate: identical results for identical seeds, zero violations on HEAD, and a
// non-vacuous run — commits AND typed conflicts both occurred, and every
// refusal was the typed retriable one (TypedConflicts == TxConflicted by
// construction: an untyped refusal is a hard error).
func TestMVCCContention_DeterministicAndClean(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for _, seed := range []uint64{1, 7, 42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			a, err := RunMVCCContention(ctx, contTestConfig(seed))
			if err != nil {
				t.Fatalf("run A: %v", err)
			}
			b, err := RunMVCCContention(ctx, contTestConfig(seed))
			if err != nil {
				t.Fatalf("run B: %v", err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("runs diverge:\nA: %+v\nB: %+v", a, b)
			}
			if !a.Clean() {
				t.Fatalf("violations=%v", a.Violations)
			}
			if a.TxCommitted == 0 {
				t.Fatalf("vacuous run, nothing committed: %+v", a)
			}
			if a.TxConflicted == 0 {
				t.Fatalf("no contention observed — the scenario is not colliding: %+v", a)
			}
			if a.TypedConflicts != a.TxConflicted {
				t.Fatalf("untyped refusals slipped through: typed=%d conflicted=%d", a.TypedConflicts, a.TxConflicted)
			}
			t.Logf("committed=%d conflicted=%d acked=%v", a.TxCommitted, a.TxConflicted, a.AckedIncrements)
		})
	}
}

// TestMVCCContention_GreenAcrossSeeds runs the scenario across 20 seeds — the
// determinism-independent green sweep.
func TestMVCCContention_GreenAcrossSeeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for seed := uint64(1); seed <= 20; seed++ {
		res, err := RunMVCCContention(ctx, contTestConfig(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !res.Clean() {
			t.Fatalf("seed %d: violations=%v", seed, res.Violations)
		}
		if res.TxConflicted == 0 || res.TxCommitted == 0 {
			t.Fatalf("seed %d: vacuous run: %+v", seed, res)
		}
	}
}

// TestMVCCContention_LostUpdateInjection proves the adjudication FIRES on a
// silently-dropped conflicting write: the bookkeeping acknowledges one more
// increment than the engine applied (exactly what a lost update looks like
// from the client's side — an acked write that is not in the data).
func TestMVCCContention_LostUpdateInjection(t *testing.T) {
	defer goleak.VerifyNone(t)
	h := contInjectionHarness(t)
	h.res.AckedIncrements[0]++ // acked but never applied: the lost update

	v := h.adjudicate(context.Background())
	if len(v) == 0 {
		t.Fatal("adjudication did not fire on a silently-dropped acknowledged increment")
	}
	if !strings.Contains(v[0].Message, "LOST UPDATE") {
		t.Fatalf("wrong finding: %v", v[0])
	}
}

// TestMVCCContention_PhantomApplyInjection proves the adjudication FIRES on a
// phantom apply: the engine holds an increment no acknowledged transaction
// made (a refused transaction that left a trace).
func TestMVCCContention_PhantomApplyInjection(t *testing.T) {
	defer goleak.VerifyNone(t)
	h := contInjectionHarness(t)
	// Apply an increment directly, behind the bookkeeping's back.
	setup := h.store.Engine().NewSession()
	if err := h.exec(context.Background(), setup, tmplCounterSet,
		map[string]any{"name": counterName(0), "v": int64(1)}); err != nil {
		t.Fatalf("phantom write: %v", err)
	}

	v := h.adjudicate(context.Background())
	if len(v) == 0 {
		t.Fatal("adjudication did not fire on a phantom apply")
	}
	if !strings.Contains(v[0].Message, "PHANTOM APPLY") {
		t.Fatalf("wrong finding: %v", v[0])
	}
}

// TestMVCCContention_ConflictTraceInjection proves the control-key half fires:
// a value on a control key that no acknowledged transaction wrote.
func TestMVCCContention_ConflictTraceInjection(t *testing.T) {
	defer goleak.VerifyNone(t)
	h := contInjectionHarness(t)
	setup := h.store.Engine().NewSession()
	if err := h.exec(context.Background(), setup, tmplCounterSet,
		map[string]any{"name": controlName(0), "v": int64(77)}); err != nil {
		t.Fatalf("trace write: %v", err)
	}

	v := h.adjudicate(context.Background())
	if len(v) == 0 {
		t.Fatal("adjudication did not fire on a conflicted transaction's trace")
	}
	if v[0].Op != "conflict trace" {
		t.Fatalf("wrong finding: %v", v[0])
	}
}

// contInjectionHarness builds a minimal live contention harness: one counter,
// one control key, zero acknowledged activity.
func contInjectionHarness(t *testing.T) *contHarness {
	t.Helper()
	store, err := OpenSimStore(NewSimDisk(NewSeed(3), 0), simulatorStoreConfig())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := &contHarness{
		store:           store,
		res:             &MVCCContentionResult{Counters: 1, Sessions: 1, AckedIncrements: make([]int, 1)},
		expectedControl: make([]int64, 1),
	}
	setup := store.Engine().NewSession()
	for _, q := range []map[string]any{
		{"name": counterName(0), "v": int64(0)},
		{"name": controlName(0), "v": int64(0)},
	} {
		if err := h.exec(context.Background(), setup, "CREATE (n:Person {name:$name, val:$v})", q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return h
}
