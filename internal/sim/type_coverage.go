package sim

import (
	"context"
	"fmt"
	"strings"
)

// TypedWriter is the type-coverage actor: it emits [tmplCreateTyped] CREATEs
// whose parameters span every round-tripping Cypher property kind — string,
// integer, float, boolean, list, and an ISO-8601 temporal string — with values
// drawn deterministically from the seed. The oracle records the full property
// set so the type-coverage checker can verify each kind survives commit and
// crash/recovery.
//
// # Concurrency contract
//
// TypedWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type TypedWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*TypedWriter) Name() string { return "TypedWriter" }

// NextOp returns the next Typed-node CREATE with a unique id and a value of every
// supported kind, all seed-derived so the op stream is a pure function of the
// seed. A pointer receiver carries the monotone id counter across calls.
func (w *TypedWriter) NextOp(seed *Seed, _ *GraphOracle) Op {
	id := w.counter
	w.counter++
	// ISO-8601 temporal string (the canonical Cypher-visible temporal storage):
	// a deterministic instant derived from the id.
	ts := fmt.Sprintf("2026-01-%02dT%02d:%02d:00Z", 1+int(id%28), int(id%24), int(seed.IntN(60)))
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreateTyped,
		Params: map[string]any{
			"id":  id,
			"s":   fmt.Sprintf("str-%d", id),
			"i":   int64(seed.IntN(1_000_000)),
			"f":   float64(seed.IntN(1_000_000)) / 1000.0,
			"b":   seed.IntN(2) == 0,
			"lst": []any{id, int64(seed.IntN(100)), int64(seed.IntN(100))},
			"ts":  ts,
		},
	}
}

// typeCoverageWorkload is a 100% TypedWriter mix: every op creates a Typed node
// carrying a value of each supported kind.
func typeCoverageWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&TypedWriter{}}, Weights: []float64{1.0}}
}

// CheckTypedProperties reads every modelled Typed node back through the real
// engine read path and asserts each property round-trips to its modelled value
// (compared via the canonical expr.Value String() rendering, so the comparison
// is type-aware across string/int/float/bool/list/temporal), that a never-set
// property reads NULL, and that the node exists at all. It runs on a quiescent
// graph (the deterministic loop, including immediately after crash/recovery), so
// a divergence means a property failed to round-trip or did not survive recovery.
func CheckTypedProperties(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	var vs []Violation
	cols := make([]string, len(typedPropKeys))
	for i, k := range typedPropKeys {
		cols[i] = "n." + k
	}
	proj := strings.Join(cols, ", ")
	ctx := context.Background()

	for _, id := range oracle.TypedIDs() {
		props, ok := oracle.TypedNode(id)
		if !ok {
			continue
		}
		q := fmt.Sprintf("MATCH (n:Typed {id:%d}) RETURN %s", id, proj)
		got, err := engine.projectRowStrings(ctx, q, len(typedPropKeys))
		if err != nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed property read",
				Message: fmt.Sprintf("Typed{id:%d}: read failed: %v", id, err),
			})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Tick: tick, Op: "typed node existence",
				Message: fmt.Sprintf("committed Typed{id:%d} absent in engine (did not survive recovery)", id),
			})
			continue
		}
		for i, k := range typedPropKeys {
			want := "null" // "absent" is never set, and any modelled-nil reads NULL
			if k != "absent" {
				want = canonicalValueString(props[k])
			}
			if got[i] != want {
				vs = append(vs, Violation{
					Kind: ViolationOracleDeviation, Tick: tick, Op: "typed property value",
					Message: fmt.Sprintf("Typed{id:%d}.%s = %s, want %s (kind did not round-trip)", id, k, got[i], want),
				})
			}
		}
	}
	return vs
}

// typeCoverageScenario verifies the property type system under the DST: a
// workload creates Typed nodes carrying a value of every round-tripping kind
// (string/int/float/bool/list/temporal-string + a NULL-reading absent key), and
// [CheckTypedProperties] confirms each kind round-trips and — with crash/recovery
// injected — survives WAL recovery. It is bit-reproducible.
func typeCoverageScenario() Scenario {
	return Scenario{
		Name:        ScenarioTypeCoverage,
		Description: "property type system: string/int/float/bool/list/temporal round-trip + survive crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x7A9E5,
		MaxTicks:    400,
		Workload:    typeCoverageWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
		run:         runTypeCoverage,
	}
}

// typeCoverageCheckEvery is the tick cadence for the periodic typed-property
// check inside [runTypeCoverage].
const typeCoverageCheckEvery = 60

// runTypeCoverage drives the type-coverage safety loop: it creates Typed nodes,
// checks property round-trip periodically and immediately after every
// crash/recovery (the DST-unique value — the kinds are validated against a graph
// that survived WAL recovery), plus a terminal check. It is deterministic.
func runTypeCoverage(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := typeCoverageScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: type-coverage new: %w", err)
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
			// Validate every typed property against the crash-recovered graph.
			if v := CheckTypedProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery typed check>"}, v), nil
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
		if tick%typeCoverageCheckEvery == 0 {
			if v := CheckTypedProperties(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	// Terminal typed check.
	if v := CheckTypedProperties(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
