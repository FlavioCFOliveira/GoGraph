package sim

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TypedWriter is the type-coverage actor: it emits [tmplCreateTyped] CREATEs
// whose parameters span every round-tripping Cypher property kind — string,
// integer, float, boolean, list, a plain ISO-8601 string, and all six genuine
// temporal types — with values drawn deterministically from the seed. The
// oracle records the full property set so the type-coverage checker can verify
// each kind survives commit and crash/recovery.
//
// # Concurrency contract
//
// TypedWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type TypedWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*TypedWriter) Name() string { return "TypedWriter" }

// typedDateDay returns the day-of-month a Typed node's `d` property carries.
// The stride of 7 over a 28-day month makes the date order DIFFER from the id
// order (ids 0,4 share day 1; 1,5 share day 8; …), which is what makes
// [CheckTypedTemporalOrder] load-bearing: an ORDER BY that silently fell back
// to insertion or id order would produce a different sequence.
func typedDateDay(id int64) int { return 1 + int((id*7)%28) }

// typedTimeOffsets are the zone offsets (seconds east of UTC) the `tm`
// property cycles through, so the TIME round-trip covers a positive, a
// negative, and the UTC zone rendering rather than only "Z".
var typedTimeOffsets = [3]int{0, 3600, -3600}

// NextOp returns the next Typed-node CREATE with a unique id and a value of every
// supported kind, all seed-derived so the op stream is a pure function of the
// seed. A pointer receiver carries the monotone id counter across calls.
//
// The six temporal properties are bound as genuine temporal expr values, so the
// engine stores them in its kind-tagged form and they read back as temporals;
// `ts` stays a plain ISO-8601 STRING and is the control that must read back as
// a string (rmp #2457).
func (w *TypedWriter) NextOp(seed *Seed, _ *GraphOracle) Op {
	id := w.counter
	w.counter++
	// Plain ISO-8601 STRING (NOT a temporal): the deliberate control.
	ts := fmt.Sprintf("2026-01-%02dT%02d:%02d:00Z", 1+int(id%28), int(id%24), int(seed.IntN(60)))
	return Op{
		Kind:   OpCreate,
		Cypher: tmplCreateTyped,
		Params: map[string]any{
			"id":  id,
			"s":   fmt.Sprintf("str-%d", id),
			"i":   int64(seed.IntN(1_000_000)),
			"f":   float64(seed.IntN(1_000_000)) / 1000.0,
			"b":   seed.IntN(2) == 0,
			"lst": []any{id, int64(seed.IntN(100)), int64(seed.IntN(100))},
			"ts":  ts,
			"d":   expr.NewDate(2026, 1, typedDateDay(id)),
			"ldt": expr.NewLocalDateTime(2026, 1+int(id%12), 1+int(id%28), int(id%24), int(id%60), int(id%60), 0),
			"dt":  expr.NewDateTime(2026, 1+int(id%12), 1+int(id%28), int(id%24), int(id%60), 0, 0, time.UTC),
			"lt":  expr.NewLocalTime(int(id%24), int(id%60), int(id%60), 0),
			"tm":  expr.NewTime(int(id%24), int(id%60), 0, 0, typedTimeOffsets[id%3]),
			"du":  expr.NewDuration(id%13, id%29, int64(seed.IntN(3600)), 0),
		},
	}
}

// typedExpectKind is the expr KIND every [typedPropKeys] property must read
// back as. It is the assertion that makes the type-coverage arm non-vacuous
// for temporals (rmp #2457): a temporal that degraded to an untagged
// PropString reads back as [expr.KindString] carrying the SAME text, so only
// the kind distinguishes a working round-trip from a broken one. `ts` is
// deliberately KindString — it is the plain-ISO control that proves the
// assertion discriminates instead of accepting anything.
var typedExpectKind = map[string]expr.Kind{
	"id":     expr.KindInteger,
	"s":      expr.KindString,
	"i":      expr.KindInteger,
	"f":      expr.KindFloat,
	"b":      expr.KindBool,
	"lst":    expr.KindList,
	"ts":     expr.KindString,
	"d":      expr.KindDate,
	"ldt":    expr.KindLocalDateTime,
	"dt":     expr.KindDateTime,
	"lt":     expr.KindLocalTime,
	"tm":     expr.KindTime,
	"du":     expr.KindDuration,
	"absent": expr.KindNull,
}

