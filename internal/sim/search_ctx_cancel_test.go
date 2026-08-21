package sim

// search_ctx_cancel_test.go — the tripwires that turn the cancellation battery's
// coverage claim into an assertion, plus one deliberately-failing construction
// per clause so no clause in the family is delivered unfalsified (rmp #2489).

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/search"
)

// ctxCancelPkgDirs maps each family to the source directory the tripwires parse,
// relative to this package's directory (the working directory of a `go test`
// run). Adding a sixth family to the battery means adding it here too, and
// [TestSearchCtxCancel_TableCoversEveryEntryPoint] fails until it is.
var ctxCancelPkgDirs = map[ctxFamily]string{
	famSearch:     "../../search",
	famCentrality: "../../search/centrality",
	famCommunity:  "../../search/community",
	famFlow:       "../../search/flow",
	famExtern:     "../../search/extern",
}

// ctxSourceDecl is one parsed exported declaration from a family's package.
type ctxSourceDecl struct {
	fn       *ast.FuncDecl
	key      string // "Name" or "Receiver.Name"
	file     string
	firstCtx bool // first parameter is a context.Context
	lastErr  bool // last result is an error
}

// ctxParseFamily parses one family's package (excluding _test.go) and returns
// every exported top-level function and every method on an exported type, keyed
// as [ctxSourceDecl.key]. Build constraints are deliberately NOT applied: every
// non-test .go file in the directory is parsed, so a `//go:build`-gated file's
// declarations are still seen. That is the conservative direction — an entry
// point behind a tag is still part of the surface someone can call.
func ctxParseFamily(t *testing.T, dir string) map[string]ctxSourceDecl {
	t.Helper()
	// os.ReadDir + parser.ParseFile rather than parser.ParseDir: the latter is
	// deprecated as of Go 1.25 in favour of golang.org/x/tools/go/packages, which
	// this module does not depend on and which would apply build constraints —
	// the opposite of what this tripwire wants (see the note above).
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]ctxSourceDecl{}
	for _, ent := range ents {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s/%s: %v", dir, name, perr)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !ast.IsExported(fn.Name.Name) {
				continue
			}
			recv := ctxRecvTypeName(fn)
			if recv != "" && !ast.IsExported(recv) {
				continue // a method on an unexported type is not public API
			}
			key := fn.Name.Name
			if recv != "" {
				key = recv + "." + fn.Name.Name
			}
			out[key] = ctxSourceDecl{
				fn:       fn,
				key:      key,
				file:     name,
				firstCtx: ctxFirstParamIsContext(fn),
				lastErr:  ctxLastResultIsError(fn),
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no exported declarations from %s: the tripwire is looking at the wrong directory and would pass vacuously", dir)
	}
	return out
}

// ctxRecvTypeName returns the receiver's base type name (through a pointer and
// through generic instantiation), or "" for a top-level function.
func ctxRecvTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch e := typ.(type) {
	case *ast.IndexExpr:
		typ = e.X
	case *ast.IndexListExpr:
		typ = e.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// ctxFirstParamIsContext reports whether fn's first parameter is a
// context.Context. This — not the name suffix — is the battery's definition of a
// context-accepting entry point.
func ctxFirstParamIsContext(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}
	sel, ok := fn.Type.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "context" && sel.Sel.Name == "Context"
}

// ctxLastResultIsError reports whether fn's last result is an error.
func ctxLastResultIsError(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
		return false
	}
	last := fn.Type.Results.List[len(fn.Type.Results.List)-1]
	id, ok := last.Type.(*ast.Ident)
	return ok && id.Name == "error"
}

