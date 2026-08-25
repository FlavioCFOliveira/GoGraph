package sim

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Seek-result diversity oracle (rmp #2450).
//
// [CheckIndexConsistency] result-verifies EQUALITY seeks only: for every
// distinct indexed value it cross-checks the equality seek against a full
// scan. The btree range scan (bounded and half-open), the STARTS WITH prefix
// rewrite, and IN-shaped predicates never had their RESULTS verified against
// an independent reference over churn and crash/recovery — the access-path
// parity oracle (rmp #2447) pins their PLANS, not their answers. This checker
// closes that gap: each predicate shape runs in both its literal and its
// parameterised spelling and is compared, as an id-multiset, against a
// reference built from one plain label scan filtered client-side — a path
// that touches no index machinery, so a torn, stale, or mis-bounded index
// read surfaces as a result divergence.
//
// The probe windows are drawn once, from the checker's own sub-seed
// ([seekResultsSeedMix]), and sized so the engine's seek gates admit the seek
// on the index-diversity scenario's data: the bounded age window spans 5 of
// the 500 cycled age values (~1% selectivity), the half-open floor sits in
// [455,494] (~9%, under the 10% range-seek ceiling, so RangeFrom /
// RangeCountFrom genuinely engage), and the prefix names exactly one of the
// 100 cycled city values (~1%). Plan engagement itself is asserted by the
// sibling parity oracle over the same shapes; this checker owns the answers.

// seekResultsSeedMix derives the seek-result checker's own draw stream from
// the master seed, so drawing the probe windows never perturbs the workload,
// crash, or parity streams (the same isolation rule paritySeedMix,
// checkerSeedMix, and crashSeedMix follow).
const seekResultsSeedMix uint64 = 0x8538ecb5bd456ea3

// seekResultsOp is the op label every seek-result violation carries.
const seekResultsOp = "index seek results"

// seekReferenceQuery is the independent-reference scan: a plain label scan
// projecting the three indexed properties, which the checker filters
// client-side. It deliberately carries no predicate at all, so the engine has
// nothing to push into an index — the reference cannot be served by the very
// structures under test.
const seekReferenceQuery = "MATCH (n:Person) RETURN id(n), n.age, n.city, n.name"

// IndexSeekResults is the seek-result diversity checker: a fixed, seed-drawn
// set of range, prefix, and IN-shaped probes over the index-diversity
// scenario's indexed properties, each result-verified against an independent
// full-scan reference in both literal and parameterised spellings. It is
// stateful so [IndexSeekResults.Finish] can assert non-vacuity over the whole
// run: at least one probe arm must have returned at least one row at least
// once, otherwise every comparison was between empty sets and proved nothing.
//
// # Concurrency contract
//
// IndexSeekResults is NOT safe for concurrent use; the simulator drives it
// from the single simulation goroutine.
type IndexSeekResults struct {
	// lo and hi bound the bounded numeric range probe: age in [lo, hi).
	lo, hi int64
	// fromLo is the half-open range probe's floor (age >= fromLo), drawn high
	// enough (~9% selectivity on the cycled ages) that the range-seek gate
	// admits the RangeFrom / RangeCountFrom path.
	fromLo int64
	// prefix is the STARTS WITH probe's prefix, naming exactly one cycled city.
	prefix string
	// inAges are the three distinct ages of the numeric IN-list probes.
	inAges [3]int64
	// inNames are the three distinct bulk names of the hash IN-list probes.
	inNames [3]string
	// sawRows records whether any probe arm ever returned a row.
	sawRows bool
}

// NewIndexSeekResults draws the probe windows from seed. bulk is the exclusive
// upper bound of the bulk "p<i>" name space the name probes may draw from
// (the index-diversity scenario passes [indexDiversityBulk]; the churn loop
// never deletes bulk names, so every drawn name stays resolvable for the whole
// run). bulk must be at least 20; a smaller fixture is a programmer error and
// panics in the seed draw.
func NewIndexSeekResults(seed *Seed, bulk int) *IndexSeekResults {
	lo := int64(seed.IntN(495))
	k := &IndexSeekResults{
		lo:     lo,
		hi:     lo + 5,
		fromLo: int64(455 + seed.IntN(40)),
		prefix: fmt.Sprintf("c%d", 10+seed.IntN(90)),
	}
	a := int64(seed.IntN(486))
	k.inAges = [3]int64{a, a + 5, a + 13}
	base := seed.IntN(bulk - 14)
	k.inNames = [3]string{
		fmt.Sprintf("p%d", base),
		fmt.Sprintf("p%d", base+7),
		fmt.Sprintf("p%d", base+13),
	}
	return k
}

// seekReference is the client-side-filtered view of one reference scan: the
// authoritative id-multiset each probe arm must reproduce, sorted ascending
// like every runProbeIDs result.
type seekReference struct {
	rangeIDs  []int64
	fromIDs   []int64
	prefixIDs []int64
	inAgeIDs  []int64
	inNameIDs []int64
}

// sortIDs sorts an id slice ascending in place, matching the ordering
// runProbeIDs applies, so reference and probe multisets compare positionally.
func sortIDs(ids []int64) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}

