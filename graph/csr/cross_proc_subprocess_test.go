package csr_test

import (
	"fmt"
	"strings"
	"testing"

	"gograph/internal/subproc"
)

// TestCSR_CrossProcess_Subprocess_ByteEqual spawns two independent child
// processes that each build a CSR from BarabasiAlbert(1000, 3, 42) and emit
// SHA-256 hashes of the vertices and edges slices. The parent asserts the
// hashes are identical between the two children.
//
// This extends the same-process TestCSR_CrossProcess_ByteEqual with an
// actual process boundary: the children have independent heaps, independent
// FNV-1a hash state, and independent per-shard intra-shard counters. Any
// process-local entropy that leaked into the CSR layout would surface as a
// hash mismatch.
func TestCSR_CrossProcess_Subprocess_ByteEqual(t *testing.T) {
	t.Parallel()

	run := func(label string) (verticesHash, edgesHash string) {
		stdout, stderr, err := subproc.Run(t, "csr-build-sha256")
		if err != nil {
			t.Fatalf("%s: subproc.Run: %v\nstderr: %s", label, err, stderr)
		}
		vh, eh, parseErr := parseCSRHashOutput(string(stdout))
		if parseErr != nil {
			t.Fatalf("%s: parse output: %v\nstdout: %s", label, parseErr, stdout)
		}
		return vh, eh
	}

	v1, e1 := run("child1")
	v2, e2 := run("child2")

	if v1 != v2 {
		t.Errorf("vertices hash mismatch:\n  child1=%s\n  child2=%s", v1, v2)
	}
	if e1 != e2 {
		t.Errorf("edges hash mismatch:\n  child1=%s\n  child2=%s", e1, e2)
	}
	if v1 == v2 && e1 == e2 {
		t.Logf("BarabasiAlbert(1000,3,42) CSR is byte-equal across two independent child processes: vertices=%s edges=%s",
			v1, e1)
	}
}

// parseCSRHashOutput parses the two-line output emitted by the
// "csr-build-sha256" handler:
//
//	vertices:<hex>
//	edges:<hex>
func parseCSRHashOutput(output string) (verticesHash, edgesHash string, err error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("expected 2 lines, got %d", len(lines))
	}
	vh, ok := strings.CutPrefix(lines[0], "vertices:")
	if !ok {
		return "", "", fmt.Errorf("expected line 0 to start with 'vertices:', got %q", lines[0])
	}
	eh, ok := strings.CutPrefix(lines[1], "edges:")
	if !ok {
		return "", "", fmt.Errorf("expected line 1 to start with 'edges:', got %q", lines[1])
	}
	return vh, eh, nil
}
