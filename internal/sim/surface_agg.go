package sim

// surface_agg.go — grouped aggregation, DISTINCT rows, UNION over graph data,
// and exact aggregate oracles for the cypher-surface battery (rmp #2452).
//
// Until #2452 every aggregation the sim issued was GLOBAL (a single group), so
// the grouping-key path, the columnar group kernel, and eager aggregation
// never ran under the DST; RETURN/WITH DISTINCT as a row operator never ran
// (only count(DISTINCT …) did); UNION/UNION ALL touched only literal
// constants; avg and the percentiles were checked by self-referential
// invariants a broken engine could still satisfy; collect(n.name) was issued
// but never checked; and stDev/stDevP were never issued. The probes here close
// each gap with references computed independently from the oracle model.

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// CheckCypherSurfaceGrouped runs the grouped-aggregation, DISTINCT-row, UNION
// and collect probes of the cypher-surface battery, each asserted as full
// row-set equality against a reference computed independently from the oracle
// model (rmp #2452):
//
//   - grouped count(*) and grouped sum(n.age) BY n.city vs the oracle's
//     per-city histogram ([GraphOracle.personCityStats]), ordered by city;
//   - a mixed-type grouping key (a CASE yielding FLOAT for some rows and
//     INTEGER for others) vs the oracle's per-age histogram, exercising the
//     exact INTEGER↔FLOAT grouping equivalence the engine pins end-to-end in
//     cypher/intfloat_exact_equality_test.go (rmp #2050): equal int and float
//     keys must land in ONE group, and 2^53-scale keys must not collapse;
//   - RETURN DISTINCT b.name over a pattern vs the oracle's distinct-target
//     set, plus a WITH DISTINCT mid-pipeline stage feeding a count;
//   - UNION over graph rows (complementary and OVERLAPPING predicates — the
//     overlap makes the dedup observable) vs the full sorted name set, and
//     UNION ALL of overlapping predicates vs the SUM of the arm cardinalities
//     (duplicate preservation);
//   - collect(n.name), sorted, vs the oracle's sorted name slice.
//
// Probes that read n.age or n.city are skipped while the model holds a Person
// without that property (impossible in the surface workload, which always
// binds both); the DISTINCT and collect probes run unconditionally. It runs on
// the quiescent graph, periodically, after each crash/recovery, and at the end.
func CheckCypherSurfaceGrouped(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	fail := func(op, msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}
	queryErr := func(op string, err error) {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s query error: %v", op, err)})
	}

	ages := oracle.personAges()
	names := oracle.personNamesSorted()
	agesComplete := len(ages) == oracle.personCount() // every Person carries an integer age

	// Grouped aggregation BY n.city: full row-set equality, ordered by city.
	if stats, complete := oracle.personCityStats(); complete {
		rows, err := collectGroupedRows(ctx, engine,
			"MATCH (n:Person) RETURN n.city AS city, count(*) AS c ORDER BY city")
		if err != nil {
			queryErr("grouped count by city", err)
		} else if diff := diffGroupedRows(rows, stats, func(st cityStat) int64 { return st.Count }); diff != "" {
			fail("grouped count by city", diff)
		}
		if agesComplete {
			rows, err := collectGroupedRows(ctx, engine,
				"MATCH (n:Person) RETURN n.city AS city, sum(n.age) AS s ORDER BY city")
			if err != nil {
				queryErr("grouped sum(age) by city", err)
			} else if diff := diffGroupedRows(rows, stats, func(st cityStat) int64 { return st.AgeSum }); diff != "" {
				fail("grouped sum(age) by city", diff)
			}
		}
	}

	// Mixed-type grouping key: names ending in '1' project their age as FLOAT,
	// every other row as INTEGER, so one numeric age value can reach the
	// grouping key through BOTH types and must still form a single group.
	if agesComplete {
		wantHist := make(map[int64]int64, len(ages))
		for _, a := range ages {
			wantHist[a]++
		}
		checkMixedKeyGrouping(ctx, tick, engine, wantHist, &vs)
	}

	// RETURN DISTINCT over a pattern, as a row operator.
	targets := oracle.knowsTargetNamesDistinct()
	got, err := collectStringRows(ctx, engine,
		"MATCH (a:Person)-[:KNOWS]->(b) RETURN DISTINCT b.name ORDER BY b.name")
	if err != nil {
		queryErr("RETURN DISTINCT b.name", err)
	} else if !equalStrings(got, targets) {
		fail("RETURN DISTINCT b.name", fmt.Sprintf("engine=%v, oracle=%v", got, targets))
	}
	// WITH DISTINCT mid-pipeline, feeding a count.
	if n, err := surfaceScalar(ctx, engine,
		"MATCH (a:Person)-[:KNOWS]->(b) WITH DISTINCT b.name AS nm RETURN count(nm)"); err != nil {
		queryErr("WITH DISTINCT count", err)
	} else if n != int64(len(targets)) {
		fail("WITH DISTINCT count", fmt.Sprintf("engine=%d, oracle=%d", n, len(targets)))
	}

	// UNION / UNION ALL over graph rows.
	if agesComplete {
		var ltSixty, geForty int64
		for _, a := range ages {
			if a < 60 {
				ltSixty++
			}
			if a >= 40 {
				geForty++
			}
		}
		// Complementary predicates: the union is exactly the full name set.
		got, err := collectStringRows(ctx, engine,
			"MATCH (n:Person) WHERE n.age < 50 RETURN n.name AS name"+
				" UNION MATCH (n:Person) WHERE n.age >= 50 RETURN n.name AS name")
		if err != nil {
			queryErr("UNION complementary", err)
		} else {
			sort.Strings(got)
			if !equalStrings(got, names) {
				fail("UNION complementary", fmt.Sprintf("engine=%v, oracle=%v", got, names))
			}
		}
		// Overlapping predicates: every name with age in [40,60) reaches UNION
		// through BOTH arms, so a broken dedup yields duplicates.
		got, err = collectStringRows(ctx, engine,
			"MATCH (n:Person) WHERE n.age < 60 RETURN n.name AS name"+
				" UNION MATCH (n:Person) WHERE n.age >= 40 RETURN n.name AS name")
		if err != nil {
			queryErr("UNION overlapping dedup", err)
		} else {
			sort.Strings(got)
			if !equalStrings(got, names) {
				fail("UNION overlapping dedup", fmt.Sprintf("engine=%v, oracle=%v", got, names))
			}
		}
		// UNION ALL of the same overlapping predicates preserves duplicates:
		// the row count is exactly the sum of the arm cardinalities.
		gotAll, err := collectStringRows(ctx, engine,
			"MATCH (n:Person) WHERE n.age < 60 RETURN n.name AS name"+
				" UNION ALL MATCH (n:Person) WHERE n.age >= 40 RETURN n.name AS name")
		if err != nil {
			queryErr("UNION ALL overlapping", err)
		} else if int64(len(gotAll)) != ltSixty+geForty {
			fail("UNION ALL overlapping", fmt.Sprintf("row count: engine=%d, oracle=%d (%d + %d)",
				len(gotAll), ltSixty+geForty, ltSixty, geForty))
		}
	}

	// collect(n.name): the sorted collected list equals the sorted name slice.
	res, err := engine.Run(ctx, "MATCH (n:Person) RETURN collect(n.name)", nil)
	if err != nil {
		queryErr("collect(n.name)", err)
		return vs
	}
	var collected []string
	badElem := false
	if res.Next() {
		lst, ok := rawValueAt(res, 0).(expr.ListValue)
		if !ok {
			badElem = true
		} else {
			for _, v := range lst {
				s, ok := v.(expr.StringValue)
				if !ok {
					badElem = true
					break
				}
				collected = append(collected, string(s))
			}
		}
	}
	derr := res.Err()
	_ = res.Close()
	switch {
	case derr != nil:
		queryErr("collect(n.name)", derr)
	case badElem:
		fail("collect(n.name)", "result is not a list of strings")
	default:
		sort.Strings(collected)
		if !equalStrings(collected, names) {
			fail("collect(n.name)", fmt.Sprintf("engine=%v, oracle=%v", collected, names))
		}
	}
	return vs
}

