package sim

// surface_order.go — ordering, pagination and multi-part query structure probes
// for the cypher-surface battery (rmp #2460).
//
// Until #2460 every ORDER BY the simulator issued was a SINGLE ASCENDING key on
// a string: there was no DESC anywhere, no multi-key sort, no sort on an
// expression or on an aggregate, and no tie-break exercise at all — even though
// the surface workload draws ages from 0..99 over hundreds of Persons, so ages
// collide heavily and the secondary key decides most of the order. The fused
// Top path (ORDER BY + LIMIT) was never compared against the full Sort;
// SKIP/LIMIT were only ever string-formatted into the query text, never bound as
// parameters; LIMIT 0 and a SKIP past the end were never issued; and only ONE
// WITH stage ever appeared, so two-stage pipelines and the classic
// "top-k then expand" shape were absent.
//
// Every probe here is asserted against a reference the oracle computes from its
// own model, and the run-level [OrderingStats] gate proves the sensitive arms —
// the tie-break, the reversed order, the truncating LIMIT — were actually
// exercised rather than passing on a degenerate graph.

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

const (
	// orderModulus is the divisor of the expression-sort probe's key
	// (ORDER BY n.age % 10). The surface ages are 0..99, so the key has ten
	// heavily populated buckets and the secondary name key decides nearly every
	// row inside a bucket — which is exactly what makes the probe a tie-break
	// exercise rather than a re-run of the age sort.
	orderModulus = 10
	// orderTopK is the LIMIT of the Top-vs-Sort equivalence probe: the engine
	// must return exactly the first orderTopK rows of the unlimited ordering.
	orderTopK = 5
	// orderExpandTopK is the LIMIT of the "top-k then expand" multi-part probe.
	orderExpandTopK = 10
	// orderPageSkip and orderPageLimit are the pagination probe's page window,
	// issued both as literals and as bound parameters.
	orderPageSkip  = 3
	orderPageLimit = 5
	// orderTwoStageLimit is the LIMIT of the two-stage WITH pipeline probe, and
	// orderTwoStageMinCount its group-size floor (WHERE c > 1).
	orderTwoStageLimit    = 3
	orderTwoStageMinCount = 1
)

// OrderingStats accumulates what the ordering probes actually OBSERVED over a
// run, so the terminal gate ([checkOrderingNonVacuity]) can prove the sensitive
// arms were exercised instead of passing on a graph too small or too uniform to
// distinguish a working sort from a broken one.
//
// A nil *OrderingStats is a valid receiver for every note method, so a one-shot
// (test) call may pass nil when it does not care about the run-level record.
//
// # Concurrency contract
//
// OrderingStats is NOT safe for concurrent use; the simulator drives the
// battery from a single goroutine.
type OrderingStats struct {
	// checks counts how many times the ordering battery ran with a usable model.
	checks int
	// sawAgeTie reports that at least one check ran on a model in which two
	// Persons shared an age, so the DESC probe's secondary key really had a tie
	// to break.
	sawAgeTie bool
	// descRowsMax is the largest row count a DESC-ordered probe returned. A
	// reversed order is unobservable below two rows.
	descRowsMax int
	// topKTruncated reports that at least one check had strictly more rows
	// available than the Top probe's LIMIT, so the fused Top path really
	// truncated the ordering rather than returning everything.
	topKTruncated bool
	// aggFilterSplit reports that at least one check ran the WITH…WHERE
	// aggregate filter on a threshold that KEPT some groups and DROPPED others,
	// so the predicate discriminated rather than passing (or rejecting) all.
	aggFilterSplit bool
	// expandRows reports that the "top-k then expand" probe counted at least one
	// relationship, so the second MATCH stage produced rows at least once.
	expandRows bool
}

// newOrderingStats returns an empty run-level record.
func newOrderingStats() *OrderingStats { return &OrderingStats{} }

// noteCheck records one battery run on a usable model.
func (s *OrderingStats) noteCheck(sawTie bool) {
	if s == nil {
		return
	}
	s.checks++
	s.sawAgeTie = s.sawAgeTie || sawTie
}

// noteDescRows records the row count of a DESC-ordered probe.
func (s *OrderingStats) noteDescRows(n int) {
	if s == nil {
		return
	}
	if n > s.descRowsMax {
		s.descRowsMax = n
	}
}

