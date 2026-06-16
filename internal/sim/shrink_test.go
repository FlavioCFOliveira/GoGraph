package sim

import (
	"context"
	"testing"
)

// buildFailingTrace records a clean deterministic run of n ops, then injects a
// single lost-write fault on a CREATE op deep inside it, yielding a large trace
// that fails on exactly one op — the ideal shrink target.
func buildFailingTrace(t *testing.T, seed uint64, n int) Trace {
	t.Helper()
	cfg := Config{Seed: seed, MaxTicks: n, Workload: WriteHeavyWorkload(NewSeed(seed))}
	trace, report, err := RecordTrace(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if report != nil {
		t.Fatalf("base run unexpectedly failed: %s", report)
	}
	// Inject the fault on the first CREATE op past the midpoint, so the failing op
	// is buried among many irrelevant ops.
	injected := false
	for i := len(trace.Ops) / 2; i < len(trace.Ops); i++ {
		if trace.Ops[i].Op.Kind == OpCreate {
			trace.Ops[i].Fault = FaultDropEngineWrite
			injected = true
			break
		}
	}
	if !injected {
		t.Fatal("no CREATE op available to inject a fault into")
	}
	return trace
}

// TestShrinkTrace_ReducesToSmallReproducer shrinks a 600-op trace with one
// injected lost-write fault to a tiny reproducer that still fails, and asserts
// an orders-of-magnitude reduction.
func TestShrinkTrace_ReducesToSmallReproducer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 600
	trace := buildFailingTrace(t, 2024, n)

	res, err := ShrinkTrace(ctx, trace, ShrinkConfig{})
	if err != nil {
		t.Fatalf("ShrinkTrace: %v", err)
	}
	if res.OriginalLen != n {
		t.Fatalf("OriginalLen = %d, want %d", res.OriginalLen, n)
	}
	// The single dropped-write CREATE alone reproduces the count divergence, so
	// the minimal reproducer must be a handful of ops at most — an
	// orders-of-magnitude reduction.
	if res.MinimalLen >= n/10 {
		t.Fatalf("insufficient shrink: %d ops (from %d, ratio %.1fx)", res.MinimalLen, res.OriginalLen, res.Ratio())
	}
	if res.Ratio() < 10 {
		t.Fatalf("expected >=10x reduction, got %.1fx (%d -> %d)", res.Ratio(), res.OriginalLen, res.MinimalLen)
	}

	// The minimal trace must STILL reproduce a violation under scripted replay.
	replay, err := ReplayTrace(ctx, res.Minimal)
	if err != nil {
		t.Fatalf("replay minimal: %v", err)
	}
	if !replay.Violated() {
		t.Fatal("minimal trace no longer reproduces the violation")
	}
	// The retained fault op must still be present.
	hasFault := false
	for _, op := range res.Minimal.Ops {
		if op.Fault == FaultDropEngineWrite {
			hasFault = true
		}
	}
	if !hasFault {
		t.Fatal("shrinker dropped the faulted op that causes the failure")
	}
}

// TestShrinkTrace_Deterministic shrinks the same failing trace twice and asserts
// both produce the identical minimal trace length and iteration count.
func TestShrinkTrace_Deterministic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	trace := buildFailingTrace(t, 777, 300)

	a, err := ShrinkTrace(ctx, trace, ShrinkConfig{})
	if err != nil {
		t.Fatalf("shrink a: %v", err)
	}
	b, err := ShrinkTrace(ctx, trace, ShrinkConfig{})
	if err != nil {
		t.Fatalf("shrink b: %v", err)
	}
	if a.MinimalLen != b.MinimalLen || a.Iterations != b.Iterations {
		t.Fatalf("shrink not deterministic: a(len=%d it=%d) b(len=%d it=%d)",
			a.MinimalLen, a.Iterations, b.MinimalLen, b.Iterations)
	}
}

// TestShrinkTrace_ErrorsWhenNoViolation verifies shrinking a clean trace is an
// error (nothing to shrink), not a silent empty result.
func TestShrinkTrace_ErrorsWhenNoViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Seed: 5, MaxTicks: 100, Workload: DefaultWorkload(NewSeed(5))}
	trace, report, err := RecordTrace(ctx, cfg)
	if err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	if report != nil {
		t.Fatalf("base run unexpectedly failed: %s", report)
	}
	if _, err := ShrinkTrace(ctx, trace, ShrinkConfig{}); err == nil {
		t.Fatal("expected an error shrinking a non-failing trace")
	}
}

// TestShrinkTrace_BoundedIterations verifies the iteration cap is honoured.
func TestShrinkTrace_BoundedIterations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	trace := buildFailingTrace(t, 11, 400)
	res, err := ShrinkTrace(ctx, trace, ShrinkConfig{MaxIterations: 50})
	if err != nil {
		t.Fatalf("ShrinkTrace: %v", err)
	}
	if res.Iterations > 50 {
		t.Fatalf("iteration cap breached: %d > 50", res.Iterations)
	}
}
