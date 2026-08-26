package sim

import (
	"fmt"
	"strings"
)

// OracleSnapshot is an immutable summary of the oracle state at the moment a
// simulation failed, captured for the report. It deliberately holds only
// aggregate counts and the operation history length, not the full node/edge
// maps, so a report stays compact; the seed plus the failing tick are enough to
// replay the full state.
type OracleSnapshot struct {
	NodeCount int
	EdgeCount int
	OpCount   int
}

// SimReport is the result of a failed simulation: the seed that produced it,
// the tick and operation at which the first violation was detected, every
// violation found at that tick, and a snapshot of the oracle state. A nil
// *SimReport returned from [Simulator.Run] means the simulation passed.
//
// The DST harness types share a SimXxx naming scheme by design (see SimDisk in
// disk.go).
//
//nolint:revive // intentional SimXxx naming scheme (see comment above).
type SimReport struct {
	// Shrunk, when non-nil, carries the minimal failing reproducer the shrinker
	// produced for this failure ([ShrinkTrace]). It is attached by the CLI replay
	// path after a deterministic failure is shrunk; a report from a live run
	// leaves it nil.
	Shrunk *ShrinkResult
	// Scenario is the catalogue key of the scenario that produced the failure,
	// and Mode the harness it ran under. Both are rendered, because a report is
	// read by an operator who has only the log: without them a sighting says
	// what broke but not what was running, and a CONCURRENT mode is precisely
	// the case where "not bit-reproducible" changes how it must be chased
	// (rmp #2347).
	Scenario    string
	FailedOp    Op
	Violations  []Violation
	OracleState OracleSnapshot
	Seed        uint64
	FailedTick  int64
	Mode        ExecMode
	// Repro is an explicit reproduction instruction for a run whose state is
	// NOT determined by the seed alone — typically a scenario driven from a test
	// with a configuration that has no command-line form. When it is set,
	// [SimReport.String] prints it verbatim instead of synthesising a command.
	//
	// It exists because the synthesised line was actively misleading: see
	// [SimReport.reproLine].
	Repro string
}

// reproLine returns the "Reproduce with:" body, or an explanation of why no
// command can be given.
//
// # The line used to be a lie, and a green one (rmp #2621)
//
// It was always `go run ./cmd/sim <seed>`, unconditionally. That command runs
// cmd/sim's DEFAULT workload, so for any failure raised from a NAMED scenario it
// re-ran something else entirely — and reported SUCCESS. MEASURED on the #2620
// soak failure: the report said `Reproduce with: go run ./cmd/sim 2516635845`
// and that command printed "Simulation passed. Seed: 2516635845, Ticks: 100000".
//
// A reproduction that passes is the most convincing lie an instrument can tell,
// because it is evidence for the wrong conclusion — that the failure was a
// flake. A report with NO reproduction line is strictly better.
//
// Three cases, and the line now distinguishes them:
//
//   - no scenario name: the default workload, which the seed does determine.
//     The original line was correct here and keeps it.
//   - a scenario in the catalogue: reproducible, but only with the selector the
//     old line omitted. cmd/sim has -scenario for exactly this.
//   - anything else, or an explicit Repro: the run carries state the command
//     line cannot express, so the caller supplies the instruction or the report
//     says plainly that it cannot be reproduced from the seed.
func (r *SimReport) reproLine() string {
	if r.Repro != "" {
		return r.Repro
	}
	if r.Scenario == "" {
		return fmt.Sprintf("go run ./cmd/sim %d", r.Seed)
	}
	if reg, err := DefaultRegistry(); err == nil {
		if _, ok := reg.Lookup(r.Scenario); ok {
			return fmt.Sprintf("go run ./cmd/sim -scenario=%s %d", r.Scenario, r.Seed)
		}
	}
	return fmt.Sprintf("NOT REPRODUCIBLE FROM THE SEED ALONE: scenario %q is not in the "+
		"cmd/sim catalogue, so it was driven from a test with a configuration the command line "+
		"cannot express. Re-run that test. (No command is printed rather than one that would "+
		"run a different workload and report success — rmp #2621.)", r.Scenario)
}

// String renders a human-readable failure report. It always includes a
// "Reproduce with:" line, which either reproduces the failure or says why it
// cannot — never a command that runs a different workload (see
// [SimReport.reproLine]).
//
// IT CAN NEVER RENDER EMPTY, and a report that carries no violation says so
// LOUDLY rather than rendering a bare header (rmp #2347). A non-nil report
// means the scenario failed; one that names no violated invariant is itself a
// reporting defect, and an operator who cannot tell that from a clean run has
// been told nothing. [TestSimReportNeverRendersEmptyShort] pins both halves.
func (r *SimReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "SIMULATION FAILED\n")
	if r.Scenario != "" {
		fmt.Fprintf(&b, "  Scenario:    %s (mode=%s)\n", r.Scenario, r.Mode)
	}
	fmt.Fprintf(&b, "  Seed:        %d\n", r.Seed)
	fmt.Fprintf(&b, "  Failed tick: %d\n", r.FailedTick)
	fmt.Fprintf(&b, "  Failed op:   kind=%s cypher=%q params=%v\n", r.FailedOp.Kind, r.FailedOp.Cypher, r.FailedOp.Params)
	fmt.Fprintf(&b, "  Oracle:      nodes=%d edges=%d ops=%d\n",
		r.OracleState.NodeCount, r.OracleState.EdgeCount, r.OracleState.OpCount)
	if len(r.Violations) == 0 {
		fmt.Fprintf(&b, "  Violations (0): *** REPORTING DEFECT: this report names no violated "+
			"invariant. A non-nil report means the scenario FAILED, so the absence of a "+
			"violation here is a bug in whatever built the report, not a clean run. ***\n")
	} else {
		fmt.Fprintf(&b, "  Violations (%d):\n", len(r.Violations))
		for _, v := range r.Violations {
			fmt.Fprintf(&b, "    - %s\n", v.String())
		}
	}
	fmt.Fprintf(&b, "Reproduce with: %s\n", r.reproLine())
	if r.Shrunk != nil {
		fmt.Fprintf(&b, "Minimal reproducer: %d ops (shrunk from %d, ratio %.1fx, %d replay iterations)\n",
			r.Shrunk.MinimalLen, r.Shrunk.OriginalLen, r.Shrunk.Ratio(), r.Shrunk.Iterations)
		b.WriteString(ReplayInstructions(r.Shrunk.Minimal))
	}
	return b.String()
}
