package sim

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// typedSeedParams returns a full [tmplCreateTyped] parameter set for id, with
// every temporal bound as a GENUINE temporal value. Tests override individual
// keys to build the seams they need.
func typedSeedParams(id int64) map[string]any {
	return map[string]any{
		"id":  id,
		"s":   "x",
		"i":   int64(2),
		"f":   1.0,
		"b":   true,
		"lst": []any{int64(1)},
		"ts":  "2026-01-01T00:00:00Z",
		"d":   expr.NewDate(2026, 1, typedDateDay(id)),
		"ldt": expr.NewLocalDateTime(2026, 2, 3, 4, 5, 6, 0),
		"dt":  expr.NewDateTime(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		"lt":  expr.NewLocalTime(4, 5, 6, 0),
		"tm":  expr.NewTime(4, 5, 0, 0, 3600),
		"du":  expr.NewDuration(1, 2, 3, 0),
	}
}

// TestTypeCoverage_Scenario_Passes runs the registered type-coverage scenario:
// every property kind (string/int/float/bool/list/ISO-string + the six temporal
// types + a NULL-reading absent key) must round-trip through commit WITH ITS
// KIND INTACT and survive crash/recovery. A nil report means every kind
// round-tripped on every check, including post-recovery.
func TestTypeCoverage_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioTypeCoverage)
	if !ok {
		t.Fatalf("type-coverage scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("type-coverage run: %v", err)
	}
	if report != nil {
		t.Fatalf("type-coverage reported a violation (a property kind did not round-trip/survive):\n%s", report)
	}
}

// TestTypeCoverage_NonVacuous confirms the run created Typed nodes and genuinely
// exercised BOTH durable recovery paths — it crashed at least once and published
// at least one real checkpoint — so the round-trip, kind and survival checks were
// not asserted against a graph that never left memory. It also pins that the
// temporal ORDER differs from the id order, which is what makes
// [CheckTypedTemporalOrder] a real oracle rather than a tautology.
func TestTypeCoverage_NonVacuous(t *testing.T) {
	sm, report, err := runTypeCoverageSim(context.Background(), 0x7A9E5)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runTypeCoverageSim: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
	ids := sm.Oracle().TypedIDs()
	if len(ids) < 2 {
		t.Fatalf("run modelled %d Typed nodes, want >= 2 (nothing was checked)", len(ids))
	}
	if sm.CrashCount() == 0 {
		t.Fatal("run never crashed: the post-recovery kind assertions never ran")
	}
	if sm.CheckpointCount() == 0 {
		t.Fatal("run never checkpointed: the snapshot recovery path was never exercised")
	}
	// The date order must differ from the id order, else ORDER BY n.d could be
	// satisfied by any stable ordering and the oracle would prove nothing.
	differs := false
	for i := 1; i < len(ids); i++ {
		if typedDateDay(ids[i]) < typedDateDay(ids[i-1]) {
			differs = true
			break
		}
	}
	if !differs {
		t.Fatal("temporal order equals id order: CheckTypedTemporalOrder is vacuous")
	}
}

// TestTypeCoverage_TemporalRoundTripPinned is the happy-path pin: one Typed node
// written through the simulator's own write path (so the parameters travel
// through [toExprValue]) must read back with each temporal's exact KIND and
// canonical rendering — and `ts`, the plain ISO-8601 string, must read back as a
// STRING. It fails if a temporal silently becomes a string or vice versa.
func TestTypeCoverage_TemporalRoundTripPinned(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	if _, err := sm.engine.RunWrite(ctx, tmplCreateTyped, typedSeedParams(1)); err != nil {
		t.Fatalf("seed typed node: %v", err)
	}

	want := []struct {
		key  string
		text string
		kind expr.Kind
	}{
		{"d", "2026-01-08", expr.KindDate},
		{"ldt", "2026-02-03T04:05:06", expr.KindLocalDateTime},
		{"dt", "2026-02-03T04:05:06Z", expr.KindDateTime},
		{"lt", "04:05:06", expr.KindLocalTime},
		{"tm", "04:05+01:00", expr.KindTime},
		{"du", "P1M2DT3S", expr.KindDuration},
		// The control: a plain ISO-8601 string must stay a string.
		{"ts", `"2026-01-01T00:00:00Z"`, expr.KindString},
	}
	for _, w := range want {
		got, err := sm.engine.projectRowValues(ctx, "MATCH (n:Typed {id:1}) RETURN n."+w.key, 1)
		if err != nil {
			t.Fatalf("read n.%s: %v", w.key, err)
		}
		if got == nil {
			t.Fatalf("read n.%s: no row", w.key)
		}
		if got[0].Kind() != w.kind {
			t.Errorf("n.%s kind = %v, want %v (value %s)", w.key, got[0].Kind(), w.kind, got[0].String())
		}
		if got[0].String() != w.text {
			t.Errorf("n.%s = %s, want %s", w.key, got[0].String(), w.text)
		}
	}
}