// noteTopK records whether the Top probe's LIMIT truncated the ordering.
func (s *OrderingStats) noteTopK(truncated bool) {
	if s == nil {
		return
	}
	s.topKTruncated = s.topKTruncated || truncated
}

// noteAggFilter records whether the aggregate filter both kept and dropped
// groups on this run.
func (s *OrderingStats) noteAggFilter(split bool) {
	if s == nil {
		return
	}
	s.aggFilterSplit = s.aggFilterSplit || split
}

// noteExpandRows records whether the top-k-then-expand probe counted at least
// one relationship on this run.
func (s *OrderingStats) noteExpandRows(rows int64) {
	if s == nil {
		return
	}
	s.expandRows = s.expandRows || rows > 0
}

// orderExpect is the oracle-side expected ROW ORDER of each ordering probe,
// injectable exactly as [aggExpect] is for the exact-aggregate probes (rmp
// #2452): the checker compares against whatever reference it is handed, so a
// test can prove the probes FIRE when the reference's comparator is perturbed
// (secondary key dropped, DESC read as ASC) and therefore that they pin the
// comparator itself, not merely "some order".
type orderExpect struct {
	// AgeDescNameAsc is the expected (name, age) sequence of
	// ORDER BY n.age DESC, n.name ASC — a TOTAL order, since the surface names
	// are distinct.
	AgeDescNameAsc []groupedRow
	// NameDesc is the expected name sequence of ORDER BY n.name DESC.
	NameDesc []string
	// ModTen is the expected (name, n.age % orderModulus) sequence of
	// ORDER BY n.age % 10, n.name — an ordering on an EXPRESSION, whose
	// projected key value the probe also asserts.
	ModTen []groupedRow
	// CityByCountDesc is the expected (city, count) sequence of the grouped
	// probe RETURN n.city, count(*) AS c ORDER BY c DESC, n.city ASC — an
	// ordering on an AGGREGATE.
	CityByCountDesc []groupedRow
	// CityGroupsKnown reports whether CityByCountDesc is a usable reference: it
	// is false when a modelled Person lacks a city, in which case the engine
	// would emit a null-key group the oracle does not model.
	CityGroupsKnown bool
}

// expectedOrdering derives the expected row sequences from the oracle's own
// model: rows is one entry per modelled Person (name + integer age) and stats
// the per-city histogram, usable only when cityKnown. The comparators are the
// openCypher orderings the probes request, written out once here so a test can
// pin them against hand-computed fixtures:
//
//   - age DESC then name ASC — descending on the primary key, ascending on the
//     secondary, which is where a dropped or mis-signed tie-break shows;
//   - name DESC — the exact reverse of the ascending probe the battery already
//     had;
//   - (age mod orderModulus) ASC then name ASC — an ordering whose key is an
//     EXPRESSION rather than a stored property;
//   - count DESC then city ASC — an ordering whose primary key is an AGGREGATE,
//     with the city tie-break making the sequence unique.
func expectedOrdering(rows []personOrderRow, stats []cityStat, cityKnown bool) orderExpect {
	byAgeDesc := slices.Clone(rows)
	slices.SortFunc(byAgeDesc, func(a, b personOrderRow) int {
		if a.Age != b.Age {
			return cmp.Compare(b.Age, a.Age) // DESC on the primary key
		}
		return cmp.Compare(a.Name, b.Name) // ASC on the secondary key
	})
	ageDesc := make([]groupedRow, len(byAgeDesc))
	for i, r := range byAgeDesc {
		ageDesc[i] = groupedRow{Key: r.Name, Val: r.Age}
	}

	nameDesc := make([]string, 0, len(rows))
	for _, r := range rows {
		nameDesc = append(nameDesc, r.Name)
	}
	slices.Sort(nameDesc)
	slices.Reverse(nameDesc)

	byMod := slices.Clone(rows)
	slices.SortFunc(byMod, func(a, b personOrderRow) int {
		if am, bm := a.Age%orderModulus, b.Age%orderModulus; am != bm {
			return cmp.Compare(am, bm)
		}
		return cmp.Compare(a.Name, b.Name)
	})
	modTen := make([]groupedRow, len(byMod))
	for i, r := range byMod {
		modTen[i] = groupedRow{Key: r.Name, Val: r.Age % orderModulus}
	}

	want := orderExpect{
		AgeDescNameAsc:  ageDesc,
		NameDesc:        nameDesc,
		ModTen:          modTen,
		CityGroupsKnown: cityKnown,
	}
	if cityKnown {
		byCount := slices.Clone(stats)
		slices.SortFunc(byCount, func(a, b cityStat) int {
			if a.Count != b.Count {
				return cmp.Compare(b.Count, a.Count) // DESC on the aggregate
			}
			return cmp.Compare(a.City, b.City)
		})
		want.CityByCountDesc = make([]groupedRow, len(byCount))
		for i, st := range byCount {
			want.CityByCountDesc[i] = groupedRow{Key: st.City, Val: st.Count}
		}
	}
	return want
}

