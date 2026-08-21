package sim

import (
	"context"
	"fmt"
	"strings"
)

// Intersect-planner probe oracle (rmp #2490).
//
// The conjunctive intersection path (cypher/index_intersect_plan.go, #2134)
// composes TWO single-property indexes on the same label into one access path by
// ANDing their range bitmaps, and it reaches that decision through a BUDGETED
// cardinality count per conjunct (#2266): `RangeCountFrom` for a string conjunct
// with no upper bound, `RangeCount` for one with both bounds. None of it was
// reachable under the DST, because no simulated scenario ever issued a
// two-predicate conjunction over two btree-indexed properties of one label —
// every indexed probe the simulator drove constrained a SINGLE property, which
// is the shipped single-property range seek and a different code path.
//
// index-diversity is the scenario that can reach it: it declares three indexes
// on :Person, two of them BTREE (`age`, numeric; `city`, string), and a
// conjunction over those two is exactly the shape that composes. The hash index
// on `name` deliberately cannot participate — the recogniser requires a BOUND
// BTREE per conjunct — which is what the solo-seek control below turns into
// evidence rather than an assumption.
//
// # What each arm proves, and why the rendered bound is the evidence
//
// The composition is visible in the physical plan: [exec.NodeByIndexRangeScan]
// renders one `range=lo..hi` per contributing index, joined by `∩ range=`
// (cypher/exec/plan_detail.go). That marker is specific — the multi-label
// conjunction renders its labels joined by a bare `∩` with no `range=` — so its
// presence means two indexes really were composed.
//
// The rendered STRING bound then identifies which SHAPE reached the budgeted
// gate, because the plan's bound and the count's branch are selected by the same
// extracted predicate:
//
//   - `"c95"..+inf` — a quoted low bound with nothing above it — is rendered only
//     when the extracted `pred.hi` is nil, which is precisely the condition
//     `budgetedStringRangeCount` switches on to call `RangeCountFrom`.
//   - `"c42".."c43"(excl)` — both bounds present — is rendered only when
//     `pred.hi` is non-nil, the branch that calls `RangeCount`.
//
// Be exact about what that buys. The fragment pins the PREDICATE SHAPE that
// selects the branch, so it proves this arm drove the unbounded-above (resp.
// bounded) path of the gate. It does NOT pin the branch's body: swapping
// `RangeCountFrom(lo, budget)` for `RangeCount(lo, "\xff\xff\xff\xff", budget)`
// is measured to change nothing observable here, because every key in this
// fixture sorts below that sentinel, so the two calls return the same number.
// Such a rewrite is an EQUIVALENT mutant on this data and no result- or
// plan-level oracle can distinguish it; what the DST owns is that the shape
// reaches the gate at all, and the counting functions themselves are pinned by
// their own unit tests (graph/index/btree/range_from_test.go).
//
// The quoting is what separates the string side from the numeric one, whose bound
// is unquoted.
//
// # The AND ORDER is what makes the counted VALUE observable
//
// The plan renders the parts in the order the planner ANDed them, which is
// ascending exact count with ties broken on the property key. That order is the
// only place a budgeted count's VALUE surfaces at all — the executed bitmaps, the
// rendered bounds and the answers are all independent of it — so the
// unbounded-above arm predicts the order from the counts it derived
// CLIENT-SIDE and requires the plan to match. A count taken over the wrong key
// space then shows up as a flipped order, which was measured: rewriting
// `RangeCountFrom(lo, budget)` as `RangeCount(lo, lo, budget)` (counting one
// city bucket instead of the tail) is invisible to every result and every bound,
// and inverts this order.
//
// The order is predicted rather than pinned to a constant, so nothing here
// encodes an assumption about which side is cheaper; and the windows are drawn so
// the string side is comfortably the LARGER of the two, which is what keeps an
// under-count detectable rather than seed-dependent.
//
// The BOUNDED (STARTS WITH) arm deliberately makes no order claim. The planner
// counts a prefix over the CLOSED interval [prefix, prefix+1], which on this data
// includes the next city value as well, so a client-side prediction of its count
// would have to re-implement the operator's superset semantics — modelling the
// engine instead of observing it.
//
// # The answers are verified against a reference that touches no index
//
// Every arm's id-multiset is compared against one plain label scan filtered
// client-side, exactly as [IndexSeekResults] does for the single-property
// shapes: an intersected read is verified against base data, never against
// another engine path. A composed probe additionally must retain its residual
// Filter, because each part is only a SUPERSET of its conjunct (#F-EXEC1) — a
// composition that lost the Filter would return extra rows, which the multiset
// comparison catches, and the plan assertion names the cause.
//
// Do NOT add half-open `>= lo` SINGLE-property range probes here: rmp #2450
// already ships them in [IndexSeekResults], sized so RangeFrom /
// RangeCountFrom engage on this scenario's data.

