package main

// example_test.go — assertion-based regression test for the durable-ledger
// example (#1611). The example runs a background checkpoint goroutine whose
// cadence depends on timing, prints stats (checkpoint count, WAL bytes folded,
// snapshot bytes, recovery wall-clock) that vary per run, and writes to an
// os.MkdirTemp directory whose path changes every run, so a // Output: block
// would be flaky. Instead the test asserts the DETERMINISTIC ACID invariants —
// the recovered account/transfer counts, the bit-exact recovered-amount sum,
// and the conservation identity — and never asserts on timing, checkpoint
// stats, the WAL-op count, or the temp path.
//
// TestMain runs the suite under go.uber.org/goleak so the background
// checkpointer is verified to shut down with no leaked goroutine. Run it under
// the race detector to confirm the checkpointer/commit coordination is free of
// data races:
//
//	go test -race ./examples/17_transactional_log/...

import (
	"bytes"
	"context"
	"strconv"
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
// that ACID durability guarantees: the recovered ledger reproduces the
// committed account and transfer counts, the recovered transfer amounts sum
// bit-exactly to the committed total, and the double-entry conservation
// identity holds. run() itself fails if any per-transfer EdgeWeight does not
// match the committed amount, so reaching here already proves the bit-exact
// recovery; the assertions below additionally pin the reported facts. The
// volatile telemetry lines (prefixed "# ") and the temp path are ignored.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := defaultConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Account count is exact and independent of the RNG.
	if got := facts["nodes.accounts"]; got != int64(cfg.accounts) {
		t.Errorf("nodes.accounts = %d, want %d", got, cfg.accounts)
	}
	// Exactly cfg.transfers distinct transfers are committed.
	if got := facts["edges.transfers"]; got != int64(cfg.transfers) {
		t.Errorf("edges.transfers = %d, want %d", got, cfg.transfers)
	}

	// The committed amount sum is positive (every amount is >= minAmount > 0)
	// and bounded by transfers*maxAmount.
	committed := facts["ledger.amount_sum"]
	if lo, hi := int64(cfg.transfers)*cfg.minAmount, int64(cfg.transfers)*cfg.maxAmount; committed < lo || committed > hi {
		t.Errorf("ledger.amount_sum = %d, want within [%d,%d]", committed, lo, hi)
	}

	// Recovery must reproduce the committed shape exactly: same accounts, same
	// transfers, and the same total amount — the bit-exact durable-weight
	// guarantee. run() verifies each EdgeWeight individually; these pin the
	// aggregate facts.
	recovered := []struct {
		col  string
		want int64
	}{
		{"recovered.accounts", int64(cfg.accounts)},
		{"recovered.transfers", int64(cfg.transfers)},
		{"recovered.amount_sum", committed},
		// Double-entry conservation: every transfer debits its source and
		// credits its destination by the same amount, so both totals equal the
		// committed sum.
		{"ledger.debit_sum", committed},
		{"ledger.credit_sum", committed},
	}
	for _, c := range recovered {
		if got := facts[c.col]; got != c.want {
			t.Errorf("%s = %d, want %d", c.col, got, c.want)
		}
	}

	// The conservation invariant must report true.
	if !strings.Contains(out, "ledger.conserved=true") {
		t.Errorf("missing ledger.conserved=true, got:\n%s", out)
	}
}

// TestDeterministic confirms the ledger shape is reproducible: two runs with
// the same config produce identical deterministic fact lines. The "# "
// telemetry (timing, checkpoint cadence, on-disk bytes, the temp path) is
// dropped before comparison, as the standard requires.
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, defaultConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, defaultConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s",
			factLines(a.String()), factLines(b.String()))
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for more
// distinct transfers than there are ordered account pairs is rejected before
// any work (and before any temp directory or WAL is created).
func TestRunRejectsBadConfig(t *testing.T) {
	// 5 accounts admit at most 5*4 = 20 distinct ordered pairs; 21 is impossible.
	bad := config{accounts: 5, transfers: 21, minAmount: 1, maxAmount: 10, seed: 1, checkpointEvery: 5_000_000}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted transfers > accounts*(accounts-1); want error")
	}
}

// TestRunHonoursCancellation confirms the run aborts promptly when the context
// is already cancelled, returning the context error rather than committing the
// ledger.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, &bytes.Buffer{}, defaultConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer (e.g. the config range line or the
// boolean conservation line) are skipped.
func parseFacts(t *testing.T, out string) map[string]int64 {
	t.Helper()
	facts := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed fact line: %q", line)
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			facts[k] = n
		}
	}
	return facts
}

// factLines returns only the deterministic lines of out (dropping the volatile
// "# " telemetry), joined back into a single string for equality comparison.
func factLines(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