// typeCoverageWorkload is a 100% TypedWriter mix: every op creates a Typed node
// carrying a value of each supported kind.
func typeCoverageWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&TypedWriter{}}, Weights: []float64{1.0}}
}

// CheckTypedProperties reads every modelled Typed node back through the real
// engine read path and asserts, for every property, BOTH that its value
// round-trips to the modelled value (compared via the canonical expr.Value
// String() rendering) AND that it reads back with the expected expr KIND
// ([typedExpectKind]) — that a never-set property reads NULL, and that the node
// exists at all. It runs on a quiescent graph (the deterministic loop, including
// immediately after crash/recovery), so a divergence means a property failed to
// round-trip, changed type, or did not survive recovery.
//
// The kind assertion is what makes the temporal arm honest (rmp #2457). Before
// it, temporals were written as ISO-8601 STRINGS and compared as strings, so the
// arm could not distinguish a working temporal round-trip from a broken one:
// both sides said the same text. A temporal that degrades to an untagged
// PropString now fails on kind even though its text is unchanged.
func CheckTypedProperties(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	var vs []Violation
	cols := make([]string, len(typedPropKeys))
	for i, k := range typedPropKeys {
		cols[i] = "n." + k
	}
	proj := strings.Join(cols, ", ")
	ctx := context.Background()

	for _, id := range oracle.TypedIDs() {
		props, ok := oracle.TypedNode(id)
		if !ok {
			continue
		}
		q := fmt.Sprintf("MATCH (n:Typed {id:%d}) RETURN %s", id, proj)
		got, err := engine.projectRowValues(ctx, q, len(typedPropKeys))
		if err != nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed property read",
				Message: fmt.Sprintf("Typed{id:%d}: read failed: %v", id, err),
			})
			continue
		}
		if got == nil {
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Tick: tick, Op: "typed node existence",
				Message: fmt.Sprintf("committed Typed{id:%d} absent in engine (did not survive recovery)", id),
			})
			continue
		}
		vs = append(vs, checkTypedRow(tick, id, props, got)...)
	}
	return vs
}

// checkTypedRow adjudicates one Typed node's projected row against the oracle's
// modelled property map: the expr KIND first (a type degradation is reported as
// such even when the text still matches), then the canonical value.
func checkTypedRow(tick, id int64, props map[string]any, got []expr.Value) []Violation {
	var vs []Violation
	for i, k := range typedPropKeys {
		gotV := got[i]
		if gotV == nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed property value",
				Message: fmt.Sprintf("Typed{id:%d}.%s: engine returned no value", id, k),
			})
			continue
		}
		if want, ok := typedExpectKind[k]; ok && gotV.Kind() != want {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "typed property kind",
				Message: fmt.Sprintf("Typed{id:%d}.%s has kind %v (value %s), want kind %v"+
					" — the value DEGRADED to another type (a temporal read back as a plain"+
					" string means its storage tag was lost)", id, k, gotV.Kind(), gotV.String(), want),
			})
			continue
		}
		want := "null" // "absent" is never set, and any modelled-nil reads NULL
		if k != "absent" {
			want = canonicalValueString(props[k])
		}
		if gotV.String() != want {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "typed property value",
				Message: fmt.Sprintf("Typed{id:%d}.%s = %s, want %s (kind did not round-trip)", id, k, gotV.String(), want),
			})
		}
	}
	return vs
}