// intersectSeedMix derives this checker's own draw stream from the master seed,
// so drawing the conjunct windows perturbs neither the workload, crash, parity,
// seek-result nor statistics streams (the isolation rule [paritySeedMix],
// [seekResultsSeedMix] and [statsSeedMix] follow).
const intersectSeedMix uint64 = 0x2490a5c31d7e6b94

// intersectOp is the op label every intersect-probe violation carries.
const intersectOp = "index intersect probes"

// intersectComposedMarker is what a composed intersection renders in the
// physical plan and nothing else does: one `∩ range=` per index beyond the
// primary. The multi-label conjunction joins its labels with a bare `∩`, so the
// `range=` suffix is what makes this marker specific to the index composition.
const intersectComposedMarker = "∩ range="

// intersectRangeScanOp is the physical leaf a composed intersection runs as: the
// composition reuses [exec.NodeByIndexRangeScan] rather than introducing an
// operator of its own, so the operator NAME alone cannot distinguish a composed
// probe from a single-property seek. That is precisely why the solo control arm
// exists.
const intersectRangeScanOp = "NodeByIndexRangeScan"

// intersectReferenceQuery is the independent reference: a plain label scan
// projecting the properties the probes constrain plus the node key, filtered
// client-side. It carries no predicate at all, so the engine has nothing to push
// into an index and the reference cannot be served by the structures under test.
const intersectReferenceQuery = "MATCH (n:Person) RETURN id(n), n.age, n.city"

// IndexIntersectProbes is the intersect-planner probe set: a seed-drawn
// two-predicate conjunction over the two BTREE-indexed properties of :Person, in
// both its unbounded-above ([boundStringRange.RangeCountFrom]) and its bounded
// ([boundStringRange.RangeCount]) string spelling, each result-verified against
// an independent full-scan reference and each required to compose in the
// physical plan — plus a single-property control that must seek through the SAME
// operator WITHOUT composing, so "the marker is present" cannot pass for "an
// index was used".
//
// It is stateful so [IndexIntersectProbes.Finish] can assert non-vacuity over
// the whole run: a run in which nothing ever composed, or in which every
// composed arm returned zero rows, proved nothing about the intersection path.
//
// # Concurrency contract
//
// IndexIntersectProbes is NOT safe for concurrent use; the simulator drives it
// from the single simulation goroutine.
type IndexIntersectProbes struct {
	// ageFloor is the numeric conjunct's inclusive floor (n.age >= ageFloor).
	// Drawn high enough that its exact in-range count stays well inside the
	// per-conjunct selectivity ceiling for the whole run: the churn loop only
	// ever mints ages in [0, MaxTicks], so no committed write can widen this
	// window while the label population only grows.
	ageFloor int64
	// cityFloor is the UNBOUNDED-ABOVE string conjunct's inclusive floor
	// (n.city >= cityFloor). It names a "c9k" value, so it selects exactly the
	// 10-k highest of the 100 cycled city values.
	cityFloor string
	// cityPrefix is the BOUNDED string conjunct, written as STARTS WITH, which
	// the planner rewrites to a closed [prefix, prefix+1) interval. It names
	// exactly one of the 100 cycled city values.
	cityPrefix string
	// composedSeen counts composed plans actually observed, rowsSeen the
	// composed arms that returned at least one row, and soloSeekSeen the control
	// arms that seeked through the range-scan operator without composing. All
	// three must be non-zero for the run to be non-vacuous.
	composedSeen int
	rowsSeen     int
	soloSeekSeen int
}

