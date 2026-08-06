package main

// example_test.go — the regression gate for example 37 (rmp #2313).
//
// Layer: short.
//
// # What it asserts, and what it deliberately does not
//
// It asserts only the DETERMINISTIC lines — the bare ones, which carry invariant
// verdicts and counts that must hold on any machine. The "# " lines carry
// throughputs, latencies and heap figures that vary per run; asserting on those
// would make this fail on a loaded machine for a reason unrelated to MVCC, and
// would teach the next reader to ignore a red test.
//
// The workload is shrunk so the gate fits the short layer. That is a size change,
// not a shape change: every phase still runs, with concurrent writers, readers
// beside them, contention on the shared set, and a real restart.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// testConfig is the short-layer workload: the same shape, smaller.
func testConfig() config {
	c := defaultConfig()
	c.customers = 120
	c.inventory = 4
	c.opsPerProd = 25
	c.producers = 4
	c.readers = 2
	return c
}

// runExample runs every phase and returns the report.
func runExample(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, &cfg); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	return buf.String()
}

// mustLine fails unless out contains want exactly, on its own line.
func mustLine(t *testing.T, out, want string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return
		}
	}
	t.Errorf("output is missing the line %q\n--- output ---\n%s", want, out)
}

// TestRun drives the whole example and holds it to every deterministic verdict.
func TestRun(t *testing.T) {
	out := runExample(t)

	// Phase 1 — every scaling level ran.
	mustLine(t, out, "scaling.levels=5")

	// Phase 2 — no order vanished, and the readers actually sampled. The second is
	// the non-degeneracy guard: a reader that never ran would leave the latency
	// percentiles at zero and report nothing, while the phase still "passed".
	mustLine(t, out, "contention.accounted=true")
	mustLine(t, out, "contention.readers_sampled=true")

	// Phase 3 — THE headline. A transfer debits one node and credits another in one
	// transaction, so the total cannot change whatever instant observes it.
	mustLine(t, out, "conservation.torn_observations=0")
	mustLine(t, out, "conservation.final_total_correct=true")
	// Both non-degeneracy guards. Without them the invariant passes vacuously on a
	// run where nothing was observed or nothing was committed — which is exactly how
	// a conservation check stops being a check.
	mustLine(t, out, "conservation.observed_any=true")
	mustLine(t, out, "conservation.committed_any=true")

	// Phase 4 — the data AND the clock survived.
	mustLine(t, out, "restart.all_nodes_recovered=true")
	mustLine(t, out, "restart.clock_not_rewound=true")
	mustLine(t, out, "restart.post_restart_instant_is_new=true")
}

// TestConservationCheckCanFail validates the INSTRUMENT rather than the engine.
//
// The conservation invariant is this example's self-contradictory check, and the
// project's standing rule is that an instrument which cannot fail on a defective
// build proves nothing — three of GoGraph's own instruments have been caught
// reporting a number they could only ever have produced.
//
// So a defect is injected into a REAL graph — a debit with no matching credit, the
// shape of both a lost update and a half-applied transaction — and the real observer
// is run against it. Asserting arithmetic instead would be a tautology that passes
// however broken observeTotal became.
//
// This is not hypothetical. The example's FIRST draft read balances with the
// present-time GetNodeProperty rather than through the transaction's own view — a
// textbook lost update — and this invariant reported 97 torn observations and a
// final total of 15 999 019 against 16 000 000. It caught the example's own bug
// before it could be mistaken for the engine's.
func TestConservationCheckCanFail(t *testing.T) {
	g := newGraph()
	defer func() { _ = g.Close() }()
	if err := seedAccounts(g); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := int64(transferAccounts) * startingBalance

	// POSITIVE arm: an untouched fixture must observe exactly the invariant total.
	// Without it, an observer that always returned a wrong number would "detect" the
	// injected defect below and look correct.
	if got := observeTotal(g); got != want {
		t.Fatalf("observeTotal on an untouched fixture = %d, want %d: the observer is "+
			"wrong before any defect was injected", got, want)
	}

	// NEGATIVE arm: debit one account and credit nobody.
	const stolen = 981
	if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
		v, _ := g.WriterViewOf(tx).GetNodeProperty(acct(0), "balance")
		n, _ := v.Int64()
		return g.Writer(tx).SetNodeProperty(acct(0), "balance", lpg.Int64Value(n-stolen))
	}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	got := observeTotal(g)
	if got == want {
		t.Fatalf("observeTotal still reports the invariant total %d after %d was debited "+
			"with no matching credit: the check cannot detect the defect it exists for, "+
			"so TestRun's green means nothing", got, stolen)
	}
	if got != want-stolen {
		t.Errorf("observeTotal = %d, want %d: the observer detected a change but not the "+
			"right one", got, want-stolen)
	}
}
