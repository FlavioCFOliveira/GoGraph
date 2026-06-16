package sim

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// LivenessConfig parameterises the liveness phase. It runs AFTER the safety
// phase, with all fault injection healed/disabled: only honest actors run, and
// the harness asserts the system CONVERGES (drains to quiescence) within a
// bounded tick budget. A watchdog catches the resonance class — pending work
// that never makes progress (deadlock/livelock).
type LivenessConfig struct {
	// Seed controls the honest convergence workload.
	Seed uint64
	// Connections / OpsPerConn bound the honest convergence workload (same
	// bounded-resource discipline as the concurrent harness).
	Connections int
	OpsPerConn  int
	// ConvergeBudget is the maximum simulated time the harness waits for the
	// pending() predicate to reach null. Exceeding it is a liveness failure
	// (the system did not converge). Measured on the injected clock.
	ConvergeBudget time.Duration
	// PollStep is the simulated interval between pending() samples. Values <= 0
	// default to ConvergeBudget/100 (at least 1ms).
	PollStep time.Duration
	// NoProgressGrace is the number of consecutive non-improving samples the
	// watchdog tolerates before declaring a resonance (no progress despite
	// pending work). Values <= 0 default to 8.
	NoProgressGrace int
}

// PendingState is the liveness predicate's snapshot of outstanding work. The
// system is QUIESCENT when every field is at its converged value: no in-flight
// operations, no ungated (undrained) streams, and the oracle equal to the
// engine. Goroutine-leak detection is delegated to goleak in test teardown
// (process-global goroutine counts are too noisy to gate convergence on), so it
// is deliberately NOT a pending() term.
type PendingState struct {
	InFlightOps     int   // operations not yet acknowledged (0 at quiescence)
	UngatedStreams  int   // result streams opened but not drained (0 at quiescence)
	OracleDivergent bool  // expected engine node count != observed engine node count
	ExpectedNodes   int64 // for the divergence report (baseline + acked creates)
	EngineNodeCount int64
}

// Pending reports whether any work is still outstanding. False means quiescent.
func (p PendingState) Pending() bool {
	return p.InFlightOps > 0 || p.UngatedStreams > 0 || p.OracleDivergent
}

// Magnitude is a scalar measure of outstanding work the watchdog tracks for
// progress: a strictly decreasing magnitude is progress, a flat non-zero
// magnitude across the grace window is resonance.
func (p PendingState) Magnitude() int {
	m := p.InFlightOps + p.UngatedStreams
	if p.OracleDivergent {
		// Weight a divergence by its size so shrinking divergence counts as
		// progress.
		d := p.ExpectedNodes - p.EngineNodeCount
		if d < 0 {
			d = -d
		}
		m += int(d) + 1
	}
	return m
}

// String renders a PendingState for the liveness report's pending-work dump.
func (p PendingState) String() string {
	return fmt.Sprintf("in-flight=%d ungated-streams=%d oracle-divergent=%t (expected=%d engine=%d)",
		p.InFlightOps, p.UngatedStreams, p.OracleDivergent, p.ExpectedNodes, p.EngineNodeCount)
}

// LivenessOutcome is the result of the liveness phase. Converged is true when
// the system reached quiescence within the budget. When false, FinalPending and
// Resonance describe why: Resonance == true means the watchdog detected
// no-progress-despite-pending (deadlock/livelock); Resonance == false means the
// budget simply elapsed while still making progress (under-provisioned budget).
type LivenessOutcome struct {
	Seed         uint64
	Converged    bool
	Resonance    bool
	Ticks        int
	FinalPending PendingState
}

// Report renders a VOPR-style liveness failure report with the seed and the
// pending-work dump, mirroring the safety phase's [SimReport.String]. It returns
// the empty string for a converged outcome.
func (o LivenessOutcome) Report() string {
	if o.Converged {
		return ""
	}
	var b strings.Builder
	if o.Resonance {
		fmt.Fprintf(&b, "LIVENESS FAILED (RESONANCE: no progress despite pending work)\n")
	} else {
		fmt.Fprintf(&b, "LIVENESS FAILED (did not converge within budget)\n")
	}
	fmt.Fprintf(&b, "  Seed:          %d\n", o.Seed)
	fmt.Fprintf(&b, "  Liveness ticks: %d\n", o.Ticks)
	fmt.Fprintf(&b, "  Pending work:  %s\n", o.FinalPending.String())
	fmt.Fprintf(&b, "Reproduce with: go run ./cmd/sim %d --liveness\n", o.Seed)
	return b.String()
}

