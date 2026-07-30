package cypher

// index_intersect_plan_bench_test.go — PLAN-TIME benchmarks for the conjunctive
// index-intersection recogniser (#2134, budgeted by #2266).
//
// Layer: short (benchmarks do not run under `go test`).
//
// # Why the DECLINING shapes are the ones that matter
//
// The recogniser runs on the PLANNING path, so its cost is paid once per query
// rather than once per row — and it is paid by every shape it inspects, including
// the ones it ultimately refuses. A gate that is expensive only on the shapes it
// accepts is at least buying something; a gate that is expensive on the shapes it
// declines is pure loss. So every accepting shape below is paired with a declining
// one, and a CONTROL that bails at the very first check is included to show that
// the measurement itself is not drifting.
//
// # Fixture discipline
//
// Both fixtures are built ONCE per process behind a sync.Once and shared by every
// benchmark. Rebuilding a 20 000-node indexed fixture per benchmark has produced
// ±15–37 % bands and phantom regressions on untouched control arms in this repo
// before; the cost being measured here is single-digit microseconds, so fixture
// construction inside the timed region would drown it entirely.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// iiBenchFixture is one prepared graph plus the engine that indexes it.
type iiBenchFixture struct {
	eng *Engine
	g   *lpg.Graph[string, float64]
}

var (
	iiBigOnce   sync.Once
	iiBigFix    iiBenchFixture
	iiSmallOnce sync.Once
	iiSmallFix  iiBenchFixture
)

// iiBenchBig returns the shared 20 000-node :Doc fixture with btree indexes on
// a, b and s. Built once per process.
func iiBenchBig(b *testing.B) iiBenchFixture {
	b.Helper()
	iiBigOnce.Do(func() {
		g, _ := iiFixture(b)
		iiBigFix = iiBenchFixture{g: g, eng: iiEngine(b, g, false)}
	})
	return iiBigFix
}

// iiBenchSmall returns the shared 512-node :Small fixture with btree indexes on
// a and b — a population below the seek floor. Built once per process.
func iiBenchSmall(b *testing.B) iiBenchFixture {
	b.Helper()
	iiSmallOnce.Do(func() {
		g, eng := iiSmallLabelFixture(b)
		iiSmallFix = iiBenchFixture{g: g, eng: eng}
	})
	return iiSmallFix
}

// benchPlanBuild times ONE physical-plan build of q, with parsing, semantic
// analysis and IR translation hoisted out of the timed region (they are cached per
// query string by the engine anyway, so leaving them in would measure a cache hit
// rather than the planner). The tree is discarded without Init or Close, which is
// exactly what [Engine.explainPhysical] does and is safe because no operator
// acquires anything at construction time.
func benchPlanBuild(b *testing.B, fx iiBenchFixture, q string) {
	b.Helper()
	entry, err := fx.eng.parseAndAnalyse(q)
	if err != nil {
		b.Fatalf("parseAndAnalyse: %v", err)
	}
	reg := newNowAwareRegistry(fx.eng.reg, time.Now())
	ctx := context.Background()
	// One untimed build so any lazily-initialised planner state (the index
	// manager's listing, the label registry's interning) is warm in both arms.
	var warmErr error
	fx.g.View(func() {
		_, _, warmErr = fx.eng.buildReadPhysical(ctx, entry, entry.plan, nil, reg, nil)
	})
	if warmErr != nil {
		b.Fatalf("warm build: %v", warmErr)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var (
			op       exec.Operator
			buildErr error
		)
		fx.g.View(func() {
			op, _, buildErr = fx.eng.buildReadPhysical(ctx, entry, entry.plan, nil, reg, nil)
		})
		if buildErr != nil {
			b.Fatalf("build: %v", buildErr)
		}
		if op == nil {
			b.Fatal("build returned a nil operator")
		}
	}
}

// ── ACCEPTING shapes ────────────────────────────────────────────────────────────

// BenchmarkIndexIntersectPlan_AcceptTwoNumeric plans the canonical composition:
// two selective numeric conjuncts on different indexed properties.
func BenchmarkIndexIntersectPlan_AcceptTwoNumeric(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.a < 10 AND n.b < 30 RETURN n.s AS s`)
}

// BenchmarkIndexIntersectPlan_AcceptNumericAndString plans a composition across
// index types — the numeric companion ANDed with a string btree.
func BenchmarkIndexIntersectPlan_AcceptNumericAndString(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.a < 10 AND n.s < "s000300" RETURN n.s AS s`)
}

// BenchmarkIndexIntersectPlan_AcceptTwoOfThree plans a shape where two conjuncts
// compose and a third, far broader one is refused and left to the residual
// Filter — so the build pays both an accepting and a declining decision.
func BenchmarkIndexIntersectPlan_AcceptTwoOfThree(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.a < 10 AND n.b < 30 AND n.s < "s019000" RETURN n.s AS s`)
}

// ── DECLINING shapes ────────────────────────────────────────────────────────────

// BenchmarkIndexIntersectPlan_DeclineBroadNumeric plans two indexed numeric
// conjuncts that each cover ~90 % of the label. Both are refused, so the whole
// composition is refused — and the query gains nothing whatsoever from the work
// the gate did to reach that verdict.
func BenchmarkIndexIntersectPlan_DeclineBroadNumeric(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.a < 900 AND n.b < 900 RETURN n.s AS s`)
}

// BenchmarkIndexIntersectPlan_DeclineBroadString plans an open-ended string range
// (RangeCountFrom over all 20 000 distinct keys) ANDed with a broad numeric range.
// This is the worst declining shape available: the string btree holds one distinct
// value per node, so an unbudgeted count visits every entry in the index.
func BenchmarkIndexIntersectPlan_DeclineBroadString(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.s > "s000000" AND n.a < 900 RETURN n.s AS s`)
}

// BenchmarkIndexIntersectPlan_DeclineOneIndexed plans a conjunction in which only
// one side is indexed, so there is nothing to intersect with — yet the indexed
// side is still counted before the "fewer than two parts" verdict is reached.
func BenchmarkIndexIntersectPlan_DeclineOneIndexed(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.a < 900 AND n.missing = 1 RETURN n.s AS s`)
}

// BenchmarkIndexIntersectPlan_DeclineSmallLabel plans two indexed conjuncts over a
// label whose population is below the seek floor. NO count can change the verdict
// here, so every cardinality probe this shape pays is waste by construction.
func BenchmarkIndexIntersectPlan_DeclineSmallLabel(b *testing.B) {
	benchPlanBuild(b, iiBenchSmall(b),
		`MATCH (n:Small) WHERE n.a < 60 AND n.b < 60 RETURN n.a AS a`)
}

// ── CONTROL ─────────────────────────────────────────────────────────────────────

// BenchmarkIndexIntersectPlan_ControlSingleConjunct plans a predicate that is not
// a conjunction at all, so the recogniser bails at its first, cheapest check. It
// must not move: if it does, the measurement is drifting rather than the gate
// improving. It still exercises the shipped single-property range seek.
func BenchmarkIndexIntersectPlan_ControlSingleConjunct(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.a < 10 RETURN n.s AS s`)
}

// BenchmarkIndexIntersectPlan_ControlNoIndex plans a conjunction over properties
// that carry no index, so no count is ever reached. The second control arm.
func BenchmarkIndexIntersectPlan_ControlNoIndex(b *testing.B) {
	benchPlanBuild(b, iiBenchBig(b),
		`MATCH (n:Doc) WHERE n.missing1 < 10 AND n.missing2 < 30 RETURN n.s AS s`)
}