// TestSearchCtxCancel_TableCoversEveryEntryPoint is the battery's coverage
// tripwire, and the reason "every public context-accepting entry point is
// covered" is a claim rather than a hope.
//
// It enumerates, from source, every exported function and exported-receiver
// method in the five families whose FIRST PARAMETER is a context.Context, and
// requires the set to equal [ctxEntries]'s key set exactly. A hand-maintained
// list rots the moment someone adds an entry point; this fails at that moment
// instead.
//
// Enumerating on the parameter type rather than on a `...Ctx` name suffix is
// load-bearing, and was not free: the name-pattern inventory this task started
// from missed five real entry points — search.AStarInto, search.BellmanFordInto,
// search.DijkstraInto, search.KShortestPathsLooplessCtxWithOpts (which is THE
// implementation the two `...Ctx` k-shortest forms delegate to) and
// centrality.PageRanker.Run (one of only two genuinely independent
// implementations in the whole surface). A suffix guard would have declared
// full coverage over 53 of 58.
func TestSearchCtxCancel_TableCoversEveryEntryPoint(t *testing.T) {
	t.Parallel()

	var want []string
	for fam, dir := range ctxCancelPkgDirs {
		for _, d := range ctxParseFamily(t, dir) {
			if d.firstCtx {
				want = append(want, string(fam)+"."+d.key)
			}
		}
	}
	sort.Strings(want)

	got := make([]string, 0, len(want))
	for _, e := range ctxEntries() {
		got = append(got, ctxEntryKey(&e))
	}
	sort.Strings(got)

	// Two rows are named explicitly, because they are the two the obvious
	// enumerator shapes lose and their loss would not otherwise be visible: a
	// `...Ctx` suffix filter drops PageRanker.Run, and a package-functions-only
	// walk drops SSSP.FromCtx. Naming them makes a future narrowing of either the
	// enumerator above or the table itself fail here rather than silently shrink
	// the surface while still reporting "equal".
	for _, must := range []string{"search/centrality.PageRanker.Run", "search.SSSP.FromCtx"} {
		if !slices.Contains(want, must) {
			t.Errorf("%s was not enumerated from source: the enumerator has narrowed (a ...Ctx name filter loses PageRanker.Run; "+
				"a package-functions-only walk loses the SSSP.FromCtx method) and the coverage claim no longer covers it", must)
		}
		if !slices.Contains(got, must) {
			t.Errorf("%s is missing from ctxEntries: it is a public context-accepting entry point and must be driven", must)
		}
	}

	if slices.Equal(got, want) {
		return
	}
	missing := ctxSetDiff(want, got)
	extra := ctxSetDiff(got, want)
	t.Errorf("the cancellation table does not match the public context-accepting surface.\n"+
		"  in source but NOT in the table (%d): %v\n"+
		"  in the table but NOT in source (%d): %v\n\n"+
		"Every exported function, and every method on an exported type, whose first parameter is a "+
		"context.Context is part of this surface — whether or not its name ends in \"Ctx\". "+
		"Add the missing rows to ctxEntries (each needs a fixture that makes its answer non-zero, or "+
		"the pre-cancelled arm's value clause cannot fail on it), or delete the stale ones. "+
		"Do NOT relax this test: it is the whole basis of the family's coverage claim.",
		len(missing), missing, len(extra), extra)
}

// ctxSetDiff returns the elements of a that are absent from b.
func ctxSetDiff(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, s := range b {
		inB[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := inB[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// ctxTwinDeclName returns the source-level declaration the identity arm's twin
// closure calls, or "" when the row declares no counterpart.
func ctxTwinDeclName(e *ctxEntry) string {
	if e.twin == nil {
		return ""
	}
	if e.twinDecl != "" {
		return e.twinDecl
	}
	return strings.TrimSuffix(e.Name, "Ctx")
}

// ctxBareName drops a "Receiver." prefix from a table key, leaving the bare
// identifier a call site actually names.
func ctxBareName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// ctxCallsWithBackground reports whether fn's body contains a call to a callee
// named want whose first argument is context.Background() or context.TODO().
func ctxCallsWithBackground(fn *ast.FuncDecl, want string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if ctxCalleeName(call.Fun) != want {
			return true
		}
		inner, ok := call.Args[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "context" &&
			(sel.Sel.Name == "Background" || sel.Sel.Name == "TODO") {
			found = true
		}
		return true
	})
	return found
}

// ctxCalleeName reduces a call target to its bare identifier, seeing through a
// selector (pkg.F, x.M) and through generic instantiation (F[T], F[T, U]).
func ctxCalleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.IndexExpr:
		return ctxCalleeName(f.X)
	case *ast.IndexListExpr:
		return ctxCalleeName(f.X)
	}
	return ""
}

// ctxForwardsTo reports whether fn's body calls a callee named want at all
// (ignoring its arguments). Used to follow the one transitive delegation in the
// surface: search.EppsteinKShortest forwards to search.KShortestPathsLoopless,
// which is itself the context.Background() delegation.
func ctxForwardsTo(fn *ast.FuncDecl, want string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && ctxCalleeName(call.Fun) == want {
			found = true
		}
		return true
	})
	return found
}

