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
