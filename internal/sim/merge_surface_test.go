package sim

// merge_surface_test.go — MERGE surface completeness tests (rmp #2461): the
// scripted happy path for all four families with hand-computed effect sets, the
// two engine-semantics pins that the workload deliberately does not drive
// (a whole-map ON CREATE that destroys the merge key, and whole-pattern MERGE
// duplicating an existing endpoint), the sensitivity proofs that INVERTING the
// oracle's created-vs-matched adjudication is CAUGHT by the counters and parity
// checks, and the wiring proof for the terminal non-vacuity gate.
//
// It also holds the tests for the two families added since: the zero-row-driver
// no-ops (rmp #2512) and the node-only outer-relationship action over a
// CONSTRUCTED handle/id collision (rmp #2515).

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
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
	const wantClauses = 21 // ten families + eight branches + sub-cases + crash + survivor
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
	// The whole-entity relationship arm, on a fresh ordered pair so its ON CREATE
	// branch fires (rmp #2510).
	pairSetAllOp := mergeOp(tmplMergePairSetAll, map[string]any{
		"a": "wp3", "b": "wp4", "map": map[string]any{mergePairRelKey: int64(7)}})
	ms.noteOp(pairSetAllOp, true, oracle)
	oracle.ApplyMerge(pairSetAllOp.Cypher, pairSetAllOp.Params)
	// The two outer-target arms, each on a fresh ordered pair so their ON CREATE
	// branch fires (rmp #2511). The outer node is the counter family's Person,
	// which the model already indexes by name; the outer relationship is the
	// PAIRED edge the loop above created.
	pairOuterOp := mergeOp(tmplMergePairOuter, map[string]any{
		"a": "wp3", "b": "wp5", "m": "Ada", "v": int64(11)})
	ms.noteOp(pairOuterOp, true, oracle)
	oracle.ApplyMerge(pairOuterOp.Cypher, pairOuterOp.Params)
	pairOuterRelOp := mergeOp(tmplMergePairOuterRel, map[string]any{
		"a": "wp4", "b": "wp5", "x": "wp0", "y": "wp1", "v": int64(13)})
	ms.noteOp(pairOuterRelOp, true, oracle)
	oracle.ApplyMerge(pairOuterRelOp.Cypher, pairOuterRelOp.Params)
	// The zero-row-driver arms (rmp #2512). They have no branch to fire — the
	// clause never runs — so being issued is the whole record.
	for _, op := range []Op{
		mergeOp(tmplMergeZeroDriverNode, map[string]any{"absent": mergeZeroAbsentName, "z": mergeZeroKeys[0]}),
		mergeOp(tmplMergeZeroDriverPair, map[string]any{
			"absent": mergeZeroAbsentName, "za": mergeZeroKeys[0], "zb": mergeZeroKeys[1]}),
	} {
		ms.noteOp(op, true, oracle)
		oracle.ApplyMerge(op.Cypher, op.Params)
	}
	// The node-only outer-relationship arm (rmp #2515), both branches. Its action
	// targets the handle-collision fixture's relationship, so the fixture must be
	// modelled first; the create branch runs over a fresh merge key and the match
	// branch over the key the create just made.
	oracle.seedMergeHandleFixture()
	for _, op := range []Op{
		mergeOp(tmplMergeHandleOuterRelCreate, map[string]any{
			"x": mergeHandleSrcName, "y": mergeHandleDstName, "n": mergeHandleNodeKeys[0], "v": int64(17)}),
		mergeOp(tmplMergeHandleOuterRelMatch, map[string]any{
			"x": mergeHandleSrcName, "y": mergeHandleDstName, "n": mergeHandleNodeKeys[0], "v": int64(19)}),
	} {
		ms.noteOp(op, true, oracle)
		oracle.ApplyMerge(op.Cypher, op.Params)
	}
	ms.noteOp(Op{Kind: OpMalformed, Cypher: tmplMergeParamMap}, false, oracle)
	ms.noteRecovery(oracle)

	if seen := ms.patternCasesSeen(); seen < mergePatternCasesRequired {
		t.Fatalf("stats recorded %d sub-cases, want at least %d", seen, mergePatternCasesRequired)
	}
	if v := checkMergeSurfaceNonVacuity(0, ms); len(v) > 0 {
		t.Fatalf("fully-exercised stats must be clean: %v", v)
	}

	// The sub-case clause must genuinely discriminate: drop sub-cases until the
	// record falls below the threshold and exactly that one clause fires.
	for c := mergePatternCase(0); c < mergePatternCaseCount && ms.patternCasesSeen() >= mergePatternCasesRequired; c++ {
		ms.patternCases[c] = false
	}
	v := checkMergeSurfaceNonVacuity(0, ms)
	if len(v) != 1 || !strings.Contains(v[0].Message, "sub-cases") {
		t.Fatalf("dropping sub-cases must fire exactly the sub-case clause, got: %v", v)
	}
}

