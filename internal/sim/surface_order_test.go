package sim

import (
	"context"
	"slices"
	"sort"
	"testing"
)

// orderFixturePersons is the ordering battery's fixture: thirteen Persons whose
// shape makes every ordering arm observable rather than incidental.
//
//   - Ages COLLIDE three times over (99, 41, 35 and 30 each appear twice), so
//     the secondary name key decides real ties;
//   - ages are not multiples of ten, so `n.age % 10` is a genuinely different
//     key from `n.age` — it reorders the rows rather than reproducing them;
//   - the city histogram is UNEQUAL (4/3/3/2/1), so ordering by an aggregate is
//     distinguishable from ordering by the group key, four cities survive the
//     `c > 1` two-stage filter (so its LIMIT 3 truncates), and the median group
//     size splits the aggregate filter into kept and dropped groups;
//   - thirteen rows exceed both orderTopK (5) and orderExpandTopK (10), so both
//     LIMITs truncate;
//   - the KNOWS edges leave from Persons on BOTH sides of the top-10 boundary
//     (s11 and s7 are inside, s3 and s10 are outside), so the expand probe's
//     count is wrong unless the top-k SELECTION is right;
//   - the names sort lexicographically ("s1" < "s10" < "s11" < … < "s2"), which
//     keeps the expected sequences from accidentally agreeing with a numeric
//     ordering.
var orderFixturePersons = []cityPerson{
	{"s1", 41, "c0"},
	{"s2", 41, "c0"},
	{"s3", 23, "c0"},
	{"s4", 35, "c1"},
	{"s5", 35, "c1"},
	{"s6", 12, "c2"},
	{"s7", 52, "c2"},
	{"s8", 30, "c3"},
	{"s9", 30, "c3"},
	{"s10", 7, "c4"},
	{"s11", 99, "c1"},
	{"s12", 99, "c2"},
	{"s13", 60, "c0"},
}

// orderFixtureEdges are the fixture's KNOWS edges: three leave from Persons
// INSIDE the top-10 by (age DESC, name ASC) and two from Persons outside it, so
// the expected expand count (3) is only reachable with the right selection.
var orderFixtureEdges = [][2]string{
	{"s11", "s1"}, {"s11", "s2"}, {"s7", "s3"}, // inside the top 10
	{"s3", "s1"}, {"s10", "s1"}, // outside the top 10
}

// orderFixture seeds [orderFixturePersons] and [orderFixtureEdges] into both
// the engine and the oracle.
func orderFixture(t *testing.T) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	return seedCityFixture(t, orderFixturePersons, orderFixtureEdges)
}

// orderFixtureExpect returns the oracle-computed reference for the fixture.
func orderFixtureExpect(t *testing.T, o *GraphOracle) orderExpect {
	t.Helper()
	rows, complete := o.personOrderRows()
	if !complete {
		t.Fatalf("fixture model is not totally ordered (missing name/age or duplicate name)")
	}
	stats, cityComplete := o.personCityStats()
	if !cityComplete {
		t.Fatalf("fixture model has a Person without a city")
	}
	return expectedOrdering(rows, stats, cityComplete)
}