// groupedRow is one engine row of a (string, integer) two-column query, in
// result order: the grouping key and its integer aggregate for the grouped
// probes here, and the (name, sort key) pair the ordering probes compare in
// sequence (rmp #2460).
type groupedRow struct {
	Key string
	Val int64
}

// collectGroupedRows drains a two-column (string, integer) grouped query.
func collectGroupedRows(ctx context.Context, engine *EngineAdapter, query string) ([]groupedRow, error) {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	var out []groupedRow
	for res.Next() {
		k, okK := res.StringAt(0)
		v, okV := res.IntAt(1)
		if !okK || !okV {
			_ = res.Close()
			return nil, fmt.Errorf("row %d is not (string, integer)", len(out))
		}
		out = append(out, groupedRow{Key: k, Val: v})
	}
	derr := res.Err()
	_ = res.Close()
	return out, derr
}

// diffGroupedRows compares the engine's grouped rows against the oracle stats
// (both ordered by city) using val to select which per-city aggregate the
// probe asserted. It returns "" on full row-set equality.
func diffGroupedRows(rows []groupedRow, stats []cityStat, val func(cityStat) int64) string {
	if len(rows) != len(stats) {
		return fmt.Sprintf("group count: engine=%d, oracle=%d (engine=%v)", len(rows), len(stats), rows)
	}
	for i, st := range stats {
		if rows[i].Key != st.City || rows[i].Val != val(st) {
			return fmt.Sprintf("row %d: engine=(%q,%d), oracle=(%q,%d)",
				i, rows[i].Key, rows[i].Val, st.City, val(st))
		}
	}
	return ""
}

