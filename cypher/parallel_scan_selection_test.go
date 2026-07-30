package cypher

// parallel_scan_selection_test.go — regression gate for rmp #2257.
//
// # The defect
//
// [tryBuildParallelScanProject] fused `Projection(scalar) → [Selection] → scan`
// into a morsel-parallel ParallelScanProject. It screened the PROJECTION items
// with exprHasNonScalar but never screened the optional Selection's predicate,
// and [buildOpts.forWorker] copies subEval and patEval BY POINTER.
//
// So `WHERE COUNT { … }` handed one SubqueryEvaluator to N worker goroutines.
// Both that evaluator and the pattern evaluator document themselves as not safe
// for concurrent use, and both memoise into plain maps at eval time.
//
// Measured on a DEFAULT engine (no options) over 60 000 nodes, which clears
// DefaultParallelScanThreshold = 50 000:
//
//	go run -race  → 185 data races, then panic: index out of range [1] with length 0
//	go run        → 5 of 5 runs dead: 4x "fatal error: concurrent map writes",
//	                1x nil pointer dereference
//
// `fatal error: concurrent map writes` is a runtime throw that recover cannot
// catch, so an ordinary Cypher query took down the host process. That is why the
// fix declines the fusion rather than trying to contain the panic.
//
// # Why the green suite was blind to it
//
// Every delivered test for these shapes projects an AGGREGATE (`RETURN count(a)`),
// which routes to ParallelAggregateScan and never builds the fused
// projection subtree. Tripping it needs a SCALAR projection — `RETURN a.id` —
// together with a subquery in WHERE. Scaling the existing fixtures up would not
// have found it.
//
// # Why these assertions are white-box
//
// Asserting "no crash" would be a probabilistic gate: whether N workers actually
// collide is timing-dependent, so a passing run would prove nothing. The plan
// assertions below are deterministic — they pin the screen itself. The paired
// positive case is what stops the gate from passing vacuously if the fused shape
// were ever disabled outright.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// parallelSelectionNodes is comfortably more than one DefaultMorselSize (1024),
// so the fused shape really does split across several workers rather than
// degenerating to a single morsel. The engine below lowers the row threshold so
// this stays a fast unit test instead of needing the 50 000-node default.
const parallelSelectionNodes = 4096

// parallelSelectionFixture builds parallelSelectionNodes :P nodes, every third
// also :Q, each with two handle-carrying :K out-edges. Every node therefore has
// out-degree 2, which makes every count below hand-computable.
func parallelSelectionFixture(t *testing.T) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < parallelSelectionNodes; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%s, P): %v", k, err)
		}
		if i%3 == 0 {
			if err := g.SetNodeLabel(k, "Q"); err != nil {
				t.Fatalf("SetNodeLabel(%s, Q): %v", k, err)
			}
		}
	}
	for i := 0; i < parallelSelectionNodes; i++ {
		src := fmt.Sprintf("n%d", i)
		for j := 1; j <= 2; j++ {
			dst := fmt.Sprintf("n%d", (i+j)%parallelSelectionNodes)
			h, err := g.AddEdgeH(src, dst, 1.0)
			if err != nil {
				t.Fatalf("AddEdgeH(%s->%s): %v", src, dst, err)
			}
			g.SetEdgeLabelByHandle(src, dst, h, "K")
		}
	}
	// ParallelScanThreshold is lowered so the fused shape engages on 4096 rows.
	// Nothing else is overridden: the defect reproduced on a default engine, and
	// this only moves the row gate down so the gate can be a unit test.
	eng := NewEngineWithOptions(g, EngineOptions{ParallelScanThreshold: 16})
	return g, eng
}

