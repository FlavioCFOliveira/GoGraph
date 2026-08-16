package sim

// merge_surface_test.go — MERGE surface completeness tests (rmp #2461): the
// scripted happy path for all four families with hand-computed effect sets, the
// two engine-semantics pins that the workload deliberately does not drive
// (a whole-map ON CREATE that destroys the merge key, and whole-pattern MERGE
// duplicating an existing endpoint), the sensitivity proofs that INVERTING the
// oracle's created-vs-matched adjudication is CAUGHT by the counters and parity
// checks, and the wiring proof for the terminal non-vacuity gate.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// wantCounters asserts the engine reported exactly want, so every number in the
// happy path is hand-computed rather than read back from whatever the engine
// produced.
func wantCounters(t *testing.T, label string, got, want *exec.QueryCounters) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: counters are nil, want %+v", label, *want)
	}
	if *got != *want {
		t.Fatalf("%s: counters = %+v, want %+v", label, *got, *want)
	}
}

// mergeOp is the op-building shorthand the tests share.
func mergeOp(cypher string, params map[string]any) Op {
	return Op{Kind: OpMerge, Cypher: cypher, Params: params}
}

// TestMergeSurface_ScriptedHappyPath drives all four MERGE families through the
// real engine and asserts, at each step, that the reported effect set equals the
// hand-computed expectation, that the counters oracle accepts it, and that the
// read-back parity checks are clean against the oracle model.
func TestMergeSurface_ScriptedHappyPath(t *testing.T) {
	a := NewEngineAdapter(newTestEngine(t))
	oracle := NewGraphOracle()
	checker := NewInvariantChecker(NewSeed(1))

	parity := func(step string) {
		t.Helper()
		if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
			t.Fatalf("parity after %s: %v", step, v)
		}
		if v := checker.Check(0, oracle, a); len(v) > 0 {
			t.Fatalf("count parity after %s: %v", step, v)
		}
	}

	// Family (a), ON CREATE branch: the pattern's name plus the ON CREATE mc are
	// two assignments on one new, one-label node.
	opA := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
	committed, counters := runCountedOp(t, a, opA)
	mustCleanCounters(t, opA, committed, counters, oracle)
	wantCounters(t, "counter MERGE / ON CREATE", counters,
		&exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2})
	oracle.ApplyMerge(opA.Cypher, opA.Params)
	parity("counter MERGE create")

	// Family (a), ON MATCH branch: exactly one assignment, and the counter
	// increments. The parity probe reads mc back, so a non-incrementing engine
	// fails here.
	for i, wantMC := range []int64{2, 3} {
		committed, counters = runCountedOp(t, a, opA)
		mustCleanCounters(t, opA, committed, counters, oracle)
		wantCounters(t, "counter MERGE / ON MATCH", counters, &exec.QueryCounters{PropertiesSet: 1})
		oracle.ApplyMerge(opA.Cypher, opA.Params)
		parity("counter MERGE match")
		if got := oracle.nodes[oracle.byName["Ada"]].Properties["mc"]; got != wantMC {
			t.Fatalf("match %d: modelled mc = %v, want %d", i+1, got, wantMC)
		}
	}

	// Family (a) over a Person with NO mc: n.mc + 1 is null, and assigning null
	// removes an absent property — a committed all-zero no-op.
	createOp := Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": "Alan", "age": int64(41)}}
	if committed, _ := runCountedOp(t, a, createOp); !committed {
		t.Fatal("seed CREATE did not commit")
	}
	oracle.ApplyCreate(createOp.Cypher, createOp.Params)
	opAless := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Alan"})
	committed, counters = runCountedOp(t, a, opAless)
	mustCleanCounters(t, opAless, committed, counters, oracle)
	wantCounters(t, "counter MERGE / mc-less match", counters, &exec.QueryCounters{})
	oracle.ApplyMerge(opAless.Cypher, opAless.Params)
	parity("counter MERGE on an mc-less Person")

	// Family (b), ON CREATE branch: the pattern writes name (+1 property), the
	// whole-entity replace then CLEARS it (-1 property) and writes the map's two
	// entries (+2) — three set, one removed.
	opB := mergeOp(tmplMergePersonSetAll, map[string]any{
		"name": "Grace", "map": map[string]any{"name": "Grace", "age": int64(45)}})
	committed, counters = runCountedOp(t, a, opB)
	mustCleanCounters(t, opB, committed, counters, oracle)
	wantCounters(t, "set-all MERGE / ON CREATE", counters,
		&exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 3, PropertiesRemoved: 1})
	oracle.ApplyMerge(opB.Cypher, opB.Params)
	parity("set-all MERGE create")

	// Family (b) on a match: ON CREATE does not fire, so nothing is applied even
	// though the map differs from the stored properties.
	opBMatch := mergeOp(tmplMergePersonSetAll, map[string]any{
		"name": "Grace", "map": map[string]any{"name": "Grace", "age": int64(99)}})
	committed, counters = runCountedOp(t, a, opBMatch)
	mustCleanCounters(t, opBMatch, committed, counters, oracle)
	wantCounters(t, "set-all MERGE / match", counters, &exec.QueryCounters{})
	oracle.ApplyMerge(opBMatch.Cypher, opBMatch.Params)
	parity("set-all MERGE match")
	if got := oracle.nodes[oracle.byName["Grace"]].Properties["age"]; got != int64(45) {
		t.Fatalf("a matched set-all MERGE must not re-apply the map: age = %v, want 45", got)
	}

	// Family (c), all four sub-cases. Every creating case reports the SAME set —
	// the whole pattern is created as a unit whatever already exists.
	create := exec.QueryCounters{NodesCreated: 2, RelationshipsCreated: 1, PropertiesSet: 2, LabelsAdded: 2}
	for _, tc := range []struct {
		step string
		a, b string
		want exec.QueryCounters
		kase mergePatternCase
	}{
		{"(i) neither endpoint present", "wp0", "wp1", create, mergePatternNeither},
		{"(iv) whole pattern present", "wp0", "wp1", exec.QueryCounters{}, mergePatternWhole},
		{"(ii) one endpoint present", "wp0", "wp2", create, mergePatternOneEndpoint},
		{"(iii) both endpoints present", "wp1", "wp0", create, mergePatternBothEndpoints},
	} {
		op := mergeOp(tmplMergePairPattern, map[string]any{"a": tc.a, "b": tc.b})
		if got, ok := classifyMergePattern(op, oracle); !ok || got != tc.kase {
			t.Fatalf("%s: classified as %v (ok=%v), want %v", tc.step, got, ok, tc.kase)
		}
		committed, counters = runCountedOp(t, a, op)
		mustCleanCounters(t, op, committed, counters, oracle)
		wantCounters(t, "whole-pattern MERGE "+tc.step, counters, &tc.want)
		oracle.ApplyMerge(op.Cypher, op.Params)
		parity("whole-pattern MERGE " + tc.step)
	}

	// Family (d): the map-parameter MERGE is rejected before anything is applied.
	nodesBefore, err := a.NodeCount()
	if err != nil {
		t.Fatalf("NodeCount: %v", err)
	}
	opD := Op{Kind: OpMalformed, Cypher: tmplMergeParamMap,
		Params: map[string]any{"map": map[string]any{"name": "Edsger", "age": int64(30)}}}
	committed, counters = runCountedOp(t, a, opD)
	if committed || counters != nil {
		t.Fatalf("MERGE (n $map) must be rejected: committed=%v counters=%+v", committed, counters)
	}
	if v := checkMergeRejection(0, opD, committed); len(v) > 0 {
		t.Fatalf("rejection check fired on a correctly rejected op: %v", v)
	}
	oracle.ApplyMalformed(opD.Cypher, opD.Params)
	if nodesAfter, err := a.NodeCount(); err != nil || nodesAfter != nodesBefore {
		t.Fatalf("rejected MERGE changed the graph: NodeCount %d -> %d (err %v)", nodesBefore, nodesAfter, err)
	}
	parity("rejected map-parameter MERGE")
}