// TestSearchCtxCancel_TwinIsAStructuralDelegation keeps the family's vacuity
// LABELS honest.
//
// The battery documents that for all but two rows the Background-identity arm is
// guaranteed by construction, because the counterpart's whole body is a
// delegation to the context-aware form with context.Background(). That claim is
// a statement about the source, so it is checked against the source:
//
//   - every row marked twinIsDelegation must in fact delegate (directly, or
//     transitively through one other exported non-Ctx entry in the same package);
//   - every row NOT so marked, but which has a counterpart, must in fact NOT
//     delegate — otherwise the battery is advertising a load-bearing arm that is
//     really guaranteed, which is the more dangerous of the two errors.
//
// When a twin stops delegating, its identity arm becomes a genuine
// cross-implementation oracle and the file header's vacuity paragraph becomes a
// false claim in shipped documentation. This test fails at that moment.
func TestSearchCtxCancel_TwinIsAStructuralDelegation(t *testing.T) {
	t.Parallel()
	parsed := map[ctxFamily]map[string]ctxSourceDecl{}
	for fam, dir := range ctxCancelPkgDirs {
		parsed[fam] = ctxParseFamily(t, dir)
	}

	delegating, loadBearing := 0, 0
	for _, e := range ctxEntries() {
		twinName := ctxTwinDeclName(&e)
		if twinName == "" {
			if e.twinIsDelegation {
				t.Errorf("%s: twinIsDelegation is set but the row declares no counterpart (twin == nil), so the label describes nothing",
					ctxEntryKey(&e))
			}
			continue
		}
		decl, ok := parsed[e.Family][twinName]
		if !ok {
			t.Errorf("%s: declared counterpart %q does not exist in %s; the twin closure and the tripwire disagree about what it compares against",
				ctxEntryKey(&e), twinName, ctxCancelPkgDirs[e.Family])
			continue
		}
		// Callee names in the AST are bare identifiers: a method declared as
		// SSSP.FromCtx is called as s.FromCtx, so the receiver prefix must be
		// dropped before matching. Getting this wrong made the tripwire report
		// SSSP.From as a non-delegation on its first run.
		direct := ctxCallsWithBackground(decl.fn, ctxBareName(e.Name))
		transitive := false
		if !direct {
			for other, od := range parsed[e.Family] {
				if other == twinName || od.firstCtx {
					continue
				}
				if ctxForwardsTo(decl.fn, ctxBareName(other)) && ctxCallsWithBackground(od.fn, ctxBareName(other)+"Ctx") {
					transitive = true
					break
				}
			}
		}
		isDelegation := direct || transitive
		switch {
		case e.twinIsDelegation && !isDelegation:
			t.Errorf("%s: marked twinIsDelegation, but %s (%s) no longer reaches its result by calling %s with context.Background(). "+
				"Its identity arm has just become a REAL cross-implementation oracle: clear twinIsDelegation and correct the "+
				"\"guaranteed by construction\" paragraph in search_ctx_cancel.go's header",
				ctxEntryKey(&e), twinName, decl.file, e.Name)
		case !e.twinIsDelegation && isDelegation:
			t.Errorf("%s: NOT marked twinIsDelegation, but %s (%s) does delegate to %s with context.Background(), so its identity "+
				"arm is guaranteed and cannot fail. Set twinIsDelegation so the label matches, or the battery advertises an "+
				"oracle it does not have",
				ctxEntryKey(&e), twinName, decl.file, e.Name)
		}
		if isDelegation {
			delegating++
		} else {
			loadBearing++
		}
	}

	// The tripwire must be watching something in BOTH directions: if every row
	// were a delegation the load-bearing branch above would never run, and if
	// none were the guaranteed branch would never run.
	if delegating == 0 {
		t.Error("no counterpart in the table is a structural delegation: the vacuity label is no longer describing anything and this tripwire has gone half-blind")
	}
	if loadBearing == 0 {
		t.Error("every counterpart in the table is a structural delegation: no identity arm in the family can fail, so the arm proves nothing about any implementation")
	}
}

