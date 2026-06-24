package sim

import (
	"context"
	"fmt"
)

// SurfaceWriter builds the graph the cypher-surface battery reads: it creates
// Person nodes that ALWAYS carry name+age (so aggregate/filter invariants have
// no null-age ambiguity) and KNOWS edges between existing Persons. It avoids
// MERGE and SET so the oracle's Person/age model is unambiguous.
//
// # Concurrency contract
//
// SurfaceWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type SurfaceWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*SurfaceWriter) Name() string { return "SurfaceWriter" }

// NextOp returns a fresh Person CREATE or a KNOWS edge between two existing
// Persons, seed-derived.
func (w *SurfaceWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) >= 2 && seed.Float64() < 0.4 {
		a := names[seed.IntN(len(names))]
		b := names[seed.IntN(len(names))]
		return Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": a, "b": b}}
	}
	name := fmt.Sprintf("s%d", w.counter)
	w.counter++
	return Op{Kind: OpCreate, Cypher: tmplCreatePerson, Params: map[string]any{"name": name, "age": int64(seed.IntN(100))}}
}

// surfaceWorkload is a 100% SurfaceWriter mix.
func surfaceWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&SurfaceWriter{}}, Weights: []float64{1.0}}
}

// CheckCypherSurface runs a battery of diverse read queries — aggregation
// (count/sum), WHERE and WITH...WHERE filters, a pattern-count, UNWIND over
// range(), OPTIONAL MATCH, and ORDER BY — and asserts each result matches an
// invariant computed independently from the oracle model. It broadens the DST's
// coverage of the Cypher read surface beyond the minimal per-tick parity probe,
// comparing result INVARIANTS (scalar values, the sorted-name sequence) rather
// than plan-specific row order where it is not determined.
func CheckCypherSurface(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation

	scalar := func(label, query string, want int64) {
		res, err := engine.Run(ctx, query, nil)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s query error: %v", label, err)})
			return
		}
		var got int64
		if res.Next() {
			if v, ok := res.ScalarInt(); ok {
				got = v
			}
		}
		derr := res.Err()
		_ = res.Close()
		if derr != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s drain error: %v", label, derr)})
			return
		}
		if got != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s: engine=%d, oracle invariant=%d", label, got, want)})
		}
	}

	ages := oracle.personAges()
	var sum, geHalf, ltThirty int64
	for _, a := range ages {
		sum += a
		if a >= 50 {
			geHalf++
		}
		if a < 30 {
			ltThirty++
		}
	}
	knows := int64(oracle.knowsCount())

	scalar("count(Person)", "MATCH (n:Person) RETURN count(n)", int64(oracle.personCount()))
	scalar("WHERE n.age>=50 count", "MATCH (n:Person) WHERE n.age >= 50 RETURN count(n)", geHalf)
	scalar("sum(n.age)", "MATCH (n:Person) RETURN sum(n.age)", sum)
	scalar("WITH...WHERE n.age<30 count", "MATCH (n:Person) WITH n WHERE n.age < 30 RETURN count(n)", ltThirty)
	scalar("count(KNOWS)", "MATCH ()-[r:KNOWS]->() RETURN count(r)", knows)
	scalar("pattern count(*)", "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN count(*)", knows)
	scalar("OPTIONAL MATCH count(m)", "MATCH (n:Person) OPTIONAL MATCH (n)-[:KNOWS]->(m) RETURN count(m)", knows)
	scalar("UNWIND range count", "UNWIND range(1, 25) AS x RETURN count(x)", 25)

	// ORDER BY: the projected name sequence must equal the oracle's sorted names.
	wantNames := oracle.personNamesSorted()
	res, err := engine.Run(ctx, "MATCH (n:Person) RETURN n.name ORDER BY n.name", nil)
	if err != nil {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "ORDER BY n.name",
			Message: fmt.Sprintf("ORDER BY query error: %v", err)})
		return vs
	}
	var gotNames []string
	for res.Next() {
		if s, ok := res.StringAt(0); ok {
			gotNames = append(gotNames, s)
		}
	}
	derr := res.Err()
	_ = res.Close()
	if derr != nil {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "ORDER BY n.name",
			Message: fmt.Sprintf("ORDER BY drain error: %v", derr)})
		return vs
	}
	if len(gotNames) != len(wantNames) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "ORDER BY n.name",
			Message: fmt.Sprintf("ORDER BY row count: engine=%d, oracle=%d", len(gotNames), len(wantNames))})
		return vs
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "ORDER BY n.name",
				Message: fmt.Sprintf("ORDER BY mismatch at row %d: engine=%q oracle=%q", i, gotNames[i], wantNames[i])})
			break
		}
	}
	return vs
}

// cypherSurfaceScenario broadens the DST's Cypher read-surface coverage: a
// workload builds a Person/KNOWS graph, and [CheckCypherSurface] verifies a
// battery of diverse read shapes (count/sum aggregation, WHERE, WITH...WHERE,
// pattern-count, OPTIONAL MATCH, UNWIND range, ORDER BY) against
// independently-computed oracle invariants, periodically, after each
// crash/recovery, and at the end. It is bit-reproducible.
func cypherSurfaceScenario() Scenario {
	return Scenario{
		Name:        ScenarioCypherSurface,
		Description: "broad Cypher read surface (aggregation/WHERE/WITH/OPTIONAL MATCH/UNWIND/ORDER BY) vs oracle invariants",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5C0FACE,
		MaxTicks:    500,
		Workload:    surfaceWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		run:         runCypherSurface,
	}
}

// cypherSurfaceCheckEvery is the periodic surface-battery cadence.
const cypherSurfaceCheckEvery = 70

// runCypherSurface drives the cypher-surface safety loop, running the read
// battery periodically, after each crash/recovery, and at the end. Deterministic.
func runCypherSurface(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := cypherSurfaceScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: cypher-surface new: %w", err)
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
			if v := CheckCypherSurface(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery surface>"}, v), nil
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
		if tick%cypherSurfaceCheckEvery == 0 {
			if v := CheckCypherSurface(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckCypherSurface(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
