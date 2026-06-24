package sim

import (
	"context"
	"runtime"
	"testing"

	"go.uber.org/goleak"
)

// TestCPUStarvation_ConvergesUnderClampedCore runs the cpu-starvation scenario:
// a hog-heavy concurrent workload competing with honest queries on a single
// clamped core, followed by a liveness convergence assertion. The system must
// still make forward progress — no deadlock/livelock (RESONANCE), no panic, no
// goroutine leak — proving the CLAUDE.md fair-scheduling mandate holds under CPU
// starvation. It must NOT run in parallel: the scenario clamps the process-global
// GOMAXPROCS for its duration.
func TestCPUStarvation_ConvergesUnderClampedCore(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioCPUStarvation)
	if !ok {
		t.Fatalf("cpu-starvation scenario not registered")
	}

	before := runtime.GOMAXPROCS(0)
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("cpu-starvation run: %v", err)
	}
	if report != nil {
		t.Fatalf("cpu-starvation reported a liveness/safety violation (fair scheduling broke):\n%s", report)
	}
	// The scenario must restore GOMAXPROCS on return — a leaked clamp would slow
	// every later test on the shared runtime.
	if after := runtime.GOMAXPROCS(0); after != before {
		t.Fatalf("cpu-starvation leaked a GOMAXPROCS clamp: before=%d after=%d", before, after)
	}
}