// TestTypeCoverage_DetectsValueMismatch is the meta-test: it proves the checker
// CATCHES a property that fails to round-trip. The oracle is told a Typed node
// carries one value while the engine stores a different one; CheckTypedProperties
// must flag the divergence.
func TestTypeCoverage_DetectsValueMismatch(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	// Engine stores f:1.0; oracle is told f:999.0 — a deliberate divergence.
	if _, err := sm.engine.RunWrite(ctx, tmplCreateTyped, typedSeedParams(1)); err != nil {
		t.Fatalf("seed typed node: %v", err)
	}
	model := typedSeedParams(1)
	model["f"] = 999.0
	sm.oracle.typed[1] = model
	if v := CheckTypedProperties(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("checker FAILED to detect a property value mismatch")
	}
}

// TestTypeCoverage_DetectsTemporalDegradedToString is the SENSITIVITY PROOF for
// rmp #2457: it reproduces exactly the pre-#2457 write — the temporals bound as
// ISO-8601 STRINGS instead of temporal values — while the oracle models the
// genuine temporals, and requires the checker to FIRE on every degraded key.
//
// This is the failure the old string-only arm could not see: the stored text is
// byte-for-byte the temporal's canonical rendering, so only the KIND assertion
// separates a working temporal round-trip from a broken one.
func TestTypeCoverage_DetectsTemporalDegradedToString(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	model := typedSeedParams(1)
	// The seam: bind each temporal's own canonical text as a plain STRING, so the
	// engine stores an UNTAGGED PropString that reads back as expr.StringValue
	// carrying identical text.
	degraded := typedSeedParams(1)
	for _, k := range []string{"d", "ldt", "dt", "lt", "tm", "du"} {
		ev, cerr := toExprValue(model[k])
		if cerr != nil {
			t.Fatalf("toExprValue(%s): %v", k, cerr)
		}
		degraded[k] = ev.String()
	}
	if _, err := sm.engine.RunWrite(ctx, tmplCreateTyped, degraded); err != nil {
		t.Fatalf("seed degraded node: %v", err)
	}
	sm.oracle.typed[1] = model

	vs := CheckTypedProperties(1, sm.oracle, sm.engine)
	if len(vs) == 0 {
		t.Fatal("checker FAILED to detect temporals degraded to plain strings" +
			" — the temporal arm is still a tautology")
	}
	// Every one of the six temporal keys must be reported, and each must be
	// reported as a KIND failure (the text is identical, so a value-only checker
	// would see nothing).
	for _, k := range []string{"d", "ldt", "dt", "lt", "tm", "du"} {
		found := false
		for _, v := range vs {
			if v.Op == "typed property kind" && strings.Contains(v.Message, "."+k+" has kind") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("degraded temporal %q was NOT reported as a kind violation; got %v", k, vs)
		}
	}
}

// typedListFixture seeds one Typed node per entry of lists — with the given
// `lst` value and every other typed property from [typedSeedParams] — in BOTH
// the engine and the oracle, so the list-predicate checker sees a consistent
// state. The caller owns nothing: the simulator is closed by t.Cleanup.
func typedListFixture(t *testing.T, lists map[int64][]any) *Simulator {
	t.Helper()
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()
	for id, l := range lists {
		params := typedSeedParams(id)
		params["lst"] = l
		if _, err := sm.engine.RunWrite(ctx, tmplCreateTyped, params); err != nil {
			t.Fatalf("seed Typed{id:%d}: %v", id, err)
		}
		sm.oracle.typed[id] = params
	}
	return sm
}

