package cypher

// subquery_adjacency_rebuild_diff_test.go — the STRUCTURAL falsification of the
// claim that a correlated subquery rebuilds the whole adjacency once per outer
// row (rmp #2646).
//
// # Why a counter and not a stopwatch
//
// The claim is about a COMPLEXITY CLASS, and a stopwatch cannot settle one. A
// timing ratio between two shapes conflates a large constant factor with a worse
// exponent, it moves with the host's load, and — worst of all — a "fraction of a
// fixed workload inside a fixed window" is a RATE dressed up as a structural fact.
//
// [csrPairUncachedBuildCount] and [slotTypeResolveCount] admit no such
// confusion. They count O(V+E) constructions. Bracket one query drive between two
// reads of them and the delta is an ABSOLUTE COUNT of full adjacency rebuilds that
// drive performed. Run the same query shape over a graph of 250 nodes and one of
// 500 and the answer is unambiguous:
//
//   - a delta that roughly DOUBLES with the node count is O(rows) — the rebuild is
//     per outer row, and the shape is quadratic in graph size;
//   - a delta that does not move is O(1) — the rebuild is amortised, and any
//     remaining cost lives somewhere else.
//
// # The controls come first
//
// A counter oracle that would also pass on a build where the instrumented code
// never ran proves nothing, so TestAdjacencyRebuildCounters_Controls establishes
// that these two counters actually observe the thing they name BEFORE any verdict
// is read off them: a cold Engine's first one-hop query must show a build (the
// counter fires at all), and the identical repeat on the same warm Engine must show
// none (the counter observes the cache, rather than merely incrementing on every
// call). Only with both halves green does a zero delta mean "amortised" and a
// non-zero delta mean "rebuilt".

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// buildRebuildFixture builds an n-node graph with exactly one outgoing :R
// relationship per node, so out-degree is pinned at 1 for EVERY n. The work an
// amortised implementation owes per outer row is therefore CONSTANT and the total
// is O(n): every super-linear growth the sweep sees is the implementation's, never
// the workload's. This is the same fixture shape as bench/audit352's
// buildRelGraphN, restated in-package because the counters are unexported.
func buildRebuildFixture(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := eng.RunAny(ctx, fmt.Sprintf(`CREATE (:P {sid:%d})`, 100000+i), nil); err != nil {
			tb.Fatalf("fixture node %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		q := fmt.Sprintf(`MATCH (a:P {sid:%d}), (b:P {sid:%d}) CREATE (a)-[:R]->(b)`,
			100000+i, 100000+((i+7)%n))
		if _, err := eng.RunAny(ctx, q, nil); err != nil {
			tb.Fatalf("fixture rel %d: %v", i, err)
		}
	}
	return g
}

// rebuildDelta is one bracketed observation: the rows a drive shipped and the
// number of full O(V+E) adjacency rebuilds it performed while doing so.
type rebuildDelta struct {
	rows   int
	pairs  uint64
	filter uint64
	// absent is the subset of pairs whose cause was that no cache existed to
	// consult, as opposed to one consulted and missed.
	absent uint64
	// degree and hop count the two adjacency-level rewrites that answer a
	// subquery WITHOUT compiling an inner pipeline at all. They are recorded
	// because they explain the mechanism behind a zero rebuild count: a shape can
	// show zero because its inner plan is amortised, or because it never built an
	// inner plan. Those are different facts and a fix must not confuse them.
	degree uint64
	hop    uint64
}

func (d rebuildDelta) String() string {
	return fmt.Sprintf("rows=%d csr_pair_builds=%d (absent_cache=%d) slot_type_resolutions=%d "+
		"degree_rewrites=%d labelled_hop_rewrites=%d",
		d.rows, d.pairs, d.absent, d.filter, d.degree, d.hop)
}

