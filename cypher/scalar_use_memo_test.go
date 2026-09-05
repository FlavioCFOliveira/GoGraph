package cypher

// scalar_use_memo_test.go — tests for the cross-execution memo of
// analyseNodeScalarUse (rmp #2383).
//
// The memo shares one analysis across every execution of a cached plan, where
// each execution previously built its own. That turns two properties into
// correctness requirements, and each has a test below:
//
//  1. The memoised analysis must equal the one the unmemoised code would have
//     produced — otherwise the optimisation changes results.
//  2. No consumer may MUTATE it. Before the memo a mutation was invisible,
//     because the mutated map died with the execution that made it; now it would
//     corrupt every later execution of the same query and race with concurrent
//     ones.
//
// Both are checked by the same oracle, and it is deliberately an ABSOLUTE one
// rather than a differential: after the query has run many times, recompute the
// analysis FRESH for every expression in the memo and require deep equality. A
// differential between two engines would go green if both shared the same broken
// value. reflect.DeepEqual is used rather than a hand-written field comparison
// precisely so a field added to nodeScalarUse later cannot silently escape the
// check.

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// memoQueries covers the three call sites that consult the memo and the gates
// each applies: the row predicate (newRowPredicate), the scalar-projection
// filler, and the captured-expression builder. The shapes are chosen so the
// analysis lands in materially different states — presence-only keys, a whole-node
// use that must bail, the field-extractor flags, and a relationship variable.
var memoQueries = []string{
	`MATCH (n:Person) WHERE n.age > 30 RETURN n.name AS name`,
	`MATCH (n:Person) WHERE n.nick IS NULL RETURN n.name AS name`,
	`MATCH (n:Person) WHERE n.age > 10 AND n.name <> 'x' RETURN n.age + 1 AS a`,
	// A predicate that IS analysed above a projection that returns the whole node,
	// so the analysis is memoised and its consumer still takes the eager path.
	`MATCH (n:Person) WHERE n.age > 30 RETURN n`,
	`MATCH (n:Person) RETURN id(n) AS i, labels(n) AS l`,
	// A pattern comprehension is outside the analysis's allowlist, so this shape
	// memoises a BAILOUT — the state the two nulling call sites gate on. It has to
	// sit inside the PREDICATE: a comprehension in the projection list never
	// reaches analyseNodeScalarUse at all, so putting it there tests nothing (which
	// is what TestNodeScalarUseMemoObservesBothBailoutStates caught).
	`MATCH (n:Person) WHERE size([(n)-[:KNOWS]->(m) | m.name]) > 0 RETURN n.name AS name`,
	`MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN r.since AS s, a.name AS an`,
	`MATCH (a:Person)-[r:KNOWS]->(b:Person) WHERE r.since IS NOT NULL RETURN b.name AS bn`,
}

// buildMemoGraph seeds a graph through the write path so the queries above have
// something to match and the count store is populated as production maintains it.
// planCacheKeyFor returns the key the engine actually files a query under.
//
// A query that inlines a hoistable string literal is cached under its REWRITTEN
// text, not its original (rmp #2412), so a white-box lookup by the query as
// written finds nothing. Deriving the key the same way the engine does keeps
// these tests asserting what they were written to assert — that the memo is
// consulted — instead of accidentally asserting how the cache is keyed.
func planCacheKeyFor(q string) string {
	if stripped, _, ok := parser.StripLiterals(q); ok {
		return stripped
	}
	return q
}

func buildMemoGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	seed := NewEngine(g)
	runWrite(t, seed, `CREATE (a:Person {name:'a', age: 41, nick:'ax'}),
	                         (b:Person {name:'b', age: 22}),
	                         (c:Person {name:'c', age: 35, nick:'cx'})`)
	runWrite(t, seed, `MATCH (a:Person {name:'a'}), (b:Person {name:'b'})
	                   CREATE (a)-[:KNOWS {since: 2020}]->(b)`)
	runWrite(t, seed, `MATCH (b:Person {name:'b'}), (c:Person {name:'c'})
	                   CREATE (b)-[:KNOWS]->(c)`)
	return g
}

