package sim

// null_semantics.go — the null-semantics scenario: non-degenerate NULL and
// three-valued-logic (3VL) coverage for the DST (rmp #2453).
//
// The cypher-surface workload guarantees every Person carries an age, so its
// WHERE n.age IS NULL probe is asserted against the constant 0 — it cannot
// distinguish a working IS NULL from a broken one; OPTIONAL MATCH is observed
// only through count(m), collapsing the NULL-padding semantics; and count(n)
// vs count(n.prop) NULL-skipping is never differentiated. The probes here run
// against a writer that deliberately creates a seed-chosen fraction of
// AGELESS Persons, so both sides of every NULL distinction are populated:
//
//   - count(n) vs count(n.age) — the aggregate NULL-skipping distinction;
//   - WHERE n.age IS NULL / IS NOT NULL — both populations genuinely non-empty
//     (enforced by the terminal non-vacuity assertion);
//   - sum/min/max plus the exact avg/stDev/stDevP/percentile battery of
//     rmp #2452 ([expectedAggregates]/[compareExactAggregates]), referenced
//     over the age-bearing subset only;
//   - OPTIONAL MATCH NULL-row padding as full row-set equality including the
//     NULL markers;
//   - 3VL predicate arithmetic: n.age > 30, NOT (n.age > 30) — the classic
//     NOT NULL = NULL trap — the excluded-middle disjunction, and the 3VL
//     partition identity gt + not-gt + is-null = total;
//   - coalesce(n.age, -1) making the NULL replacement observable in a sum.
//
// The cypher-surface scenario is untouched: only the null-semantics workload
// emits the ageless template, so the no-NULL guarantees of rmp #2452 hold.

import (
	"context"
	"fmt"
	"sort"
)

// NullSemanticsWriter builds the graph the null-semantics battery reads: it
// creates Person nodes of three deterministic, seed-chosen shapes — AGELESS
// (name+city, ~1/3 of creates, via [tmplCreatePersonNoAge]), CITYLESS
// (name+age, via [tmplCreatePerson]), and full (name+age+city, via
// [tmplCreatePersonCity]) — plus KNOWS edges between existing Persons, so the
// NULL-padding OPTIONAL MATCH probe sees both matched and NULL rows. It avoids
// MERGE and SET so the oracle's age-present vs age-absent bookkeeping is
// unambiguous.
//
// # Concurrency contract
//
// NullSemanticsWriter is NOT safe for concurrent use; it is invoked from the
// single simulation goroutine.
type NullSemanticsWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*NullSemanticsWriter) Name() string { return "NullSemanticsWriter" }

// nullSemanticsAgelessFrac is the fraction of Person creates that omit the age
// property (~1/3, so both IS NULL populations stay non-trivial), and
// nullSemanticsCitylessFrac the additional fraction that omits the city
// (exercising the second optional-property dimension of the model).
const (
	nullSemanticsAgelessFrac  = 1.0 / 3.0
	nullSemanticsCitylessFrac = 1.0 / 6.0
)

// NextOp returns a KNOWS edge between two existing Persons or a fresh Person
// CREATE in one of the three property shapes, all seed-derived.
func (w *NullSemanticsWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) >= 2 && seed.Float64() < 0.35 {
		a := names[seed.IntN(len(names))]
		b := names[seed.IntN(len(names))]
		return Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": a, "b": b}}
	}
	name := fmt.Sprintf("s%d", w.counter)
	w.counter++
	city := fmt.Sprintf("c%d", seed.IntN(surfaceCityVocab))
	age := int64(seed.IntN(100))
	switch r := seed.Float64(); {
	case r < nullSemanticsAgelessFrac:
		return Op{Kind: OpCreate, Cypher: tmplCreatePersonNoAge,
			Params: map[string]any{"name": name, "city": city}}
	case r < nullSemanticsAgelessFrac+nullSemanticsCitylessFrac:
		return Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": name, "age": age}}
	default:
		return Op{Kind: OpCreate, Cypher: tmplCreatePersonCity,
			Params: map[string]any{"name": name, "age": age, "city": city}}
	}
}

// nullSemanticsWorkload is a 100% NullSemanticsWriter mix.
func nullSemanticsWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&NullSemanticsWriter{}}, Weights: []float64{1.0}}
}