// driveCounted runs q to completion on e and returns the rows it shipped together
// with the rebuild counts attributable to that drive.
//
// The counters are read IMMEDIATELY before and IMMEDIATELY after the exercised
// window, so nothing outside it — fixture construction, a neighbouring test, the
// warm-up drive — can be silently folded into the observation.
func driveCounted(tb testing.TB, e *Engine, q string) rebuildDelta {
	tb.Helper()
	pairBefore := csrPairUncachedBuildCount.Load()
	filterBefore := slotTypeResolveCount.Load()
	absentBefore := csrPairAbsentCacheBuildCount.Load()
	degreeBefore := degreeRewriteCount.Load()
	hopBefore := labelledHopRewriteCount.Load()

	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", q, err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close %q: %v", q, err)
	}

	return rebuildDelta{
		rows:   rows,
		pairs:  csrPairUncachedBuildCount.Load() - pairBefore,
		filter: slotTypeResolveCount.Load() - filterBefore,
		absent: csrPairAbsentCacheBuildCount.Load() - absentBefore,
		degree: degreeRewriteCount.Load() - degreeBefore,
		hop:    labelledHopRewriteCount.Load() - hopBefore,
	}
}

// TestAdjacencyRebuildCounters_Controls validates both counters against cases
// whose answer is known independently of the claim under test, so that a reading
// taken from them afterwards carries meaning.
//
// Positive control — a COLD Engine has an empty [csrPairCache] and an empty
// the per-type-set filter LRU it replaced, so its first `-[:R]->` traversal MUST construct one of
// each. A counter that stayed at zero here would be inert, and every subsequent
// zero would be an artefact rather than a finding.
//
// Negative control — the identical query on the same now-WARM Engine reads an
// unchanged graph at an unchanged instant, so both structures must be served from
// cache and NEITHER counter may move. A counter that incremented here would be
// counting calls rather than builds, and every non-zero delta would be noise.
func TestAdjacencyRebuildCounters_Controls(t *testing.T) {
	const n = 60
	g := buildRebuildFixture(t, n)
	e := NewEngine(g)
	const oneHop = `MATCH (a:P)-[:R]->(b:P) RETURN a.sid, b.sid`

	cold := driveCounted(t, e, oneHop)
	if cold.rows != n {
		t.Fatalf("cold one-hop shipped %d rows, want %d", cold.rows, n)
	}
	if cold.pairs == 0 {
		t.Errorf("positive control FAILED: cold one-hop built no CSR pair (%s); "+
			"csrPairUncachedBuildCount is inert and no zero reading from it is evidence", cold)
	}
	if cold.filter == 0 {
		t.Errorf("positive control FAILED: cold one-hop resolved no slot types (%s); "+
			"slotTypeResolveCount is inert and no zero reading from it is evidence", cold)
	}
	t.Logf("positive control (cold Engine, one-hop): %s", cold)

	warm := driveCounted(t, e, oneHop)
	if warm.rows != n {
		t.Fatalf("warm one-hop shipped %d rows, want %d", warm.rows, n)
	}
	if warm.pairs != 0 || warm.filter != 0 {
		t.Errorf("negative control FAILED: warm repeat still rebuilt (%s); "+
			"the counters are counting calls, not builds", warm)
	}
	t.Logf("negative control (warm Engine, same query): %s", warm)
}