// NewIndexIntersectProbes draws the two conjunct windows from seed.
//
// bulk is the size of the scenario's bulk load, used only to reject a fixture
// too small for the planner's population floor
// (cypher.rangeSeekMinLabelPopulation is 1024): below it every conjunct is
// declined whatever its selectivity, so every arm would assert a composition
// that cannot happen. A caller passing less than 1100 is a programmer error and
// panics here rather than producing a scenario that fails for the wrong reason.
func NewIndexIntersectProbes(seed *Seed, bulk int) *IndexIntersectProbes {
	if bulk < 1100 {
		panic(fmt.Sprintf("sim: NewIndexIntersectProbes: bulk=%d is below the planner's "+
			"label-population floor (1024) plus margin: no conjunct could ever compose", bulk))
	}
	return &IndexIntersectProbes{
		// [485, 489]: selects 15..11 of the 500 cycled ages, i.e. 3.0%..2.2% of
		// the bulk population, against a 10% per-conjunct ceiling. The churn loop
		// only ever mints ages in [0, MaxTicks], so no committed write can widen
		// this window and its exact count is fixed for the whole run.
		ageFloor: int64(485 + seed.IntN(5)),
		// "c94" or "c95": selects 6 or 5 of the 100 cycled cities, i.e. 6%..5%.
		// Deliberately the LARGER of the two conjuncts — at least 1.67x the age
		// side — so the ascending-count AND order is stable across the run AND an
		// under-count of this side inverts it detectably.
		cityFloor: fmt.Sprintf("c9%d", 4+seed.IntN(2)),
		// "c10".."c99": one cycled city, so the closed prefix interval the
		// planner counts spans two, i.e. ~2%.
		cityPrefix: fmt.Sprintf("c%d", 10+seed.IntN(90)),
	}
}

// intersectReference is the client-side-filtered view of one reference scan: the
// authoritative id-multisets the two composed arms must reproduce.
type intersectReference struct {
	// fromIDs is (age >= ageFloor AND city >= cityFloor), the unbounded-above
	// arm; prefixIDs is (age >= ageFloor AND city STARTS WITH cityPrefix), the
	// bounded arm; soloIDs is (city >= cityFloor), the control.
	fromIDs   []int64
	prefixIDs []int64
	soloIDs   []int64
	// ageCount and cityFromCount are the per-conjunct cardinalities of the
	// unbounded-above arm, counted client-side. They are EXACTLY the numbers the
	// planner's two budgeted counts must produce for that arm — both bounds it
	// derives there are closed on the low side and unbounded above, so neither
	// count is a superset — which is what lets the arm predict the AND order.
	ageCount      int
	cityFromCount int
}