// TestMergeSurface_SetAllReplacesMergeKey pins the destructive half of the
// whole-map ON CREATE semantics that the workload deliberately never binds: a
// map WITHOUT the merge key leaves a NAMELESS node, which makes the statement
// non-idempotent — the next identical MERGE cannot match what the first created
// and creates a second node. This is why the workload's map always carries name.
func TestMergeSurface_SetAllReplacesMergeKey(t *testing.T) {
	a := NewEngineAdapter(newTestEngine(t))
	ctx := context.Background()
	op := mergeOp(tmplMergePersonSetAll, map[string]any{
		"name": "Ada", "map": map[string]any{"age": int64(36)}})

	committed, counters := runCountedOp(t, a, op)
	if !committed {
		t.Fatal("set-all MERGE did not commit")
	}
	// One property set by the pattern plus one map entry; the pattern's name is
	// then cleared by the replace.
	wantCounters(t, "set-all with a name-less map", counters,
		&exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2, PropertiesRemoved: 1})

	n, err := a.scalarCount("MATCH (n:Person {name:'Ada'}) RETURN count(n)")
	if err != nil {
		t.Fatalf("name probe: %v", err)
	}
	if n != 0 {
		t.Fatalf("SET n = $map must destroy the merge key: %d nodes still named Ada, want 0", n)
	}

	// Non-idempotence: the second MERGE cannot match the nameless node.
	if committed, _ := runCountedOp(t, a, op); !committed {
		t.Fatal("second set-all MERGE did not commit")
	}
	total, err := a.NodeCount()
	if err != nil {
		t.Fatalf("NodeCount: %v", err)
	}
	if total != 2 {
		t.Fatalf("a name-destroying ON CREATE SET n = $map is not idempotent: NodeCount = %d, want 2", total)
	}

	// The oracle models the same thing: properties are exactly the map, and a
	// node the map left nameless is not indexed by name.
	oracle := NewGraphOracle()
	oracle.ApplyMerge(op.Cypher, op.Params)
	if oracle.HasPersonName("Ada") {
		t.Fatal("oracle indexed a node whose name the replace destroyed")
	}
	if got, err := a.projectRowStrings(ctx, "MATCH (n:Person) RETURN n.name, n.age", 2); err != nil {
		t.Fatalf("read back: %v", err)
	} else if got[0] != "null" || got[1] != "36" {
		t.Fatalf("engine node = (name %s, age %s), want (null, 36)", got[0], got[1])
	}
}

