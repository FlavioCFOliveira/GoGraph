package sim

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Access-path parity oracle (rmp #2447).
//
// This file closes the simulator's most proven blind spot: rmp #2414 was a
// defect where a `$param` predicate full-scanned while the identical literal
// seeked — every answer still correct, only the plan collapsed — and the DST
// could not see it because it never invoked Explain/Profile and bound nil
// params in every surface probe. The engine-level contract is pinned in
// cypher/param_literal_parity_test.go; this checker brings the same invariant
// under the simulator's crash/recovery machinery.
//
// For each predicate shape (equality, bounded range, STARTS WITH, IN-list) on
// an indexed property the checker runs the literal spelling and the identical
// parameterised spelling and asserts three things:
//
//  1. both arms return the same result multiset (order-insensitive);
//  2. both arms resolve through the same access-path leaf operator
//     (seek vs scan), read from the engine's physical Explain rendering;
//  3. for the shapes the engine is known to seek, the LITERAL arm really
//     seeks — a pair that agrees on two scans would otherwise pass while
//     proving nothing (the vacuity the pinned engine test also guards).
//
// A companion plan-stability oracle captures the Explain rendering of a fixed
// probe set once and asserts it is byte-identical after every crash/recovery,
// so a plan-cache rebuild that silently changes an access path is a violation
// even when both arms of the pair change together.

// paritySeedMix derives the access-path parity checker's own draw stream from
// the master seed, so building the probe set never perturbs the workload or
// crash streams (the same isolation rule checkerSeedMix and crashSeedMix
// follow).
const paritySeedMix uint64 = 0xc4ceb9fe1a85ec53

// PlanEngine is the slice of the engine surface the access-path parity checker
// needs beyond [Engine]: the physical-plan rendering (Explain) and the profiled
// execution (Profile). The simulator's [EngineAdapter] satisfies it.
//
// # Concurrency contract
//
// Implementations need only be safe for single-goroutine use; the simulator
// never calls them concurrently.
type PlanEngine interface {
	Engine
	// Explain returns the engine's physical-plan rendering for query without
	// executing it.
	Explain(query string, params map[string]any) (string, error)
	// Profile executes the (read-only) query and returns the physical plan
	// annotated with per-operator rows, db-hits, and time.
	Profile(ctx context.Context, query string, params map[string]any) (string, error)
}

// ParityProbe is one predicate shape written twice — once with the value
// inlined, once parameterised — over the same indexed property. Literal and
// Param must be the SAME predicate: the checker asserts their plans and their
// result multisets agree.
type ParityProbe struct {
	// Shape names the predicate shape for violation messages
	// (e.g. "equality", "range", "starts-with", "in-list").
	Shape string
	// Literal is the query with the value(s) inlined. It must project a single
	// integer column — id(n) in the scenario probes — so results can be
	// compared as multisets.
	Literal string
	// Param is the identical query with the value(s) replaced by parameters.
	Param string
	// Params binds Param's parameters to exactly the inlined values.
	Params map[string]any
	// MustSeek asserts the LITERAL arm resolves through an index seek
	// (NodeByIndexSeek / NodeByIndexSeekSet / NodeByIndexRangeScan). Set it for
	// shapes the engine is known to seek on the probed data, so a pair that
	// agrees on two scans is reported instead of passing vacuously.
	MustSeek bool
}

// accessPathOps is the set of physical leaf operators that constitute an
// access path: how the engine reaches base data. Plan lines naming any other
// operator (Project, Filter, joins, aggregations) are not access paths and are
// ignored by the parity comparison, so an incidental change of a downstream
// operator does not masquerade as an access-path divergence.
var accessPathOps = map[string]bool{
	"AllNodesScan":         true,
	"NodeByLabelScan":      true,
	"NodeByIndexSeek":      true,
	"NodeByIndexSeekSet":   true,
	"NodeByIndexRangeScan": true,
	"AllNodesCountScan":    true,
	"LabelCountScan":       true,
	"ParallelCountScan":    true,
}

// seekOps is the subset of accessPathOps that resolves through an index.
var seekOps = map[string]bool{
	"NodeByIndexSeek":      true,
	"NodeByIndexSeekSet":   true,
	"NodeByIndexRangeScan": true,
}

// accessPathLeaves extracts, in plan order, the access-path operator names
// present in a rendered physical plan. The rendering is an indented tree whose
// node lines are "Name" or "Name [detail]" behind tree-drawing glyphs, so the
// operator name is the first token after the glyphs are trimmed.
func accessPathLeaves(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, " │├└─")
		name, _, _ := strings.Cut(trimmed, " ")
		if accessPathOps[name] {
			out = append(out, name)
		}
	}
	return out
}