// TestNodeScalarUseMemoIsConsulted proves the memo is READ, not merely populated.
// A memo that is filled and then ignored is invisible to any result-level test —
// the results would be identical and the allocation win simply absent — so the
// hit counter is the only thing that can distinguish the two.
//
// It also pins the property that makes the lazy fill sound: because every
// execution of one cached plan runs the same build over the same plan, the set of
// analysed expressions is identical every time, so the table is COMPLETE after the
// first execution and no later execution may miss.
func TestNodeScalarUseMemoIsConsulted(t *testing.T) {
	g := buildMemoGraph(t)

	for _, q := range memoQueries {
		t.Run(q, func(t *testing.T) {
			e := NewEngine(g)

			// First execution: cold. Every analysed expression is a miss.
			drainRows(t, e, q)
			entry, ok := e.cache.get(planCacheKeyFor(q))
			if !ok {
				t.Fatalf("query is not in the plan cache after one execution")
			}
			coldMisses := entry.scalarUse.misses.Load()
			if coldMisses == 0 {
				t.Skipf("this shape analyses no expression, so there is nothing to memoise")
			}
			coldHits := entry.scalarUse.hits.Load()

			// Second and third executions: the table is already complete, so every
			// lookup must hit and NOTHING may be added.
			drainRows(t, e, q)
			drainRows(t, e, q)

			if got := entry.scalarUse.misses.Load(); got != coldMisses {
				t.Errorf("misses grew from %d to %d after the first execution: the table is not complete "+
					"after one build, so some expression is analysed fresh every time", coldMisses, got)
			}
			if got := entry.scalarUse.hits.Load(); got <= coldHits {
				t.Errorf("hits stayed at %d across two further executions: the memo is populated but never "+
					"consulted, so the analysis is still being recomputed", got)
			}
		})
	}
}

// TestNodeScalarUseMemoValueIsNotMutated is the correctness gate. After the query
// has run repeatedly — so every consumer has had the shared analysis in its hands
// many times — each memoised entry must still deep-equal a FRESH analysis of the
// same expression.
//
// This is one assertion covering two requirements: that the memoised value is
// what the unmemoised code would have produced, and that no consumer wrote to it.
// Either failure shows up as the same inequality.
func TestNodeScalarUseMemoValueIsNotMutated(t *testing.T) {
	g := buildMemoGraph(t)

	for _, q := range memoQueries {
		t.Run(q, func(t *testing.T) {
			e := NewEngine(g)
			for i := 0; i < 5; i++ {
				drainRows(t, e, q)
			}
			entry, ok := e.cache.get(planCacheKeyFor(q))
			if !ok {
				t.Fatalf("query is not in the plan cache")
			}

			checked := 0
			entry.scalarUse.m.Range(func(k, v any) bool {
				checked++
				got := v.(*nodeScalarAnalysis)
				wantUses, wantBail := analyseNodeScalarUse(k.(ast.Expression))
				if got.bailout != wantBail {
					t.Errorf("memoised bailout is %v, a fresh analysis says %v", got.bailout, wantBail)
				}
				if !reflect.DeepEqual(got.uses, wantUses) {
					t.Errorf("the memoised analysis no longer matches a fresh one after 5 executions:\n"+
						" memo: %#v\nfresh: %#v\n"+
						"either the memo returns something the unmemoised path would not have, or a "+
						"consumer mutated the shared value", got.uses, wantUses)
				}
				return true
			})
			if checked == 0 {
				t.Skipf("this shape analyses no expression, so there is nothing to compare")
			}
		})
	}
}

// TestNodeScalarUseMemoObservesBothBailoutStates asserts the corpus above really
// does reach both branches of the memoised pair, rather than only ever storing
// bailout=false. Without this the two tests could pass while the bailout state —
// the one the nulling call sites gate on — was never memoised at all.
func TestNodeScalarUseMemoObservesBothBailoutStates(t *testing.T) {
	g := buildMemoGraph(t)

	seen := map[bool]string{}
	for _, q := range memoQueries {
		e := NewEngine(g)
		drainRows(t, e, q)
		entry, ok := e.cache.get(planCacheKeyFor(q))
		if !ok {
			t.Fatalf("query %q is not in the plan cache", q)
		}
		entry.scalarUse.m.Range(func(_, v any) bool {
			seen[v.(*nodeScalarAnalysis).bailout] = q
			return true
		})
	}

	for _, want := range []bool{false, true} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no query in memoQueries memoised a bailout=%v analysis, so that state is untested; "+
				"observed only %v", want, seen)
		}
	}
}