// CheckCypherSurfaceOrdering runs the ordering, pagination and multi-part
// query-structure probes of the cypher-surface battery, each asserted as full
// row-SEQUENCE equality against a reference computed independently from the
// oracle model (rmp #2460):
//
//   - ORDER BY n.age DESC, n.name ASC — descending primary key with an
//     ascending tie-break, over a workload whose ages collide heavily;
//   - ORDER BY n.name DESC — the reverse of the battery's ascending probe;
//   - ORDER BY n.age % 10, n.name — an ordering on an EXPRESSION, whose
//     projected key value is asserted alongside the sequence;
//   - RETURN n.city, count(*) AS c ORDER BY c DESC, n.city ASC — an ordering on
//     an AGGREGATE;
//   - Top-vs-Sort equivalence: the same ordering with LIMIT k must return
//     exactly the first k rows of the unlimited ordering, and the two arms must
//     resolve through DIFFERENT physical operators (Top vs Sort) — see
//     [checkTopFusion];
//   - pagination: the same page written with LITERAL and with PARAMETERISED
//     SKIP/LIMIT, LIMIT 0, a SKIP past the end, and SKIP/LIMIT over a DESC
//     ordering — see [checkOrderingPagination];
//   - multi-part structure: a two-stage WITH pipeline, "top-k then expand", and
//     WITH … WHERE on an aggregated value — see [checkOrderingMultiPart].
//
// It runs on the quiescent graph, periodically, after each crash/recovery, and
// at the end. st accumulates the run-level observations the terminal
// non-vacuity gate reads and may be nil.
//
// The probes are skipped while the model holds a Person without both a string
// name and an integer age, or two Persons sharing a name: the expected row
// sequence would then not be a total order, and a probe whose reference is
// ambiguous can only produce noise. The surface workload binds both properties
// and issues distinct names, so this is a guard, not an expectation.
func CheckCypherSurfaceOrdering(tick int64, oracle *GraphOracle, engine *EngineAdapter, st *OrderingStats) []Violation {
	rows, complete := oracle.personOrderRows()
	if !complete || len(rows) == 0 {
		return nil
	}
	stats, cityComplete := oracle.personCityStats()
	want := expectedOrdering(rows, stats, cityComplete)
	st.noteCheck(hasAgeTie(rows))

	vs := compareOrdering(tick, &want, engine, st)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	vs = append(vs, checkOrderingPagination(tick, names, engine)...)
	vs = append(vs, checkOrderingMultiPart(tick, &want, stats, cityComplete, oracle.knowsOutDegreeByName(), engine, st)...)
	return vs
}

// hasAgeTie reports whether two modelled Persons share an age — the condition
// under which the secondary sort key decides an order at all.
func hasAgeTie(rows []personOrderRow) bool {
	seen := make(map[int64]bool, len(rows))
	for _, r := range rows {
		if seen[r.Age] {
			return true
		}
		seen[r.Age] = true
	}
	return false
}

