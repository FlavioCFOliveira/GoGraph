package sim

// mvcc_isolation_test.go — validation of the rmp #2436 isolation checkers.
// Two halves, per the task's acceptance criteria:
//
//   - INJECTION: each checker must FIRE when fed a deliberately broken
//     adjudication or a simulated violation — an oracle that cannot fail
//     proves nothing. The injections rig the harness's own expectation state
//     against a real store, so every probe exercises the same query path the
//     workload drives.
//   - GREEN GATE: RunMVCCSessions is clean across 20 seeds on HEAD, and the
//     probes were actually exercised (non-vacuous counters), including the
//     interleaved-commit stability probes the task's "even when other sessions
//     commit in between" clause demands.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// injectionHarness builds a minimal live mvccHarness over a real store with
// one write session, for driving the probe methods directly.
func injectionHarness(t *testing.T) (*mvccHarness, *mvccSessionState) {
	t.Helper()
	store, err := OpenSimStore(NewSimDisk(NewSeed(7), 0), simulatorStoreConfig())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := &mvccHarness{
		store:   store,
		oracle:  NewGraphOracle(),
		checker: NewInvariantChecker(NewSeed(7 ^ checkerSeedMix)),
		adapter: NewEngineAdapter(store.Engine()),
		res:     &MVCCSessionsResult{},
		tick:    1,
	}
	s := &mvccSessionState{id: 0, sess: store.Engine().NewSession(), rng: NewSeed(1)}
	return h, s
}

// TestMVCCIsolation_StabilityCheckerFires validates checker (a) by broken
// adjudication: a read-only transaction whose BEGIN expectation deliberately
// disagrees with the engine (the oracle was never told about a committed
// node) must produce a snapshot-stability violation.
func TestMVCCIsolation_StabilityCheckerFires(t *testing.T) {
	defer goleak.VerifyNone(t)
	h, s := injectionHarness(t)
	mustExecCommit(t, s.sess, "CREATE (n:Person {name:'x', age:1})", nil)

	tx, err := s.sess.BeginReadTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s.tx, s.readOnly = tx, true
	s.expectNodes = 0 // broken: the engine committed 1 Person the oracle never saw

	h.adjudicateCountRead(s, mvccReadTemplates[0], 1, true)
	if len(h.res.Violations) == 0 {
		t.Fatal("snapshot-stability checker did not fire on a broken adjudication")
	}
	if v := h.res.Violations[0]; v.Op != "snapshot stability" || v.Kind != ViolationACIDIsolation {
		t.Fatalf("wrong violation: %+v", v)
	}
	if h.res.StabilityProbes != 1 {
		t.Fatalf("StabilityProbes=%d, want 1", h.res.StabilityProbes)
	}
}

// TestMVCCIsolation_RYOWCheckerFires validates checker (b) by simulated
// violation: the workspace claims the transaction created a node the engine
// never saw (a lost write). The probe must first record a doom suspect —
// the rmp #2354 contract allows a refused-void write to diverge — and a clean
// COMMIT of the suspect transaction must then convert it into a violation.
func TestMVCCIsolation_RYOWCheckerFires(t *testing.T) {
	defer goleak.VerifyNone(t)
	h, s := injectionHarness(t)

	tx, err := s.sess.BeginTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.tx = tx
	s.otx = h.oracle.BeginTx()
	// Mirror a create into the workspace WITHOUT executing it on the engine —
	// the simulated silently-lost write.
	s.otx.ApplyCreate(tmplCreatePerson, map[string]any{"name": "ghost", "age": int64(1)})

	if err := h.probeRYOW(s, OpCreate, tmplCreatePerson, map[string]any{"name": "ghost", "age": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if s.doomSuspect == "" {
		t.Fatal("read-your-own-writes probe did not record the divergence as a doom suspect")
	}
	if h.res.RYOWProbes == 0 {
		t.Fatal("RYOWProbes not counted")
	}
	// The suspect transaction COMMITTING cleanly is the violation.
	if err := h.finish(s); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range h.res.Violations {
		if v.Op == "read-your-own-writes" && strings.Contains(v.Message, "COMMITTED cleanly") {
			found = true
		}
	}
	if !found {
		t.Fatalf("clean COMMIT of a doom-suspect tx did not fire the RYOW violation: %v", h.res.Violations)
	}
}

// TestMVCCIsolation_CrossTxRYOWCheckerFires validates the session half of
// checker (b): the oracle claims the session committed a name in an earlier
// transaction, the engine holds no such node, and the probe at the next BEGIN
// must fire.
func TestMVCCIsolation_CrossTxRYOWCheckerFires(t *testing.T) {
	defer goleak.VerifyNone(t)
	h, s := injectionHarness(t)
	// Broken adjudication: the committed model holds a name the engine lacks.
	h.oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "ghost", "age": int64(1)})
	s.lastCommitted = "ghost"

	tx, err := s.sess.BeginTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s.tx = tx
	s.otx = h.oracle.BeginTx()

	if err := h.probeCrossTxRYOW(s); err != nil {
		t.Fatal(err)
	}
	if len(h.res.Violations) == 0 || h.res.Violations[0].Op != "cross-tx read-your-own-writes" {
		t.Fatalf("cross-tx RYOW checker did not fire: %v", h.res.Violations)
	}
	if h.res.RYOWCrossTx != 1 {
		t.Fatalf("RYOWCrossTx=%d, want 1", h.res.RYOWCrossTx)
	}
}