// personAgePartition returns the ascending-sorted names of the modelled
// Persons that CARRY an age property and of those that do NOT — the oracle's
// age-present vs age-absent bookkeeping the null-semantics battery is
// referenced against (rmp #2453). Presence of the key, not its value, decides
// the side, mirroring Cypher's IS NULL on a missing property.
func (o *GraphOracle) personAgePartition() (withAge, ageless []string) {
	for _, n := range o.nodes {
		if !hasLabel(n, "Person") {
			continue
		}
		nm, ok := n.Properties["name"].(string)
		if !ok {
			continue
		}
		if _, has := n.Properties["age"]; has {
			withAge = append(withAge, nm)
		} else {
			ageless = append(ageless, nm)
		}
	}
	sort.Strings(withAge)
	sort.Strings(ageless)
	return withAge, ageless
}

// CheckNullSemantics runs the NULL / 3VL probe battery against references
// computed independently from the oracle's age-present vs age-absent
// partition (rmp #2453). Every scalar probe is exact; the OPTIONAL MATCH
// probe is full row-set equality including the NULL markers; and the 3VL
// partition identity is asserted over the ENGINE-returned counts, so the
// identity itself is observable rather than implied. It runs on the quiescent
// graph, periodically, after each crash/recovery, and at the end.
func CheckNullSemantics(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation

	// scalarProbe runs a one-integer-column query, asserts it equals want, and
	// returns the engine's value with ok=false on a query/drain error (so the
	// partition-identity check below never runs on garbage).
	scalarProbe := func(label, query string, want int64) (int64, bool) {
		res, err := engine.Run(ctx, query, nil)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s query error: %v", label, err)})
			return 0, false
		}
		var got int64
		if res.Next() {
			got, _ = res.IntAt(0)
		}
		derr := res.Err()
		_ = res.Close()
		if derr != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s drain error: %v", label, derr)})
			return 0, false
		}
		if got != want {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: label,
				Message: fmt.Sprintf("%s: engine=%d, oracle=%d", label, got, want)})
		}
		return got, true
	}

	withAge, ageless := oracle.personAgePartition()
	ages := oracle.personAges()
	total := int64(len(withAge) + len(ageless))
	if len(ages) != len(withAge) {
		// A modelled age that is not an int64 would silently skew every
		// arithmetic reference below; the workload never binds one, so this is
		// a model defect and the battery fails loudly instead of mis-asserting.
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "null-semantics model",
			Message: fmt.Sprintf("oracle models %d age-bearing Persons but only %d integer ages", len(withAge), len(ages))})
		return vs
	}
	var sum, gt30, le30 int64
	for _, a := range ages {
		sum += a
		if a > 30 {
			gt30++
		} else {
			le30++
		}
	}

	// (a) count(n) vs count(n.age): the aggregate NULL-skipping distinction.
	eTotal, okTotal := scalarProbe("count(n)", "MATCH (n:Person) RETURN count(n)", total)
	scalarProbe("count(n.age)", "MATCH (n:Person) RETURN count(n.age)", int64(len(withAge)))

	// (b) IS NULL / IS NOT NULL — both sides referenced against the partition.
	eNull, okNull := scalarProbe("WHERE age IS NULL",
		"MATCH (n:Person) WHERE n.age IS NULL RETURN count(n)", int64(len(ageless)))
	scalarProbe("WHERE age IS NOT NULL",
		"MATCH (n:Person) WHERE n.age IS NOT NULL RETURN count(n)", int64(len(withAge)))

	// (c) NULL-skipping aggregates over the age-bearing subset only. sum of an
	// all-NULL (or empty) column is 0 per openCypher, so it is asserted
	// unconditionally; min/max are NULL below one value, so they are guarded;
	// the exact avg/stDev/stDevP/percentile battery (rmp #2452 helpers) needs
	// two values (stDev is NULL for n < 2).
	scalarProbe("sum(age) skips NULL", "MATCH (n:Person) RETURN sum(n.age)", sum)
	if len(ages) > 0 {
		scalarProbe("min(age) skips NULL", "MATCH (n:Person) RETURN min(n.age)", ages[0])
		scalarProbe("max(age) skips NULL", "MATCH (n:Person) RETURN max(n.age)", ages[len(ages)-1])
	}
	if len(ages) >= 2 {
		vs = append(vs, compareExactAggregates(tick, expectedAggregates(ages), engine)...)
	}

	// (e) 3VL predicate arithmetic. NULL > 30 is NULL, so the NULL-aged rows
	// are excluded from n.age > 30, from NOT (n.age > 30) — the classic
	// NOT NULL = NULL trap — and from the excluded-middle disjunction alike.
	eGt, okGt := scalarProbe("3VL age>30",
		"MATCH (n:Person) WHERE n.age > 30 RETURN count(n)", gt30)
	eNot, okNot := scalarProbe("3VL NOT(age>30)",
		"MATCH (n:Person) WHERE NOT (n.age > 30) RETURN count(n)", le30)
	scalarProbe("3VL age>30 OR age<=30",
		"MATCH (n:Person) WHERE n.age > 30 OR n.age <= 30 RETURN count(n)", int64(len(withAge)))
	if okTotal && okNull && okGt && okNot && eGt+eNot+eNull != eTotal {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "3VL partition identity",
			Message: fmt.Sprintf("engine counts violate gt + not-gt + is-null = total: %d + %d + %d != %d",
				eGt, eNot, eNull, eTotal)})
	}

	// (f) coalesce(n.age, -1): the NULL replacement is observable — every
	// ageless row contributes exactly -1 to the sum.
	scalarProbe("sum(coalesce(age,-1))",
		"MATCH (n:Person) RETURN sum(coalesce(n.age, -1))", sum-int64(len(ageless)))

	// (d) OPTIONAL MATCH NULL-row padding, as row-set equality.
	vs = append(vs, checkOptionalMatchNullRows(ctx, tick, oracle, engine, ageless)...)
	return vs
}