// compareOrdering issues the four ordering probes and the Top-vs-Sort
// equivalence, comparing each against want. Every comparison is
// order-SENSITIVE: the probes exist precisely to pin the sequence, so a
// set-wise comparison would not distinguish a working comparator from a broken
// one.
func compareOrdering(tick int64, want *orderExpect, engine *EngineAdapter, st *OrderingStats) []Violation {
	ctx := context.Background()
	var vs []Violation
	fail := func(op, msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}
	queryErr := func(op string, err error) {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s query error: %v", op, err)})
	}

	// ORDER BY n.age DESC, n.name ASC — the tie-break probe. Its unlimited
	// result is also the reference arm of the Top-vs-Sort equivalence below.
	full, err := collectGroupedRows(ctx, engine, orderQueryAgeDescNameAsc)
	if err != nil {
		queryErr("ORDER BY age DESC, name ASC", err)
	} else {
		st.noteDescRows(len(full))
		if diff := diffOrderedPairs(full, want.AgeDescNameAsc); diff != "" {
			fail("ORDER BY age DESC, name ASC", diff)
		}
	}

	// ORDER BY n.name DESC — the reverse of the battery's ascending probe.
	if got, err := collectStringRows(ctx, engine, "MATCH (n:Person) RETURN n.name ORDER BY n.name DESC"); err != nil {
		queryErr("ORDER BY name DESC", err)
	} else {
		st.noteDescRows(len(got))
		if !equalStrings(got, want.NameDesc) {
			fail("ORDER BY name DESC", fmt.Sprintf("engine=%v, oracle=%v", got, want.NameDesc))
		}
	}

	// ORDER BY n.age % 10, n.name — an ordering on an EXPRESSION. The projected
	// key is asserted alongside the sequence, so an engine that ordered by the
	// right key but projected the wrong value is caught too.
	if got, err := collectGroupedRows(ctx, engine,
		fmt.Sprintf("MATCH (n:Person) RETURN n.name AS name, n.age %% %d AS m ORDER BY n.age %% %d, n.name",
			orderModulus, orderModulus)); err != nil {
		queryErr("ORDER BY age % 10, name", err)
	} else if diff := diffOrderedPairs(got, want.ModTen); diff != "" {
		fail("ORDER BY age % 10, name", diff)
	}

	// ORDER BY c DESC, n.city ASC — an ordering on an AGGREGATE.
	if want.CityGroupsKnown {
		if got, err := collectGroupedRows(ctx, engine,
			"MATCH (n:Person) RETURN n.city AS city, count(*) AS c ORDER BY c DESC, n.city ASC"); err != nil {
			queryErr("ORDER BY count DESC, city ASC", err)
		} else if diff := diffOrderedPairs(got, want.CityByCountDesc); diff != "" {
			fail("ORDER BY count DESC, city ASC", diff)
		}
	}

	// Top-vs-Sort equivalence on the SAME ordering.
	if err == nil {
		vs = append(vs, compareTopAgainstSort(tick, want, full, engine, st)...)
	}
	return vs
}

// orderQueryAgeDescNameAsc is the unlimited two-key ordering probe. It is
// shared by the tie-break probe, the Top-vs-Sort equivalence, and the plan
// assertion, so all three speak about exactly the same ordering.
const orderQueryAgeDescNameAsc = "MATCH (n:Person) RETURN n.name AS name, n.age AS age ORDER BY n.age DESC, n.name ASC"

// orderQueryAgeDescNameAscTop is [orderQueryAgeDescNameAsc] with a LIMIT, the
// arm the engine answers through the fused Top operator.
var orderQueryAgeDescNameAscTop = fmt.Sprintf("%s LIMIT %d", orderQueryAgeDescNameAsc, orderTopK)

// compareTopAgainstSort asserts that ORDER BY … LIMIT k returns exactly the
// first k rows of the unlimited ordering. It is a real equivalence rather than
// a tautology because the engine answers the two arms with DIFFERENT physical
// operators — the unlimited arm sorts everything (Sort), the limited arm keeps a
// bounded heap (Top) — which [checkTopFusion] asserts from the plan rendering.
//
// The limited arm is compared against BOTH references: the oracle's own prefix
// (so a defect shared by the two arms is still caught) and the unlimited arm the
// engine itself just returned (so the two operators are pinned to each other).
func compareTopAgainstSort(tick int64, want *orderExpect, full []groupedRow, engine *EngineAdapter, st *OrderingStats) []Violation {
	const op = "Top vs Sort (ORDER BY … LIMIT)"
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}

	got, err := collectGroupedRows(context.Background(), engine, orderQueryAgeDescNameAscTop)
	if err != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s query error: %v", op, err)}}
	}
	st.noteTopK(len(full) > orderTopK)
	if diff := diffOrderedPairs(got, prefixRows(want.AgeDescNameAsc, orderTopK)); diff != "" {
		fail("vs oracle prefix: " + diff)
	}
	if diff := diffOrderedPairs(got, prefixRows(full, orderTopK)); diff != "" {
		fail("vs the engine's own unlimited ordering: " + diff)
	}
	return append(vs, checkTopFusion(tick, engine)...)
}

