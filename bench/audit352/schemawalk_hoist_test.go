package audit352_test

// schemawalk_hoist_test.go — the permanent regression gate for rmp #2645:
// the row-context schema walk is derived at PLAN-BUILD time, never per row.
//
// # What #2645 changed
//
// `cypher.newSchemaWalk` allocates a slice and sorts it. It used to run once per
// ROW, from `buildRowCtxWithUse` and from `evalRowPooled`, at twelve call sites
// that each handed those builders a bare `map[string]int`. #2645 introduced the
// `cypher.rowSchema` carrier — snapshot, walk and width, built once by
// `newRowSchema` — deleted the map-taking forms of the three builders, and
// converted every site. `newSchemaWalk` now has exactly one caller.
//
// # Why this gate is expressed as a frame and as a slope, never as a percentage
//
// A share can move for a dozen unrelated reasons and would make this gate
// bark at innocent changes. Two properties cannot:
//
//	(1) NOT PER ROW. In an exact (MemProfileRate=1) profile of a bracketed query
//	    window, every allocation beneath newSchemaWalk must sit on a stack that
//	    also passes through the PLAN BUILDER. An allocation reached from an
//	    operator's Next instead is, by construction, a site that was left
//	    unconverted or has regressed to the per-row form.
//
//	(2) CONSTANT IN ROWS. The same shape run over 4x the rows must derive
//	    EXACTLY the same number of walks. A per-row derivation cannot satisfy
//	    this at any row count; a plan-time one satisfies it trivially. This is
//	    the structural half of the gate: it needs no frame taxonomy, so it keeps
//	    working even if the builder's internal frame names change.
//
// Absence of a frame from an exact profile is proof the code did not run —
// presence is not a sampling accident at rate 1 — so a PARTIAL conversion
// cannot satisfy (1), and no conversion at all cannot satisfy (2).
//
// # Why the window cannot simply exclude the plan build
//
// `(*Engine).Run` rebuilds the physical plan on every execution
// (`buildReadPhysical`), so the one legitimate, plan-time derivation of each
// rowSchema lands inside any window that contains a query. The gate therefore
// discriminates by STACK rather than by count: measured on the converted tree,
// every surviving derivation is reached through `newRowSchema` from a plan
// builder, and none from an operator.
//
// # Measured at the conversion (2026-08-29, exact rate-1 windows, n=3000)
//
//	shape                                    walk objects   window objects
//	RETURN DISTINCT p.bucket                   3000 -> 1       4646 -> 1647
//	RETURN p.bucket + 1 AS b, count(*)         3000 -> 1      16900 -> 13901
//	UNWIND [1,2,3] AS x RETURN p.firstName,x  12000 -> 2      76449 -> 64451
//	... ORDER BY p.salary SKIP 0 LIMIT 10      3001 -> 2      37522 -> 34525
//	MATCH (p:Person) RETURN p.salary (control)    0 -> 1       1550 -> 1551
//
// The control shape executes fully columnar and never reached the row path, so
// it gained the one plan-time derivation (24 bytes per execution) that every
// other shape saved thousands of. That trade is recorded here so a future reader
// does not rediscover it as a regression.

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// frameSchemaWalk is the derivation #2645 removed from the per-row path. It is
// unexported in package cypher, so a profile frame is the only way to observe
// it — which is exactly the observation that settles the question.
const frameSchemaWalk = "cypher.newSchemaWalk"

// planBuilderFrames mark a stack as belonging to physical-plan construction.
// One of them is present on every legitimate (plan-time) walk derivation and on
// none of the per-row ones, which is what makes the discriminator sound rather
// than merely plausible.
var planBuilderFrames = []string{
	"cypher.buildOperatorRec",
	"cypher.buildOperator",
	"cypher.buildOperatorWrite",
	"cypher.buildPlanEngine",
	"cypher.(*Engine).buildReadPhysical",
	"cypher.tryBuildColumnarFilterChain",
}

// isPlanBuildStack reports whether frames contains a plan-builder frame.
//
// The comparison is by SUFFIX because runtime.CallersFrames reports fully
// qualified names ("github.com/…/cypher.newSchemaWalk"). An equality test here
// matched nothing, counted every derivation as zero, and made this gate pass on
// a tree it had not examined — the exact failure mode
// [TestSchemaWalkGateIsNotBlind] now rules out.
func isPlanBuildStack(frames []string) bool {
	for _, f := range frames {
		for _, want := range planBuilderFrames {
			if hasFrameSuffix(f, want) {
				return true
			}
		}
	}
	return false
}

