package cypher

// rel_stored_dir_test.go — the gate for rmp #2864: the stored orientation of a
// bound relationship is resolved at PLAN time for a directed hop, and the change
// is ANSWER-IDENTICAL to the per-row [relStoredInverted] ladder it replaces.
//
// A result-identical change is invisible to a differential test on its own, so
// three kinds of evidence are gathered here and none of them substitutes for
// another:
//
//  1. DIFFERENTIAL — every query runs with the plan-time path on and off over
//     the same binary ([relDirPlanDisabled]) and must agree row for row.
//  2. WHITE-BOX — [relDirPlanCount] / [relDirLadderCount] prove which arm
//     actually answered, which no comparison of answers can show.
//  3. MUTATION — each guard is shown to be load-bearing by being given the
//     wrong input and producing the wrong answer.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixtures — both storage routes
//
// The orientation ladder's FIRST question is the by-handle relationship-type
// record, which Cypher CREATE always writes and the Go API never does. The two
// fixtures below therefore reach the ladder through different arms, and a test
// that used only one would leave the other unproven. Both carry a RECIPROCAL
// pair with DISTINCT per-direction properties, which is what makes a wrong
// orientation observable rather than merely present.
// ─────────────────────────────────────────────────────────────────────────────

// relDirCypherFixture builds the graph through Cypher CREATE, so every edge
// carries a by-handle type record and the ladder answers at question 1.
//
// It returns the GRAPH, not an Engine. Both arms of the differential test must
// run over the SAME graph: node ids are allocated per graph, and a rendered
// PathValue carries them, so two separately-built fixtures differ in every
// path row for a reason that has nothing to do with the change under test.
func relDirCypherFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE (:N {k:'a'})`,
		`CREATE (:N {k:'b'})`,
		`CREATE (:N {k:'c'})`,
		`CREATE (:Tag {k:'t'})`,
		// reciprocal pair a<->b, distinct per-direction payload
		`MATCH (x:N {k:'a'}),(y:N {k:'b'}) CREATE (x)-[:T {w:1}]->(y)`,
		`MATCH (x:N {k:'b'}),(y:N {k:'a'}) CREATE (x)-[:T {w:2}]->(y)`,
		// non-reciprocal control
		`MATCH (x:N {k:'b'}),(y:N {k:'c'}) CREATE (x)-[:T {w:3}]->(y)`,
		// parallel edges b->c (multigraph)
		`MATCH (x:N {k:'b'}),(y:N {k:'c'}) CREATE (x)-[:T {w:4}]->(y)`,
		// self-loop
		`MATCH (x:N {k:'c'}) CREATE (x)-[:T {w:5}]->(x)`,
		// second type, and a low-cardinality label for the anchor-swap shape
		`MATCH (x:N {k:'a'}),(y:Tag {k:'t'}) CREATE (x)-[:HAS {w:6}]->(y)`,
	} {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			res.Close()
			t.Fatalf("seed drain %q: %v", q, err)
		}
		res.Close()
	}
	return g
}

// relDirGoAPIFixture builds the SAME shape through the Go API only. No
// by-handle type record exists, so the ladder falls through question 1 to the
// topology probes — the exact arm that the 26_social_scale_bench profile found
// at 19.59s.
func relDirGoAPIFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	mk := func(k, label string) {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, label); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "k", lpg.StringValue(k)); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "N")
	mk("b", "N")
	mk("c", "N")
	mk("t", "Tag")
	edge := func(s, d, typ string, w int64) {
		if err := g.AddEdge(s, d, 1); err != nil {
			t.Fatal(err)
		}
		g.SetEdgeLabel(s, d, typ)
		if err := g.SetEdgeProperty(s, d, "w", lpg.Int64Value(w)); err != nil {
			t.Fatal(err)
		}
	}
	edge("a", "b", "T", 1)
	edge("b", "a", "T", 2)
	edge("b", "c", "T", 3)
	edge("c", "c", "T", 5)
	edge("a", "t", "HAS", 6)
	return g
}

// relDirColumnarFixture is wide enough for the COLUMNAR recogniser to admit the
// hop, which the 4-node fixtures above are not: a chunk chain needs a scan the
// planner estimates as worth batching. It carries reciprocal pairs with
// distinct per-direction payloads, exactly as they do, so a wrong orientation is
// observable, and it is written through the Go API so the ladder arm falls to
// the topology probes.
//
// It exists because the columnar path is a SEPARATE producer of the
// stored-direction cell — [exec.Expand.appendChunkRow] writes it with
// AppendBool into an [expr.KindBool] chunk column, not as a boxed row cell — and
// a corpus that only reached the row-mode path left that emission unpinned. The
// row-mode plans of the small fixtures were verified with EXPLAIN, which is how
// the gap was found rather than assumed.
func relDirColumnarFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	const n = 200
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("p%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "age", lpg.Int64Value(int64(i%40))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			t.Fatal(err)
		}
	}
	edge := func(a, b string, w int64) {
		if err := g.AddEdge(a, b, 1); err != nil {
			t.Fatal(err)
		}
		g.SetEdgeLabel(a, b, "K")
		if err := g.SetEdgeProperty(a, b, "w", lpg.Int64Value(w)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		a := fmt.Sprintf("p%d", i)
		b := fmt.Sprintf("p%d", (i+7)%n)
		edge(a, b, int64(1000+i))
		// Every third pair is RECIPROCAL, with a payload that differs from its
		// partner's, so an orientation swap changes the sum.
		if i%3 == 0 {
			edge(b, a, int64(2000+i))
		}
	}
	return g
}

// relDirColumnarQueries are shapes the planner admits into the CHUNK chain, each
// binding a NAMED relationship variable on an UNDIRECTED hop — the only
// combination that emits the stored-direction chunk column. Verified columnar by
// [TestRelStoredDir_ColumnarEmissionIsCovered], which fails rather than skips if
// the planner stops admitting them.
var relDirColumnarQueries = []struct {
	name string
	q    string
	// columnar asserts this shape is planned through the CHUNK chain. False for
	// a shape that is in this list for a DIFFERENT reason — the wide fixture is
	// also what makes an ir.Apply rebase reachable — so the columnar coverage
	// assertion stays a claim about the shapes that actually make it.
	columnar bool
}{
	{"columnar_count_star",
		`MATCH (a:P)-[r:K]-(b:P) WHERE b.age > 10 RETURN count(*) AS c`, true},
	{"columnar_sum_rel_prop",
		`MATCH (a:P)-[r:K]-(b:P) WHERE b.age > 10 RETURN sum(r.w) AS s`, true},
	{"columnar_grouped",
		`MATCH (a:P)-[r:K]-(b:P) WHERE b.age > 10 RETURN b.name AS nm, sum(r.w) AS s ORDER BY nm LIMIT 20`, true},
	{"columnar_count_rel",
		`MATCH (a:P)-[r:K]-(b:P) RETURN count(r) AS c`, true},
	{"columnar_collect_endpoints",
		`MATCH (a:P)-[r:K]-(b:P) WHERE b.age > 30 RETURN sum(r.w) AS s, count(r) AS c`, true},
	// Two comma-free MATCH clauses put the undirected named hop on the INNER
	// side of an ir.Apply, which is the one path that REBASES dirCol: the inner
	// subtree is built against a fresh 0-based schema and every recorded column
	// is then shifted by the outer width. Verified with EXPLAIN to plan as an
	// Apply over an Expand, and to take 1270 orientation decisions — so a
	// dirCol left unshifted reads the wrong cell rather than nothing.
	{"apply_inner_undirected_rebases_dircol",
		`MATCH (a:P {age:3}) MATCH (b:P)-[r:K]-(c:P) WHERE c.age > 20 RETURN sum(r.w) AS s`, false},
}

// TestRelStoredDir_ColumnarEmissionIsCovered fails — it does NOT skip — when the
// planner stops routing these shapes through the chunk chain, because then the
// differential coverage of the columnar emission silently disappears and every
// other test here would still pass.
func TestRelStoredDir_ColumnarEmissionIsCovered(t *testing.T) {
	relDirPlanDisabled.Store(false)
	relDirColDisabled.Store(false)
	eng := NewEngine(relDirColumnarFixture(t))
	checked := 0
	for _, tc := range relDirColumnarQueries {
		if !tc.columnar {
			continue
		}
		checked++
		plan, err := eng.Explain(tc.q, nil)
		if err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		if !strings.Contains(plan, "columnarExpand") {
			t.Errorf("%s is no longer planned through the chunk chain, so the "+
				"columnar stored-direction emission is UNCOVERED:\n%s", tc.name, plan)
		}
	}
	if checked == 0 {
		t.Fatal("no shape claims to be columnar, so this test asserts nothing")
	}
}

// TestRelStoredDir_ColumnarDifferentialAgainstLadder is the differential for the
// columnar emission: with both halves off every orientation goes to the ladder,
// with both on the chunk column answers, and the rows must be identical.
//
// Not parallel: process-wide plan-build switches.
func TestRelStoredDir_ColumnarDifferentialAgainstLadder(t *testing.T) {
	g := relDirColumnarFixture(t)
	for _, tc := range relDirColumnarQueries {
		t.Run(tc.name, func(t *testing.T) {
			relDirPlanDisabled.Store(true)
			relDirColDisabled.Store(true)
			ladder := relDirRunRows(t, NewEngine(g), tc.q)
			relDirPlanDisabled.Store(false)
			relDirColDisabled.Store(false)
			planned := relDirRunRows(t, NewEngine(g), tc.q)
			if len(ladder) == 0 {
				t.Fatal("the shape returned no rows: it cannot detect a wrong orientation")
			}
			if len(ladder) != len(planned) {
				t.Fatalf("row count: ladder=%d planned=%d", len(ladder), len(planned))
			}
			for i := range ladder {
				if ladder[i] != planned[i] {
					t.Errorf("row %d: ladder=%q planned=%q", i, ladder[i], planned[i])
				}
			}
		})
	}
	relDirPlanDisabled.Store(false)
	relDirColDisabled.Store(false)
}

// relDirFixtures is the fixture matrix every table-driven test below runs over.
var relDirFixtures = []struct {
	name  string
	build func(*testing.T) *lpg.Graph[string, float64]
}{
	{"cypher_written", relDirCypherFixture},
	{"go_api_written", relDirGoAPIFixture},
}

// relDirQueries is the query corpus. Each entry names the surface it reaches,
// because a corpus whose coverage is not stated is a corpus whose gaps are not
// known.
var relDirQueries = []struct {
	name string
	q    string
}{
	// The two hops the acceptance criterion names, over the reciprocal pair.
	{"reverse_hop_reciprocal",
		`MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN r.w AS w, id(r) AS rid, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	{"undirected_hop_reciprocal",
		`MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r.w AS w, id(r) AS rid, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	{"forward_hop_reciprocal",
		`MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w, id(r) AS rid, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	// Non-reciprocal control, all three directions.
	{"forward_hop_plain",
		`MATCH (n:N {k:'b'})-[r:T]->(m:N {k:'c'}) RETURN r.w AS w, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	{"reverse_hop_plain",
		`MATCH (n:N {k:'c'})<-[r:T]-(m:N {k:'b'}) RETURN r.w AS w, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	{"undirected_hop_plain",
		`MATCH (n:N {k:'c'})-[r:T]-(m) RETURN r.w AS w, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	// Self-loop: src == dst, where "inverted" is unobservable and must stay so.
	{"self_loop_all_dirs",
		`MATCH (n:N {k:'c'})-[r:T]-(x:N {k:'c'}) RETURN r.w AS w, startNode(r).k AS sk, endNode(r).k AS ek ORDER BY w`},
	// WHERE forces populateRowCtx — the PER-ROW edgeVarMeta read.
	{"where_on_rel_reverse",
		`MATCH (n:N {k:'a'})<-[r:T]-(m) WHERE r.w = 2 RETURN r.w AS w, startNode(r).k AS sk`},
	{"where_on_rel_undirected",
		`MATCH (n:N {k:'a'})-[r:T]-(m) WHERE r.w > 0 RETURN r.w AS w, startNode(r).k AS sk ORDER BY w`},
	// Two property reads of one variable — the projection-FUSION closure path.
	{"fused_two_reads_reverse",
		`MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN r.w AS a, r.w AS b`},
	// type(r) — the field extractor, whose materialisation class differs.
	{"type_extractor_reverse",
		`MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN type(r) AS ty, r.w AS w`},
	// Bare relationship projection — the whole RelationshipValue escapes.
	{"bare_rel_reverse", `MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN r AS rr`},
	{"bare_rel_undirected", `MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r AS rr ORDER BY id(rr)`},
	// Named path — the pathChainStep route, which reads the same triplet.
	{"named_path_reverse", `MATCH p = (n:N {k:'a'})<-[r:T]-(m) RETURN p`},
	{"named_path_undirected", `MATCH p = (n:N {k:'a'})-[r:T]-(m) RETURN p`},
	// OPTIONAL MATCH — planned as OptionalApply over a plain Expand.
	{"optional_reverse",
		`MATCH (n:N {k:'a'}) OPTIONAL MATCH (n)<-[r:T]-(m) RETURN r.w AS w, startNode(r).k AS sk`},
	{"optional_miss",
		`MATCH (n:N {k:'a'}) OPTIONAL MATCH (n)<-[r:NOPE]-(m) RETURN r.w AS w`},
	// Aggregation — reaches the columnar pre-projection chain.
	{"aggregate_reverse", `MATCH (n:N)<-[r:T]-(m) RETURN sum(r.w) AS s, count(r) AS c`},
	{"aggregate_undirected", `MATCH (n:N)-[r:T]-(m) RETURN sum(r.w) AS s, count(r) AS c`},
	// Multi-hop with cyphermorphism across a direction change.
	{"two_hop_mixed_dirs",
		`MATCH (a:N {k:'a'})-[r1:T]->(b)<-[r2:T]-(c) RETURN r1.w AS w1, r2.w AS w2 ORDER BY w1, w2`},
	// UNION with DISAGREEING directions on ONE name — the demotion shape.
	{"union_mixed_dirs",
		`MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w
		 UNION ALL
		 MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN r.w AS w`},
	{"union_mixed_dirs_reversed",
		`MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN r.w AS w
		 UNION ALL
		 MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w`},
	{"union_undirected_then_forward",
		`MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r.w AS w
		 UNION ALL
		 MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w`},
	// WITH boundary: the column carries a self-describing RelationshipValue
	// afterwards, so the triplet coordinates no longer apply.
	{"with_forward_rel",
		`MATCH (n:N {k:'a'})<-[r:T]-(m) WITH r AS rr RETURN rr.w AS w, startNode(rr).k AS sk`},
	// Anchor-swap candidate: a written-forward hop the planner may re-root,
	// executing it as DirIn. Read p.Direction at the node, not the query text.
	{"anchor_swap_candidate",
		`MATCH (n:N)-[r:HAS]->(t:Tag) RETURN r.w AS w, startNode(r).k AS sk, endNode(r).k AS ek`},
	// Variable-length control: the VLE route is untouched and must stay so.
	{"vle_undirected",
		`MATCH (n:N {k:'a'})-[r:T*1..2]-(m) RETURN [x IN r | x.w] AS ws ORDER BY ws`},
}

// relDirRunRows renders every row of q as one comparable string.
func relDirRunRows(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	defer res.Close()
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("%s=%v", c, rec[c]))
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain %q: %v", q, err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. DIFFERENTIAL
// ─────────────────────────────────────────────────────────────────────────────

// TestRelStoredDir_DifferentialAgainstLadder is the correctness gate: for every
// fixture and every query, the plan-resolved orientation must produce exactly
// the rows the per-row ladder produces.
//
// Not parallel: it flips a process-wide plan-build switch.
func TestRelStoredDir_DifferentialAgainstLadder(t *testing.T) {
	for _, fx := range relDirFixtures {
		for _, tc := range relDirQueries {
			t.Run(fx.name+"/"+tc.name, func(t *testing.T) {
				g := fx.build(t)
				// BOTH halves off in the base arm. Disabling only the
				// plan-time one leaves an UNDIRECTED shape answered by the
				// column in both arms, which compares it against itself: a
				// mutation that flipped the emitted direction bit passed every
				// subtest here and was caught only by the TCK.
				relDirPlanDisabled.Store(true)
				relDirColDisabled.Store(true)
				ladder := relDirRunRows(t, NewEngine(g), tc.q)
				relDirPlanDisabled.Store(false)
				relDirColDisabled.Store(false)
				planned := relDirRunRows(t, NewEngine(g), tc.q)
				if len(ladder) != len(planned) {
					t.Fatalf("row count: ladder=%d planned=%d\n  ladder  = %v\n  planned = %v",
						len(ladder), len(planned), ladder, planned)
				}
				for i := range ladder {
					if ladder[i] != planned[i] {
						t.Errorf("row %d:\n  ladder  = %q\n  planned = %q", i, ladder[i], planned[i])
					}
				}
			})
		}
	}
	relDirPlanDisabled.Store(false)
	relDirColDisabled.Store(false)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. WHITE-BOX — which arm answered
// ─────────────────────────────────────────────────────────────────────────────

// relDirArms is how the three answering paths partition a shape's orientation
// decisions. Every decision lands in exactly one bucket, which is what lets a
// test assert that a path was NOT taken as firmly as that it was.
type relDirArms struct {
	plan   uint64 // answered from edgeVarInfo.dir (a directed hop)
	column uint64 // answered from the emitted per-row cell (an undirected hop)
	ladder uint64 // answered by relStoredInverted
}

func (a relDirArms) total() uint64 { return a.plan + a.column + a.ladder }

func (a relDirArms) String() string {
	return fmt.Sprintf("plan=%d column=%d ladder=%d", a.plan, a.column, a.ladder)
}

// relDirCounts runs q with the counters armed and returns how the decisions
// partitioned across the three arms.
func relDirCounts(t *testing.T, eng *Engine, q string) relDirArms {
	t.Helper()
	relDirPlanCount.Store(0)
	relDirColumnCount.Store(0)
	relDirLadderCount.Store(0)
	relDirCountersOn.Store(true)
	defer relDirCountersOn.Store(false)
	relDirRunRows(t, eng, q)
	return relDirArms{
		plan:   relDirPlanCount.Load(),
		column: relDirColumnCount.Load(),
		ladder: relDirLadderCount.Load(),
	}
}

// TestRelStoredDir_PlanPathIsReached proves the new arm actually RUNS, which
// the differential test above cannot show: it compares answers, and the two
// arms agree by construction.
//
// Not parallel: it arms process-wide counters.
func TestRelStoredDir_PlanPathIsReached(t *testing.T) {
	relDirPlanDisabled.Store(false)
	for _, fx := range relDirFixtures {
		t.Run(fx.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				q    string
				want string // which arm must answer EVERY decision this shape takes
			}{
				{"forward_hop",
					`MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w`, "plan"},
				{"reverse_hop",
					`MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN r.w AS w`, "plan"},
				{"undirected_hop",
					`MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r.w AS w`, "column"},
				{"undirected_anonymous_rel",
					`MATCH (n:N {k:'a'})-[:T]-(m) RETURN m.k AS k ORDER BY k`, "none"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					eng := NewEngine(fx.build(t))
					arms := relDirCounts(t, eng, tc.q)
					if tc.want == "none" {
						// An ANONYMOUS relationship is never hydrated, so it
						// registers no edgeVarMeta entry and emits no direction
						// column. Nothing to decide is the correct answer here,
						// and pinning it is what stops the column being emitted
						// for a hop that could never read it.
						if arms.total() != 0 {
							t.Fatalf("an anonymous relationship took %s", arms)
						}
						return
					}
					if arms.total() == 0 {
						t.Fatal("no orientation decision was taken at all — the oracle " +
							"cannot distinguish the arms and proves nothing")
					}
					got := map[string]uint64{
						"plan": arms.plan, "column": arms.column, "ladder": arms.ladder,
					}
					if got[tc.want] != arms.total() {
						t.Fatalf("want every decision answered by the %s arm, got %s",
							tc.want, arms)
					}
				})
			}
		})
	}
}

// TestRelStoredDir_DisabledSwitchForcesTheLadder proves [relDirPlanDisabled] is
// the seam the differential test claims it is: with it set, a DIRECTED hop must
// take the ladder and NOT the plan arm. Without this, an ineffective switch
// would make the differential test compare one arm against itself.
//
// Not parallel: process-wide switch and counters.
func TestRelStoredDir_DisabledSwitchForcesTheLadder(t *testing.T) {
	relDirPlanDisabled.Store(true)
	relDirColDisabled.Store(true)
	defer relDirPlanDisabled.Store(false)
	defer relDirColDisabled.Store(false)
	g := relDirCypherFixture(t)
	for _, tc := range []struct{ name, q string }{
		{"directed", `MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w`},
		{"undirected", `MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r.w AS w`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arms := relDirCounts(t, NewEngine(g), tc.q)
			if arms.total() == 0 {
				t.Fatal("no orientation decision was taken — the switches cannot be shown to work")
			}
			if arms.ladder != arms.total() {
				t.Errorf("with both paths disabled, want every decision on the ladder, got %s", arms)
			}
		})
	}
}

// TestRelStoredDir_ColumnSwitchIsIndependent proves the two seams are separable,
// which is what makes the two halves of rmp #2864 measurable APART: with only
// the column disabled, a directed hop must still be answered by the plan and an
// undirected one must fall to the ladder.
//
// Not parallel: process-wide switches and counters.
func TestRelStoredDir_ColumnSwitchIsIndependent(t *testing.T) {
	relDirPlanDisabled.Store(false)
	relDirColDisabled.Store(true)
	defer relDirColDisabled.Store(false)
	g := relDirCypherFixture(t)
	if arms := relDirCounts(t, NewEngine(g),
		`MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w`); arms.plan != arms.total() || arms.total() == 0 {
		t.Errorf("directed hop with the column disabled: want all-plan, got %s", arms)
	}
	if arms := relDirCounts(t, NewEngine(g),
		`MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r.w AS w`); arms.ladder != arms.total() || arms.total() == 0 {
		t.Errorf("undirected hop with the column disabled: want all-ladder, got %s", arms)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. The acceptance-criterion regression test
// ─────────────────────────────────────────────────────────────────────────────

// TestRelStoredDir_ReciprocalPairReverseAndUndirected pins the rmp #2504
// surface against rmp #2864's plan-time resolution, with the EXPECTED rows
// written out rather than compared against another arm — so it fails on a build
// where BOTH arms are wrong.
//
// a<->b is a reciprocal pair: a->b carries w=1 and b->a carries w=2. Anchored at
// a, the reverse hop must bind ONLY b->a (w=2, stored b->a) and the undirected
// hop must bind BOTH, each with the storage endpoints of its own edge.
func TestRelStoredDir_ReciprocalPairReverseAndUndirected(t *testing.T) {
	relDirPlanDisabled.Store(false)
	for _, fx := range relDirFixtures {
		t.Run(fx.name, func(t *testing.T) {
			eng := NewEngine(fx.build(t))
			t.Run("reverse_hop", func(t *testing.T) {
				got := relDirRunRows(t, eng,
					`MATCH (n:N {k:'a'})<-[r:T]-(m) RETURN id(r) AS rid, r.w AS w, `+
						`startNode(r).k AS sk, endNode(r).k AS ek`)
				want := []string{`rid=2 w=2 sk="b" ek="a"`}
				assertRelDirRows(t, got, want)
			})
			// The undirected hop MUST carry id(r). Without it the oracle cannot
			// fail: an undirected hop over a reciprocal pair enumerates BOTH
			// edges, so swapping which row reports which edge leaves the
			// ORDER BY w multiset untouched. A mutation that flipped the
			// emitted direction bit passed this assertion and was caught only
			// by the TCK. Pairing each edge's own id with its own payload is
			// what makes the swap visible.
			t.Run("undirected_hop", func(t *testing.T) {
				ids := relDirRunRows(t, eng,
					`MATCH (n:N {k:'a'})-[r:T]->(m) RETURN id(r) AS rid`)
				assertRelDirRows(t, ids, []string{"rid=1"}) // a->b is handle 1
				got := relDirRunRows(t, eng,
					`MATCH (n:N {k:'a'})-[r:T]-(m) RETURN id(r) AS rid, r.w AS w, `+
						`startNode(r).k AS sk, endNode(r).k AS ek ORDER BY rid`)
				want := []string{
					`rid=1 w=1 sk="a" ek="b"`,
					`rid=2 w=2 sk="b" ek="a"`,
				}
				assertRelDirRows(t, got, want)
			})
			t.Run("forward_hop_control", func(t *testing.T) {
				got := relDirRunRows(t, eng,
					`MATCH (n:N {k:'a'})-[r:T]->(m) RETURN id(r) AS rid, r.w AS w, `+
						`startNode(r).k AS sk, endNode(r).k AS ek`)
				want := []string{`rid=1 w=1 sk="a" ek="b"`}
				assertRelDirRows(t, got, want)
			})
		})
	}
}

func assertRelDirRows(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d\n  got  = %v\n  want = %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. The name-collision demotion
// ─────────────────────────────────────────────────────────────────────────────

// TestRelStoredDir_CollisionFallsBackToLadder is the regression test for the
// hazard that makes [demoteRelDirOnDisagreement] necessary, and it is written so
// it FAILS on a build that omits the demotion.
//
// `... -[r:T]-> ... UNION ALL ... <-[r:T]- ...` registers the name `r` TWICE in
// one build, with opposite directions and the SAME triplet columns; the second
// registration wins, and because edgeVarMeta is read per row, both branches then
// read it. Trusting the last writer returned one row where two are required —
// measured, on exactly this fixture.
//
// Two assertions, and both are needed: the ROWS (a wrong direction loses or
// swaps one) and the COUNTERS (the ladder must have answered, which is what
// proves the demotion fired rather than the rows being right by luck).
//
// Not parallel: process-wide counters.
func TestRelStoredDir_CollisionFallsBackToLadder(t *testing.T) {
	relDirPlanDisabled.Store(false)
	for _, fx := range relDirFixtures {
		t.Run(fx.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				q    string
				want []string
			}{
				{"forward_then_reverse",
					`MATCH (n:N {k:'a'})-[r:T]->(m) WHERE r.w = 1 RETURN r.w AS w
					 UNION ALL
					 MATCH (n:N {k:'a'})<-[r:T]-(m) WHERE r.w = 2 RETURN r.w AS w`,
					[]string{"w=1", "w=2"}},
				{"reverse_then_forward",
					`MATCH (n:N {k:'a'})<-[r:T]-(m) WHERE r.w = 2 RETURN r.w AS w
					 UNION ALL
					 MATCH (n:N {k:'a'})-[r:T]->(m) WHERE r.w = 1 RETURN r.w AS w`,
					[]string{"w=2", "w=1"}},
				{"undirected_then_forward",
					`MATCH (n:N {k:'a'})-[r:T]-(m) RETURN r.w AS w
					 UNION ALL
					 MATCH (n:N {k:'a'})-[r:T]->(m) RETURN r.w AS w`,
					[]string{"w=1", "w=2", "w=1"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					eng := NewEngine(fx.build(t))
					arms := relDirCounts(t, eng, tc.q)
					got := relDirRunRows(t, eng, tc.q)
					assertRelDirRows(t, got, tc.want)
					if arms.ladder != arms.total() || arms.total() == 0 {
						t.Errorf("want every decision on the ladder, got %s: the disagreeing "+
							"registration was NOT fully demoted, so these rows are right "+
							"only by coincidence", arms)
					}
				})
			}
		})
	}
}

// TestRelStoredDir_DemotionIsMonotone pins the property the header claims: once
// a name has been demoted, no later registration can promote it back. It is a
// unit test of the rule rather than of a query, because the third registration
// is what a two-branch query cannot reach.
func TestRelStoredDir_DemotionIsMonotone(t *testing.T) {
	t.Parallel()
	out := exec.DirOut
	in := exec.DirIn
	for _, tc := range []struct {
		name string
		seq  []exec.Direction
		want exec.Direction
	}{
		{"all_out", []exec.Direction{out, out, out}, out},
		{"all_in", []exec.Direction{in, in}, in},
		{"out_then_in", []exec.Direction{out, in}, relDirUnresolved},
		{"out_in_out", []exec.Direction{out, in, out}, relDirUnresolved},
		{"out_both_out", []exec.Direction{out, relDirUnresolved, out}, relDirUnresolved},
		{"single_both", []exec.Direction{relDirUnresolved}, relDirUnresolved},
		{"in_then_out", []exec.Direction{in, out}, relDirUnresolved},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var live edgeVarInfo
			for i, d := range tc.seq {
				next := edgeVarInfo{dir: d}
				if i > 0 {
					next = demoteRelDirOnDisagreement(next, live)
				}
				live = next
			}
			if live.dir != tc.want {
				t.Fatalf("resolved dir = %v, want %v", live.dir, tc.want)
			}
		})
	}
}

// TestRelStoredDir_DemotionClearsTheColumnCoordinate pins the dirCol half of
// [demoteRelDirOnDisagreement] at the UNIT level, and it is deliberately not
// claimed as more than that.
//
// # What is proven, and what is not
//
// Removing this half of the rule was MUTATION-TESTED and changed no answer: the
// 24-query differential corpus, the whole cypher package suite and all 3897 TCK
// scenarios still passed, and two purpose-built UNION shapes designed to make a
// surviving coordinate land on ANOTHER undirected hop's direction cell —
//
//	MATCH (a:N)-[q:T]-(b)-[r:T]->(c) … UNION ALL MATCH (x:N)-[r:T]-(y) …
//	MATCH (a:N)-[q:T]-(b)<-[r:T]-(c) … UNION ALL MATCH (x:N)-[r:T]-(y) …
//
// both returned identical rows with and without it, because in each the stale
// cell agreed with the truth.
//
// So the guard is ARGUED, not PINNED end to end: it is kept because it costs one
// comparison at plan-build time, because it can only ever make a shape slower,
// and because the alternative is to rely on a coordinate one hop emits being
// read by another hop's rows — which is exactly the class of hazard the
// direction half of this rule was caught by MEASUREMENT, not by reasoning. This
// test therefore pins the RULE so it cannot silently regress, and says plainly
// that no end-to-end consequence of its absence has been demonstrated.
func TestRelStoredDir_DemotionClearsTheColumnCoordinate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		prev, next   int
		wantAfterDem int
	}{
		{"agreeing_columns_kept", 3, 3, 3},
		{"undirected_then_directed", 3, -1, -1},
		{"directed_then_undirected", -1, 3, -1},
		{"different_columns", 3, 7, -1},
		{"both_absent", -1, -1, -1},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := demoteRelDirOnDisagreement(
				edgeVarInfo{dir: exec.DirBoth, dirCol: tc.next},
				edgeVarInfo{dir: exec.DirBoth, dirCol: tc.prev})
			if got.dirCol != tc.wantAfterDem {
				t.Fatalf("dirCol = %d, want %d", got.dirCol, tc.wantAfterDem)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. MUTATION — every guard is load-bearing
// ─────────────────────────────────────────────────────────────────────────────

// TestRelStoredDir_MutationEachGuardCanFail feeds [relStoredInvertedForHop] the
// WRONG plan direction for a known row and asserts the answer flips. Without
// this, a guard that had been wired to a constant would pass every test above:
// the differential test would compare two identically-wrong arms, and the
// counters only say WHICH arm ran, not that its answer depends on its input.
func TestRelStoredDir_MutationEachGuardCanFail(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	g.SetEdgeLabel("a", "b", "T")
	if err := g.AddEdge("b", "a", 1); err != nil {
		t.Fatal(err)
	}
	g.SetEdgeLabel("b", "a", "T")

	view := g.ReadAt(nil)
	aID, okA := view.AdjList().Mapper().Lookup("a")
	bID, okB := view.AdjList().Mapper().Lookup("b")
	if !okA || !okB {
		t.Fatal("fixture node ids did not resolve")
	}

	// The row is the FORWARD traversal a -> b, whose stored orientation is
	// a -> b: not inverted. A DirOut meta must agree; a DirIn meta must not,
	// or the guard is not reading the field at all.
	for _, tc := range []struct {
		name string
		dir  exec.Direction
		want bool
	}{
		{"correct_DirOut_says_forward", exec.DirOut, false},
		{"mutated_DirIn_says_inverted", exec.DirIn, true},
		{"sentinel_defers_to_the_ladder", relDirUnresolved, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := relStoredInvertedForHop(
				exec.Row{}, edgeVarInfo{dir: tc.dir, dirCol: -1}, view,
				graph.NodeID(aID), graph.NodeID(bID), "a", "b", 0)
			if got != tc.want {
				t.Fatalf("relStoredInvertedForHop(dir=%v) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

// TestRelStoredDir_ColumnSentinelRefusesAStaleCoordinate is the unit proof of
// the sentinel-not-default rule for the COLUMN half: only an [expr.BoolValue]
// cell is read as a direction, so a coordinate that has gone stale across a
// projection — where the slot holds a node id, an edge handle, a boxed entity or
// nothing at all — falls back to the ladder instead of reading one of those as
// an orientation.
func TestRelStoredDir_ColumnSentinelRefusesAStaleCoordinate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		row     exec.Row
		dirCol  int
		wantInv bool
		wantOK  bool
	}{
		{"bool_true_is_read", exec.Row{expr.BoolValue(true)}, 0, true, true},
		{"bool_false_is_read", exec.Row{expr.BoolValue(false)}, 0, false, true},
		{"no_column_declines", exec.Row{expr.BoolValue(true)}, -1, false, false},
		{"out_of_range_declines", exec.Row{expr.BoolValue(true)}, 7, false, false},
		{"stale_node_id_declines", exec.Row{expr.IntegerValue(1)}, 0, false, false},
		{"stale_zero_id_declines", exec.Row{expr.IntegerValue(0)}, 0, false, false},
		{"stale_rel_value_declines", exec.Row{expr.RelationshipValue{ID: 1}}, 0, false, false},
		{"null_declines", exec.Row{expr.Null}, 0, false, false},
		{"empty_row_declines", exec.Row{}, 0, false, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inv, ok := relStoredDirFromRow(tc.row, tc.dirCol)
			if inv != tc.wantInv || ok != tc.wantOK {
				t.Fatalf("relStoredDirFromRow = (%v, %v), want (%v, %v)",
					inv, ok, tc.wantInv, tc.wantOK)
			}
		})
	}
}

// TestRelStoredDir_ColumnValueIsLoadBearing feeds [relStoredInvertedForHop] a
// row whose direction cell says the OPPOSITE of the truth and asserts the answer
// follows the cell. Without it, an emission wired to a constant would satisfy
// every other test here: the differential test would compare two arms that are
// wrong in the same way, and the counters only say which arm ran.
func TestRelStoredDir_ColumnValueIsLoadBearing(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	g.SetEdgeLabel("a", "b", "T")
	view := g.ReadAt(nil)
	aID, _ := view.AdjList().Mapper().Lookup("a")
	bID, _ := view.AdjList().Mapper().Lookup("b")

	// meta.dir is the DirBoth sentinel, so the column decides. The row is the
	// traversal a -> b, stored a -> b, so the truthful cell is false.
	for _, cell := range []bool{false, true} {
		cell := cell
		t.Run(fmt.Sprintf("cell_%v", cell), func(t *testing.T) {
			t.Parallel()
			row := exec.Row{expr.IntegerValue(aID), expr.IntegerValue(0),
				expr.IntegerValue(bID), expr.BoolValue(cell)}
			got := relStoredInvertedForHop(row,
				edgeVarInfo{dir: relDirUnresolved, srcCol: 0, edgeCol: 1, dstCol: 2, dirCol: 3},
				view, graph.NodeID(aID), graph.NodeID(bID), "a", "b", 0)
			if got != cell {
				t.Fatalf("the answer was %v with a %v cell — the column is not being read", got, cell)
			}
		})
	}
}

// TestRelStoredDir_ZeroValueMetaIsSlowNotWrong pins the sentinel choice: an
// edgeVarInfo built WITHOUT a direction — a hand-made row, or a future producer
// that forgets to set one — must reach the ladder, not silently assert an
// orientation. This is why [relDirUnresolved] is the ZERO value and DirOut is
// not.
func TestRelStoredDir_ZeroValueMetaIsSlowNotWrong(t *testing.T) {
	if got := (edgeVarInfo{}).dir; got != relDirUnresolved {
		t.Fatalf("zero-value edgeVarInfo.dir = %v, want the unresolved sentinel", got)
	}
	relDirPlanCount.Store(0)
	relDirLadderCount.Store(0)
	relDirCountersOn.Store(true)
	defer relDirCountersOn.Store(false)

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddEdge("b", "a", 1); err != nil {
		t.Fatal(err)
	}
	g.SetEdgeLabel("b", "a", "T")
	view := g.ReadAt(nil)
	aID, _ := view.AdjList().Mapper().Lookup("a")
	bID, _ := view.AdjList().Mapper().Lookup("b")

	// Traversal a -> b, stored b -> a: the ladder must find it inverted.
	if got := relStoredInvertedForHop(exec.Row{}, edgeVarInfo{}, view,
		graph.NodeID(aID), graph.NodeID(bID), "a", "b", 0); !got {
		t.Error("the zero-value meta did not resolve the inverted storage through the ladder")
	}
	if relDirPlanCount.Load() != 0 {
		t.Error("a zero-value meta was answered by the plan arm")
	}
	if relDirLadderCount.Load() == 0 {
		t.Error("a zero-value meta did not reach the ladder")
	}
}

// TestRelStoredDir_AnchorSwapReadsTheExecutedDirection pins the premise that the
// direction must come from the ir.Expand node and never from the query text:
// [mirrorAnchorSite] re-roots a written-forward hop as a new ir.Expand carrying
// the REVERSED direction, so a `-[r]->` query can execute as DirIn.
//
// The test is only meaningful if the swap actually fires, so it asserts that
// [anchorSwapBuildCount] moved; if it does not, the test says so rather than
// reporting coverage it did not have.
func TestRelStoredDir_AnchorSwapReadsTheExecutedDirection(t *testing.T) {
	relDirPlanDisabled.Store(false)
	// The swap needs a large from-label and a tiny to-label for the cost model
	// to prefer re-rooting, so this test carries its own fixture: 400 :N nodes,
	// one :Tag, and one written-forward :HAS edge between them.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 400; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "N"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "k", lpg.StringValue(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddNode("t"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeLabel("t", "Tag"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetNodeProperty("t", "k", lpg.StringValue("t")); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("n7", "t", 1); err != nil {
		t.Fatal(err)
	}
	g.SetEdgeLabel("n7", "t", "HAS")
	if err := g.SetEdgeProperty("n7", "t", "w", lpg.Int64Value(6)); err != nil {
		t.Fatal(err)
	}

	const q = `MATCH (n:N)-[r:HAS]->(t:Tag) RETURN r.w AS w, startNode(r).k AS sk, endNode(r).k AS ek`
	before := anchorSwapBuildCount.Load()
	eng := NewEngine(g)
	arms := relDirCounts(t, eng, q)
	got := relDirRunRows(t, eng, q)
	swapped := anchorSwapBuildCount.Load() - before
	// The endpoints are the STORED ones: n7 -> t, whichever end the plan
	// anchored on and whichever direction it executed.
	assertRelDirRows(t, got, []string{`w=6 sk="n7" ek="t"`})
	// The two assertions TOGETHER are the proof. The counters say the PLAN
	// answered (so the recorded direction was used, not the ladder), and the
	// rows say the answer was right — which it can only be if the recorded
	// direction was the one the re-rooted hop EXECUTES (DirIn) rather than the
	// one the query text reads (`-[r]->`). Reading the query text would still
	// have satisfied the counters and failed the rows.
	if arms.total() == 0 || arms.plan != arms.total() {
		t.Errorf("orientation decisions: %s — want the plan arm to have answered "+
			"every one, or this test does not pin the recorded direction", arms)
	}
	if swapped == 0 {
		t.Fatal("the planner did not re-root this hop, so the anchor-swap " +
			"direction path is NOT covered — the fixture no longer qualifies " +
			"for the swap and this test proves nothing")
	}
}

// relDirBenchFixture builds a fan graph for the benchmarks below: one hub with
// `fan` neighbours, every edge written through the Go API so the orientation
// ladder falls to the TOPOLOGY probes — the arm the 26_social_scale_bench
// profile attributed 19.59s of 20.11s to.
func relDirBenchFixture(fan int, reciprocal bool) *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	_ = g.AddNode("hub")
	_ = g.SetNodeLabel("hub", "H")
	for i := 0; i < fan; i++ {
		k := fmt.Sprintf("n%d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "P")
		_ = g.AddEdge("hub", k, 1)
		g.SetEdgeLabel("hub", k, "K")
		_ = g.SetEdgeProperty("hub", k, "w", lpg.Int64Value(int64(i)))
		if reciprocal {
			_ = g.AddEdge(k, "hub", 1)
			g.SetEdgeLabel(k, "hub", "K")
			_ = g.SetEdgeProperty(k, "hub", "w", lpg.Int64Value(int64(-i)))
		}
	}
	return g
}

// relDirBenchQueries are the shapes the A/B measures. Each binds a relationship
// variable and reads a property from it, so every emitted row takes exactly one
// orientation decision.
var relDirBenchQueries = []struct {
	name       string
	q          string
	reciprocal bool
	undirected bool
}{
	{"out_hop", `MATCH (h:H)-[r:K]->(p:P) RETURN sum(r.w) AS s`, false, false},
	{"in_hop", `MATCH (p:P)<-[r:K]-(h:H) RETURN sum(r.w) AS s`, false, false},
	{"out_hop_reciprocal", `MATCH (h:H)-[r:K]->(p:P) RETURN sum(r.w) AS s`, true, false},
	{"in_hop_reciprocal", `MATCH (p:P)<-[r:K]-(h:H) RETURN sum(r.w) AS s`, true, false},
	{"undirected_hop", `MATCH (h:H)-[r:K]-(p:P) RETURN sum(r.w) AS s`, false, true},
	{"undirected_hop_reciprocal", `MATCH (h:H)-[r:K]-(p:P) RETURN sum(r.w) AS s`, true, true},
	{"undirected_bare_rel", `MATCH (h:H)-[r:K]-(p:P) RETURN r AS rr`, true, true},
	{"bare_rel_out", `MATCH (h:H)-[r:K]->(p:P) RETURN r AS rr`, false, false},
}

// BenchmarkRelStoredDir is the A/B vehicle, and it is ONE BINARY running BOTH
// arms: RELDIR_ARM=ladder sets [relDirPlanDisabled] and anything else leaves the
// plan-time path on. Selecting the arm at run time rather than at build time is
// what keeps the comparison from being an artefact of two different binaries'
// code layout, which this project has measured at several percent on
// expansion benchmarks.
//
// Each sub-benchmark VERIFIES, before the timer starts, that the shape actually
// takes an orientation decision per emitted row and that it takes it on the arm
// in force. A benchmark that measured a shape where no decision is taken would
// report a clean "no difference" and prove nothing; that check is the only
// reason the result can be read as being about this change at all.
func BenchmarkRelStoredDir(b *testing.B) {
	// Three arms, one binary:
	//   plan   — both halves of rmp #2864 on (the production setting)
	//   ladder — both halves off: every orientation on the per-row ladder (HEAD)
	//   nocol  — the plan-time half on, the undirected COLUMN off, so the
	//            undirected shape alone moves and the column can be priced
	//            without the directed change confounding it.
	arm := os.Getenv("RELDIR_ARM")
	if arm == "" {
		arm = "plan"
	}
	relDirPlanDisabled.Store(arm == "ladder")
	relDirColDisabled.Store(arm == "ladder" || arm == "nocol")
	defer relDirPlanDisabled.Store(false)
	defer relDirColDisabled.Store(false)

	for _, tc := range relDirBenchQueries {
		g := relDirBenchFixture(2000, tc.reciprocal)
		b.Run(tc.name, func(b *testing.B) {
			eng := NewEngine(g)
			ctx := context.Background()

			// Arm the counters for ONE untimed run: it warms the plan cache and
			// the CSR pair, and it is the oracle for "this shape decides an
			// orientation, on this arm".
			relDirPlanCount.Store(0)
			relDirColumnCount.Store(0)
			relDirLadderCount.Store(0)
			relDirCountersOn.Store(true)
			rows := 0
			res, err := eng.Run(ctx, tc.q, nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
				rows++
			}
			if err := res.Err(); err != nil {
				b.Fatal(err)
			}
			res.Close()
			relDirCountersOn.Store(false)
			arms := relDirArms{
				plan:   relDirPlanCount.Load(),
				column: relDirColumnCount.Load(),
				ladder: relDirLadderCount.Load(),
			}
			if arms.total() == 0 {
				b.Fatalf("%s takes NO orientation decision (rows=%d): this shape "+
					"cannot measure the change", tc.name, rows)
			}
			// Which arm MUST answer, per shape and per arm in force. Asserting
			// it is what makes the number about the change rather than about
			// whatever the binary happened to do.
			want := "plan"
			switch {
			case arm == "ladder":
				want = "ladder"
			case tc.undirected && arm == "nocol":
				want = "ladder"
			case tc.undirected:
				want = "column"
			}
			counted := map[string]uint64{
				"plan": arms.plan, "column": arms.column, "ladder": arms.ladder,
			}
			if counted[want] != arms.total() {
				b.Fatalf("%s arm=%s: want every decision on the %s arm, got %s",
					tc.name, arm, want, arms)
			}
			b.Logf("arm=%s rows=%d %s", arm, rows, arms)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := eng.Run(ctx, tc.q, nil)
				if err != nil {
					b.Fatal(err)
				}
				for res.Next() {
					_ = res.ValueAt(0)
				}
				if err := res.Err(); err != nil {
					b.Fatal(err)
				}
				res.Close()
			}
		})
	}
}

var _ = expr.Null // keep the expr import honest for future row assertions