// CheckTypedTemporalOrder asserts that ORDER BY over a TEMPORAL property agrees
// with an ordering the oracle computes itself from the temporals it modelled.
// The engine is asked for the Typed ids ordered by (n.d, n.id); the oracle sorts
// its own modelled ([expr.DateValue], id) pairs and the two id sequences must be
// identical.
//
// This is a genuine oracle, not a self-check: the dates are laid out with a
// stride ([typedDateDay]) that makes date order differ from id order, so an
// engine that ignored the temporal ordering — or compared the tagged storage
// strings byte-wise — would produce a different sequence. Fewer than two
// modelled nodes cannot order anything, so the check reports nothing.
func CheckTypedTemporalOrder(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ids := oracle.TypedIDs()
	if len(ids) < 2 {
		return nil
	}
	type dated struct {
		d  expr.DateValue
		id int64
	}
	model := make([]dated, 0, len(ids))
	for _, id := range ids {
		props, ok := oracle.TypedNode(id)
		if !ok {
			continue
		}
		dv, ok := props["d"].(expr.DateValue)
		if !ok {
			return []Violation{{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed temporal order",
				Message: fmt.Sprintf("oracle models Typed{id:%d}.d as %T, want expr.DateValue", id, props["d"]),
			}}
		}
		model = append(model, dated{d: dv, id: id})
	}
	sort.Slice(model, func(i, j int) bool {
		a, b := model[i], model[j]
		if a.d.Year != b.d.Year {
			return a.d.Year < b.d.Year
		}
		if a.d.Month != b.d.Month {
			return a.d.Month < b.d.Month
		}
		if a.d.Day != b.d.Day {
			return a.d.Day < b.d.Day
		}
		return a.id < b.id
	})
	want := make([]string, len(model))
	for i, m := range model {
		want[i] = strconv.FormatInt(m.id, 10)
	}

	rows, err := engine.queryRowStrings(context.Background(),
		"MATCH (n:Typed) RETURN n.id ORDER BY n.d, n.id", 1)
	if err != nil {
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed temporal order",
			Message: fmt.Sprintf("ORDER BY temporal read failed: %v", err),
		}}
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0]
	}
	if !equalStrings(got, want) {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "typed temporal order",
			Message: fmt.Sprintf("ORDER BY n.d, n.id produced ids %v, oracle-computed temporal order is %v", got, want),
		}}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// list-valued property predicates (rmp #2459)
// ─────────────────────────────────────────────────────────────────────────────

// typedListProbeNodes bounds how many Typed nodes the per-node list-predicate
// probes touch in one check. The predicates are identical for every node, so an
// unbounded sweep would grow the checker's cost with the run while proving
// nothing new; the sample is taken with a stride that always yields the FIRST
// and the NEWEST modelled node, so a defect confined to the most recently
// written list is still seen.
const typedListProbeNodes = 8

// typedListAbsentElem is an integer no modelled `lst` can hold: [TypedWriter]
// stores the node's own id (bounded by the scenario's tick count) plus two seed
// draws in [0,100) in every list. It is the membership probe's NEGATIVE CONTROL
// — `WHERE <it> IN n.lst` must count zero nodes — which is what proves the
// positive membership count discriminates rather than matching everything.
const typedListAbsentElem = 10_000_000

// typedListCol is one adjudicated column of a list-predicate probe: the
// expression as written (for the failure message), the canonical text the
// oracle computes for it, and the expr kind it must read back as.
type typedListCol struct {
	what string
	text string
	kind expr.Kind
}

// CheckTypedListPredicates drives the LIST-valued property surface over the
// `lst` property every Typed node carries, with every expectation computed by
// the oracle from the list it modelled (rmp #2459).
//
// Before #2459 the DST wrote a list-valued property and only ever read it back
// WHOLE: no predicate ever indexed, sized, sliced, unwound, reduced, or tested
// membership in a stored list, so the engine's whole list-expression surface
// over stored data was unexercised. The probes here close that gap:
//
//   - [checkTypedListRow] — subscript, negative subscript, both out-of-range
//     directions, size(), slice, and reduce(), one query per sampled node;
//   - [checkTypedListUnwind] — UNWIND over the STORED list, aggregated by
//     count(), sum() and collect();
//   - [checkTypedListMembership] — `WHERE <elem> IN n.lst` over the whole
//     graph, once with an element the model holds and once with one it cannot.
//
// It runs on a quiescent graph — periodically and immediately after each
// crash/recovery — so the predicates are also proven against a list that
// survived real recovery.
func CheckTypedListPredicates(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	ids := oracle.TypedIDs()
	if len(ids) == 0 {
		return nil
	}
	lists, vs := typedModelledLists(tick, oracle, ids)
	if len(vs) > 0 {
		return vs
	}
	if len(lists) == 0 {
		return nil
	}
	// Non-vacuity: each list leads with its node's own unique id, so two
	// modelled nodes already guarantee two distinct elements. Fewer than that
	// means every membership, reduce and aggregate probe below is comparing a
	// constant, which proves nothing (the assert-something-was-seen rule).
	if len(lists) >= 2 {
		if d := typedListDistinctElems(lists); d < 2 {
			return []Violation{{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed list model",
				Message: fmt.Sprintf("%d modelled lists carry only %d distinct element(s):"+
					" the list predicates are vacuous", len(lists), d),
			}}
		}
	}

	ctx := context.Background()
	sample := typedListSample(ids)
	out := make([]Violation, 0, 4)
	for _, id := range sample {
		l, ok := lists[id]
		if !ok {
			continue
		}
		out = append(out, checkTypedListRow(ctx, tick, id, l, engine)...)
		out = append(out, checkTypedListUnwind(ctx, tick, id, l, engine)...)
	}
	// Membership is asked over the WHOLE modelled set, so the count is a genuine
	// oracle: the probe element is drawn from the newest modelled list, and the
	// small value range means it recurs across several nodes rather than matching
	// exactly one. Every list in lists is non-empty ([typedModelledLists] rejects
	// an empty one), so indexing the chosen list is safe.
	newest, ok := lists[ids[len(ids)-1]]
	if !ok {
		return out
	}
	elem := newest[len(newest)-1]
	want := 0
	for _, l := range lists {
		if slices.Contains(l, elem) {
			want++
		}
	}
	out = append(out, checkTypedListMembership(ctx, tick, elem, want, engine)...)
	out = append(out, checkTypedListMembership(ctx, tick, typedListAbsentElem, 0, engine)...)
	return out
}

