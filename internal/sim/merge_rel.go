package sim

import (
	"context"
	"fmt"
)

// tmplMergeKnowsN merges a KNOWS edge between two existing Persons, initialising
// a hit counter r.n to 1 on creation and incrementing it on every subsequent
// match. Re-running it for the same (a,b) pair must be idempotent on the edge
// count (MERGE matches the existing edge rather than creating a parallel one) and
// must increment r.n — the canonical MERGE-relationship + ON CREATE/ON MATCH SET
// invariant (CY3).
const tmplMergeKnowsN = "MATCH (a:Person {name:$a}),(b:Person {name:$b}) MERGE (a)-[r:KNOWS]->(b) ON CREATE SET r.n=1 ON MATCH SET r.n=r.n+1"

// MergeRelWriter builds a Person population and repeatedly MERGEs KNOWS edges
// between existing Persons with a hit counter. Because every KNOWS edge in this
// scenario is born through MERGE (never a bare CREATE), r.n is always initialised
// before it is incremented, so the counter invariant is well-defined. It never
// deletes, so the edge set only grows or re-hits.
//
// # Concurrency contract
//
// MergeRelWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type MergeRelWriter struct{}

// Name returns the actor's identifier.
func (MergeRelWriter) Name() string { return "MergeRelWriter" }

// NextOp creates a Person when there are fewer than two to link (or one draw in
// three, to keep the population growing), otherwise MERGEs a KNOWS edge between
// two seed-chosen existing Persons. The op is a pure function of (seed, oracle).
func (MergeRelWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) < 2 || seed.IntN(3) == 0 {
		return HonestWriter{}.opCreatePerson(seed)
	}
	a := names[seed.IntN(len(names))]
	b := names[seed.IntN(len(names))]
	return Op{Kind: OpMerge, Cypher: tmplMergeKnowsN, Params: map[string]any{"a": a, "b": b}}
}

// applyMergeKnowsN models [tmplMergeKnowsN] in the shared oracle: with both
// endpoints present it creates the KNOWS edge with n=1 on first sight and
// increments n on every re-merge; a missing endpoint is a committed no-op (the
// MATCH found nothing, so MERGE ran zero times). A self-merge (a==b) is a
// well-defined self-loop the engine supports, modelled identically.
func (o *GraphOracle) applyMergeKnowsN(params map[string]any) OracleResult {
	a, _ := paramString(params, "a")
	b, _ := paramString(params, "b")
	srcID, srcOK := o.byName[a]
	dstID, dstOK := o.byName[b]
	if !srcOK || !dstOK {
		return OracleResult{Committed: true} // MATCH found nothing.
	}
	k := edgeKey{src: srcID, dst: dstID, label: "KNOWS"}
	if e, exists := o.edges[k]; exists {
		//nolint:forcetypeassert // the oracle writes Properties["n"] only as int64 (the MERGE creation path in this file), so the ON MATCH increment reads back an int64
		e.Properties["n"] = e.Properties["n"].(int64) + 1 // ON MATCH SET r.n=r.n+1
		return OracleResult{Committed: true}
	}
	o.edges[k] = &EdgeState{SrcID: srcID, DstID: dstID, Label: "KNOWS", Properties: map[string]any{"n": int64(1)}}
	return OracleResult{Committed: true, EdgesCreated: 1}
}

// CheckMergeRel reads every modelled KNOWS edge's r.n counter back through the
// real engine and asserts it equals the modelled hit count. Running it on the
// quiescent graph, including immediately after crash/recovery, verifies the
// MERGE-relationship idempotency (edge-count parity is covered by the shared
// durability check) and that ON CREATE/ON MATCH SET counter updates round-trip
// and survive WAL + snapshot recovery.
func CheckMergeRel(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	for _, e := range oracle.KnowsEdgesByName() {
		nv, ok := e.Props["n"]
		if !ok {
			continue
		}
		//nolint:forcetypeassert // nv comes from the same oracle map this file populates exclusively with int64 counters
		want := nv.(int64)
		q := fmt.Sprintf("MATCH (a:Person {name:'%s'})-[r:KNOWS]->(b:Person {name:'%s'}) RETURN r.n", e.Src, e.Dst)
		got, err := engine.projectRowStrings(ctx, q, 1)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "merge-rel read",
				Message: fmt.Sprintf("KNOWS(%q->%q).n read failed: %v", e.Src, e.Dst, err)})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{Kind: ViolationACIDDurability, Tick: tick, Op: "merge-rel existence",
				Message: fmt.Sprintf("committed KNOWS(%q->%q) absent (did not survive recovery)", e.Src, e.Dst)})
			continue
		}
		if got[0] != fmt.Sprintf("%d", want) {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "merge-rel counter",
				Message: fmt.Sprintf("KNOWS(%q->%q).n = %s, want %d (ON CREATE/ON MATCH SET did not round-trip)", e.Src, e.Dst, got[0], want)})
		}
	}
	return vs
}

// mergeRelWorkload is a 100% MergeRelWriter mix.
func mergeRelWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{MergeRelWriter{}}, Weights: []float64{1.0}}
}

// mergeRelCheckEvery is the periodic merge-rel-check cadence.
const mergeRelCheckEvery = 60

// mergeRelScenario verifies the MERGE-relationship + ON CREATE/ON MATCH SET
// surface under the DST: the workload repeatedly MERGEs KNOWS edges with a hit
// counter, and [CheckMergeRel] confirms the counter round-trips and — with
// crash+checkpoint injected — survives both WAL and snapshot recovery. Edge-count
// parity (idempotency) is enforced by the shared durability check. Bit-reproducible.
func mergeRelScenario() Scenario {
	return Scenario{
		Name:        ScenarioMergeRel,
		Description: "MERGE (a)-[r:KNOWS]->(b) ON CREATE/ON MATCH SET counter: idempotent edge count + counter survives crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x3E4EE10,
		MaxTicks:    500,
		Workload:    mergeRelWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 40},
		run:         runMergeRel,
	}
}

// runMergeRel drives the merge-rel safety loop: it MERGEs KNOWS edges with a hit
// counter, checkpoints and crashes per the schedule, and runs the shared parity
// check plus [CheckMergeRel] periodically, immediately after every
// crash/recovery, and once at the end. Deterministic and replayable.
func runMergeRel(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := mergeRelScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: merge-rel new: %w", err)
	}
	defer func() { _ = sm.Close() }()

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
			if v := CheckMergeRel(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery merge-rel>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		// Per-op counters oracle (#2448): a MERGE that created must report the
		// edge + ON CREATE SET effect, a MERGE that matched exactly the ON MATCH
		// SET assignment — adjudicated on the pre-apply model.
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
		if tick%mergeRelCheckEvery == 0 {
			if v := CheckMergeRel(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckMergeRel(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
