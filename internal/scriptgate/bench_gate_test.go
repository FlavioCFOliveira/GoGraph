package scriptgate

import (
	"strings"
	"testing"
)

// TestBenchGate guards #1447/#1448: it runs the self-contained test for
// scripts/bench_gate.sh, which proves the benchstat regression gate fires
// on a +100% regression, passes within threshold, and — after #1447 — fails
// when an expected headline benchmark disappears from the head run.
//
// test_bench_gate.sh self-skips (printing "SKIP:") when benchstat is not
// installed, a sanctioned environment-precondition skip; we honour it here
// rather than failing.
func TestBenchGate(t *testing.T) {
	out, err := runShellGate(t, "scripts/test_bench_gate.sh")
	if strings.Contains(out, "SKIP:") {
		t.Skipf("bench-gate self-test skipped (benchstat absent):\n%s", out)
	}
	if err != nil {
		t.Fatalf("bench-gate self-test failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "0 failed") {
		t.Fatalf("bench-gate self-test did not report all cases passing:\n%s", out)
	}
}