// TestExpectedOrdering_HandComputed pins [expectedOrdering] against a
// hand-computed ordering of the fixture — including the deliberate ties, which
// only the secondary key resolves — so the reference itself is proven to encode
// the requested comparators (DESC on the primary key, ASC tie-break, an
// expression key, an aggregate key) rather than merely "some order".
func TestExpectedOrdering_HandComputed(t *testing.T) {
	rows := make([]personOrderRow, 0, len(orderFixturePersons))
	for _, p := range orderFixturePersons {
		rows = append(rows, personOrderRow{Name: p.name, Age: p.age})
	}
	stats := []cityStat{
		{City: "c0", Count: 4}, {City: "c1", Count: 3}, {City: "c2", Count: 3},
		{City: "c3", Count: 2}, {City: "c4", Count: 1},
	}
	got := expectedOrdering(rows, stats, true)

	// ORDER BY n.age DESC, n.name ASC. Ties at 99 (s11 < s12), 41 (s1 < s2),
	// 35 (s4 < s5) and 30 (s8 < s9) are broken by the ascending name.
	wantAgeDesc := []groupedRow{
		{"s11", 99}, {"s12", 99}, {"s13", 60}, {"s7", 52}, {"s1", 41}, {"s2", 41},
		{"s4", 35}, {"s5", 35}, {"s8", 30}, {"s9", 30}, {"s3", 23}, {"s6", 12}, {"s10", 7},
	}
	if diff := diffOrderedPairs(got.AgeDescNameAsc, wantAgeDesc); diff != "" {
		t.Errorf("AgeDescNameAsc: %s", diff)
	}

	// ORDER BY n.name DESC — lexicographic, so s9 leads and s1 trails.
	wantNameDesc := []string{"s9", "s8", "s7", "s6", "s5", "s4", "s3", "s2", "s13", "s12", "s11", "s10", "s1"}
	if !equalStrings(got.NameDesc, wantNameDesc) {
		t.Errorf("NameDesc = %v, want %v", got.NameDesc, wantNameDesc)
	}

	// ORDER BY n.age % 10, n.name — a different key from the age itself: the
	// 99-year-olds land last and the 30-year-olds first, alongside s13 (60).
	wantModTen := []groupedRow{
		{"s13", 0}, {"s8", 0}, {"s9", 0}, {"s1", 1}, {"s2", 1}, {"s6", 2}, {"s7", 2},
		{"s3", 3}, {"s4", 5}, {"s5", 5}, {"s10", 7}, {"s11", 9}, {"s12", 9},
	}
	if diff := diffOrderedPairs(got.ModTen, wantModTen); diff != "" {
		t.Errorf("ModTen: %s", diff)
	}

	// ORDER BY count(*) DESC, city ASC — the equal 3s are separated by the city.
	wantCity := []groupedRow{{"c0", 4}, {"c1", 3}, {"c2", 3}, {"c3", 2}, {"c4", 1}}
	if diff := diffOrderedPairs(got.CityByCountDesc, wantCity); diff != "" {
		t.Errorf("CityByCountDesc: %s", diff)
	}

	// The fixture must genuinely contain ties, or every perturbation below that
	// targets the tie-break would be vacuous.
	if !hasAgeTie(rows) {
		t.Fatal("fixture degenerate: no two Persons share an age, so the tie-break arm proves nothing")
	}
	if !cityKnown(&got) {
		t.Fatal("CityGroupsKnown must be true when the city histogram is complete")
	}
}

// cityKnown reports whether want carries a usable city-group reference.
func cityKnown(want *orderExpect) bool { return want.CityGroupsKnown && len(want.CityByCountDesc) > 0 }

