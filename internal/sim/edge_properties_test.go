package sim

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// TestEdgeProperties_Scenario_Passes runs the edge-properties scenario: every
// surviving KNOWS instance's properties must round-trip, every DELETE r'd
// instance must stay absent with both endpoints alive, and all of it must
// survive crash/recovery.
func TestEdgeProperties_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioEdgeProperties)
	if !ok {
		t.Fatalf("edge-properties scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("edge-properties run: %v", err)
	}
	if report != nil {
		t.Fatalf("edge-properties reported a violation:\n%s", report)
	}
}

// edgePropsOpStream drives the edge-properties workload for ticks ticks
// (crash/recovery included, per the scenario schedule) and returns one
// canonical fingerprint per executed op. It fails the test on any harness
// error or invariant violation.
func edgePropsOpStream(t *testing.T, seed uint64, ticks int) []string {
	t.Helper()
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(seed)
	cfg.MaxTicks = ticks
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	ctx := context.Background()
	out := make([]string, 0, ticks)
	for i := 0; i < ticks; i++ {
		tick := sm.clock.Tick()
		report, err := sm.maybeCrash(ctx, tick)
		if err != nil {
			t.Fatalf("crash recovery at tick %d: %v", tick, err)
		}
		if report != nil {
			t.Fatalf("durability violation at tick %d:\n%s", tick, report)
		}
		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		if v := CheckOpCounters(tick, op, committed, counters, sm.oracle); len(v) > 0 {
			t.Fatalf("counters violation at tick %d: %v", tick, v)
		}
		sm.applyToOracle(op, committed)
		out = append(out, fingerprintOp(op))
	}
	if v := CheckEdgeProperties(int64(ticks), sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("edge-property violation: %v", v)
	}
	return out
}

// fingerprintOp renders an op as a canonical string (template plus sorted
// key=value parameters) for determinism comparison.
func fingerprintOp(op Op) string {
	keys := make([]string, 0, len(op.Params))
	for k := range op.Params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString(op.Cypher)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, canonicalValueString(op.Params[k]))
	}
	return b.String()
}

// TestEdgeProperties_NonVacuous confirms the workload actually exercises every
// relationship-write family — instance CREATE, standalone SET r.weight,
// REMOVE r.since, and DELETE r — and that a genuine parallel-edge pair (two
// live instances between the same endpoints) existed during the run.
func TestEdgeProperties_NonVacuous(t *testing.T) {
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	ctx := context.Background()
	counts := map[string]int{}
	sawParallel := false
	for i := 0; i < cfg.MaxTicks; i++ {
		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		counts[op.Cypher]++
		if !sawParallel {
			pairs := map[string]int{}
			for _, in := range sm.oracle.KnowsInstancesByEID() {
				pairs[in.Src+"\x00"+in.Dst]++
				if pairs[in.Src+"\x00"+in.Dst] > 1 {
					sawParallel = true
					break
				}
			}
		}
	}
	for _, tmpl := range []string{
		tmplCreateKnowsInst, tmplSetKnowsWeight, tmplRemoveKnowsSince, tmplDeleteKnowsInst,
	} {
		if counts[tmpl] == 0 {
			t.Errorf("vacuous: template never emitted: %s", tmpl)
		}
	}
	if !sawParallel {
		t.Error("vacuous: no parallel-edge pair (two live instances between the same endpoints) was ever modelled")
	}
	if len(sm.oracle.DeletedKnowsInstances()) == 0 {
		t.Error("vacuous: no DELETE r tombstone was recorded")
	}
}

// TestEdgeProperties_Deterministic verifies the extended workload stays
// bit-reproducible: two runs from the same seed (crash/recovery included)
// produce the identical op stream.
func TestEdgeProperties_Deterministic(t *testing.T) {
	const ticks = 200
	a := edgePropsOpStream(t, 0xE1DE, ticks)
	b := edgePropsOpStream(t, 0xE1DE, ticks)
	if len(a) != len(b) {
		t.Fatalf("op stream lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("op stream diverged at tick %d:\n  a: %s\n  b: %s", i+1, a[i], b[i])
		}
	}
}

// scriptOp runs op through the engine, asserts it committed and that its
// reported counters agree with the oracle's expectation, then applies it to
// the model — the scripted-sequence analogue of one simulator tick.
func scriptOp(t *testing.T, sm *Simulator, tick int64, op Op) {
	t.Helper()
	committed, counters := sm.executeCounted(context.Background(), op)
	if !committed {
		t.Fatalf("op %q did not commit", op.Cypher)
	}
	if v := CheckOpCounters(tick, op, committed, counters, sm.oracle); len(v) > 0 {
		t.Fatalf("op %q counters violation: %v", op.Cypher, v)
	}
	sm.applyToOracle(op, committed)
}

