package cypher

// profile_parity_test.go — the gates on the profiling instrument, over a DERIVED
// corpus rather than a hand-picked one (rmp #2665).
//
// Layer: short.
//
// # What went wrong, and why the old gates could not see it
//
// Installing a [exec.Profiler] must not change the plan. The builder wraps on the
// way OUT of its recursion, so a parent is constructed with its child ALREADY
// wrapped and runs its type assertions against the wrapper. Wrap preserves every
// INTERFACE the operator exposes, which keeps a CAPABILITY assertion
// (`child.(exec.ChunkProducer)`) answering the same either way. It cannot preserve
// a CONCRETE type: no wrapper is an *exec.Expand. A plan-shape recogniser that
// asserts on a concrete operator type therefore stopped recognising its own shape
// the moment a Profiler was installed.
//
// `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN b.salary` — 960000 rows under Run —
// then rendered a row-mode Filter/Expand under PROFILE where EXPLAIN rendered
// ColumnarFilter/columnarExpand, AND reported rows=0 at every level above the leaf,
// with no error. (The zero came from the second half of the defect: the recogniser
// declined AFTER buildOperator had written the schema, and the re-build stacked its
// columns on top — see [buildStateSnapshot].)
//
// TestProfile_PlanShapeIsIdenticalProfiledOrNot in explain_physical_test.go performs
// exactly the right comparison, over five HAND-PICKED shapes, none of which puts an
// Expand under a columnar projection. TestProfile_EveryOperatorIsMeasured cannot
// catch it either: rows=0 is still "measured". And nothing anywhere asserted that
// PROFILE and Run agree on HOW MANY ROWS the query returns, which is why rows=0 for
// a 960000-row query read as success.
//
// # What this file gates instead
//
// A corpus derived from the clause vocabulary rather than chosen by hand, run
// across four engine configurations, asserting three properties per case:
//
//  1. SHAPE — the tree Profile renders is identical to the one Explain renders,
//     node for node, once measurements are stripped;
//  2. CARDINALITY — the root operator's measured row count equals the number of
//     rows Run returns for the same query (the oracle whose absence let rows=0
//     pass); and
//  3. COMPLETENESS — no node renders "(not measured)".
//
// and one property over the corpus as a whole:
//
//  4. CLOSURE — every operator the exec package can render appears in at least one
//     corpus plan, or is named in [profileCorpusExcluded] with the reason it
//     cannot. The partition is exact and disjoint, so ADDING an operator to exec
//     fails this test until the corpus covers it or the exclusion is written down,
//     and an operator that was declared unreachable failing to be unreachable fails
//     it too.
//
// Property 4 is the structural half: it is what stops a NEW plan shape escaping
// the gate the way the columnar expand chain escaped the five hand-picked ones.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixture
// ─────────────────────────────────────────────────────────────────────────────

// profileCorpusNodes is above the hash join's size floor, so the disconnected
// equi-join shapes really do plan as joins rather than as products — a corpus that
// silently fell back would cover fewer operators while still passing.
const profileCorpusNodes = 420

// profileCorpusGraph builds the corpus fixture: labelled nodes carrying one
// property of each kind the access paths discriminate on, and a :K adjacency
// dense enough to close triangles (which the fused cyclic expand needs) without
// making any single query large.
func profileCorpusGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < profileCorpusNodes; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
		if i%3 == 0 {
			if err := g.SetNodeLabel(k, "Q"); err != nil {
				t.Fatalf("SetNodeLabel Q %s: %v", k, err)
			}
		}
		for key, v := range map[string]lpg.PropertyValue{
			"age":   lpg.Int64Value(int64(i % 40)),
			"name":  lpg.StringValue(fmt.Sprintf("nm%d", i)),
			"score": lpg.Float64Value(float64(i) / 3.0),
			"big":   lpg.Int64Value(int64(100_000 + i)),
			"bt":    lpg.Int64Value(int64(i)),
		} {
			if err := g.SetNodeProperty(k, key, v); err != nil {
				t.Fatalf("SetNodeProperty %s.%s: %v", k, key, err)
			}
		}
	}
	for i := 0; i < profileCorpusNodes; i++ {
		src := fmt.Sprintf("n%d", i)
		for d := 1; d <= 3; d++ {
			dst := fmt.Sprintf("n%d", (i+d*7)%profileCorpusNodes)
			if err := g.AddEdge(src, dst, float64(d)); err != nil {
				t.Fatalf("AddEdge %s->%s: %v", src, dst, err)
			}
			g.SetEdgeLabel(src, dst, "K")
		}
	}
	return g
}

