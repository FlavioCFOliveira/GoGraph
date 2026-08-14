package sim

// stats_regime_test.go — unit tests for the statistics-driven planning regime
// oracle (rmp #2456): the happy path pins the db.stats.refresh() row shape,
// the rate-limit refusal, and the tracked-pairs observable on a real engine;
// the sensitivity proofs force each violation class through an honest seam
// (a genuinely different engine across the refresh boundary, a genuinely
// refreshed engine where a recovered one was claimed, a run that never
// engaged the path) and assert the checker fires.
//
// Layer: short.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestStatsRegime_RefreshAndThrottleContract is the happy path on the parity
// fixture, pinning the whole measured contract: a fresh engine holds no
// statistics; the first refresh returns exactly one ok=true row with the
// rebuilt detail and publishes exactly the fixture's three (Person,
// name/age/city) pairs; a back-to-back second call is refused in-band; every
// probe answer is identical across both calls; and the terminal Finish is
// satisfied. At HEAD the sim's Explain surface renders no statistics-derived
// annotation for the probe shapes, so the legal-plan-change report channel
// stays at zero (measured for rmp #2456).
func TestStatsRegime_RefreshAndThrottleContract(t *testing.T) {
	t.Parallel()
	a := newParityEngine(t, &cypher.EngineOptions{})
	k := NewStatsRegime(parityFixtureProbes()...)

	if got := a.StatsTrackedPairs(); got != 0 {
		t.Fatalf("StatsTrackedPairs on a fresh engine = %d, want 0", got)
	}
	if v := k.CheckRefresh(1, a, ExpectRebuild); len(v) != 0 {
		t.Fatalf("first refresh: expected a clean check, got violations:\n%v", v)
	}
	if got := a.StatsTrackedPairs(); got != 3 {
		t.Fatalf("StatsTrackedPairs after the rebuild = %d, want 3 — one per (Person, name/age/city)", got)
	}
	if v := k.CheckRefresh(2, a, ExpectRefusal); len(v) != 0 {
		t.Fatalf("back-to-back refresh: expected a clean in-band refusal, got violations:\n%v", v)
	}
	if got := a.StatsTrackedPairs(); got != 3 {
		t.Fatalf("StatsTrackedPairs after the refusal = %d, want 3 — a refusal must be a no-op", got)
	}
	if k.Refreshes() != 1 || k.Refusals() != 1 {
		t.Fatalf("refreshes=%d refusals=%d, want 1 and 1", k.Refreshes(), k.Refusals())
	}
	if k.PlanChanges() != 0 {
		t.Fatalf("PlanChanges = %d, want 0 at HEAD (no stats annotation on the sim Explain surface)", k.PlanChanges())
	}
	if v := k.Finish(3); len(v) != 0 {
		t.Fatalf("non-vacuity must be satisfied after a rebuild and a refusal, got:\n%v", v)
	}
}

// regimeFlipEngine serves every call from pre until the db.stats.refresh()
// query passes through it, and from post afterwards (the refresh itself runs
// on post). It is the honest seam for the result-identity sensitivity proof:
// the checker genuinely observes different data on the two sides of the
// refresh boundary, exactly what a refresh that corrupted a read path would
// look like.
type regimeFlipEngine struct {
	pre      *EngineAdapter
	post     *EngineAdapter
	switched bool
}

func (s *regimeFlipEngine) current() *EngineAdapter {
	if s.switched {
		return s.post
	}
	return s.pre
}

func (s *regimeFlipEngine) Run(ctx context.Context, query string, params map[string]any) (Result, error) {
	if query == statsRefreshQuery {
		s.switched = true
		return s.post.Run(ctx, query, params)
	}
	return s.current().Run(ctx, query, params)
}

func (s *regimeFlipEngine) Explain(query string, params map[string]any) (string, error) {
	return s.current().Explain(query, params)
}

func (s *regimeFlipEngine) Profile(ctx context.Context, query string, params map[string]any) (string, error) {
	return s.current().Profile(ctx, query, params)
}

func (s *regimeFlipEngine) NodeCount() (int64, error) { return s.current().NodeCount() }
func (s *regimeFlipEngine) EdgeCount() (int64, error) { return s.current().EdgeCount() }
func (s *regimeFlipEngine) StatsTrackedPairs() int    { return s.current().StatsTrackedPairs() }