// TestSearchCtxCancel_TwinErrorArityMatchesSource pins the twinReportsErr flag
// to the counterpart's real signature.
//
// The flag decides whether the identity arm compares errors at all, and it
// encodes a real property of this surface: roughly twenty non-Ctx counterparts
// CANNOT report an error, so they silently swallow whatever the context-aware
// form would have returned — search.FloydWarshall discards
// search.ErrNegativeCycle exactly this way, which is why the DST had to call
// FloydWarshallCtx to see that sentinel at all. A stale flag would either
// compare an error that does not exist or stop comparing one that does.
func TestSearchCtxCancel_TwinErrorArityMatchesSource(t *testing.T) {
	t.Parallel()
	parsed := map[ctxFamily]map[string]ctxSourceDecl{}
	for fam, dir := range ctxCancelPkgDirs {
		parsed[fam] = ctxParseFamily(t, dir)
	}
	swallowing := 0
	for _, e := range ctxEntries() {
		twinName := ctxTwinDeclName(&e)
		if twinName == "" {
			if e.twinReportsErr {
				t.Errorf("%s: twinReportsErr is set but the row declares no counterpart", ctxEntryKey(&e))
			}
			continue
		}
		decl, ok := parsed[e.Family][twinName]
		if !ok {
			continue // already reported by the delegation tripwire
		}
		if decl.lastErr != e.twinReportsErr {
			t.Errorf("%s: twinReportsErr=%t but %s (%s) %s an error as its last result. "+
				"A counterpart that cannot return an error SWALLOWS the context-aware form's error, and the identity arm must "+
				"then compare values only",
				ctxEntryKey(&e), e.twinReportsErr, twinName, decl.file,
				map[bool]string{true: "does return", false: "does not return"}[decl.lastErr])
		}
		if !decl.lastErr {
			swallowing++
		}
	}
	if swallowing == 0 {
		t.Error("no counterpart in the table swallows errors any more: every non-Ctx sibling now reports one. That is a real API change — " +
			"update the header's error-swallowing note rather than this test")
	}
}

// TestSearchCtxCancel_RunsCleanAndIsDeterministic drives the whole battery over
// several ticks and requires it to be both clean and bit-reproducible: the same
// tick must produce the same fixtures and hence the same verdict, which is what
// lets a DST failure replay.
func TestSearchCtxCancel_RunsCleanAndIsDeterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{1, 200, 400, 600, 800, 4242} {
		first := ctxRenderViolations(searchCtxCancelViolations(tick))
		if first != "" {
			t.Errorf("tick %d: the cancellation battery reported violations on a clean tree:\n%s", tick, first)
		}
		second := ctxRenderViolations(searchCtxCancelViolations(tick))
		if first != second {
			t.Errorf("tick %d: the battery is not deterministic — two runs of the same tick disagreed.\n  first:\n%s\n  second:\n%s",
				tick, first, second)
		}
	}
}

// ctxRenderViolations joins a violation slice into a stable, comparable string.
func ctxRenderViolations(vs []Violation) string {
	parts := make([]string, 0, len(vs))
	for i := range vs {
		parts = append(parts, vs[i].String())
	}
	return strings.Join(parts, "\n")
}

