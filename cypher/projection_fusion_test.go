package cypher

// projection_fusion_test.go — rmp #2658. One shared [expr.RowContext] per input
// row for a whole projection body, instead of one per projection ITEM.
//
// The claim under test has four parts, and each has its own oracle here:
//
//  1. THE WIN IS TAKEN, AND IT IS COUNTED. `RETURN r.a, r.b` must build one row
//     context and resolve the relationship ONCE per row, not twice. Proved by two
//     counters read across an A/B over the SAME binary
//     ([projFusionDisabled]) — never by timing, which cannot separate "once" from
//     "twice" at a unit test's sizes. The unfused arm is what makes the counters
//     non-vacuous: it must read exactly 2 per row.
//
//  2. NOTHING ELSE MOVED. A single-item body and every fast path build the same
//     number of contexts they built before — for the fast paths, none at all.
//
//  3. THE PER-EXPRESSION LAZY GATE SURVIVES. rmp #2388 pinned the gate as per
//     EXPRESSION ([TestLazyRelationshipGateIsPerExpressionNotPerQuery]), so a body
//     mixing a field extractor with a scalar read of the same variable must NOT be
//     collapsed onto one materialisation level. Fusion declines there, and the
//     decline is asserted rather than assumed.
//
//  4. SOUNDNESS IS UNCHANGED. No lazily materialised value reaches a result row;
//     every value a shared context serves comes from the row's own pinned
//     snapshot; the shared context neither outlives its row nor crosses a
//     goroutine.
//
// The counters are a process-global instrument ([projFusionCountersOn]), so no
// test in this file may call t.Parallel.
//
// Layer: short. Engines and graphs are local, so the suite is goleak-clean
// (enforced by TestMain in testmain_test.go).

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixtures and instrumentation helpers
// ─────────────────────────────────────────────────────────────────────────────

// newFusionGraph returns the empty MULTIGRAPH every fixture in this file seeds:
// multigraph, because one fixture creates two parallel relationships between one
// endpoint pair and openCypher requires multigraph semantics for that.
func newFusionGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

// fusionRelRows is the row count every counter assertion below is stated per. It
// is above one deliberately: a per-row claim proved on a single row cannot
// distinguish "once per row" from "once per query".
const fusionRelRows = 8

// buildFusionRelGraph seeds n disjoint (a:P)-[:R]->(b:P) pairs through CYPHER, so
// every relationship records its type by-handle and property reads take the
// per-instance route — the route measured at HEAD as the one 217 of the TCK's 218
// value-key relationship materialisations take.
//
// Each edge carries a value key, a second value key, a key that is absent on half
// the edges (so an IS NULL mix has both answers to give), a temporal and a bytes
// property (both of which map to Cypher null and so exercise the kind gating).
func buildFusionRelGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := newFusionGraph()
	e := NewEngine(g)
	for i := 0; i < n; i++ {
		s := strconv.Itoa(i)
		runWrite(t, e, `CREATE (a:P {nm:'a`+s+`', ord:`+s+`}), (b:P {nm:'b`+s+`', ord:`+s+`})`)
		props := `p_int:` + s + `, p_str:'s` + s + `', d_date: date('2020-01-02'), p_list:[1,2,3]`
		if i%2 == 0 {
			props += `, p_odd:` + s
		}
		runWrite(t, e, `MATCH (a:P {nm:'a`+s+`'}), (b:P {nm:'b`+s+`'}) CREATE (a)-[:R {`+props+`}]->(b)`)
	}
	return g
}

// buildFusionParallelEdgeGraph seeds ONE endpoint pair carrying TWO parallel
// relationships whose per-instance property values DIFFER. A shared context that
// resolved the relationship once for the row but bound the WRONG instance — or
// that leaked one instance's value into the other's row — shows up here and
// nowhere else: with identical sibling values the two are indistinguishable.
func buildFusionParallelEdgeGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := newFusionGraph()
	e := NewEngine(g)
	runWrite(t, e, `CREATE (a:P {nm:'a'}), (b:P {nm:'b'})`)
	runWrite(t, e, `MATCH (a:P {nm:'a'}),(b:P {nm:'b'}) CREATE (a)-[:R {p_int:1, p_str:'one'}]->(b)`)
	runWrite(t, e, `MATCH (a:P {nm:'a'}),(b:P {nm:'b'}) CREATE (a)-[:R {p_int:2, p_str:'two'}]->(b)`)
	return g
}

// fusionCounters is one reading of the projection instrumentation.
type fusionCounters struct {
	ctxBuilds uint64 // row contexts built for a projection body
	relBinds  uint64 // calls to buildRelationshipValueFromRow
	unbound   uint64 // fused items that ran with no bound context
}

// measureFusion arms the counters, runs body, and returns the deltas. The arming
// is process-global, which is why nothing in this file runs in parallel.
func measureFusion(body func()) fusionCounters {
	projFusionCountersOn.Store(true)
	defer projFusionCountersOn.Store(false)
	before := fusionCounters{
		ctxBuilds: projRowCtxBuildCount.Load(),
		relBinds:  relRowBindCount.Load(),
		unbound:   projFusedItemUnboundCount.Load(),
	}
	body()
	return fusionCounters{
		ctxBuilds: projRowCtxBuildCount.Load() - before.ctxBuilds,
		relBinds:  relRowBindCount.Load() - before.relBinds,
		unbound:   projFusedItemUnboundCount.Load() - before.unbound,
	}
}