// scanReference runs the plain label scan once and buckets every node id by
// filtering the projected property values client-side. A row whose property is
// absent or of another kind is skipped for that bucket, exactly as the engine's
// own predicates would skip it.
func (k *IndexIntersectProbes) scanReference(engine Engine) (*intersectReference, error) {
	res, err := engine.Run(context.Background(), intersectReferenceQuery, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()

	ref := &intersectReference{}
	for res.Next() {
		id, ok := res.IntAt(0)
		if !ok {
			continue
		}
		age, ageOK := res.IntAt(1)
		city, cityOK := res.StringAt(2)
		if ageOK && age >= k.ageFloor {
			ref.ageCount++
		}
		if cityOK && city >= k.cityFloor {
			ref.cityFromCount++
			ref.soloIDs = append(ref.soloIDs, id)
			if ageOK && age >= k.ageFloor {
				ref.fromIDs = append(ref.fromIDs, id)
			}
		}
		if cityOK && strings.HasPrefix(city, k.cityPrefix) && ageOK && age >= k.ageFloor {
			ref.prefixIDs = append(ref.prefixIDs, id)
		}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	sortIDs(ref.fromIDs)
	sortIDs(ref.prefixIDs)
	sortIDs(ref.soloIDs)
	return ref, nil
}

// intersectArm is one conjunction written twice — values inlined and
// parameterised — with the reference multiset it must reproduce and the string
// bound its plan must render.
type intersectArm struct {
	// shape names the arm for violation messages.
	shape string
	// literal and param are the same predicate, spelled with the values inlined
	// and with them bound as parameters; params binds param's placeholders to
	// exactly the inlined values.
	literal string
	param   string
	params  map[string]any
	// want is the reference id-multiset both spellings must reproduce.
	want []int64
	// wantBound is the rendered STRING bound the plan must carry, which is what
	// identifies which SHAPE reached the budgeted gate (see the file header).
	wantBound string
	// countFn names the counting function that shape selects, for the message.
	countFn string
	// orderKnown marks an arm whose two per-conjunct counts are both exact, so
	// the AND order can be predicted; stringCount and numericCount are those
	// counts, and stringProp / numericProp their property keys (needed for the
	// planner's tie-break, which is on the property key).
	orderKnown                bool
	stringCount, numericCount int
	stringProp, numericProp   string
}

// Check runs every arm through engine and returns one violation per divergence:
//
//   - a composed arm whose id-multiset differs from the independent full-scan
//     reference is a [ViolationACIDConsistency]: the intersected read disagrees
//     with the base data;
//   - a parameterised spelling that differs from its literal twin is a
//     [ViolationACIDConsistency] (the rmp #2414 family, on results);
//   - a composed arm whose plan does not carry [intersectComposedMarker] is a
//     [ViolationOracleDeviation]: the shape the probe exists to drive was not
//     planned, so the arm proved nothing about the intersection path;
//   - a composed arm whose plan does not carry the expected string bound is a
//     [ViolationOracleDeviation]: the composition happened but through the other
//     budgeted count, so the branch this arm targets was not the one that ran;
//   - a composed arm that lost its residual Filter is a
//     [ViolationACIDConsistency]: each part is only a superset of its conjunct
//     (#F-EXEC1), so the exact predicate must still be re-applied per row;
//   - the control arm composing, or not reaching the range-scan operator at all,
//     is a [ViolationOracleDeviation]: without it a composed marker could not be
//     distinguished from "any index was used".
//
// A query that errors is a [ViolationOracleDeviation]. Every message renders
// sorted ids, so it is deterministic.
func (k *IndexIntersectProbes) Check(tick int64, engine PlanEngine) []Violation {
	c := &InvariantChecker{}
	ref, err := k.scanReference(engine)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("reference scan %q failed: %v", intersectReferenceQuery, err))
		return c.violations
	}

	arms := []intersectArm{
		{
			shape: "intersect-range-count-from",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d AND n.city >= '%s' RETURN id(n)",
				k.ageFloor, k.cityFloor),
			param:        "MATCH (n:Person) WHERE n.age >= $lo AND n.city >= $c RETURN id(n)",
			params:       map[string]any{"lo": k.ageFloor, "c": k.cityFloor},
			want:         ref.fromIDs,
			wantBound:    fmt.Sprintf("%q..+inf", k.cityFloor),
			countFn:      "RangeCountFrom",
			orderKnown:   true,
			stringCount:  ref.cityFromCount,
			numericCount: ref.ageCount,
			stringProp:   "city",
			numericProp:  "age",
		},
		{
			shape: "intersect-range-count",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d AND n.city STARTS WITH '%s' RETURN id(n)",
				k.ageFloor, k.cityPrefix),
			param:     "MATCH (n:Person) WHERE n.age >= $lo AND n.city STARTS WITH $p RETURN id(n)",
			params:    map[string]any{"lo": k.ageFloor, "p": k.cityPrefix},
			want:      ref.prefixIDs,
			wantBound: fmt.Sprintf("%q..", k.cityPrefix),
			countFn:   "RangeCount",
		},
	}
	for i := range arms {
		k.checkArm(c, tick, engine, &arms[i])
	}
	k.checkSoloControl(c, tick, engine, ref.soloIDs)
	return c.violations
}