// TestTypeCoverage_ListPredicateContractsPinned is the happy-path pin for rmp
// #2459: over a hand-chosen stored list it asserts the exact value AND expr kind
// every list predicate must produce, so the contracts the checker relies on are
// recorded here rather than assumed — a subscript past either end yields NULL
// (not an error and not a clamp), a NEGATIVE index counts from the end, and a
// slice is half-open and clamps to the list's own bounds. The oracle-driven
// checker must then be clean on the same fixture.
func TestTypeCoverage_ListPredicateContractsPinned(t *testing.T) {
	sm := typedListFixture(t, map[int64][]any{
		1: {int64(10), int64(20), int64(30)},
		2: {int64(20), int64(40)},
	})
	ctx := context.Background()

	single := []struct {
		expr string
		text string
		kind expr.Kind
	}{
		{"n.lst[0]", "10", expr.KindInteger},
		{"n.lst[1]", "20", expr.KindInteger},
		{"n.lst[-1]", "30", expr.KindInteger},       // negative counts from the END
		{"n.lst[-3]", "10", expr.KindInteger},       // …to the first element
		{"n.lst[3]", "null", expr.KindNull},         // past the end: NULL, not an error
		{"n.lst[-4]", "null", expr.KindNull},        // before the start: NULL
		{"n.lst[99]", "null", expr.KindNull},        // far past the end: still NULL
		{"size(n.lst)", "3", expr.KindInteger},      // size of a STORED list
		{"n.lst[0..2]", "[10, 20]", expr.KindList},  // half-open [from, to)
		{"n.lst[1..99]", "[20, 30]", expr.KindList}, // clamps to the list bounds
		{"reduce(acc = 0, x IN n.lst | acc + x)", "60", expr.KindInteger},
	}
	for _, w := range single {
		got, err := sm.engine.projectRowValues(ctx, "MATCH (n:Typed {id:1}) RETURN "+w.expr, 1)
		if err != nil {
			t.Fatalf("read %s: %v", w.expr, err)
		}
		if got == nil {
			t.Fatalf("read %s: no row", w.expr)
		}
		if got[0].Kind() != w.kind {
			t.Errorf("%s kind = %v, want %v (value %s)", w.expr, got[0].Kind(), w.kind, got[0].String())
		}
		if got[0].String() != w.text {
			t.Errorf("%s = %s, want %s", w.expr, got[0].String(), w.text)
		}
	}

	// UNWIND over the STORED list, aggregated three ways.
	got, err := sm.engine.projectRowValues(ctx,
		"MATCH (n:Typed {id:1}) UNWIND n.lst AS x RETURN count(x), sum(x), collect(x)", 3)
	if err != nil || got == nil {
		t.Fatalf("UNWIND probe: got=%v err=%v", got, err)
	}
	for i, w := range []string{"3", "60", "[10, 20, 30]"} {
		if got[i].String() != w {
			t.Errorf("UNWIND column %d = %s, want %s", i, got[i].String(), w)
		}
	}

	// Membership over the whole label: 20 is in both lists, 10 in one, and the
	// absent-element control in none.
	for _, m := range []struct {
		elem int64
		want string
	}{{20, "2"}, {10, "1"}, {40, "1"}, {typedListAbsentElem, "0"}} {
		q := fmt.Sprintf("MATCH (n:Typed) WHERE %d IN n.lst RETURN count(n)", m.elem)
		got, err := sm.engine.projectRowValues(ctx, q, 1)
		if err != nil || got == nil {
			t.Fatalf("membership %d: got=%v err=%v", m.elem, got, err)
		}
		if got[0].String() != m.want {
			t.Errorf("count of `%d IN n.lst` = %s, want %s", m.elem, got[0].String(), m.want)
		}
	}

	if v := CheckTypedListPredicates(1, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("list-predicate checker fired on a faithful model: %v", v)
	}
}