// RunLiveness runs the LIVENESS phase against srv: it executes a bounded honest
// convergence workload (no faults) and then polls the pending() predicate on the
// injected clock until the system is quiescent or the budget elapses, with a
// watchdog for the resonance (no-progress) class.
//
// The honest workload is run to completion first (every connection goroutine is
// joined), so at the polling stage the only residual pending work is structural
// (an oracle divergence the engine never reconciled, or a leaked goroutine). A
// healthy system is quiescent on the first poll; a system with a real
// liveness bug (a stuck writer, a leaked stream goroutine, a permanent oracle
// divergence) never reaches quiescence and the budget/watchdog fires.
//
// # Determinism
//
// The convergence workload follows the concurrent (non-bit-reproducible) model,
// but the polling and watchdog are driven by the injected [clock.Clock], so the
// liveness decision (converged vs budget-exceeded vs resonance) is deterministic
// under a [clock.Fake] for a given pending() trajectory.
func RunLiveness(ctx context.Context, srv *SimServer, clk clock.Clock, cfg LivenessConfig) (LivenessOutcome, error) {
	if cfg.Connections <= 0 {
		cfg.Connections = 8
	}
	if cfg.OpsPerConn <= 0 {
		cfg.OpsPerConn = 10
	}
	if cfg.ConvergeBudget <= 0 {
		cfg.ConvergeBudget = time.Second
	}
	if cfg.PollStep <= 0 {
		cfg.PollStep = cfg.ConvergeBudget / 100
		if cfg.PollStep <= 0 {
			cfg.PollStep = time.Millisecond
		}
	}
	if cfg.NoProgressGrace <= 0 {
		cfg.NoProgressGrace = 8
	}

	// Baseline the engine's node count BEFORE the convergence workload, so the
	// oracle divergence is measured against this phase's own acknowledged creates
	// rather than any nodes a prior (safety) phase left in the shared engine. The
	// liveness invariant is: expected == baseline + ackedCreates must equal the
	// engine's live count once the honest workload has drained.
	baselineNodes, err := queryNodeCount(srv)
	if err != nil {
		return LivenessOutcome{Seed: cfg.Seed}, fmt.Errorf("sim: liveness baseline node-count: %w", err)
	}

	// Run the honest convergence workload to completion (faults disabled — this is
	// a plain honest mix). Joining it means no honest op is left in flight; the
	// polling stage then observes only structural residue.
	conc, err := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        cfg.Seed,
		Connections: cfg.Connections,
		OpsPerConn:  cfg.OpsPerConn,
		Mix:         &ConcurrentMix{WriterWeight: 0.6, ReaderWeight: 0.4}, // honest only
	})
	if err != nil {
		return LivenessOutcome{Seed: cfg.Seed}, fmt.Errorf("sim: liveness convergence workload: %w", err)
	}

	// pending() snapshots the residual outstanding work. The honest workload is
	// joined, so InFlightOps/UngatedStreams are 0 by construction; the meaningful
	// residue is an unreconciled oracle divergence — the engine's live node count
	// not matching the baseline plus this phase's acknowledged creates. Goroutine
	// leaks are caught by goleak in test teardown, not here.
	pending := func() PendingState {
		expected := baselineNodes + conc.AckedCreates
		engine, qerr := queryNodeCount(srv)
		if qerr != nil {
			// A query failure during convergence polling is itself a non-quiescent
			// signal; model it as a divergence so the harness does not falsely
			// declare convergence.
			return PendingState{OracleDivergent: true, ExpectedNodes: expected, EngineNodeCount: -1}
		}
		return PendingState{
			OracleDivergent: expected != engine,
			ExpectedNodes:   expected,
			EngineNodeCount: engine,
		}
	}

	out := pollLiveness(ctx, clk, cfg, pending)
	out.Seed = cfg.Seed
	return out, nil
}

// pollLiveness polls pending() on the clock until quiescent, the budget elapses,
// or the watchdog detects resonance. It returns the classified outcome.
func pollLiveness(ctx context.Context, clk clock.Clock, cfg LivenessConfig, pending func() PendingState) LivenessOutcome {
	deadline := clk.Now().Add(cfg.ConvergeBudget)
	ticker := clk.NewTicker(cfg.PollStep)
	defer ticker.Stop()

	prevMag := -1
	stagnant := 0
	ticks := 0

	for {
		state := pending()
		if !state.Pending() {
			return LivenessOutcome{Converged: true, Ticks: ticks, FinalPending: state}
		}

		// Watchdog: track progress. A strictly smaller magnitude resets the
		// stagnation counter; a non-improving magnitude that persists for the grace
		// window is the resonance (no-progress-despite-pending) class.
		mag := state.Magnitude()
		if prevMag < 0 || mag < prevMag {
			stagnant = 0
		} else {
			stagnant++
		}
		prevMag = mag
		if stagnant >= cfg.NoProgressGrace {
			return LivenessOutcome{Converged: false, Resonance: true, Ticks: ticks, FinalPending: state}
		}

		if !clk.Now().Before(deadline) {
			return LivenessOutcome{Converged: false, Resonance: false, Ticks: ticks, FinalPending: state}
		}

		select {
		case <-ctx.Done():
			return LivenessOutcome{Converged: false, Resonance: false, Ticks: ticks, FinalPending: state}
		case <-ticker.C():
			ticks++
		}
	}
}