// TestSubqueryAdjacencyRebuild_ScalesWithGraph is the verdict. For each subquery
// shape it drives the query once to warm the Engine's caches and the plan cache,
// then takes ONE bracketed observation at n=250 and one at n=500.
//
// The assertion is a STRUCTURAL FLOOR, not a proportion: an amortised shape's
// rebuild count is bounded by a small constant that cannot grow with n, so the
// test fails a shape whose count at n=500 exceeds that constant. It is deliberately
// not written as "n=500 must be under 2x n=250" — that form passes a shape which
// rebuilds 3n times at both sizes.
func TestSubqueryAdjacencyRebuild_ScalesWithGraph(t *testing.T) {
	// The ceiling an amortised shape must respect. A drive may legitimately need a
	// couple of cold builds (its own first pair, its own first type resolution); it
	// may never need one PER ROW.
	const amortisedCeiling = 8

	// unrewritten selects the arm a shape is observed on. rmp #2648 normalises a
	// single-MATCH BLOCK-form subquery into the pattern the adjacency recognisers
	// see, so `COUNT { MATCH (a)-[:R]->(:P) }` is now answered by one adjacency
	// walk and builds no inner pipeline AT ALL. That is a different fact from
	// #2646's "the inner pipeline's rebuild is amortised", and [rebuildDelta]'s
	// own godoc warns that the two must not be confused — both produce a zero.
	//
	// So the shape is observed TWICE: once on a default engine, where the claim is
	// that no inner plan exists, and once on an engine built with
	// [EngineOptions.DisableAdjacencyCountRewrites], which is where #2646's claim
	// still lives and is still measured. Dropping the second arm would have
	// silently deleted #2646's structural verdict while leaving this test green.
	shapes := []struct {
		name        string
		query       string
		unrewritten bool
	}{
		{name: "plain_match", query: `MATCH (a:P) RETURN a.sid`},
		{name: "optional_match", query: `MATCH (a:P) OPTIONAL MATCH (a)-[:R]->(b:P) RETURN a.sid, b.sid`},
		{name: "pattern_predicate", query: `MATCH (a:P) WHERE (a)-[:R]->(:P) RETURN a.sid`},
		{name: "exists_subquery", query: `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:R]->(:P) } RETURN a.sid`},
		{name: "count_subquery", query: `MATCH (a:P) RETURN a.sid AS sid, COUNT { MATCH (a)-[:R]->(:P) } AS c`},
		{
			name:        "count_subquery_unrewritten",
			query:       `MATCH (a:P) RETURN a.sid AS sid, COUNT { MATCH (a)-[:R]->(:P) } AS c`,
			unrewritten: true,
		},
	}
	sizes := []int{250, 500}

	type key struct {
		shape string
		n     int
	}
	obs := map[key]rebuildDelta{}

	for _, n := range sizes {
		g := buildRebuildFixture(t, n)
		e := NewEngine(g)
		// The same graph, read by an engine that may not answer a count from the
		// adjacency. Both arms therefore differ in exactly one variable.
		eOff := NewEngineWithOptions(g, EngineOptions{DisableAdjacencyCountRewrites: true})
		for _, s := range shapes {
			arm := e
			if s.unrewritten {
				arm = eOff
			}
			// Warm-up drive: populates the Engine's shared caches and the plan
			// cache, so the bracketed observation that follows measures ONLY what
			// the shape rebuilds that no cache could have served.
			if w := driveCounted(t, arm, s.query); w.rows != n {
				t.Fatalf("%s n=%d warm-up shipped %d rows, want %d", s.name, n, w.rows, n)
			}
			d := driveCounted(t, arm, s.query)
			if d.rows != n {
				t.Fatalf("%s n=%d shipped %d rows, want %d", s.name, n, d.rows, n)
			}
			obs[key{s.name, n}] = d
		}
	}

	t.Logf("%-20s %-40s | %s", "shape", "n=250", "n=500")
	for _, s := range shapes {
		a, b := obs[key{s.name, 250}], obs[key{s.name, 500}]
		t.Logf("%-20s  csr=%-4d abs=%-4d typ=%-4d deg=%-4d hop=%-4d | csr=%-4d abs=%-4d typ=%-4d deg=%-4d hop=%-4d",
			s.name, a.pairs, a.absent, a.filter, a.degree, a.hop,
			b.pairs, b.absent, b.filter, b.degree, b.hop)
	}

	for _, s := range shapes {
		for _, n := range sizes {
			d := obs[key{s.name, n}]
			if d.pairs > amortisedCeiling || d.filter > amortisedCeiling {
				t.Errorf("%s n=%d rebuilds the whole adjacency per row: %s (ceiling %d)",
					s.name, n, d, amortisedCeiling)
			}
		}
	}

	// WHICH recogniser serves WHICH shape is pinned exactly, per arm, and it is
	// measured rather than assumed. An unexplained change here means a recogniser
	// moved, which is never licensed as a side effect of a performance change
	// however much faster it got — every entry below is a deliberate decision with
	// a task behind it.
	//
	//   - pattern_predicate — one labelled-hop walk per outer row (rmp #2235).
	//   - count_subquery — one labelled-hop walk per outer row SINCE rmp #2648,
	//     which normalises the block form into the pattern the recogniser sees. It
	//     read 0 before that, because ast.CountSubquery.Pattern is nil for this
	//     spelling and the recogniser rejects a nil pattern on its first line.
	//   - count_subquery_unrewritten — 0 by construction, on the arm that forbids
	//     both rewrites. This is where #2646's amortisation claim is still
	//     measured; the ceiling assertions above are what measure it.
	//   - exists_subquery — 0, and NOT because the recogniser refuses it: a
	//     WHERE-position EXISTS is planned as a SemiApply operator, so it never
	//     reaches the expression evaluator where either rewrite lives.
	//   - plain_match, optional_match — 0, no subquery.
	//
	// The degree rewrite fires for no shape here: every pattern in the table has a
	// LABEL on its far node, and a label is a Selection a degree cannot filter on.
	for _, s := range shapes {
		for _, n := range sizes {
			d := obs[key{s.name, n}]
			wantHop := uint64(0)
			if s.name == "pattern_predicate" || s.name == "count_subquery" {
				wantHop = uint64(n)
			}
			if d.degree != 0 {
				t.Errorf("%s n=%d: degreeRewriteCount moved to %d, want 0", s.name, n, d.degree)
			}
			if d.hop != wantHop {
				t.Errorf("%s n=%d: labelledHopRewriteCount = %d, want %d — a recogniser changed",
					s.name, n, d.hop, wantHop)
			}
		}
	}
}