// hasSeekLeaf reports whether any of the extracted access-path leaves is an
// index seek.
func hasSeekLeaf(leaves []string) bool {
	for _, l := range leaves {
		if seekOps[l] {
			return true
		}
	}
	return false
}

// CheckAccessPathParity runs every probe's literal and parameterised arm
// through engine and returns a violation for each parity breach found:
//
//   - a result-multiset divergence between the arms is a
//     [ViolationACIDConsistency] (the engine answered the same predicate two
//     different ways);
//   - an access-path divergence (one arm seeks, the other scans) with equal
//     results is a [ViolationOracleDeviation] — the rmp #2414 class, invisible
//     to every result-only oracle;
//   - a MustSeek probe whose LITERAL arm does not seek is a
//     [ViolationOracleDeviation], because the parity assertion would otherwise
//     hold vacuously on two scans;
//   - one Profile probe (the first MustSeek probe's literal arm) must report a
//     non-zero db-hit total, so a silently un-instrumented or data-blind
//     profile surfaces instead of passing.
//
// Violation messages name the shape, both plans, and both result summaries;
// result ids are sorted before rendering so messages are deterministic. The
// oracle argument is unused (the engine is cross-checked against itself, like
// [CheckIndexConsistency]) and kept for signature uniformity.
func CheckAccessPathParity(tick int64, _ *GraphOracle, engine PlanEngine, probes ...ParityProbe) []Violation {
	c := &InvariantChecker{}
	profiled := false
	for _, p := range probes {
		c.checkOneParityProbe(tick, engine, p)
		if p.MustSeek && !profiled {
			c.checkProfileDbHits(tick, engine, p)
			profiled = true
		}
	}
	return c.violations
}

// checkOneParityProbe runs both arms of one probe and appends a violation per
// parity breach.
func (c *InvariantChecker) checkOneParityProbe(tick int64, engine PlanEngine, p ParityProbe) {
	litPlan, err := engine.Explain(p.Literal, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "access-path parity",
			fmt.Sprintf("shape %q: Explain literal %q failed: %v", p.Shape, p.Literal, err))
		return
	}
	parPlan, err := engine.Explain(p.Param, p.Params)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "access-path parity",
			fmt.Sprintf("shape %q: Explain param %q failed: %v", p.Shape, p.Param, err))
		return
	}
	litIDs, err := runProbeIDs(engine, p.Literal, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "access-path parity",
			fmt.Sprintf("shape %q: literal arm %q failed: %v", p.Shape, p.Literal, err))
		return
	}
	parIDs, err := runProbeIDs(engine, p.Param, p.Params)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "access-path parity",
			fmt.Sprintf("shape %q: param arm %q failed: %v", p.Shape, p.Param, err))
		return
	}

	detail := func() string {
		return fmt.Sprintf("shape %q\nliteral query: %s\nparam query:   %s\nliteral plan:\n%s\nparam plan:\n%s\nliteral result: %s\nparam result:   %s",
			p.Shape, p.Literal, p.Param, litPlan, parPlan, summariseIDs(litIDs), summariseIDs(parIDs))
	}

	// 1. The answers must agree regardless of the plans: a seek that returns
	// different rows than the scan is a correctness bug, worse than any plan
	// collapse.
	if !equalIDMultisets(litIDs, parIDs) {
		c.add(ViolationACIDConsistency, tick, "access-path parity",
			"literal and parameter arms of the same predicate returned different result multisets\n"+detail())
		return
	}

	// 2. The access path must be the same for both spellings.
	litLeaves := accessPathLeaves(litPlan)
	parLeaves := accessPathLeaves(parPlan)
	if strings.Join(litLeaves, "→") != strings.Join(parLeaves, "→") {
		c.add(ViolationOracleDeviation, tick, "access-path parity",
			fmt.Sprintf("literal resolves via [%s] but the identical parameter form resolves via [%s]\n%s",
				strings.Join(litLeaves, " "), strings.Join(parLeaves, " "), detail()))
		return
	}

	// 3. Vacuity guard: a MustSeek pair that agrees on two scans compares
	// nothing — the literal arm regressing to a scan is itself the report.
	if p.MustSeek && !hasSeekLeaf(litLeaves) {
		c.add(ViolationOracleDeviation, tick, "access-path parity",
			"the LITERAL arm does not seek, so this pair proves nothing about parity\n"+detail())
	}
}