// seekResultArm is one predicate shape written twice — value(s) inlined and
// parameterised — with the reference multiset it must reproduce.
type seekResultArm struct {
	shape   string
	literal string
	param   string
	params  map[string]any
	want    []int64
}

// scanReference runs the plain label scan once and buckets every node id into
// the reference sets by filtering the projected property values client-side.
// Rows whose relevant property is absent or of another kind are skipped for
// that bucket, exactly as the engine's own predicates would skip them.
func (k *IndexSeekResults) scanReference(engine Engine) (*seekReference, error) {
	res, err := engine.Run(context.Background(), seekReferenceQuery, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()

	ref := &seekReference{}
	for res.Next() {
		id, ok := res.IntAt(0)
		if !ok {
			continue
		}
		if age, ok := res.IntAt(1); ok {
			if age >= k.lo && age < k.hi {
				ref.rangeIDs = append(ref.rangeIDs, id)
			}
			if age >= k.fromLo {
				ref.fromIDs = append(ref.fromIDs, id)
			}
			if age == k.inAges[0] || age == k.inAges[1] || age == k.inAges[2] {
				ref.inAgeIDs = append(ref.inAgeIDs, id)
			}
		}
		if city, ok := res.StringAt(2); ok && strings.HasPrefix(city, k.prefix) {
			ref.prefixIDs = append(ref.prefixIDs, id)
		}
		if name, ok := res.StringAt(3); ok &&
			(name == k.inNames[0] || name == k.inNames[1] || name == k.inNames[2]) {
			ref.inNameIDs = append(ref.inNameIDs, id)
		}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	sortIDs(ref.rangeIDs)
	sortIDs(ref.fromIDs)
	sortIDs(ref.prefixIDs)
	sortIDs(ref.inAgeIDs)
	sortIDs(ref.inNameIDs)
	return ref, nil
}

// Check runs every probe arm through engine and returns a violation for each
// divergence found:
//
//   - a probe arm (literal spelling) whose id-multiset differs from the
//     independent full-scan reference is a [ViolationACIDConsistency] — the
//     index-served answer disagrees with the base data;
//   - a parameterised spelling whose id-multiset differs from its literal
//     twin is a [ViolationACIDConsistency] (the rmp #2414 family, on results);
//   - the IN-list and UNWIND spellings of the same predicate disagreeing with
//     each other is a [ViolationACIDConsistency] (per rmp #2183 they may plan
//     differently — seek-set vs label scan — but must answer identically);
//   - count arms (bounded range, half-open range, prefix) must equal the
//     reference cardinality in both spellings, driving the dedicated
//     range-count paths.
//
// Probe failures (a query erroring) are [ViolationOracleDeviation]. Result
// ids are sorted before rendering so messages are deterministic.
func (k *IndexSeekResults) Check(tick int64, engine Engine) []Violation {
	c := &InvariantChecker{}
	ref, err := k.scanReference(engine)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, seekResultsOp,
			fmt.Sprintf("reference scan %q failed: %v", seekReferenceQuery, err))
		return c.violations
	}

	inAgeList := fmt.Sprintf("[%d, %d, %d]", k.inAges[0], k.inAges[1], k.inAges[2])
	inNameList := fmt.Sprintf("['%s', '%s', '%s']", k.inNames[0], k.inNames[1], k.inNames[2])
	agesParam := []any{k.inAges[0], k.inAges[1], k.inAges[2]}
	namesParam := []any{k.inNames[0], k.inNames[1], k.inNames[2]}

	arms := []seekResultArm{
		{
			shape:   "range",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d AND n.age < %d RETURN id(n)", k.lo, k.hi),
			param:   "MATCH (n:Person) WHERE n.age >= $lo AND n.age < $hi RETURN id(n)",
			params:  map[string]any{"lo": k.lo, "hi": k.hi},
			want:    ref.rangeIDs,
		},
		{
			shape:   "range-from",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d RETURN id(n)", k.fromLo),
			param:   "MATCH (n:Person) WHERE n.age >= $lo RETURN id(n)",
			params:  map[string]any{"lo": k.fromLo},
			want:    ref.fromIDs,
		},
		{
			shape:   "starts-with",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.city STARTS WITH '%s' RETURN id(n)", k.prefix),
			param:   "MATCH (n:Person) WHERE n.city STARTS WITH $p RETURN id(n)",
			params:  map[string]any{"p": k.prefix},
			want:    ref.prefixIDs,
		},
		{
			shape:   "in-age",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.age IN %s RETURN id(n)", inAgeList),
			param:   "MATCH (n:Person) WHERE n.age IN $vs RETURN id(n)",
			params:  map[string]any{"vs": agesParam},
			want:    ref.inAgeIDs,
		},
		{
			shape:   "in-age-unwind",
			literal: fmt.Sprintf("UNWIND %s AS v MATCH (n:Person) WHERE n.age = v RETURN id(n)", inAgeList),
			param:   "UNWIND $vs AS v MATCH (n:Person) WHERE n.age = v RETURN id(n)",
			params:  map[string]any{"vs": agesParam},
			want:    ref.inAgeIDs,
		},
		{
			shape:   "in-name",
			literal: fmt.Sprintf("MATCH (n:Person) WHERE n.name IN %s RETURN id(n)", inNameList),
			param:   "MATCH (n:Person) WHERE n.name IN $vs RETURN id(n)",
			params:  map[string]any{"vs": namesParam},
			want:    ref.inNameIDs,
		},
		{
			shape:   "in-name-unwind",
			literal: fmt.Sprintf("UNWIND %s AS v MATCH (n:Person) WHERE n.name = v RETURN id(n)", inNameList),
			param:   "UNWIND $vs AS v MATCH (n:Person) WHERE n.name = v RETURN id(n)",
			params:  map[string]any{"vs": namesParam},
			want:    ref.inNameIDs,
		},
	}
	litByShape := make(map[string][]int64, len(arms))
	for i := range arms {
		k.checkArm(c, tick, engine, &arms[i], litByShape)
	}
	k.checkSpellings(c, tick, "in-age", "in-age-unwind", litByShape)
	k.checkSpellings(c, tick, "in-name", "in-name-unwind", litByShape)

	k.checkCountArm(c, tick, engine, "range-count",
		fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d AND n.age < %d RETURN count(n)", k.lo, k.hi),
		"MATCH (n:Person) WHERE n.age >= $lo AND n.age < $hi RETURN count(n)",
		map[string]any{"lo": k.lo, "hi": k.hi}, int64(len(ref.rangeIDs)))
	k.checkCountArm(c, tick, engine, "range-count-from",
		fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d RETURN count(n)", k.fromLo),
		"MATCH (n:Person) WHERE n.age >= $lo RETURN count(n)",
		map[string]any{"lo": k.fromLo}, int64(len(ref.fromIDs)))
	k.checkCountArm(c, tick, engine, "starts-with-count",
		fmt.Sprintf("MATCH (n:Person) WHERE n.city STARTS WITH '%s' RETURN count(n)", k.prefix),
		"MATCH (n:Person) WHERE n.city STARTS WITH $p RETURN count(n)",
		map[string]any{"p": k.prefix}, int64(len(ref.prefixIDs)))

	return c.violations
}

