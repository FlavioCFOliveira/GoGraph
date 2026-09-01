package contention_test

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bench/contention"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// childMode is the subproc mode that performs ONE window of one measurement.
//
// A fresh child per WINDOW, not merely per measurement, is the whole point.
// Two kinds of state accumulate across a process and neither can be reset:
//
//   - The mutex and block profiles are cumulative for a process's entire
//     lifetime, so a second measurement inherits the first one's samples.
//   - The heap is cumulative in the same way. When both windows ran in one
//     child, the profiled window inherited the unprofiled window's warm heap,
//     raised GC goal, mapped pages and populated pools — and measured FASTER
//     while carrying a full-rate profiler, in five of six runs.
//
// One window, one process, no inheritance.
//
// argv: <workload> <level> <window> <outDir>
const childMode = "contention-observe"

// envOpsScale multiplies a workload's declared Ops in the CHILD.
//
// It exists for the differential that separates a one-shot cold-start cost
// from steady-state contention: a lock site whose ABSOLUTE blocked time is
// unchanged when the same workload does ten times the work paid that cost
// once, at start-up, and is not contention the module suffers in service. A
// site whose blocked time rises with the work is.
//
// The child reads it from its inherited environment; subproc.RunCtx passes
// os.Environ() through, so the parent need only set it once.
const envOpsScale = "GOGRAPH_CONTENTION_OPS_SCALE"

// scaledOps applies envOpsScale to a declared operation count. A malformed or
// non-positive value is ignored rather than silently halving the workload.
func scaledOps(ops int) (int, error) {
	raw := os.Getenv(envOpsScale)
	if raw == "" {
		return ops, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("bad %s=%q", envOpsScale, raw)
	}
	scaled := int(float64(ops) * f)
	if scaled < 1 {
		scaled = 1
	}
	return scaled, nil
}

func init() {
	subproc.Register(childMode, func(args []string) int {
		if len(args) != 4 {
			fmt.Fprintf(os.Stderr, "usage: %s <workload> <level> <window> <outDir>\n", childMode)
			return 2
		}
		name, levelArg, windowArg, outDir := args[0], args[1], args[2], args[3]

		w, ok := contention.ByName(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown workload %q\n", name)
			return 2
		}
		level, err := strconv.Atoi(levelArg)
		if err != nil || level < 1 {
			fmt.Fprintf(os.Stderr, "bad level %q\n", levelArg)
			return 2
		}
		win, ok := contention.ParseWindow(windowArg)
		if !ok {
			fmt.Fprintf(os.Stderr, "bad window %q, want %q or %q\n",
				windowArg, contention.WindowEffect, contention.WindowProbe)
			return 2
		}

		ops, err := scaledOps(w.Ops)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 2
		}
		w.Ops = ops

		m, err := contention.Observe(w, level, win, outDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "observe: %v\n", err)
			return 1
		}
		if err := contention.WriteMetrics(outDir, &m); err != nil {
			fmt.Fprintf(os.Stderr, "write metrics: %v\n", err)
			return 1
		}
		// One machine-readable line so the parent can report without
		// re-reading the file.
		fmt.Printf("OK workload=%s window=%s level=%d ops=%d ops_per_sec=%.1f p50_ns=%d p99_ns=%d errors=%d\n",
			m.Workload, win, m.Level, m.Ops, m.OpsPerSec, m.P50Nanos, m.P99Nanos, m.Errors)
		return 0
	})
}

func TestMain(m *testing.M) {
	subproc.Dispatch() // exits in child mode; no-op in the parent
	goleak.VerifyTestMain(m)
}