// checkArm runs both spellings of one composed arm, verifies the answers against
// the reference and each other, and adjudicates the physical plan of the literal
// spelling (the parameterised plan is pinned against it by the sibling
// access-path parity oracle, which owns the literal-vs-param plan invariant).
func (k *IndexIntersectProbes) checkArm(c *InvariantChecker, tick int64, engine PlanEngine, arm *intersectArm) {
	lit, err := runProbeIDs(engine, arm.literal, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: literal arm %q failed: %v", arm.shape, arm.literal, err))
		return
	}
	par, err := runProbeIDs(engine, arm.param, arm.params)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: param arm %q failed: %v", arm.shape, arm.param, err))
		return
	}
	if !equalIDMultisets(lit, arm.want) {
		c.add(ViolationACIDConsistency, tick, intersectOp,
			fmt.Sprintf("shape %q: the intersected read disagrees with the independent full-scan reference\nquery: %s\nscan reference: %s\nprobe arm:      %s",
				arm.shape, arm.literal, summariseIDs(arm.want), summariseIDs(lit)))
	}
	if !equalIDMultisets(par, lit) {
		c.add(ViolationACIDConsistency, tick, intersectOp,
			fmt.Sprintf("shape %q: literal and parameter spellings of the same conjunction returned different result multisets\nliteral query: %s\nparam query:   %s\nliteral result: %s\nparam result:   %s",
				arm.shape, arm.literal, arm.param, summariseIDs(lit), summariseIDs(par)))
	}

	plan, perr := engine.Explain(arm.literal, nil)
	if perr != nil {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: Explain(%q) failed: %v", arm.shape, arm.literal, perr))
		return
	}
	if !strings.Contains(plan, intersectComposedMarker) {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: the planner did NOT compose the two indexes, so this arm exercised no intersection\nquery: %s\nplan:\n%s",
				arm.shape, arm.literal, plan))
		return
	}
	k.composedSeen++
	if len(lit) > 0 {
		k.rowsSeen++
	}
	if !strings.Contains(plan, arm.wantBound) {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: the plan composed but carries no %s bound, so the budgeted %s branch was not the one that ran\nquery: %s\nplan:\n%s",
				arm.shape, arm.wantBound, arm.countFn, arm.literal, plan))
	}
	if !strings.Contains(plan, "Filter") {
		c.add(ViolationACIDConsistency, tick, intersectOp,
			fmt.Sprintf("shape %q: the composed probe lost its residual Filter — each part is only a SUPERSET of its conjunct (#F-EXEC1), so the exact predicate must still be re-applied per row\nquery: %s\nplan:\n%s",
				arm.shape, arm.literal, plan))
	}
	k.checkAndOrder(c, tick, arm, plan)
}

// checkAndOrder requires the composed parts to be ANDed cheapest-first, against
// a prediction derived from the reference scan.
//
// It is the ONLY oracle in this file that can see a budgeted count's VALUE. The
// executed bitmaps, the rendered bounds and the answers are all independent of
// what the gate counted, so a count taken over the wrong key space is otherwise
// invisible; the order is where it surfaces, because the planner sorts the parts
// by ascending exact count and breaks ties on the property key.
//
// The prediction comes from the client-side cardinalities, never from a constant,
// so the check encodes no assumption about which conjunct is cheaper on this
// data — only that the plan agrees with the counts the base data implies.
func (k *IndexIntersectProbes) checkAndOrder(c *InvariantChecker, tick int64, arm *intersectArm, plan string) {
	if !arm.orderKnown {
		return
	}
	// The planner's own rule: ascending count, ties on the property key.
	stringFirst := arm.stringCount < arm.numericCount ||
		(arm.stringCount == arm.numericCount && arm.stringProp < arm.numericProp)

	// Scoped to the ACCESS-PATH LINE, not the whole plan. The rendered tree puts
	// the residual Filter's predicate text above the scan, and that text names the
	// same bounds; comparing positions across the whole plan would let a change in
	// the Filter rendering decide this check's verdict.
	line := intersectScanLine(plan)
	mark := strings.Index(line, intersectComposedMarker)
	bound := strings.Index(line, arm.wantBound)
	if mark < 0 || bound < 0 {
		// Both were established by the caller before this point; a miss here can
		// only mean the plan changed under us, which the caller already reported.
		return
	}
	gotStringFirst := bound < mark
	if gotStringFirst == stringFirst {
		return
	}
	side := func(stringSide bool) string {
		if stringSide {
			return arm.stringProp
		}
		return arm.numericProp
	}
	c.add(ViolationOracleDeviation, tick, intersectOp,
		fmt.Sprintf("shape %q: the composed parts are ANDed in the wrong order — the base data has"+
			" %s=%d and %s=%d, so %q is the cheaper conjunct and must be the PRIMARY, but the plan makes"+
			" %q the primary. The AND order is the only observable a budgeted count's VALUE has, so a"+
			" count taken over the wrong key space shows up here and nowhere else.\nquery: %s\nplan:\n%s",
			arm.shape, arm.stringProp, arm.stringCount, arm.numericProp, arm.numericCount,
			side(stringFirst), side(gotStringFirst), arm.literal, plan))
}