// checkArm runs both spellings of one probe arm and appends a violation per
// divergence: literal vs reference, and parameter vs literal. Successful
// literal results are stashed in litByShape for the cross-spelling check.
func (k *IndexSeekResults) checkArm(c *InvariantChecker, tick int64, engine Engine, arm *seekResultArm, litByShape map[string][]int64) {
	lit, err := runProbeIDs(engine, arm.literal, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, seekResultsOp,
			fmt.Sprintf("shape %q: literal arm %q failed: %v", arm.shape, arm.literal, err))
		return
	}
	par, err := runProbeIDs(engine, arm.param, arm.params)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, seekResultsOp,
			fmt.Sprintf("shape %q: param arm %q failed: %v", arm.shape, arm.param, err))
		return
	}
	litByShape[arm.shape] = lit
	if len(lit) > 0 || len(par) > 0 {
		k.sawRows = true
	}
	if !equalIDMultisets(lit, arm.want) {
		c.add(ViolationACIDConsistency, tick, seekResultsOp,
			fmt.Sprintf("shape %q: the probe arm disagrees with the independent full-scan reference\nquery: %s\nscan reference: %s\nprobe arm:      %s",
				arm.shape, arm.literal, summariseIDs(arm.want), summariseIDs(lit)))
	}
	if !equalIDMultisets(par, lit) {
		c.add(ViolationACIDConsistency, tick, seekResultsOp,
			fmt.Sprintf("shape %q: literal and parameter spellings of the same predicate returned different result multisets\nliteral query: %s\nparam query:   %s\nliteral result: %s\nparam result:   %s",
				arm.shape, arm.literal, arm.param, summariseIDs(lit), summariseIDs(par)))
	}
}

