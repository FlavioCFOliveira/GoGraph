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
// implicitly by the probe matching on it; age/tag/score are the mutable
// scalars. Since rmp #2461 the list also carries mc, the hit counter the
// two-branch node MERGE ([tmplMergePersonCounter]) maintains: projecting it on
// every probe is what makes the ON CREATE/ON MATCH counter value round-trip
// continuously — and, because the probe re-runs immediately after every
// crash/recovery, survive the WAL and the snapshot.
var schemaMutationProps = []string{"age", "tag", "score", "mc"}

// SchemaMutationWriter mutates the labels and properties of existing Person
// nodes: it removes a property (REMOVE n.tag), removes a label (REMOVE n:Vip),
// adds a label (SET n:Vip), merges a property map (SET n += $props), and
// replaces the property set (SET n = $props). Since rmp #2454 it also drives
// the FOREACH write path with two genuinely different bodies: a per-element
// CREATE over a bound list ([tmplForeachCreatePersons] — the one op family
// through which this writer creates nodes) and a per-element SET on an outer
// MATCH variable ([tmplForeachSetTag]).
//
// Since rmp #2461 it additionally drives the four MERGE families the DST did
// not reach: a node MERGE with BOTH action branches
// ([tmplMergePersonCounter]), the whole-map ON CREATE assignment
// ([tmplMergePersonSetAll]), a whole-pattern MERGE that creates both endpoints
// and the relationship together ([tmplMergePairPattern]), and the
// map-parameter MERGE the engine must REJECT ([tmplMergeParamMap]). The first
// three create nodes, so the writer is no longer create-free; the last is the
// one statement it emits deliberately expecting an engine error, and it is
// modelled as an [OpMalformed] no-op for exactly that reason. Every other op
// targets a name the oracle already models, so it emits nothing the engine
// would reject on well-formedness grounds. The co-actor HonestWriter keeps the
// population churning and is still the only actor that deletes.
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
	switch seed.IntN(13) {
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
	case 7:
		return w.opMergeCounter(seed, names)
	case 8:
		return w.opMergeSetAll(seed, names)
	case 9:
		return w.opMergePairPattern(seed)
	case 10:
		return w.opMergeParamMap(seed)
	case 11:
		return w.opMergePairSetAll(seed)
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
//
// It also runs [CheckMergePairRelProps], so the whole-entity relationship write
// of the MERGE-surface family (rmp #2510) is verified at every one of this
// check's call sites — periodically, immediately after every crash/recovery, and
// once at the end — rather than needing its own schedule. The PAIRED endpoints
// are deliberately absent from the name index this function's Person loop walks,
// so the two probes cover disjoint state.
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
	return append(vs, CheckMergePairRelProps(tick, oracle, engine)...)
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
// on an outer variable, each modelled by the oracle as its expansion), and
// since rmp #2461 the four MERGE families the DST did not previously reach (a
// node MERGE with both action branches, ON CREATE SET n = $map, a whole-pattern
// MERGE, and the map-parameter MERGE the engine must reject) — and
// [CheckSchemaMutation] confirms each mutation round-trips and — with
// crash+checkpoint injected — survives both WAL and snapshot recovery. Two
// terminal gates ([checkForeachNonVacuity] and [checkMergeSurfaceNonVacuity])
// prove the added templates were actually issued, that each of their branches
// and sub-cases fired, and that the state they wrote was exercised through
// recovery. It is bit-reproducible.
func schemaMutationScenario() Scenario {
	return Scenario{
		Name:        ScenarioSchemaMutation,
		Description: "schema mutation: REMOVE prop/label, SET label, SET n += $map, SET n = $map, FOREACH create/set expansion, MERGE surface (both action branches, ON CREATE SET n = $map, whole-pattern, rejected map param) + multi-label match; survives crash/recovery",
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
	merges := newMergeSurfaceStats()
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
			merges.noteRecovery(sm.oracle)
			if v := CheckSchemaMutation(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery schema-mutation>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		foreach.noteOp(op, committed)
		// Both stats records read the PRE-apply model, so they must be updated
		// before applyToOracle advances it (rmp #2454, #2461).
		merges.noteOp(op, committed, sm.oracle)
		// The map-parameter MERGE must be rejected by the engine, never applied
		// (rmp #2461); an acceptance is a deviation on the tick that caused it.
		if v := checkMergeRejection(tick, op, committed); len(v) > 0 {
			return sm.report(tick, op, v), nil
		}
		// Per-op counters oracle (#2448): every committed mutation's effect report
		// (SET/REMOVE property, SET/REMOVE label, SET-map, FOREACH expansion, and
		// since #2461 each MERGE family's create-vs-match adjudication) must match
		// the effect the oracle predicts, adjudicated on the pre-apply model.
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
	// Assert-something-was-seen (rmp #2454, #2461): both FOREACH templates and
	// all four MERGE families were issued, each family's branches and sub-cases
	// actually fired, and the state they wrote was exercised through
	// crash/recovery. The two gates are evaluated together and their violations
	// concatenated, so neither can mask the other.
	terminal := checkForeachNonVacuity(lastTick, foreach)
	terminal = append(terminal, checkMergeSurfaceNonVacuity(lastTick, merges)...)
	if len(terminal) > 0 {
		return sm.report(lastTick, lastOp, terminal), nil
	}
	return nil, nil
}