// prefixRows returns the first k rows of s (all of them when it is shorter).
func prefixRows(s []groupedRow, k int) []groupedRow {
	if len(s) <= k {
		return s
	}
	return s[:k]
}

// orderingOps is the set of physical operators that implement ordering and
// pagination. Plan lines naming any other operator are ignored, so an
// incidental change elsewhere in the plan cannot masquerade as a change of the
// ordering strategy.
var orderingOps = map[string]bool{"Sort": true, "Top": true, "Limit": true, "Skip": true}

// orderingPlanOps extracts, in plan order, the ordering/pagination operator
// names present in a rendered physical plan. It reads the same rendering
// grammar [accessPathLeaves] does: indented lines of "Name" or "Name [detail]"
// behind tree-drawing glyphs, so the operator name is the first token once the
// glyphs are trimmed.
func orderingPlanOps(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, " │├└─")
		name, _, _ := strings.Cut(trimmed, " ")
		if orderingOps[name] {
			out = append(out, name)
		}
	}
	return out
}

// checkTopFusion asserts the two arms of the Top-vs-Sort equivalence really do
// resolve through different operators: the unlimited ordering through Sort, the
// LIMITed one through the fused Top. Without this the equivalence could hold
// vacuously — two arms answered by the identical plan prove nothing about the
// fused path, exactly as an access-path parity pair agreeing on two scans
// proves nothing about seeking (rmp #2447).
//
// Both arms are issued with NIL parameters and a LITERAL limit on purpose. At
// HEAD the fusion fires only for a literal LIMIT with no SKIP: `ORDER BY …
// LIMIT $m` plans as Limit→Sort and `ORDER BY … SKIP 3 LIMIT 5` as
// Limit→Skip→Sort. That asymmetry is a planner property, not a documented
// contract, so it is deliberately NOT asserted here — the pagination probes pin
// the parameterised arms by RESULT, which is the contract that must hold.
func checkTopFusion(tick int64, engine *EngineAdapter) []Violation {
	const op = "Top/Sort plan divergence"
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}
	sortPlan, err := engine.Explain(orderQueryAgeDescNameAsc, nil)
	if err != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("explain (unlimited) error: %v", err)}}
	}
	topPlan, err := engine.Explain(orderQueryAgeDescNameAscTop, nil)
	if err != nil {
		return []Violation{{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("explain (limited) error: %v", err)}}
	}
	sortOps, topOps := orderingPlanOps(sortPlan), orderingPlanOps(topPlan)
	if !slices.Contains(sortOps, "Sort") || slices.Contains(sortOps, "Top") {
		fail(fmt.Sprintf("the unlimited ordering must plan as Sort (and never Top); ops=%v plan=\n%s", sortOps, sortPlan))
	}
	if !slices.Contains(topOps, "Top") || slices.Contains(topOps, "Sort") {
		fail(fmt.Sprintf("ORDER BY … LIMIT %d must plan as the fused Top (and never Sort); ops=%v plan=\n%s",
			orderTopK, topOps, topPlan))
	}
	return vs
}