// profileCorpusArm is one engine configuration the whole corpus runs against.
// The configurations exist to reach access paths and operators a single engine
// cannot: an index seek needs indexes, the fused cyclic expand is opt-in, and the
// morsel-parallel leaves are gated on a live-node threshold.
type profileCorpusArm struct {
	name string
	eng  *Engine
}

func profileCorpusArms(t *testing.T, g *lpg.Graph[string, float64]) []profileCorpusArm {
	t.Helper()
	indexed := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: 10})
	for _, ddl := range []string{
		"CREATE INDEX FOR (p:P) ON (p.name)",
		"CREATE INDEX FOR (p:P) ON (p.age)",
		"CREATE INDEX FOR (p:P) ON (p.score)",
		"CREATE INDEX idx_bt FOR (p:P) ON (p.bt) OPTIONS {indexType: 'btree'}",
	} {
		res, err := indexed.RunAny(context.Background(), ddl, nil)
		if err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("%s close: %v", ddl, cerr)
		}
	}
	return []profileCorpusArm{
		{"plain", NewEngine(g)},
		{"indexed+parallel", indexed},
		{"cyclic-intersect", NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true})},
		{"parallel", NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: 10})},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The corpus
// ─────────────────────────────────────────────────────────────────────────────

// profileCorpus is the derived query set: the clause vocabulary crossed with the
// pattern vocabulary, grouped by what each group is there to reach. It is a
// CORPUS, not a list of interesting cases — the closure gate below is what keeps
// it honest, by failing when an operator the engine can render appears in none of
// these plans.
//
// Every query must be valid against the fixture in EVERY arm: an arm-specific
// query would make the parity comparison depend on which engine ran it.
var profileCorpus = []string{
	// Scans and scalar projections — the columnar read chain and its row fallback.
	"MATCH (n:P) RETURN n.age",
	"MATCH (n) RETURN n.age",
	"MATCH (n:P) RETURN n",
	"MATCH (n:P) WHERE n.age > 10 RETURN n.name",
	"MATCH (n:P) WHERE n.age > 10 RETURN n",
	"MATCH (n:P:Q) RETURN n.age",
	"MATCH (n:P) RETURN n.age, n.name, n.big",

	// Aggregation, global and grouped.
	"MATCH (n:P) RETURN count(*)",
	"MATCH (n:P) RETURN count(n)",
	"MATCH (n) RETURN count(*)",
	"MATCH (n:P) RETURN n.age, count(*)",
	"MATCH (n:P) RETURN sum(n.age)",
	"MATCH (n:P) RETURN n.age, sum(n.big)",
	"MATCH (n:P) RETURN collect(n.age)",
	"MATCH (n) RETURN n.age, count(*)",
	"MATCH (n) RETURN n.age, max(n.big)",

	// Ordering, distinctness and paging.
	"MATCH (n:P) RETURN DISTINCT n.age",
	"MATCH (n:P) RETURN n.age ORDER BY n.age LIMIT 5",
	"MATCH (n:P) RETURN n.age ORDER BY n.age",
	"MATCH (n:P) RETURN n.age ORDER BY n.age DESC",
	"MATCH (n:P) RETURN n.age SKIP 3 LIMIT 4",
	"MATCH (n:P) RETURN n.age LIMIT 7",
	"MATCH (n:P) WITH n ORDER BY n.age RETURN n.age",
	"MATCH (n:P) WITH n.age AS a RETURN a ORDER BY a LIMIT 3",
	"MATCH (n:P) WITH n.age AS a, count(*) AS c RETURN a, c",

	// Traversals — this is the family the defect lived in.
	"MATCH (a:P)-[:K]->(b:P) RETURN b.age",
	"MATCH (a:P)-[:K]->(b:P) RETURN b.big",
	"MATCH (a:P)-[r:K]->(b:P) RETURN r",
	"MATCH (a:P)-[:K]->(b:P) WHERE b.age > 20 RETURN b.age",
	"MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P) RETURN c.age",
	"MATCH (a:P)-[:K]->(a) RETURN a.age",
	"MATCH (a:P)-[:K]-(b:P) RETURN b.age",
	"MATCH (a:P)-[:K*1..2]->(b:P) RETURN b.age",
	"MATCH p = (a:P)-[:K*1..2]->(b:P) RETURN length(p)",
	"MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b:P) RETURN a.age, b.age",
	"MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b) RETURN a.age, b.age",
	"MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b) RETURN a.age",

	// Cycles — the fused cyclic expand only admits an unlabelled direct stack.
	"MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*)",
	"MATCH (a)-[r1:K]->(b)-[r2:K]->(c)-[r3:K]->(a) RETURN a.age",

	// Disconnected equi-joins — hash join, its columnar tier, and the index
	// nested loop the cost gate awards the shape once an index exists.
	"MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)",
	"MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.age",
	"MATCH (a:P), (b:P) WHERE a.name = b.name RETURN count(*)",
	"MATCH (a:P), (b:P) RETURN count(*)",

	// Access paths: equality seek, key-set seek, range seek, prefix seek.
	"MATCH (n:P) WHERE n.name = 'nm7' RETURN n.age",
	"MATCH (n:P) WHERE n.name IN ['nm7','nm8'] RETURN n.age",
	"MATCH (n:P) WHERE n.name = 'nm7' OR n.name = 'nm8' RETURN n.age",
	"MATCH (n:P) WHERE n.name STARTS WITH 'nm1' RETURN n.age",
	"MATCH (n:P) WHERE n.name >= 'nm2' AND n.name < 'nm3' RETURN n.age",
	"MATCH (n:P) WHERE n.score > 5.5 RETURN n.age",
	"MATCH (n:P) WHERE n.bt = 5 RETURN n.age",
	"MATCH (n:P) WHERE n.bt > 100 AND n.bt < 200 RETURN n.age",
	"MATCH (n:P) WHERE n.bt >= 100 RETURN n.age",
	"UNWIND ['nm7','nm8'] AS k MATCH (n:P) WHERE n.name = k RETURN n.age",

	// Subqueries and pattern predicates — the apply family.
	"MATCH (a:P) WHERE exists { MATCH (a)-[:K]->(b:P) } RETURN a.age",
	"MATCH (a:P) WHERE NOT exists { MATCH (a)-[:K]->(b:P) } RETURN a.age",
	"MATCH (a:P) WHERE (a)-[:K]->(:P) RETURN a.age",
	"MATCH (a:P) RETURN a.age, count { MATCH (a)-[:K]->(b:P) } AS c",
	"MATCH (a:P) RETURN [(a)-[:K]->(b:P) | b.age] AS l",

	// Shortest path.
	"MATCH (a:P {name:'nm0'}), (b:P {name:'nm7'}), p = shortestPath((a)-[:K*1..5]->(b)) RETURN length(p)",
	"MATCH (a:P {name:'nm0'}), (b:P {name:'nm7'}), p = allShortestPaths((a)-[:K*1..5]->(b)) RETURN length(p)",

	// Row sources that are not scans.
	"UNWIND [1,2,3] AS x RETURN x",
	"RETURN 1 AS x",
	"CALL db.labels() YIELD label RETURN label",

	// Set operations — the plan ROOT here is built above buildOperator's
	// recursion, which is how it stayed unmeasured until this gate was written.
	"MATCH (n:P) RETURN n.age UNION MATCH (m:Q) RETURN m.age",
	"MATCH (n:P) RETURN n.age UNION ALL MATCH (m:Q) RETURN m.age",
}

