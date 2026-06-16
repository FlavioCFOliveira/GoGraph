package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun_PassesWithSeed verifies a small fixed-seed run exits 0 and prints the
// success summary including the seed.
func TestRun_PassesWithSeed(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"42", "--ticks=100"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Simulation passed. Seed: 42") {
		t.Fatalf("missing success line, got %q", out.String())
	}
}

// TestRun_InvalidSeed verifies a non-numeric seed argument exits 2.
func TestRun_InvalidSeed(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"not-a-number"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "invalid seed") {
		t.Fatalf("missing error message, got %q", errBuf.String())
	}
}

// TestRun_UnknownWorkload verifies an unknown workload name exits 2.
func TestRun_UnknownWorkload(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--workload=bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRun_Verbose verifies the verbose flag prints a per-op trace and the
// header.
func TestRun_Verbose(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"7", "--ticks=20", "--verbose"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "Running simulation:") {
		t.Fatalf("missing verbose header, got %q", s)
	}
	if !strings.Contains(s, "tick=") {
		t.Fatalf("missing per-op trace, got %q", s)
	}
}

// TestRun_WorkloadNames verifies each accepted workload name runs cleanly.
func TestRun_WorkloadNames(t *testing.T) {
	for _, name := range []string{"default", "write-heavy", "read-heavy"} {
		t.Run(name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			if code := run([]string{"3", "--ticks=80", "--workload=" + name}, &out, &errBuf); code != 0 {
				t.Fatalf("%s: exit %d stderr=%q", name, code, errBuf.String())
			}
		})
	}
}

// TestRun_WireMode verifies the Phase-3 lock-step wire mode runs and reports a
// reproducible round-trip.
func TestRun_WireMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"42", "--mode=wire"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Wire round-trip reproducible") {
		t.Fatalf("missing wire success line, got %q", out.String())
	}
}

// TestRun_ConcurrentMode verifies the Phase-3 concurrent mode runs and reports a
// consistent quiescence.
func TestRun_ConcurrentMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"7", "--mode=concurrent", "--conns=8", "--ops-per-conn=10"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Concurrent run consistent") {
		t.Fatalf("missing concurrent success line, got %q", out.String())
	}
}

// TestRun_LivenessMode verifies the Phase-3 two-phase safety->liveness mode runs
// and converges.
func TestRun_LivenessMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"9", "--mode=liveness", "--conns=8", "--ops-per-conn=10"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "converged") {
		t.Fatalf("missing liveness success line, got %q", out.String())
	}
}

// TestRun_UnknownMode verifies an unknown mode exits 2.
func TestRun_UnknownMode(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"1", "--mode=bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown mode") {
		t.Fatalf("missing error, got %q", errBuf.String())
	}
}
