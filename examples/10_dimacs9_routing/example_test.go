package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun drives run into a buffer and asserts only the deterministic
// invariants of the example's output: the synthetic graph's node and
// edge counts, and the concrete shortest path and distance from node 0
// to node 11. The trailing latency lines (p50/p95/p99) are wall-clock
// measurements that vary per run and per machine, so they are
// deliberately not asserted. This is the assertion-based form of the
// regression test mandated by docs/examples-standard.md for
// non-deterministic examples.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// Deterministic structural invariants. The synthetic generator
	// produces 12 nodes and 24 directed edges for the fixed inputs.
	for _, want := range []string{
		"Graph:  12 nodes, 24 edges",
		"SSSP:   node 0 -> node 11",
		"distance: 155",
		"path:     0 -> 1 -> 2 -> 3 -> 11",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}

	// The latency summary must be present and labelled as
	// environment-dependent, but its values are not asserted.
	if !strings.Contains(out, "Latency (environment-dependent") {
		t.Errorf("expected an environment-dependent latency summary, got:\n%s", out)
	}
	for _, label := range []string{"p50:", "p95:", "p99:"} {
		if !strings.Contains(out, label) {
			t.Errorf("expected latency label %q, got:\n%s", label, out)
		}
	}
}