// TestSchemaMutation_MergeGateWired proves the terminal MERGE gate is actually
// wired into the run loop: a schema-mutation run too short to issue the MERGE
// families and to crash after one must end with a merge non-vacuity report
// rather than a silent pass.
//
// It also pins the rmp #2515 clauses specifically. Together with the full-length
// TestSchemaMutation_Scenario_Passes — which returns a CLEAN report, and therefore
// could not have left any of these clauses unsatisfied — this is what proves the
// live workload actually issued the node-only outer-relationship family and fired
// BOTH of its branches, rather than the family having quietly fallen back to its
// fixture-absent arm on every tick.
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
	var msgs []string
	for _, v := range report.Violations {
		if v.Op == "merge non-vacuity" {
			found = true
			msgs = append(msgs, v.Message)
		}
	}
	if !found {
		t.Fatalf("expected a merge non-vacuity violation, got:\n%s", report)
	}
	for _, want := range []string{
		"the handle-collision arm was vacuous",
		"never took its ON CREATE branch, so the misdirection-prone",
		"never took its ON MATCH branch, so the misdirection-prone",
	} {
		if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, want) }) {
			t.Fatalf("gate did not fire the rmp #2515 clause %q; fired: %v", want, msgs)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Zero-row-driver family (rmp #2512)
// ─────────────────────────────────────────────────────────────────────────────

// zeroDriverOps are the two ops of the zero-row-driver family, one per merge
// operator: the node-only [exec.Merge] and the whole-pattern [exec.MergePattern].
// Both were defective, so both are driven.
func zeroDriverOps() []Op {
	return []Op{
		mergeOp(tmplMergeZeroDriverNode, map[string]any{
			"absent": mergeZeroAbsentName, "z": mergeZeroKeys[0]}),
		mergeOp(tmplMergeZeroDriverPair, map[string]any{
			"absent": mergeZeroAbsentName, "za": mergeZeroKeys[0], "zb": mergeZeroKeys[1]}),
	}
}

// TestMergeZeroDriver_ScriptedNoOp drives both zero-row-driver ops through the
// REAL engine and asserts the whole guard end to end: the statement commits, its
// effect report is all-zero, the counters oracle accepts it, node and edge counts
// are unchanged, and [CheckMergeZeroDriverAbsent] finds nothing.
//
// The graph is seeded first, so the assertion is that the MERGE changed a
// NON-EMPTY graph not at all — a stronger statement than that an empty graph
// stayed empty, and one an engine that no-ops everything could not pass, because
// the seeding itself is counted.
func TestMergeZeroDriver_ScriptedNoOp(t *testing.T) {
	a := NewEngineAdapter(newTestEngine(t))
	oracle := NewGraphOracle()

	seed := mergeOp(tmplMergePersonCounter, map[string]any{"name": "Ada"})
	committed, counters := runCountedOp(t, a, seed)
	mustCleanCounters(t, seed, committed, counters, oracle)
	oracle.ApplyMerge(seed.Cypher, seed.Params)

	nodesBefore, err := a.NodeCount()
	if err != nil {
		t.Fatalf("NodeCount: %v", err)
	}
	edgesBefore, err := a.EdgeCount()
	if err != nil {
		t.Fatalf("EdgeCount: %v", err)
	}

	for _, op := range zeroDriverOps() {
		committed, counters := runCountedOp(t, a, op)
		if !committed {
			t.Fatalf("%q did not commit; a MERGE behind a MATCH that binds nothing is a legal no-op", op.Cypher)
		}
		wantCounters(t, "zero-row driver "+op.Cypher, counters, &exec.QueryCounters{})
		mustCleanCounters(t, op, committed, counters, oracle)
		oracle.ApplyMerge(op.Cypher, op.Params)
	}

	if got, err := a.NodeCount(); err != nil || got != nodesBefore {
		t.Fatalf("node count = %d (err %v), want %d: a MERGE driven by zero rows created a node (rmp #2512)",
			got, err, nodesBefore)
	}
	if got, err := a.EdgeCount(); err != nil || got != edgesBefore {
		t.Fatalf("edge count = %d (err %v), want %d: a MERGE driven by zero rows created a relationship (rmp #2512)",
			got, err, edgesBefore)
	}
	if v := CheckMergeZeroDriverAbsent(0, a); len(v) > 0 {
		t.Fatalf("zero-driver absence check must be clean after the family ran: %v", v)
	}
	if v := CheckSchemaMutation(0, oracle, a); len(v) > 0 {
		t.Fatalf("schema-mutation parity must be clean after the family ran: %v", v)
	}
}

// TestMergeZeroDriver_CountersArmIsReachable proves the counters oracle actually
// ADJUDICATES this family rather than skipping it. expectedOpCounters returns
// (want, exact); CheckOpCounters silently accepts anything when exact is false,
// so a template missing from the dispatch list in counters_oracle.go has a guard
// that can never fail. That is precisely how rmp #2510's arm sat dead until rmp
// #2511 found it, so the reachability is pinned here rather than assumed.
func TestMergeZeroDriver_CountersArmIsReachable(t *testing.T) {
	oracle := NewGraphOracle()
	for _, op := range zeroDriverOps() {
		want, exact := expectedOpCounters(op, oracle)
		if !exact {
			t.Fatalf("%q is not reached by the counters dispatch: its guard is DEAD CODE and can never fail", op.Cypher)
		}
		if want != (exec.QueryCounters{}) {
			t.Fatalf("%q expectation = %+v, want the all-zero effect set", op.Cypher, want)
		}
	}
	// The check must also REJECT a non-zero report — the shape rmp #2512 produced.
	phantom := &exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 1}
	for _, op := range zeroDriverOps() {
		if v := CheckOpCounters(0, op, true, phantom, oracle); len(v) == 0 {
			t.Fatalf("%q: counters oracle accepted a phantom effect report %+v", op.Cypher, *phantom)
		}
	}
}

// TestMergeZeroDriver_CheckerCatchesPhantom is the meta-test for the read-back
// half: it INJECTS by hand exactly the state a regressed engine would leave —
// a Person in the unreachable key namespace, a PAIRED edge between two of them,
// and a Person by the driver's never-matching name — and asserts the checker
// reports each one. Without this the clean result in
// TestMergeZeroDriver_ScriptedNoOp would be consistent with a checker that
// cannot fail.
func TestMergeZeroDriver_CheckerCatchesPhantom(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inject  string
		wantOp  string
		wantMsg string
	}{
		{
			name:    "phantom_node",
			inject:  "CREATE (:Person {name:'" + mergeZeroKeys[0] + "'})",
			wantOp:  "merge zero-driver node",
			wantMsg: "CREATED a node",
		},
		{
			name: "phantom_pattern",
			inject: "CREATE (:Person {name:'" + mergeZeroKeys[0] + "'})-[:" + relPaired +
				"]->(:Person {name:'" + mergeZeroKeys[1] + "'})",
			wantOp:  "merge zero-driver relationship",
			wantMsg: "CREATED the pattern",
		},
		{
			name:    "broken_premise",
			inject:  "CREATE (:Person {name:'" + mergeZeroAbsentName + "'})",
			wantOp:  "merge zero-driver premise",
			wantMsg: "must bind NOTHING",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewEngineAdapter(newTestEngine(t))
			if v := CheckMergeZeroDriverAbsent(0, a); len(v) > 0 {
				t.Fatalf("empty graph must be clean: %v", v)
			}
			op := Op{Kind: OpCreate, Cypher: tc.inject}
			if committed, _ := runCountedOp(t, a, op); !committed {
				t.Fatalf("injection %q did not commit", tc.inject)
			}
			v := CheckMergeZeroDriverAbsent(0, a)
			if len(v) == 0 {
				t.Fatalf("checker did not fire on %q: it cannot detect rmp #2512", tc.inject)
			}
			found := false
			for _, viol := range v {
				if viol.Op == tc.wantOp && strings.Contains(viol.Message, tc.wantMsg) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected a %q violation containing %q, got: %v", tc.wantOp, tc.wantMsg, v)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Node-only outer-relationship action over a constructed collision (rmp #2515)
// ─────────────────────────────────────────────────────────────────────────────

// newHandleCollisionSim builds a schema-mutation Simulator and runs the fixture
// bootstrap on it, returning both. It is the shared setup for the family's tests:
// every one of them needs the CONSTRUCTED collision, because on any other graph
// the defect they exist to detect cannot manifest.
func newHandleCollisionSim(t *testing.T) (*Simulator, *mergeHandleFixture) {
	t.Helper()
	sc := schemaMutationScenario()
	sm, err := New(sc.DeterministicConfig(sc.DefaultSeed))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	f, v := seedMergeHandleCollision(context.Background(), sm)
	if len(v) > 0 {
		t.Fatalf("fixture bootstrap reported violations: %v", v)
	}
	return sm, f
}

// TestMergeHandleCollision_FixtureIsConstructed proves the precondition is BUILT
// rather than awaited: the fixture relationship's stable handle equals a live
// node's id, that node is the decoy, and the detector agrees on the state the
// bootstrap just produced.
//
// The equivalent end-to-end matrix reproduced this collision on about 1% of its
// runs, so a family that merely hoped for it would adjudicate nothing on 99 runs
// in 100 while reporting green.
func TestMergeHandleCollision_FixtureIsConstructed(t *testing.T) {
	sm, f := newHandleCollisionSim(t)
	g := sm.graph()
	t.Logf("fixture: %s(%q->%q) handle=%d, decoy %q node id=%d",
		relPaired, f.srcKey, f.dstKey, f.handle, mergeHandleDecoyName, f.decoyID)

	if f.decoyID == 0 {
		t.Fatal("decoy node id 0 is the reserved no-handle sentinel: no relationship could ever collide with it")
	}
	if f.handle != uint64(f.decoyID) {
		t.Fatalf("handle = %d, want %d (the decoy's node id): the collision was not constructed", f.handle, f.decoyID)
	}
	if got := personNameByID(g, graph.NodeID(f.handle)); got != mergeHandleDecoyName {
		t.Fatalf("node id %d is %q, want the decoy %q", f.handle, got, mergeHandleDecoyName)
	}
	// The handle must come from the ENGINE's own durable identity for the edge,
	// not from the counter the bootstrap seeded.
	handle, ok := g.FirstEdgeHandle(f.srcKey, f.dstKey)
	if !ok || handle != f.handle {
		t.Fatalf("FirstEdgeHandle(%q,%q) = (%d,%v), want (%d,true)", f.srcKey, f.dstKey, handle, ok, f.handle)
	}
	if v := CheckMergeHandleCollision(0, f, g, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("the freshly-built fixture must be clean: %v", v)
	}
	// The bootstrap's three nodes and one relationship are modelled, so count
	// parity holds and nothing it created is loose in the graph.
	if v := NewInvariantChecker(NewSeed(1)).Check(0, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("count parity after the fixture bootstrap: %v", v)
	}
}

// TestMergeHandleCollision_ScriptedBranches drives both branches of the family
// through the REAL engine over the constructed collision and asserts the whole
// chain per step: the hand-computed effect set, the counters oracle accepting it,
// and the detector confirming the write reached the RELATIONSHIP and no node.
//
// Each of the four (template × created/matched) cells is covered, because the
// branch a statement carries and the branch that FIRES are independent.
func TestMergeHandleCollision_ScriptedBranches(t *testing.T) {
	sm, f := newHandleCollisionSim(t)
	ctx := context.Background()

	step := func(label, tmpl, n string, v int64, want exec.QueryCounters) {
		t.Helper()
		op := mergeOp(tmpl, map[string]any{
			"x": mergeHandleSrcName, "y": mergeHandleDstName, "n": n, "v": v})
		committed, counters := sm.executeCounted(ctx, op)
		mustCleanCounters(t, op, committed, counters, sm.oracle)
		wantCounters(t, label, counters, &want)
		sm.oracle.ApplyMerge(op.Cypher, op.Params)
		if vs := CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine); len(vs) > 0 {
			t.Fatalf("%s: %v", label, vs)
		}
		if vs := CheckSchemaMutation(0, sm.oracle, sm.engine); len(vs) > 0 {
			t.Fatalf("%s: parity: %v", label, vs)
		}
	}

	// ON CREATE, node created: the pattern's name plus the relationship write.
	step("ON CREATE / created", tmplMergeHandleOuterRelCreate, mergeHandleNodeKeys[0], 17,
		exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2})
	// ON MATCH, node matched: the relationship write alone.
	step("ON MATCH / matched", tmplMergeHandleOuterRelMatch, mergeHandleNodeKeys[0], 19,
		exec.QueryCounters{PropertiesSet: 1})
	// ON CREATE, node matched: the branch does not fire, so nothing is applied.
	step("ON CREATE / matched", tmplMergeHandleOuterRelCreate, mergeHandleNodeKeys[0], 23,
		exec.QueryCounters{})
	// ON MATCH, node created: the node is written, the branch is not.
	step("ON MATCH / created", tmplMergeHandleOuterRelMatch, mergeHandleNodeKeys[1], 29,
		exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 1})

	// The last write that fired was the ON MATCH one, so 19 is what the
	// relationship must still hold — read back through the engine, not the model.
	got, err := sm.engine.projectRowStrings(ctx, "MATCH (x:Person {name:'"+mergeHandleSrcName+"'})-[k:"+
		relPaired+"]->(y:Person {name:'"+mergeHandleDstName+"'}) RETURN k."+mergePairRelKey, 1)
	if err != nil {
		t.Fatalf("relationship read-back: %v", err)
	}
	if got[0] != "19" {
		t.Fatalf("fixture relationship .%s = %s, want 19", mergePairRelKey, got[0])
	}
}

// TestMergeHandleCollision_CountersArmIsReachable proves the counters oracle
// ADJUDICATES this family rather than skipping it. expectedOpCounters returns
// (want, exact) and CheckOpCounters accepts anything when exact is false, so a
// template missing from the dispatch list in counters_oracle.go has a guard that
// can never fail — which is how rmp #2510's arm sat dead for two tasks.
func TestMergeHandleCollision_CountersArmIsReachable(t *testing.T) {
	created := exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 2}
	matched := exec.QueryCounters{PropertiesSet: 1}
	for _, tc := range []struct {
		name, tmpl string
		present    bool
		want       exec.QueryCounters
	}{
		{"on_create_creates", tmplMergeHandleOuterRelCreate, false, created},
		{"on_create_matches", tmplMergeHandleOuterRelCreate, true, exec.QueryCounters{}},
		{"on_match_matches", tmplMergeHandleOuterRelMatch, true, matched},
		{"on_match_creates", tmplMergeHandleOuterRelMatch, false,
			exec.QueryCounters{NodesCreated: 1, LabelsAdded: 1, PropertiesSet: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oracle := NewGraphOracle()
			oracle.seedMergeHandleFixture()
			if tc.present {
				oracle.newPairNode(mergeHandleNodeKeys[0])
			}
			op := mergeOp(tc.tmpl, map[string]any{
				"x": mergeHandleSrcName, "y": mergeHandleDstName, "n": mergeHandleNodeKeys[0], "v": int64(7)})
			want, exact := expectedOpCounters(op, oracle)
			if !exact {
				t.Fatalf("%q is not reached by the counters dispatch: its guard is DEAD CODE and can never fail", tc.tmpl)
			}
			if want != tc.want {
				t.Fatalf("expectation = %+v, want %+v", want, tc.want)
			}
			// And it must REJECT a report that is off by the branch's one assignment.
			wrong := want
			wrong.PropertiesSet++
			if v := CheckOpCounters(0, op, true, &wrong, oracle); len(v) == 0 {
				t.Fatalf("counters oracle accepted %+v against an expectation of %+v", wrong, want)
			}
		})
	}
}

