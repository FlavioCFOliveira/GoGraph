package sim

import (
	"context"
	"math"
	"testing"
)

// cityPerson is one fixture Person for the grouped-surface tests.
type cityPerson struct {
	name string
	age  int64
	city string
}

// seedCityFixture creates the given Persons (via [tmplCreatePersonCity]) and
// KNOWS edges in BOTH the engine and the oracle, so the grouped checker sees a
// consistent state.
func seedCityFixture(t *testing.T, persons []cityPerson, edges [][2]string) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	o := NewGraphOracle()
	ctx := context.Background()
	for _, p := range persons {
		params := map[string]any{"name": p.name, "age": p.age, "city": p.city}
		if _, err := a.RunWrite(ctx, tmplCreatePersonCity, params); err != nil {
			t.Fatalf("create %s: %v", p.name, err)
		}
		o.ApplyCreate(tmplCreatePersonCity, params)
	}
	for _, e := range edges {
		params := map[string]any{"a": e[0], "b": e[1]}
		if _, err := a.RunWrite(ctx, tmplCreateKnows, params); err != nil {
			t.Fatalf("knows %s->%s: %v", e[0], e[1], err)
		}
		o.ApplyCreate(tmplCreateKnows, params)
	}
	return a, o
}

// cityFixture is the standard grouped-surface fixture: six Persons over three
// cities with duplicated ages, arranged so at least one age value reaches the
// mixed-type grouping key through BOTH arms of the CASE — s1 (name ends in
// '1') projects age 40 as a FLOAT while s2 projects the same 40 as an INTEGER,
// and s21/s5 do the same for age 30 — which makes the exact INTEGER↔FLOAT
// single-group equivalence observable, not vacuous.
func cityFixture(t *testing.T) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	return seedCityFixture(t,
		[]cityPerson{
			{"s1", 40, "c0"},
			{"s2", 40, "c1"},
			{"s21", 30, "c2"},
			{"s3", 10, "c0"},
			{"s4", 20, "c1"},
			{"s5", 30, "c2"},
		},
		[][2]string{{"s1", "s2"}, {"s1", "s3"}, {"s2", "s3"}},
	)
}

// TestSurfaceGrouped_PassAndCatch verifies the grouped/DISTINCT/UNION/collect
// battery is clean on a consistent fixture — including the mixed-type
// grouping-key merge the fixture forces — and that each probe family FIRES
// when its oracle reference is perturbed (the sensitivity proof of rmp #2452).
func TestSurfaceGrouped_PassAndCatch(t *testing.T) {
	t.Run("baseline clean, mixed int/float keys merged", func(t *testing.T) {
		a, o := cityFixture(t)
		if v := CheckCypherSurfaceGrouped(0, o, a); len(v) > 0 {
			t.Fatalf("consistent fixture should be clean, got: %v", v)
		}
	})

	t.Run("perturbed city histogram fires the grouped check", func(t *testing.T) {
		a, o := cityFixture(t)
		// Move one Person to a city the engine never stored: the per-city
		// count histogram (and the sum histogram) no longer match.
		o.nodes[o.byName["s3"]].Properties["city"] = "zz"
		if v := CheckCypherSurfaceGrouped(0, o, a); len(v) == 0 {
			t.Fatal("grouped check FAILED to detect a perturbed city histogram")
		}
	})

	t.Run("perturbed age histogram fires the mixed-key and sum checks", func(t *testing.T) {
		a, o := cityFixture(t)
		o.nodes[o.byName["s4"]].Properties["age"] = int64(21)
		if v := CheckCypherSurfaceGrouped(0, o, a); len(v) == 0 {
			t.Fatal("grouped check FAILED to detect a perturbed age histogram")
		}
	})

	t.Run("removed oracle edges fire the DISTINCT checks", func(t *testing.T) {
		a, o := cityFixture(t)
		for k := range o.edges {
			delete(o.edges, k)
		}
		if v := CheckCypherSurfaceGrouped(0, o, a); len(v) == 0 {
			t.Fatal("DISTINCT checks FAILED to detect removed oracle edges")
		}
	})

	t.Run("ghost person fires the UNION and collect checks", func(t *testing.T) {
		a, o := cityFixture(t)
		o.ApplyCreate(tmplCreatePersonCity, map[string]any{"name": "ghost", "age": int64(55), "city": "c9"})
		if v := CheckCypherSurfaceGrouped(0, o, a); len(v) == 0 {
			t.Fatal("UNION/collect checks FAILED to detect a ghost person")
		}
	})
}

