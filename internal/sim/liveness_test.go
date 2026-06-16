package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestLiveness_HealthyConverges proves a healthy system reaches quiescence in the
// liveness phase: after an honest convergence workload, pending() is null (oracle
// equals engine, goroutines back to baseline) and the outcome is Converged.
func TestLiveness_HealthyConverges(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	out, err := RunLiveness(context.Background(), srv, clock.Real(), LivenessConfig{
		Seed:           0x11FE,
		Connections:    12,
		OpsPerConn:     15,
		ConvergeBudget: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunLiveness: %v", err)
	}
	if !out.Converged {
		t.Fatalf("healthy system did not converge:\n%s", out.Report())
	}
	if out.FinalPending.Pending() {
		t.Errorf("converged but pending state is non-null: %s", out.FinalPending)
	}
}

// TestLiveness_InjectedDeadlockCaughtByWatchdog proves the watchdog catches the
// resonance class: a pending() predicate that never makes progress (a modelled
// deadlock/livelock) is detected within the grace window and reported with the
// seed and pending dump. This drives pollLiveness directly with a synthetic
// stuck predicate (the in-package seam) so the failure is deterministic.
func TestLiveness_InjectedDeadlockCaughtByWatchdog(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Unix(0, 0))
	cfg := LivenessConfig{
		Seed:            0xDEAD,
		ConvergeBudget:  time.Hour, // large: the watchdog must fire FIRST, before the budget
		PollStep:        time.Second,
		NoProgressGrace: 4,
	}

	// A stuck predicate: always pending with a constant magnitude (no progress).
	stuck := func() PendingState {
		return PendingState{InFlightOps: 3} // never drains, magnitude never shrinks
	}

	// Advance the fake clock in the background so the ticker fires; the watchdog
	// must trip on stagnation well before the 1-hour budget.
	out := pollWithFakeAdvance(t, fake, time.Second, func() LivenessOutcome {
		return pollLiveness(context.Background(), fake, cfg, stuck)
	})
	out.Seed = cfg.Seed
	if out.Converged {
		t.Fatal("watchdog did not catch the modelled deadlock — reported Converged")
	}
	if !out.Resonance {
		t.Errorf("modelled deadlock classified as budget-exceeded, not resonance: %+v", out)
	}
	if out.Report() == "" {
		t.Error("a failed liveness outcome must produce a report")
	}
	if got := out.FinalPending.InFlightOps; got != 3 {
		t.Errorf("pending dump lost the stuck work: in-flight=%d, want 3", got)
	}
}

// TestLiveness_BudgetExceededWhileProgressing proves the non-resonance failure
// arm: a predicate that keeps improving but too slowly exhausts the budget and
// is reported as did-not-converge (NOT resonance, since it was making progress).
func TestLiveness_BudgetExceededWhileProgressing(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Unix(0, 0))
	cfg := LivenessConfig{
		Seed:            0xB0D,
		ConvergeBudget:  5 * time.Second,
		PollStep:        time.Second,
		NoProgressGrace: 100, // high: never trip the watchdog; let the budget win
	}

	// A predicate that shrinks by 1 each poll but starts far above what the budget
	// allows, so it is still progressing when the budget elapses.
	remaining := 1000
	progressing := func() PendingState {
		if remaining > 0 {
			remaining--
		}
		return PendingState{InFlightOps: remaining}
	}

	out := pollWithFakeAdvance(t, fake, time.Second, func() LivenessOutcome {
		return pollLiveness(context.Background(), fake, cfg, progressing)
	})
	if out.Converged {
		t.Fatal("expected budget-exceeded, got Converged")
	}
	if out.Resonance {
		t.Error("a progressing-but-slow run was misclassified as resonance")
	}
}

// TestLiveness_EndToEndSafetyThenLiveness wires the two phases together: a
// safety-phase concurrent run (with the overload faults active) followed by a
// liveness phase (faults healed) that must converge. This is the two-phase
// safety→liveness flow.
func TestLiveness_EndToEndSafetyThenLiveness(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// Phase 1 (safety): concurrent mixed workload INCLUDING the bounded overload
	// "faults". Must stay consistent and panic/leak free.
	safety, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:        0x5AFE,
		Connections: 16,
		OpsPerConn:  20,
		Mix:         &ConcurrentMix{WriterWeight: 0.5, ReaderWeight: 0.3, OverloadWeight: 0.2},
	})
	if err != nil {
		t.Fatalf("safety phase: %v", err)
	}
	if !safety.Consistent() {
		t.Fatalf("safety phase inconsistent: engine=%d acked=%d panics=%d transport=%d",
			safety.EngineNodeCount, safety.AckedCreates, safety.Panics, safety.TransportErrors)
	}

	// Phase 2 (liveness): faults healed, honest-only workload must converge.
	live, err := RunLiveness(context.Background(), srv, clock.Real(), LivenessConfig{
		Seed:           0x5AFE ^ 0x9E37,
		Connections:    8,
		OpsPerConn:     10,
		ConvergeBudget: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("liveness phase: %v", err)
	}
	if !live.Converged {
		t.Fatalf("system did not converge after the safety phase:\n%s", live.Report())
	}
}

// pollWithFakeAdvance runs fn on a goroutine (fn blocks on a fake ticker) while
// repeatedly advancing the fake clock by step so the ticker fires, and returns
// fn's result. It exists because a fake clock never advances on its own, so a
// goroutine waiting on its ticker would otherwise block forever.
func pollWithFakeAdvance(t *testing.T, fake *clock.Fake, step time.Duration, fn func() LivenessOutcome) LivenessOutcome {
	t.Helper()
	done := make(chan LivenessOutcome, 1)
	go func() { done <- fn() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case out := <-done:
			return out
		default:
		}
		fake.Advance(step)
		time.Sleep(time.Millisecond)
		if time.Now().After(deadline) {
			t.Fatal("pollWithFakeAdvance: outcome not produced within real-time safety net")
		}
	}
}
