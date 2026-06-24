package sim

import (
	"context"
	"fmt"
	"strings"
)

// EdgePropsWriter is the edge-property coverage actor: it grows a Person
// population and links pairs with KNOWS edges carrying an ISO-8601 `since`
// string and a float `weight`, so the columnar edge-property tier is exercised
// through the Cypher write+read path. It only creates an edge for a pair that
// has no existing KNOWS edge (a simple-graph re-CREATE is a no-op that would
// desync properties), falling back to a fresh Person otherwise.
//
// # Concurrency contract
//
// EdgePropsWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type EdgePropsWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*EdgePropsWriter) Name() string { return "EdgePropsWriter" }

// NextOp returns either a fresh Person CREATE or a KNOWS-with-properties edge
// between two existing, not-yet-linked Persons, all seed-derived.
func (w *EdgePropsWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	makeEdge := len(names) >= 2 && seed.Float64() < 0.5
	if makeEdge {
		a := names[seed.IntN(len(names))]
		b := names[seed.IntN(len(names))]
		if a != b && !oracle.HasKnowsByName(a, b) {
			since := fmt.Sprintf("2026-%02d-%02d", 1+int(seed.IntN(12)), 1+int(seed.IntN(28)))
			weight := float64(seed.IntN(100000)) / 100.0
			return Op{Kind: OpCreate, Cypher: tmplCreateKnowsProps,
				Params: map[string]any{"a": a, "b": b, "since": since, "weight": weight}}
		}
	}
	name := fmt.Sprintf("ep%d", w.counter)
	w.counter++
	return Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": name, "age": int64(seed.IntN(100))}}
}

// edgePropertiesWorkload is a 100% EdgePropsWriter mix.
func edgePropertiesWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&EdgePropsWriter{}}, Weights: []float64{1.0}}
}

// CheckEdgeProperties reads every modelled KNOWS edge's properties back through
// the real engine read path and asserts each round-trips to its modelled value
// (canonical type-aware compare) and that the edge still exists. Run on a
// quiescent graph including immediately after crash/recovery, so edge properties
// are validated against a WAL-recovered (and columnar-tier-rebuilt) graph.
func CheckEdgeProperties(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	var vs []Violation
	cols := make([]string, len(knowsEdgePropKeys))
	for i, k := range knowsEdgePropKeys {
		cols[i] = "r." + k
	}
	proj := strings.Join(cols, ", ")
	ctx := context.Background()

	for _, e := range oracle.KnowsEdgesByName() {
		if e.Src == "" || e.Dst == "" {
			continue
		}
		q := fmt.Sprintf(
			"MATCH (:Person {name:'%s'})-[r:KNOWS]->(:Person {name:'%s'}) RETURN %s",
			e.Src, e.Dst, proj)
		got, err := engine.projectRowStrings(ctx, q, len(knowsEdgePropKeys))
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "edge property read",
				Message: fmt.Sprintf("KNOWS(%q->%q): read failed: %v", e.Src, e.Dst, err)})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{Kind: ViolationACIDDurability, Tick: tick, Op: "edge existence",
				Message: fmt.Sprintf("committed KNOWS(%q->%q) absent in engine (did not survive recovery)", e.Src, e.Dst)})
			continue
		}
		for i, k := range knowsEdgePropKeys {
			want := canonicalValueString(e.Props[k])
			if got[i] != want {
				vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "edge property value",
					Message: fmt.Sprintf("KNOWS(%q->%q).%s = %s, want %s", e.Src, e.Dst, k, got[i], want)})
			}
		}
	}
	return vs
}

// edgePropertiesScenario verifies edge properties under the DST: a workload
// creates KNOWS edges carrying `since` (ISO string) and `weight` (float), and
// [CheckEdgeProperties] confirms each round-trips and survives crash/recovery
// (exercising the columnar edge-property tier through the Cypher read path). It
// is bit-reproducible.
func edgePropertiesScenario() Scenario {
	return Scenario{
		Name:        ScenarioEdgeProperties,
		Description: "edge properties (KNOWS.since/weight) round-trip + survive crash/recovery (columnar edge tier)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xED9E9405,
		MaxTicks:    500,
		Workload:    edgePropertiesWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		run:         runEdgeProperties,
	}
}

// edgePropertiesCheckEvery is the periodic edge-property check cadence.
const edgePropertiesCheckEvery = 80

// runEdgeProperties drives the edge-property safety loop: it builds KNOWS edges
// with properties and runs [CheckEdgeProperties] periodically, after every
// crash/recovery, and once at the end. It is deterministic.
func runEdgeProperties(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: edge-properties new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	var lastTick int64
	var lastOp Op
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		crashesBefore := sm.crashCount
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		if sm.crashCount > crashesBefore {
			if v := CheckEdgeProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery edge props>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
		if tick%edgePropertiesCheckEvery == 0 {
			if v := CheckEdgeProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckEdgeProperties(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