// TestMergeHandleCollision_CheckerCatchesMisdirection is the meta-test for the
// read-back: it INJECTS by hand exactly the state a regressed engine leaves — the
// relationship without the property and the DECOY node with it — and asserts the
// detector reports both halves. Without this the clean result of the scripted
// test would be consistent with a checker that cannot fail.
//
// It also pins the two halves as independent detectors: the decoy clause fires
// even when the relationship is correct, which is what makes a write landing on
// BOTH (a partially-fixed engine) visible too.
func TestMergeHandleCollision_CheckerCatchesMisdirection(t *testing.T) {
	t.Run("misdirected_write", func(t *testing.T) {
		sm, f := newHandleCollisionSim(t)
		// The model records the write the statement was supposed to perform...
		outer := sm.oracle.pairedEdgeBetween(mergeHandleSrcName, mergeHandleDstName)
		if outer == nil {
			t.Fatal("the fixture relationship is not modelled")
		}
		outer.Properties[mergePairRelKey] = int64(17)
		// ...while the engine put it on the node whose id equals the handle.
		inject := Op{Kind: OpUpdate, Cypher: "MATCH (p:Person {name:'" + mergeHandleDecoyName + "'}) SET p." +
			mergePairRelKey + " = 17"}
		if committed, _ := sm.executeCounted(context.Background(), inject); !committed {
			t.Fatal("injection did not commit")
		}
		v := CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine)
		var sawRel, sawDecoy bool
		for _, viol := range v {
			switch viol.Op {
			case "merge handle-collision rel-prop":
				sawRel = strings.Contains(viol.Message, "did not reach the relationship")
			case "merge handle-collision decoy":
				sawDecoy = strings.Contains(viol.Message, "MISDIRECTED")
			}
		}
		if !sawRel || !sawDecoy {
			t.Fatalf("detector missed a misdirected write (rel=%v decoy=%v): %v", sawRel, sawDecoy, v)
		}
	})

	t.Run("decoy_clause_fires_alone", func(t *testing.T) {
		sm, f := newHandleCollisionSim(t)
		// The relationship is correct in both engine and model; only the stray node
		// property is wrong, so the decoy clause is the sole detector.
		op := mergeOp(tmplMergeHandleOuterRelCreate, map[string]any{
			"x": mergeHandleSrcName, "y": mergeHandleDstName, "n": mergeHandleNodeKeys[0], "v": int64(17)})
		if committed, _ := sm.executeCounted(context.Background(), op); !committed {
			t.Fatal("family op did not commit")
		}
		sm.oracle.ApplyMerge(op.Cypher, op.Params)
		if v := CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine); len(v) > 0 {
			t.Fatalf("a correct write must be clean: %v", v)
		}
		inject := Op{Kind: OpUpdate, Cypher: "MATCH (p:Person {name:'" + mergeHandleDecoyName + "'}) SET p." +
			mergePairRelKey + " = 17"}
		if committed, _ := sm.executeCounted(context.Background(), inject); !committed {
			t.Fatal("injection did not commit")
		}
		v := CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine)
		if len(v) != 1 || v[0].Op != "merge handle-collision decoy" {
			t.Fatalf("want exactly the decoy violation, got: %v", v)
		}
	})

	t.Run("broken_premise", func(t *testing.T) {
		sm, f := newHandleCollisionSim(t)
		// A fixture whose handle no longer names the decoy is a graph on which the
		// defect cannot manifest. Reporting it is what stops the family passing for
		// the wrong reason.
		stale := *f
		stale.handle++
		v := CheckMergeHandleCollision(0, &stale, sm.graph(), sm.oracle, sm.engine)
		if len(v) == 0 {
			t.Fatal("detector accepted a fixture whose handle changed: the premise check is vacuous")
		}
		if v[0].Op != "merge handle-collision premise" || !strings.Contains(v[0].Message, "not stable") {
			t.Fatalf("want a premise violation naming the changed handle, got: %v", v)
		}
	})

	t.Run("no_fixture", func(t *testing.T) {
		sm, _ := newHandleCollisionSim(t)
		v := CheckMergeHandleCollision(0, nil, sm.graph(), sm.oracle, sm.engine)
		if len(v) != 1 || !strings.Contains(v[0].Message, "no handle-collision fixture") {
			t.Fatalf("an unbound fixture must be reported, got: %v", v)
		}
	})
}