// checkOrderingPagination pins the pagination contract on the ascending name
// ordering, whose expected page the oracle computes by slicing its own sorted
// name list:
//
//   - the same page written with LITERAL and with PARAMETERISED SKIP/LIMIT must
//     equal the oracle's slice AND each other — the literal/parameter parity
//     theme of rmp #2447, here on the pagination clauses rather than on a
//     predicate;
//   - LIMIT 0 returns ZERO rows (literal and parameterised), never the whole
//     result;
//   - a SKIP past the end returns ZERO rows rather than erroring or wrapping;
//   - SKIP/LIMIT composed with a DESC ordering pages the REVERSED sequence.
//
// The LIMIT 0 and past-the-end contracts are pinned as VERIFIED against this
// engine rather than assumed: both return an empty result with no error.
func checkOrderingPagination(tick int64, names []string, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	fail := func(op, msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}
	queryErr := func(op string, err error) {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s query error: %v", op, err)})
	}
	page := func(op, query string, params map[string]any, wantPage []string) []string {
		got, err := collectStringRowsParams(ctx, engine, query, params)
		if err != nil {
			queryErr(op, err)
			return nil
		}
		if !equalStrings(got, wantPage) {
			fail(op, fmt.Sprintf("engine=%v, oracle=%v", got, wantPage))
		}
		return got
	}

	wantAsc := pageOf(names, orderPageSkip, orderPageLimit)
	lit := page("SKIP/LIMIT literal",
		fmt.Sprintf("MATCH (n:Person) RETURN n.name ORDER BY n.name SKIP %d LIMIT %d", orderPageSkip, orderPageLimit),
		nil, wantAsc)
	par := page("SKIP/LIMIT parameterised",
		"MATCH (n:Person) RETURN n.name ORDER BY n.name SKIP $k LIMIT $m",
		map[string]any{"k": int64(orderPageSkip), "m": int64(orderPageLimit)}, wantAsc)
	if !equalStrings(lit, par) {
		fail("SKIP/LIMIT literal vs parameterised",
			fmt.Sprintf("the same page answered two ways: literal=%v, parameterised=%v", lit, par))
	}

	// LIMIT 0 — zero rows, both spellings.
	page("LIMIT 0 literal", "MATCH (n:Person) RETURN n.name ORDER BY n.name LIMIT 0", nil, nil)
	page("LIMIT 0 parameterised", "MATCH (n:Person) RETURN n.name ORDER BY n.name LIMIT $m",
		map[string]any{"m": int64(0)}, nil)

	// SKIP past the end — zero rows, both spellings.
	past := int64(len(names) + 1)
	page("SKIP past end literal",
		fmt.Sprintf("MATCH (n:Person) RETURN n.name ORDER BY n.name SKIP %d", past), nil, nil)
	page("SKIP past end parameterised", "MATCH (n:Person) RETURN n.name ORDER BY n.name SKIP $k",
		map[string]any{"k": past}, nil)

	// DESC ordering paged: the window is taken from the REVERSED sequence, so a
	// direction the engine dropped after the sort surfaces here even though the
	// row COUNT would be identical.
	desc := slices.Clone(names)
	slices.Reverse(desc)
	page("SKIP/LIMIT over DESC", "MATCH (n:Person) RETURN n.name ORDER BY n.name DESC SKIP $k LIMIT $m",
		map[string]any{"k": int64(orderPageSkip), "m": int64(orderPageLimit)},
		pageOf(desc, orderPageSkip, orderPageLimit))
	return vs
}

