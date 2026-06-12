package scriptgate

import (
	"strings"
	"testing"
)

// TestReleaseSoakGate guards #1399: it runs the self-contained hermetic
// test for scripts/release_soak_gate.sh, which proves the gate fails
// when no green soak run exists for the release commit, passes when one
// does, and fails closed when the GitHub CLI is unavailable.
func TestReleaseSoakGate(t *testing.T) {
	out, err := runShellGate(t, "scripts/test_release_soak_gate.sh")
	if err != nil {
		t.Fatalf("release soak-gate self-test failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ALL CASES PASSED") {
		t.Fatalf("release soak-gate self-test did not report success:\n%s", out)
	}
}