// TestSurfaceOrdering_PassAndCatch verifies the whole ordering battery is clean
// on a consistent fixture and that each probe family FIRES when the oracle's
// model is perturbed — the model-side half of the sensitivity proof (the
// comparator-side half is [TestOrderingComparator_PerturbedReferenceFires]).
func TestSurfaceOrdering_PassAndCatch(t *testing.T) {
	t.Run("baseline clean", func(t *testing.T) {
		a, o := orderFixture(t)
		st := newOrderingStats()
		if v := CheckCypherSurfaceOrdering(0, o, a, st); len(v) > 0 {
			t.Fatalf("consistent fixture should be clean, got: %v", v)
		}
		// The battery must have OBSERVED the conditions that make it able to
		// fail, so a clean result here is evidence rather than silence.
		if v := checkOrderingNonVacuity(0, st); len(v) > 0 {
			t.Fatalf("fixture should satisfy every non-vacuity gate, got: %v", v)
		}
	})

	t.Run("perturbed age fires the ordering probes", func(t *testing.T) {
		a, o := orderFixture(t)
		// s7 moves from 52 to 1: the engine still returns it fourth, the oracle
		// now expects it last.
		o.nodes[o.byName["s7"]].Properties["age"] = int64(1)
		if v := CheckCypherSurfaceOrdering(0, o, a, nil); len(v) == 0 {
			t.Fatal("ordering battery FAILED to detect a perturbed age")
		}
	})

	t.Run("perturbed city fires the aggregate ordering and multi-part probes", func(t *testing.T) {
		a, o := orderFixture(t)
		o.nodes[o.byName["s10"]].Properties["city"] = "c0"
		if v := CheckCypherSurfaceOrdering(0, o, a, nil); len(v) == 0 {
			t.Fatal("ordering battery FAILED to detect a perturbed city histogram")
		}
	})

	t.Run("ghost person fires the ordering and pagination probes", func(t *testing.T) {
		a, o := orderFixture(t)
		o.ApplyCreate(tmplCreatePersonCity, map[string]any{"name": "s99", "age": int64(77), "city": "c0"})
		if v := CheckCypherSurfaceOrdering(0, o, a, nil); len(v) == 0 {
			t.Fatal("ordering battery FAILED to detect a ghost person")
		}
	})

	t.Run("removed oracle edge fires the top-k expand probe", func(t *testing.T) {
		a, o := orderFixture(t)
		// Drop one edge that leaves from INSIDE the top 10, so only the expand
		// probe's expectation changes.
		src := o.byName["s11"]
		for k := range o.edges {
			if k.src == src {
				delete(o.edges, k)
				break
			}
		}
		vs := CheckCypherSurfaceOrdering(0, o, a, nil)
		if len(vs) == 0 {
			t.Fatal("ordering battery FAILED to detect a removed KNOWS edge")
		}
		for _, v := range vs {
			if v.Op == "top-k then expand" {
				return
			}
		}
		t.Fatalf("the deviation was not reported by the top-k expand probe: %v", vs)
	})

	t.Run("edge from outside the top-k must not count", func(t *testing.T) {
		// s3 and s10 are outside the top 10 and their edges must be excluded.
		// Adding one to the ORACLE's expectation (by pretending s11 owns it)
		// would change the count, which proves the probe counts a SELECTION and
		// not simply every KNOWS edge.
		_, o := orderFixture(t)
		outDeg := o.knowsOutDegreeByName()
		if outDeg["s3"] != 1 || outDeg["s10"] != 1 {
			t.Fatalf("fixture degenerate: expected one edge from each of s3/s10, got %v", outDeg)
		}
		rows, _ := o.personOrderRows()
		stats, known := o.personCityStats()
		want := expectedOrdering(rows, stats, known)
		var top int64
		for _, r := range prefixRows(want.AgeDescNameAsc, orderExpandTopK) {
			top += outDeg[r.Key]
		}
		var all int64
		for _, d := range outDeg {
			all += d
		}
		if top != 3 || all != 5 {
			t.Fatalf("expected top-10 out-degree 3 of 5 total, got %d of %d", top, all)
		}
	})
}