// csrPairCacheRouteProbe records the CACHE-LOOKUP events of the adjacency cache
// and the BUILD/REUSE events of the relationship-type column stored inside it, so
// a rebuild counted by [csrPairUncachedBuildCount] can be attributed to the route
// that produced it rather than merely observed.
//
// Since rmp #2251 the column lives beside the pair, so a pair hit and a column
// reuse are one lookup. They are still counted separately, because a pair can hit
// while the column is absent — an untyped query warmed the pair — and only the
// column's own counter can say whether the O(V+E) resolution actually ran.
//
// NOT parallel-safe: it installs a global metrics backend.
type csrPairCacheRouteProbe struct {
	pairHits   atomic.Uint64
	pairMisses atomic.Uint64
	colReuses  atomic.Uint64
	colBuilds  atomic.Uint64
}

func (p *csrPairCacheRouteProbe) IncCounter(name string, delta uint64) {
	switch name {
	case "cypher.csr_pair_cache.hits":
		p.pairHits.Add(delta)
	case "cypher.csr_pair_cache.misses":
		p.pairMisses.Add(delta)
	case "cypher.reltype_column.reuses":
		p.colReuses.Add(delta)
	case "cypher.reltype_column.builds":
		p.colBuilds.Add(delta)
	}
}

func (p *csrPairCacheRouteProbe) ObserveLatency(string, time.Duration) {}
func (p *csrPairCacheRouteProbe) SetGauge(string, float64)             {}

func (p *csrPairCacheRouteProbe) String() string {
	return fmt.Sprintf("pair{hit=%d miss=%d} reltype_column{reuse=%d build=%d}",
		p.pairHits.Load(), p.pairMisses.Load(), p.colReuses.Load(), p.colBuilds.Load())
}

// setMetricsBackendForTest installs b as the process-wide metrics sink (nil
// restores the no-op default). It exists so a test that must bracket a drive it
// does not own can reach the same global [probedDrive] uses.
func setMetricsBackendForTest(b metrics.Backend) { metrics.SetBackend(b) }

// probedDrive runs q on e with a fresh metrics backend installed, returning both
// the rebuild delta and the cache-lookup events that accompanied it.
func probedDrive(t *testing.T, e *Engine, q string) (rebuildDelta, *csrPairCacheRouteProbe) {
	t.Helper()
	p := &csrPairCacheRouteProbe{}
	metrics.SetBackend(p)
	defer metrics.SetBackend(nil)
	return driveCounted(t, e, q), p
}