// pageOf returns the SKIP skip LIMIT limit window of s: the empty slice when
// skip is at or past the end, and the tail when fewer than limit rows remain.
func pageOf(s []string, skip, limit int) []string {
	if skip >= len(s) {
		return nil
	}
	out := s[skip:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// checkOrderingMultiPart drives the multi-part query shapes the battery never
// issued — every earlier probe had at most ONE WITH stage — against references
// the oracle computes from its own model:
//
//   - a TWO-STAGE pipeline: group and filter in the first WITH, then order,
//     truncate and collect in the second. The literal `c > 1` floor is the
//     shape reported in rmp #2460; the discriminating filter is the third probe
//     below, whose threshold is chosen so both sides are populated;
//   - TOP-K THEN EXPAND: order Persons, keep the top k, and expand from exactly
//     those k. The oracle predicts the count from the SAME top-k selection,
//     which is well defined because the ordering is made total by the name
//     tie-break — an ambiguous top-k would make the expectation unknowable, so
//     the ordering is strengthened rather than the assertion weakened;
//   - WITH … WHERE on an AGGREGATED value, with the group-size threshold set to
//     the MEDIAN group size so the predicate keeps some groups and drops others
//     (the same both-sides-populated discipline the filtered-subquery probes
//     use for their age floor).
func checkOrderingMultiPart(tick int64, want *orderExpect, stats []cityStat, cityKnown bool,
	outDeg map[string]int64, engine *EngineAdapter, st *OrderingStats) []Violation {
	ctx := context.Background()
	var vs []Violation
	fail := func(op, msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: op, Message: msg})
	}
	queryErr := func(op string, err error) {
		vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s query error: %v", op, err)})
	}

	if cityKnown {
		// Two-stage WITH pipeline. stats is ascending by city, so the expected
		// collected list is the first orderTwoStageLimit surviving cities in that
		// same ascending order.
		var wantCities []string
		for _, s := range stats {
			if s.Count > orderTwoStageMinCount && len(wantCities) < orderTwoStageLimit {
				wantCities = append(wantCities, s.City)
			}
		}
		query := fmt.Sprintf("MATCH (n:Person) WITH n.city AS city, count(*) AS c WHERE c > %d"+
			" WITH city ORDER BY city LIMIT %d RETURN collect(city)", orderTwoStageMinCount, orderTwoStageLimit)
		if got, err := collectListStrings(ctx, engine, query); err != nil {
			queryErr("two-stage WITH pipeline", err)
		} else if !equalStrings(got, wantCities) {
			fail("two-stage WITH pipeline", fmt.Sprintf("engine=%v, oracle=%v", got, wantCities))
		}

		// WITH … WHERE on an aggregated value, at a threshold that splits.
		thr := medianGroupCount(stats)
		var wantGroups, wantRows, dropped int64
		for _, s := range stats {
			if s.Count > thr {
				wantGroups++
				wantRows += s.Count
			} else {
				dropped++
			}
		}
		st.noteAggFilter(wantGroups > 0 && dropped > 0)
		got, err := collectGroupedRow2(ctx, engine,
			"MATCH (n:Person) WITH n.city AS city, count(*) AS c WHERE c > $minC RETURN count(city) AS groups, sum(c) AS rows",
			map[string]any{"minC": thr})
		switch {
		case err != nil:
			queryErr("WITH … WHERE aggregate", err)
		case got[0] != wantGroups || got[1] != wantRows:
			fail("WITH … WHERE aggregate", fmt.Sprintf("c > %d: engine=(groups=%d, rows=%d), oracle=(groups=%d, rows=%d)",
				thr, got[0], got[1], wantGroups, wantRows))
		}
	}

	// Top-k then expand: the oracle sums the out-degrees of exactly the Persons
	// the total ordering puts in the top k.
	var wantExpand int64
	for _, r := range prefixRows(want.AgeDescNameAsc, orderExpandTopK) {
		wantExpand += outDeg[r.Key]
	}
	query := fmt.Sprintf("MATCH (n:Person) WITH n ORDER BY n.age DESC, n.name ASC LIMIT %d"+
		" MATCH (n)-[:KNOWS]->(m) RETURN count(m)", orderExpandTopK)
	if got, err := surfaceScalar(ctx, engine, query); err != nil {
		queryErr("top-k then expand", err)
	} else {
		st.noteExpandRows(got)
		if got != wantExpand {
			fail("top-k then expand", fmt.Sprintf("engine=%d, oracle=%d (top %d by age DESC, name ASC)",
				got, wantExpand, orderExpandTopK))
		}
	}
	return vs
}

// medianGroupCount returns the middle per-city row count of stats — the
// threshold the aggregate-filter probe compares against, chosen so that BOTH
// sides of `c > threshold` are populated whenever the group sizes have any
// spread at all. A constant floor would leave the predicate passing every group
// on the surface workload, which spreads hundreds of Persons over ten cities.
func medianGroupCount(stats []cityStat) int64 {
	if len(stats) == 0 {
		return 0
	}
	counts := make([]int64, 0, len(stats))
	for _, s := range stats {
		counts = append(counts, s.Count)
	}
	slices.Sort(counts)
	return counts[len(counts)/2]
}

// diffOrderedPairs compares two (string, integer) row sequences ORDER
// SENSITIVELY, returning "" when they are identical and a message naming the
// first differing row otherwise.
func diffOrderedPairs(got, want []groupedRow) string {
	if len(got) != len(want) {
		return fmt.Sprintf("row count: engine=%d, oracle=%d (engine=%v, oracle=%v)",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Sprintf("row %d: engine=(%q,%d), oracle=(%q,%d)",
				i, got[i].Key, got[i].Val, want[i].Key, want[i].Val)
		}
	}
	return ""
}

