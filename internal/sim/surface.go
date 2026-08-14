package sim

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// checkSurfaceAll runs the full Cypher-surface battery: the original read-shape
// probes ([CheckCypherSurface]), the extended read-clause/aggregation/subquery/
// procedure probes ([CheckCypherSurfaceExtended]), the grouped-aggregation /
// DISTINCT-row / UNION-over-data probes ([CheckCypherSurfaceGrouped]), the
// entity-valued function and path-materialisation probes
// ([CheckCypherSurfaceEntity]), the graph-independent expression battery
// ([CheckExprLiterals]), and the non-deterministic function invariants
// ([CheckNonDeterministicFuncs]). It is the single entry point the scenario
// calls periodically, after each crash/recovery, and at the end.
func checkSurfaceAll(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	vs := make([]Violation, 0, 8)
	vs = append(vs, CheckCypherSurface(tick, oracle, engine)...)
	vs = append(vs, CheckCypherSurfaceExtended(tick, oracle, engine)...)
	vs = append(vs, CheckCypherSurfaceGrouped(tick, oracle, engine)...)
	vs = append(vs, CheckCypherSurfaceEntity(tick, oracle, engine)...)
	vs = append(vs, CheckExprLiterals(tick, engine)...)
	vs = append(vs, CheckNonDeterministicFuncs(tick, engine)...)
	return vs
}

// surfaceCityVocab is the size of the seeded city vocabulary (c0..c9) the
// surface workload draws from. Small on purpose: with ~10 cities over hundreds
// of Persons every city groups several rows, so grouped aggregation is
// non-trivial (rmp #2452) while the full grouped row set stays tiny.
const surfaceCityVocab = 10

// SurfaceWriter builds the graph the cypher-surface battery reads: it creates
// Person nodes that ALWAYS carry name+age+city (so aggregate/filter/grouping
// invariants have no null ambiguity) and KNOWS edges between existing Persons.
// It avoids MERGE and SET so the oracle's Person model is unambiguous.
//
// # Concurrency contract
//
// SurfaceWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type SurfaceWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*SurfaceWriter) Name() string { return "SurfaceWriter" }

// NextOp returns a fresh Person CREATE (name, age, and a city from the seeded
// c0..c9 vocabulary) or a KNOWS edge between two existing Persons, seed-derived.
func (w *SurfaceWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) >= 2 && seed.Float64() < 0.4 {
		a := names[seed.IntN(len(names))]
		b := names[seed.IntN(len(names))]
		return Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": a, "b": b}}
	}
	name := fmt.Sprintf("s%d", w.counter)
	w.counter++
	return Op{Kind: OpCreate, Cypher: tmplCreatePersonCity, Params: map[string]any{
		"name": name,
		"age":  int64(seed.IntN(100)),
		"city": fmt.Sprintf("c%d", seed.IntN(surfaceCityVocab)),
	}}
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

