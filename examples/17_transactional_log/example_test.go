package main

// example_test.go — assertion-based regression test for the transactional
// log example (#1189). The example runs a background checkpoint goroutine
// whose cadence depends on timing, prints stats that vary per run, and
// writes to an os.MkdirTemp directory whose path changes every run, so a
// // Output: block would be flaky. Instead the test asserts the
// DETERMINISTIC ACID invariant — every committed edge and its label is
// recovered from disk after a simulated crash — and never asserts on
// timing, checkpoint counts, or the temp path.
//
// TestMain runs the suite under go.uber.org/goleak so the background
// checkpointer is verified to shut down with no leaked goroutine. Run it
// under the race detector to confirm the shared-lock coordination is free
// of data races:
//
//	go test -race ./examples/17_transactional_log/...

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under go.uber.org/goleak so the
// background-checkpointer example doubles as a goroutine-leak check.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun drives run into a buffer and asserts the deterministic invariants
// that ACID durability guarantees: every committed edge and label is
// recovered. It deliberately ignores the volatile parts — the checkpoint
// stats, the WAL-op count (which depends on whether the background
// checkpointer fired before the crash), and the temp directory path.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// run() itself returns an error if any committed edge or label is
	// missing after recovery, so reaching here already proves the
	// invariant. We additionally assert the report lines so a regression
	// that silently drops the per-edge confirmation is caught.
	if !strings.Contains(out, fmt.Sprintf("Committed %d transactions.", len(commits))) {
		t.Errorf("missing committed-count line, got:\n%s", out)
	}
	for _, c := range commits {
		want := fmt.Sprintf("recovered %s -> %s with label %q", c[0], c[1], c[2])
		if !strings.Contains(out, want) {
			t.Errorf("output missing recovered edge %q, got:\n%s", want, out)
		}
	}

	// Exactly one recovery confirmation line per committed transaction:
	// no edge dropped, none duplicated.
	if got, want := strings.Count(out, "  recovered "), len(commits); got != want {
		t.Errorf("recovered-edge line count = %d, want %d:\n%s", got, want, out)
	}
}