// TestSearchCtxCancel_EveryClauseCanFail is the family's assert-something-was-
// seen gate: it constructs, for each clause the battery owns, an entry point that
// breaks exactly that clause, and requires the clause to fire.
//
// Without it every clause would be delivered green and unfalsified, which is the
// standard way a cancellation oracle turns out to have been unable to fail all
// along. The synthetic rows below are the permanent witness; the real-code witness
// is separate and quoted in the task record — before rmp #2593 corrected their
// poll ordering, the cancel-err and cancel-val clauses fired on seven genuine
// entry points (BellmanFordCtx, BellmanFordInto, KCoreCtx,
// KShortestPathsLooplessCtx, KShortestPathsLooplessCtxWithOpts,
// EppsteinKShortestCtx, PushRelabelMaxFlowCtx) at all six ticks tried.
func TestSearchCtxCancel_EveryClauseCanFail(t *testing.T) {
	t.Parallel()
	f, cleanup, vs := newCtxFixtures(7)
	if len(vs) > 0 {
		t.Fatalf("fixture construction failed: %s", ctxRenderViolations(vs))
	}
	defer cleanup()

	sentinel := errors.New("probe: synthetic failure")

	cases := []struct {
		name   string
		entry  ctxEntry
		clause string
	}{
		{
			name:   "bg-err fires when the entry point errors under Background",
			clause: "[bg-err]",
			entry: ctxEntry{
				Name: "ProbeCtx", Family: famSearch,
				run: func(context.Context, *ctxFixtures) (string, error) { return "v", sentinel },
			},
		},
		{
			name:   "twin-err fires when the counterpart errors",
			clause: "[twin-err]",
			entry: ctxEntry{
				Name: "ProbeCtx", Family: famSearch, twinReportsErr: true,
				run:  func(ctx context.Context, _ *ctxFixtures) (string, error) { return "v", ctx.Err() },
				twin: func(*ctxFixtures) (string, error) { return "v", sentinel },
			},
		},
		{
			name:   "identity fires when the two sides disagree",
			clause: "[identity]",
			entry: ctxEntry{
				Name: "ProbeCtx", Family: famSearch,
				run:  func(ctx context.Context, _ *ctxFixtures) (string, error) { return "left", ctx.Err() },
				twin: func(*ctxFixtures) (string, error) { return "right", nil },
			},
		},
		{
			name:   "cancel-err fires when a cancelled context is ignored",
			clause: "[cancel-err]",
			entry: ctxEntry{
				Name: "ProbeCtx", Family: famSearch,
				// Returns a DIFFERENT value on the second call, so cancel-val is
				// satisfied and only cancel-err can be the clause that fires.
				run: ctxProbeAlternating(nil),
			},
		},
		{
			name:   "cancel-err fires on a non-nil error that is not a cancellation",
			clause: "[cancel-err]",
			entry: ctxEntry{
				Name: "ProbeCtx", Family: famSearch,
				// The classic mis-scored row: err != nil would have passed this.
				run: ctxProbeAlternating(sentinel),
			},
		},
		{
			name:   "cancel-val fires when cancellation changes nothing observable",
			clause: "[cancel-val]",
			entry: ctxEntry{
				Name: "ProbeCtx", Family: famSearch,
				run: func(ctx context.Context, _ *ctxFixtures) (string, error) {
					// Honours the context but returns the full answer anyway.
					return "the complete answer", ctx.Err()
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ctxRenderViolations(ctxCancelCheckEntry(11, &tc.entry, f))
			if !strings.Contains(got, tc.clause) {
				t.Errorf("clause %s did not fire on a construction that breaks it — the clause cannot fail and proves nothing.\ngot: %q",
					tc.clause, got)
			}
		})
	}

	// The mirror of the above: a row that honours everything must produce NOTHING,
	// so the clauses are not firing unconditionally.
	clean := ctxEntry{
		Name: "ProbeCtx", Family: famSearch, twinReportsErr: true,
		run: func(ctx context.Context, _ *ctxFixtures) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return "answer", nil
		},
		twin: func(*ctxFixtures) (string, error) { return "answer", nil },
	}
	if got := ctxRenderViolations(ctxCancelCheckEntry(11, &clean, f)); got != "" {
		t.Errorf("a compliant entry point produced violations, so the clauses fire unconditionally:\n%s", got)
	}
}

// ctxProbeAlternating returns a run func that ignores its context, yielding a
// different value on each successive call so the cancel-val clause is satisfied
// and cancel-err is isolated as the only clause under test. err is what it
// returns on the second (pre-cancelled) call.
func ctxProbeAlternating(err error) func(context.Context, *ctxFixtures) (string, error) {
	n := 0
	return func(context.Context, *ctxFixtures) (string, error) {
		n++
		if n == 1 {
			return "first", nil
		}
		return fmt.Sprintf("call-%d", n), err
	}
}