// checkSoloControl drives the single-property spelling of the SAME string
// conjunct and requires it to reach the range-scan operator WITHOUT composing.
//
// It is what stops the composed-marker assertions from being satisfiable by
// "some index was used": the composition reuses the single-property operator, so
// only a probe that seeks through that very operator and still renders no `∩
// range=` proves the marker discriminates composition. Its answers are verified
// against the same independent reference, so a control that seeked wrongly is
// reported rather than trusted.
func (k *IndexIntersectProbes) checkSoloControl(c *InvariantChecker, tick int64, engine PlanEngine, want []int64) {
	const shape = "solo-control"
	query := fmt.Sprintf("MATCH (n:Person) WHERE n.city >= '%s' RETURN id(n)", k.cityFloor)
	got, err := runProbeIDs(engine, query, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: %q failed: %v", shape, query, err))
		return
	}
	if !equalIDMultisets(got, want) {
		c.add(ViolationACIDConsistency, tick, intersectOp,
			fmt.Sprintf("shape %q: the single-property seek disagrees with the independent full-scan reference\nquery: %s\nscan reference: %s\nprobe arm:      %s",
				shape, query, summariseIDs(want), summariseIDs(got)))
	}
	plan, perr := engine.Explain(query, nil)
	if perr != nil {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: Explain(%q) failed: %v", shape, query, perr))
		return
	}
	if strings.Contains(plan, intersectComposedMarker) {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: a SINGLE-property predicate composed an intersection, so the composed marker does not discriminate composition and every composed assertion above is vacuous\nquery: %s\nplan:\n%s",
				shape, query, plan))
		return
	}
	if !strings.Contains(plan, intersectRangeScanOp) {
		c.add(ViolationOracleDeviation, tick, intersectOp,
			fmt.Sprintf("shape %q: the control did not reach %s at all, so it cannot show that the marker — and not merely the operator — is what distinguishes a composition\nquery: %s\nplan:\n%s",
				shape, intersectRangeScanOp, query, plan))
		return
	}
	k.soloSeekSeen++
}

// intersectScanLine returns the rendered plan line for the access-path operator a
// composition runs as, or the empty string when the plan carries none.
//
// It exists so an order comparison reads only the operator's own PlanDetail. The
// plan is an indented tree whose Filter line sits above the scan and renders the
// predicate text — which names the same bounds — so a positional comparison over
// the whole plan would be answered by the wrong line.
func intersectScanLine(plan string) string {
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, intersectRangeScanOp) {
			return line
		}
	}
	return ""
}

// Counts reports what the run observed: how many composed plans were seen, how
// many composed arms returned at least one row, and how many single-property
// controls seeked WITHOUT composing. It is the same state
// [IndexIntersectProbes.Finish] adjudicates, exposed so a test logs and asserts
// the measured numbers instead of inferring them from the gate's silence.
func (k *IndexIntersectProbes) Counts() (composed, withRows, soloSeeks int) {
	return k.composedSeen, k.rowsSeen, k.soloSeekSeen
}

// Finish asserts non-vacuity over the whole run and must be called once, after
// the terminal [IndexIntersectProbes.Check]:
//
//   - at least one arm must have composed, or the intersection path never ran;
//   - at least one composed arm must have returned a row, or every multiset
//     comparison was between empty sets;
//   - at least one control arm must have seeked without composing, or nothing
//     established that the composed marker discriminates a composition.
//
// Each is reported as a [ViolationOracleDeviation]: the engine is not wrong, the
// RUN failed to exercise what it claims to cover.
func (k *IndexIntersectProbes) Finish(tick int64) []Violation {
	var out []Violation
	add := func(msg string) {
		out = append(out, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: intersectOp, Message: msg})
	}
	if k.composedSeen == 0 {
		add("vacuous run: the planner never composed two indexes, so the intersect path" +
			" (and its budgeted RangeCount/RangeCountFrom gate) never ran")
	}
	if k.rowsSeen == 0 {
		add("vacuous run: no composed arm ever returned a row, so every intersected" +
			" comparison was between empty sets and proved nothing")
	}
	if k.soloSeekSeen == 0 {
		add("vacuous run: no single-property control ever seeked without composing, so" +
			" nothing established that the composed marker distinguishes an intersection" +
			" from any other index seek")
	}
	return out
}
