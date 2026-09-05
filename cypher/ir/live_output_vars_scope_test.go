package ir

// live_output_vars_scope_test.go — direct unit tests for the scope walker's
// subquery arms, added with rmp #2659.
//
// These are internal (package ir) tests on purpose. The three operators under
// test — SemiApply, AntiSemiApply, RollUpApply — return {Outer, Inner} from
// Children() while documenting that only the OUTER side is visible downstream,
// so any walker that descends Children() blindly leaks the subquery's private
// bindings into the enclosing scope. openCypher forbids that leak:
// CIP2015-05-13-EXISTS states that variables introduced in an existential
// subquery "are not available outside the subquery context".
//
// Why not drive these through the translator instead? For SemiApply and
// AntiSemiApply the translator does reach the arms, and
// TestExistsCorrelationSetExcludesPriorSubqueryVars (in the sibling _test
// package) pins that end-to-end. For RollUpApply it does NOT: every
// RollUpApply the translator builds is immediately wrapped in a Projection by
// translateWith / returnClause, and the walker's Projection case stops there
// and returns the projected names. A query-level test for the RollUpApply arm
// would therefore pass whether or not the arm existed — it would be measuring
// the Projection. An earlier draft of this file did exactly that and skipped
// itself; testing the walker directly is what makes the arm falsifiable.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scopeNames returns the walker's result as a sorted slice, for comparison.
func scopeNames(plan LogicalPlan) []string {
	out := make([]string, 0, 8)
	for k := range liveOutputVars(plan) {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestLiveOutputVarsSubqueryArms asserts that each subquery-shaped operator
// exposes its outer side's scope and hides its inner side's bindings.
func TestLiveOutputVarsSubqueryArms(t *testing.T) {
	// A shared outer pipeline binding `a` (scan) and `made` (a write), with no
	// Projection, so the walker must reach the operator under test.
	newOuter := func() LogicalPlan {
		return NewCreateNode("made", nil, "", &NodeByLabelScan{NodeVar: "a", Label: "P"})
	}
	// A shared inner subplan binding `r` and `b` — private to the subquery.
	newInner := func() LogicalPlan {
		return NewExpand("a", "r", nil, DirectionOutgoing, "b", NewArgument([]string{"a"}))
	}

	// Premise guard: the inner subplan really does bind r and b, so their
	// absence below is a scope decision and not an empty inner plan.
	if got := scopeNames(newInner()); !slices.Contains(got, "b") || !slices.Contains(got, "r") {
		t.Fatalf("premise check: the inner subplan does not bind r and b: %v", got)
	}
	// Premise guard: the outer pipeline really does bind both a and made, which
	// is also the #2659 regression in miniature — CreateNode.Vars() alone is
	// just {"made"}, and the walker must still see `a` beneath it.
	wantOuter := []string{"a", "made"}
	if got := scopeNames(newOuter()); !slices.Equal(got, wantOuter) {
		t.Fatalf("premise check: outer pipeline scope is %v, want %v (CreateNode must union its child's live outputs)", got, wantOuter)
	}

	cases := []struct {
		name string
		plan LogicalPlan
		want []string
		why  string
	}{
		{
			name: "SemiApply",
			plan: NewSemiApply(newOuter(), newInner()),
			want: []string{"a", "made"},
			why:  "SemiApply.Vars() documents that only outer variables are visible downstream",
		},
		{
			name: "AntiSemiApply",
			plan: NewAntiSemiApply(newOuter(), newInner()),
			want: []string{"a", "made"},
			why:  "AntiSemiApply carries the identical contract",
		},
		{
			name: "RollUpApply",
			plan: NewRollUpApply(newOuter(), newInner(), "collected"),
			want: []string{"a", "collected", "made"},
			why:  "RollUpApply.Vars() exposes Outer's vars plus CollectVar — the collected list is visible, the inner pattern's variables are not",
		},
		{
			name: "Foreach",
			plan: NewForeach(newOuter(), newInner(), 1),
			want: []string{"a", "made"},
			why:  "Foreach.Vars() is Outer.Vars(): the loop variable and the body's bindings are scoped to the body",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scopeNames(c.plan)
			if !slices.Equal(got, c.want) {
				t.Errorf("scope leak or loss (%s)\n got: %v\nwant: %v", c.why, got, c.want)
			}
			// Spell the leak out separately so a failure names the culprit.
			for _, private := range []string{"r", "b"} {
				if slices.Contains(got, private) {
					t.Errorf("inner-side binding %q leaked into the enclosing scope (%s); got %v", private, c.why, got)
				}
			}
		})
	}
}

// TestLiveOutputVarSliceIsDeterministicAndScoped pins the two properties
// existsSubPlan relies on: the slice is scope-accurate (same membership as the
// walker) and its order is stable across calls, because it becomes
// Argument.Variables, which Explain renders and the plan-stability tests
// compare.
func TestLiveOutputVarSliceIsDeterministicAndScoped(t *testing.T) {
	plan := NewSemiApply(
		NewCreateNode("made", nil, "", &NodeByLabelScan{NodeVar: "a", Label: "P"}),
		NewExpand("a", "r", nil, DirectionOutgoing, "b", NewArgument([]string{"a"})),
	)

	first := liveOutputVarSlice(plan)
	// Membership must equal the walker's, exactly.
	gotSorted := slices.Clone(first)
	slices.Sort(gotSorted)
	if want := scopeNames(plan); !slices.Equal(gotSorted, want) {
		t.Errorf("slice membership diverges from the walker\n got: %v\nwant: %v", gotSorted, want)
	}
	// Order must be identical across repeated calls. A map-iteration-ordered
	// implementation fails this with high probability over 64 attempts.
	for i := range 64 {
		if next := liveOutputVarSlice(plan); !slices.Equal(first, next) {
			t.Fatalf("order is not deterministic: call 0 gave %v, call %d gave %v", first, i+1, next)
		}
	}
	// nil is handled without panicking.
	if got := liveOutputVarSlice(nil); got != nil {
		t.Errorf("liveOutputVarSlice(nil) = %v, want nil", got)
	}
}

// TestLiveOutputVarSliceIntersectionInvariantHolds makes the assumption behind
// liveOutputVarSlice visible.
//
// The helper intersects the ordered walk (collectAllVars) with the scoped walk
// (liveOutputVars). That equals the scope only while every name an arm emits
// also appears in some node's Vars() in the subtree. liveOutputVarSlice repairs
// a violation at runtime so the scope can never be narrowed — but a repair is a
// silent widening of a code path nobody meant to take, so this test asserts the
// fast path is the one actually used.
//
// If a future arm emits a name with no matching Vars() entry, production stays
// correct (the repair fires) and this test fails, which is the intended split:
// safety in the code, visibility in the suite.
func TestLiveOutputVarSliceIntersectionInvariantHolds(t *testing.T) {
	outer := NewCreateNode("made", nil, "", &NodeByLabelScan{NodeVar: "a", Label: "P"})
	inner := NewExpand("a", "r", nil, DirectionOutgoing, "b", NewArgument([]string{"a"}))

	plans := map[string]LogicalPlan{
		"SemiApply":     NewSemiApply(outer, inner),
		"AntiSemiApply": NewAntiSemiApply(outer, inner),
		"RollUpApply":   NewRollUpApply(outer, inner, "collected"),
		"Foreach":       NewForeach(outer, inner, 1),
		"nested":        NewSemiApply(NewSemiApply(outer, inner), inner),
		"projection":    NewSemiApply(NewProjection([]ProjectionItem{{Name: "a"}}, outer), inner),
	}
	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			live := liveOutputVars(plan)
			got := liveOutputVarSlice(plan)
			if len(got) != len(live) {
				t.Errorf("intersection invariant violated: liveOutputVarSlice returned %d names but the scope has %d (%v vs %v). "+
					"An arm is emitting a name that collectAllVars cannot reach; the repair path kept the answer correct, "+
					"but add the name to that operator's Vars() or stop emitting it directly.", len(got), len(live), got, live)
			}
			for _, v := range got {
				if _, ok := live[v]; !ok {
					t.Errorf("liveOutputVarSlice returned %q, which is not in the scope %v", v, live)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Completeness guard
// ─────────────────────────────────────────────────────────────────────────────

// varsDropsAChild classifies every LogicalPlan implementation in this package
// against the invariant that governs liveOutputVars' default branch:
//
//	default computes self.Vars() ∪ ⋃ liveOutputVars(child), which is correct
//	ONLY for operators that forward EVERY child's bindings downstream.
//	Any operator whose Vars() drops a child needs its own arm.
//
// The classification was produced by comparing each operator's Children()
// against its Vars() across the whole package, not by reacting to defects one
// at a time — rmp #2659 was reported three times, each time naming one more
// operator, because the set had never been closed.
//
// Values:
//
//	forwardsAll — Vars() reflects every child, so the default branch is right.
//	hasArm      — Vars() drops a child AND liveOutputVars has a dedicated arm.
//	benign      — Vars() drops a child, no arm, but the leak cannot occur.
//	dead        — Vars() drops a child, no arm, and no production construction
//	              site exists. A trap if one is ever added.
type varsClass int

const (
	forwardsAll varsClass = iota
	hasArm
	benign
	dead
)

// operatorClassification is the closed set. A LogicalPlan implementation that
// is not listed here fails TestLiveOutputVarsCoversEveryOperator, which forces
// whoever adds an operator to decide whether it needs an arm.
var operatorClassification = map[string]varsClass{
	// ── Multi-child, Vars() drops a child → must have an arm ──────────────
	"SemiApply":     hasArm,
	"AntiSemiApply": hasArm,
	"RollUpApply":   hasArm,
	"Foreach":       hasArm,

	// ── Multi-child, Vars() drops a child, no arm ─────────────────────────
	// Union/UnionAll expose Left.Vars() only. The leak cannot occur because
	// every UNION branch is terminated by a Projection (set operands must be
	// union-compatible, so the translator always projects), and the Projection
	// arm stops the walk before Right's internals are reached.
	"Union":    benign,
	"UnionAll": benign,

	// ── Single child which is an INNER subquery side; Vars() returns nil ──
	// The default branch would return the entire inner subtree — a total leak.
	// Neither type has a production construction site today (only
	// cypher/ir/subquery_test.go builds them), so it is unreachable rather
	// than fixed. Wiring either one requires adding an arm at the same time.
	"SubqueryExists": dead,
	"SubqueryCount":  dead,

	// ── Multi-child, Vars() unions BOTH sides → default is correct ────────
	"Apply":           forwardsAll,
	"CorrelatedApply": forwardsAll,
	"OptionalApply":   forwardsAll,

	// ── Row re-shapers: own arm, stop the walk ────────────────────────────
	"Projection":       hasArm,
	"EagerAggregation": hasArm,

	// ── Passthroughs: own arm (descend to child, add nothing) ─────────────
	"Selection": hasArm, "Sort": hasArm, "Top": hasArm, "Limit": hasArm,
	"Distinct": hasArm, "Skip": hasArm, "NamedPath": hasArm,
	"Eager": hasArm, "ProduceResults": hasArm,

	// ── Appenders and leaves: output row = child's row + own bindings, so
	//    the default branch is correct by construction. ───────────────────
	"AllNodesScan": forwardsAll, "Argument": forwardsAll,
	"NodeByLabelScan": forwardsAll, "NodeByIndexSeek": forwardsAll,
	"NodeByIndexRangeScan": forwardsAll, "Expand": forwardsAll,
	"OptionalExpand": forwardsAll, "VarLengthExpand": forwardsAll,
	"ShortestPath": forwardsAll, "ProjectEndpoints": forwardsAll,
	"Unwind": forwardsAll, "ProcedureCall": forwardsAll,
	"CreateNode": forwardsAll, "CreateRelationship": forwardsAll,
	"SetProperty": forwardsAll, "SetAllProperties": forwardsAll,
	"SetLabels": forwardsAll, "RemoveProperty": forwardsAll,
	"RemoveLabels": forwardsAll, "DeleteNode": forwardsAll,
	"DeleteRelationship": forwardsAll, "DetachDelete": forwardsAll,
	"Merge": forwardsAll, "MergePattern": forwardsAll,
	"MergeRelationship": forwardsAll,
	"CreateIndex":       forwardsAll, "DropIndex": forwardsAll,
	"CreateConstraint": forwardsAll, "DropConstraint": forwardsAll,
	// DDL leaves: both Children() and Vars() return nil, so there is no child
	// to drop and the default branch is trivially correct. These two were
	// missed by a hand-rolled regex sweep of the package (they declare their
	// methods on an anonymous receiver, `func (*ShowIndexes) Children()`) and
	// were found only by the AST walk below — which is the reason this guard
	// parses the source instead of trusting a pattern match.
	"ShowConstraints": forwardsAll, "ShowIndexes": forwardsAll,
}

// TestLiveOutputVarsCoversEveryOperator closes the set by construction.
//
// It parses this package's own source, finds every type that implements
// LogicalPlan (i.e. declares both Children() and Vars()), and requires each one
// to appear in operatorClassification. Adding a new operator therefore breaks
// this test until its author states which class it belongs to — which is the
// point. Review caught the missing arms three times; this catches the fourth.
func TestLiveOutputVarsCoversEveryOperator(t *testing.T) {
	// Every non-test .go file in this directory. parser.ParseFile over an
	// explicit glob is used rather than the deprecated parser.ParseDir; it also
	// suits this sweep better, because it deliberately ignores build tags —
	// an operator behind a tag still needs classifying.
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package source: %v", err)
	}
	fset := token.NewFileSet()

	// A type implements LogicalPlan when it declares both methods. Receivers
	// may be named or anonymous (`func (*ShowIndexes) Children()`), so match on
	// the receiver TYPE and never on a receiver identifier.
	methods := map[string]map[string]bool{}
	parsed := 0
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, src, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", src, perr)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok {
				continue
			}
			if n := fn.Name.Name; n == "Children" || n == "Vars" {
				if methods[ident.Name] == nil {
					methods[ident.Name] = map[string]bool{}
				}
				methods[ident.Name][n] = true
			}
		}
	}
	if parsed == 0 {
		t.Fatal("premise check: parsed no source files, so this test would pass vacuously")
	}

	var impls []string
	for typ, m := range methods {
		if m["Children"] && m["Vars"] {
			impls = append(impls, typ)
		}
	}
	slices.Sort(impls)

	// Premise guard: if the AST walk found nothing, the test would pass
	// vacuously. The package is known to hold dozens of operators.
	if len(impls) < 40 {
		t.Fatalf("premise check: found only %d LogicalPlan implementations (%v) — the source walk is broken, "+
			"so this test would pass without checking anything", len(impls), impls)
	}
	t.Logf("parsed %d source files; LogicalPlan implementations discovered: %d", parsed, len(impls))

	for _, typ := range impls {
		if _, ok := operatorClassification[typ]; !ok {
			t.Errorf("operator %q implements LogicalPlan but is not classified in operatorClassification.\n"+
				"Decide which class it belongs to: does its Vars() drop a child?\n"+
				"  - No  → forwardsAll (liveOutputVars' default branch is correct).\n"+
				"  - Yes → it needs its own arm in liveOutputVars, then mark it hasArm.\n"+
				"Getting this wrong is a SILENT WRONG ANSWER: a dropped child that is still walked "+
				"leaks private bindings into the enclosing scope (rmp #2659).", typ)
		}
	}
	// And the reverse: a stale entry means an operator was renamed or removed.
	for typ := range operatorClassification {
		if !slices.Contains(impls, typ) {
			t.Errorf("operatorClassification lists %q, which no longer implements LogicalPlan in this package — "+
				"remove the stale entry", typ)
		}
	}
}