// checkMixedKeyGrouping issues the mixed-type grouping-key probe — a CASE that
// yields toFloat(n.age) when the name ends in '1' and the INTEGER n.age
// otherwise — and asserts the group set equals the oracle's per-age histogram.
// Because equal INTEGER and FLOAT keys must group together (the exact
// cross-type equivalence of rmp #2050), the engine must produce exactly one
// group per distinct age with the full count, never a split int-group and
// float-group pair; a repeated numeric key is therefore flagged directly.
func checkMixedKeyGrouping(ctx context.Context, tick int64, engine *EngineAdapter, want map[int64]int64, vs *[]Violation) {
	const op = "mixed int/float grouping key"
	query := "MATCH (n:Person) RETURN CASE WHEN n.name ENDS WITH '1' THEN toFloat(n.age) ELSE n.age END AS k, count(*) AS c"
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		*vs = append(*vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("query error: %v", err)})
		return
	}
	got := make(map[int64]int64, len(want))
	var bad string
	for res.Next() {
		var key int64
		switch t := rawValueAt(res, 0).(type) {
		case expr.IntegerValue:
			key = int64(t)
		case expr.FloatValue:
			f := float64(t)
			key = int64(f)
			if float64(key) != f {
				bad = fmt.Sprintf("non-integral float group key %v", f)
			}
		default:
			bad = fmt.Sprintf("group key is %T, want a numeric", t)
		}
		if bad != "" {
			break
		}
		if _, dup := got[key]; dup {
			// Two rows with the same numeric key means the engine SPLIT an
			// int-keyed and a float-keyed group that must be one group.
			bad = fmt.Sprintf("numeric key %d appears in two groups (int/float split)", key)
			break
		}
		c, ok := res.IntAt(1)
		if !ok {
			bad = "count column is not an integer"
			break
		}
		got[key] = c
	}
	derr := res.Err()
	_ = res.Close()
	if derr != nil {
		*vs = append(*vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("drain error: %v", derr)})
		return
	}
	if bad != "" {
		*vs = append(*vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: bad})
		return
	}
	if len(got) != len(want) {
		*vs = append(*vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op,
			Message: fmt.Sprintf("group count: engine=%d, oracle=%d", len(got), len(want))})
		return
	}
	for k, wc := range want {
		if got[k] != wc {
			*vs = append(*vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op,
				Message: fmt.Sprintf("group %d: engine count=%d, oracle=%d", k, got[k], wc)})
			return
		}
	}
}

// aggExpect is the oracle-side exact expectation for the five global
// aggregates the surface battery pins (rmp #2452). Floats are compared with a
// 1e-9 relative tolerance ([floatClose]); pDisc is exact (the engine returns
// an IntegerValue when every input is an integer, as every surface age is).
type aggExpect struct {
	Avg    float64
	StDev  float64
	StDevP float64
	PCont  float64
	PDisc  int64
}

