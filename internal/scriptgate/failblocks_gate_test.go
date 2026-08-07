package scriptgate

import (
	"strings"
	"testing"
)

// TestFailblocksGate guards rmp #2347: it runs the self-contained hermetic test
// for scripts/failblocks.awk, the filter that surfaces a failing coverage run's
// output.
//
// The filter it replaced matched only the FIRST line of a failing test and
// silently discarded every indented continuation line, so an ST3 durability
// violation reached the operator as a header with no body — naming neither the
// violated invariant nor the reproduction seed. The self-test runs the OLD
// filter alongside the new one on the same input and asserts the old one loses
// the body, so the guard demonstrates the defect rather than merely asserting
// its absence.
func TestFailblocksGate(t *testing.T) {
	out, err := runShellGate(t, "scripts/test_failblocks.sh")
	if err != nil {
		t.Fatalf("failblocks gate self-test failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "PASS: failblocks.awk preserves every failure block") {
		t.Fatalf("failblocks gate self-test did not report success:\n%s", out)
	}
}
