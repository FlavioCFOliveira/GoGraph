package main

// example_test.go — assertion-based regression test for the concurrent
// reads example (#1174). The example spawns goroutines, so completion
// order is non-deterministic and a // Output: block would be flaky.
// Instead the test asserts the deterministic aggregates the readers
// compute over the shared immutable CSR, and TestMain runs the suite
// under go.uber.org/goleak so the example doubles as a goroutine-leak
// check. Run it under the race detector:
//
//	go test -race ./examples/20_concurrent_reads/...

import (
	"bytes"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under go.uber.org/goleak so
// the concurrent example doubles as a goroutine-leak check.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun drives run into a buffer and asserts the deterministic
// aggregates regardless of goroutine timing: the summed Dijkstra cost,
// the BFS reach count, and the PageRank live-rank count. It never
// asserts on per-goroutine ordering or timing — only the stable results.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	agg, err := run(&buf)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Deterministic aggregates, independent of goroutine scheduling.
	if want := int64(110); agg.dijkstraCost != want {
		t.Errorf("dijkstra summed cost = %d, want %d", agg.dijkstraCost, want)
	}
	if want := 100; agg.bfsReached != want {
		t.Errorf("BFS reached %d nodes, want %d", agg.bfsReached, want)
	}
	if want := 100; agg.pagerankLive != want {
		t.Errorf("PageRank live ranks = %d, want %d", agg.pagerankLive, want)
	}

	// The report is written in a fixed key order, so the structural
	// lines are stable even though the goroutines finish in any order.
	out := buf.String()
	for _, want := range []string{
		"Concurrent results over a single immutable CSR:",
		"dijkstra  8 SSSPs, summed cost = 110",
		"bfs       BFS reached 100 nodes",
		"100 live ranks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, got:\n%s", want, out)
		}
	}
}