// TestOrderingComparator_PerturbedReferenceFires is the comparator-side
// sensitivity proof: the probes run against the REAL engine while the oracle's
// comparator is perturbed one way at a time, and each perturbation must fire.
// Together with [TestExpectedOrdering_HandComputed] this proves the probes pin
// the ordering CONTRACT — direction, tie-break, key — and not merely that the
// engine returned the right rows in some order.
func TestOrderingComparator_PerturbedReferenceFires(t *testing.T) {
	a, o := orderFixture(t)
	want := orderFixtureExpect(t, o)
	if v := compareOrdering(0, &want, a, nil); len(v) > 0 {
		t.Fatalf("the correct reference should be clean, got: %v", v)
	}

	// The rows in their oracle-model order, used to rebuild perturbed references.
	rows, _ := o.personOrderRows()

	t.Run("secondary key dropped", func(t *testing.T) {
		// A comparator that knows only `age DESC` leaves tied rows in whatever
		// order it received them. Feeding it a name-DESCENDING input therefore
		// reverses every tie relative to the requested `n.name ASC` tie-break —
		// which is exactly how a dropped secondary key becomes observable.
		byNameDesc := slices.Clone(rows)
		sort.SliceStable(byNameDesc, func(i, j int) bool { return byNameDesc[i].Name > byNameDesc[j].Name })
		sort.SliceStable(byNameDesc, func(i, j int) bool { return byNameDesc[i].Age > byNameDesc[j].Age })
		perturbed := want
		perturbed.AgeDescNameAsc = make([]groupedRow, len(byNameDesc))
		for i, r := range byNameDesc {
			perturbed.AgeDescNameAsc[i] = groupedRow{Key: r.Name, Val: r.Age}
		}
		if v := compareOrdering(0, &perturbed, a, nil); len(v) == 0 {
			t.Fatal("a reference with NO secondary key did not fire: the tie-break is not actually asserted")
		}
	})

	t.Run("DESC treated as ASC", func(t *testing.T) {
		byAgeAsc := slices.Clone(rows)
		sort.SliceStable(byAgeAsc, func(i, j int) bool {
			if byAgeAsc[i].Age != byAgeAsc[j].Age {
				return byAgeAsc[i].Age < byAgeAsc[j].Age
			}
			return byAgeAsc[i].Name < byAgeAsc[j].Name
		})
		perturbed := want
		perturbed.AgeDescNameAsc = make([]groupedRow, len(byAgeAsc))
		for i, r := range byAgeAsc {
			perturbed.AgeDescNameAsc[i] = groupedRow{Key: r.Name, Val: r.Age}
		}
		if v := compareOrdering(0, &perturbed, a, nil); len(v) == 0 {
			t.Fatal("an ASCENDING reference for a DESC ordering did not fire: the direction is not asserted")
		}
	})

	t.Run("name DESC treated as ASC", func(t *testing.T) {
		perturbed := want
		perturbed.NameDesc = slices.Clone(want.NameDesc)
		slices.Reverse(perturbed.NameDesc)
		if v := compareOrdering(0, &perturbed, a, nil); len(v) == 0 {
			t.Fatal("an ASCENDING reference for ORDER BY n.name DESC did not fire")
		}
	})

	t.Run("aggregate ordering direction flipped", func(t *testing.T) {
		perturbed := want
		perturbed.CityByCountDesc = slices.Clone(want.CityByCountDesc)
		slices.Reverse(perturbed.CityByCountDesc)
		if v := compareOrdering(0, &perturbed, a, nil); len(v) == 0 {
			t.Fatal("an ASCENDING reference for ORDER BY count(*) DESC did not fire")
		}
	})

	t.Run("expression key computed with the wrong modulus", func(t *testing.T) {
		byMod5 := slices.Clone(rows)
		sort.SliceStable(byMod5, func(i, j int) bool {
			if a, b := byMod5[i].Age%5, byMod5[j].Age%5; a != b {
				return a < b
			}
			return byMod5[i].Name < byMod5[j].Name
		})
		perturbed := want
		perturbed.ModTen = make([]groupedRow, len(byMod5))
		for i, r := range byMod5 {
			perturbed.ModTen[i] = groupedRow{Key: r.Name, Val: r.Age % 5}
		}
		if v := compareOrdering(0, &perturbed, a, nil); len(v) == 0 {
			t.Fatal("a reference keyed on age%5 did not fire against an age%10 ordering")
		}
	})

	t.Run("Top arm compared against a wrong unlimited arm", func(t *testing.T) {
		// The Top probe asserts the LIMITed arm against BOTH the oracle prefix
		// and the engine's own unlimited result; handing it a wrong unlimited
		// arm must fire, so the arm-to-arm half is real.
		bogus := []groupedRow{{"zzz", 1}, {"zzy", 2}, {"zzx", 3}, {"zzw", 4}, {"zzv", 5}, {"zzu", 6}}
		if v := compareTopAgainstSort(0, &want, bogus, a, nil); len(v) == 0 {
			t.Fatal("the Top probe did not fire against a wrong unlimited arm")
		}
	})
}