// collectStringRowsParams is [collectStringRows] with bound parameters: it
// drains a one-string-column query, preserving result order.
func collectStringRowsParams(ctx context.Context, engine *EngineAdapter, query string, params map[string]any) ([]string, error) {
	res, err := engine.Run(ctx, query, params)
	if err != nil {
		return nil, err
	}
	var out []string
	for res.Next() {
		s, ok := res.StringAt(0)
		if !ok {
			_ = res.Close()
			return nil, fmt.Errorf("row %d is not a string", len(out))
		}
		out = append(out, s)
	}
	derr := res.Err()
	_ = res.Close()
	return out, derr
}

// collectGroupedRow2 runs a single-row two-integer-column query with bound
// parameters and returns both columns.
func collectGroupedRow2(ctx context.Context, engine *EngineAdapter, query string, params map[string]any) ([2]int64, error) {
	var out [2]int64
	res, err := engine.Run(ctx, query, params)
	if err != nil {
		return out, err
	}
	if res.Next() {
		a, okA := res.IntAt(0)
		b, okB := res.IntAt(1)
		if !okA || !okB {
			_ = res.Close()
			return out, fmt.Errorf("row is not (integer, integer)")
		}
		out[0], out[1] = a, b
	} else {
		_ = res.Close()
		return out, fmt.Errorf("query returned no row")
	}
	derr := res.Err()
	_ = res.Close()
	return out, derr
}

// collectListStrings drains a single-row query whose only column is a LIST of
// strings, returning the list in element order. A non-list column, or a
// non-string element, is an error rather than a silently skipped row.
func collectListStrings(ctx context.Context, engine *EngineAdapter, query string) ([]string, error) {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	if res.Next() {
		lst, ok := rawValueAt(res, 0).(expr.ListValue)
		if !ok {
			_ = res.Close()
			return nil, fmt.Errorf("column 0 is %T, want a list", rawValueAt(res, 0))
		}
		for i, v := range lst {
			s, ok := v.(expr.StringValue)
			if !ok {
				_ = res.Close()
				return nil, fmt.Errorf("list element %d is %T, want a string", i, v)
			}
			out = append(out, string(s))
		}
	}
	derr := res.Err()
	_ = res.Close()
	return out, derr
}

// checkOrderingNonVacuity is the terminal assert-something-was-seen gate of the
// ordering coverage (rmp #2460). A clean run must have observed, at least once,
// every condition that makes the ordering probes able to fail:
//
//   - the battery ran at all on a usable model;
//   - two Persons shared an age, so the DESC probe's secondary key really broke
//     a tie (the whole point of ordering on the heavily colliding age);
//   - a DESC probe returned more than one row, since a reversed order is
//     indistinguishable from an ascending one below two rows;
//   - more rows were available than the Top probe's LIMIT, so the fused Top path
//     genuinely truncated;
//   - the aggregate filter kept some groups and dropped others, so WITH … WHERE
//     on an aggregate discriminated;
//   - the top-k-then-expand probe counted at least one relationship, so its
//     second MATCH stage produced rows.
func checkOrderingNonVacuity(tick int64, st *OrderingStats) []Violation {
	var vs []Violation
	fail := func(msg string) {
		vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "ordering non-vacuity", Message: msg})
	}
	if st == nil || st.checks == 0 {
		fail("the ordering battery never ran on a usable model: every ordering probe was skipped")
		return vs
	}
	if !st.sawAgeTie {
		fail("no check saw two Persons sharing an age: the DESC probe's secondary key never broke a tie")
	}
	if st.descRowsMax < 2 {
		fail(fmt.Sprintf("the DESC probes never returned more than %d row(s): a reversed order is unobservable", st.descRowsMax))
	}
	if !st.topKTruncated {
		fail(fmt.Sprintf("no check had more than %d rows available: the fused Top path never truncated an ordering", orderTopK))
	}
	if !st.aggFilterSplit {
		fail("no check ran WITH … WHERE on an aggregate threshold that both kept and dropped groups: the filter never discriminated")
	}
	if !st.expandRows {
		fail(fmt.Sprintf("the top-%d-then-expand probe never counted a relationship: its second MATCH stage never produced a row", orderExpandTopK))
	}
	return vs
}
