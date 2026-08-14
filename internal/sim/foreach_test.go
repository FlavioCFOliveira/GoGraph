package sim

// foreach_test.go — FOREACH write-path equivalence tests (rmp #2454): the
// scripted happy path for both FOREACH arms (empty list included, pinning the
// engine contract of cypher/foreach_test.go TestForeach_EmptyList), the
// sensitivity proofs that a perturbed oracle expansion is CAUGHT by the
// counters and parity checks, and the wiring proof for the terminal
// non-vacuity gate.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// runCountedOp executes op against the adapter exactly as the tick loop's
// executeCounted does — drain, read the counters from the SAME result, close —
// so the tests adjudicate the same effect report the scenario sees.
func runCountedOp(t *testing.T, a *EngineAdapter, op Op) (bool, *exec.QueryCounters) {
	t.Helper()
	res, err := a.RunWrite(context.Background(), op.Cypher, op.Params)
	if err != nil {
		return false, nil
	}
	for res.Next() {
	}
	drainErr := res.Err()
	var counters *exec.QueryCounters
	if cr, ok := res.(counterReporter); ok {
		counters = cr.Counters()
	}
	_ = res.Close()
	return drainErr == nil, counters
}

// mustCleanCounters asserts CheckOpCounters accepts the op's reported effect
// set against the pre-apply oracle.
func mustCleanCounters(t *testing.T, op Op, committed bool, got *exec.QueryCounters, oracle *GraphOracle) {
	t.Helper()
	if !committed {
		t.Fatalf("op %q did not commit", op.Cypher)
	}
	if v := CheckOpCounters(0, op, committed, got, oracle); len(v) > 0 {
		t.Fatalf("counters oracle rejected %q: %v (got %+v)", op.Cypher, v, got)
	}
}

// TestForeach_ScriptedHappyPath drives both FOREACH arms plus the empty-list
// no-op through the real engine and asserts, at each step, that the reported
// counters match the expansion exactly and that the read-back parity checks
// are clean against the oracle's expansion model.
func TestForeach_ScriptedHappyPath(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()

	// Arm (a): FOREACH…CREATE over a three-name list — the expansion of three
	// single CREATEs: 3 nodes, 3 labels, 3 name assignments.
	createOp := Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons,
		Params: map[string]any{"names": []any{"Ada", "Grace", "Edsger"}}}
	committed, counters := runCountedOp(t, a, createOp)
	mustCleanCounters(t, createOp, committed, counters, oracle)
	if counters == nil || counters.NodesCreated != 3 || counters.LabelsAdded != 3 || counters.PropertiesSet != 3 {
		t.Fatalf("FOREACH create counters = %+v, want {NodesCreated:3 LabelsAdded:3 PropertiesSet:3}", counters)
	}
	oracle.ApplyCreate(createOp.Cypher, createOp.Params)
	if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
		t.Fatalf("parity after FOREACH create: %v", v)
	}
	if n, err := a.NodeCount(); err != nil || n != 3 {
		t.Fatalf("engine NodeCount = %d (err %v), want 3", n, err)
	}

	// Arm (b): FOREACH…SET on an outer MATCH variable over a two-tag list —
	// two assignments applied, the LAST element stored.
	setOp := Op{Kind: OpUpdate, Cypher: tmplForeachSetTag,
		Params: map[string]any{"name": "Grace", "tags": []any{"t1", "t2"}}}
	committed, counters = runCountedOp(t, a, setOp)
	mustCleanCounters(t, setOp, committed, counters, oracle)
	if counters == nil || counters.PropertiesSet != 2 {
		t.Fatalf("FOREACH set counters = %+v, want {PropertiesSet:2}", counters)
	}
	oracle.ApplyMatch(setOp.Cypher, setOp.Params)
	// The oracle models tag = "t2" (last wins); the parity check reads the
	// engine's value back and compares, so a first-wins engine would fail here.
	if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
		t.Fatalf("parity after FOREACH set (last element must win): %v", v)
	}

	// A name miss: MATCH found nothing, the whole statement applies nothing.
	missOp := Op{Kind: OpUpdate, Cypher: tmplForeachSetTag,
		Params: map[string]any{"name": "Nobody", "tags": []any{"t9"}}}
	committed, counters = runCountedOp(t, a, missOp)
	mustCleanCounters(t, missOp, committed, counters, oracle)
	oracle.ApplyMatch(missOp.Cypher, missOp.Params)

	// Empty list, both arms: FOREACH over [] runs the body zero times — a
	// committed no-op with an all-zero effect set (the engine contract pinned
	// by cypher/foreach_test.go TestForeach_EmptyList).
	for _, op := range []Op{
		{Kind: OpCreate, Cypher: tmplForeachCreatePersons, Params: map[string]any{"names": []any{}}},
		{Kind: OpUpdate, Cypher: tmplForeachSetTag, Params: map[string]any{"name": "Ada", "tags": []any{}}},
	} {
		committed, counters = runCountedOp(t, a, op)
		mustCleanCounters(t, op, committed, counters, oracle)
		if counters == nil || (*counters != exec.QueryCounters{}) {
			t.Fatalf("empty-list %q counters = %+v, want non-nil all-zero", op.Cypher, counters)
		}
		if op.Kind == OpCreate {
			oracle.ApplyCreate(op.Cypher, op.Params)
		} else {
			oracle.ApplyMatch(op.Cypher, op.Params)
		}
	}
	if n, err := a.NodeCount(); err != nil || n != 3 {
		t.Fatalf("empty-list FOREACH must be a no-op: NodeCount = %d (err %v), want 3", n, err)
	}
	if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
		t.Fatalf("terminal parity: %v", v)
	}
}