// Scripted-op builders for the relationship-write templates.
func opPerson(name string) Op {
	return Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": name, "age": int64(30)}}
}

func opKnowsInst(a, b string, eid int64, since string, weight float64) Op {
	return Op{Kind: OpCreate, Cypher: tmplCreateKnowsInst,
		Params: map[string]any{"a": a, "b": b, "eid": eid, "since": since, "weight": weight}}
}

func opSetWeight(a, b string, eid int64, weight float64) Op {
	return Op{Kind: OpUpdate, Cypher: tmplSetKnowsWeight,
		Params: map[string]any{"a": a, "b": b, "eid": eid, "weight": weight}}
}

func opRemoveSince(a, b string, eid int64) Op {
	return Op{Kind: OpUpdate, Cypher: tmplRemoveKnowsSince, Params: map[string]any{"a": a, "b": b, "eid": eid}}
}

func opDeleteInst(a, b string, eid int64) Op {
	return Op{Kind: OpDelete, Cypher: tmplDeleteKnowsInst, Params: map[string]any{"a": a, "b": b, "eid": eid}}
}

// scriptParallelPair drives the canonical scripted sequence over a
// parallel-edge pair on sm: persons A and B, twin instances eid 1 and 2, SET
// r.weight on eid 1, REMOVE r.since on eid 2 (twice — the second is the no-op
// removal the counters must report as nothing), then DELETE r on eid 1. Each
// op's reported counters are checked against the oracle, and the read-back
// check must be clean at every stage.
func scriptParallelPair(t *testing.T, sm *Simulator) {
	t.Helper()
	scriptOp(t, sm, 1, opPerson("A"))
	scriptOp(t, sm, 2, opPerson("B"))
	scriptOp(t, sm, 3, opKnowsInst("A", "B", 1, "2026-01-01", 1.5))
	scriptOp(t, sm, 4, opKnowsInst("A", "B", 2, "2026-02-02", 2.5)) // parallel twin
	if v := CheckEdgeProperties(4, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("after parallel CREATE: %v", v)
	}
	scriptOp(t, sm, 5, opSetWeight("A", "B", 1, 9.75))
	scriptOp(t, sm, 6, opRemoveSince("A", "B", 2))
	scriptOp(t, sm, 7, opRemoveSince("A", "B", 2)) // removing an absent property counts nothing
	if v := CheckEdgeProperties(7, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("after SET/REMOVE: %v", v)
	}
	scriptOp(t, sm, 8, opDeleteInst("A", "B", 1))
	if v := CheckEdgeProperties(8, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("after DELETE r: %v", v)
	}
}

