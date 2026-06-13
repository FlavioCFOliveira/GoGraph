package scriptgate

import (
	"strings"
	"testing"
)

// TestNightlyGate guards #1448: it runs the self-contained test for
// scripts/test_nightly_gate.sh, which proves the nightly failure-detection
// logic (mirrored from nightly.yml) catches conditions that emit no "^FAIL"
// line — OOM kills (exit 137), signal terminations, and empty logs — and
// accepts a healthy run with "^ok " lines.
//
// Unlike the bench gate, this self-test has no external tool dependency, so
// it never skips: it must report all cases passing.
func TestNightlyGate(t *testing.T) {
	out, err := runShellGate(t, "scripts/test_nightly_gate.sh")
	if err != nil {
		t.Fatalf("nightly-gate self-test failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "0 failed") {
		t.Fatalf("nightly-gate self-test did not report all cases passing:\n%s", out)
	}
}