// TestNodeScalarUseMemoIsBoundedForSynthesisedPredicates is the resource-safety
// gate, and it exists because the memo's first design was WRONG about its own
// bound.
//
// That design argued the table could not grow without limit because "every
// execution of one cached plan runs the same build over the same plan, so the set
// of analysed expressions is identical every time". Two build paths break it by
// synthesising a FRESH ast node per execution and handing it straight to the
// analyser:
//
//   - the min-label re-anchor, which is enabled by default and fires on any
//     multi-label pattern, builds a residual *ast.LabelPredicate at
//     min_label_scan_plan.go:251;
//   - the single-edge anchor swap builds one at anchor_swap_plan.go:300 and wraps
//     it in a fresh ir.Selection.
//
// A pointer-keyed memo takes a miss AND a store for those on every execution, so
// without an explicit ceiling the table grows for as long as the plan stays
// cached — an unbounded cache, which the project's bounded-resource rule forbids
// outright and which no amount of throughput justifies.
//
// The query below is a multi-label match, so it drives the min-label path.
func TestNodeScalarUseMemoIsBoundedForSynthesisedPredicates(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	seed := NewEngine(g)
	runWrite(t, seed, `CREATE (:Person:Admin {name:'a', age: 30}), (:Person {name:'b', age: 31})`)

	e := NewEngine(g)
	const q = `MATCH (n:Person:Admin) WHERE n.age > 1 RETURN n.name AS name`

	// Deliberately MORE executions than the ceiling allows entries. That is what
	// makes the assertion non-vacuous: an unbounded memo ends with about one entry
	// per execution, so it lands far above the ceiling, while a bounded one stops
	// at it. Verified to fail with the ceiling check removed (768 entries against a
	// ceiling of 256).
	const executions = 3 * scalarUseMemoMaxEntries
	for i := 0; i < executions; i++ {
		drainRows(t, e, q)
	}

	entry, ok := e.cache.get(planCacheKeyFor(q))
	if !ok {
		t.Fatalf("query is not in the plan cache")
	}
	entries := 0
	entry.scalarUse.m.Range(func(_, _ any) bool {
		entries++
		return true
	})

	if entries > scalarUseMemoMaxEntries {
		t.Errorf("after %d executions the memo holds %d entries, above its declared ceiling of %d: "+
			"the table is growing with the execution count, so it is an unbounded cache",
			executions, entries, scalarUseMemoMaxEntries)
	}
	// And the ceiling must actually be doing the work here, not be vacuously
	// satisfied by a query that happens to synthesise nothing: this shape must
	// have taken far more misses than it has entries.
	if misses := entry.scalarUse.misses.Load(); misses < int64(executions) {
		t.Errorf("this shape took only %d misses over %d executions, so it no longer exercises the "+
			"per-execution synthesis this test exists to bound; pick a shape that does",
			misses, executions)
	}
}

// TestNodeScalarUseMemoConcurrentExecutions runs one cached plan from many
// goroutines at once. The memo is the only field of a planCacheEntry written after
// the entry is published, so this is the case its sync.Map exists for: under -race
// this fails if a store and a load can overlap unsafely, and the post-condition
// re-checks the shared value against a fresh analysis in case a concurrent
// double-compute stored a divergent one.
func TestNodeScalarUseMemoConcurrentExecutions(t *testing.T) {
	g := buildMemoGraph(t)
	e := NewEngine(g)
	const q = `MATCH (n:Person) WHERE n.age > 10 AND n.nick IS NULL RETURN n.name AS name`

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				res, err := e.Run(context.Background(), q, nil)
				if err != nil {
					t.Errorf("Run: %v", err)
					return
				}
				for res.Next() { // intentional full drain
				}
				if err := res.Err(); err != nil {
					t.Errorf("Err: %v", err)
				}
				if err := res.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	entry, ok := e.cache.get(planCacheKeyFor(q))
	if !ok {
		t.Fatalf("query is not in the plan cache")
	}
	entry.scalarUse.m.Range(func(k, v any) bool {
		got := v.(*nodeScalarAnalysis)
		wantUses, wantBail := analyseNodeScalarUse(k.(ast.Expression))
		if got.bailout != wantBail || !reflect.DeepEqual(got.uses, wantUses) {
			t.Errorf("after %d concurrent executions the shared analysis diverges from a fresh one:\n"+
				" memo: %#v (bailout %v)\nfresh: %#v (bailout %v)",
				goroutines, got.uses, got.bailout, wantUses, wantBail)
		}
		return true
	})
}

// TestNodeScalarUseMemoAbsentFallsBackToTheAnalysis pins that a build without a
// plan-cache entry — the plan-rendering paths and the write path's builder — still
// analyses correctly rather than silently skipping the analysis. A nil memo must
// mean "compute it", never "there is nothing to compute".
func TestNodeScalarUseMemoAbsentFallsBackToTheAnalysis(t *testing.T) {
	predExpr := &ast.BinaryOp{
		Operator: ">",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Right:    &ast.IntLiteral{Value: 30},
	}

	wantUses, wantBail := analyseNodeScalarUse(predExpr)

	for _, tc := range []struct {
		name  string
		bopts *buildOpts
	}{
		{"nil buildOpts", nil},
		{"buildOpts with no memo", &buildOpts{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotUses, gotBail := analyseNodeScalarUseFor(tc.bopts, predExpr)
			if gotBail != wantBail || !reflect.DeepEqual(gotUses, wantUses) {
				t.Errorf("with %s the analysis returned %#v (bailout %v), want %#v (bailout %v)",
					tc.name, gotUses, gotBail, wantUses, wantBail)
			}
		})
	}
}