// walkShapes are the shapes #2645 was measured on. Each reaches the row-context
// builders through a DIFFERENT one of the converted sites, and the last is the
// null control that never reaches them at all.
var walkShapes = []struct {
	name  string
	query string
	rows  func(n int) int
	// derives records whether building this shape's physical plan constructs a
	// rowSchema at all. It gates the not-blind check: a shape that legitimately
	// derives none (whole-node projection) must not be required to show one.
	derives bool
}{
	// buildIRProjection's general path, under Distinct — the largest share
	// measured at HEAD (64.57% of all objects allocated by the query).
	{"distinct", `MATCH (p:Person) RETURN DISTINCT p.bucket`, func(int) int { return 100 }, true},
	// newAggregationEval's pre-projection closure (17.75%).
	{"agg_expr", `MATCH (p:Person) RETURN p.bucket + 1 AS b, count(*) AS c`, func(int) int { return 100 }, true},
	// buildUnwindOperator's list closure, plus the projection (15.70%).
	{"unwind", `MATCH (p:Person) UNWIND [1,2,3] AS x RETURN p.firstName, x`, func(n int) int { return 3 * n }, true},
	// irSortKeys (hoisted by #2652) over the projection general path (8.00%).
	{"sort_top", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`, func(int) int { return 10 }, true},
	// newRowPredicate (hoisted by #2415) over a columnar filter chain.
	{"scan_filter", `MATCH (p:Person) WHERE p.bucket < 25 RETURN p.firstName`, func(n int) int { return n * 25 / 100 }, true},
	// Fully columnar ON THIS FIXTURE. The same query plans
	// ParallelScanProject on the 120 000-node benchGraph and takes the row path
	// there — see [TestSchemaWalkIsNotPerRowUnderParallelScan]. Both regimes are
	// covered deliberately: a plan-shape change must not be able to move this
	// site out from under the gate.
	{"columnar_project", `MATCH (p:Person) RETURN p.salary`, func(n int) int { return n }, true},
}

var (
	walkEngineMu sync.Mutex
	walkEngines  = map[int]*cypher.Engine{}
)

// walkShapeEngine builds (once per size) the fixture the shapes run on.
// `bucket` is i%100 so `bucket < 25` ships exactly 25% of rows, and `salary` is
// spread by a multiplicative hash so ORDER BY is not already sorted.
func walkShapeEngine(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	walkEngineMu.Lock()
	defer walkEngineMu.Unlock()
	if e, ok := walkEngines[n]; ok {
		return e
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(key)); err != nil {
			tb.Fatalf("SetNodeProperty firstName: %v", err)
		}
		if err := g.SetNodeProperty(key, "salary", lpg.Int64Value(int64(100_000+(i*2_654_435_761)%65_536))); err != nil {
			tb.Fatalf("SetNodeProperty salary: %v", err)
		}
		if err := g.SetNodeProperty(key, "bucket", lpg.Int64Value(int64(i%100))); err != nil {
			tb.Fatalf("SetNodeProperty bucket: %v", err)
		}
	}
	e := cypher.NewEngine(g)
	walkEngines[n] = e
	return e
}

// walkDerivations runs one query inside an exact bracketed window and returns
// how many objects were allocated beneath newSchemaWalk, split by whether the
// stack passed through the plan builder.
func walkDerivations(tb testing.TB, eng *cypher.Engine, query string, wantRows int) (planTime, perRow int64, detail string) {
	tb.Helper()
	// Warm the plan cache OUTSIDE the window so one-off compilation cannot be
	// read as a path difference.
	if got := drainCounting(tb, eng, query); got != wantRows {
		tb.Fatalf("warm-up shipped %d rows, want %d", got, wantRows)
	}
	at := exerciseAttributed(tb, 1, func() {
		if got := drainCounting(tb, eng, query); got != wantRows {
			tb.Fatalf("shipped %d rows, want %d", got, wantRows)
		}
	})
	at.assertDescribesWindow(tb, query)
	var sb strings.Builder
	for _, site := range at.sites {
		hit := false
		for _, f := range site.frames {
			if hasFrameSuffix(f, frameSchemaWalk) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if isPlanBuildStack(site.frames) {
			planTime += site.objects
			continue
		}
		perRow += site.objects
		fmt.Fprintf(&sb, "      %d objects via:", site.objects)
		shown := 0
		for _, f := range site.frames {
			if isAllocatorPlumbing(f) {
				continue
			}
			fmt.Fprintf(&sb, " <- %s", shortFn(f))
			if shown++; shown >= 8 {
				break
			}
		}
		sb.WriteString("\n")
	}
	return planTime, perRow, sb.String()
}

// TestSchemaWalkIsNotPerRow is the #2645 gate. See the file comment for why the
// two assertions are a frame and a slope rather than a percentage.
func TestSchemaWalkIsNotPerRow(t *testing.T) {
	const (
		small = 1_000
		large = 4 * small
	)
	for _, s := range walkShapes {
		s := s
		t.Run(s.name, func(t *testing.T) {
			planSmall, perRowSmall, detail := walkDerivations(
				t, walkShapeEngine(t, small), s.query, s.rows(small))
			if perRowSmall != 0 {
				t.Errorf("%d objects were allocated beneath %s from OUTSIDE the plan builder. "+
					"The walk is a pure function of a schema frozen for the whole execution, so "+
					"reaching it from a per-row closure means a site was left unconverted or has "+
					"regressed to the map-taking form deleted by #2645.\n%s",
					perRowSmall, frameSchemaWalk, detail)
			}

			planLarge, perRowLarge, detailLarge := walkDerivations(
				t, walkShapeEngine(t, large), s.query, s.rows(large))
			if perRowLarge != 0 {
				t.Errorf("at %dx the rows, %d objects beneath %s came from outside the plan "+
					"builder:\n%s", large/small, perRowLarge, frameSchemaWalk, detailLarge)
			}

			t.Logf("%-16s walks: n=%d -> %d, n=%d -> %d (plan-time)",
				s.name, small, planSmall, large, planLarge)

			// THE GATE MUST SEE SOMETHING. Every shape derives at least one
			// rowSchema while its physical plan is built, and that build happens
			// inside the window because (*Engine).Run rebuilds the plan per
			// execution. A zero here does not mean "nothing per row", it means the
			// instrument observed no newSchemaWalk frame at all — a renamed frame,
			// an inlined-away derivation, or a broken suffix match — and the
			// per-row assertions above are then vacuous rather than satisfied.
			if s.derives && planSmall == 0 {
				t.Errorf("%s: the window contains NO allocation beneath %s, not even the "+
					"plan-time derivation every physical build performs. This gate is blind, "+
					"so its per-row verdict is worthless. Check the frame name before trusting "+
					"a pass.", s.name, frameSchemaWalk)
			}

			// The structural half: a plan-time derivation cannot depend on how
			// many rows flow through the plan it built.
			if planSmall != planLarge {
				t.Errorf("%s derived %d walks over %d rows but %d over %d rows. A count that "+
					"tracks the row count IS a per-row derivation, whatever the stacks say.",
					s.name, planSmall, s.rows(small), planLarge, s.rows(large))
			}
		})
	}
}

// parallelShapes cover the morsel-parallel plan, which the small fixture never
// selects: on this package's 120 000-node benchGraph, `RETURN p.salary` plans
// Project -> ParallelScanProject and drives the converted projection closure
// from several worker goroutines at once. That is the path where the change was
// worth the most (-24.73% allocs/op, -8.42% wall clock), and it is also the one
// that exercises the rowSchema concurrency contract: one carrier, written once
// at build time, read concurrently by every worker.
//
// The assertion here is the stack invariant only. The strict
// constant-in-rows check that [TestSchemaWalkIsNotPerRow] applies would be WRONG
// on this path: each morsel worker builds its own fused sub-plan, so the number
// of plan-time derivations tracks the MORSEL count, which grows with rows.
// Measured at 120 000 rows: 119 derivations for a one-item projection, 238 for a
// two-item one — roughly one per thousand rows, against the 120 000 the
// pre-change tree performed. The rate bound below is what separates those two
// regimes, and it is an absolute ratio fixed by morsel granularity, not a
// fraction of any measured baseline.
var parallelShapes = []struct {
	name    string
	query   string
	rows    int
	derives bool
}{
	{"parallel_salary", `MATCH (p:Person) RETURN p.salary`, nodeCount, true},
	// The null control of the #2645 A/B: same fixture, same row count, same
	// ParallelScanProject plan, but a whole-node projection never reaches the
	// converted general path. It derived zero walks before the change and zero
	// after, which is why it could prove the other shapes' movement was real.
	{"parallel_whole_node", `MATCH (p:Person) RETURN p`, nodeCount, false},
}

// maxWalksPerRow bounds how often a walk may legitimately be derived. A per-row
// derivation is exactly one per row; morsel-parallel plan building is about one
// per thousand. One per hundred sits between the two regimes by a factor of ten
// on either side, so it cannot be tripped by morsel-size tuning and cannot be
// satisfied by a regression to per-row.
const maxWalksPerRow = 0.01

func TestSchemaWalkIsNotPerRowUnderParallelScan(t *testing.T) {
	eng := cypher.NewEngine(benchGraph)
	for _, s := range parallelShapes {
		s := s
		t.Run(s.name, func(t *testing.T) {
			plan, err := eng.Explain(s.query, nil)
			if err != nil {
				t.Fatalf("Explain(%q): %v", s.query, err)
			}
			if !strings.Contains(plan, "ParallelScanProject") {
				t.Fatalf("%s no longer plans ParallelScanProject, so this test covers "+
					"nothing it claims to cover:\n%s", s.name, plan)
			}
			planTime, perRow, detail := walkDerivations(t, eng, s.query, s.rows)
			t.Logf("%-20s rows=%d  plan-time walks=%d (%.5f per row)",
				s.name, s.rows, planTime, float64(planTime)/float64(s.rows))
			if perRow != 0 {
				t.Errorf("%d objects beneath %s came from outside the plan builder:\n%s",
					perRow, frameSchemaWalk, detail)
			}
			if s.derives && planTime == 0 {
				t.Errorf("%s: no allocation beneath %s at all — this gate is blind, so its "+
					"verdict is worthless", s.name, frameSchemaWalk)
			}
			if got := float64(planTime) / float64(s.rows); got > maxWalksPerRow {
				t.Errorf("%s derived %d walks over %d rows (%.4f per row, limit %.2f). At that "+
					"rate the derivation is tracking rows, not plan builds.",
					s.name, planTime, s.rows, got, maxWalksPerRow)
			}
		})
	}
}
