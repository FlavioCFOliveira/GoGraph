package sim

import "testing"

// collectCrashTicks runs a schedule built from seed over [1,ticks] and returns
// the ticks at which it fired. The schedule draws from the crash sub-seed
// exactly as the simulator wires it.
func collectCrashTicks(seed uint64, cfg CrashConfig, ticks int64) []int64 {
	cs := NewCrashSchedule(NewSeed(seed^crashSeedMix), cfg)
	var fired []int64
	for tick := int64(1); tick <= ticks; tick++ {
		if cs.ShouldCrash(tick) {
			fired = append(fired, tick)
		}
	}
	return fired
}

// TestCrashSchedule_Reproducible verifies the same seed yields the identical
// crash tick sequence across two independent schedules — the core determinism
// guarantee.
func TestCrashSchedule_Reproducible(t *testing.T) {
	cfg := CrashConfig{Enabled: true}
	a := collectCrashTicks(0xC0FFEE, cfg, 20_000)
	b := collectCrashTicks(0xC0FFEE, cfg, 20_000)

	if len(a) == 0 {
		t.Fatal("expected at least one crash over 20k ticks at the default rate")
	}
	if len(a) != len(b) {
		t.Fatalf("crash count not reproducible: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("crash schedule diverged at index %d: %d vs %d", i, a[i], b[i])
		}
	}
}

// TestCrashSchedule_DifferentSeedsDiffer verifies distinct seeds explore
// distinct crash schedules (a smoke test that the seed actually drives timing).
func TestCrashSchedule_DifferentSeedsDiffer(t *testing.T) {
	cfg := CrashConfig{Enabled: true}
	a := collectCrashTicks(1, cfg, 20_000)
	b := collectCrashTicks(2, cfg, 20_000)
	if len(a) == len(b) && equalInt64(a, b) {
		t.Fatal("two different seeds produced the identical crash schedule")
	}
}

// TestCrashSchedule_StabilityWindowRespected verifies that consecutive crashes
// are always separated by more than the stability window: recovery is given
// time to settle and be re-validated before the next fault.
func TestCrashSchedule_StabilityWindowRespected(t *testing.T) {
	const window = 50
	cfg := CrashConfig{Enabled: true, StabilityWindow: window}
	fired := collectCrashTicks(0xABCD, cfg, 50_000)
	if len(fired) < 2 {
		t.Fatalf("need at least two crashes to test spacing, got %d", len(fired))
	}
	for i := 1; i < len(fired); i++ {
		gap := fired[i] - fired[i-1]
		if gap <= window {
			t.Fatalf("crashes %d and %d only %d ticks apart, want > %d (stability window)",
				fired[i-1], fired[i], gap, window)
		}
	}
}

// TestCrashSchedule_DisabledNeverFiresNorDraws verifies the safe default: a
// disabled schedule never crashes and never consumes a draw, so enabling
// crashes cannot shift any other seed-driven stream.
func TestCrashSchedule_DisabledNeverFiresNorDraws(t *testing.T) {
	seed := NewSeed(7 ^ crashSeedMix)
	probe := NewSeed(7 ^ crashSeedMix) // identical generator state.

	cs := NewCrashSchedule(seed, CrashConfig{Enabled: false})
	for tick := int64(1); tick <= 10_000; tick++ {
		if cs.ShouldCrash(tick) {
			t.Fatalf("disabled schedule fired at tick %d", tick)
		}
	}
	// The schedule's seed must be untouched: it should still match a fresh
	// generator at the same value. Compare the next draw from each.
	if seed.Uint64N(1<<40) != probe.Uint64N(1<<40) {
		t.Fatal("disabled schedule consumed a draw from its seed")
	}
}

// TestCrashSchedule_ZeroValueConfigIsDisabled verifies the zero CrashConfig is
// inert, so a caller that does not opt in gets the no-crash behaviour.
func TestCrashSchedule_ZeroValueConfigIsDisabled(t *testing.T) {
	cs := NewCrashSchedule(NewSeed(1), CrashConfig{})
	if cs.Enabled() {
		t.Fatal("zero CrashConfig must be disabled")
	}
	if cs.ShouldCrash(1) {
		t.Fatal("zero CrashConfig must never crash")
	}
}

// TestCrashSchedule_DefaultsApplied verifies non-positive parameters fall back
// to the package defaults when crashes are enabled.
func TestCrashSchedule_DefaultsApplied(t *testing.T) {
	cs := NewCrashSchedule(NewSeed(1), CrashConfig{Enabled: true})
	if cs.crashProb != defaultCrashProb {
		t.Fatalf("crashProb = %v, want default %v", cs.crashProb, defaultCrashProb)
	}
	if cs.stabilityWindow != defaultStabilityWindow {
		t.Fatalf("stabilityWindow = %d, want default %d", cs.stabilityWindow, defaultStabilityWindow)
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
