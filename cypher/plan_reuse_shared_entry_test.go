package cypher

// plan_reuse_shared_entry_test.go — rmp #2693.
//
// # What is under test, and what is NOT
//
// #2693 asked whether the physical operator tree could be reused across
// executions of one cached plan. It CANNOT, and the reason is recorded on
// [readBuildScaffold]: the tree binds this execution's [lpg.ReadView] (whose
// snapshot the reclamation horizon releases at EndRead), this execution's
// parameters (captured inside the compiled projection and filter closures) and
// this statement's frozen now(). What #2693 delivered instead is three
// reductions in what the REBUILD allocates, and every one of them makes a shared
// object cheaper or later:
//
//   - [planCacheEntry.containsWrite] — a new field on the entry that EVERY
//     concurrent execution of the query text reads;
//   - three lazily created memo maps on [subqueryEvaluator];
//   - [readBuildScaffold] — the five per-execution build objects in one heap
//     object.
//
// So the risk this file has to close is not "does a reused tree leak state"
// (nothing is reused) but "does making these objects cheaper leak state between
// concurrent executions of one cached plan". Three ways it could:
//
//  1. the entry memo could be WRONG, and a write query would then execute through
//     [Engine.Run], which has no transaction to record it in;
//  2. a lazily created map could be written through a nil reference, which is a
//     panic, or shared between goroutines, which is a `concurrent map writes`
//     runtime throw that recover cannot catch (the shape of rmp #2257);
//  3. the merged scaffold could mis-wire an evaluator, so a subquery or pattern
//     predicate silently answered false instead of being evaluated — the shape of
//     rmp #2507, which is invisible to a test that only checks for absence of
//     error.
//
// Each is pinned below, and each assertion is written so that reverting the
// corresponding change fails it. Run this package under -race: (2) is a race
// before it is a wrong answer.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// sharedEntryNodes is small on purpose: this file is about state leaking between
// executions, and a bigger population would only make each execution slower
// without making a leak more likely.
const sharedEntryNodes = 64

// newSharedEntryRig builds sharedEntryNodes :N nodes chained by :E edges, every
// node carrying an Int64 "v". The chain is what gives the degree and
// labelled-hop recognisers something to answer, and it leaves node
// sharedEntryNodes-1 with NO outgoing edge — so a predicate over the chain has a
// non-trivial answer and a test that accidentally matched everything (or
// nothing) is visible.
func newSharedEntryRig(tb testing.TB) *Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < sharedEntryNodes; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			tb.Fatalf("AddNode %s: %v", id, err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", id, err)
		}
		if err := g.SetNodeProperty(id, "v", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty %s: %v", id, err)
		}
	}
	for i := 0; i+1 < sharedEntryNodes; i++ {
		src, dst := fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1)
		if err := g.AddEdge(src, dst, 1); err != nil {
			tb.Fatalf("AddEdge %d: %v", i, err)
		}
		g.SetEdgeLabel(src, dst, "E")
	}
	return NewEngine(g)
}

