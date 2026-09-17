package cypher

// rowbind_test.go — the gate for rmp #2865: the per-row binding ladder is
// resolved ONCE into a [rowBindPlan], and the change is ANSWER-IDENTICAL to the
// name-keyed ladder it replaces.
//
// Four kinds of evidence, none substituting for another:
//
//  1. WHITE-BOX, STRUCTURAL — the compiled body of [populateRowCtx] contains no
//     call to a map-LOOKUP runtime routine. This is the claim "the per-row path
//     performs no name lookup" proved against the machine code rather than
//     against a reading of the source.
//  2. WHITE-BOX, EQUIVALENCE — for a hand-built buildOpts covering every kind and
//     every collision between kinds, the resolved entry equals a direct probe of
//     the same maps.
//  3. DEFERRAL — a plan built BEFORE the Apply/hash-join rebase resolves to the
//     POST-rebase columns. This is what makes the resolution sound, and it fails
//     on any build that resolves eagerly.
//  4. BEHAVIOURAL — the name-collision shapes that a green TCK does not catch
//     (rmp #2864 established that: 3897/3897 held with its demotion rule removed).

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. WHITE-BOX, STRUCTURAL — no name lookup on the per-row path
// ─────────────────────────────────────────────────────────────────────────────

// mapLookupRuntimeCalls is every runtime entry point a Go map READ can compile
// to. A map WRITE (mapassign*) is deliberately absent: the RowContext is still a
// map keyed by variable name, and binding it positionally is rmp #2876, out of
// scope here. Listing the read routines explicitly — rather than matching the
// substring "map" — is what keeps the assertion from passing merely because a
// symbol was renamed.
var mapLookupRuntimeCalls = []string{
	"runtime.mapaccess1(",
	"runtime.mapaccess2(",
	"runtime.mapaccess1_fast32(",
	"runtime.mapaccess2_fast32(",
	"runtime.mapaccess1_fast64(",
	"runtime.mapaccess2_fast64(",
	"runtime.mapaccess1_fast64ptr(",
	"runtime.mapaccess2_fast64ptr(",
	"runtime.mapaccess1_faststr(",
	"runtime.mapaccess2_faststr(",
	"runtime.mapaccess1_fat(",
	"runtime.mapaccess2_fat(",
}

// TestPopulateRowCtx_PerRowPathPerformsNoNameLookup compiles this package with
// -gcflags=-S and asserts that the emitted body of [populateRowCtx] — the
// function that runs once per ROW and, before rmp #2865, asked up to seven
// name-keyed map questions per VARIABLE — calls no map-lookup routine at all.
//
// It is deliberately structural. A behavioural test cannot tell "resolved once"
// from "probed per row", because the two produce the same rows; and a reading of
// the source cannot see an inlined callee dragging a lookup back in. The
// compiler's own listing sees both.
//
// It reads the COMPILER's listing rather than disassembling the running test
// binary, because `go tool objdump` returns an empty listing for the binary
// `go test` builds and runs (measured: exit 0, no stdout, no stderr, for every
// symbol filter including none at all), so an objdump-based form of this test
// would pass vacuously.
//
// At f62a3c83 — the commit this change is built on — the same body contained
// nine runtime.mapaccess2_faststr calls and one runtime.mapaccess1_faststr call.
func TestPopulateRowCtx_PerRowPathPerformsNoNameLookup(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool unavailable: cannot compile the package for its assembly listing")
	}
	cmd := exec.Command("go", "build", "-gcflags=-S", "github.com/FlavioCFOliveira/GoGraph/cypher")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("go build -gcflags=-S failed (%v): %s", err, truncate(stderr.String(), 400))
	}
	body, ok := funcAssembly(stderr.String(), "github.com/FlavioCFOliveira/GoGraph/cypher.populateRowCtx")
	if !ok {
		t.Fatalf("no STEXT block for populateRowCtx in the assembly listing (%d bytes): the "+
			"extraction matched nothing, so this test proves nothing", stderr.Len())
	}
	for _, call := range mapLookupRuntimeCalls {
		if strings.Contains(body, call) {
			t.Errorf("populateRowCtx still calls %s: a name lookup survives on the per-row path", call)
		}
	}
	// The counterweight: assert the write is STILL there. Without it a build that
	// accidentally emptied the loop would pass the assertion above for the wrong
	// reason.
	if !strings.Contains(body, "runtime.mapassign_faststr(") {
		t.Error("populateRowCtx calls no map assignment either: the loop is not binding anything, " +
			"so the absence of lookups above proves nothing")
	}
}