// TestForeach_CountersSensitivity is the sensitivity proof for the counters
// oracle: when the oracle's expansion is perturbed to model one create fewer
// (a len-1 list) or one assignment fewer, CheckOpCounters must fire against
// the engine's true effect report.
func TestForeach_CountersSensitivity(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()

	// Engine runs the THREE-name create; the perturbed op claims a TWO-name
	// expansion, so the derived expectation is 2/2/2 against a 3/3/3 report.
	trueOp := Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons,
		Params: map[string]any{"names": []any{"Ada", "Grace", "Edsger"}}}
	committed, counters := runCountedOp(t, a, trueOp)
	if !committed || counters == nil {
		t.Fatalf("FOREACH create did not commit with counters (committed=%v counters=%v)", committed, counters)
	}
	perturbed := Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons,
		Params: map[string]any{"names": []any{"Ada", "Grace"}}}
	v := CheckOpCounters(0, perturbed, committed, counters, oracle)
	if len(v) == 0 {
		t.Fatal("counters oracle accepted a len-1-perturbed FOREACH create expansion")
	}
	if !strings.Contains(v[0].Message, "NodesCreated") {
		t.Fatalf("violation should name NodesCreated: %s", v[0].Message)
	}
	oracle.ApplyCreate(trueOp.Cypher, trueOp.Params)

	// Same perturbation on the SET arm: engine assigned twice, the perturbed
	// expansion models one assignment.
	trueSet := Op{Kind: OpUpdate, Cypher: tmplForeachSetTag,
		Params: map[string]any{"name": "Ada", "tags": []any{"t1", "t2"}}}
	committed, counters = runCountedOp(t, a, trueSet)
	if !committed || counters == nil {
		t.Fatalf("FOREACH set did not commit with counters (committed=%v counters=%v)", committed, counters)
	}
	perturbedSet := Op{Kind: OpUpdate, Cypher: tmplForeachSetTag,
		Params: map[string]any{"name": "Ada", "tags": []any{"t1"}}}
	if v := CheckOpCounters(0, perturbedSet, committed, counters, oracle); len(v) == 0 {
		t.Fatal("counters oracle accepted a len-1-perturbed FOREACH set expansion")
	}
}