// CheckCypherSurfaceExtended broadens the Cypher-surface battery with the
// read-clause, expression and procedure shapes the earlier DST did not drive,
// each verified against an invariant computed independently from the oracle
// model over the Person/KNOWS graph: DISTINCT and count(DISTINCT) (CY5); 3VL
// boolean AND, list membership IN, IS NULL and <> (CY6); the STARTS WITH /
// ENDS WITH / CONTAINS / =~ string predicates (CY7); ORDER BY … SKIP … LIMIT
// pagination (CY8); the min/max/sum aggregates plus the EXACT
// avg/stDev/stDevP/percentileCont/percentileDisc oracle-arithmetic probes
// (CY10, rmp #2452 — see [expectedAggregates] for the pinned definitions);
// EXISTS { } / COUNT { } / pattern-comprehension subqueries (CY13); and the
// db.* schema introspection procedures (CY16). It runs on the quiescent graph,
// including after crash/recovery.
func CheckCypherSurfaceExtended(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
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
			got, _ = res.IntAt(0)
		}
		derr := res.Err()
		_ = res.Close()
		if derr == nil && got != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s: engine=%d, oracle=%d", label, got, want)})
		} else if derr != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s drain error: %v", label, derr)})
		}
	}
	ages := oracle.personAges() // sorted, all Persons carry an integer age here
	names := oracle.personNamesSorted()
	pc := int64(oracle.personCount())
	knows := int64(oracle.knowsCount())
	pwok := int64(oracle.personsWithOutgoingKnows())

	var distinct = map[int64]bool{}
	var ge30lt70, inSet, ne50 int64
	for _, a := range ages {
		distinct[a] = true
		if a >= 30 && a < 70 {
			ge30lt70++
		}
		if a == 20 || a == 40 || a == 60 {
			inSet++
		}
		if a != 50 {
			ne50++
		}
	}
	var sum int64
	for _, a := range ages {
		sum += a
	}
	var startsS, contains0, ends5, regexAll int64
	for _, nm := range names {
		if strings.HasPrefix(nm, "s") {
			startsS++
		}
		if strings.Contains(nm, "0") {
			contains0++
		}
		if strings.HasSuffix(nm, "5") {
			ends5++
		}
		if reSName.MatchString(nm) {
			regexAll++
		}
	}

	// CY5 — DISTINCT / count(DISTINCT).
	scalar("count(DISTINCT age)", "MATCH (n:Person) RETURN count(DISTINCT n.age)", int64(len(distinct)))
	// CY6 — 3VL boolean, IN, IS NULL, <>.
	scalar("WHERE age>=30 AND age<70", "MATCH (n:Person) WHERE n.age >= 30 AND n.age < 70 RETURN count(n)", ge30lt70)
	scalar("WHERE age IN [20,40,60]", "MATCH (n:Person) WHERE n.age IN [20,40,60] RETURN count(n)", inSet)
	scalar("WHERE age IS NULL", "MATCH (n:Person) WHERE n.age IS NULL RETURN count(n)", 0)
	scalar("WHERE age <> 50", "MATCH (n:Person) WHERE n.age <> 50 RETURN count(n)", ne50)
	// CY7 — string predicates.
	scalar("STARTS WITH s", "MATCH (n:Person) WHERE n.name STARTS WITH 's' RETURN count(n)", startsS)
	scalar("CONTAINS 0", "MATCH (n:Person) WHERE n.name CONTAINS '0' RETURN count(n)", contains0)
	scalar("ENDS WITH 5", "MATCH (n:Person) WHERE n.name ENDS WITH '5' RETURN count(n)", ends5)
	scalar("=~ ^s[0-9]+$", `MATCH (n:Person) WHERE n.name =~ '^s[0-9]+$' RETURN count(n)`, regexAll)
	// CY10 — aggregations, all exact. min/max/sum as integers; avg / stDev /
	// stDevP / percentileCont / percentileDisc against oracle arithmetic
	// (rmp #2452). The exact probes REPLACE the former boolInvariant
	// self-checks (avg∈[min,max], percentileCont∈[min,max], percentileDisc is
	// a real age): an exact match implies every one of those interval
	// invariants, and the multi-aggregate WITH projection the self-checks
	// exercised is preserved by the five-aggregate probe query itself — so the
	// replacement strictly strengthens the battery.
	if len(ages) > 0 {
		scalar("min(age)", "MATCH (n:Person) RETURN min(n.age)", ages[0])
		scalar("max(age)", "MATCH (n:Person) RETURN max(n.age)", ages[len(ages)-1])
		scalar("sum(age)", "MATCH (n:Person) RETURN sum(n.age)", sum)
	}
	if len(ages) >= 2 { // stDev is NULL for n < 2, so the exact probe needs two rows
		vs = append(vs, compareExactAggregates(tick, expectedAggregates(ages), engine)...)
	}
	// CY13 — EXISTS / COUNT / pattern-comprehension subqueries.
	scalar("EXISTS subquery", "MATCH (n:Person) WHERE EXISTS { (n)-[:KNOWS]->() } RETURN count(n)", pwok)
	scalar("COUNT subquery sum", "MATCH (n:Person) RETURN sum(COUNT { (n)-[:KNOWS]->() })", knows)
	scalar("pattern comprehension", "MATCH (a:Person) RETURN sum(size([(a)-[:KNOWS]->(b) | b]))", knows)

	// CY8 — ORDER BY … SKIP … LIMIT pagination against the sorted name slice.
	const skipK, limitM = 3, 5
	wantPage := names
	if skipK < len(wantPage) {
		wantPage = wantPage[skipK:]
	} else {
		wantPage = nil
	}
	if len(wantPage) > limitM {
		wantPage = wantPage[:limitM]
	}
	res, err := engine.Run(ctx, fmt.Sprintf("MATCH (n:Person) RETURN n.name ORDER BY n.name SKIP %d LIMIT %d", skipK, limitM), nil)
	if err == nil {
		var page []string
		for res.Next() {
			if s, ok := res.StringAt(0); ok {
				page = append(page, s)
			}
		}
		_ = res.Err()
		_ = res.Close()
		if !equalStrings(page, wantPage) {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "SKIP/LIMIT page",
				Message: fmt.Sprintf("SKIP %d LIMIT %d: engine=%v oracle=%v", skipK, limitM, page, wantPage)})
		}
	}

	// CY16 — db.* schema introspection vs the modelled schema.
	if pc > 0 {
		if labels := collectStringCol(ctx, engine, "CALL db.labels() YIELD label RETURN label"); !setEquals(labels, []string{"Person"}) {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "db.labels",
				Message: fmt.Sprintf("db.labels()=%v, want {Person}", labels)})
		}
		if keys := collectStringCol(ctx, engine, "CALL db.propertyKeys() YIELD propertyKey RETURN propertyKey"); !containsAll(keys, "name", "age") {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "db.propertyKeys",
				Message: fmt.Sprintf("db.propertyKeys()=%v, want superset of {name,age}", keys)})
		}
	}
	if knows > 0 {
		if rts := collectStringCol(ctx, engine, "CALL db.relationshipTypes() YIELD relationshipType RETURN relationshipType"); !setEquals(rts, []string{"KNOWS"}) {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "db.relationshipTypes",
				Message: fmt.Sprintf("db.relationshipTypes()=%v, want {KNOWS}", rts)})
		}
	}
	return vs
}

