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

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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
		// Double-entry totals, reconstructed by walking the recovered graph:
		// debit_sum aggregates outflows per source, credit_sum inflows per
		// destination. For a balanced ledger both equal the committed sum, but
		// they are now genuinely distinct aggregations (see reconcileNetPositions),
		// not one accumulator counted twice.
		{"ledger.debit_sum", committed},
		{"ledger.credit_sum", committed},
		// The per-account reconciliation (net position from the recovered graph
		// == the plan replay) must hold.
		{"ledger.accounts_reconciled", 1},
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

// TestReconcileNetPositionsHasTeeth proves the double-entry reconciliation is a
// genuine check, not a tautology: it reconciles a faithful ledger and rejects a
// wrong endpoint, a wrong amount, and a transfer count that exceeds the
// recovered edges. This guards against a regression back to the earlier code,
// where debit and credit were the same accumulator and the check could never
// fail.
func TestReconcileNetPositionsHasTeeth(t *testing.T) {
	accountIDs := []string{"a", "b", "c"}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, id := range accountIDs {
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
	}
	if err := g.AddEdge("a", "b", 10); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	if err := g.AddEdge("b", "c", 5); err != nil {
		t.Fatalf("AddEdge b->c: %v", err)
	}

	faithful := []transfer{{src: 0, dst: 1, amount: 10}, {src: 1, dst: 2, amount: 5}}
	if _, _, ok := reconcileNetPositions(g, accountIDs, faithful); !ok {
		t.Error("faithful ledger did not reconcile; want reconciled")
	}

	cases := []struct {
		name string
		want []transfer
	}{
		{"wrong endpoint", []transfer{{src: 0, dst: 2, amount: 10}, {src: 1, dst: 2, amount: 5}}},
		{"wrong amount", []transfer{{src: 0, dst: 1, amount: 11}, {src: 1, dst: 2, amount: 5}}},
		// The graph has fewer edges than the plan expects (a lost transfer).
		{"missing edge", []transfer{{src: 0, dst: 1, amount: 10}, {src: 1, dst: 2, amount: 5}, {src: 2, dst: 0, amount: 1}}},
		// The graph has more edges than the plan expects (a spurious transfer).
		{"spurious edge", []transfer{{src: 0, dst: 1, amount: 10}}},
	}
	for _, c := range cases {
		if _, _, ok := reconcileNetPositions(g, accountIDs, c.want); ok {
			t.Errorf("%s: reconciliation passed a corrupted ledger; want rejected", c.name)
		}
	}

	// Anomaly path: a recovered node whose key is not a known account must not
	// reconcile, even when the recognised account edges match the plan.
	gAnomaly := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, id := range accountIDs {
		if err := gAnomaly.AddNode(id); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
	}
	if err := gAnomaly.AddNode("zz-unknown"); err != nil {
		t.Fatalf("AddNode zz-unknown: %v", err)
	}
	if err := gAnomaly.AddEdge("a", "b", 10); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	if err := gAnomaly.AddEdge("zz-unknown", "a", 3); err != nil {
		t.Fatalf("AddEdge zz-unknown->a: %v", err)
	}
	if _, _, ok := reconcileNetPositions(gAnomaly, accountIDs, []transfer{{src: 0, dst: 1, amount: 10}}); ok {
		t.Error("reconciliation passed a recovered node with an unknown account id; want rejected")
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