// TestStatsRegime_FiresOnResultChangeAcrossRefresh is the mandatory
// sensitivity proof for the identity oracle: when the answers genuinely
// change across the refresh boundary (post holds one extra node inside the
// range probe's window and the prefix probe's city), the checker must fire
// ACID_CONSISTENCY violations naming the changed shapes — a refresh may
// change the plan, never the result.
func TestStatsRegime_FiresOnResultChangeAcrossRefresh(t *testing.T) {
	t.Parallel()
	flip := &regimeFlipEngine{
		pre:  newSeekResultsEngine(t, 300),
		post: newSeekResultsEngine(t, 300),
	}
	// One extra node on the post side only: age 101 lands inside the range
	// probe's [100,105) window, city c17 inside the starts-with probe's prefix.
	res, err := flip.post.RunWrite(context.Background(),
		"CREATE (:Person {name:'x-extra', age:101, city:'c17'})", nil)
	if err != nil {
		t.Fatalf("post-side extra node: %v", err)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("post-side extra node close: %v", cerr)
	}

	k := NewStatsRegime(parityFixtureProbes()...)
	v := k.CheckRefresh(1, flip, ExpectRebuild)
	if len(v) == 0 {
		t.Fatal("a genuine result change across the refresh boundary produced no violation")
	}
	foundRange, foundPrefix := false, false
	for _, viol := range v {
		if viol.Kind != ViolationACIDConsistency {
			t.Fatalf("violation kind = %s, want %s:\n%s", viol.Kind, ViolationACIDConsistency, viol.String())
		}
		if !strings.Contains(viol.Message, "changed a query RESULT") {
			t.Fatalf("violation does not name the identity contract:\n%s", viol.Message)
		}
		if strings.Contains(viol.Message, `shape "range"`) {
			foundRange = true
		}
		if strings.Contains(viol.Message, `shape "starts-with"`) {
			foundPrefix = true
		}
	}
	if !foundRange || !foundPrefix {
		t.Fatalf("expected the range and starts-with shapes to be named (range=%v, starts-with=%v):\n%v",
			foundRange, foundPrefix, v)
	}
}

// TestStatsRegime_FiresOnUnexpectedThrottleOutcome pins both expectation
// directions of the throttle contract: ExpectRefusal on a fresh engine (the
// rebuild goes through, so the guard did not engage) and ExpectRebuild on an
// engine whose limiter is already stamped (the fresh-limiter claim is false)
// must each fire ORACLE_DEVIATION.
func TestStatsRegime_FiresOnUnexpectedThrottleOutcome(t *testing.T) {
	t.Parallel()
	a := newSeekResultsEngine(t, 60)
	k := NewStatsRegime(parityFixtureProbes()...)

	v := k.CheckRefresh(1, a, ExpectRefusal)
	if len(v) != 1 || v[0].Kind != ViolationOracleDeviation ||
		!strings.Contains(v[0].Message, "NOT refused") {
		t.Fatalf("ExpectRefusal on a fresh engine must fire the amplification-guard violation, got:\n%v", v)
	}

	v = k.CheckRefresh(2, a, ExpectRebuild)
	if len(v) != 1 || v[0].Kind != ViolationOracleDeviation ||
		!strings.Contains(v[0].Message, "refused") {
		t.Fatalf("ExpectRebuild inside the window must fire the leaked-limiter violation, got:\n%v", v)
	}
}

// TestStatsRegime_RecoveredContract pins both halves of the post-crash
// regime: a fresh engine (statistics never built — the recovered shape)
// passes CheckRecovered, and an engine that genuinely holds statistics fed to
// CheckRecovered fires — the honest seam for a recovery that started leaking
// or rebuilding collector state.
func TestStatsRegime_RecoveredContract(t *testing.T) {
	t.Parallel()
	k := NewStatsRegime(parityFixtureProbes()...)

	fresh := newSeekResultsEngine(t, 60)
	if v := k.CheckRecovered(1, fresh); len(v) != 0 {
		t.Fatalf("a statistics-free engine must pass the recovered-regime check, got:\n%v", v)
	}

	refreshed := newSeekResultsEngine(t, 60)
	res, err := refreshed.Run(context.Background(), statsRefreshQuery, nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("refresh drain: %v", err)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("refresh close: %v", cerr)
	}
	if got := refreshed.StatsTrackedPairs(); got <= 0 {
		t.Fatalf("fixture defect: the refreshed engine tracks %d pairs, want > 0", got)
	}
	v := k.CheckRecovered(2, refreshed)
	if len(v) != 1 || v[0].Kind != ViolationOracleDeviation ||
		!strings.Contains(v[0].Message, "want 0") {
		t.Fatalf("an engine holding statistics must fail the recovered-regime check, got:\n%v", v)
	}
}

// TestStatsRegime_FiresOnVacuousRun pins the non-vacuity gate: a run that
// never completed a rebuild and never observed a refusal proved nothing about
// the statistics path, and Finish must say so twice — once per missing
// observation.
func TestStatsRegime_FiresOnVacuousRun(t *testing.T) {
	t.Parallel()
	k := NewStatsRegime(parityFixtureProbes()...)
	v := k.Finish(1)
	if len(v) != 2 {
		t.Fatalf("Finish on an idle checker returned %d violations, want 2 (no rebuild, no refusal):\n%v", len(v), v)
	}
	if !strings.Contains(v[0].Message, "statistics path never engaged") {
		t.Fatalf("first vacuity violation does not name the missing rebuild:\n%s", v[0].Message)
	}
	if !strings.Contains(v[1].Message, "throttle") {
		t.Fatalf("second vacuity violation does not name the unexercised throttle:\n%s", v[1].Message)
	}
}