// funcAssembly extracts one function's STEXT block from a `-gcflags=-S` listing.
// The block runs from the symbol's own `<name> STEXT` header to the next
// column-zero STEXT header, which is how the compiler delimits them.
func funcAssembly(listing, symbol string) (string, bool) {
	lines := strings.Split(listing, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, symbol+" STEXT") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		l := lines[j]
		if l != "" && !strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, " ") && strings.Contains(l, " STEXT") {
			end = j
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. WHITE-BOX, EQUIVALENCE — the resolved entry equals a direct probe
// ─────────────────────────────────────────────────────────────────────────────

// rowBindProbeOpts builds a buildOpts in which every kind is represented and
// every pairwise collision that a real plan can produce is present:
//
//   - "r"     — an edge variable, and nothing else (the common case).
//   - "rs"    — an edge variable ALSO registered as an aggregate scalar column,
//     which the ladder reaches by falling through a failed reconstruction.
//   - "p"     — a chained path variable.
//   - "pv"    — a VLE path variable that is ALSO a VLE relationship variable,
//     which `MATCH p = (a)-[r*]->(b)` produces.
//   - "agg"   — an aggregate scalar column.
//   - "alias" — a projection-alias scalar column, and also an aggregate one.
//   - "n"     — nothing at all (a plain node).
func rowBindProbeOpts() *buildOpts {
	return &buildOpts{
		edgeVarMeta: map[string]edgeVarInfo{
			"r":  {edgeType: "T", srcCol: 1, edgeCol: 2, dstCol: 3, dirCol: -1},
			"rs": {edgeType: "U", srcCol: 4, edgeCol: 5, dstCol: 6, dirCol: 7},
		},
		pathVarChain: map[string]pathChainInfo{
			"p": {leadingCol: 8, steps: []pathChainStep{{edgeType: "T", srcCol: 8, edgeCol: 9, dstCol: 10}}},
		},
		pathVarMeta: map[string]pathVarInfo{
			"pv": {edgeType: "T", listCol: 11},
		},
		vleRelMeta: map[string]vleRelInfo{
			"pv": {edgeType: "T", listCol: 11},
		},
		scalarCols: map[string]struct{}{
			"rs": {}, "agg": {}, "alias": {},
		},
		projAliasScalarCols: map[string]struct{}{
			"alias": {},
		},
	}
}

func rowBindProbeSchema() map[string]int {
	return map[string]int{"r": 0, "rs": 4, "p": 8, "pv": 11, "agg": 12, "alias": 13, "n": 14}
}

// TestRowBindPlan_ResolvedEntryEqualsADirectProbe is the equivalence proof: for
// every variable of the walk, the resolved entry records exactly the membership
// and exactly the metadata a direct lookup of the same maps returns at the same
// instant. It is the claim the whole change rests on, stated as a test rather
// than as a comment.
func TestRowBindPlan_ResolvedEntryEqualsADirectProbe(t *testing.T) {
	t.Parallel()
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprintf("gated=%v", gated), func(t *testing.T) {
			t.Parallel()
			b := rowBindProbeOpts()
			var use map[string]*nodeScalarUse
			if gated {
				use = map[string]*nodeScalarUse{
					"r": {keys: map[string]struct{}{"since": {}}},
					"n": {keys: map[string]struct{}{"name": {}}},
					// "rs", "p", "pv", "agg" and "alias" deliberately absent:
					// an unreferenced variable is what the demand gate skips.
				}
			}
			plan := newRowBindPlan(newRowSchema(rowBindProbeSchema()), b, nil, use)
			vars := plan.resolved()
			if len(vars) != len(rowBindProbeSchema()) {
				t.Fatalf("resolved %d entries, want %d", len(vars), len(rowBindProbeSchema()))
			}
			for i := range vars {
				v := &vars[i]
				t.Run(v.name, func(t *testing.T) {
					_, wantChain := b.pathVarChain[v.name]
					_, wantVLE := b.pathVarMeta[v.name]
					_, wantVLERel := b.vleRelMeta[v.name]
					_, wantEdge := b.edgeVarMeta[v.name]
					_, wantScalar := b.scalarCols[v.name]
					_, wantAlias := b.projAliasScalarCols[v.name]
					_, wantUsed := use[v.name]
					for _, c := range []struct {
						name string
						got  bool
						want bool
					}{
						{"bindPathChain", v.kind&bindPathChain != 0, wantChain},
						{"bindPathVLE", v.kind&bindPathVLE != 0, wantVLE},
						{"bindVLERel", v.kind&bindVLERel != 0, wantVLERel},
						{"bindEdge", v.kind&bindEdge != 0, wantEdge},
						{"bindScalarCol", v.kind&bindScalarCol != 0, wantScalar},
						{"bindProjAliasScalarCol", v.kind&bindProjAliasScalarCol != 0, wantAlias},
						{"bindScalarUsed", v.kind&bindScalarUsed != 0, wantUsed && gated},
					} {
						if c.got != c.want {
							t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
						}
					}
					if wantEdge && !reflect.DeepEqual(v.meta.edge, b.edgeVarMeta[v.name]) {
						t.Errorf("edge metadata = %+v, want %+v", v.meta.edge, b.edgeVarMeta[v.name])
					}
					if wantVLERel && !reflect.DeepEqual(v.meta.vleRel, b.vleRelMeta[v.name]) {
						t.Errorf("vleRel metadata = %+v, want %+v", v.meta.vleRel, b.vleRelMeta[v.name])
					}
					if wantChain && v.meta.chain.leadingCol != b.pathVarChain[v.name].leadingCol {
						t.Errorf("chain leadingCol = %d, want %d",
							v.meta.chain.leadingCol, b.pathVarChain[v.name].leadingCol)
					}
					if wantVLE && v.meta.path.listCol != b.pathVarMeta[v.name].listCol {
						t.Errorf("path listCol = %d, want %d",
							v.meta.path.listCol, b.pathVarMeta[v.name].listCol)
					}
					// meta is allocated only for an entity kind: a plain scalar or
					// node variable must not pay for one.
					if hasMeta := v.meta != nil; hasMeta != (v.kind&bindEntityKinds != 0) {
						t.Errorf("meta allocated = %v for kind %b", hasMeta, v.kind)
					}
					if gated {
						if got, want := v.use, use[v.name]; got != want {
							t.Errorf("use = %p, want %p", got, want)
						}
					} else if v.use != nil {
						t.Errorf("an UNGATED plan resolved a scalar-use pointer (%p): relUse must "+
							"stay nil there, or the presence-only relationship path (#1638) "+
							"changes behaviour on an eager caller", v.use)
					}
				})
			}
		})
	}
}