// TestOrderingPagination_ContractsAndSensitivity pins the pagination contracts
// this engine actually implements — VERIFIED here rather than assumed — and
// proves the page comparison is not vacuous.
func TestOrderingPagination_ContractsAndSensitivity(t *testing.T) {
	a, o := orderFixture(t)
	ctx := context.Background()
	names := o.personNamesSorted()

	t.Run("LIMIT 0 returns zero rows", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			query  string
			params map[string]any
		}{
			{"literal", "MATCH (n:Person) RETURN n.name ORDER BY n.name LIMIT 0", nil},
			{"parameterised", "MATCH (n:Person) RETURN n.name ORDER BY n.name LIMIT $m", map[string]any{"m": int64(0)}},
		} {
			got, err := collectStringRowsParams(ctx, a, tc.query, tc.params)
			if err != nil {
				t.Fatalf("%s LIMIT 0: unexpected error %v", tc.name, err)
			}
			if len(got) != 0 {
				t.Fatalf("%s LIMIT 0 returned %d rows (%v), want zero", tc.name, len(got), got)
			}
		}
	})

	t.Run("SKIP past the end returns zero rows", func(t *testing.T) {
		got, err := collectStringRowsParams(ctx, a,
			"MATCH (n:Person) RETURN n.name ORDER BY n.name SKIP $k", map[string]any{"k": int64(len(names) + 1)})
		if err != nil {
			t.Fatalf("SKIP past end: unexpected error %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("SKIP past end returned %d rows (%v), want zero", len(got), got)
		}
	})

	t.Run("literal and parameterised pages agree with the oracle", func(t *testing.T) {
		if v := checkOrderingPagination(0, names, a); len(v) > 0 {
			t.Fatalf("pagination probes should be clean, got: %v", v)
		}
		// The window must actually truncate, or the probe would compare the
		// whole result set with itself.
		if len(names) <= orderPageSkip+orderPageLimit {
			t.Fatalf("fixture degenerate: %d names cannot exercise SKIP %d LIMIT %d",
				len(names), orderPageSkip, orderPageLimit)
		}
		want := pageOf(names, orderPageSkip, orderPageLimit)
		if len(want) != orderPageLimit {
			t.Fatalf("expected a full page of %d, got %v", orderPageLimit, want)
		}
	})

	t.Run("a perturbed name reference fires", func(t *testing.T) {
		wrong := slices.Clone(names)
		slices.Reverse(wrong) // the ASCENDING page reference is now descending
		if v := checkOrderingPagination(0, wrong, a); len(v) == 0 {
			t.Fatal("pagination probes did not fire against a reversed name reference")
		}
	})
}

// TestOrderingTopFusion_PlansDiverge asserts the Top-vs-Sort equivalence is not
// a tautology: the engine really answers the two arms with different physical
// operators, so the LIMITed arm exercises the fused Top path and the unlimited
// arm the full Sort.
func TestOrderingTopFusion_PlansDiverge(t *testing.T) {
	a, _ := orderFixture(t)
	if v := checkTopFusion(0, a); len(v) > 0 {
		t.Fatalf("Top/Sort plan divergence should hold, got: %v", v)
	}

	sortPlan, err := a.Explain(orderQueryAgeDescNameAsc, nil)
	if err != nil {
		t.Fatalf("explain unlimited: %v", err)
	}
	topPlan, err := a.Explain(orderQueryAgeDescNameAscTop, nil)
	if err != nil {
		t.Fatalf("explain limited: %v", err)
	}
	if sortPlan == topPlan {
		t.Fatalf("the two arms rendered the SAME plan, so the equivalence proves nothing:\n%s", sortPlan)
	}
	t.Logf("unlimited ops=%v\n%s", orderingPlanOps(sortPlan), sortPlan)
	t.Logf("limited   ops=%v\n%s", orderingPlanOps(topPlan), topPlan)
}