// typedModelledLists returns the oracle's modelled `lst` for every id as a plain
// int64 slice. It reports a violation instead of a model when a modelled list is
// missing, is not a list of integers, or is EMPTY: every expectation below is
// computed from this model, so a degenerate one would silently make the whole
// arm vacuous rather than merely unchecked.
func typedModelledLists(tick int64, oracle *GraphOracle, ids []int64) (map[int64][]int64, []Violation) {
	modelViolation := func(format string, args ...any) []Violation {
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "typed list model",
			Message: fmt.Sprintf(format, args...),
		}}
	}
	lists := make(map[int64][]int64, len(ids))
	for _, id := range ids {
		props, ok := oracle.TypedNode(id)
		if !ok {
			continue
		}
		raw, ok := props["lst"].([]any)
		if !ok {
			return nil, modelViolation("oracle models Typed{id:%d}.lst as %T, want a []any list", id, props["lst"])
		}
		if len(raw) == 0 {
			return nil, modelViolation("oracle models Typed{id:%d}.lst as EMPTY:"+
				" every list predicate over it would be vacuous", id)
		}
		l := make([]int64, len(raw))
		for i, e := range raw {
			n, ok := e.(int64)
			if !ok {
				return nil, modelViolation("oracle models Typed{id:%d}.lst[%d] as %T,"+
					" want an int64 (the reduce arm needs a numeric list)", id, i, e)
			}
			l[i] = n
		}
		lists[id] = l
	}
	return lists, nil
}

// typedListDistinctElems counts the distinct integers across every modelled
// list — the measure the non-vacuity gate in [CheckTypedListPredicates] reads.
func typedListDistinctElems(lists map[int64][]int64) int {
	seen := make(map[int64]struct{}, 4*len(lists))
	for _, l := range lists {
		for _, e := range l {
			seen[e] = struct{}{}
		}
	}
	return len(seen)
}

// typedListSample picks at most [typedListProbeNodes] ids out of the ascending
// id sequence, with an integer stride that spans the whole range and always
// yields ids[0] and the last id exactly.
func typedListSample(ids []int64) []int64 {
	if len(ids) <= typedListProbeNodes {
		return ids
	}
	out := make([]int64, typedListProbeNodes)
	for i := range out {
		out[i] = ids[i*(len(ids)-1)/(typedListProbeNodes-1)]
	}
	return out
}