// checkSpellings asserts the IN-list spelling and its UNWIND twin agree with
// each other. Both already ran against the reference; this explicit pairwise
// comparison exists so a joint drift of both spellings away from each other is
// named as a spelling divergence rather than left to two reference messages.
// A shape whose arm errored (absent from litByShape) is skipped — the error
// was already reported.
func (k *IndexSeekResults) checkSpellings(c *InvariantChecker, tick int64, a, b string, litByShape map[string][]int64) {
	av, aok := litByShape[a]
	bv, bok := litByShape[b]
	if !aok || !bok {
		return
	}
	if !equalIDMultisets(av, bv) {
		c.add(ViolationACIDConsistency, tick, seekResultsOp,
			fmt.Sprintf("the %q and %q spellings of the same predicate returned different result multisets: %s vs %s",
				a, b, summariseIDs(av), summariseIDs(bv)))
	}
}

// checkCountArm runs both spellings of one count probe and appends a violation
// when the literal count disagrees with the reference cardinality or the two
// spellings disagree with each other.
func (k *IndexSeekResults) checkCountArm(c *InvariantChecker, tick int64, engine Engine, shape, literal, param string, params map[string]any, want int64) {
	lit, err := c.countQuery(engine, literal, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, seekResultsOp,
			fmt.Sprintf("shape %q: literal count arm %q failed: %v", shape, literal, err))
		return
	}
	par, err := c.countQuery(engine, param, params)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, seekResultsOp,
			fmt.Sprintf("shape %q: param count arm %q failed: %v", shape, param, err))
		return
	}
	if lit > 0 || par > 0 {
		k.sawRows = true
	}
	if lit != want {
		c.add(ViolationACIDConsistency, tick, seekResultsOp,
			fmt.Sprintf("shape %q: count disagrees with the independent full-scan reference: scan=%d count-arm=%d\nquery: %s",
				shape, want, lit, literal))
	}
	if par != lit {
		c.add(ViolationACIDConsistency, tick, seekResultsOp,
			fmt.Sprintf("shape %q: literal and parameter count spellings disagree: literal=%d param=%d\nliteral query: %s\nparam query:   %s",
				shape, lit, par, literal, param))
	}
}

// Finish asserts non-vacuity over the whole run: at least one probe arm must
// have returned at least one row in at least one [IndexSeekResults.Check]
// invocation. A run in which every comparison was between empty multisets
// proved nothing about the index read paths and is reported as a
// [ViolationOracleDeviation] rather than passing silently. Call it once, after
// the terminal Check.
func (k *IndexSeekResults) Finish(tick int64) []Violation {
	if k.sawRows {
		return nil
	}
	return []Violation{{
		Kind: ViolationVacuousRun,
		Tick: tick,
		Op:   seekResultsOp,
		Message: "vacuous run: no probe arm ever returned a row, so every seek-result " +
			"comparison was between empty sets and proved nothing",
	}}
}