// TestOrderingPlanOps_Grammar pins the plan-rendering grammar the Top/Sort
// assertion reads: operator names behind the tree glyphs, restricted to the
// ordering operators, in plan order.
func TestOrderingPlanOps_Grammar(t *testing.T) {
	plan := "Project\n" +
		"└─ Limit\n" +
		"   └─ Skip\n" +
		"      └─ Sort\n" +
		"         └─ Project\n" +
		"            └─ NodeByLabelScan [Person]\n"
	got := orderingPlanOps(plan)
	want := []string{"Limit", "Skip", "Sort"}
	if !slices.Equal(got, want) {
		t.Fatalf("orderingPlanOps = %v, want %v", got, want)
	}
	if ops := orderingPlanOps("Project\n└─ Top\n   └─ NodeByLabelScan [Person]\n"); !slices.Equal(ops, []string{"Top"}) {
		t.Fatalf("orderingPlanOps(Top plan) = %v, want [Top]", ops)
	}
}

// TestOrderingNonVacuity_GatesFire proves every terminal gate is load-bearing:
// a fully-populated record is clean, and dropping any single observation fires
// exactly that gate.
func TestOrderingNonVacuity_GatesFire(t *testing.T) {
	full := func() *OrderingStats {
		return &OrderingStats{
			checks: 3, sawAgeTie: true, descRowsMax: 13,
			topKTruncated: true, aggFilterSplit: true, expandRows: true,
		}
	}
	if v := checkOrderingNonVacuity(0, full()); len(v) > 0 {
		t.Fatalf("a fully-populated record should be clean, got: %v", v)
	}
	if v := checkOrderingNonVacuity(0, nil); len(v) == 0 {
		t.Fatal("a nil record must report that the battery never ran")
	}

	for _, tc := range []struct {
		name string
		bend func(*OrderingStats)
	}{
		{"never ran", func(s *OrderingStats) { s.checks = 0 }},
		{"no age tie", func(s *OrderingStats) { s.sawAgeTie = false }},
		{"single-row DESC", func(s *OrderingStats) { s.descRowsMax = 1 }},
		{"Top never truncated", func(s *OrderingStats) { s.topKTruncated = false }},
		{"aggregate filter never split", func(s *OrderingStats) { s.aggFilterSplit = false }},
		{"expand never produced a row", func(s *OrderingStats) { s.expandRows = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := full()
			tc.bend(st)
			if v := checkOrderingNonVacuity(0, st); len(v) == 0 {
				t.Fatalf("gate %q did not fire", tc.name)
			}
		})
	}
}

// TestOrderingSkipsAnAmbiguousModel proves the battery SKIPS rather than
// guesses when the oracle cannot define a total order — and that skipping is
// not a silent pass, because the terminal non-vacuity gate then reports that no
// check ever ran.
func TestOrderingSkipsAnAmbiguousModel(t *testing.T) {
	a, o := orderFixture(t)
	delete(o.nodes[o.byName["s5"]].Properties, "age")

	rows, complete := o.personOrderRows()
	if complete {
		t.Fatalf("personOrderRows must report an ageless Person as incomplete (rows=%d)", len(rows))
	}
	st := newOrderingStats()
	if v := CheckCypherSurfaceOrdering(0, o, a, st); len(v) > 0 {
		t.Fatalf("an ambiguous model must be skipped, not compared, got: %v", v)
	}
	if st.checks != 0 {
		t.Fatalf("a skipped check must not be recorded as run (checks=%d)", st.checks)
	}
	if v := checkOrderingNonVacuity(0, st); len(v) == 0 {
		t.Fatal("a run whose every ordering check was skipped must fail the non-vacuity gate")
	}
}

// TestPersonOrderRows_DuplicateNameIsIncomplete pins the other half of the
// totality guard: two Persons sharing a name make the expected sequence
// ambiguous, so the reference reports itself unusable.
func TestPersonOrderRows_DuplicateNameIsIncomplete(t *testing.T) {
	_, o := orderFixture(t)
	o.nodes[o.byName["s9"]].Properties["name"] = "s8"
	if _, complete := o.personOrderRows(); complete {
		t.Fatal("personOrderRows reported a duplicate-name model as totally ordered")
	}
}