// checkTypedListRow drives the single-row list-predicate battery over one Typed
// node's stored `lst` and adjudicates every column against a value the oracle
// computes from its own modelled list — the expr KIND first, then the canonical
// rendering, so a subscript that returned the whole list, or an out-of-range
// read that returned an element instead of NULL, is reported as the type error
// it is.
//
// The columns pin the engine's contract as VERIFIED in this engine (rmp #2459),
// not as assumed:
//
//   - a NEGATIVE index counts from the end, so n.lst[-1] is the LAST element;
//   - an index past EITHER end yields NULL rather than an error or a clamp;
//   - a slice is half-open [from, to) and clamps to the list's own bounds, so
//     n.lst[0..2] is the first two elements of a longer list and the whole of a
//     shorter one.
func checkTypedListRow(ctx context.Context, tick, id int64, l []int64, engine *EngineAdapter) []Violation {
	const op = "typed list predicate"
	// "null" is the canonical rendering of NULL, which is what BOTH out-of-range
	// subscripts must produce.
	const nullText = "null"

	var sum int64
	for _, e := range l {
		sum += e
	}
	head := make([]any, 0, 2)
	for i := 0; i < len(l) && i < 2; i++ {
		head = append(head, l[i])
	}
	want := []typedListCol{
		{"n.lst[0]", canonicalValueString(l[0]), expr.KindInteger},
		{"n.lst[-1]", canonicalValueString(l[len(l)-1]), expr.KindInteger},
		{fmt.Sprintf("n.lst[%d] (past the end)", len(l)), nullText, expr.KindNull},
		{fmt.Sprintf("n.lst[-%d] (before the start)", len(l)+1), nullText, expr.KindNull},
		{"size(n.lst)", canonicalValueString(int64(len(l))), expr.KindInteger},
		{"n.lst[0..2]", canonicalValueString(head), expr.KindList},
		{"reduce(acc = 0, x IN n.lst | acc + x)", canonicalValueString(sum), expr.KindInteger},
	}
	q := fmt.Sprintf("MATCH (n:Typed {id:%d}) RETURN n.lst[0], n.lst[-1], n.lst[%d], n.lst[-%d],"+
		" size(n.lst), n.lst[0..2], reduce(acc = 0, x IN n.lst | acc + x)", id, len(l), len(l)+1)
	got, err := engine.projectRowValues(ctx, q, len(want))
	if vs := typedListRowFailure(tick, id, op, "list-predicate", err, got); len(vs) > 0 {
		return vs
	}
	return compareTypedListCols(tick, id, op, want, got)
}

// checkTypedListUnwind drives UNWIND over a STORED list property — the read
// path that turns one row carrying a list into one row per element — and
// aggregates the unwound stream three ways against the oracle's model: count(x)
// must equal the modelled length, sum(x) the modelled sum, and collect(x) must
// reproduce the modelled elements.
//
// collect is compared as a MULTISET (both sides sorted by canonical rendering)
// because openCypher does not specify the input order an aggregate observes.
// The ORDER of the same list is pinned separately, and absolutely, by the
// n.lst[0] / n.lst[-1] / n.lst[0..2] columns of [checkTypedListRow] — so a
// reversed list still fails, just not here.
func checkTypedListUnwind(ctx context.Context, tick, id int64, l []int64, engine *EngineAdapter) []Violation {
	const op = "typed list UNWIND"
	var sum int64
	for _, e := range l {
		sum += e
	}
	q := fmt.Sprintf("MATCH (n:Typed {id:%d}) UNWIND n.lst AS x RETURN count(x), sum(x), collect(x)", id)
	got, err := engine.projectRowValues(ctx, q, 3)
	if vs := typedListRowFailure(tick, id, op, "UNWIND", err, got); len(vs) > 0 {
		return vs
	}
	vs := compareTypedListCols(tick, id, op, []typedListCol{
		{"count(x) over UNWIND n.lst", canonicalValueString(int64(len(l))), expr.KindInteger},
		{"sum(x) over UNWIND n.lst", canonicalValueString(sum), expr.KindInteger},
	}, got[:2])

	lv, ok := got[2].(expr.ListValue)
	if !ok {
		return append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: op,
			Message: fmt.Sprintf("Typed{id:%d}: collect(x) over UNWIND n.lst is %s, want a list",
				id, typedValueDesc(got[2])),
		})
	}
	gotElems := make([]string, 0, len(lv))
	for _, v := range lv {
		gotElems = append(gotElems, v.String())
	}
	wantElems := make([]string, 0, len(l))
	for _, e := range l {
		wantElems = append(wantElems, canonicalValueString(e))
	}
	slices.Sort(gotElems)
	slices.Sort(wantElems)
	if !equalStrings(gotElems, wantElems) {
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: op,
			Message: fmt.Sprintf("Typed{id:%d}: collect(x) over UNWIND n.lst = %v, oracle-modelled elements are %v"+
				" (compared as a multiset)", id, gotElems, wantElems),
		})
	}
	return vs
}

