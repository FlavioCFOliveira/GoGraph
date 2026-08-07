package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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
	cfg := defaultConfig()
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, &cfg); err != nil {
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
	if err := run(context.Background(), &buf, &cfg); err != nil {
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

// TestTornGate_CatchesADeliberateTear is the torn-total gate's NEGATIVE CONTROL
// (rmp #2333).
//
// TestRun and TestRunReproducibleAcrossReaderScaling only ever assert that the gate
// stays quiet, and a gate that has never been shown to fire is not evidence that
// anything passed. rmp #2333 is exactly that bill coming due: one unattributable
// torn value, and a reproduction search of roughly 7.3 million clean observations
// that could not even settle whether the engine or the instrument was at fault.
//
// So this run is deliberately broken. config.faultSplitMultiStatement commits a
// multi-statement transfer's debit and credit as two independent transactions, which
// leaves the ledger genuinely short by the transfer amount for the duration of a
// commit and an fsync. That is a REAL torn total, not a simulated one, and the gate
// must both catch it and attribute it correctly.
func TestTornGate_CatchesADeliberateTear(t *testing.T) {
	cfg := defaultConfig()
	cfg.faultSplitMultiStatement = true
	// Wide and shallow: enough accounts and few enough writers that write-write
	// conflicts stay rare, so the run reaches the torn-total check rather than
	// failing earlier by exhausting a retry chain.
	cfg.accounts = 64
	cfg.writers = 2
	cfg.readers = 6
	cfg.opsPerWriter = 40
	cfg.sweepOps = 0

	var buf bytes.Buffer
	err := run(context.Background(), &buf, &cfg)
	if err == nil {
		t.Fatalf("deliberately torn run reported success; the gate cannot detect a real tear.\noutput:\n%s", buf.String())
	}
	msg := err.Error()
	if !strings.Contains(msg, "ISOLATION VIOLATION") {
		t.Fatalf("deliberately torn run failed for the wrong reason: %v", err)
	}
	// The diagnosis must be attached, not merely a bare number.
	if !strings.Contains(msg, "verdict=") {
		t.Errorf("torn report carries no verdict; the gate detected the tear but cannot attribute it: %v", err)
	}
	// A read-transaction reader observing this tear re-reads at the same instant,
	// and that per-account state IS inconsistent — the debit landed, the credit has
	// not. The verdict must therefore name isolation and must NOT blame the
	// aggregate. (Only Engine.Run readers may report UNATTRIBUTED; if one of those
	// happened to be the sole observer, the run still failed correctly.)
	if strings.Contains(msg, "AGGREGATE DEFECT") {
		t.Errorf("a genuine isolation tear was misattributed to the aggregate: %v", err)
	}
	if strings.Contains(msg, "INCONSISTENT EVIDENCE") {
		t.Errorf("the same-instant diagnosis contradicted itself on a known-real tear: %v", err)
	}
	t.Logf("negative control fired as intended: %v", err)
}

// TestAsInt64_RejectsNull pins that a NULL is an ERROR rather than a zero
// (rmp #2333).
//
// Mapping NULL to 0 turned any defect that produced a NULL sum into a torn-total
// report of "0" — a fabricated isolation violation — and turned a NULL balance into
// a zero balance reported as a lost update. Both blamed the engine for something it
// had not done.
func TestAsInt64_RejectsNull(t *testing.T) {
	for _, v := range []any{nil, any(expr.Null)} {
		if _, err := asInt64(v); err == nil {
			t.Errorf("asInt64(%#v) = nil error, want a rejection: a NULL must not read as 0", v)
		}
	}
	if got, err := asInt64(expr.IntegerValue(42)); err != nil || got != 42 {
		t.Errorf("asInt64(42) = (%d, %v), want (42, nil)", got, err)
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