// ─────────────────────────────────────────────────────────────────────────────
// The gates
// ─────────────────────────────────────────────────────────────────────────────

// profileRootRows extracts the root operator's measured row count from a rendered
// profile. The root is the first line by construction of [exec.RenderPlanNode].
var profileRootRows = regexp.MustCompile(`^[^\n]*\(rows=(\d+),`)

// TestProfile_DerivedCorpusShapeCardinalityAndCompleteness is the widened
// transparency gate: properties 1, 2 and 3 from the file comment, over the whole
// derived corpus in every engine configuration.
func TestProfile_DerivedCorpusShapeCardinalityAndCompleteness(t *testing.T) {
	t.Parallel()
	g := profileCorpusGraph(t)
	for _, arm := range profileCorpusArms(t, g) {
		t.Run(arm.name, func(t *testing.T) {
			for _, q := range profileCorpus {
				t.Run(q, func(t *testing.T) {
					explained := stripAnnotations(explainOK(t, arm.eng, q))
					profiled, err := arm.eng.Profile(context.Background(), q, nil)
					if err != nil {
						t.Fatalf("Profile(%q): %v", q, err)
					}

					// 1. Shape.
					if got := stripAnnotations(profiled); got != explained {
						t.Errorf("profiling changed the plan.\n--- Explain ---\n%s\n--- Profile ---\n%s\n"+
							"An assertion the wrapper does not satisfy makes the parent build a "+
							"different operator, so every measurement describes a plan that never "+
							"runs (rmp #2665).", explained, got)
					}

					// 2. Cardinality. Without this a plan that reported rows=0 for a
					// query returning 960000 rows read as a success.
					m := profileRootRows.FindStringSubmatch(profiled)
					if m == nil {
						t.Fatalf("the profile root carries no row count:\n%s", profiled)
					}
					rootRows, cerr := strconv.Atoi(m[1])
					if cerr != nil {
						t.Fatalf("unparsable root row count %q: %v", m[1], cerr)
					}
					if ran := len(runRows(t, arm.eng, q)); rootRows != ran {
						t.Errorf("PROFILE reports %d rows at the plan root; Run returns %d.\n%s",
							rootRows, ran, profiled)
					}

					// 3. Completeness.
					if strings.Contains(profiled, "(not measured)") {
						t.Errorf("a profiled plan contains an unmeasured operator:\n%s\n"+
							"A composite or above-the-recursion lowering has to pass its "+
							"operator through profileIntermediate.", profiled)
					}
				})
			}
		})
	}
}