// TestTypeCoverage_ListPredicatesDetectPerturbation is the SENSITIVITY PROOF for
// rmp #2459: each arm of the list battery must FIRE when the oracle's modelled
// list is perturbed in the one way that arm is responsible for seeing. Without
// it the arm could be reading the engine's answer back into its own expectation
// and proving nothing.
func TestTypeCoverage_ListPredicatesDetectPerturbation(t *testing.T) {
	base := func() map[int64][]any {
		return map[int64][]any{
			1: {int64(10), int64(20), int64(30)},
			2: {int64(20), int64(40)},
		}
	}
	// perturb replaces the modelled (NOT the stored) list of one node.
	perturb := func(sm *Simulator, id int64, l []any) {
		props := typedSeedParams(id)
		props["lst"] = l
		sm.oracle.typed[id] = props
	}

	t.Run("baseline clean", func(t *testing.T) {
		sm := typedListFixture(t, base())
		if v := CheckTypedListPredicates(1, sm.oracle, sm.engine); len(v) > 0 {
			t.Fatalf("baseline should be clean, got: %v", v)
		}
	})

	t.Run("first element changed fires the subscript arm", func(t *testing.T) {
		sm := typedListFixture(t, base())
		perturb(sm, 1, []any{int64(11), int64(20), int64(30)})
		if v := CheckTypedListPredicates(1, sm.oracle, sm.engine); len(v) == 0 {
			t.Fatal("n.lst[0] FAILED to detect a changed first element")
		}
	})

	t.Run("last element changed fires the negative-subscript arm", func(t *testing.T) {
		sm := typedListFixture(t, base())
		perturb(sm, 1, []any{int64(10), int64(20), int64(31)})
		if v := CheckTypedListPredicates(1, sm.oracle, sm.engine); len(v) == 0 {
			t.Fatal("n.lst[-1] FAILED to detect a changed last element")
		}
	})

	t.Run("interior element changed fires reduce and UNWIND", func(t *testing.T) {
		sm := typedListFixture(t, base())
		// Neither subscript column reads element 1, so only the sum-bearing arms
		// (reduce, sum(x), collect(x)) can see this one.
		perturb(sm, 1, []any{int64(10), int64(99), int64(30)})
		vs := CheckTypedListPredicates(1, sm.oracle, sm.engine)
		if len(vs) == 0 {
			t.Fatal("reduce/UNWIND FAILED to detect a changed interior element")
		}
		saw := map[string]bool{}
		for _, v := range vs {
			saw[v.Op] = true
		}
		if !saw["typed list predicate"] || !saw["typed list UNWIND"] {
			t.Errorf("expected BOTH the reduce and the UNWIND arms to fire, got ops %v", saw)
		}
	})

	t.Run("element removed fires size and the out-of-range column", func(t *testing.T) {
		sm := typedListFixture(t, base())
		perturb(sm, 1, []any{int64(10), int64(20)})
		if v := CheckTypedListPredicates(1, sm.oracle, sm.engine); len(v) == 0 {
			t.Fatal("size(n.lst) FAILED to detect a removed element")
		}
	})

	t.Run("order reversed fires the ORDERED columns only", func(t *testing.T) {
		sm := typedListFixture(t, base())
		// A reversal leaves size, sum, reduce and the collect MULTISET untouched:
		// it is exactly the defect the ordered subscript/slice columns exist for.
		perturb(sm, 1, []any{int64(30), int64(20), int64(10)})
		vs := CheckTypedListPredicates(1, sm.oracle, sm.engine)
		if len(vs) == 0 {
			t.Fatal("the ordered columns FAILED to detect a reversed list")
		}
		for _, v := range vs {
			if v.Op == "typed list UNWIND" {
				t.Errorf("the UNWIND multiset arm must be blind to a reversal, but it fired: %v", v)
			}
		}
	})

	t.Run("membership counts the WHOLE model, not the sample", func(t *testing.T) {
		// More nodes than the per-node probe samples, so a node the sample skips
		// can be perturbed to isolate the membership arm from every other arm.
		lists := make(map[int64][]any, 10)
		ids := make([]int64, 0, 10)
		for id := int64(0); id < 10; id++ {
			lists[id] = []any{id, int64(100 + id)}
			ids = append(ids, id)
		}
		sm := typedListFixture(t, lists)
		if v := CheckTypedListPredicates(1, sm.oracle, sm.engine); len(v) > 0 {
			t.Fatalf("baseline should be clean, got: %v", v)
		}
		sample := typedListSample(ids)
		var skipped int64 = -1
		for _, id := range ids {
			if !slices.Contains(sample, id) {
				skipped = id
				break
			}
		}
		if skipped < 0 {
			t.Fatalf("no id was skipped by the sample %v: the isolation is impossible", sample)
		}
		// The probe element is the last element of the NEWEST list (id 9 → 109);
		// claiming a non-sampled node also holds it changes only the count.
		perturb(sm, skipped, []any{skipped, int64(109)})
		vs := CheckTypedListPredicates(1, sm.oracle, sm.engine)
		if len(vs) == 0 {
			t.Fatal("membership FAILED to detect a model that claims one more node holds the element")
		}
		for _, v := range vs {
			if v.Op != "typed list membership" {
				t.Errorf("only the membership arm should fire, got %s: %s", v.Op, v.Message)
			}
		}
	})

	t.Run("empty modelled list is reported as vacuous", func(t *testing.T) {
		sm := typedListFixture(t, base())
		perturb(sm, 1, []any{})
		vs := CheckTypedListPredicates(1, sm.oracle, sm.engine)
		if len(vs) == 0 {
			t.Fatal("an EMPTY modelled list must be reported: every predicate over it is vacuous")
		}
		if vs[0].Op != "typed list model" {
			t.Errorf("empty list reported as %q, want the model gate", vs[0].Op)
		}
	})

	t.Run("a single distinct element across the model is reported as vacuous", func(t *testing.T) {
		sm := typedListFixture(t, map[int64][]any{
			1: {int64(7)},
			2: {int64(7), int64(7)},
		})
		vs := CheckTypedListPredicates(1, sm.oracle, sm.engine)
		if len(vs) == 0 {
			t.Fatal("a model with one distinct element must be reported: membership/reduce prove nothing")
		}
		if vs[0].Op != "typed list model" {
			t.Errorf("degenerate model reported as %q, want the model gate", vs[0].Op)
		}
	})
}