// sharedEntryShapes are the read shapes this file drives. Between them they
// reach EVERY lazily created memo on the two evaluators, and the memo field
// records WHICH — but it records what was MEASURED, not what was predicted, and
// the two differed.
//
// The measurement is a mutation matrix: each of the four nil guards was deleted
// in turn and each shape run alone, so a shape that reaches a guard panics with
// "assignment to entry in nil map" and a shape that does not reaches nothing.
// Result (rmp #2693):
//
//	guard deleted                     | shape that panicked
//	----------------------------------+-------------------------------------
//	subqueryEvaluator.compiled        | COUNT subquery — and ONLY that one
//	subqueryEvaluator.degree          | COUNT subquery — and ONLY that one
//	subqueryEvaluator.labelledHop     | COUNT subquery — and ONLY that one
//	patternEvaluator.labelledHop      | inline pattern predicate — only that
//
// The prediction that an `EXISTS { … }` would reach subqueryEvaluator.degree
// was WRONG: neither EXISTS shape touches the evaluator at all on this fixture,
// because both are planned as an operator and answered without ever evaluating a
// subquery expression. They stay in the list as correctness coverage of that
// operator route — the route is not free of the containsWrite memo or the merged
// scaffold — but they are NOT what covers the lazy guards, and labelling them as
// though they were would leave three guards apparently checked and actually
// unchecked. Two shapes carry all four.
//
// want is the count the shape must return on newSharedEntryRig. The chain has
// sharedEntryNodes nodes and sharedEntryNodes-1 edges, so a predicate asking for
// an outgoing :E hop admits every node but the last.
var sharedEntryShapes = []struct {
	name  string
	query string
	memo  string
	want  int64
}{
	{
		name:  "plain label count",
		query: "MATCH (n:N) RETURN count(n) AS c",
		memo:  "none — the baseline, whose plan is Project ← LabelCountScan",
		want:  sharedEntryNodes,
	},
	{
		name:  "EXISTS over an unlabelled hop",
		query: "MATCH (n:N) WHERE EXISTS { MATCH (n)-[:E]->(m) } RETURN count(n) AS c",
		memo:  "none — measured: answered by an operator, the evaluator is never called",
		want:  sharedEntryNodes - 1,
	},
	{
		name:  "EXISTS over a LABELLED hop",
		query: "MATCH (n:N) WHERE EXISTS { MATCH (n)-[:E]->(m:N) } RETURN count(n) AS c",
		memo:  "none — measured: answered by an operator, the evaluator is never called",
		want:  sharedEntryNodes - 1,
	},
	{
		name:  "COUNT subquery compared to a bound the adjacency cannot answer",
		query: "MATCH (n:N) WHERE COUNT { MATCH (n)-[:E]->(m) WHERE m.v > 0 } > 0 RETURN count(n) AS c",
		memo:  "subqueryEvaluator.compiled AND .degree AND .labelledHop (all three)",
		want:  sharedEntryNodes - 1,
	},
	{
		name:  "inline pattern predicate",
		query: "MATCH (n:N) WHERE (n)-[:E]->(:N) RETURN count(n) AS c",
		memo:  "patternEvaluator.labelledHop",
		want:  sharedEntryNodes - 1,
	},
}

// runSharedEntryShape executes one shape and returns its single count.
func runSharedEntryShape(ctx context.Context, eng *Engine, query string) (int64, error) {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer res.Close()
	var (
		got  int64
		rows int
	)
	for res.Next() {
		rows++
		v, ok := res.Record()["c"].(expr.IntegerValue)
		if !ok {
			return 0, fmt.Errorf("column c is %T, not an expr.IntegerValue", res.Record()["c"])
		}
		got = int64(v)
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, fmt.Errorf("got %d rows, want exactly 1", rows)
	}
	return got, nil
}

// TestSharedEntryShapesAnswerCorrectlySerially is the precondition for the
// concurrent test below: if a shape's expected count were wrong, or if a shape
// stopped reaching the memo it names, the concurrent test would agree with a
// wrong answer and prove nothing.
func TestSharedEntryShapesAnswerCorrectlySerially(t *testing.T) {
	eng := newSharedEntryRig(t)
	ctx := context.Background()
	for _, sh := range sharedEntryShapes {
		t.Run(sh.name, func(t *testing.T) {
			got, err := runSharedEntryShape(ctx, eng, sh.query)
			if err != nil {
				t.Fatalf("%s (memo: %s): %v", sh.query, sh.memo, err)
			}
			if got != sh.want {
				t.Fatalf("%s (memo: %s): count = %d, want %d",
					sh.query, sh.memo, got, sh.want)
			}
		})
	}
}

