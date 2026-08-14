package sim

import (
	"context"
	"fmt"
	"slices"
)

// Schema-mutation templates. They mutate the LABELS and PROPERTIES of existing
// Person nodes (matched by their logical key, the unique name), exercising the
// mutation clauses the earlier DST did not drive: REMOVE property / REMOVE label
// (CY1), SET label / SET n += $map / SET n = $map (CY2), and — via the checker's
// (n:Person:Vip) probe — multi-label matching (CY14). Person is reused (rather
// than a bespoke label) so the shared oracle's name-keyed node model and the
// crash-durability machinery apply unchanged: name is always preserved, so no
// mutation can strand a node the durability checker probes by name.
const (
	tmplSetTag       = "MATCH (n:Person {name:$name}) SET n.tag=$tag"
	tmplRemoveTag    = "MATCH (n:Person {name:$name}) REMOVE n.tag"
	tmplAddVip       = "MATCH (n:Person {name:$name}) SET n:Vip"
	tmplRemoveVip    = "MATCH (n:Person {name:$name}) REMOVE n:Vip"
	tmplMergeProps   = "MATCH (n:Person {name:$name}) SET n += $props"
	tmplReplaceProps = "MATCH (n:Person {name:$name}) SET n = $props"
)

// schemaMutationProps are the scalar property keys the checker projects and
// verifies on each Person, in a fixed order. name is the key and is verified
// implicitly by the probe matching on it; age/tag/score are the mutable scalars.
var schemaMutationProps = []string{"age", "tag", "score"}

// SchemaMutationWriter mutates the labels and properties of existing Person
// nodes: it removes a property (REMOVE n.tag), removes a label (REMOVE n:Vip),
// adds a label (SET n:Vip), merges a property map (SET n += $props), and
// replaces the property set (SET n = $props). Since rmp #2454 it also drives
// the FOREACH write path with two genuinely different bodies: a per-element
// CREATE over a bound list ([tmplForeachCreatePersons] — the one op family
// through which this writer creates nodes) and a per-element SET on an outer
// MATCH variable ([tmplForeachSetTag]). Apart from the FOREACH create arm it
// never creates or deletes a node (the co-actor HonestWriter keeps the
// population churning), and every op targets a name the oracle already models,
// so it never emits a statement the engine would reject on well-formedness
// grounds.
//
// # Concurrency contract
//
// SchemaMutationWriter is NOT safe for concurrent use; it is invoked from the
// single simulation goroutine.
type SchemaMutationWriter struct{}

// Name returns the actor's identifier.
func (SchemaMutationWriter) Name() string { return "SchemaMutationWriter" }

// NextOp picks a mutation on a seed-chosen existing Person. When the graph is
// empty it creates a Person (nothing to mutate yet), so the op stream is a pure
// function of (seed state, oracle state).
func (w SchemaMutationWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) == 0 {
		return HonestWriter{}.opCreatePerson(seed)
	}
	name := names[seed.IntN(len(names))]
	switch seed.IntN(8) {
	case 0:
		return Op{Kind: OpUpdate, Cypher: tmplSetTag, Params: map[string]any{"name": name, "tag": fmt.Sprintf("t%d", seed.IntN(1000))}}
	case 1:
		return Op{Kind: OpUpdate, Cypher: tmplRemoveTag, Params: map[string]any{"name": name}}
	case 2:
		return Op{Kind: OpUpdate, Cypher: tmplAddVip, Params: map[string]any{"name": name}}
	case 3:
		return Op{Kind: OpUpdate, Cypher: tmplRemoveVip, Params: map[string]any{"name": name}}
	case 4:
		return Op{Kind: OpUpdate, Cypher: tmplMergeProps, Params: map[string]any{
			"name": name, "props": map[string]any{"score": int64(seed.IntN(1000))}}}
	case 5:
		return w.opForeachCreate(seed)
	case 6:
		return w.opForeachSetTag(seed, names)
	default:
		// SET n = $props REPLACES the property set — it must carry name (the key)
		// and age so the node stays matchable and the durability probe keeps working.
		return Op{Kind: OpUpdate, Cypher: tmplReplaceProps, Params: map[string]any{
			"name": name, "props": map[string]any{"name": name, "age": int64(seed.IntN(100))}}}
	}
}