// TestTypeCoverage_ListPredicatesNonVacuous confirms the registered scenario
// really drove the list arm: the run must model lists that are non-empty and
// carry at least two distinct elements over the whole run — the condition
// [CheckTypedListPredicates] enforces — and the checker must be clean on the
// terminal graph.
func TestTypeCoverage_ListPredicatesNonVacuous(t *testing.T) {
	sm, report, err := runTypeCoverageSim(context.Background(), 0x7A9E5)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runTypeCoverageSim: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
	ids := sm.Oracle().TypedIDs()
	lists, vs := typedModelledLists(0, sm.Oracle(), ids)
	if len(vs) > 0 {
		t.Fatalf("terminal model is not a usable list model: %v", vs)
	}
	if len(lists) < 2 {
		t.Fatalf("run modelled %d lists, want >= 2", len(lists))
	}
	for id, l := range lists {
		if len(l) == 0 {
			t.Fatalf("Typed{id:%d} modelled an EMPTY list", id)
		}
	}
	if d := typedListDistinctElems(lists); d < 2 {
		t.Fatalf("the whole run carries %d distinct list element(s), want >= 2", d)
	}
	if v := CheckTypedListPredicates(0, sm.Oracle(), sm.engine); len(v) > 0 {
		t.Fatalf("list predicates fired on the terminal graph: %v", v)
	}
	// Wiring: the battery entry point the scenario loop calls must really include
	// the list arm, else the checker above would be dead code inside the run.
	perturbed := typedSeedParams(ids[0])
	perturbed["lst"] = []any{int64(-1), int64(-2), int64(-3)}
	sm.oracle.typed[ids[0]] = perturbed
	saw := false
	for _, v := range checkTypedAll(0, sm.Oracle(), sm.engine) {
		if strings.HasPrefix(v.Op, "typed list") {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatal("checkTypedAll does not report the list arm: the scenario never runs it")
	}
}

// TestTypeCoverage_TemporalOrderOracle detects a wrong temporal ordering. The
// engine holds dates whose order differs from the id order; the oracle is then
// told a DIFFERENT date for one node, so the ordering it computes no longer
// matches the engine's and [CheckTypedTemporalOrder] must fire. The same check
// must pass on the untouched model, so the test pins both directions.
func TestTypeCoverage_TemporalOrderOracle(t *testing.T) {
	cfg := Config{Seed: 1, MaxTicks: 1, Workload: typeCoverageWorkload(NewSeed(1))}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	ctx := context.Background()

	const n = 6
	for id := int64(0); id < n; id++ {
		params := typedSeedParams(id)
		if _, err := sm.engine.RunWrite(ctx, tmplCreateTyped, params); err != nil {
			t.Fatalf("seed typed node %d: %v", id, err)
		}
		sm.oracle.typed[id] = params
	}
	if v := CheckTypedTemporalOrder(1, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("temporal-order oracle fired on a faithful model: %v", v)
	}

	// Perturb the model: node 0 now claims the LAST date, so the oracle's
	// computed order no longer matches the engine's.
	perturbed := typedSeedParams(0)
	perturbed["d"] = expr.NewDate(2099, 12, 31)
	sm.oracle.typed[0] = perturbed
	if v := CheckTypedTemporalOrder(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("temporal-order oracle FAILED to detect a divergent ordering")
	}
}