// TestForeach_ParitySensitivity is the sensitivity proof for the read-back
// parity checks: an oracle expansion that models MORE creates than the engine
// ran trips CheckSchemaMutation (a modelled Person is absent), and one that
// models FEWER trips the invariant checker's node-count parity — so the
// structural equivalence oracle genuinely adjudicates per-item state, in both
// directions.
func TestForeach_ParitySensitivity(t *testing.T) {
	// Direction 1: engine ran a 2-name FOREACH, oracle models a 3-name expansion.
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()
	engineOp := Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons,
		Params: map[string]any{"names": []any{"Ada", "Grace"}}}
	if committed, _ := runCountedOp(t, a, engineOp); !committed {
		t.Fatal("engine FOREACH create did not commit")
	}
	oracle.ApplyCreate(tmplForeachCreatePersons,
		map[string]any{"names": []any{"Ada", "Grace", "Edsger"}})
	v := CheckSchemaMutation(0, oracle, a)
	if len(v) == 0 {
		t.Fatal("CheckSchemaMutation missed a FOREACH-created Person the engine never wrote")
	}
	if !strings.Contains(v[0].Message, "Edsger") {
		t.Fatalf("violation should name the phantom Person: %s", v[0].Message)
	}

	// Direction 2: engine ran a 3-name FOREACH, oracle models a 2-name
	// expansion — the extra engine node is invisible to the by-name probes, so
	// it is the node-count parity that must fire.
	eng2 := newTestEngine(t)
	a2 := NewEngineAdapter(eng2)
	oracle2 := NewGraphOracle()
	if committed, _ := runCountedOp(t, a2, Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons,
		Params: map[string]any{"names": []any{"Ada", "Grace", "Edsger"}}}); !committed {
		t.Fatal("engine FOREACH create did not commit")
	}
	oracle2.ApplyCreate(tmplForeachCreatePersons,
		map[string]any{"names": []any{"Ada", "Grace"}})
	if v := NewInvariantChecker(NewSeed(1)).Check(0, oracle2, a2); len(v) == 0 {
		t.Fatal("node-count parity missed an engine node the truncated expansion does not model")
	}
}

// TestForeach_NonVacuityGate verifies the assert-something-was-seen gate: an
// empty stats record fires every clause, and a fully-exercised record is
// clean.
func TestForeach_NonVacuityGate(t *testing.T) {
	if v := checkForeachNonVacuity(0, newForeachStats()); len(v) != 4 {
		t.Fatalf("empty stats must fire all four clauses, got %d: %v", len(v), v)
	}

	fs := newForeachStats()
	oracle := NewGraphOracle()
	oracle.ApplyCreate(tmplForeachCreatePersons, map[string]any{"names": []any{"Ada"}})
	fs.noteOp(Op{Kind: OpCreate, Cypher: tmplForeachCreatePersons,
		Params: map[string]any{"names": []any{"Ada"}}}, true)
	fs.noteOp(Op{Kind: OpUpdate, Cypher: tmplForeachSetTag,
		Params: map[string]any{"name": "Ada", "tags": []any{"t1"}}}, true)
	fs.noteRecovery(oracle)
	if v := checkForeachNonVacuity(0, fs); len(v) > 0 {
		t.Fatalf("fully-exercised stats must be clean: %v", v)
	}
}

// TestSchemaMutation_ForeachGateWired proves the terminal gate is actually
// wired into the run loop: a schema-mutation run whose crash schedule is
// disabled can never satisfy the crash-after-FOREACH clause, so it must end
// with a foreach non-vacuity report rather than a silent pass.
func TestSchemaMutation_ForeachGateWired(t *testing.T) {
	sc := schemaMutationScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.Crash = CrashConfig{}
	cfg.Checkpoint = CheckpointConfig{}
	cfg.MaxTicks = 120 // enough ticks to issue both FOREACH templates
	report, err := runSchemaMutationCfg(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runSchemaMutationCfg: %v", err)
	}
	if report == nil {
		t.Fatal("a crash-free run must fail the FOREACH non-vacuity gate; got a clean report")
	}
	found := false
	for _, v := range report.Violations {
		if v.Op == "foreach non-vacuity" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a foreach non-vacuity violation, got:\n%s", report)
	}
}