// TestMergeHandleCollision_SurvivesRecovery proves the CONSTRUCTED precondition is
// durable: after a crash and a real snapshot+WAL recovery the relationship keeps
// its handle, the decoy keeps the node id that handle collides with, and the
// family's write is still on the relationship.
//
// The family runs behind crash injection, so a collision that dissolved on the
// first recovery would leave every later tick adjudicating nothing — the precise
// failure the premise clause exists to make loud, verified here to be absent.
func TestMergeHandleCollision_SurvivesRecovery(t *testing.T) {
	sm, f := newHandleCollisionSim(t)
	ctx := context.Background()

	op := mergeOp(tmplMergeHandleOuterRelCreate, map[string]any{
		"x": mergeHandleSrcName, "y": mergeHandleDstName, "n": mergeHandleNodeKeys[0], "v": int64(41)})
	if committed, _ := sm.executeCounted(ctx, op); !committed {
		t.Fatal("family op did not commit")
	}
	sm.oracle.ApplyMerge(op.Cypher, op.Params)

	if err := sm.store.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// SIGKILL-equivalent, exactly as [Simulator.maybeCrash] performs it.
	sm.disk.Crash()
	store, err := OpenSimStore(sm.disk, sm.store.Config())
	if err != nil {
		t.Fatalf("crash recovery: %v", err)
	}
	sm.store = store
	sm.engine = NewEngineAdapter(store.Engine())

	if v := CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("the collision did not survive crash + recovery: %v", v)
	}
	if v := CheckSchemaMutation(0, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("post-recovery parity: %v", v)
	}
}