// TestSubqueryAdjacencyRebuild_Attribution pins the ROUTE the correlated COUNT
// subquery takes through [csrPairCachedAt], which the rebuild counters alone
// cannot say.
//
// That function reaches the O(V+E) build by exactly four routes: a nil cache, a
// nil read view, a write-transaction view that must not share
// ([viewCarriesOwnWrites]), and a genuine cache MISS. Only the last consults the
// cache, and only the last therefore emits a "cypher.csr_pair_cache.misses" event.
// The lookup events consequently DISCRIMINATE between "went looking and found
// nothing" and "never had a cache to look in" — a distinction no timing and no
// profile can supply, and the whole of the root cause when a rebuild turns out to
// be unamortised.
//
// # What this asserted before rmp #2646, and what it asserts now
//
// It was written to FALSIFY the claim that the subquery rebuilt per row. It found
// the claim true and the cause exact: 100 rebuilds, 100 of them on the
// absent-cache route, with zero hits and zero misses, because
// [buildOpts.forSubquery] carried neither cache into the inner build.
//
// The fix put the cache on that allowlist, so the assertion is now the inverse
// and is a strictly stronger guard: the subject arm must CONSULT the cache and be
// SERVED from it — one hit per outer row, no miss, no build. A regression dropping
// the field from the allowlist shows builds returning and hits collapsing to
// zero; a regression breaking invalidation instead shows misses rather than hits.
// The two failure modes stay distinguishable, which is the point of reading lookup
// events rather than time.
//
// The reference arm is what makes any of these readings meaningful. A cold
// top-level one-hop takes the miss route by construction, so it must show exactly
// one build AND one miss — proving the probe can still see a miss at all, before
// its silence anywhere else is read as evidence of anything.
//
// # Why the subject arm forbids the adjacency rewrites (rmp #2648)
//
// The paragraph above says asserting the HITS is "what makes this fail on a
// future change that stops the inner Expand from running at all instead of
// passing on a vacuous zero". rmp #2648 is exactly that change, and it is
// licensed: it normalises a single-MATCH block form into the pattern the
// labelled-hop recogniser sees, so on a DEFAULT engine this query now performs no
// inner Expand, no cache lookup and no rebuild — it walks the anchor's adjacency
// once. This test duly went red, which is the gate working.
//
// What it attributes, though, is the route the INNER PIPELINE takes through
// [csrPairCachedAt], and that is still worth pinning: it is the path every
// subquery the recognisers refuse continues to take. The subject arm is therefore
// built with [EngineOptions.DisableAdjacencyCountRewrites], which is the same
// instrument rmp #2647 introduced for the differential oracles — the arm is on
// the inner path because the engine was TOLD to be, not because no recogniser
// happens to accept the spelling. The default-engine reading is asserted too, at
// the end, so the two facts are both recorded here and cannot silently swap.
func TestSubqueryAdjacencyRebuild_Attribution(t *testing.T) {
	const n = 100
	const countSub = `MATCH (a:P) RETURN a.sid AS sid, COUNT { MATCH (a)-[:R]->(:P) } AS c`

	// Reference arm: a cold Engine, top-level one-hop. The cache exists and is
	// empty, so this is the MISS route.
	refDelta, refProbe := probedDrive(t, NewEngine(buildRebuildFixture(t, n)),
		`MATCH (a:P)-[:R]->(b:P) RETURN a.sid`)
	t.Logf("reference (cold top-level one-hop): %s  %s", refDelta, refProbe)
	if refDelta.pairs != 1 || refProbe.pairMisses.Load() != 1 {
		t.Fatalf("reference arm did not take the miss route: %s %s; "+
			"a silent miss counter in the subject arm below would prove nothing",
			refDelta, refProbe)
	}

	// Subject arm: the correlated COUNT subquery on a WARM Engine, so the outer
	// query's own adjacency needs are already cached and every build the drive
	// performs is attributable to the inner pipeline. The adjacency rewrites are
	// forbidden on this arm so that there IS an inner pipeline to attribute — see
	// the note on rmp #2648 above.
	g := buildRebuildFixture(t, n)
	e := NewEngineWithOptions(g, EngineOptions{DisableAdjacencyCountRewrites: true})
	if w := driveCounted(t, e, countSub); w.rows != n {
		t.Fatalf("warm-up shipped %d rows, want %d", w.rows, n)
	}
	subDelta, subProbe := probedDrive(t, e, countSub)
	t.Logf("subject (warm correlated COUNT subquery, rewrites forbidden): %s  %s", subDelta, subProbe)
	if subDelta.degree != 0 || subDelta.hop != 0 {
		t.Fatalf("the subject arm took an adjacency rewrite (%s), so there was no inner "+
			"pipeline to attribute and every reading below is vacuous — "+
			"EngineOptions.DisableAdjacencyCountRewrites did not reach the dispatch site", subDelta)
	}

	// No rebuild at all: the inner Expand's adjacency is served from the Engine's
	// caches on every one of the n outer rows.
	if subDelta.pairs != 0 || subDelta.filter != 0 {
		t.Errorf("subject arm still rebuilds: %s — want zero pair builds and zero slot-type resolutions", subDelta)
	}
	// And it was SERVED, not merely spared: one lookup per outer row, every one a
	// hit. Asserting the HITS, rather than only the absence of builds, is what makes
	// this fail on a future change that stops the inner Expand from running at all
	// instead of passing on a vacuous zero.
	if subProbe.pairHits.Load() != uint64(n) || subProbe.pairMisses.Load() != 0 {
		t.Errorf("CSR-pair cache not serving the inner Expand per row: %s, want hit=%d miss=0",
			subProbe, n)
	}
	if subProbe.colReuses.Load() != uint64(n) || subProbe.colBuilds.Load() != 0 {
		t.Errorf("relationship-type column not serving the inner Expand per row: %s, want reuse=%d build=0",
			subProbe, n)
	}
	// The absent-cache route must be gone entirely: that counter firing again is the
	// exact signature of the allowlist having regressed.
	if subDelta.absent != 0 {
		t.Errorf("subject arm took the absent-cache route %d times: buildOpts.forSubquery "+
			"is no longer carrying csrPairCache", subDelta.absent)
	}

	// The rmp #2648 arm, recorded here so this file states BOTH facts about the
	// same query and neither can be lost by a later edit to the other: on a
	// DEFAULT engine the same text builds no inner pipeline at all, so it makes no
	// cache lookup — it takes one adjacency walk per outer row instead.
	//
	// This is what the assertions above would read as "a vacuous zero", and the
	// reason they are not allowed to see it. A regression that made the
	// normalisation stop firing shows up here as hop=0 with the lookups returning.
	dOn := NewEngine(g)
	if w := driveCounted(t, dOn, countSub); w.rows != n {
		t.Fatalf("default-arm warm-up shipped %d rows, want %d", w.rows, n)
	}
	onDelta, onProbe := probedDrive(t, dOn, countSub)
	t.Logf("default arm (rewrites live): %s  %s", onDelta, onProbe)
	if onDelta.hop != uint64(n) {
		t.Errorf("the default arm answered the block form from the adjacency %d time(s), want %d "+
			"— rmp #2648's normalisation is not firing for `COUNT { MATCH … }`: %s", onDelta.hop, n, onDelta)
	}
	if onProbe.pairHits.Load() != 0 || onProbe.pairMisses.Load() != 0 || onDelta.pairs != 0 {
		t.Errorf("the default arm still consulted the CSR-pair cache (%s, %s); the adjacency "+
			"walk does not build an inner Expand, so there is nothing for it to look up", onDelta, onProbe)
	}
}