// TestMergeSurface_WholePatternIsAllOrNothing pins the second engine semantics
// the family rests on: an unbound endpoint is NEVER reused, so a whole-pattern
// MERGE over an existing key creates a DUPLICATE of it — including when the
// existing node came from a plain CREATE rather than from a previous MERGE.
func TestMergeSurface_WholePatternIsAllOrNothing(t *testing.T) {
	a := NewEngineAdapter(newTestEngine(t))
	oracle := NewGraphOracle()

	seed := Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": "wp0", "age": int64(1)}}
	if committed, _ := runCountedOp(t, a, seed); !committed {
		t.Fatal("seed CREATE did not commit")
	}
	oracle.ApplyCreate(seed.Cypher, seed.Params)

	op := mergeOp(tmplMergePairPattern, map[string]any{"a": "wp0", "b": "wp1"})
	if got, _ := classifyMergePattern(op, oracle); got != mergePatternOneEndpoint {
		t.Fatalf("classified as %v, want %v", got, mergePatternOneEndpoint)
	}
	committed, counters := runCountedOp(t, a, op)
	mustCleanCounters(t, op, committed, counters, oracle)
	wantCounters(t, "whole-pattern MERGE over an existing CREATE-born endpoint", counters,
		&exec.QueryCounters{NodesCreated: 2, RelationshipsCreated: 1, PropertiesSet: 2, LabelsAdded: 2})
	oracle.ApplyMerge(op.Cypher, op.Params)

	n, err := a.scalarCount("MATCH (n:Person {name:'wp0'}) RETURN count(n)")
	if err != nil {
		t.Fatalf("duplicate probe: %v", err)
	}
	if n != 2 {
		t.Fatalf("whole-pattern MERGE must not reuse an unbound endpoint: %d nodes named wp0, want 2", n)
	}
	// The duplicate is modelled, so count parity still holds and the endpoint
	// name probes the shared checker runs still reach the edge.
	if v := NewInvariantChecker(NewSeed(1)).Check(0, oracle, a); len(v) > 0 {
		t.Fatalf("count/sample parity after a duplicating whole-pattern MERGE: %v", v)
	}
	// The duplicate is NOT in the name index, so the per-name property and label
	// probes never see the ambiguity.
	if oracle.HasPersonName("wp1") {
		t.Fatal("whole-pattern endpoints must stay out of the oracle name index")
	}
}