// TestMVCCIsolation_PairCheckerFires validates checker (c) end to end with a
// SIMULATED subset visibility: exactly one member of a registered committed
// pair exists on the engine, so every probe and the present-state sweep must
// report the strict subset.
func TestMVCCIsolation_PairCheckerFires(t *testing.T) {
	defer goleak.VerifyNone(t)
	h, s := injectionHarness(t)
	// Half a pair on the engine; the registry claims the pair committed whole.
	mustExecCommit(t, s.sess, "CREATE (n:Person {name:'mv-s0-pa0', age:1})", nil)
	h.pairs = append(h.pairs, mvccPair{a: "mv-s0-pa0", b: "mv-s0-pb0", foldSeq: 0})

	tx, err := s.sess.BeginReadTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s.tx, s.readOnly = tx, true
	s.beginFoldSeq = 0 // the pair folded at-or-before this tx's BEGIN: expect 2

	if err := h.probePairs(s); err != nil {
		t.Fatal(err)
	}
	if len(h.res.Violations) == 0 || h.res.Violations[0].Op != "atomic visibility" {
		t.Fatalf("pair probe did not fire on a strict subset: %v", h.res.Violations)
	}
	if !strings.Contains(h.res.Violations[0].Message, "STRICT SUBSET") {
		t.Fatalf("subset not named: %s", h.res.Violations[0].Message)
	}
	if h.res.PairProbes != 1 {
		t.Fatalf("PairProbes=%d, want 1", h.res.PairProbes)
	}

	// The present-state sweep must agree.
	if v := h.checkCommittedPairs(1); len(v) == 0 || !strings.Contains(v[0].Message, "STRICT SUBSET") {
		t.Fatalf("present-state pair sweep did not fire: %v", v)
	}
}

// TestMVCCSessions_IsolationGreen20Seeds is the rmp #2436 green gate: the
// multi-session mode with all isolation checkers active is clean across 20
// seeds on HEAD, and each checker was genuinely exercised — including
// stability probes with commits folded between BEGIN and the read, doomed
// transactions observed surfacing as conflicts, and committed pairs probed.
func TestMVCCSessions_IsolationGreen20Seeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	var agg MVCCSessionsResult
	for seed := uint64(1); seed <= 20; seed++ {
		res, err := RunMVCCSessions(ctx, mvccTestConfig(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !res.Clean() {
			t.Fatalf("seed %d: violations=%v foldErrors=%v", seed, res.Violations, res.FoldErrors)
		}
		if res.StabilityProbes == 0 || res.RYOWProbes == 0 {
			t.Fatalf("seed %d: vacuous run: %+v", seed, res)
		}
		agg.StabilityProbes += res.StabilityProbes
		agg.StabilityInterleaved += res.StabilityInterleaved
		agg.RYOWProbes += res.RYOWProbes
		agg.RYOWCrossTx += res.RYOWCrossTx
		agg.PairsCommitted += res.PairsCommitted
		agg.PairProbes += res.PairProbes
		agg.TxDoomed += res.TxDoomed
	}
	if agg.StabilityInterleaved == 0 {
		t.Fatal("no stability probe ever saw a commit folded mid-transaction — the interesting case never ran")
	}
	if agg.RYOWCrossTx == 0 || agg.PairsCommitted == 0 || agg.PairProbes == 0 {
		t.Fatalf("checker never exercised: %+v", agg)
	}
	if agg.TxDoomed == 0 {
		t.Fatal("no doomed transaction observed surfacing as a conflict across 20 seeds — the rmp #2354 contract path never ran")
	}
	t.Logf("aggregate: stab=%d/%d ryow=%d cross=%d pairs=%d probes=%d doomed=%d",
		agg.StabilityProbes, agg.StabilityInterleaved, agg.RYOWProbes,
		agg.RYOWCrossTx, agg.PairsCommitted, agg.PairProbes, agg.TxDoomed)
}