// TestExpectedAggregates_HandComputed pins [expectedAggregates] against
// hand-computed values on small fixed inputs, proving the reference itself
// encodes the right definitions (sample vs population standard deviation,
// linear-interpolation percentileCont, nearest-rank percentileDisc).
func TestExpectedAggregates_HandComputed(t *testing.T) {
	pin := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	// ages {10,20,30,40}: mean 25; Σ(x-25)² = 225+25+25+225 = 500;
	// sample stdev = sqrt(500/3); population = sqrt(500/4) = sqrt(125);
	// percentileCont(0.5): pos = 0.5·3 = 1.5 → (20+30)/2 = 25;
	// percentileDisc(0.5): idx = ceil(0.5·4)-1 = 1 → 20.
	e4 := expectedAggregates([]int64{10, 20, 30, 40})
	pin("avg{10..40}", e4.Avg, 25.0)
	pin("stDev{10..40}", e4.StDev, math.Sqrt(500.0/3.0))
	pin("stDevP{10..40}", e4.StDevP, math.Sqrt(125.0))
	pin("pCont{10..40}", e4.PCont, 25.0)
	if e4.PDisc != 20 {
		t.Errorf("pDisc{10..40} = %d, want 20 (nearest rank, ceil(0.5·4)-1 = index 1)", e4.PDisc)
	}
	// A swapped (population↔sample) reference would differ by the factor
	// sqrt(4/3) here — far beyond the checker's 1e-9 tolerance.
	if floatClose(e4.StDev, e4.StDevP) {
		t.Fatal("fixture degenerate: sample and population stdev coincide, the swap sensitivity below would be vacuous")
	}

	// ages {10,20,30} (odd n): pCont pos = 1 → exactly 20; pDisc idx =
	// ceil(1.5)-1 = 1 → 20; sample stdev = sqrt((100+0+100)/2) = 10;
	// population = sqrt(200/3).
	e3 := expectedAggregates([]int64{10, 20, 30})
	pin("avg{10,20,30}", e3.Avg, 20.0)
	pin("stDev{10,20,30}", e3.StDev, 10.0)
	pin("stDevP{10,20,30}", e3.StDevP, math.Sqrt(200.0/3.0))
	pin("pCont{10,20,30}", e3.PCont, 20.0)
	if e3.PDisc != 20 {
		t.Errorf("pDisc{10,20,30} = %d, want 20", e3.PDisc)
	}

	// n = 2 boundary: {10,20} → stdev sqrt(50), stdevp 5, pCont 15, pDisc 10.
	e2 := expectedAggregates([]int64{10, 20})
	pin("stDev{10,20}", e2.StDev, math.Sqrt(50.0))
	pin("stDevP{10,20}", e2.StDevP, 5.0)
	pin("pCont{10,20}", e2.PCont, 15.0)
	if e2.PDisc != 10 {
		t.Errorf("pDisc{10,20} = %d, want 10", e2.PDisc)
	}
}

// TestExactAggregates_EngineMatches_WrongDefinitionFires runs the exact
// aggregate probe against the real engine: the correct reference is clean,
// and a reference built with a WRONG definition — population and sample
// standard deviation swapped, or a percentile computed by the other method —
// fires. Together with [TestExpectedAggregates_HandComputed] this proves both
// that the reference encodes the openCypher definitions and that the engine
// implements exactly those definitions (sample stDev, population stDevP,
// interpolated percentileCont, nearest-rank percentileDisc — pinned from
// cypher/funcs/aggregators.go).
func TestExactAggregates_EngineMatches_WrongDefinitionFires(t *testing.T) {
	a, _ := seedCityFixture(t, []cityPerson{
		{"s1", 10, "c0"}, {"s2", 20, "c1"}, {"s3", 30, "c0"}, {"s4", 40, "c1"},
	}, nil)
	want := expectedAggregates([]int64{10, 20, 30, 40})

	if v := compareExactAggregates(0, want, a); len(v) > 0 {
		t.Fatalf("correct reference should be clean, got: %v", v)
	}

	swapped := want
	swapped.StDev, swapped.StDevP = want.StDevP, want.StDev
	if v := compareExactAggregates(0, swapped, a); len(v) == 0 {
		t.Fatal("a population/sample-swapped stdev reference did NOT fire: the probe cannot distinguish the definitions")
	}

	wrongDisc := want
	wrongDisc.PDisc = 25 // the interpolated (percentileCont) value; nearest rank is 20
	if v := compareExactAggregates(0, wrongDisc, a); len(v) == 0 {
		t.Fatal("an interpolated percentileDisc reference did NOT fire: the probe cannot distinguish disc from cont")
	}

	wrongCont := want
	wrongCont.PCont = 20 // the nearest-rank (percentileDisc) value; interpolation gives 25
	if v := compareExactAggregates(0, wrongCont, a); len(v) == 0 {
		t.Fatal("a nearest-rank percentileCont reference did NOT fire: the probe cannot distinguish cont from disc")
	}
}