// TestMergeSurface_CountersSensitivity is the sensitivity proof for the counters
// oracle: for each family, INVERTING the oracle's created-vs-matched
// adjudication must be caught. The engine's true effect report is held fixed and
// only the model it is judged against is perturbed, so the test proves the check
// discriminates rather than that the engine misbehaves.
func TestMergeSurface_CountersSensitivity(t *testing.T) {
	// (a) engine CREATED; a model that already holds the name predicts a match.
	t.Run("counter create judged as match", func(t *testing.T) {
		a := NewEngineAdapter(newTestEngine(t))
		op := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
		committed, counters := runCountedOp(t, a, op)
		if !committed || counters == nil {
			t.Fatalf("MERGE did not commit with counters (committed=%v)", committed)
		}
		inverted := NewGraphOracle()
		inverted.ApplyMerge(op.Cypher, op.Params) // model says the name is present
		v := CheckOpCounters(0, op, committed, counters, inverted)
		if len(v) == 0 {
			t.Fatal("counters oracle accepted a create judged against a matched model")
		}
		if !strings.Contains(v[0].Message, "NodesCreated") {
			t.Fatalf("violation should name NodesCreated: %s", v[0].Message)
		}
	})

	// (a) engine MATCHED; an empty model predicts a create.
	t.Run("counter match judged as create", func(t *testing.T) {
		a := NewEngineAdapter(newTestEngine(t))
		op := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
		if committed, _ := runCountedOp(t, a, op); !committed {
			t.Fatal("priming MERGE did not commit")
		}
		committed, counters := runCountedOp(t, a, op)
		if !committed || counters == nil {
			t.Fatalf("second MERGE did not commit with counters (committed=%v)", committed)
		}
		if v := CheckOpCounters(0, op, committed, counters, NewGraphOracle()); len(v) == 0 {
			t.Fatal("counters oracle accepted a match judged against an empty model")
		}
	})

	// (b) engine CREATED and applied the whole-map replace; a model that holds
	// the name predicts the all-zero match effect.
	t.Run("set-all create judged as match", func(t *testing.T) {
		a := NewEngineAdapter(newTestEngine(t))
		op := mergeOp(tmplMergePersonSetAll, map[string]any{
			"name": "Grace", "map": map[string]any{"name": "Grace", "age": int64(45)}})
		committed, counters := runCountedOp(t, a, op)
		if !committed || counters == nil {
			t.Fatalf("set-all MERGE did not commit with counters (committed=%v)", committed)
		}
		inverted := NewGraphOracle()
		inverted.ApplyMerge(op.Cypher, op.Params)
		v := CheckOpCounters(0, op, committed, counters, inverted)
		if len(v) == 0 {
			t.Fatal("counters oracle accepted a set-all create judged against a matched model")
		}
		if !strings.Contains(v[0].Message, "PropertiesRemoved") {
			t.Fatalf("violation should name PropertiesRemoved (the replace's merge-key teardown): %s", v[0].Message)
		}
	})

	// (c) engine CREATED the whole pattern; a model that already holds the
	// PAIRED edge predicts the all-zero match effect, and vice versa.
	t.Run("whole-pattern create judged as match", func(t *testing.T) {
		a := NewEngineAdapter(newTestEngine(t))
		op := mergeOp(tmplMergePairPattern, map[string]any{"a": "wp0", "b": "wp1"})
		committed, counters := runCountedOp(t, a, op)
		if !committed || counters == nil {
			t.Fatalf("whole-pattern MERGE did not commit with counters (committed=%v)", committed)
		}
		inverted := NewGraphOracle()
		inverted.ApplyMerge(op.Cypher, op.Params) // model says the pattern exists
		v := CheckOpCounters(0, op, committed, counters, inverted)
		if len(v) == 0 {
			t.Fatal("counters oracle accepted a whole-pattern create judged against a matched model")
		}
		if !strings.Contains(v[0].Message, "RelationshipsCreated") {
			t.Fatalf("violation should name RelationshipsCreated: %s", v[0].Message)
		}
	})

	t.Run("whole-pattern match judged as create", func(t *testing.T) {
		a := NewEngineAdapter(newTestEngine(t))
		op := mergeOp(tmplMergePairPattern, map[string]any{"a": "wp0", "b": "wp1"})
		if committed, _ := runCountedOp(t, a, op); !committed {
			t.Fatal("priming whole-pattern MERGE did not commit")
		}
		committed, counters := runCountedOp(t, a, op)
		if !committed || counters == nil {
			t.Fatalf("second whole-pattern MERGE did not commit with counters (committed=%v)", committed)
		}
		if v := CheckOpCounters(0, op, committed, counters, NewGraphOracle()); len(v) == 0 {
			t.Fatal("counters oracle accepted a whole-pattern match judged against an empty model")
		}
	})
}