// profileCorpusExcluded names every operator the exec package can render that the
// corpus above deliberately does NOT reach, with the reason. Membership is
// asserted to be exact and disjoint against what the corpus renders, so this list
// cannot rot silently in either direction.
var profileCorpusExcluded = map[string]string{
	// PROFILE refuses a writing statement — executing a write as the side effect of
	// a diagnostic is not something a user asks for — so no write operator can ever
	// appear in a profiled plan.
	"CreateNode":         "write operator; Engine.Profile refuses writing statements",
	"CreateRelationship": "write operator; Engine.Profile refuses writing statements",
	"DeleteNode":         "write operator; Engine.Profile refuses writing statements",
	"DeleteRelationship": "write operator; Engine.Profile refuses writing statements",
	"DetachDelete":       "write operator; Engine.Profile refuses writing statements",
	"Foreach":            "write operator; Engine.Profile refuses writing statements",
	"Merge":              "write operator; Engine.Profile refuses writing statements",
	"MergePattern":       "write operator; Engine.Profile refuses writing statements",
	"MergeRelationship":  "write operator; Engine.Profile refuses writing statements",
	"RemoveLabels":       "write operator; Engine.Profile refuses writing statements",
	"RemoveProperty":     "write operator; Engine.Profile refuses writing statements",
	"SetAllProperties":   "write operator; Engine.Profile refuses writing statements",
	"SetLabels":          "write operator; Engine.Profile refuses writing statements",
	"SetProperty":        "write operator; Engine.Profile refuses writing statements",
	"Eager":              "built only by the write planner (cypher/ir/writes.go) to break a read-write dependency",

	// DDL is planned and executed outside the physical read plan: Engine.Profile
	// answers "DDL has no query plan" for these statements.
	"CreateConstraintOp": "DDL; Engine.Profile answers 'DDL has no query plan'",
	"CreateIndexOp":      "DDL; Engine.Profile answers 'DDL has no query plan'",
	"DropConstraintOp":   "DDL; Engine.Profile answers 'DDL has no query plan'",
	"DropIndexOp":        "DDL; Engine.Profile answers 'DDL has no query plan'",
	"StaticRows":         "built only by the SHOW surface (cypher/show.go), which drives exec.Run directly and has no plan tree",

	// Not plan nodes.
	"ResultSet":        "the result driver, not an operator inside a plan tree",
	"profiledOp":       "the profiling wrapper itself; PlanTree renders the operator it measures",
	"profiledChunkOp":  "profiling wrapper variant for a ChunkProducer; PlanTree renders the operator it measures",
	"profiledNodeIDOp": "profiling wrapper variant for a NodeIDColumnProducer; PlanTree renders the operator it measures",

	// Reachable in principle, unreached in practice — recorded rather than hidden.
	"OptionalExpand": "no OPTIONAL MATCH form reached it: every shape tried lowers to OptionalApply over a plain Expand (rmp #2665 report). Candidate dead operator; if it becomes reachable this gate fails and the corpus must cover it",
	"singleRow":      "the one-row source exec.OptionalExpand builds internally; unreachable for the same reason",
}