// applySchemaMutation advances the shared oracle for a schema-mutation template,
// updating the matched Person's modelled Labels/Properties. It is invoked from
// [GraphOracle.ApplyMatch]. A name miss (the Person was deleted) is a committed
// no-op, exactly as the engine's MATCH-found-nothing behaves. It returns true
// when the template was one of ours (so ApplyMatch can stop), false otherwise.
func (o *GraphOracle) applySchemaMutation(cypher string, params map[string]any) (OracleResult, bool) {
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{}, false
	}
	id, found := o.byName[name]
	switch cypher {
	case tmplSetTag:
		if found {
			o.nodes[id].Properties["tag"] = params["tag"]
		}
		return OracleResult{Committed: true}, true
	case tmplRemoveTag:
		if found {
			delete(o.nodes[id].Properties, "tag")
		}
		return OracleResult{Committed: true}, true
	case tmplAddVip:
		if found {
			o.addLabel(id, "Vip")
		}
		return OracleResult{Committed: true}, true
	case tmplRemoveVip:
		if found {
			o.removeLabel(id, "Vip")
		}
		return OracleResult{Committed: true}, true
	case tmplMergeProps:
		if found {
			for k, v := range params["props"].(map[string]any) {
				o.nodes[id].Properties[k] = v
			}
		}
		return OracleResult{Committed: true}, true
	case tmplReplaceProps:
		if found {
			np := make(map[string]any)
			for k, v := range params["props"].(map[string]any) {
				np[k] = v
			}
			o.nodes[id].Properties = np
		}
		return OracleResult{Committed: true}, true
	}
	return OracleResult{}, false
}

// addLabel appends label to the node's Labels if not already present.
func (o *GraphOracle) addLabel(id uint64, label string) {
	n := o.nodes[id]
	if !slices.Contains(n.Labels, label) {
		n.Labels = append(n.Labels, label)
	}
}

// removeLabel drops label from the node's Labels if present.
func (o *GraphOracle) removeLabel(id uint64, label string) {
	n := o.nodes[id]
	if i := slices.Index(n.Labels, label); i >= 0 {
		n.Labels = slices.Delete(n.Labels, i, i+1)
	}
}

// CheckSchemaMutation reads every modelled Person back through the real engine
// and asserts each scalar property equals its modelled value (a removed/never-set
// property reads NULL — CY1) and that the Vip label membership matches the model,
// probed via the multi-label pattern (n:Person:Vip {name}) (CY14). Running it on
// the quiescent graph, including immediately after crash/recovery, verifies every
// REMOVE / SET-label / SET-map mutation round-trips and survives WAL + snapshot
// recovery. It reads the shared oracle's own NodeState, so it needs no second
// model.
func CheckSchemaMutation(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation

	proj := "n." + schemaMutationProps[0]
	for _, k := range schemaMutationProps[1:] {
		proj += ", n." + k
	}
	for _, name := range oracle.NodeNames() {
		id := oracle.byName[name]
		n := oracle.nodes[id]
		if n == nil || !hasLabel(n, "Person") {
			continue
		}
		q := fmt.Sprintf("MATCH (n:Person {name:'%s'}) RETURN %s", name, proj)
		got, err := engine.projectRowStrings(ctx, q, len(schemaMutationProps))
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "person property read",
				Message: fmt.Sprintf("Person{name:%q}: read failed: %v", name, err)})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{Kind: ViolationACIDDurability, Tick: tick, Op: "person existence",
				Message: fmt.Sprintf("committed Person{name:%q} absent (did not survive recovery)", name)})
			continue
		}
		for i, k := range schemaMutationProps {
			want := "null"
			if v, ok := n.Properties[k]; ok {
				want = canonicalValueString(v)
			}
			if got[i] != want {
				vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "person property value",
					Message: fmt.Sprintf("Person{name:%q}.%s = %s, want %s (REMOVE/SET-map did not round-trip)", name, k, got[i], want)})
			}
		}

		// Vip label membership via a multi-label pattern (CY14).
		vipGot, err := engine.scalarCount(fmt.Sprintf("MATCH (n:Person:Vip {name:'%s'}) RETURN count(n)", name))
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "person label read",
				Message: fmt.Sprintf("Person{name:%q}: label query error: %v", name, err)})
			continue
		}
		want := int64(0)
		if hasLabel(n, "Vip") {
			want = 1
		}
		if vipGot != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "person label",
				Message: fmt.Sprintf("Person{name:%q} :Vip membership: engine=%d, want=%d (SET/REMOVE label did not round-trip)", name, vipGot, want)})
		}
	}
	return vs
}