// TestSubqueryAdjacencyRebuild_WritePathStaysUncached is the CORRECTNESS half of
// rmp #2646, and it is the half that could have gone wrong.
//
// Sharing an adjacency cache across a subquery boundary is only sound while the
// cached pair describes a state the reader is entitled to see. Two independent
// guards keep that true, and this test exercises both at once by constructing the
// case that would break if either failed.
//
// # The oracle, and why it cannot pass vacuously
//
// The Engine's cache is deliberately WARMED at an epoch in which no :Z edge
// exists. A single statement then creates the :Z edges and, in a LATER CLAUSE of
// that same statement, asks EXISTS about them — a write landing BETWEEN the outer
// rows of one query, which is exactly the freshness case rmp #2317 exists for.
//
//   - served the stale pair, the answer is 0;
//   - resolved freshly, the answer is the node count.
//
// An oracle whose two outcomes are 0 and n cannot be satisfied by accident, which
// is what a counter assertion alone would risk here.
//
// # What actually protects it
//
// Not the cache key, as it turns out. MEASURED: the statement performs one
// uncached build per outer row and records ZERO cache lookups — neither hit nor
// miss — so the cache is never consulted on this path at all. It is protected by
// EXCLUSION, at two layers that would each suffice:
//
//   - [viewCarriesOwnWrites] is tested by [csrPairCachedAt] and by
//     [edgeTypeFilterFor] before either consults its cache, so a view that can see
//     its own uncommitted writes is refused by both (rmp #2446);
//   - the write path's own buildOpts never carried csrPairCache in the first
//     place, so [buildOpts.forSubquery] has nothing to hand down.
//
// The key-freshness argument is therefore the THIRD line of defence rather than
// the first, and this test is written to fail if the first two ever stop holding:
// the answer assertion catches a stale serve, and the lookup assertion catches the
// cache being consulted here at all.
func TestSubqueryAdjacencyRebuild_WritePathStaysUncached(t *testing.T) {
	const n = 4
	ctx := context.Background()
	e := NewEngine(buildRebuildFixture(t, n))

	// Warm the Engine's caches at the pre-:Z epoch.
	warm, err := e.Run(ctx, `MATCH (a:P)-[:R]->(b:P) RETURN a.sid`, nil)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	for warm.Next() {
	}
	if err := warm.Err(); err != nil {
		t.Fatalf("warm Err: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("warm Close: %v", err)
	}

	probe := &csrPairCacheRouteProbe{}
	pairBefore := csrPairUncachedBuildCount.Load()
	setMetricsBackendForTest(probe)
	res, err := e.RunAny(ctx,
		`MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN count(*) AS c`, nil)
	if err != nil {
		setMetricsBackendForTest(nil)
		t.Fatalf("write statement: %v", err)
	}
	var got []string
	for res.Next() {
		got = append(got, fmt.Sprint(res.Record()))
	}
	resErr := res.Err()
	closeErr := res.Close()
	setMetricsBackendForTest(nil)
	builds := csrPairUncachedBuildCount.Load() - pairBefore

	if resErr != nil {
		t.Fatalf("write statement Err: %v", resErr)
	}
	if closeErr != nil {
		t.Fatalf("write statement Close: %v", closeErr)
	}

	want := fmt.Sprintf("map[c:%d]", n)
	if len(got) != 1 || got[0] != want {
		t.Errorf("STALE ADJACENCY SERVED TO A WRITE STATEMENT: got %v, want [%s]; "+
			"the subquery did not observe the :Z edges its own statement created", got, want)
	}
	if probe.pairHits.Load() != 0 || probe.pairMisses.Load() != 0 || probe.colReuses.Load() != 0 {
		t.Errorf("a write statement CONSULTED a shared adjacency cache (%s); "+
			"viewCarriesOwnWrites must exclude it at every entry point (rmp #2446). "+
			"A column REUSE counts as a consultation exactly as a pair hit does: the "+
			"column is stored inside the cached pair, so being served one means having "+
			"been served the pair.", probe)
	}
	if builds == 0 {
		t.Errorf("write statement performed no adjacency build at all (%d); the oracle "+
			"is not exercising the path it claims to", builds)
	}
	t.Logf("write path: rows=%v builds=%d lookups=%s", got, builds, probe)
}

// BenchmarkAdjacencyCacheLookup measures what rmp #2646 COSTS, as opposed to what
// it saves. The fix does not make the per-outer-row work vanish: it replaces a
// Θ(V+E) rebuild with a cache lookup, and that lookup takes a mutex —
// [csrPairCache.mu]. It was TWO lookups and two mutexes until rmp #2251 stored the
// relationship-type column inside the cached pair; what remains is
// Engine-global and therefore shared by every concurrent query.
//
// The measured unit is exactly what [exec.Expand.Init] performs once per outer
// row: one call of the closure [expandAdjacencySource] returns, which is one
// [csrPairAndColumnCachedFor] — the pair and its relationship-type column in a
// single lookup. Nothing else is in the timing window, so the number is the per-row
// toll of the fix and not a query's worth of other work attributed to it.
//
// The serial arm gives the uncontended toll. The parallel arm is the one that could
// actually justify a contingent refinement (an unsynchronised single-entry memo on
// the child buildOpts), because a global mutex on a per-row path is a contention
// shape, not merely an instruction-count one — so it is measured rather than argued
// about.
//
// The benchmark asserts it is measuring HITS. A lookup that missed would be timing
// the rebuild this fix exists to remove, and would report a number several orders
// of magnitude too large while looking perfectly plausible.
func BenchmarkAdjacencyCacheLookup(b *testing.B) {
	const n = 2000
	g := buildRebuildFixture(b, n)

	// The caches are constructed HERE rather than borrowed from an Engine, and the
	// reason is a trap this benchmark walked into first: an Engine warmed through
	// e.Run holds a pair filed under a SNAPSHOT key (versioned, startTS > 0), while
	// a hand-built present read asks under {startTS: 0, versioned: false}. Those
	// keys never match, so every call missed — and [csrPairCache.put] then refused
	// to file the present-read pair too, because [csrPairKey.newerThan] orders by
	// startTS first and the resident snapshot entry always wins. The benchmark
	// would have timed a 2000-node rebuild and reported it as the cost of a mutex.
	// Owning the caches removes the interference entirely and measures exactly the
	// configuration [exec.Expand.Init] sees.
	bopts := &buildOpts{csrPairCache: newCSRPairCache()}
	view := g.ReadAt(nil)
	src := expandAdjacencySource(bopts, view, []string{"R"})

	// One warm call to populate both caches, then prove the steady state is a HIT
	// before timing it. The guard is not decoration: it is the only thing standing
	// between this benchmark and a plausible-looking number three orders of
	// magnitude too large.
	src()
	before := csrPairUncachedBuildCount.Load()
	if fwd, _, admit := src(); fwd == nil || !admit.Active() {
		b.Fatalf("adjacency source returned nil (fwd=%v admit=%v)", fwd == nil, !admit.Active())
	}
	if got := csrPairUncachedBuildCount.Load() - before; got != 0 {
		b.Fatalf("steady state is not a cache hit: %d rebuilds on the probe call", got)
	}

	b.Run("serial", func(b *testing.B) {
		guard := csrPairUncachedBuildCount.Load()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			fwd, _, admit := src()
			if fwd == nil || !admit.Active() {
				b.Fatal("nil adjacency")
			}
		}
		b.StopTimer()
		if got := csrPairUncachedBuildCount.Load() - guard; got != 0 {
			b.Fatalf("timed %d rebuilds, not hits — the measurement is void", got)
		}
	})

	// Contention: every goroutine takes the same Engine-global lock, which is the
	// shape a massively concurrent workload would present. It was TWO locks before
	// rmp #2251 stored the relationship-type column beside the pair.
	b.Run("parallel", func(b *testing.B) {
		guard := csrPairUncachedBuildCount.Load()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				fwd, _, admit := src()
				if fwd == nil || !admit.Active() {
					b.Fatal("nil adjacency")
				}
			}
		})
		b.StopTimer()
		if got := csrPairUncachedBuildCount.Load() - guard; got != 0 {
			b.Fatalf("timed %d rebuilds, not hits — the measurement is void", got)
		}
	})
}
