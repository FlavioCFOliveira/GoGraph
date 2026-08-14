package sim

import (
	"context"
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