// TestEdgeProperties_ScriptedParallelPair covers all three relationship-write
// families over one parallel-edge pair on the in-memory multigraph engine:
// only the targeted instance changes, its twin survives with its own property
// map, the deleted instance's endpoints stay alive, and every op's counters
// match the oracle's expectation exactly.
func TestEdgeProperties_ScriptedParallelPair(t *testing.T) {
	sm, err := New(Config{Seed: 1, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(1)), Multigraph: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	scriptParallelPair(t, sm)

	if got := sm.oracle.EdgeCount(); got != 1 {
		t.Fatalf("oracle edge count after DELETE r = %d, want 1 (the surviving twin)", got)
	}
	gotE, err := sm.engine.EdgeCount()
	if err != nil {
		t.Fatalf("engine.EdgeCount: %v", err)
	}
	if gotE != 1 {
		t.Fatalf("engine edge count after DELETE r = %d, want 1 (the surviving twin)", gotE)
	}
	insts := sm.oracle.KnowsInstancesByEID()
	if len(insts) != 1 || insts[0].EID != 2 {
		t.Fatalf("surviving instance = %+v, want exactly eid 2", insts)
	}
	if dels := sm.oracle.DeletedKnowsInstances(); len(dels) != 1 || dels[0].EID != 1 {
		t.Fatalf("deleted tombstones = %+v, want exactly eid 1", dels)
	}
}

// TestEdgeProperties_ScriptedParallelPair_SurvivesCrash runs the same scripted
// sequence on the DURABLE (SimDisk-backed) multigraph store, then crashes and
// recovers: the surviving twin's asymmetric property map (weight kept, since
// removed), the deleted instance's absence, and both endpoints must all
// survive WAL recovery.
func TestEdgeProperties_ScriptedParallelPair_SurvivesCrash(t *testing.T) {
	sm, err := New(Config{
		Seed: 2, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(2)),
		Multigraph: true,
		Disk:       DiskConfig{CapacityBytes: 1 << 30}, // opts into the durable store; no scheduled crash
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	scriptParallelPair(t, sm)

	// SIGKILL-equivalent crash, then reopen through real WAL recovery — exactly
	// what Simulator.maybeCrash does.
	sm.disk.Crash()
	store, err := OpenSimStore(sm.disk, sm.store.Config())
	if err != nil {
		t.Fatalf("recovery reopen: %v", err)
	}
	sm.store = store
	sm.engine = NewEngineAdapter(store.Engine())

	if v := sm.checker.CheckDurability(9, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("post-recovery durability: %v", v)
	}
	if v := CheckEdgeProperties(9, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("post-recovery edge properties: %v", v)
	}
}

// TestEdgeProperties_DetectsTwinPerturbation is the sensitivity proof for the
// read-back check: perturbing one parallel twin's property map in the MODEL
// (while the engine holds the real values) must fire the checker.
func TestEdgeProperties_DetectsTwinPerturbation(t *testing.T) {
	sm, err := New(Config{Seed: 3, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(3)), Multigraph: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	scriptParallelPair(t, sm)

	// Corrupt the surviving twin's modelled weight.
	k := edgeKey{src: sm.oracle.byName["A"], dst: sm.oracle.byName["B"], label: "KNOWS", eid: 2}
	sm.oracle.edges[k].Properties["weight"] = 999.0
	if v := CheckEdgeProperties(10, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("checker FAILED to detect a perturbed parallel-twin property map")
	}
}

// TestEdgeProperties_DetectsWrongInstanceTarget is the second sensitivity
// proof: the engine mutates instance eid 1 but the model records the SET
// against its twin (eid 2) — the exact wrong-instance identity confusion the
// by-handle code guards — and the checker must fire.
func TestEdgeProperties_DetectsWrongInstanceTarget(t *testing.T) {
	sm, err := New(Config{Seed: 4, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(4)), Multigraph: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	scriptOp(t, sm, 1, opPerson("A"))
	scriptOp(t, sm, 2, opPerson("B"))
	scriptOp(t, sm, 3, opKnowsInst("A", "B", 1, "2026-01-01", 1.5))
	scriptOp(t, sm, 4, opKnowsInst("A", "B", 2, "2026-02-02", 2.5))

	// Engine SETs eid 1; the model (wrongly) applies the same SET to eid 2.
	committed, _ := sm.executeCounted(ctx, opSetWeight("A", "B", 1, 9.75))
	if !committed {
		t.Fatal("engine SET did not commit")
	}
	sm.applyToOracle(opSetWeight("A", "B", 2, 9.75), committed)
	if v := CheckEdgeProperties(5, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("checker FAILED to detect a SET recorded against the wrong parallel-edge instance")
	}
}

// TestEdgeProperties_CountersSensitivity_DeleteR is the counters sensitivity
// proof: a DELETE r whose reported effect set deviates from the oracle's
// expectation (a phantom node deletion, a missing relationship deletion, or a
// nil report) must fire CheckOpCounters, and the exact expected set must not.
func TestEdgeProperties_CountersSensitivity_DeleteR(t *testing.T) {
	o := NewGraphOracle()
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "A", "age": int64(1)})
	o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "B", "age": int64(1)})
	o.ApplyCreate(tmplCreateKnowsInst, map[string]any{"a": "A", "b": "B", "eid": int64(1), "since": "2026-01-01", "weight": 1.5})

	del := opDeleteInst("A", "B", 1)
	for _, tc := range []struct {
		got     *exec.QueryCounters
		name    string
		wantHit bool
	}{
		{name: "exact effect passes", got: &exec.QueryCounters{RelationshipsDeleted: 1}, wantHit: false},
		{name: "phantom node deletion fires", got: &exec.QueryCounters{RelationshipsDeleted: 1, NodesDeleted: 1}, wantHit: true},
		{name: "missing relationship deletion fires", got: &exec.QueryCounters{}, wantHit: true},
		{name: "nil counters on a committed write fires", got: nil, wantHit: true},
	} {
		v := CheckOpCounters(1, del, true, tc.got, o)
		if hit := len(v) > 0; hit != tc.wantHit {
			t.Errorf("%s: violations=%v, wantHit=%v", tc.name, v, tc.wantHit)
		}
	}
}

