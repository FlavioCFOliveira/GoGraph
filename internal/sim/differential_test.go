package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// recordTraceForDiff records a deterministic write-heavy trace for the seed, so
// the differential has a real op stream (reads + writes) to compare variants on.
func recordTraceForDiff(t *testing.T, seed uint64, ticks int) Trace {
	t.Helper()
	cfg := Config{
		Seed:     seed,
		MaxTicks: ticks,
		Workload: WriteHeavyWorkload(NewSeed(seed)),
	}
	trace, _, err := RecordTrace(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RecordTrace(seed=%d): %v", seed, err)
	}
	if trace.Len() == 0 {
		t.Fatalf("recorded an empty trace for seed=%d", seed)
	}
	return trace
}

// TestDifferential_IdenticalVariantsAgreeShort is the short-layer positive
// smoke: one variant pair on one seed over a small trace must produce
// byte-identical observable output. It keeps the differential plumbing wired on
// every PR; the full seed/pair matrix runs in the soak lane below.
func TestDifferential_IdenticalVariantsAgreeShort(t *testing.T) {
	trace := recordTraceForDiff(t, 0x5217E, 120)
	a, b := DefaultVariantPair()
	res, err := DifferentialTrace(context.Background(), trace, &a, &b)
	if err != nil {
		t.Fatalf("DifferentialTrace: %v", err)
	}
	if !res.Agreed {
		t.Fatalf("variants diverged on an equivalent-result toggle (a regression):\n%s", res.String())
	}
}

// TestDifferential_ParallelScanAgreesShort is the short-layer parallelism
// differential: the morsel-parallel count reduce (threshold lowered to 1 so it
// engages on the small trace graph) must produce byte-identical observable
// output to the forced-serial path. It brings the engine's parallel count path
// under the DST on every PR; the full seed sweep is in the soak matrix below.
func TestDifferential_ParallelScanAgreesShort(t *testing.T) {
	trace := recordTraceForDiff(t, 0x5217E, 120)
	a, b := ParallelScanVariantPair()
	res, err := DifferentialTrace(context.Background(), trace, &a, &b)
	if err != nil {
		t.Fatalf("DifferentialTrace: %v", err)
	}
	if !res.Agreed {
		t.Fatalf("parallel-scan and serial-scan diverged (a parallel-reduce regression):\n%s", res.String())
	}
}

// TestDifferential_IdenticalVariantsAgree is the PRIMARY positive case: the
// engine's default plan and the same engine with the hash-join (and,
// separately, the range-seek) optimisation disabled MUST produce byte-identical
// observable output on the same recorded trace.
//
// Gated to the soak layer: it sweeps two variant pairs across three seeds, each
// over a 400-op trace, which is minutes-long under -race. The short-layer
// TestDifferential_IdenticalVariantsAgreeShort covers the same path at one
// (pair, seed) on every PR.
func TestDifferential_IdenticalVariantsAgree(t *testing.T) {
	testlayers.RequireSoak(t)
	pairs := []struct {
		name string
		pair func() (EngineVariant, EngineVariant)
	}{
		{"hash-join", DefaultVariantPair},
		{"range-seek", RangeSeekVariantPair},
		{"parallel-scan", ParallelScanVariantPair},
	}
	seeds := []uint64{0x5217E, 0xC0FFEE, 0xDA7A}
	for _, p := range pairs {
		for _, seed := range seeds {
			t.Run(p.name, func(t *testing.T) {
				trace := recordTraceForDiff(t, seed, 400)
				a, b := p.pair()
				res, err := DifferentialTrace(context.Background(), trace, &a, &b)
				if err != nil {
					t.Fatalf("DifferentialTrace: %v", err)
				}
				if !res.Agreed {
					t.Fatalf("variants diverged on an equivalent-result toggle (a regression):\n%s", res.String())
				}
			})
		}
	}
}

// TestDifferential_CatchesInjectedDivergence is the AC negative case: when one
// variant drops a write the trace applied, the differential must catch the
// divergence and report the first diverging op.
func TestDifferential_CatchesInjectedDivergence(t *testing.T) {
	trace := recordTraceForDiff(t, 0x5217E, 150)

	// Find the first write op to drop on variant B.
	injectAt := -1
	for i := range trace.Ops {
		if trace.Ops[i].Op.Kind.IsWrite() {
			injectAt = i
			break
		}
	}
	if injectAt < 0 {
		t.Fatal("no write op in the trace to inject a divergence into")
	}

	a, b := DefaultVariantPair()
	res, err := DifferentialTraceInjectB(context.Background(), trace, &a, &b, injectAt)
	if err != nil {
		t.Fatalf("DifferentialTraceInjectB: %v", err)
	}
	if res.Agreed {
		t.Fatal("differential FAILED to catch an injected lost-write divergence")
	}
	if res.DivergedAt < injectAt {
		t.Errorf("divergence reported at op %d, before the injection at %d", res.DivergedAt, injectAt)
	}
	if res.SignatureA == res.SignatureB {
		t.Errorf("diverging signatures are equal: %q", res.SignatureA)
	}
}

// TestDifferential_NoOpInjection asserts that injecting at -1 is equivalent to a
// plain differential (a guard that the injection plumbing is opt-in).
func TestDifferential_NoOpInjection(t *testing.T) {
	trace := recordTraceForDiff(t, 0xDA7A, 120)
	a, b := DefaultVariantPair()
	res, err := DifferentialTraceInjectB(context.Background(), trace, &a, &b, -1)
	if err != nil {
		t.Fatalf("DifferentialTraceInjectB: %v", err)
	}
	if !res.Agreed {
		t.Fatalf("no-op injection still diverged:\n%s", res.String())
	}
}