// reSName matches a surface-writer Person name (s followed by digits).
var reSName = regexp.MustCompile(`^s\d+$`)

// collectStringCol runs query and returns every col-0 StringValue (unquoted).
func collectStringCol(ctx context.Context, engine *EngineAdapter, query string) []string {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return nil
	}
	var out []string
	for res.Next() {
		if s, ok := res.StringAt(0); ok {
			out = append(out, s)
		}
	}
	_ = res.Err()
	_ = res.Close()
	return out
}

// setEquals reports whether got, as a set, equals want.
func setEquals(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[string]bool, len(got))
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
}

// containsAll reports whether got contains every listed value.
func containsAll(got []string, want ...string) bool {
	m := make(map[string]bool, len(got))
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
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
		Description: "broad Cypher read surface (grouped+exact aggregation/DISTINCT/UNION/WHERE/WITH/OPTIONAL MATCH/UNWIND/ORDER BY) vs oracle invariants",
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
			if v := checkSurfaceAll(tick, sm.oracle, sm.engine); len(v) > 0 {
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
			if v := checkSurfaceAll(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := checkSurfaceAll(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	if v := checkSurfaceNonVacuity(lastTick, sm.oracle); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}

// checkSurfaceNonVacuity is the terminal non-vacuity assertion of the
// cypher-surface scenario (rmp #2452): it proves the grouped-aggregation and
// DISTINCT probes actually exercised non-degenerate shapes during the run —
// at least two distinct cities and two distinct ages among the surviving
// Persons, and at least one KNOWS target — rather than passing vacuously on a
// single-group or edgeless graph. It runs only at the end (mid-run the graph
// may legitimately be small right after recovery).
func checkSurfaceNonVacuity(tick int64, oracle *GraphOracle) []Violation {
	var vs []Violation
	stats, complete := oracle.personCityStats()
	if !complete {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "surface non-vacuity",
			Message: "a modelled Person lacks a city: the grouped-by-city probes were skipped for part of the run"})
	}
	if len(stats) < 2 {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "surface non-vacuity",
			Message: fmt.Sprintf("terminal graph has %d distinct cities; grouped aggregation needs >= 2 groups to be non-trivial", len(stats))})
	}
	distinctAges := make(map[int64]bool)
	for _, a := range oracle.personAges() {
		distinctAges[a] = true
	}
	if len(distinctAges) < 2 {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "surface non-vacuity",
			Message: fmt.Sprintf("terminal graph has %d distinct ages; the exact-aggregate probes need spread to be non-trivial", len(distinctAges))})
	}
	if len(oracle.knowsTargetNamesDistinct()) == 0 {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "surface non-vacuity",
			Message: "terminal graph has no KNOWS target: the DISTINCT-row probes never saw a row"})
	}
	return vs
}