// TestEdgeProperties_RemoveCountersPerInstance is the regression guard for
// the engine defect the counters oracle found (rmp #2449, fixed as #2500):
// REMOVE on parallel edges must attribute PropertiesRemoved per INSTANCE.
// Before the fix, the -properties gate read the per-pair aggregate store
// (lpgMutatorAdapter.DelEdgeProperty / walMutatorAdapter.DelEdgeProperty in
// cypher/api.go), so only the first removal on a (src,dst) pair reported
// -properties 1 and a later removal of a genuinely present property on a
// sibling instance reported 0 — while the STATE effect was per-instance and
// correct throughout. The DST demands the exact count via the
// tmplRemoveKnowsSince expectation in expectedKnowsInstCounters; this test
// pins the same contract directly, plus the absent-property no-op.
func TestEdgeProperties_RemoveCountersPerInstance(t *testing.T) {
	sm, err := New(Config{Seed: 5, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(5)), Multigraph: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	scriptOp(t, sm, 1, opPerson("A"))
	scriptOp(t, sm, 2, opPerson("B"))
	scriptOp(t, sm, 3, opKnowsInst("A", "B", 1, "2026-01-01", 1.5))
	scriptOp(t, sm, 4, opKnowsInst("A", "B", 2, "2026-02-02", 2.5))

	remove := func(eid int64) *exec.QueryCounters {
		res, err := sm.engine.RunWrite(ctx, tmplRemoveKnowsSince,
			map[string]any{"a": "A", "b": "B", "eid": eid})
		if err != nil {
			t.Fatalf("REMOVE eid=%d: %v", eid, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("REMOVE eid=%d drain: %v", eid, err)
		}
		c := res.(counterReporter).Counters()
		_ = res.Close()
		return c
	}
	first := remove(1)
	second := remove(2)
	// A third REMOVE re-targets instance 1, whose `since` is already gone:
	// removing an absent property is an openCypher no-op and counts nothing.
	absent := remove(1)

	// The state effect is per-instance and correct: both `since` values are gone.
	probe := func(eid int64) []string {
		got, err := sm.engine.projectRowStrings(ctx, fmt.Sprintf(
			"MATCH (:Person {name:'A'})-[r:KNOWS]->(:Person {name:'B'}) WHERE r.eid = %d RETURN r.since", eid), 1)
		if err != nil {
			t.Fatalf("probe eid=%d: %v", eid, err)
		}
		return got
	}
	if got := probe(1); len(got) == 0 || got[0] != canonicalValueString(nil) {
		t.Fatalf("eid 1 since after REMOVE = %v, want null", got)
	}
	if got := probe(2); len(got) == 0 || got[0] != canonicalValueString(nil) {
		t.Fatalf("eid 2 since after REMOVE = %v, want null", got)
	}

	// The counters are per-instance: each removal of a present `since` reports
	// exactly one -properties, and the absent-property re-removal reports zero.
	if first == nil || first.PropertiesRemoved != 1 {
		t.Fatalf("first REMOVE PropertiesRemoved = %+v, want 1 (per-instance attribution, rmp #2500)", first)
	}
	if second == nil || second.PropertiesRemoved != 1 {
		t.Fatalf("second REMOVE PropertiesRemoved = %+v, want 1: a genuinely present property on a "+
			"sibling parallel instance must count — the per-pair aggregate gate is the #2500 defect", second)
	}
	if absent == nil || absent.PropertiesRemoved != 0 {
		t.Fatalf("absent-property REMOVE PropertiesRemoved = %+v, want 0 (openCypher no-op)", absent)
	}
}

// TestEdgeProperties_DetectsMismatch is the original meta-test on the eid-less
// template: a modelled edge property that disagrees with the engine must be
// flagged (the legacy simple-graph probe path, no instance pinning).
func TestEdgeProperties_DetectsMismatch(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	if _, err := sm.engine.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": "A", "age": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := sm.engine.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": "B", "age": int64(1)}); err != nil {
		t.Fatal(err)
	}
	sm.oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "A", "age": int64(1)})
	sm.oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "B", "age": int64(1)})
	if _, err := sm.engine.RunWrite(ctx, tmplCreateKnowsProps, map[string]any{"a": "A", "b": "B", "since": "2026-01-01", "weight": 1.0}); err != nil {
		t.Fatal(err)
	}
	// Model a DIFFERENT weight than the engine stored.
	sm.oracle.ApplyCreate(tmplCreateKnowsProps, map[string]any{"a": "A", "b": "B", "since": "2026-01-01", "weight": 999.0})
	if v := CheckEdgeProperties(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("checker FAILED to detect an edge-property mismatch")
	}
}
