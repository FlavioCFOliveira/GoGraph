package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test under a goroutine-leak check: this example spawns
// writer and reader goroutines and opens WAL-backed engines, so a leaked
// goroutine (or an unreleased WAL) after run returns is a defect the check
// catches. Combined with `go test -race`, it certifies the concurrent phase is
// both leak-free and data-race-free.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun pins the deterministic facts of the default configuration. Only bare
// "fact" lines are asserted; every "# " telemetry line (throughput, contention,
// heap, the scaling sweep) is ignored, because it varies per run and machine.
//
// The invariants pinned here are the point of the example: the conserved total
// held under concurrency (total_balance_invariant_holds=1), no reader observed a
// partial transaction, no update was lost (lost_updates=0), and the ledger
// conserved money end to end (conservation.holds=1). Run under -race.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}

	facts := parseFacts(t, buf.String())

	want := map[string]string{
		"config.accounts":               "32",
		"config.writers":                "4",
		"config.readers":                "4",
		"config.ops_per_writer":         "150",
		"config.seed":                   "1",
		"accounts":                      "32",
		"transfers.planned":             "600",
		"transfers.multi_statement":     "300",
		"transfers.single_statement":    "300",
		"initial_total":                 "46625986168",
		"transfers.committed":           "600",
		"final_total":                   "46625986168",
		"conservation.holds":            "1",
		"lost_updates":                  "0",
		"no_negative_balances":          "1",
		"total_balance_invariant_holds": "1",
	}
	for k, v := range want {
		got, ok := facts[k]
		if !ok {
			t.Errorf("missing fact %q", k)
			continue
		}
		if got != v {
			t.Errorf("fact %q = %q, want %q", k, got, v)
		}
	}
}

// TestRunReproducibleAcrossReaderScaling verifies the deterministic facts do not
// depend on the concurrency shape: changing the reader and writer counts (which
// only changes scheduling, not the committed set) must leave every conservation
// and no-lost-update invariant intact and the totals unchanged.
func TestRunReproducibleAcrossReaderScaling(t *testing.T) {
	cfg := defaultConfig()
	cfg.writers = 3
	cfg.readers = 7
	cfg.opsPerWriter = 200 // 600 total transfers, same as the default plan size
	cfg.sweepOps = 0       // skip the sweep; this test targets the main invariants

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	// The committed set is identical (same accounts, same amounts, same seed),
	// so the totals match the default run exactly regardless of the worker split.
	for _, k := range []string{"initial_total", "final_total"} {
		if facts[k] != "46625986168" {
			t.Errorf("fact %q = %q, want %q", k, facts[k], "46625986168")
		}
	}
	for _, k := range []string{"conservation.holds", "no_negative_balances", "total_balance_invariant_holds"} {
		if facts[k] != "1" {
			t.Errorf("invariant %q = %q, want 1", k, facts[k])
		}
	}
	if facts["lost_updates"] != "0" {
		t.Errorf("lost_updates = %q, want 0", facts["lost_updates"])
	}
	if facts["transfers.committed"] != "600" {
		t.Errorf("transfers.committed = %q, want 600", facts["transfers.committed"])
	}
}

// parseFacts splits run's output into deterministic facts, collecting every bare
// "key=value" line and skipping the "# " telemetry lines. It fails the test on a
// duplicate fact key, which would signal an ambiguous report.
func parseFacts(t *testing.T, out string) map[string]string {
	t.Helper()
	facts := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed output line: %q", line)
		}
		if _, dup := facts[k]; dup {
			t.Fatalf("duplicate fact key %q", k)
		}
		facts[k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}
	return facts
}