// TestMergeSurface_ParitySensitivity is the sensitivity proof for the read-back
// parity checks: a wrong ON CREATE/ON MATCH counter VALUE is caught by the
// per-name property probe (it is projected on every Person since #2461), and a
// whole-pattern MERGE the model did not apply is caught by node/edge-count
// parity — so the state side of the oracle adjudicates too, not just the
// counters.
func TestMergeSurface_ParitySensitivity(t *testing.T) {
	// Direction 1: the engine incremented mc once; the model claims two hits.
	a := NewEngineAdapter(newTestEngine(t))
	oracle := NewGraphOracle()
	op := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
	for i := 0; i < 2; i++ {
		if committed, _ := runCountedOp(t, a, op); !committed {
			t.Fatalf("MERGE %d did not commit", i)
		}
		oracle.ApplyMerge(op.Cypher, op.Params)
	}
	if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
		t.Fatalf("faithful model should be clean: %v", v)
	}
	oracle.nodes[oracle.byName["Ada"]].Properties["mc"] = int64(99)
	v := CheckSchemaMutation(0, oracle, a)
	if len(v) == 0 {
		t.Fatal("parity missed a wrong ON MATCH counter value")
	}
	if !strings.Contains(v[0].Message, "mc") {
		t.Fatalf("violation should name the mc property: %s", v[0].Message)
	}

	// Direction 2: the engine created a whole pattern the model never applied —
	// two nodes and an edge the by-name probes cannot see, so it is count parity
	// that must fire.
	a2 := NewEngineAdapter(newTestEngine(t))
	oracle2 := NewGraphOracle()
	pat := mergeOp(tmplMergePairPattern, map[string]any{"a": "wp0", "b": "wp1"})
	if committed, _ := runCountedOp(t, a2, pat); !committed {
		t.Fatal("whole-pattern MERGE did not commit")
	}
	if v := NewInvariantChecker(NewSeed(1)).Check(0, oracle2, a2); len(v) == 0 {
		t.Fatal("count parity missed a whole-pattern MERGE the model never applied")
	}

	// Direction 3: the model applied a whole pattern the engine never ran.
	a3 := NewEngineAdapter(newTestEngine(t))
	oracle3 := NewGraphOracle()
	oracle3.ApplyMerge(pat.Cypher, pat.Params)
	if v := NewInvariantChecker(NewSeed(1)).Check(0, oracle3, a3); len(v) == 0 {
		t.Fatal("count parity missed a modelled whole pattern the engine never wrote")
	}
}