// TestRowBindPlan_NilBoptsResolvesToBareNodes pins the nil-buildOpts path the
// legacy builders take: every variable resolves to no kind at all, which is the
// eager upgrade.
func TestRowBindPlan_NilBoptsResolvesToBareNodes(t *testing.T) {
	t.Parallel()
	plan := newRowBindPlan(newRowSchema(map[string]int{"a": 0, "b": 1}), nil, nil, nil)
	for _, v := range plan.resolved() {
		if v.kind != 0 || v.meta != nil || v.use != nil {
			t.Errorf("%s resolved to kind %b meta=%v use=%v over a nil buildOpts", v.name, v.kind, v.meta, v.use)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. DEFERRAL — the resolution observes the POST-rebase maps
// ─────────────────────────────────────────────────────────────────────────────

// TestRowBindPlan_ResolutionIsDeferredPastTheApplyRebase is the test that fails
// on an eager build, and the reason the resolution cannot live in [newRowSchema].
//
// The Apply and hash-join builders REWRITE every edgeVarMeta / pathVarChain /
// pathVarMeta / vleRelMeta entry the inner subtree registered, shifting its
// columns by the outer width, AFTER that subtree — and the row schemas its
// closures captured — has been built (buildOperator's *ir.Apply case and
// [shiftApplyMetaColumns]; it closes Match8 [3]). A plan that resolved when it
// was constructed would freeze the inner-relative columns and reconstruct every
// relationship from the wrong slots.
//
// This simulates that order exactly: construct the plan, THEN mutate the map,
// THEN take the first row.
func TestRowBindPlan_ResolutionIsDeferredPastTheApplyRebase(t *testing.T) {
	t.Parallel()
	b := &buildOpts{edgeVarMeta: map[string]edgeVarInfo{
		"r": {edgeType: "T", srcCol: 0, edgeCol: 1, dstCol: 2, dirCol: -1},
	}}
	plan := newRowBindPlan(newRowSchema(map[string]int{"r": 1}), b, nil, nil)

	// The rebase: the inner subtree's columns shift by the outer width.
	const outerWidth = 7
	info := b.edgeVarMeta["r"]
	info.srcCol += outerWidth
	info.edgeCol += outerWidth
	info.dstCol += outerWidth
	b.edgeVarMeta["r"] = info

	vars := plan.resolved()
	if len(vars) != 1 {
		t.Fatalf("resolved %d entries, want 1", len(vars))
	}
	if got := vars[0].meta.edge; !reflect.DeepEqual(got, info) {
		t.Fatalf("resolved edge metadata = %+v, want the POST-rebase %+v: the plan resolved "+
			"EAGERLY and froze the inner-relative columns", got, info)
	}
}

// TestRowBindPlan_ResolvesExactlyOnce pins that a second row does not re-resolve
// — which is the whole optimisation — and that a mutation AFTER the first row is
// not observed. The second half is not a wish: it is the invariant the deferral
// argument rests on, so it is stated where a future change that mutates a meta
// map at execution time will trip over it.
func TestRowBindPlan_ResolvesExactlyOnce(t *testing.T) {
	t.Parallel()
	b := &buildOpts{scalarCols: map[string]struct{}{}}
	plan := newRowBindPlan(newRowSchema(map[string]int{"x": 0}), b, nil, nil)
	if got := plan.resolved()[0].kind; got != 0 {
		t.Fatalf("first resolution kind = %b, want 0", got)
	}
	b.scalarCols["x"] = struct{}{}
	if got := plan.resolved()[0].kind; got != 0 {
		t.Fatalf("second resolution kind = %b: the plan re-resolved, so the per-row path is "+
			"still paying for the lookup", got)
	}
}

// TestRowBindPlan_ResolvedIsAllocationFree pins that reading a warm plan costs
// nothing. `once.Do(p.resolve)` builds a fresh method value per call, and
// sync.Once.Do hands it to the non-inlinable doSlow, so escape analysis
// heap-allocates it on EVERY call whether or not the slow path is taken — an
// allocation per row on the hottest function in the engine. [newRowBindPlan]
// binds the method value once instead; this is what stops that regressing.
func TestRowBindPlan_ResolvedIsAllocationFree(t *testing.T) {
	plan := newRowBindPlan(newRowSchema(rowBindProbeSchema()), rowBindProbeOpts(), nil, nil)
	_ = plan.resolved() // warm: the one resolution allocates, and is not measured
	if n := testing.AllocsPerRun(200, func() { _ = plan.resolved() }); n != 0 {
		t.Errorf("resolved() allocates %v objects per call on a warm plan, want 0", n)
	}
}

// TestRowBindPlan_ConcurrentResolutionIsSafe exercises the contract that makes a
// plan shareable by the parallel scan/project and hash-join tiers: many
// goroutines may take the first row at once. Meaningful under -race.
func TestRowBindPlan_ConcurrentResolutionIsSafe(t *testing.T) {
	t.Parallel()
	plan := newRowBindPlan(newRowSchema(rowBindProbeSchema()), rowBindProbeOpts(), nil, nil)
	const workers = 32
	var wg sync.WaitGroup
	got := make([][]boundVar, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = plan.resolved()
		}()
	}
	wg.Wait()
	for i := 1; i < workers; i++ {
		if len(got[i]) != len(got[0]) {
			t.Fatalf("worker %d saw %d entries, worker 0 saw %d", i, len(got[i]), len(got[0]))
		}
		for j := range got[0] {
			if got[i][j].name != got[0][j].name || got[i][j].kind != got[0][j].kind {
				t.Fatalf("worker %d entry %d = %+v, worker 0 = %+v", i, j, got[i][j], got[0][j])
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. BEHAVIOURAL — the collision shapes a green TCK does not catch
// ─────────────────────────────────────────────────────────────────────────────

// rowBindCollisionGraph is a reciprocal pair with distinct per-direction payload
// plus a third node, built through Cypher so every edge carries a by-handle type
// record. Distinct payload is what makes a binding taken from the wrong entry
// observable rather than merely present.
func rowBindCollisionGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE (:N {k:'a'})`,
		`CREATE (:N {k:'b'})`,
		`CREATE (:N {k:'c'})`,
		`MATCH (x:N {k:'a'}),(y:N {k:'b'}) CREATE (x)-[:T {w:1, times:10}]->(y)`,
		`MATCH (x:N {k:'b'}),(y:N {k:'a'}) CREATE (x)-[:T {w:2, times:20}]->(y)`,
		`MATCH (x:N {k:'b'}),(y:N {k:'c'}) CREATE (x)-[:T {w:3, times:30}]->(y)`,
		// c has TWO distinct in-edges, which is what gives the Apply-rebase shape
		// below a match at all: `()-[r1]->()<--()` needs a second relationship
		// into the middle node that is not r1 itself.
		`MATCH (x:N {k:'a'}),(y:N {k:'c'}) CREATE (x)-[:T {w:4, times:40}]->(y)`,
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

// TestRowBindPlan_NameCollisionShapes runs the query shapes in which ONE name is
// bound by more than one producer in the SAME plan, or is registered in more than
// one of the maps the ladder consults. These are the shapes that motivate the
// per-fact collision analysis, and they are here because a green TCK does not
// reach them: rmp #2864 established that 3897/3897 held with its demotion rule
// removed, and only a purpose-built test failed.
func TestRowBindPlan_NameCollisionShapes(t *testing.T) {
	t.Parallel()
	eng := NewEngine(rowBindCollisionGraph(t))
	for _, tc := range []struct {
		name string
		q    string
		want []string
	}{
		{
			// Two hops bind `r` in one build with OPPOSITE directions over a
			// reciprocal pair. The second registration wins in edgeVarMeta and
			// both arms then read it; rmp #2864 returned ONE row here.
			"union_all_same_rel_name_opposite_directions",
			//
			// Each arm is pinned to ONE row: `a` has two out-edges, and the order
			// a scan returns them in is unspecified, so an unpinned first arm
			// would make this test flake on row ORDER rather than on the
			// binding it is meant to check.
			`MATCH (n:N {k:'a'})-[r:T]->(m:N {k:'b'}) RETURN r.w AS w
			 UNION ALL
			 MATCH (n:N {k:'a'})<-[r:T]-(m:N {k:'b'}) RETURN r.w AS w`,
			[]string{"w=1", "w=2"},
		},
		{
			// The Match8 [3] shape: the WITH puts the second MATCH under an
			// Apply, so `r1`'s triplet is registered inner-relative and then
			// rebased by the outer width. It returned NULL before the rebase
			// existed, and it returns the wrong sum on any build that resolves
			// the ladder before the rebase runs. 70 = 30 + 40, the two edges
			// into `c` other than the one r1 binds.
			"apply_rebase_shifts_the_edge_triplet",
			`MATCH (a:N {k:'a'}) WITH 1 AS x MATCH (:N)-[r1:T]->()<--() RETURN sum(r1.times) AS s`,
			[]string{"s=70"},
		},
		{
			// One name bound by two hops in ONE pattern: the later registration
			// wins and serves the rows of both.
			"same_rel_name_twice_in_one_pattern",
			`MATCH (a:N {k:'a'})-[r:T]->(b:N {k:'b'})-[r2:T]->(c:N {k:'c'})
			 RETURN r.w AS w1, r2.w AS w2`,
			[]string{"w1=1 w2=3"},
		},
		{
			// A name in edgeVarMeta AND in scalarCols: `r` is a relationship
			// variable, and then an UNWIND element variable.
			//
			// THE EXPECTED VALUE IS A DEFECT, PINNED AS A DIFFERENTIAL, NOT AS A
			// DESIRED ANSWER. The rows should be 1, 2, 3, 4 — the collected
			// weights — and every one comes back NULL. It comes back NULL at
			// f62a3c83 too, MEASURED, so it is not this change's doing and
			// fixing it is not this change's scope; it is recorded separately.
			// The case is here because it is the collision this change resolves,
			// and pinning the behaviour is what makes a future change to it
			// visible instead of silent.
			"unwind_element_reuses_a_relationship_name_PREEXISTING_DEFECT",
			`MATCH (:N)-[r:T]->() WITH collect(r.w) AS ws UNWIND ws AS r RETURN r ORDER BY r`,
			[]string{"r=null", "r=null", "r=null", "r=null"},
		},
		{
			// A named path and its relationship list bind under one plan, and the
			// path variable is in pathVarMeta while the relationship variable is
			// in vleRelMeta.
			"named_vle_path_and_its_rel_list",
			`MATCH p = (a:N {k:'a'})-[rs:T*1..1]->(b)
			 RETURN length(p) AS len, [x IN rs | x.w] AS ws ORDER BY ws`,
			[]string{"len=1 ws=[1]", "len=1 ws=[4]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := relDirRunRows(t, eng, tc.q)
			assertRelDirRows(t, got, tc.want)
		})
	}
}

// TestRowBindPlan_UngatedPlanBindsEveryVariable pins the difference between a
// gated and an ungated plan at the level the demand gate acts on: an ungated
// plan (scalarUse nil) binds every variable of the walk, a gated one binds only
// the ones its analysis names. A change that lost the distinction would either
// resurrect the waste #1630 removed or drop a binding an expression needs.
func TestRowBindPlan_UngatedPlanBindsEveryVariable(t *testing.T) {
	t.Parallel()
	b := &buildOpts{}
	rs := newRowSchema(map[string]int{"a": 0, "b": 1})

	ungated := newRowBindPlan(rs, b, nil, nil)
	gated := newRowBindPlan(rs, b, nil, map[string]*nodeScalarUse{"a": {keys: map[string]struct{}{"k": {}}}})

	for _, v := range ungated.resolved() {
		if v.kind&bindScalarUsed != 0 {
			t.Errorf("ungated plan marked %s as scalar-used", v.name)
		}
	}
	var gatedUsed []string
	for _, v := range gated.resolved() {
		if v.kind&bindScalarUsed != 0 {
			gatedUsed = append(gatedUsed, v.name)
		}
	}
	if len(gatedUsed) != 1 || gatedUsed[0] != "a" {
		t.Errorf("gated plan marked %v as scalar-used, want exactly [a]", gatedUsed)
	}
}
