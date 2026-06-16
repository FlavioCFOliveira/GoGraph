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
	Seed        uint64
	FailedTick  int64
	FailedOp    Op
	Violations  []Violation
	OracleState OracleSnapshot
	// Shrunk, when non-nil, carries the minimal failing reproducer the shrinker
	// produced for this failure ([ShrinkTrace]). It is attached by the CLI replay
	// path after a deterministic failure is shrunk; a report from a live run
	// leaves it nil.
	Shrunk *ShrinkResult
}

// String renders a human-readable failure report. It always includes a
// "Reproduce with:" line carrying the seed so a failure can be replayed
// verbatim.
func (r *SimReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "SIMULATION FAILED\n")
	fmt.Fprintf(&b, "  Seed:        %d\n", r.Seed)
	fmt.Fprintf(&b, "  Failed tick: %d\n", r.FailedTick)
	fmt.Fprintf(&b, "  Failed op:   kind=%s cypher=%q params=%v\n", r.FailedOp.Kind, r.FailedOp.Cypher, r.FailedOp.Params)
	fmt.Fprintf(&b, "  Oracle:      nodes=%d edges=%d ops=%d\n",
		r.OracleState.NodeCount, r.OracleState.EdgeCount, r.OracleState.OpCount)
	fmt.Fprintf(&b, "  Violations (%d):\n", len(r.Violations))
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "    - %s\n", v.String())
	}
	fmt.Fprintf(&b, "Reproduce with: go run ./cmd/sim %d\n", r.Seed)
	if r.Shrunk != nil {
		fmt.Fprintf(&b, "Minimal reproducer: %d ops (shrunk from %d, ratio %.1fx, %d replay iterations)\n",
			r.Shrunk.MinimalLen, r.Shrunk.OriginalLen, r.Shrunk.Ratio(), r.Shrunk.Iterations)
		b.WriteString(ReplayInstructions(r.Shrunk.Minimal))
	}
	return b.String()
}
