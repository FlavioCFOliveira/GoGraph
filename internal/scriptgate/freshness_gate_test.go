package scriptgate

import (
	"strings"
	"testing"
)

// TestDocFreshnessGate guards #1402: it runs the self-contained hermetic
// test for scripts/check_doc_freshness.sh, which proves the strict
// re-stamp rule rejects an edit-without-restamp (the bolt.md gap) and
// accepts a proper re-stamp.
func TestDocFreshnessGate(t *testing.T) {
	out, err := runShellGate(t, "scripts/test_check_doc_freshness.sh")
	if err != nil {
		t.Fatalf("doc-freshness gate self-test failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "SKIP") {
		t.Skipf("doc-freshness gate self-test skipped:\n%s", out)
	}
	if !strings.Contains(out, "ALL CASES PASSED") {
		t.Fatalf("doc-freshness gate self-test did not report success:\n%s", out)
	}
}
