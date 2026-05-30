package main

// example_test.go — assertion-based regression test for the Bolt server
// example (#1180). The example drives a network round-trip, so its stdout is
// not byte-stable (the OS-assigned listener address varies per run). The test
// therefore drives run into a bytes.Buffer and asserts the deterministic
// query result — the ordered :Person names — rather than any timing- or
// address-dependent output. TestMain wraps the suite in go.uber.org/goleak so
// the example doubles as a goroutine-leak check; run it under -race.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under go.uber.org/goleak so the
// example doubles as a goroutine-leak check: the server's per-connection
// goroutines and the serve goroutine must all be drained by run's teardown.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun drives the example end to end and asserts the deterministic query
// result. It does not assert the listener address line (the port is
// OS-assigned) nor any timing.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// The client round-trip returns exactly the seeded names, in order.
	if got, want := strings.Count(out, "name = "), len(people); got != want {
		t.Errorf("expected %d name lines, got %d:\n%s", want, got, out)
	}
	if got := fmt.Sprintf("Returned %d rows:", len(people)); !strings.Contains(out, got) {
		t.Errorf("expected row-count line %q, got:\n%s", got, out)
	}
	for _, name := range people {
		want := "name = " + name
		if !strings.Contains(out, want) {
			t.Errorf("expected returned name line %q, got:\n%s", want, out)
		}
	}

	// The names must appear in sorted order, which is what ORDER BY guarantees.
	lastIdx := -1
	for _, name := range people {
		idx := strings.Index(out, "name = "+name)
		if idx <= lastIdx {
			t.Errorf("name %q out of order in output:\n%s", name, out)
		}
		lastIdx = idx
	}

	// Lifecycle markers: the server announced itself and shut down cleanly.
	if !strings.Contains(out, "GoGraph Bolt v5 server listening on") {
		t.Errorf("expected listening banner, got:\n%s", out)
	}
	if !strings.Contains(out, "Server shut down cleanly.") {
		t.Errorf("expected clean-shutdown line, got:\n%s", out)
	}
}