// checkTypedListMembership counts the Typed nodes whose STORED list contains
// elem through `WHERE <elem> IN n.lst` and compares the engine's count with the
// oracle's own scan of every modelled list. The caller drives it twice per
// check — once with an element the model really holds, once with
// [typedListAbsentElem] and an expected count of zero — so the probe is proven
// to discriminate instead of counting the whole label.
func checkTypedListMembership(ctx context.Context, tick, elem int64, want int, engine *EngineAdapter) []Violation {
	const op = "typed list membership"
	q := fmt.Sprintf("MATCH (n:Typed) WHERE %d IN n.lst RETURN count(n)", elem)
	got, err := engine.projectRowValues(ctx, q, 1)
	if err != nil {
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("`%d IN n.lst` read failed: %v", elem, err),
		}}
	}
	if len(got) == 0 || got[0] == nil {
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("`%d IN n.lst` returned no count row", elem),
		}}
	}
	if got[0].Kind() != expr.KindInteger {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: op,
			Message: fmt.Sprintf("count of `%d IN n.lst` is %s, want an integer", elem, typedValueDesc(got[0])),
		}}
	}
	if w := strconv.Itoa(want); got[0].String() != w {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: op,
			Message: fmt.Sprintf("`%d IN n.lst` matched %s nodes, oracle counted %s modelled lists containing it",
				elem, got[0].String(), w),
		}}
	}
	return nil
}

// typedListRowFailure adjudicates the two ways a list probe can fail before any
// column is compared: the query erroring, or the node the checker knows is
// modelled yielding no row at all (which is a durability failure, not a value
// mismatch).
func typedListRowFailure(tick, id int64, op, what string, err error, got []expr.Value) []Violation {
	if err != nil {
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
			Message: fmt.Sprintf("Typed{id:%d}: %s read failed: %v", id, what, err),
		}}
	}
	if got == nil {
		return []Violation{{
			Kind: ViolationACIDDurability, Tick: tick, Op: op,
			Message: fmt.Sprintf("committed Typed{id:%d} yielded no row for the %s probe", id, what),
		}}
	}
	return nil
}

// compareTypedListCols adjudicates a projected row against the oracle's
// per-column expectations: the KIND first (a predicate that returned the wrong
// TYPE is reported as such even when its text happens to match), then the
// canonical value.
func compareTypedListCols(tick, id int64, op string, want []typedListCol, got []expr.Value) []Violation {
	var vs []Violation
	for i, w := range want {
		gv := got[i]
		if gv == nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: op,
				Message: fmt.Sprintf("Typed{id:%d}: `%s` returned no value", id, w.what),
			})
			continue
		}
		if gv.Kind() != w.kind {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: op,
				Message: fmt.Sprintf("Typed{id:%d}: `%s` has kind %v (value %s), want kind %v",
					id, w.what, gv.Kind(), gv.String(), w.kind),
			})
			continue
		}
		if gv.String() != w.text {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: op,
				Message: fmt.Sprintf("Typed{id:%d}: `%s` = %s, oracle computed %s from its modelled list",
					id, w.what, gv.String(), w.text),
			})
		}
	}
	return vs
}

// typedValueDesc renders a value's kind and text for a failure message,
// tolerating a nil value (an engine that projected no value at all).
func typedValueDesc(v expr.Value) string {
	if v == nil {
		return "absent"
	}
	return fmt.Sprintf("kind %v (value %s)", v.Kind(), v.String())
}

// typeCoverageScenario verifies the property type system under the DST: a
// workload creates Typed nodes carrying a value of every round-tripping kind
// (string/int/float/bool/list/ISO-string + all six genuine temporal types + a
// NULL-reading absent key), and [CheckTypedProperties] confirms each kind
// round-trips WITH ITS KIND INTACT and — with crash/recovery injected —
// survives recovery. Since rmp #2459 the list-valued property is also driven
// through the list-expression surface ([CheckTypedListPredicates]) rather than
// only read back whole. It is bit-reproducible.
//
// Checkpointing is enabled (rmp #2457) so the temporals are proven across BOTH
// durable paths: a crash before the next checkpoint recovers them by replaying
// the WAL, and a crash after one recovers them from the published snapshot (the
// checkpoint truncates the WAL prefix it folded, so the snapshot is the only
// source for the folded ops). A tag lost by either serialiser would surface as
// a kind violation at the post-recovery check.
func typeCoverageScenario() Scenario {
	return Scenario{
		Name:        ScenarioTypeCoverage,
		Description: "property type system: string/int/float/bool/list/6 temporal kinds round-trip + survive crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x7A9E5,
		MaxTicks:    400,
		Workload:    typeCoverageWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 45},
		run:         runTypeCoverage,
	}
}