// TestMergeSurface_RejectionCheck proves the map-parameter rejection check has
// teeth: it is silent on the correct outcome (the engine refused the statement)
// and fires on the regression it exists to catch (the engine accepting it).
func TestMergeSurface_RejectionCheck(t *testing.T) {
	op := Op{Kind: OpMalformed, Cypher: tmplMergeParamMap,
		Params: map[string]any{"map": map[string]any{"name": "Ada"}}}
	if v := checkMergeRejection(0, op, false); len(v) > 0 {
		t.Fatalf("check fired on a rejected op: %v", v)
	}
	v := checkMergeRejection(0, op, true)
	if len(v) != 1 {
		t.Fatalf("check must fire when the engine accepts the op, got %d violations", len(v))
	}
	if !strings.Contains(v[0].Message, "ACCEPTED") {
		t.Fatalf("violation should say the statement was accepted: %s", v[0].Message)
	}
	// A different template is never the rejection family's business.
	other := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
	if v := checkMergeRejection(0, other, true); len(v) > 0 {
		t.Fatalf("check fired on an unrelated committed op: %v", v)
	}
}

// TestMergeSurface_NonVacuityGate verifies the assert-something-was-seen gate:
// an empty stats record fires every clause, and a fully-exercised record is
// clean.
func TestMergeSurface_NonVacuityGate(t *testing.T) {
	const wantClauses = 10 // four families + three branches + sub-cases + crash + survivor
	if v := checkMergeSurfaceNonVacuity(0, newMergeSurfaceStats()); len(v) != wantClauses {
		t.Fatalf("empty stats must fire all %d clauses, got %d: %v", wantClauses, len(v), v)
	}

	ms := newMergeSurfaceStats()
	oracle := NewGraphOracle()
	counterOp := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
	setAllOp := mergeOp(tmplMergePersonSetAll, map[string]any{
		"name": "Grace", "map": map[string]any{"name": "Grace", "age": int64(45)}})

	// ON CREATE branches (the model does not hold either name yet).
	ms.noteOp(counterOp, true, oracle)
	oracle.ApplyMerge(counterOp.Cypher, counterOp.Params)
	ms.noteOp(setAllOp, true, oracle)
	oracle.ApplyMerge(setAllOp.Cypher, setAllOp.Params)
	// ON MATCH branch of the counter family.
	ms.noteOp(counterOp, true, oracle)
	oracle.ApplyMerge(counterOp.Cypher, counterOp.Params)
	// Three of the four whole-pattern sub-cases.
	for _, p := range []map[string]any{
		{"a": "wp0", "b": "wp1"}, // neither
		{"a": "wp0", "b": "wp1"}, // whole pattern
		{"a": "wp0", "b": "wp2"}, // one endpoint
	} {
		op := mergeOp(tmplMergePairPattern, p)
		ms.noteOp(op, true, oracle)
		oracle.ApplyMerge(op.Cypher, op.Params)
	}
	ms.noteOp(Op{Kind: OpMalformed, Cypher: tmplMergeParamMap}, false, oracle)
	ms.noteRecovery(oracle)

	if seen := ms.patternCasesSeen(); seen != mergePatternCasesRequired {
		t.Fatalf("stats recorded %d sub-cases, want %d", seen, mergePatternCasesRequired)
	}
	if v := checkMergeSurfaceNonVacuity(0, ms); len(v) > 0 {
		t.Fatalf("fully-exercised stats must be clean: %v", v)
	}

	// The sub-case clause must genuinely discriminate: drop one and it fires.
	ms.patternCases[mergePatternOneEndpoint] = false
	v := checkMergeSurfaceNonVacuity(0, ms)
	if len(v) != 1 || !strings.Contains(v[0].Message, "sub-cases") {
		t.Fatalf("dropping a sub-case must fire exactly the sub-case clause, got: %v", v)
	}
}

// TestSchemaMutation_MergeGateWired proves the terminal MERGE gate is actually
// wired into the run loop: a schema-mutation run too short to issue the MERGE
// families and to crash after one must end with a merge non-vacuity report
// rather than a silent pass.
func TestSchemaMutation_MergeGateWired(t *testing.T) {
	sc := schemaMutationScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.Crash = CrashConfig{}
	cfg.Checkpoint = CheckpointConfig{}
	cfg.MaxTicks = 6
	report, err := runSchemaMutationCfg(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runSchemaMutationCfg: %v", err)
	}
	if report == nil {
		t.Fatal("a run too short to exercise the MERGE families must fail the gate; got a clean report")
	}
	found := false
	for _, v := range report.Violations {
		if v.Op == "merge non-vacuity" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a merge non-vacuity violation, got:\n%s", report)
	}
}