// TestSharedEntryConcurrentExecutionOfOneCachedPlan drives every shape from many
// goroutines on ONE engine, so each shape's [planCacheEntry] — including its
// containsWrite memo and its scalarUse table — is shared by every execution of
// it, and each execution builds its own scaffold and its own evaluators.
//
// It is deliberately run with the shapes INTERLEAVED across goroutines rather
// than one shape per goroutine: a per-goroutine shape would let a leak between
// two executions of the same plan hide behind the fact that only one goroutine
// ever built that plan.
//
// Under -race this is also the gate on the lazy memo maps: an evaluator shared
// between two executions would show up as a data race on the map header long
// before it showed up as a wrong count.
func TestSharedEntryConcurrentExecutionOfOneCachedPlan(t *testing.T) {
	eng := newSharedEntryRig(t)
	ctx := context.Background()

	// Warm every entry FIRST, on one goroutine, so the run below exercises the
	// cache-HIT path — which is the path #2693 is about. Without this the first
	// execution of each shape would be a miss and the test would be measuring
	// entry construction as well.
	for _, sh := range sharedEntryShapes {
		got, err := runSharedEntryShape(ctx, eng, sh.query)
		if err != nil {
			t.Fatalf("warm %s: %v", sh.query, err)
		}
		if got != sh.want {
			t.Fatalf("warm %s: count = %d, want %d", sh.query, got, sh.want)
		}
	}

	const (
		goroutines = 16
		iterations = 40
	)
	var (
		wg   sync.WaitGroup
		errs = make(chan error, goroutines*iterations)
	)
	for w := 0; w < goroutines; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				sh := sharedEntryShapes[(w+i)%len(sharedEntryShapes)]
				got, err := runSharedEntryShape(ctx, eng, sh.query)
				if err != nil {
					errs <- fmt.Errorf("worker %d iter %d %q: %w", w, i, sh.query, err)
					continue
				}
				if got != sh.want {
					errs <- fmt.Errorf("worker %d iter %d %q (memo: %s): count = %d, want %d",
						w, i, sh.query, sh.memo, got, sh.want)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	var n int
	for err := range errs {
		n++
		if n <= 10 {
			t.Errorf("%v", err)
		}
	}
	if n > 10 {
		t.Errorf("... and %d further failures", n-10)
	}
	if n > 0 {
		t.Fatalf("%d of %d concurrent executions of a shared cached plan were wrong or "+
			"failed", n, goroutines*iterations)
	}
}

// TestSharedEntryContainsWriteMemoRejectsEveryWriteShape pins the memo #2693
// added to [planCacheEntry]. It is the assertion that fails if the memo is
// computed wrongly, computed on the wrong plan, or skipped for some class of
// entry: every one of these must be refused by [Engine.Run], because Run has no
// transaction to record a mutation in.
//
// The read control at the end is what stops the test passing vacuously by
// refusing everything.
func TestSharedEntryContainsWriteMemoRejectsEveryWriteShape(t *testing.T) {
	eng := newSharedEntryRig(t)
	ctx := context.Background()
	writes := []string{
		"CREATE (n:W {id: 1})",
		"MATCH (n:N) SET n.v = 1",
		"MATCH (n:N) REMOVE n.v",
		"MATCH (n:N) SET n:Extra",
		"MATCH (n:N) REMOVE n:N",
		"MERGE (n:W {id: 2})",
		"MATCH (n:N) WITH n LIMIT 1 DETACH DELETE n",
		"MATCH (n:N) WITH n LIMIT 1 CREATE (n)-[:E2]->(:W)",
		"MATCH (n:N) FOREACH (x IN [1] | SET n.v = x)",
		// A write buried UNDER a reading clause, which is the shape the structural
		// check exists for: the query text starts with MATCH and only the plan says
		// it writes.
		"MATCH (n:N) WHERE n.v = 0 WITH n ORDER BY n.v SET n.v = 99 RETURN n.v AS c",
	}
	for _, q := range writes {
		t.Run(q, func(t *testing.T) {
			res, err := eng.Run(ctx, q, nil)
			if res != nil {
				res.Close()
			}
			if err == nil {
				t.Fatalf("Run executed a WRITE query: %q. entry.containsWrite must be "+
					"true for this plan", q)
			}
			if !errors.Is(err, ErrWriteInReadOnlyTx) {
				t.Fatalf("Run(%q) failed with %v, want ErrWriteInReadOnlyTx — a different "+
					"error means the query was refused somewhere else and this case no "+
					"longer tests the memo", q, err)
			}
		})
	}
	// The control: a read must still be admitted, twice, so the second execution
	// reads the memo off a cache HIT rather than recomputing it.
	for i := 0; i < 2; i++ {
		got, err := runSharedEntryShape(ctx, eng, "MATCH (n:N) RETURN count(n) AS c")
		if err != nil {
			t.Fatalf("read control run %d: %v", i, err)
		}
		if got != sharedEntryNodes {
			t.Fatalf("read control run %d: count = %d, want %d", i, got, sharedEntryNodes)
		}
	}
}

// TestSubqueryEvaluatorMemosAreLazy pins the lazy contract directly, so a change
// that restored eager construction fails here with a clear reason rather than
// silently costing three allocations per read again.
//
// It also pins the half that matters for correctness: a nil memo must still be
// READ safely, which is what makes the lazy shape legal at all.
func TestSubqueryEvaluatorMemosAreLazy(t *testing.T) {
	eng := newSharedEntryRig(t)
	snap := eng.g.BeginRead()
	defer eng.g.EndRead(snap)
	rv := eng.g.ReadAt(snap)
	walker := &lpgNodeWalker{g: rv}
	labelSrc := &lpgLabelResolver{g: rv, eng: eng}
	e := newSubqueryEvaluator(walker, labelSrc, newNowAwareRegistry(eng.reg, time.Now()), rv)

	if e.compiled != nil {
		t.Errorf("compiled map was allocated at construction; it must be created on "+
			"first write (got %v)", e.compiled)
	}
	if e.degree != nil {
		t.Errorf("degree map was allocated at construction; it must be created on "+
			"first write (got %v)", e.degree)
	}
	if e.labelledHop != nil {
		t.Errorf("labelledHop map was allocated at construction; it must be created on "+
			"first write (got %v)", e.labelledHop)
	}
	// Reading a nil memo must answer "not memoised", not panic. This is the
	// property the whole lazy shape rests on.
	var probe ast.Expression = &ast.ExistsSubquery{}
	if sh, seen := e.degree[probe]; seen || sh != nil {
		t.Errorf("a nil degree memo reported a hit (%v, %v)", sh, seen)
	}
	if sh, seen := e.labelledHop[probe]; seen || sh != nil {
		t.Errorf("a nil labelledHop memo reported a hit (%v, %v)", sh, seen)
	}
	if cs, seen := e.compiled[probe]; seen || cs != nil {
		t.Errorf("a nil compiled memo reported a hit (%v, %v)", cs, seen)
	}
}

// TestReadBuildScaffoldWiring pins the wiring the merge could get wrong. A
// scaffold that handed out a pointer to the wrong embedded field, or left an
// evaluator off the buildOpts, would produce a build in which every subquery and
// every pattern predicate answered from a zero value — which is a WRONG ANSWER,
// not an error, and is exactly the class of defect rmp #2507 was.
//
// The identity assertions are what make this a wiring test rather than a
// smoke test: they prove the five objects handed to the build are the five
// EMBEDDED in the single allocation, not fresh ones beside it.
func TestReadBuildScaffoldWiring(t *testing.T) {
	eng := newSharedEntryRig(t)
	snap := eng.g.BeginRead()
	defer eng.g.EndRead(snap)
	rv := eng.g.ReadAt(snap)
	ctx := context.Background()

	var sc readBuildScaffold
	walker, labelSrc, subEval, patEval, bopts := (&sc).init(
		ctx, eng, rv, newNowAwareRegistry(eng.reg, time.Now()))

	if walker != &sc.walker || labelSrc != &sc.labelSrc ||
		subEval != &sc.subEval || patEval != &sc.patEval || bopts != &sc.bopts {
		t.Fatalf("init returned pointers that are not the scaffold's own embedded "+
			"fields: walker=%p/%p labelSrc=%p/%p subEval=%p/%p patEval=%p/%p bopts=%p/%p",
			walker, &sc.walker, labelSrc, &sc.labelSrc, subEval, &sc.subEval,
			patEval, &sc.patEval, bopts, &sc.bopts)
	}
	if bopts.subEval != subEval {
		t.Errorf("buildOpts.subEval = %p, want the scaffold's own %p — a build with the "+
			"wrong evaluator answers every EXISTS/COUNT from a zero value",
			bopts.subEval, subEval)
	}
	if bopts.patEval != patEval {
		t.Errorf("buildOpts.patEval = %p, want the scaffold's own %p", bopts.patEval, patEval)
	}
	if bopts.queryCtx != ctx {
		t.Errorf("buildOpts.queryCtx was not threaded; a subquery drive would not observe " +
			"cancellation")
	}
	if walker.g != rv || labelSrc.g != rv || subEval.g != rv || patEval.g != rv {
		t.Errorf("not every scaffold member was bound to the execution's read view: "+
			"walker=%p labelSrc=%p subEval=%p patEval=%p rv=%p",
			walker.g, labelSrc.g, subEval.g, patEval.g, rv)
	}
	if labelSrc.eng != eng {
		t.Errorf("labelResolver was not bound to the engine")
	}
	if subEval.walker != walker || subEval.labels != labelSrc {
		t.Errorf("the subquery evaluator was not bound to the scaffold's walker/resolver")
	}
	if bopts.maxCollectItems != eng.maxCollectItems {
		t.Errorf("buildOpts.maxCollectItems = %d, want the Engine's %d",
			bopts.maxCollectItems, eng.maxCollectItems)
	}
}

// TestCountVarRewriteMemoIsConsultedAndAgrees pins the memo rmp #2693 put in
// front of [rewriteCountVarToCountStar].
//
// Two things have to hold, and a test that checked only one would be worthless:
//
//   - the memo must be CONSULTED — a memo that is populated and never read costs
//     memory and saves nothing, and is invisible to any result-identical check;
//   - the memoised answer must EQUAL the direct one, because the rewrite decides
//     whether count(v) is normalised to count(*), and normalising one that is
//     nullable would change the count the query returns.
//
// The equality half is checked against the direct function on the very node the
// build reaches, so it compares the two answers rather than trusting that a pure
// function is pure.
func TestCountVarRewriteMemoIsConsultedAndAgrees(t *testing.T) {
	eng := newSharedEntryRig(t)
	ctx := context.Background()
	// count(n) over a labelled scan: the count(<bare pattern-bound variable>)
	// spelling the rewrite exists for. A count(*) query would leave the rewrite
	// inert and the test would pass without ever storing anything.
	const q = "MATCH (n:N) RETURN count(n) AS c"

	entry, _, err := eng.parseAndAnalyse(q)
	if err != nil {
		t.Fatalf("parseAndAnalyse: %v", err)
	}

	const executions = 5
	for i := 0; i < executions; i++ {
		got, rerr := runSharedEntryShape(ctx, eng, q)
		if rerr != nil {
			t.Fatalf("execution %d: %v", i, rerr)
		}
		if got != sharedEntryNodes {
			t.Fatalf("execution %d: count = %d, want %d", i, got, sharedEntryNodes)
		}
	}

	misses := entry.countVarRewrite.misses.Load()
	hits := entry.countVarRewrite.hits.Load()
	stored := entry.countVarRewrite.entries.Load()
	if misses == 0 && hits == 0 {
		t.Fatalf("the count-var memo was never consulted across %d executions of %q, so "+
			"the routing in buildOperatorRec is not reached and the memo saves nothing",
			executions, q)
	}
	if hits == 0 {
		t.Errorf("the memo was consulted %d times but never HIT across %d executions of "+
			"one cached plan: every execution recomputed the rewrite, which is what the "+
			"memo exists to stop (stored=%d)", misses, executions, stored)
	}
	if stored == 0 {
		t.Errorf("the memo stored nothing (hits=%d misses=%d); a memo that answers "+
			"without storing is the ceiling path, not the intended one", hits, misses)
	}

	// The agreement half: reach the aggregation node the build reaches and compare
	// the memo's answer with the direct rewrite's, field by field on the aggregate
	// list — which is the only thing the rewrite changes.
	agg := findEagerAggregation(entry.plan)
	if agg == nil {
		t.Fatalf("no ir.EagerAggregation in the plan for %q, so this test is not "+
			"reaching the rewrite at all", q)
	}
	direct := rewriteCountVarToCountStar(agg)
	memoised := entry.countVarRewrite.get(agg)
	if len(direct.Aggregates) != len(memoised.Aggregates) {
		t.Fatalf("memoised rewrite has %d aggregates, direct has %d",
			len(memoised.Aggregates), len(direct.Aggregates))
	}
	for i := range direct.Aggregates {
		d, m := direct.Aggregates[i], memoised.Aggregates[i]
		if d.Function != m.Function || d.OutputName != m.OutputName || d.Distinct != m.Distinct {
			t.Errorf("aggregate %d: memoised {%q %q %v} != direct {%q %q %v} — the memo "+
				"changed the aggregate the build sees",
				i, m.Function, m.OutputName, m.Distinct, d.Function, d.OutputName, d.Distinct)
		}
		// Argument is the field the rewrite edits: count(v) carries the variable
		// name, count(*) carries the empty string. A divergence here changes the
		// integer the query returns, because count(v) counts non-null bindings and
		// count(*) counts rows.
		if d.Argument != m.Argument {
			t.Errorf("aggregate %d: memoised Argument = %q, direct = %q",
				i, m.Argument, d.Argument)
		}
		if (d.ArgumentExpr == nil) != (m.ArgumentExpr == nil) {
			t.Errorf("aggregate %d: memoised ArgumentExpr nil=%v, direct nil=%v",
				i, m.ArgumentExpr == nil, d.ArgumentExpr == nil)
		}
	}
	// The rewrite must actually have FIRED on this shape, or the agreement check
	// above compared two copies of an untouched node and proved nothing.
	if len(agg.Aggregates) > 0 && agg.Aggregates[0].Argument == "" {
		t.Fatalf("the plan's aggregate already spells count(*), so this fixture never " +
			"exercises the rewrite — fix the query, not the assertion")
	}
	if len(memoised.Aggregates) > 0 && memoised.Aggregates[0].Argument != "" {
		t.Fatalf("the rewrite did not fire: aggregate argument is still %q after the "+
			"memoised rewrite, so nothing was normalised to count(*)",
			memoised.Aggregates[0].Argument)
	}
}

// findEagerAggregation returns the first [ir.EagerAggregation] in p, or nil.
func findEagerAggregation(p ir.LogicalPlan) *ir.EagerAggregation {
	if p == nil {
		return nil
	}
	if agg, ok := p.(*ir.EagerAggregation); ok {
		return agg
	}
	for _, ch := range p.Children() {
		if got := findEagerAggregation(ch); got != nil {
			return got
		}
	}
	return nil
}

// readPathAllocCeiling is the number of heap allocations one cache-HIT execution
// of [readPathAllocQuery] is permitted to make, end to end through the public
// [Engine.Run] API and a full drain.
//
// It is a STRUCTURAL number, not a proportional one: the query is fixed, its plan
// is fixed at two operators, and it returns exactly one row, so the count does
// not depend on the population, the machine, or the duration of the run. That is
// what makes it a gate rather than a rate.
//
// The value is what rmp #2693 left, measured on this host through this very
// test. Two figures were measured and they differ by one allocation, so which one
// this constant is matters:
//
//	                            | before #2693 | after #2693
//	public Engine.Run + drain   |      32      |     20      <- this gate
//	BenchmarkPlanReusePhases    |      33      |     21
//	/4full (the transcription)  |              |
//
// The benchmark's transcription drains through its own loop rather than
// [Result.Record], which accounts for the one-allocation difference. The gate
// measures the PUBLIC path, so the constant is the public path's number; the
// benchmark's is quoted here only so the two are never mistaken for each other.
// The reduction is -37.5% through the public API and -36.4% in the benchmark
// (p=0.002, n=6, benchstat, GOMAXPROCS 1 and 4 alike).
//
// The ceiling is set AT the measured value with no headroom, on purpose. Headroom
// is what lets a regression land unnoticed, and the number is deterministic —
// benchstat reported ± 0% across six replicates. It also has teeth, which was
// checked rather than assumed: run against the tree as it stood before #2693
// (HEAD 0943eb33) this same measurement reads 32.00, so the ceiling fails there
// by 12. If a legitimate change adds an allocation, the honest move is to raise
// this constant in the same commit and say why, not to pad it in advance.
//
// The 20 that remain break down as (from an -memprofilerate=1 profile, rmp
// #2693): 13 in the physical build, 1 for the per-statement now-aware registry,
// 1 for the MVCC snapshot, the rest in the drain. Six of the 13 are the operator
// objects and their projection items themselves, which only reusing the physical
// tree could remove — and that is refuted; see [readBuildScaffold].
const readPathAllocCeiling = 20

// readPathAllocQuery is the query the ceiling is measured on: bench/contention's
// cypher-read-label-small, which is the workload every number in rmp #2693 was
// gathered on. Its plan is "Project ← LabelCountScan", so the count comes from
// the count store in O(1) and essentially the whole cost of the read is
// per-execution overhead — which is exactly why it is the right gate.
const readPathAllocQuery = "MATCH (n:N) RETURN count(n) AS c"

// readPathAllocChildEnv makes this test binary re-enter as a MEASUREMENT-ONLY
// child. When it is set, TestReadPathAllocationCeiling measures and prints;
// when it is not, the test re-execs itself with it set and asserts on what the
// child printed.
const readPathAllocChildEnv = "GOGRAPH_READPATH_ALLOC_CHILD"

// readPathAllocMarker is how the child reports its measurement to the parent.
const readPathAllocMarker = "READPATH_ALLOCS="

var readPathAllocRE = regexp.MustCompile(readPathAllocMarker + `([0-9.]+)`)

// TestReadPathAllocationCeiling is the regression gate for rmp #2693.
//
// It measures through the PUBLIC API, not through the benchmark's transcription
// of the read path, so a change that moved an allocation from the build into Run
// itself cannot hide from it.
//
// # Why this re-execs itself instead of just measuring (rmp #2753)
//
// [testing.AllocsPerRun] divides runtime.MemStats.Mallocs, which is
// PROCESS-GLOBAL. Every goroutine alive anywhere in this test binary during the
// measurement window is counted here too — including the sibling tests that
// t.Parallel() and GOGRAPH_PARALLEL_SUITE leave running. That is not a
// hypothetical: under `make ci` this gate read 32.00 against a ceiling of 20
// while the read path was untouched, and it did so in BOTH gate phases, stably,
// which is what ruled out ordinary measurement noise.
//
// The cause was established by a controlled 2x2 rather than argued, and BOTH
// factors are necessary — neither alone reproduces it:
//
//	in-binary concurrency | external load | reading
//	no                    | no            |  20.00
//	no                    | yes (load 11) |  20.00
//	yes                   | no            |  20.00
//	yes                   | yes (load 37) |  32.00   <- the gate's condition
//
// Sibling goroutines are the contaminator; external load is the amplifier,
// because it stretches the window in wall-clock so those siblings get more
// iterations inside it. Under the gate the cypher package took 199.4s against
// ~106s alone.
//
// Five earlier hypotheses were refuted with measurements (plain build, -race,
// -cover, GOGC=off/400/1, and a min-of-five estimator); they are recorded on
// rmp #2753. A sixth — that external load alone sufficed — was refuted here:
// row 2 above tests exactly that and reads 20.00.
//
// So the remedy is not a better estimator on a contaminated process; it is a
// process with nothing to contaminate it. The child runs THIS test and nothing
// else, so no sibling test goroutine exists. Raising the ceiling to 32 was
// rejected outright: the isolated reading is exactly 20, so a raised ceiling
// would blind the gate to a genuine 12-allocation regression.
func TestReadPathAllocationCeiling(t *testing.T) {
	if os.Getenv(readPathAllocChildEnv) == "1" {
		runReadPathAllocChild(t)
		return
	}

	// os.Args[0] is this test binary's own path, not user-supplied input.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestReadPathAllocationCeiling$", "-test.v") //nolint:gosec // G204: os.Args[0] is the test binary itself
	cmd.Env = append(os.Environ(), readPathAllocChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("measurement child failed: %v\n%s", err, out)
	}
	m := readPathAllocRE.FindSubmatch(out)
	if m == nil {
		// An absent marker is a HARD failure, never a pass. A gate that cannot
		// find its own measurement has stopped measuring, and silence must not
		// read as success.
		t.Fatalf("measurement child printed no %s marker; the gate did not measure anything\n%s",
			readPathAllocMarker, out)
	}
	allocs, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		t.Fatalf("unparseable marker %q: %v", m[1], err)
	}

	if allocs > float64(readPathAllocCeiling) {
		t.Errorf("one cache-hit execution of %q allocated %.1f objects, ceiling %d "+
			"(rmp #2693). An allocation was added to the read path; either remove it or "+
			"raise readPathAllocCeiling in the same change and record why.",
			readPathAllocQuery, allocs, readPathAllocCeiling)
	}
	// The lower bound is not belt-and-braces: if the count falls well below the
	// ceiling, the gate has silently stopped measuring the path it was written for
	// — a query rewritten to a cheaper plan, or a drain that stopped draining —
	// and a ceiling nobody can reach cannot fail. Raise the constant deliberately
	// instead of leaving a gate that passes for the wrong reason.
	if allocs < float64(readPathAllocCeiling)-3 {
		t.Errorf("one cache-hit execution of %q allocated only %.1f objects against a "+
			"ceiling of %d. That is a win, but it means this gate is no longer measuring "+
			"the read path it was calibrated on: lower readPathAllocCeiling to the new "+
			"measured value so it keeps its teeth.",
			readPathAllocQuery, allocs, readPathAllocCeiling)
	}
	t.Logf("cache-hit read of %q: %.2f allocs/op (ceiling %d), measured in an isolated child",
		readPathAllocQuery, allocs, readPathAllocCeiling)
}