// schemaMutationWorkload mixes HonestWriter (which creates/links/deletes Persons,
// keeping the population and the shared oracle churning) with SchemaMutationWriter
// (which mutates their labels and properties). A write-biased mix keeps a real
// population under continuous mutation.
func schemaMutationWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{HonestWriter{}, SchemaMutationWriter{}},
		Weights: []float64{0.4, 0.6},
	}
}

// schemaMutationCheckEvery is the periodic schema-mutation-check cadence.
const schemaMutationCheckEvery = 60

// schemaMutationScenario verifies the schema-mutation clause surface under the
// DST: the workload exercises REMOVE property, REMOVE label, SET label,
// SET n += $map, and SET n = $map on Person nodes — plus, since rmp #2454, the
// FOREACH write path (per-element CREATE over a bound list and per-element SET
// on an outer variable, each modelled by the oracle as its expansion) — and
// [CheckSchemaMutation] confirms each mutation round-trips and — with
// crash+checkpoint injected — survives both WAL and snapshot recovery. A
// terminal gate ([checkForeachNonVacuity]) proves the FOREACH templates were
// actually issued and exercised through recovery. It is bit-reproducible.
func schemaMutationScenario() Scenario {
	return Scenario{
		Name:        ScenarioSchemaMutation,
		Description: "schema mutation: REMOVE prop/label, SET label, SET n += $map, SET n = $map, FOREACH create/set expansion + multi-label match; survives crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5C4E3A17,
		MaxTicks:    500,
		Workload:    schemaMutationWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 40},
		run:         runSchemaMutation,
	}
}

// runSchemaMutation drives the schema-mutation safety loop: it churns Person
// labels/properties, checkpoints and crashes per the schedule, and runs the
// shared parity check plus [CheckSchemaMutation] periodically, immediately after
// every crash/recovery (the DST-unique value — the mutations are validated
// against a graph that survived recovery), and once at the end, followed by the
// FOREACH non-vacuity gate (rmp #2454). Deterministic.
func runSchemaMutation(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := schemaMutationScenario()
	return runSchemaMutationCfg(ctx, sc.DeterministicConfig(seed))
}

// runSchemaMutationCfg is [runSchemaMutation] over an explicit [Config], split
// out so tests can prove the terminal FOREACH non-vacuity gate is wired: a
// config whose budget or crash schedule cannot satisfy the gate must yield a
// violation report, not a silent pass.
func runSchemaMutationCfg(ctx context.Context, cfg Config) (*SimReport, error) {
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: schema-mutation new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	foreach := newForeachStats()
	var lastTick int64
	var lastOp Op
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		if err := sm.maybeCheckpoint(tick); err != nil {
			return nil, err
		}
		crashesBefore := sm.crashCount
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		if sm.crashCount > crashesBefore {
			foreach.noteRecovery(sm.oracle)
			if v := CheckSchemaMutation(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery schema-mutation>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		foreach.noteOp(op, committed)
		// Per-op counters oracle (#2448): every committed mutation's effect report
		// (SET/REMOVE property, SET/REMOVE label, SET-map, FOREACH expansion) must
		// match the effect the oracle predicts, adjudicated on the pre-apply model.
		if v := CheckOpCounters(tick, op, committed, counters, sm.oracle); len(v) > 0 {
			return sm.report(tick, op, v), nil
		}
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
		if tick%schemaMutationCheckEvery == 0 {
			if v := CheckSchemaMutation(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckSchemaMutation(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	// Assert-something-was-seen (rmp #2454): both FOREACH templates were issued
	// and FOREACH-written state was exercised through crash/recovery.
	if v := checkForeachNonVacuity(lastTick, foreach); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