// runFusionArm executes q over graph g on a FRESH engine, with fusion enabled or
// disabled, returning the rows and the counter deltas.
//
// The engine is fresh, and the toggle stays set across the run, because
// [projFusionDisabled] is read at PLAN BUILD time — which happens inside the first
// Run, not in the constructor. Reusing an engine across the two arms would measure
// one arm twice.
//
// Both arms normally share ONE graph, so the two row sets are comparable value for
// value including internal entity ids. write routes through RunAny for a statement
// the read-only Run refuses; such a statement mutates the graph, so its caller must
// hand each arm its own.
func runFusionArm(t *testing.T, g *lpg.Graph[string, float64], q string, fused, write bool) ([]string, fusionCounters) {
	t.Helper()
	projFusionDisabled.Store(!fused)
	defer projFusionDisabled.Store(false)
	e := NewEngine(g)
	var rows []string
	c := measureFusion(func() { rows = drainFusionRows(t, e, q, write) })
	return rows, c
}

// drainFusionRows runs q and returns each row rendered canonically and SORTED.
//
// Sorted because emission order is not part of any claim in this file, and one case
// has to seed a separate graph per arm (it deletes as it reads), where the scan's
// walk order is not guaranteed to agree between the two.
func drainFusionRows(t *testing.T, e *Engine, q string, write bool) []string {
	t.Helper()
	var res *Result
	var err error
	if write {
		res, err = e.RunAny(context.Background(), q, nil)
	} else {
		res, err = e.Run(context.Background(), q, nil)
	}
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		var sb []byte
		for i, c := range cols {
			if i > 0 {
				sb = append(sb, '|')
			}
			sb = append(sb, fmt.Sprintf("%s=%v", c, rec[c])...)
		}
		out = append(out, string(sb))
	}
	// A RUNTIME error becomes a comparable pseudo-row rather than a fatal: some
	// shapes the acceptance criteria name are REQUIRED to raise one (reading a
	// property of a relationship deleted in the same statement raises
	// DeletedEntityAccess), and the claim is that both arms raise the SAME error at
	// the same point. A plan-time error still fatals above, because a query that
	// cannot plan is a test bug and would otherwise pass identically in both arms.
	if err := res.Err(); err != nil {
		out = append(out, "ERR="+err.Error())
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	sort.Strings(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. The win is taken, and it is counted
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectionFusionResolvesOncePerRow is the primary acceptance oracle. For
// `RETURN r.p_int, r.p_str` over N rows:
//
//	fused:   N context builds, N relationship resolutions
//	unfused: 2N of each
//
// and the two arms return identical rows. The unfused arm is not decoration: it is
// what proves the counters can read 2N, so a fused reading of N means the work was
// removed rather than merely never instrumented.
func TestProjectionFusionResolvesOncePerRow(t *testing.T) {
	const q = `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s`
	g := buildFusionRelGraph(t, fusionRelRows)

	fusedRows, fusedC := runFusionArm(t, g, q, true, false)
	plainRows, plainC := runFusionArm(t, g, q, false, false)

	if len(fusedRows) != fusionRelRows {
		t.Fatalf("the query returned %d rows, want %d: the counter assertions below are per row",
			len(fusedRows), fusionRelRows)
	}
	assertEqualRows(t, q, fusedRows, plainRows)

	if plainC.ctxBuilds != 2*fusionRelRows {
		t.Fatalf("unfused: %d row-context builds over %d rows, want %d (one per ITEM per row). "+
			"Without this reading the fused count below is unfalsifiable",
			plainC.ctxBuilds, fusionRelRows, 2*fusionRelRows)
	}
	if plainC.relBinds != 2*fusionRelRows {
		t.Fatalf("unfused: %d relationship resolutions over %d rows, want %d",
			plainC.relBinds, fusionRelRows, 2*fusionRelRows)
	}
	if fusedC.ctxBuilds != fusionRelRows {
		t.Fatalf("fused: %d row-context builds over %d rows, want %d (one per ROW)",
			fusedC.ctxBuilds, fusionRelRows, fusionRelRows)
	}
	if fusedC.relBinds != fusionRelRows {
		t.Fatalf("fused: %d relationship resolutions over %d rows, want %d (one per ROW)",
			fusedC.relBinds, fusionRelRows, fusionRelRows)
	}
	if fusedC.unbound != 0 {
		t.Fatalf("fused: %d item evaluations ran with NO bound context. The closure stays "+
			"correct there, so this is the one way the whole improvement can be deleted "+
			"silently: the driver is not bracketing the row", fusedC.unbound)
	}
}

// TestProjectionFusionScalesWithColumnCount pins that the saving is per COLUMN, not
// a fixed one-off: a three-property body must still build one context per row while
// the unfused arm builds three.
func TestProjectionFusionScalesWithColumnCount(t *testing.T) {
	const q = `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s, r.p_list AS l`
	g := buildFusionRelGraph(t, fusionRelRows)

	fusedRows, fusedC := runFusionArm(t, g, q, true, false)
	plainRows, plainC := runFusionArm(t, g, q, false, false)
	assertEqualRows(t, q, fusedRows, plainRows)

	if plainC.relBinds != 3*fusionRelRows {
		t.Fatalf("unfused: %d relationship resolutions, want %d (3 items x %d rows)",
			plainC.relBinds, 3*fusionRelRows, fusionRelRows)
	}
	if fusedC.relBinds != fusionRelRows {
		t.Fatalf("fused: %d relationship resolutions, want %d (1 per row regardless of column count)",
			fusedC.relBinds, fusionRelRows)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Nothing else moved
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectionFusionLeavesSingleItemAndFastPathsAlone pins the two "no change"
// halves of the acceptance criteria at once, because they are the same claim about
// different shapes: a body fusion does not touch must build exactly the contexts it
// built before.
//
// The counter is compared between the two arms rather than against a literal, so
// the assertion is "fusion changed nothing here" and not "this shape happens to
// cost K", which would be a brittle restatement of the current plan.
func TestProjectionFusionLeavesSingleItemAndFastPathsAlone(t *testing.T) {
	g := buildFusionRelGraph(t, fusionRelRows)
	for _, tc := range []struct {
		name string
		q    string
		// wantCtx is the exact number of projection row-context builds the shape
		// must cost, stated per fixture (fusionRelRows rows).
		wantCtx uint64
	}{
		// One general-path item: one context per row, fused or not. Fusing a single
		// item would buy nothing and would stop this being byte-identical.
		{"single-item", `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i`, fusionRelRows},
		// Every item on an inline fast path: the edge-variable reconstruction and
		// the Variable column lookups build no context at all, and must keep
		// building none — forcing one onto them would be a regression.
		{"variable-fast-paths", `MATCH (a:P)-[r:R]->(b:P) RETURN r, a, b`, 0},
		// A two-column bare projection over an upstream WITH. The WITH carries ONE
		// general-path item (the other is a Variable rename, itself a fast path), so
		// it pays one context per row and does not fuse; the RETURN's two bare
		// columns pay nothing. A count above 1x rows means the bare-column path had
		// started building one.
		{"bare-columns", `MATCH (a:P)-[r:R]->(b:P) WITH r.p_int AS i, a AS aa RETURN aa, i`, fusionRelRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fusedRows, fusedC := runFusionArm(t, g, tc.q, true, false)
			plainRows, plainC := runFusionArm(t, g, tc.q, false, false)
			assertEqualRows(t, tc.q, fusedRows, plainRows)
			if len(fusedRows) == 0 {
				t.Fatalf("%q returned no rows, so this asserted nothing", tc.q)
			}
			if fusedC.ctxBuilds != tc.wantCtx || plainC.ctxBuilds != tc.wantCtx {
				t.Fatalf("%q built %d row contexts fused and %d unfused, want %d in both",
					tc.q, fusedC.ctxBuilds, plainC.ctxBuilds, tc.wantCtx)
			}
			if fusedC.unbound != 0 {
				t.Fatalf("%q: %d unbound fused evaluations", tc.q, fusedC.unbound)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. The per-expression lazy gate survives
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectionFusionDeclinesWhenItWouldDemoteAnItem is the counterpart to
// [TestLazyRelationshipGateIsPerExpressionNotPerQuery]. Each shape below mixes, in
// ONE body, two uses of the SAME variable that want DIFFERENT materialisation
// levels. Fusion must decline: the shared level would have to be the more eager of
// the two, which is sound but measurably worse, and rmp #2388 bought that
// difference deliberately.
//
// The oracle is the context count: a declined body still builds one context per
// ITEM per row, so a fused reading here would be the regression.
func TestProjectionFusionDeclinesWhenItWouldDemoteAnItem(t *testing.T) {
	g := buildFusionRelGraph(t, fusionRelRows)
	for _, tc := range []struct {
		name  string
		q     string
		items uint64 // general-path items in the body
	}{
		// type(r) type-switches on a concrete relationship, so r must be eager for
		// that item; r.p_str alone would be lazy. This is the exact shape #2388's
		// per-expression test names.
		{"extractor-with-scalar-read", `MATCH (a:P)-[r:R]->(b:P) RETURN type(r) AS t, r.p_str AS s`, 2},
		// properties(r) needs the whole entity (a nulled analysis), r.p_int does not:
		// the only sound union is the eager one, which would demote the second item.
		{"whole-entity-with-scalar-read", `MATCH (a:P)-[r:R]->(b:P) RETURN size(properties(r)) AS p, r.p_int AS i`, 2},
		// keys(n) forces a concrete partial node carrying the key set; n.nm alone
		// would be lazy. Same rule, node side.
		{"node-extractor-with-scalar-read", `MATCH (a:P)-[r:R]->(b:P) RETURN size(keys(a)) AS k, a.nm AS nm`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fusedRows, fusedC := runFusionArm(t, g, tc.q, true, false)
			plainRows, plainC := runFusionArm(t, g, tc.q, false, false)
			assertEqualRows(t, tc.q, fusedRows, plainRows)
			want := tc.items * fusionRelRows
			if plainC.ctxBuilds != want {
				t.Fatalf("%q unfused built %d contexts, want %d: the fixture or the item count is wrong, "+
					"so the decline assertion below cannot be read", tc.q, plainC.ctxBuilds, want)
			}
			if fusedC.ctxBuilds != want {
				t.Fatalf("%q built %d contexts with fusion enabled, want %d. Fusion collapsed a body "+
					"whose items want different materialisation levels: the second item lost the "+
					"per-expression lazy path rmp #2388 bought for it", tc.q, fusedC.ctxBuilds, want)
			}
		})
	}
}

// TestProjectionFusionIsPerVariableNotPerBody is the other half of that rule, and
// the reason the union is a per-VARIABLE merge rather than one level for the body:
// when two items want different levels for DIFFERENT variables, both levels can be
// served by one context, so fusion must proceed.
//
// `RETURN size(keys(r)) AS k, a.nm AS nm` wants a concrete partial for r and a lazy value
// for a. A design that carried one level for the whole body would have to decline
// here — or demote a — and this test fails on both.
func TestProjectionFusionIsPerVariableNotPerBody(t *testing.T) {
	const q = `MATCH (a:P)-[r:R]->(b:P) RETURN size(keys(r)) AS k, a.nm AS nm`
	g := buildFusionRelGraph(t, fusionRelRows)

	fusedRows, fusedC := runFusionArm(t, g, q, true, false)
	plainRows, plainC := runFusionArm(t, g, q, false, false)
	assertEqualRows(t, q, fusedRows, plainRows)

	if plainC.ctxBuilds != 2*fusionRelRows {
		t.Fatalf("unfused built %d contexts, want %d (2 items x %d rows)",
			plainC.ctxBuilds, 2*fusionRelRows, fusionRelRows)
	}
	if fusedC.ctxBuilds != fusionRelRows {
		t.Fatalf("fused built %d contexts, want %d. Two items wanting different levels for "+
			"DIFFERENT variables must still share one context: declining here means the "+
			"materialisation decision is per body rather than per variable",
			fusedC.ctxBuilds, fusionRelRows)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Soundness
// ─────────────────────────────────────────────────────────────────────────────

// fusionEscapeQueries pair a scalar read — the trigger for a lazy binding — with a
// shape that puts the entity, or something built from it, into the result. The union
// gate must deny where ANY single item would have denied, so none of these may leak
// a lazily materialised value into a row.
//
// They overlap deliberately with lazyRelEscapeQueries: that table proves the
// PER-ITEM gate, this one proves the UNION does not widen it.
var fusionEscapeQueries = []string{
	`MATCH (a:P)-[r:R]->(b:P) RETURN r, r.p_int AS i`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r AS rr`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN properties(r) AS p, r.p_int AS i, r.p_str AS s`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN [r] AS l, r.p_int AS i, r.p_str AS s`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN {rel: r, n: a} AS m, r.p_int AS i, a.nm AS nm`,
	`MATCH p = (a:P)-[r:R]->(b:P) RETURN p, r.p_int AS i, r.p_str AS s`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN a, a.nm AS nm, b.nm AS bnm`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN coalesce(r) AS c, r.p_int AS i, r.p_str AS s`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN keys(r) AS k, r.p_int AS i, r.p_str AS s`,
	// The fused shapes themselves: a lazy value IS produced for these, and must
	// still not reach a cell through any container.
	`MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s, r.p_list AS l`,
	`MATCH (a:P)-[r:R]->(b:P) RETURN a.nm AS an, b.nm AS bn, r.p_int AS i`,
}

// TestProjectionFusionNoLazyValueReachesAResultRow scans every cell of every row —
// recursing into lists, maps, nodes, relationships and paths — for a value that
// only a non-escaping context may hold. Such a value is not merely mis-serialised:
// it carries a reference to the pinned ReadView past the query's visibility
// barrier, so it is a lifetime bug.
func TestProjectionFusionNoLazyValueReachesAResultRow(t *testing.T) {
	e := NewEngine(buildFusionRelGraph(t, 3))
	for _, q := range fusionEscapeQueries {
		t.Run(q, func(t *testing.T) {
			res, err := e.Run(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			defer func() {
				if cerr := res.Close(); cerr != nil {
					t.Errorf("Close: %v", cerr)
				}
			}()
			cols := res.Columns()
			rows := 0
			for res.Next() {
				rows++
				for i, c := range cols {
					if p := findLazyRel(c, res.ValueAt(i)); p != "" {
						t.Fatalf("a lazily materialised value reached a result row at %s: the "+
							"shared context admitted a level no single item would have", p)
					}
				}
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Err: %v", err)
			}
			if rows == 0 {
				t.Fatalf("the query returned no rows, so the scan asserted nothing")
			}
		})
	}
}

// TestProjectionFusionSharedContextMatchesOnePinnedSnapshot is the MVCC criterion.
// A writer commits v1 and v2 together, so any snapshot sees them EQUAL; a fused
// reader projects both through the SAME shared context. A row where they differ
// means one column was served from a different instant than the other — exactly
// what sharing a context could have introduced, since the second column's value is
// now read through a binding built for the first.
//
// The assertion is on internal consistency, not on WHICH generation is observed:
// which one a run sees is a race, and asserting it would be a flake.
func TestProjectionFusionSharedContextMatchesOnePinnedSnapshot(t *testing.T) {
	g := newFusionGraph()
	e := NewEngine(g)
	runWrite(t, e, `CREATE (a:P {nm:'a'}), (b:P {nm:'b'})`)
	runWrite(t, e, `MATCH (a:P {nm:'a'}),(b:P {nm:'b'}) CREATE (a)-[:R {v1:0, v2:0}]->(b)`)

	const q = `MATCH (a:P)-[r:R]->(b:P) RETURN r.v1 AS v1, r.v2 AS v2`
	// Prove the shape is fused before relying on it: an unfused reading would make
	// the consistency assertion a test of the pre-existing per-item path.
	if _, c := runFusionArm(t, g, q, true, false); c.ctxBuilds != 1 {
		t.Fatalf("the probe shape built %d contexts for 1 row, want 1: it is not fused, so this "+
			"test would assert nothing about the shared context", c.ctxBuilds)
	}

	var stop sync.WaitGroup
	done := make(chan struct{})
	stop.Add(1)
	go func() {
		defer stop.Done()
		for gen := 1; ; gen++ {
			select {
			case <-done:
				return
			default:
			}
			s := strconv.Itoa(gen)
			res, err := e.RunInTx(context.Background(),
				`MATCH (a:P)-[r:R]->(b:P) SET r.v1 = `+s+`, r.v2 = `+s, nil)
			if err != nil {
				return
			}
			for res.Next() { // intentional full drain
			}
			_ = res.Close()
		}
	}()

	for i := 0; i < 400; i++ {
		res, err := e.Run(context.Background(), q, nil)
		if err != nil {
			close(done)
			stop.Wait()
			t.Fatalf("Run: %v", err)
		}
		for res.Next() {
			rec := res.Record()
			if fmt.Sprint(rec["v1"]) != fmt.Sprint(rec["v2"]) {
				close(done)
				stop.Wait()
				t.Fatalf("a fused row observed v1=%v and v2=%v; they are written in one "+
					"transaction, so no single snapshot can hold them unequal: the two "+
					"columns were served from different instants",
					rec["v1"], rec["v2"])
			}
		}
		if err := res.Err(); err != nil {
			close(done)
			stop.Wait()
			t.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			close(done)
			stop.Wait()
			t.Fatalf("Close: %v", err)
		}
	}
	close(done)
	stop.Wait()
}

// TestProjectionFusionUnderConcurrentQueriesIsRaceFree drives the fused shape from
// many goroutines over ONE engine. Each execution builds its own operator tree and
// therefore its own binder, which is the ownership contract [exec.RowBinder]
// states; this is what fails under -race if a binder were ever shared, or if the
// shared context outlived the row that built it.
func TestProjectionFusionUnderConcurrentQueriesIsRaceFree(t *testing.T) {
	e := NewEngine(buildFusionRelGraph(t, fusionRelRows))
	const q = `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s, a.nm AS an ORDER BY i`
	want := drainRows(t, e, q)
	if len(want) != fusionRelRows {
		t.Fatalf("baseline returned %d rows, want %d", len(want), fusionRelRows)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for w := 0; w < goroutines; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				res, err := e.Run(context.Background(), q, nil)
				if err != nil {
					errs <- fmt.Sprintf("Run: %v", err)
					return
				}
				n := 0
				for res.Next() {
					rec := res.Record()
					got := fmt.Sprintf("i=%v|s=%v|an=%v", rec["i"], rec["s"], rec["an"])
					if got != want[n] {
						errs <- fmt.Sprintf("row %d: got %q, want %q", n, got, want[n])
						_ = res.Close()
						return
					}
					n++
				}
				if err := res.Err(); err != nil {
					errs <- fmt.Sprintf("Err: %v", err)
					_ = res.Close()
					return
				}
				if n != len(want) {
					errs <- fmt.Sprintf("got %d rows, want %d", n, len(want))
					_ = res.Close()
					return
				}
				if err := res.Close(); err != nil {
					errs <- fmt.Sprintf("Close: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
}

// TestProjectionFusionUnderMorselParallelProjectionIsRaceFree drives a fused body
// inside the MORSEL-PARALLEL scan tier, where several worker goroutines each run
// their own copy of the projection subtree at the same time. That is the one place a
// binder could be reached from more than one goroutine, and the reason it is built
// per subtree rather than registered on buildOpts — which is also why
// [buildOpts.forWorker] has no sixth eval-time field to reset.
//
// The parallel leaf is asserted to have ENGAGED. Without that the test is a serial
// run wearing a parallel name.
func TestProjectionFusionUnderMorselParallelProjectionIsRaceFree(t *testing.T) {
	const n = 600
	g := buildSubsetLabelGraph(t, n)
	on, off := engines(g)
	// Two computed items, so the body takes the general (context-building) path
	// rather than the columnar property path, and so fusion applies.
	const q = `MATCH (n:Few) RETURN n.v + 0 AS a, n.gp + 0 AS b`

	before := parallelScanProjectBuildCount.Load()
	gotOn := drainSortedPS(t, on, q)
	engaged := parallelScanProjectBuildCount.Load() > before
	gotOff := drainSortedPS(t, off, q)
	assertEqualRows(t, q, gotOn, gotOff)
	if !engaged {
		t.Fatalf("the morsel-parallel projection did not engage for %q, so no worker goroutine "+
			"drove a fused binder and this test proved nothing about concurrency", q)
	}
	if len(gotOn) != (n+2)/3 {
		t.Fatalf("%q returned %d rows, want %d", q, len(gotOn), (n+2)/3)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Result identity across the shapes the acceptance criteria name
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectionFusionResultsAreIdenticalAcrossShapes is a differential over the
// shapes the task singles out as the places a shared context could change an
// answer: a dynamic-looking subscript, a presence test mixed with a value read of
// the same entity, the property kinds that map to Cypher null, a multigraph
// parallel-edge pair with divergent per-instance values, and a relationship read
// after it was deleted in the same statement.
//
// Each runs on a fusion-enabled and a fusion-disabled engine and the rows must be
// identical. Where fusion is expected to engage that is asserted too, so a shape
// cannot pass by quietly declining.
func TestProjectionFusionResultsAreIdenticalAcrossShapes(t *testing.T) {
	rel := func(t *testing.T) *lpg.Graph[string, float64] { return buildFusionRelGraph(t, fusionRelRows) }
	par := buildFusionParallelEdgeGraph

	for _, tc := range []struct {
		name string
		seed func(*testing.T) *lpg.Graph[string, float64]
		q    string
		rows int
		// wantFuse asserts whether the shape is expected to fuse. Stated per case so
		// a shape cannot pass by quietly declining.
		wantFuse bool
		// fuseNotAsserted drops the wantFuse check for a shape whose plan is built by
		// a different builder, where whether fusion reaches it is not this case's
		// claim — result identity is.
		fuseNotAsserted bool
		// write routes the statement through RunAny (the read-only Run refuses it)
		// AND seeds a SEPARATE graph per arm, because it mutates what it reads.
		write bool
	}{
		{
			// r["k"] with a literal key is a scalar key use, so it joins the union.
			name: "subscript-and-property", seed: rel,
			q:    `MATCH (a:P)-[r:R]->(b:P) RETURN r["p_str"] AS s, r.p_int AS i`,
			rows: fusionRelRows, wantFuse: true,
		},
		{
			// A presence-only key in one item and a value key in another: C1 keeps
			// them apart, and the fused reading must still answer both.
			name: "is-null-mixed-with-value-key", seed: rel,
			q:    `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_odd IS NULL AS absent, r.p_int AS i`,
			rows: fusionRelRows, wantFuse: true,
		},
		{
			// A temporal property (a tagged storage encoding) beside a plain value
			// key, on the by-handle route.
			name: "temporal-kind-by-handle", seed: rel,
			q:    `MATCH (a:P)-[r:R]->(b:P) RETURN r.d_date AS d, r.p_int AS i`,
			rows: fusionRelRows, wantFuse: true,
		},
		{
			// The PER-PAIR route, over the #2388 fixture — the only one carrying a
			// BYTES property (which has no Cypher mapping and so reads as null) and
			// all six tagged temporal encodings. Written through the raw storage API,
			// which Cypher cannot express, so this route and these kinds are reachable
			// nowhere else.
			name: "bytes-and-temporal-kinds-per-pair", seed: buildLazyRelPerPairGraph,
			q: `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_bytes AS bt, r.d_date AS d, ` +
				`r.d_duration AS du, r.p_str AS s`,
			rows: 1, wantFuse: true,
		},
		{
			// Two parallel edges between one pair, with DIFFERENT per-instance
			// values. One shared context per row must bind the row's OWN instance.
			name: "multigraph-parallel-edges", seed: par,
			q:    `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s`,
			rows: 2, wantFuse: true,
		},
		{
			// A relationship read after DELETE in the same statement. The delete
			// stamps a frozen snapshot into the row, so neither arm may resolve the
			// (now absent) edge from storage.
			// openCypher forbids reading a property of an entity deleted in the same
			// statement, so both arms must raise the SAME DeletedEntityAccess — which
			// arrives as the single ERR= pseudo-row.
			name: "delete-then-read", seed: rel,
			q:    `MATCH (a:P)-[r:R]->(b:P) DELETE r RETURN r.p_int AS i, r.p_str AS s`,
			rows: 1, fuseNotAsserted: true, write: true,
		},
		{
			// Node receivers with an ORDER BY above: the sink is not columnar, so the
			// columnar projection falls back to its row-at-a-time arm — and fusion
			// applies there too. MEASURED, not assumed: this case was written
			// expecting a decline and the counter said otherwise (16 contexts fused
			// against 32 unfused over 16 rows).
			name: "node-properties-row-fallback", seed: rel,
			q:    `MATCH (a:P) RETURN a.nm AS nm, a.ord AS o ORDER BY nm`,
			rows: 2 * fusionRelRows, wantFuse: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A non-mutating case shares ONE graph, so the two row sets are comparable
			// value for value, internal entity ids included.
			gFused := tc.seed(t)
			gPlain := gFused
			if tc.write {
				gPlain = tc.seed(t)
			}
			fusedRows, fusedC := runFusionArm(t, gFused, tc.q, true, tc.write)
			plainRows, plainC := runFusionArm(t, gPlain, tc.q, false, tc.write)
			if len(fusedRows) != tc.rows {
				t.Fatalf("%q returned %d rows, want %d", tc.q, len(fusedRows), tc.rows)
			}
			assertEqualRows(t, tc.q, fusedRows, plainRows)
			fused := fusedC.ctxBuilds < plainC.ctxBuilds
			if !tc.fuseNotAsserted && fused != tc.wantFuse {
				t.Fatalf("%q: fused=%v (contexts %d fused vs %d unfused), want fused=%v",
					tc.q, fused, fusedC.ctxBuilds, plainC.ctxBuilds, tc.wantFuse)
			}
			if fusedC.unbound != 0 {
				t.Fatalf("%q: %d fused evaluations ran unbracketed", tc.q, fusedC.unbound)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. The union and the class gate, unit-tested directly
// ─────────────────────────────────────────────────────────────────────────────

// fusionUse builds one nodeScalarUse from a value-key set and a presence-key set,
// with the C1 invariant already satisfied by the caller's choice of sets.
func fusionUse(keys, presence []string, mutate ...func(*nodeScalarUse)) *nodeScalarUse {
	u := &nodeScalarUse{keys: map[string]struct{}{}, presenceKeys: map[string]struct{}{}}
	for _, k := range keys {
		u.keys[k] = struct{}{}
	}
	for _, k := range presence {
		u.presenceKeys[k] = struct{}{}
	}
	for _, m := range mutate {
		m(u)
	}
	u.internPresenceMaps()
	return u
}

// TestUnionNodeScalarUsesAndClassGate covers the decision table directly, because
// the end-to-end tests above can only observe the OUTCOME (fused or not) and not
// which of the two gates — the union's admissibility or the class check — produced
// it. Each row names the reason.
func TestUnionNodeScalarUsesAndClassGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		items        []fusableProjItem
		wantUnionOK  bool
		wantUnionNil bool
		wantPreserve bool
		reason       string
	}{
		{
			name: "all-eager-shares-one-eager-context",
			items: []fusableProjItem{
				{idx: 0, use: nil},
				{idx: 1, use: nil},
			},
			wantUnionOK: true, wantUnionNil: true, wantPreserve: true,
			reason: "every item is already fully eager, so one eager context per row " +
				"replaces one per item with nothing demoted",
		},
		{
			name: "mixed-eager-and-scalar-is-inadmissible",
			items: []fusableProjItem{
				{idx: 0, use: nil},
				{idx: 1, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"a"}, nil)}},
			},
			wantUnionOK: false, wantUnionNil: true, wantPreserve: true,
			reason: "the only sound shared level is the eager one, which would demote the scalar item",
		},
		{
			name: "two-value-key-reads-of-one-variable-merge",
			items: []fusableProjItem{
				{idx: 0, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"a"}, nil)}},
				{idx: 1, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"b"}, nil)}},
			},
			wantUnionOK: true, wantUnionNil: false, wantPreserve: true,
			reason: "the target shape: both items stay lazy and read their key on demand",
		},
		{
			name: "extractor-beside-scalar-read-of-the-same-variable-is-refused",
			items: []fusableProjItem{
				{idx: 0, use: map[string]*nodeScalarUse{
					"r": fusionUse(nil, nil, func(u *nodeScalarUse) { u.needsType = true }),
				}},
				{idx: 1, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"a"}, nil)}},
			},
			wantUnionOK: true, wantUnionNil: false, wantPreserve: false,
			reason: "type(r) forces a concrete value for r, which would demote item 1's lazy read — " +
				"the per-expression gate rmp #2388 pinned",
		},
		{
			name: "presence-key-promoted-to-a-value-key-is-refused",
			items: []fusableProjItem{
				{idx: 0, use: map[string]*nodeScalarUse{"r": fusionUse(nil, []string{"a"})}},
				{idx: 1, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"a"}, nil)}},
			},
			wantUnionOK: true, wantUnionNil: false, wantPreserve: false,
			reason: "C1 makes a value-and-presence key value-needed, replacing item 0's kind-gated " +
				"presence check with a value read",
		},
		{
			name: "different-levels-for-different-variables-are-both-served",
			items: []fusableProjItem{
				{idx: 0, use: map[string]*nodeScalarUse{
					"r": fusionUse(nil, nil, func(u *nodeScalarUse) { u.needsKeyNames = true }),
				}},
				{idx: 1, use: map[string]*nodeScalarUse{"a": fusionUse([]string{"nm"}, nil)}},
			},
			wantUnionOK: true, wantUnionNil: false, wantPreserve: true,
			reason: "the union is per variable, so r may be concrete while a stays lazy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			union, ok := unionNodeScalarUses(tc.items)
			if ok != tc.wantUnionOK {
				t.Fatalf("unionNodeScalarUses admissible=%v, want %v (%s)", ok, tc.wantUnionOK, tc.reason)
			}
			if (union == nil) != tc.wantUnionNil {
				t.Fatalf("unionNodeScalarUses returned nil=%v, want nil=%v (%s)",
					union == nil, tc.wantUnionNil, tc.reason)
			}
			if got := fusionPreservesEveryItem(union, tc.items); got != tc.wantPreserve {
				t.Fatalf("fusionPreservesEveryItem=%v, want %v (%s)", got, tc.wantPreserve, tc.reason)
			}
		})
	}

	t.Run("merged-keys-are-the-union", func(t *testing.T) {
		items := []fusableProjItem{
			{idx: 0, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"a"}, []string{"p"})}},
			{idx: 1, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"b"}, []string{"q"})}},
		}
		union, ok := unionNodeScalarUses(items)
		if !ok || union == nil {
			t.Fatalf("union refused an all-scalar item set")
		}
		m := union["r"]
		for _, k := range []string{"a", "b"} {
			if _, has := m.keys[k]; !has {
				t.Fatalf("merged value keys %v are missing %q", m.keys, k)
			}
		}
		for _, k := range []string{"p", "q"} {
			if _, has := m.presenceKeys[k]; !has {
				t.Fatalf("merged presence keys %v are missing %q", m.presenceKeys, k)
			}
		}
		if m.presenceMaps == nil {
			t.Fatal("the merged presence table was not re-interned: the per-item tables are indexed " +
				"by a different key set and cannot answer for the merged one")
		}
	})

	t.Run("inputs-are-not-mutated", func(t *testing.T) {
		// The per-item analyses come from nodeScalarUseMemo, whose entries are shared
		// across every execution of a cached plan and immutable by contract
		// (TestNodeScalarUseMemoValueIsNotMutated). Mutating one here would corrupt
		// every later execution of the query.
		u0 := fusionUse([]string{"a"}, []string{"p"})
		u1 := fusionUse([]string{"p"}, nil)
		items := []fusableProjItem{
			{idx: 0, use: map[string]*nodeScalarUse{"r": u0}},
			{idx: 1, use: map[string]*nodeScalarUse{"r": u1}},
		}
		if _, ok := unionNodeScalarUses(items); !ok {
			t.Fatal("union refused an all-scalar item set")
		}
		if len(u0.keys) != 1 || len(u0.presenceKeys) != 1 {
			t.Fatalf("item 0's analysis was mutated: keys=%v presenceKeys=%v", u0.keys, u0.presenceKeys)
		}
		if _, still := u0.presenceKeys["p"]; !still {
			t.Fatal("C1 reconciliation on the merged copy reached item 0's own analysis")
		}
	})
}

// TestNewProjRowBinderDeclinesBelowTwoItems pins the single-item rule structurally,
// which the counter tests can only pin behaviourally: one item's shared context IS
// that item's own context, so no binder is built and the body stays exactly as the
// build loop made it.
func TestNewProjRowBinderDeclinesBelowTwoItems(t *testing.T) {
	rs := newRowSchema(map[string]int{"r": 0})
	itemA := fusableProjItem{idx: 0, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"a"}, nil)}}
	itemB := fusableProjItem{idx: 1, use: map[string]*nodeScalarUse{"r": fusionUse([]string{"b"}, nil)}}

	if b, fns := newProjRowBinder([]fusableProjItem{itemA}, rs, nil, nil, nil, nil); b != nil || fns != nil {
		t.Fatalf("a single-item body was fused (binder=%v, fns=%d)", b != nil, len(fns))
	}
	if b, fns := newProjRowBinder(nil, rs, nil, nil, nil, nil); b != nil || fns != nil {
		t.Fatalf("an empty body was fused (binder=%v, fns=%d)", b != nil, len(fns))
	}
	b, fns := newProjRowBinder([]fusableProjItem{itemA, itemB}, rs, nil, nil, nil, nil)
	if b == nil || len(fns) != 2 {
		t.Fatalf("two mergeable items were NOT fused (binder=%v, fns=%d): the decline above would "+
			"then prove nothing, because nothing fuses", b != nil, len(fns))
	}
}

// TestProjRowBinderContextDoesNotOutliveItsRow pins the lifetime contract directly.
// Between BindRow and ReleaseRow the binder serves one context and serves the SAME
// one to every item of that row; after ReleaseRow it serves none, so a stray
// evaluation falls back to building its own instead of reading a map that may
// already have been recycled into another row.
func TestProjRowBinderContextDoesNotOutliveItsRow(t *testing.T) {
	b := &projRowBinder{rs: newRowSchema(map[string]int{})}
	if b.bound {
		t.Fatal("a fresh binder reports a bound row")
	}
	b.BindRow(nil)
	if !b.bound {
		t.Fatal("BindRow did not open the row window")
	}
	first := b.contextFor(nil)
	if first == nil {
		t.Fatal("contextFor returned no context inside the row window")
	}
	if second := b.contextFor(nil); !sameRowContext(first, second) {
		t.Fatal("two items of one row were served DIFFERENT contexts: the context is being rebuilt " +
			"per item, which is the cost this change removes")
	}
	b.ReleaseRow()
	if b.bound || b.ctx != nil || b.pooled != nil {
		t.Fatalf("ReleaseRow left state behind: bound=%v ctx=%v pooled=%v",
			b.bound, b.ctx != nil, b.pooled != nil)
	}
}

// sameRowContext reports whether two RowContext values are the SAME map, by
// writing through one and reading it back through the other. Comparing maps with
// == is illegal in Go, and reflect.DeepEqual would answer "equal" for two distinct
// empty maps — exactly the case this must distinguish.
func sameRowContext(a, b expr.RowContext) bool {
	const probe = "\x00fusion-identity-probe"
	a[probe] = expr.Null
	_, shared := b[probe]
	delete(a, probe)
	return shared
}