// runReadPathAllocChild performs the measurement and prints it. It deliberately
// does NOT assert: the parent owns the verdict, so a child that somehow measured
// under contamination reports its number and lets the parent judge it, rather
// than failing in a place whose output nobody reads.
func runReadPathAllocChild(t *testing.T) {
	t.Helper()
	eng := newSharedEntryRig(t)
	ctx := context.Background()

	// Warm the plan cache and every lazily-initialised structure the engine
	// carries, so the measured runs are cache HITS. Without this the first
	// iteration would include parsing, planning and entry construction, and the
	// average would describe a path production never repeats.
	for i := 0; i < 20; i++ {
		got, err := runSharedEntryShape(ctx, eng, readPathAllocQuery)
		if err != nil {
			t.Fatalf("warm %d: %v", i, err)
		}
		if got != sharedEntryNodes {
			t.Fatalf("warm %d: count = %d, want %d", i, got, sharedEntryNodes)
		}
	}

	var (
		observed int64
		failures int
	)
	// MINIMUM of several samples. Contamination can only ADD allocations, never
	// remove them, so the minimum is the robust estimator. In this child there
	// should be nothing left to contaminate it — the minimum is kept as defence
	// in depth, not as the fix, because on its own it was measured NOT to be
	// enough (rmp #2753).
	const allocSamples = 5
	allocs := math.Inf(1)
	for i := 0; i < allocSamples; i++ {
		sample := testing.AllocsPerRun(200, func() {
			got, err := runSharedEntryShape(ctx, eng, readPathAllocQuery)
			if err != nil {
				failures++
				return
			}
			observed = got
		})
		allocs = math.Min(allocs, sample)
	}
	// The oracle: prove the work actually happened. An allocation count measured
	// over a function that errored out immediately would be beautifully low and
	// entirely meaningless.
	if failures != 0 {
		t.Fatalf("%d of the measured runs failed, so the allocation count describes an "+
			"error path", failures)
	}
	if observed != sharedEntryNodes {
		t.Fatalf("the measured runs returned count = %d, want %d — the allocation count "+
			"describes the wrong query", observed, sharedEntryNodes)
	}
	fmt.Printf("%s%.2f\n", readPathAllocMarker, allocs)
}