// TestParallelScanSelection_SubqueryPredicateDeclinesFusion_2257 is the primary
// gate: a Selection predicate containing a subquery must NOT be fused into the
// morsel-parallel scan, because the workers would share one SubqueryEvaluator.
func TestParallelScanSelection_SubqueryPredicateDeclinesFusion_2257(t *testing.T) {
	t.Parallel()
	_, eng := parallelSelectionFixture(t)

	// Each of these predicates needs the shared SubqueryEvaluator or pattern
	// evaluator, so each must decline the fusion.
	for _, q := range []string{
		`MATCH (a:P) WHERE COUNT { (a)-[r:K]->(b) } > 0 RETURN a.id`,
		`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } > 0 RETURN a.id`,
		`MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN a.id`,
		`MATCH (a:P) WHERE EXISTS { (a)-[:K]->() } RETURN a.id`,
		`MATCH (a:P) WHERE size([ (a)-[:K]->(x) | 1 ]) > 0 RETURN a.id`,
	} {
		plan, err := eng.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", q, err)
		}
		if strings.Contains(plan, "ParallelScanProject") {
			t.Errorf("query %q was fused into ParallelScanProject; its predicate needs the "+
				"shared subquery/pattern evaluator, which forWorker copies by pointer, so N "+
				"workers would race on it and the process would die with a runtime throw.\nplan:\n%s",
				q, plan)
		}
	}
}

// TestParallelScanSelection_ScalarPredicateStillFuses_2257 is the paired
// positive case. Without it the gate above would keep passing if the fused shape
// were disabled altogether, which would hide a silent loss of parallelism.
func TestParallelScanSelection_ScalarPredicateStillFuses_2257(t *testing.T) {
	t.Parallel()
	_, eng := parallelSelectionFixture(t)

	const q = `MATCH (a:P) WHERE a.id IS NOT NULL RETURN a.id`
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain(%q): %v", q, err)
	}
	if !strings.Contains(plan, "ParallelScanProject") {
		t.Fatalf("a purely scalar Selection predicate no longer fuses into "+
			"ParallelScanProject, so the #2257 screen is over-broad and parallelism "+
			"was lost silently.\nplan:\n%s", plan)
	}
}

// TestParallelScanSelection_SubqueryPredicateAnswersCorrectly_2257 is the
// behavioural half: the declined queries must still return the right answer, on
// the serial path, without crashing. Run under -race this also exercises the
// path that produced 185 races before the fix.
func TestParallelScanSelection_SubqueryPredicateAnswersCorrectly_2257(t *testing.T) {
	t.Parallel()
	_, eng := parallelSelectionFixture(t)

	// Absolute oracles. Every node has exactly two :K out-edges, so every :P node
	// satisfies a positive degree predicate and none has out-degree above two.
	const all = parallelSelectionNodes

	// The ":Q far node" count is enumerated here rather than written as a literal.
	// Enumerating it is the honest thing to do: the tempting shortcut — "every node
	// has one :Q out-neighbour, so the answer is all of them" — is FALSE, and I
	// asserted it before checking. A node at i%3==0 has out-neighbours i+1 and i+2,
	// congruent to 1 and 2 mod 3, so NEITHER is :Q. This loop is still an absolute
	// oracle: it is plain Go over the fixture's own definition and shares no code
	// with the engine.
	isQ := func(j int) bool { return j%3 == 0 }
	wantQ := 0
	for i := 0; i < parallelSelectionNodes; i++ {
		if isQ((i+1)%parallelSelectionNodes) || isQ((i+2)%parallelSelectionNodes) {
			wantQ++
		}
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{`MATCH (a:P) WHERE COUNT { (a)-[r:K]->(b) } > 0 RETURN a.id`, all},
		{`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } > 0 RETURN a.id`, all},
		{`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } > 2 RETURN a.id`, 0},
		{`MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN a.id`, wantQ},
		{`MATCH (a:P) WHERE EXISTS { (a)-[:K]->() } RETURN a.id`, all},
	} {
		res, err := eng.RunAny(context.Background(), tc.query, nil)
		if err != nil {
			t.Fatalf("RunAny(%q): %v", tc.query, err)
		}
		rows := 0
		for res.Next() {
			rows++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("drain(%q): %v", tc.query, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("close(%q): %v", tc.query, err)
		}
		if rows != tc.want {
			t.Errorf("query %q returned %d rows, want %d (hand-computed)", tc.query, rows, tc.want)
		}
	}
}