// TestSearchCtxCancel_TableSanityClausesCanFail does for the driver's own
// anti-vacuity guards what the test above does for the per-entry clauses.
func TestSearchCtxCancel_TableSanityClausesCanFail(t *testing.T) {
	t.Parallel()
	full := ctxEntries()
	if got := ctxCancelTableSanity(1, full); len(got) != 0 {
		t.Fatalf("the real table failed its own sanity clauses:\n%s", ctxRenderViolations(got))
	}

	// Dropping a whole family must be caught.
	withoutFlow := make([]ctxEntry, 0, len(full))
	for _, e := range full {
		if e.Family != famFlow {
			withoutFlow = append(withoutFlow, e)
		}
	}
	if got := ctxRenderViolations(ctxCancelTableSanity(1, withoutFlow)); !strings.Contains(got, "search/flow family contributes no entry point") {
		t.Errorf("dropping the whole flow family did not trip the table-sanity guard: %q", got)
	}

	// Dropping every concurrent reduction must be caught.
	withoutReductions := make([]ctxEntry, 0, len(full))
	for _, e := range full {
		e.concurrentReduction = false
		withoutReductions = append(withoutReductions, e)
	}
	if got := ctxRenderViolations(ctxCancelTableSanity(1, withoutReductions)); !strings.Contains(got, "concurrent-reduction entry point") {
		t.Errorf("clearing every concurrentReduction flag did not trip the table-sanity guard: %q", got)
	}

	// Turning every identity arm into a delegation must be caught.
	allDelegations := make([]ctxEntry, 0, len(full))
	for _, e := range full {
		e.twinIsDelegation = true
		allDelegations = append(allDelegations, e)
	}
	if got := ctxRenderViolations(ctxCancelTableSanity(1, allDelegations)); !strings.Contains(got, "structural delegation") {
		t.Errorf("marking every counterpart as a delegation did not trip the table-sanity guard: %q", got)
	}
}

// TestSearchCtxCancel_NoGoroutineLeak is the teardown arm.
//
// Nine of the entry points start goroutines, and the set is NOT derivable from
// the name: search.DiameterCtx fans out across workers without carrying
// "Parallel" in its name, and both PageRank implementations start a worker pool.
// This test drives every such row under Background AND under a pre-cancelled
// context, then requires the process to be back to its baseline goroutine set.
//
// # What this proves, and what it does not
//
// It proves the pools do not leak INDEFINITELY. It does NOT prove they are
// joined before the call returns: goleak samples the goroutine set at one
// instant and retries for a short while, so a worker that exits slightly late
// still passes. A promptness claim would need a different instrument, and a
// goroutine-COUNT comparison is not it — the count is a quantity the runtime
// decides, so a delta assertion flakes in both directions and belongs to the
// class of gate this package has removed three times (rmp #2587, #2589, #2591).
//
// It also cannot use t.Parallel: a parallel test's parent parks in
// testing.tRunner.func1, which goleak does not ignore, so the two are
// structurally incompatible.
func TestSearchCtxCancel_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	f, cleanup, vs := newCtxFixtures(97)
	if len(vs) > 0 {
		t.Fatalf("fixture construction failed: %s", ctxRenderViolations(vs))
	}
	defer cleanup()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	spawning := 0
	for _, e := range ctxEntries() {
		if !e.spawns {
			continue
		}
		spawning++
		if _, err := e.run(context.Background(), f); err != nil {
			t.Errorf("%s: errored under context.Background(): %v", ctxEntryKey(&e), err)
		}
		// The return value is deliberately unexamined here: what this arm is for
		// is the goroutine set after the call, and the cancellation contract is
		// adjudicated by searchCtxCancelViolations, not twice.
		_, _ = e.run(cancelled, f)
	}
	if spawning == 0 {
		t.Fatal("no entry point in the table is marked as spawning goroutines, so this teardown arm exercised nothing")
	}
	t.Logf("drove %d goroutine-spawning entry points under Background and under a pre-cancelled context", spawning)
}