// checkOptionalMatchNullRows asserts the OPTIONAL MATCH NULL-padding row set:
//
//	MATCH (n:Person) WHERE n.age IS NULL
//	OPTIONAL MATCH (n)-[:KNOWS]->(m)
//	RETURN n.name, m.name ORDER BY n.name
//
// The oracle predicts exactly which rows carry a NULL m: one row per outgoing
// KNOWS edge of each ageless Person, or a single (name, NULL) row when it has
// none. Cells are compared through the canonical expr.Value rendering
// ([canonicalValueString] — a string renders quoted, NULL renders as "null"),
// exactly what the engine's own String() yields, so the NULL markers are part
// of the equality. ORDER BY pins only n.name (the order of several m for one n
// is unspecified), so the primary ordering is asserted on the drained n.name
// sequence and the row SET is compared sorted.
func checkOptionalMatchNullRows(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter, ageless []string) []Violation {
	const op = "OPTIONAL MATCH null rows"
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}

	res, err := engine.Run(ctx,
		"MATCH (n:Person) WHERE n.age IS NULL OPTIONAL MATCH (n)-[:KNOWS]->(m) RETURN n.name, m.name ORDER BY n.name", nil)
	if err != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("query error: %v", err)}}
	}
	var got []string
	prevName, orderOK := "", true
	for res.Next() {
		name, ok := res.StringAt(0)
		if !ok {
			fail(fmt.Sprintf("row %d: n.name is not a string", len(got)))
			break
		}
		if name < prevName {
			orderOK = false
		}
		prevName = name
		got = append(got, rawValueAt(res, 0).String()+" | "+rawValueAt(res, 1).String())
	}
	derr := res.Err()
	_ = res.Close()
	if derr != nil {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("drain error: %v", derr)})
		return vs
	}
	if !orderOK {
		fail("ORDER BY n.name violated: the drained n.name sequence is not non-decreasing")
	}

	// Oracle reference: outgoing KNOWS targets per ageless Person.
	agelessIDs := make(map[uint64]string, len(ageless))
	for _, nm := range ageless {
		if id, ok := oracle.byName[nm]; ok {
			agelessIDs[id] = nm
		}
	}
	targets := make(map[string][]string, len(ageless))
	for k := range oracle.edges {
		if k.label != "KNOWS" {
			continue
		}
		src, ok := agelessIDs[k.src]
		if !ok {
			continue
		}
		if dst := oracle.nameOf(k.dst); dst != "" {
			targets[src] = append(targets[src], dst)
		}
	}
	want := make([]string, 0, len(got))
	for _, nm := range ageless {
		nc := canonicalValueString(nm)
		ts := targets[nm]
		if len(ts) == 0 {
			want = append(want, nc+" | null")
			continue
		}
		for _, t := range ts {
			want = append(want, nc+" | "+canonicalValueString(t))
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !equalStrings(got, want) {
		fail(fmt.Sprintf("row set (sorted): engine=%v, oracle=%v", got, want))
	}
	return vs
}

// checkNullSemanticsNonVacuity is the terminal non-vacuity assertion of the
// null-semantics scenario (rmp #2453): it proves the probes actually
// exercised non-degenerate NULL shapes during the run — both the IS NULL and
// the IS NOT NULL populations non-empty (the age-bearing side with at least
// two members, the exact-aggregate floor), both arms of the 3VL age>30 split
// inhabited, and, among the ageless Persons, at least one WITH an outgoing
// KNOWS edge and at least one WITHOUT, so the OPTIONAL MATCH probe saw both a
// matched row and a NULL-padded row. It runs only at the end (mid-run the
// graph may legitimately be small right after recovery).
func checkNullSemanticsNonVacuity(tick int64, oracle *GraphOracle) []Violation {
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationVacuousRun, Tick: tick, Op: "null-semantics non-vacuity", Message: msg})
	}
	withAge, ageless := oracle.personAgePartition()
	if len(ageless) == 0 {
		fail("terminal graph has no ageless Person: the IS NULL side of every probe was vacuous")
	}
	if len(withAge) < 2 {
		fail(fmt.Sprintf("terminal graph has %d age-bearing Persons; the IS NOT NULL side and the exact-aggregate probes need >= 2", len(withAge)))
	}
	var gt30, le30 int
	for _, a := range oracle.personAges() {
		if a > 30 {
			gt30++
		} else {
			le30++
		}
	}
	if gt30 == 0 || le30 == 0 {
		fail(fmt.Sprintf("terminal 3VL split is degenerate: %d Persons with age>30, %d with age<=30", gt30, le30))
	}
	withKnows, withoutKnows := 0, 0
	for _, nm := range ageless {
		id, ok := oracle.byName[nm]
		if !ok {
			continue
		}
		found := false
		for k := range oracle.edges {
			if k.label == "KNOWS" && k.src == id {
				found = true
				break
			}
		}
		if found {
			withKnows++
		} else {
			withoutKnows++
		}
	}
	if len(ageless) > 0 && (withKnows == 0 || withoutKnows == 0) {
		fail(fmt.Sprintf("terminal OPTIONAL MATCH shapes are one-sided: %d ageless Persons with an outgoing KNOWS, %d without", withKnows, withoutKnows))
	}
	return vs
}