// checkProfileDbHits profiles the probe's literal arm and appends a violation
// when the plan reports a zero db-hit total for a query that must touch data.
func (c *InvariantChecker) checkProfileDbHits(tick int64, engine PlanEngine, p ParityProbe) {
	prof, err := engine.Profile(context.Background(), p.Literal, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "access-path profile",
			fmt.Sprintf("shape %q: Profile %q failed: %v", p.Shape, p.Literal, err))
		return
	}
	if totalDbHits(prof) == 0 {
		c.add(ViolationOracleDeviation, tick, "access-path profile",
			fmt.Sprintf("shape %q: Profile reports ZERO db-hits for a query that must touch data\nquery: %s\nprofile:\n%s",
				p.Shape, p.Literal, prof))
	}
}

// totalDbHits sums every "dbhits=N" annotation in a profiled plan rendering.
func totalDbHits(prof string) int64 {
	var total int64
	rest := prof
	for {
		i := strings.Index(rest, "dbhits=")
		if i < 0 {
			return total
		}
		rest = rest[i+len("dbhits="):]
		var n int64
		for rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			n = n*10 + int64(rest[0]-'0')
			rest = rest[1:]
		}
		total += n
	}
}

// runProbeIDs executes a single-integer-column probe query and returns the ids
// it produced, sorted ascending so callers compare and render multisets
// deterministically. It needs only the base [Engine] surface, so the
// seek-result checker (rmp #2450) shares it.
func runProbeIDs(engine Engine, query string, params map[string]any) ([]int64, error) {
	res, err := engine.Run(context.Background(), query, params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	var ids []int64
	for res.Next() {
		id, ok := res.ScalarInt()
		if !ok {
			return nil, fmt.Errorf("probe row %d: first column is not an integer id", len(ids))
		}
		ids = append(ids, id)
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// equalIDMultisets compares two id slices already sorted by runProbeIDs.
func equalIDMultisets(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// summariseIDs renders a sorted id multiset for a violation message: the count
// and the first few ids, enough to identify the divergence without flooding
// the report on a wide probe.
func summariseIDs(ids []int64) string {
	const maxShown = 8
	shown := ids
	suffix := ""
	if len(shown) > maxShown {
		shown = shown[:maxShown]
		suffix = " …"
	}
	return fmt.Sprintf("%d rows, ids=%v%s", len(ids), shown, suffix)
}

// PlanBaseline is the plan-stability oracle's captured state: the physical
// Explain rendering of both arms of a fixed probe set at one instant. A later
// [CheckPlanStability] against the same engine (or its crash-recovered
// successor) must reproduce every rendering byte-identically — a plan-cache
// rebuild that changes an access path is a violation even when both arms of a
// pair change together, which the parity check alone would miss.
//
// # Concurrency contract
//
// PlanBaseline is immutable after [CapturePlanBaseline] returns and is safe to
// read from one goroutine at a time; the simulator drives it from the single
// simulation goroutine.
type PlanBaseline struct {
	probes []ParityProbe
	// plans holds, for probe i, the literal rendering at 2i and the param
	// rendering at 2i+1.
	plans []string
}

// CapturePlanBaseline renders both arms of every probe through engine.Explain
// and freezes the result. Capture failures are returned as an error (the
// scenario cannot assert stability against a baseline it failed to take).
func CapturePlanBaseline(engine PlanEngine, probes ...ParityProbe) (*PlanBaseline, error) {
	b := &PlanBaseline{probes: probes, plans: make([]string, 0, 2*len(probes))}
	for _, p := range probes {
		lit, err := engine.Explain(p.Literal, nil)
		if err != nil {
			return nil, fmt.Errorf("sim: plan baseline: Explain literal %q: %w", p.Literal, err)
		}
		par, err := engine.Explain(p.Param, p.Params)
		if err != nil {
			return nil, fmt.Errorf("sim: plan baseline: Explain param %q: %w", p.Param, err)
		}
		b.plans = append(b.plans, lit, par)
	}
	return b, nil
}

// CheckPlanStability re-renders every baseline probe through engine and
// returns a [ViolationOracleDeviation] for each rendering that is not
// byte-identical to its captured baseline. It is meant to run after each
// crash/recovery (the plan cache was rebuilt from scratch) and at the end of a
// scenario; probes are compared in capture order so messages are
// deterministic.
func CheckPlanStability(tick int64, base *PlanBaseline, engine PlanEngine) []Violation {
	c := &InvariantChecker{}
	for i, p := range base.probes {
		c.checkOnePlanStable(tick, engine, p.Shape, "literal", p.Literal, nil, base.plans[2*i])
		c.checkOnePlanStable(tick, engine, p.Shape, "param", p.Param, p.Params, base.plans[2*i+1])
	}
	return c.violations
}

// checkOnePlanStable re-explains one arm and appends a violation when the
// rendering drifted from the baseline.
func (c *InvariantChecker) checkOnePlanStable(tick int64, engine PlanEngine, shape, arm, query string, params map[string]any, want string) {
	got, err := engine.Explain(query, params)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, "plan stability",
			fmt.Sprintf("shape %q (%s arm): Explain %q failed: %v", shape, arm, query, err))
		return
	}
	if got != want {
		c.add(ViolationOracleDeviation, tick, "plan stability",
			fmt.Sprintf("shape %q (%s arm): plan drifted from its baseline after a plan-cache rebuild\nquery: %s\nbaseline plan:\n%s\ncurrent plan:\n%s",
				shape, arm, query, want, got))
	}
}

// indexDiversityParityProbes draws the fixed probe set the index-diversity
// scenario asserts parity and plan stability over: one probe per predicate
// shape, on the scenario's three indexed properties, with values drawn from
// the checker's own sub-seed so the set is a pure function of the run seed and
// never perturbs the workload stream. Every drawn value targets bulk-loaded
// data the churn loop never deletes (bulk names are "p<i>"; churn only deletes
// "q<i>" names), and every range/prefix draw stays around 1% selectivity so
// the engine's seek gates (10% ceiling, 1024-node floor) keep seeking for the
// whole run.
//
// MustSeek reflects the engine's measured behaviour at HEAD: equality (hash),
// bounded numeric range (btree), and STARTS WITH (btree) seek; an IN-list
// resolves through a label scan on BOTH arms, so its probe asserts parity only
// — if the engine later learns an IN seek, parity still requires both arms to
// learn it together.
func indexDiversityParityProbes(seed *Seed) []ParityProbe {
	name := fmt.Sprintf("p%d", seed.IntN(indexDiversityBulk))
	lo := seed.IntN(495)
	hi := lo + 5
	city := fmt.Sprintf("c%d", 10+seed.IntN(90))
	in := []string{
		fmt.Sprintf("p%d", seed.IntN(indexDiversityBulk)),
		fmt.Sprintf("p%d", seed.IntN(indexDiversityBulk)),
		fmt.Sprintf("p%d", seed.IntN(indexDiversityBulk)),
	}
	return []ParityProbe{
		{
			Shape:    "equality",
			Literal:  fmt.Sprintf("MATCH (n:Person) WHERE n.name = '%s' RETURN id(n)", name),
			Param:    "MATCH (n:Person) WHERE n.name = $p RETURN id(n)",
			Params:   map[string]any{"p": name},
			MustSeek: true,
		},
		{
			Shape:    "range",
			Literal:  fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d AND n.age < %d RETURN id(n)", lo, hi),
			Param:    "MATCH (n:Person) WHERE n.age >= $lo AND n.age < $hi RETURN id(n)",
			Params:   map[string]any{"lo": int64(lo), "hi": int64(hi)},
			MustSeek: true,
		},
		{
			Shape:    "starts-with",
			Literal:  fmt.Sprintf("MATCH (n:Person) WHERE n.city STARTS WITH '%s' RETURN id(n)", city),
			Param:    "MATCH (n:Person) WHERE n.city STARTS WITH $p RETURN id(n)",
			Params:   map[string]any{"p": city},
			MustSeek: true,
		},
		{
			Shape:   "in-list",
			Literal: fmt.Sprintf("MATCH (n:Person) WHERE n.name IN ['%s','%s','%s'] RETURN id(n)", in[0], in[1], in[2]),
			Param:   "MATCH (n:Person) WHERE n.name IN $p RETURN id(n)",
			Params:  map[string]any{"p": []any{in[0], in[1], in[2]}},
		},
	}
}