// TestSearchCtxCancel_PrecedenceClausesCanFail is the assert-something-was-seen
// gate for the cancellation-precedence arm: each of its two clauses is shown to
// fire on a row constructed to break exactly that clause.
func TestSearchCtxCancel_PrecedenceClausesCanFail(t *testing.T) {
	t.Parallel()
	if got := ctxRenderViolations(ctxCancelPrecedenceViolations(3)); got != "" {
		t.Fatalf("the real precedence table reported violations on a clean tree:\n%s", got)
	}
	// The arm must refuse to be empty, or removing every row would read as a pass.
	if got := ctxRenderViolations(ctxCancelPrecedenceViolations(3)); strings.Contains(got, "table is empty") {
		t.Fatal("unexpected empty-table violation on the real table")
	}
	if len(ctxCancelPrecedenceRows()) == 0 {
		t.Fatal("the precedence table is empty")
	}

	// prec-setup: an input that does NOT reach the terminal path must be caught,
	// because the precedence clause would then have nothing to outrank. A DAG
	// yields no ErrCycle.
	dag := ctxWeightedCSR(3, [][2]int{{0, 1}, {1, 2}}, []float64{1, 1})
	setupBroken := ctxCancelPrecedenceRow{
		name:     "probe",
		sentinel: search.ErrCycle,
		run: func(ctx context.Context) error {
			_, err := search.TopologicalSortCtx(ctx, dag)
			return err
		},
	}
	if got := ctxRenderPrecedence(3, setupBroken); !strings.Contains(got, "[prec-setup]") {
		t.Errorf("prec-setup did not fire on an input that never reaches the terminal path: %q", got)
	}

	// prec-order: a row whose entry point returns its sentinel even under a
	// cancelled context must be caught.
	//
	// This witness is INJECTED, and it did not start that way. Until rmp #2593 the
	// witness here was real code: search.JohnsonAPSPCtx returned
	// search.ErrNegativeCycle on a negative-cycle graph under a pre-cancelled
	// context, and driving it through the adjudicator was what proved this clause
	// could fire. #2593 fixed that site, which removed the clause's only natural
	// subject — no entry point in the module now prefers a terminal sentinel to a
	// dead context, which is precisely the state the clause exists to defend.
	//
	// So the witness is a stub that reproduces the defective ORDERING (decide the
	// input is unusable, then never consult the context) while the adjudicator
	// under test stays the real one. Do NOT "restore realism" here by pointing this
	// back at Johnson or any other entry point: the correct behaviour of every
	// entry point is now exactly what makes a real witness impossible, and
	// reintroducing one would mean reintroducing the bug.
	//
	// The stub returns a NON-NIL sentinel on purpose, which makes this gate also
	// prove that prec-order tests error IDENTITY and not non-nilness: rewriting the
	// clause as `if err == nil` — the shape this repo has been burned by, and which
	// still stands in search/centrality/security_brandes_goroutine_bound_test.go —
	// makes this assertion fail with `prec-order did not fire ...: ""`. Verified.
	orderBroken := ctxCancelPrecedenceRow{
		name:     "probe: decides the input is unusable without consulting ctx",
		sentinel: search.ErrNegativeCycle,
		run:      func(context.Context) error { return search.ErrNegativeCycle },
	}
	if got := ctxRenderPrecedence(3, orderBroken); !strings.Contains(got, "[prec-order]") {
		t.Errorf("prec-order did not fire on an entry point that returns its sentinel under a cancelled context: %q", got)
	}

	// ...and the mirror, so prec-order is not firing unconditionally: a stub with
	// the CORRECT ordering — context first, sentinel second — must produce nothing.
	orderCorrect := ctxCancelPrecedenceRow{
		name:     "probe: consults ctx before deciding",
		sentinel: search.ErrNegativeCycle,
		run: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return search.ErrNegativeCycle
		},
	}
	if got := ctxRenderPrecedence(3, orderCorrect); got != "" {
		t.Errorf("a correctly-ordered stub produced precedence violations, so the clauses fire unconditionally: %s", got)
	}
}

// ctxRenderPrecedence runs one precedence row through the same two clauses the
// checker applies and renders the result.
func ctxRenderPrecedence(tick int64, row ctxCancelPrecedenceRow) string {
	saved := ctxCancelPrecedenceOverride
	ctxCancelPrecedenceOverride = []ctxCancelPrecedenceRow{row}
	defer func() { ctxCancelPrecedenceOverride = saved }()
	return ctxRenderViolations(ctxCancelPrecedenceViolations(tick))
}