// TestProfile_DerivedCorpusCoversEveryRenderableOperator is property 4: the
// closure gate.
//
// [exec.PlanTree] names a node after its operator's CONCRETE Go type, so the set of
// names a plan can carry is exactly the set of operator structs in the exec
// package. The obligation is derived from that source rather than from a
// hand-maintained list, so adding an operator fails this test until the corpus
// covers it or the exclusion is written down with a reason.
//
// This is the half that matters. The one-shape repair fixes one recogniser; this
// is what stops the NEXT plan shape from escaping the parity gate the way the
// columnar expand chain escaped five hand-picked queries.
func TestProfile_DerivedCorpusCoversEveryRenderableOperator(t *testing.T) {
	t.Parallel()

	renderable := execOperatorStructNames(t)
	if len(renderable) < 40 {
		t.Fatalf("found only %d operator structs in cypher/exec; the detection heuristic has broken", len(renderable))
	}

	g := profileCorpusGraph(t)
	covered := map[string]bool{}
	for _, arm := range profileCorpusArms(t, g) {
		for _, q := range profileCorpus {
			for _, name := range planOperatorNames(explainOK(t, arm.eng, q)) {
				covered[name] = true
			}
		}
	}

	var uncovered, wronglyExcluded []string
	for name := range renderable {
		_, excluded := profileCorpusExcluded[name]
		switch {
		case covered[name] && excluded:
			wronglyExcluded = append(wronglyExcluded, name)
		case !covered[name] && !excluded:
			uncovered = append(uncovered, name)
		}
	}
	sort.Strings(uncovered)
	sort.Strings(wronglyExcluded)

	if len(uncovered) > 0 {
		t.Errorf("these operators can be rendered in a plan but no corpus query reaches them: %v\n"+
			"Add a query that plans to each, or record it in profileCorpusExcluded with the reason "+
			"it cannot appear in a profiled read plan. An operator covered by neither is one whose "+
			"behaviour under PROFILE nothing checks — which is exactly how rmp #2665 survived.",
			uncovered)
	}
	if len(wronglyExcluded) > 0 {
		t.Errorf("these operators are listed in profileCorpusExcluded as unreachable, but the corpus "+
			"renders them: %v\nThe exclusion is stale; remove it so the parity gate covers the "+
			"operator properly.", wronglyExcluded)
	}

	// The exclusion list must not name operators that do not exist either, or a
	// renamed operator would silently lose its exclusion AND its coverage.
	var unknown []string
	for name := range profileCorpusExcluded {
		if !renderable[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("profileCorpusExcluded names operators that no longer exist in cypher/exec: %v", unknown)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The pinned regressions
// ─────────────────────────────────────────────────────────────────────────────

// TestProfile_ExpandUnderColumnarProjectionSurvivesProfiling pins rmp #2665
// itself, at the smallest fixture that reproduces it, so the defect is named in
// one place rather than only implied by a corpus sweep.
//
// It fails on the unfixed sources in both halves: the plan renders
// Filter/Expand instead of ColumnarFilter/columnarExpand, and the root reports
// rows=0 for a query that returns 2400 rows.
func TestProfile_ExpandUnderColumnarProjectionSurvivesProfiling(t *testing.T) {
	t.Parallel()
	eng := NewEngine(profileCorpusGraph(t))
	const q = "MATCH (a:P)-[:K]->(b:P) RETURN b.big"

	ran := len(runRows(t, eng, q))
	if ran == 0 {
		t.Fatalf("the fixture produces no rows for %q, so this test cannot fail", q)
	}
	explained := explainOK(t, eng, q)
	if !strings.Contains(explained, "columnarExpand") || !strings.Contains(explained, "ColumnarFilter") {
		t.Fatalf("the fixture no longer plans this shape column-major, so the case no longer "+
			"covers what it was written for:\n%s", explained)
	}

	profiled, err := eng.Profile(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !strings.Contains(profiled, "columnarExpand") || !strings.Contains(profiled, "ColumnarFilter") {
		t.Errorf("installing the Profiler collapsed the columnar chain to row mode:\n"+
			"--- Explain ---\n%s\n--- Profile ---\n%s", explained, profiled)
	}
	if got := stripAnnotations(profiled); got != stripAnnotations(explained) {
		t.Errorf("PROFILE renders a different plan from EXPLAIN:\n--- Explain ---\n%s\n--- Profile ---\n%s",
			stripAnnotations(explained), got)
	}
	m := profileRootRows.FindStringSubmatch(profiled)
	if m == nil {
		t.Fatalf("the profile root carries no row count:\n%s", profiled)
	}
	if m[1] != strconv.Itoa(ran) {
		t.Errorf("PROFILE reports rows=%s at the plan root for a query that returns %d rows:\n%s",
			m[1], ran, profiled)
	}
	if strings.Contains(profiled, "(not measured)") {
		t.Errorf("the substituted columnar operator is rendered but not measured:\n%s", profiled)
	}
}

// TestProfile_CyclicIntersectSurvivesProfiling pins the second instance of the
// same defect class found while fixing #2665: [tryFuseCyclicIntersect] asserted on
// a concrete *exec.Expand too, so installing a Profiler vetoed the fusion for
// every shape and PROFILE rendered three Expands where EXPLAIN rendered the
// ExpandIntersect that actually runs.
//
// Unlike the columnar chain this one declines WITHOUT having mutated the builder
// state, so it produced the right rows all along — only the plan was a fiction.
func TestProfile_CyclicIntersectSurvivesProfiling(t *testing.T) {
	t.Parallel()
	eng := NewEngineWithOptions(profileCorpusGraph(t), EngineOptions{EnableCyclicIntersect: true})
	const q = "MATCH (a)-[r1:K]->(b)-[r2:K]->(c)-[r3:K]->(a) RETURN a.age"

	explained := explainOK(t, eng, q)
	if !strings.Contains(explained, "ExpandIntersect") {
		t.Fatalf("the fixture no longer fuses this cycle, so the case no longer covers what it "+
			"was written for:\n%s", explained)
	}
	profiled, err := eng.Profile(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !strings.Contains(profiled, "ExpandIntersect") {
		t.Errorf("installing the Profiler vetoed the cyclic fusion:\n--- Explain ---\n%s\n"+
			"--- Profile ---\n%s", explained, profiled)
	}
	if got := stripAnnotations(profiled); got != stripAnnotations(explained) {
		t.Errorf("PROFILE renders a different plan from EXPLAIN:\n--- Explain ---\n%s\n--- Profile ---\n%s",
			stripAnnotations(explained), got)
	}
}

// TestColumnarChain_PostBuildDeclineRestoresBuilderState pins the SECOND half of
// #2665: what happens when a recogniser declines after it has already built.
//
// Both columnar recognisers run every check they can before building, but each
// keeps a defensive fall-through for an invariant that does not hold. Taking it
// used to leave the schema columns the build had recorded in place; the caller
// then re-built the same subtree on top of them, every variable's column landed
// past the end of the emitted row, [exec.Expand] skipped every row silently, and
// the query returned ZERO rows with no error.
//
// The branch is unreachable with the concrete-type assertion fixed — a counter on
// it recorded zero hits across the whole cypher test corpus, TCK included — so
// forceColumnarChainDeclineForTest exists to drive it. With the seam set the query
// must return exactly what the columnar chain returns, since declining is defined
// to fall back to the byte-identical serial build.
func TestColumnarChain_PostBuildDeclineRestoresBuilderState(t *testing.T) {
	t.Parallel()
	g := profileCorpusGraph(t)

	// Shapes that reach a post-build decline: one per recogniser, plus the stacked
	// -Selection fusion, plus a projection of several properties.
	queries := []string{
		"MATCH (n:P) WHERE n.age > 10 RETURN n.name",
		"MATCH (a:P)-[:K]->(b:P) RETURN b.big",
		"MATCH (a:P)-[:K]->(b:P) WHERE b.age > 20 RETURN b.age",
		"MATCH (a:P)-[:K]->(b:P) WHERE b.age > 20 RETURN b.age, b.name, b.big",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			columnar := NewEngine(g)
			wantPlan := explainOK(t, columnar, q)
			want := runRows(t, columnar, q)
			if len(want) == 0 {
				t.Fatalf("%q returns no rows, so a zero-row regression would be invisible here", q)
			}
			if !strings.Contains(wantPlan, "Columnar") {
				t.Fatalf("%q does not plan column-major, so it does not exercise a recogniser:\n%s", q, wantPlan)
			}

			declined := NewEngine(g)
			declined.forceColumnarChainDeclineForTest = true
			gotPlan := explainOK(t, declined, q)
			if strings.Contains(gotPlan, "columnarExpand") || strings.Contains(gotPlan, "ColumnarFilter") {
				t.Fatalf("the decline seam did not take effect; the plan is still the columnar "+
					"chain:\n%s", gotPlan)
			}
			got := runRows(t, declined, q)

			if len(got) != len(want) {
				t.Fatalf("declining after the build changed the result cardinality: %d rows, want %d.\n"+
					"--- columnar plan ---\n%s\n--- declined plan ---\n%s\n"+
					"Zero here is the rmp #2665 signature: the re-build stacked its columns on the "+
					"ones the declined build left in the schema.", len(got), len(want), wantPlan, gotPlan)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d differs after a post-build decline: %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// planOperatorNames returns the operator names a rendered plan carries, stripped
// of tree drawing, [detail] and (measurement) suffixes.
func planOperatorNames(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		name := strings.TrimLeft(line, " │├└─")
		if i := strings.Index(name, " ["); i >= 0 {
			name = name[:i]
		}
		if i := strings.Index(name, " ("); i >= 0 {
			name = name[:i]
		}
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// execOperatorStructNames returns the set of struct type names in cypher/exec that
// are operators, using the same heuristic
// TestPlanChildren_EveryOperatorWithInputsImplementsIt uses: a struct with a Next
// method, own or promoted through embedding. That is precisely the set
// [exec.PlanTree] can name, since it reads a node's name from the concrete type.
func execOperatorStructNames(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir("exec")
	if err != nil {
		t.Fatalf("read cypher/exec: %v", err)
	}
	fset := token.NewFileSet()
	structs := map[string]bool{}
	embeds := map[string][]string{}
	methods := map[string]map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, "exec/"+name, nil, 0)
		if perr != nil {
			t.Fatalf("parse exec/%s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, isType := spec.(*ast.TypeSpec)
					if !isType {
						continue
					}
					st, isStruct := ts.Type.(*ast.StructType)
					if !isStruct {
						continue
					}
					structs[ts.Name.Name] = true
					if st.Fields == nil {
						continue
					}
					for _, fld := range st.Fields.List {
						if len(fld.Names) == 0 {
							if base := astBaseTypeName(fld.Type); base != "" {
								embeds[ts.Name.Name] = append(embeds[ts.Name.Name], base)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					continue
				}
				recv := astBaseTypeName(d.Recv.List[0].Type)
				if recv == "" {
					continue
				}
				if methods[recv] == nil {
					methods[recv] = map[string]bool{}
				}
				methods[recv][d.Name.Name] = true
			}
		}
	}

	var hasNext func(name string, seen map[string]bool) bool
	hasNext = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		if methods[name]["Next"] {
			return true
		}
		for _, e := range embeds[name] {
			if hasNext(e, seen) {
				return true
			}
		}
		return false
	}

	operators := map[string]bool{}
	for name := range structs {
		if hasNext(name, map[string]bool{}) {
			operators[name] = true
		}
	}
	return operators
}

// astBaseTypeName reduces a type expression to its bare identifier, seeing through
// pointers, slices and qualified names.
func astBaseTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return astBaseTypeName(t.X)
	case *ast.ArrayType:
		return astBaseTypeName(t.Elt)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return astBaseTypeName(t.X)
	}
	return ""
}