// typeCoverageCheckEvery is the tick cadence for the periodic typed-property
// check inside [runTypeCoverage].
const typeCoverageCheckEvery = 60

// checkTypedAll runs the full type-coverage battery in one call: the per-kind
// value + KIND round-trip ([CheckTypedProperties]), the temporal ORDER BY oracle
// ([CheckTypedTemporalOrder]), and the list-valued property predicates
// ([CheckTypedListPredicates]). It is the single entry point the scenario calls
// periodically, after each crash/recovery, and at the end.
func checkTypedAll(tick int64, oracle *GraphOracle, engine *EngineAdapter) []Violation {
	vs := make([]Violation, 0, 4)
	vs = append(vs, CheckTypedProperties(tick, oracle, engine)...)
	vs = append(vs, CheckTypedTemporalOrder(tick, oracle, engine)...)
	vs = append(vs, CheckTypedListPredicates(tick, oracle, engine)...)
	return vs
}

// runTypeCoverage drives the type-coverage safety loop: it creates Typed nodes,
// runs the full typed battery ([checkTypedAll] — property round-trip AND kind,
// the temporal ORDER BY oracle, and the list-valued property predicates)
// periodically and immediately after every crash/recovery (the DST-unique value
// — everything is validated against a graph that survived real recovery),
// publishes real checkpoints so recovery alternates between the WAL and the
// snapshot path, and ends with a terminal check. It is deterministic.
func runTypeCoverage(ctx context.Context, seed uint64) (*SimReport, error) {
	sm, report, err := runTypeCoverageSim(ctx, seed)
	if sm != nil {
		defer func() { _ = sm.Close() }()
	}
	return report, err
}

// runTypeCoverageSim is [runTypeCoverage] with the simulator handed back to the
// caller instead of closed, so a test can assert on what the run actually
// exercised — that crashes AND checkpoints really fired, and that the typed
// nodes really exist. The caller owns closing the returned simulator, which is
// non-nil whenever construction succeeded (even when a violation is reported).
func runTypeCoverageSim(ctx context.Context, seed uint64) (*Simulator, *SimReport, error) {
	sc := typeCoverageScenario()
	cfg := sc.DeterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("sim: type-coverage new: %w", err)
	}

	var lastTick int64
	var lastOp Op
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return sm, nil, err
		}
		tick := sm.clock.Tick()

		crashesBefore := sm.crashCount
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return sm, nil, err
		} else if report != nil {
			return sm, report, nil
		}
		if sm.crashCount > crashesBefore {
			// Validate every typed property — value AND kind — against the
			// crash-recovered graph, plus the temporal ordering and the list
			// predicates, so a tag lost by the WAL or the snapshot serialiser, or a
			// list that did not survive recovery intact, fails the run right here.
			if v := checkTypedAll(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery typed check>"}, v)
				return sm, rep, nil
			}
		}
		// A real checkpoint folds the committed prefix into a snapshot and
		// truncates the WAL, so the NEXT crash recovers the temporals through the
		// snapshot path rather than by WAL replay.
		if err := sm.maybeCheckpoint(tick); err != nil {
			return sm, nil, err
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if v := sm.checker.Check(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, op, v)
				return sm, rep, nil
			}
		}
		if tick%typeCoverageCheckEvery == 0 {
			if v := checkTypedAll(tick, sm.oracle, sm.engine); len(v) > 0 {
				rep := sm.report(tick, op, v)
				return sm, rep, nil
			}
		}
	}
	// Terminal typed + temporal-ordering + list-predicate check.
	if v := checkTypedAll(lastTick, sm.oracle, sm.engine); len(v) > 0 {
		rep := sm.report(lastTick, lastOp, v)
		return sm, rep, nil
	}
	return sm, nil, nil
}