// expectedAggregates derives the exact aggregate references from the oracle's
// ascending-sorted integer ages, for n >= 2. The formulas mirror the pinned
// engine definitions, VERIFIED in cypher/funcs/aggregators.go (never assumed):
//
//   - avg — arithmetic mean as a float ([funcs.AvgAgg]);
//   - stDev — SAMPLE standard deviation, sqrt(Σ(x-mean)²/(n-1)), NULL for
//     n < 2 ([funcs.StdDevAgg], Welford online — the two-pass sum here agrees
//     within far less than the 1e-9 relative tolerance);
//   - stDevP — POPULATION standard deviation, sqrt(Σ(x-mean)²/n)
//     ([funcs.StdDevPAgg]);
//   - percentileCont — ANSI-SQL PERCENTILE_CONT linear interpolation at
//     pos = p·(n-1) over the sorted values ([funcs.PercentileContAgg];
//     cypher/aggregation_percentile_test.go pins p50 of 1..10 = 5.5 e2e);
//   - percentileDisc — nearest-rank, the sorted element at
//     ceil(p·n)-1 clamped to [0, n-1] ([funcs.PercentileDiscAgg]), which
//     PRESERVES the integer representation of the chosen element.
func expectedAggregates(ages []int64) aggExpect {
	n := len(ages)
	fs := make([]float64, n)
	var sum float64
	for i, a := range ages {
		fs[i] = float64(a)
		sum += fs[i]
	}
	mean := sum / float64(n)
	var m2 float64
	for _, f := range fs {
		d := f - mean
		m2 += d * d
	}
	// percentileCont at p = 0.5: linear interpolation at pos = 0.5·(n-1).
	pos := 0.5 * float64(n-1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	frac := pos - float64(lo)
	pCont := fs[lo]*(1-frac) + fs[hi]*frac
	// percentileDisc at p = 0.5: nearest rank, ceil(0.5·n)-1.
	idx := int(math.Ceil(0.5*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	return aggExpect{
		Avg:    mean,
		StDev:  math.Sqrt(m2 / float64(n-1)),
		StDevP: math.Sqrt(m2 / float64(n)),
		PCont:  pCont,
		PDisc:  ages[idx],
	}
}

// compareExactAggregates issues one five-aggregate probe query —
// avg / stDev / stDevP / percentileCont(·,0.5) / percentileDisc(·,0.5) over
// n.age — and asserts each column equals want: the float columns within the
// 1e-9 relative tolerance, percentileDisc exactly (an IntegerValue, since
// every surface age is an integer). The caller guarantees at least two
// modelled ages (stDev is NULL below that).
func compareExactAggregates(tick int64, want aggExpect, engine *EngineAdapter) []Violation {
	const op = "exact aggregates"
	query := "MATCH (n:Person) RETURN avg(n.age), stDev(n.age), stDevP(n.age)," +
		" percentileCont(n.age, 0.5), percentileDisc(n.age, 0.5)"
	res, err := engine.Run(context.Background(), query, nil)
	if err != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("query error: %v", err)}}
	}
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}
	if res.Next() {
		floatCols := [...]struct {
			name string
			want float64
		}{
			{"avg(n.age)", want.Avg},
			{"stDev(n.age)", want.StDev},
			{"stDevP(n.age)", want.StDevP},
			{"percentileCont(n.age,0.5)", want.PCont},
		}
		for i, col := range floatCols {
			f, ok := rawValueAt(res, i).(expr.FloatValue)
			if !ok {
				fail(fmt.Sprintf("%s: engine returned %T, want a float", col.name, rawValueAt(res, i)))
				continue
			}
			if !floatClose(float64(f), col.want) {
				fail(fmt.Sprintf("%s: engine=%v, oracle=%v (rel tol 1e-9)", col.name, float64(f), col.want))
			}
		}
		if d, ok := rawValueAt(res, 4).(expr.IntegerValue); !ok {
			fail(fmt.Sprintf("percentileDisc(n.age,0.5): engine returned %T, want an integer (all inputs are integers)", rawValueAt(res, 4)))
		} else if int64(d) != want.PDisc {
			fail(fmt.Sprintf("percentileDisc(n.age,0.5): engine=%d, oracle=%d", int64(d), want.PDisc))
		}
	} else {
		fail("probe returned no row")
	}
	derr := res.Err()
	_ = res.Close()
	if derr != nil {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("drain error: %v", derr)})
	}
	return vs
}

// floatClose reports whether got equals want within a 1e-9 relative tolerance
// (absolute below magnitude one, so a want of exactly 0 still compares).
func floatClose(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

// collectStringRows drains a one-string-column query, preserving result order
// and failing loudly on a non-string cell (the callers' projections are
// name-valued by construction, so a null or non-string row is a defect, not a
// row to skip). It is the unparameterised spelling of
// [collectStringRowsParams].
func collectStringRows(ctx context.Context, engine *EngineAdapter, query string) ([]string, error) {
	return collectStringRowsParams(ctx, engine, query, nil)
}

// surfaceScalar runs a single-row integer query and returns its first column.
func surfaceScalar(ctx context.Context, engine *EngineAdapter, query string) (int64, error) {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	var n int64
	if res.Next() {
		n, _ = res.IntAt(0)
	}
	derr := res.Err()
	_ = res.Close()
	return n, derr
}

// rawValueAt returns the engine's expr.Value at column i of the current row,
// reaching through the concrete result adapter exactly as [CheckExprLiterals]
// does — the checker's narrow [Result] view has no float or list accessor.
func rawValueAt(res Result, i int) expr.Value {
	return res.(*resultAdapter).res.ValueAt(i)
}