// nullSemanticsScenario broadens the DST's NULL / 3VL coverage: a workload
// deliberately creates a seed-chosen fraction of AGELESS Persons, and
// [CheckNullSemantics] verifies count(n) vs count(n.age), IS NULL / IS NOT
// NULL, NULL-skipping aggregates, OPTIONAL MATCH NULL-row padding, 3VL
// predicate arithmetic (including the NOT NULL = NULL trap and the partition
// identity), and coalesce — against the oracle's age-present vs age-absent
// partition, periodically, after each crash/recovery, and at the end, with
// the per-op counters oracle (rmp #2448) pinning every write's effect set. It
// is bit-reproducible.
func nullSemanticsScenario() Scenario {
	return Scenario{
		Name:        ScenarioNullSemantics,
		Description: "non-degenerate NULL and 3VL semantics (count(n) vs count(n.prop), IS NULL both sides, NULL-skipping aggregates, OPTIONAL MATCH null rows, NOT/OR predicate arithmetic, coalesce) vs the oracle's age partition",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x3011AC3,
		MaxTicks:    500,
		Workload:    nullSemanticsWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 90.0, StabilityWindow: 25},
		run:         runNullSemantics,
	}
}

// nullSemanticsCheckEvery is the periodic null-semantics battery cadence.
const nullSemanticsCheckEvery = 70

// runNullSemantics drives the null-semantics safety loop, verifying each
// committed write's reported counters against the oracle's expectation
// ([CheckOpCounters], rmp #2448) and running the NULL / 3VL battery
// periodically, after each crash/recovery, and at the end, followed by the
// terminal non-vacuity assertion. Deterministic.
func runNullSemantics(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := nullSemanticsScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: null-semantics new: %w", err)
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
			if v := CheckNullSemantics(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery null semantics>"}, v), nil
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		// Per-op counters oracle (#2448) on the pre-apply model: the ageless
		// CREATE must report exactly {1 node, 1 label, 2 properties} — an age
		// that leaked into the effect report would show here first.
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
		if tick%nullSemanticsCheckEvery == 0 {
			if v := CheckNullSemantics(tick, sm.oracle, sm.engine); len(v) > 0 {
				return sm.report(tick, op, v), nil
			}
		}
	}
	if v := CheckNullSemantics(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	if v := checkNullSemanticsNonVacuity(lastTick, sm.oracle); len(v) > 0 {
		return sm.report(lastTick, lastOp, v), nil
	}
	return nil, nil
}
