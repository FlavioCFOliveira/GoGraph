package ir_test

// exists_write_correlation_test.go — plan-shape regression for rmp #2659.
//
// The runtime behaviour is pinned in cypher/exists_write_correlation_test.go.
// This file pins the PLANNER DECISION that produced it, so a future change that
// reintroduces the wrong shape is caught at the IR level, where the diagnosis is
// unambiguous, rather than only as a wrong answer three layers downstream.
//
// The defect: for a correlated EXISTS in the WHERE of a WITH that follows an
// entity-creating write clause, the correlation set was read from the outer
// plan's own Vars(). LogicalPlan.Vars is contracted to report only the
// variables an operator introduces, and the top operator of a writing pipeline
// is CreateNode, whose Vars() is just the created node's synthetic name. The
// outer variable therefore never reached the inner Argument, and the subquery's
// reference to it was planned as a fresh AllNodesScan joined by a Cartesian
// product instead of an Expand from an Argument joined by a CorrelatedApply.
//
//	before (defective)                     after (correct)
//	SemiApply                              SemiApply
//	├─ CreateNode                          ├─ CreateNode
//	│  └─ NodeByLabelScan [a:P]            │  └─ NodeByLabelScan [a:P]
//	└─ CartesianProduct                    └─ CorrelatedApply
//	   ├─ Argument            <- no `a`       ├─ Argument [.., a]
//	   └─ Selection                           └─ Selection
//	      └─ Expand                              └─ Expand
//	         └─ AllNodesScan [a]  <- defect         └─ Argument [a]

import (
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// planFor parses and translates q, failing the test on either error.
func planFor(t *testing.T, q string) ir.LogicalPlan {
	t.Helper()
	ast, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	plan, err := ir.FromAST(ast)
	if err != nil {
		t.Fatalf("translate %q: %v", q, err)
	}
	return plan
}

// findSemiApply returns the first SemiApply in a pre-order walk of plan.
func findSemiApply(plan ir.LogicalPlan) *ir.SemiApply {
	var found *ir.SemiApply
	var walk func(p ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil || found != nil {
			return
		}
		if sa, ok := p.(*ir.SemiApply); ok {
			found = sa
			return
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return found
}

// subtreeOperators returns the operator names in a pre-order walk of plan.
func subtreeOperators(plan ir.LogicalPlan) []string {
	var out []string
	var walk func(p ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		out = append(out, ir.OperatorName(p))
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

// argumentVarsIn returns the union of every Argument's declared variables in
// plan's subtree.
func argumentVarsIn(plan ir.LogicalPlan) []string {
	var out []string
	var walk func(p ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		if arg, ok := p.(*ir.Argument); ok {
			out = append(out, arg.Variables...)
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

// TestExistsAfterWriteCorrelatesOuterVariable pins the planner decision behind
// #2659, for both an entity-creating write clause (CreateNode, which was
// broken) and a property write (SetProperty, which was already correct and must
// stay correct).
//
// The read-only form of the identical statement is asserted first, as a control:
// it establishes what the correct shape IS, from the same translator, rather
// than hard-coding a shape this test merely believes to be right.
func TestExistsAfterWriteCorrelatesOuterVariable(t *testing.T) {
	const predicate = `WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`

	// Control: the read-only path was never defective. Its inner subtree is the
	// reference shape every writing variant below must also produce.
	control := findSemiApply(planFor(t, `MATCH (a:P) `+predicate))
	if control == nil {
		t.Fatal("read-only control: expected a SemiApply for a top-level EXISTS in WHERE")
	}
	controlOps := subtreeOperators(control.Inner)
	if !slices.Contains(controlOps, "CorrelatedApply") {
		t.Fatalf("read-only control: expected a CorrelatedApply in the EXISTS subtree, got %v", controlOps)
	}
	if slices.Contains(controlOps, "AllNodesScan") {
		t.Fatalf("read-only control: EXISTS subtree already contains an AllNodesScan, so this test's premise is wrong: %v", controlOps)
	}
	if !slices.Contains(argumentVarsIn(control.Inner), "a") {
		t.Fatalf("read-only control: no Argument declares the outer variable `a`: %v", argumentVarsIn(control.Inner))
	}

	writeClauses := []struct {
		name   string
		clause string
		why    string
	}{
		{
			name:   "CreateNode",
			clause: `CREATE (:Q)`,
			why:    "the #2659 defect: CreateNode.Vars() names only the created node, so `a` never reached the Argument",
		},
		{
			name:   "CreateRelationship",
			clause: `CREATE (a)-[:Z9]->(a)`,
			why:    "CreateRelationship.Vars() names only the relationship: the same omission",
		},
		{
			name:   "SetProperty",
			clause: `SET a.touched = true`,
			why:    "control: SetProperty.Vars() already names `a`, so this path was correct before the fix and must stay correct",
		},
	}

	for _, wc := range writeClauses {
		t.Run(wc.name, func(t *testing.T) {
			plan := planFor(t, `MATCH (a:P) `+wc.clause+` `+predicate)
			sa := findSemiApply(plan)
			if sa == nil {
				t.Fatalf("expected a SemiApply for a top-level EXISTS in WHERE\n%s", ir.Explain(plan))
			}
			ops := subtreeOperators(sa.Inner)

			// The outer variable must be injected, not re-scanned. This is the
			// assertion that fails on the defective build.
			if slices.Contains(ops, "AllNodesScan") {
				t.Errorf("EXISTS subtree re-scans every node instead of correlating on the outer row (%s)\noperators: %v\n%s",
					wc.why, ops, ir.Explain(plan))
			}
			// The join must be dependent, not a Cartesian product.
			if !slices.Contains(ops, "CorrelatedApply") {
				t.Errorf("EXISTS subtree is not joined by a CorrelatedApply (%s)\noperators: %v\n%s",
					wc.why, ops, ir.Explain(plan))
			}
			// The correlation itself: some Argument must carry `a`.
			if argVars := argumentVarsIn(sa.Inner); !slices.Contains(argVars, "a") {
				t.Errorf("no Argument in the EXISTS subtree declares the outer variable `a` (%s)\nArgument vars: %v\n%s",
					wc.why, argVars, ir.Explain(plan))
			}
			// And the shape must match the read-only control's, so this test
			// tracks the reference path rather than a frozen literal.
			if got, want := opSet(ops), opSet(controlOps); !slices.Equal(got, want) {
				t.Errorf("EXISTS subtree shape diverges from the read-only control (%s)\n got: %v\nwant: %v\n%s",
					wc.why, got, want, ir.Explain(plan))
			}
		})
	}
}

// TestExistsCorrelationSeedSurvivesWriteChain pins the specific contract the fix
// rests on: the correlation seed is the whole outer SCOPE, not just the outer
// plan root's own Vars(). A chain of write clauses puts several operators
// between the scan that binds `a` and the SemiApply, so a non-recursive read of
// Vars() cannot reach it.
//
// The clause ORDER here is load-bearing and must not be rearranged. The chain
// has to END in a clause whose Vars() does not name `a` — a CREATE of an
// anonymous entity. An earlier draft of this test ended the chain with
// `SET a.k = 1`, and SetProperty.Vars() is exactly []string{"a"}: the test
// passed on the defective build, i.e. it was vacuous. Verified by reverting
// the fix and observing this test fail.
func TestExistsCorrelationSeedSurvivesWriteChain(t *testing.T) {
	plan := planFor(t,
		`MATCH (a:P) SET a.k = 1 CREATE (:Q) CREATE (:R) `+
			`WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`)
	sa := findSemiApply(plan)
	if sa == nil {
		t.Fatalf("expected a SemiApply\n%s", ir.Explain(plan))
	}
	if argVars := argumentVarsIn(sa.Inner); !slices.Contains(argVars, "a") {
		t.Errorf("`a` was not carried through a chain of write clauses into the EXISTS correlation set\nArgument vars: %v\n%s",
			argVars, ir.Explain(plan))
	}
	if ops := subtreeOperators(sa.Inner); slices.Contains(ops, "AllNodesScan") {
		t.Errorf("EXISTS subtree re-scans every node after a chain of write clauses\noperators: %v\n%s", ops, ir.Explain(plan))
	}
}

// opSet returns the deduplicated, sorted operator names of ops, so two subtrees
// can be compared on the operators they use without depending on multiplicity
// or traversal order.
func opSet(ops []string) []string {
	out := slices.Clone(ops)
	slices.Sort(out)
	return slices.Compact(out)
}

// TestExistsExplainRendersCorrelatedWritePath is a readability guard: it keeps a
// rendered form of the fixed plan in the test corpus, so the shape #2659 turned
// on is visible to a reader without running the translator.
func TestExistsExplainRendersCorrelatedWritePath(t *testing.T) {
	plan := planFor(t, `MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`)
	got := ir.Explain(plan)
	for _, want := range []string{
		"CreateNode",
		"CorrelatedApply",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Explain output is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AllNodesScan") {
		t.Errorf("Explain output still contains an AllNodesScan (#2659):\n%s", got)
	}
}

// TestExistsCorrelationSetExcludesPriorSubqueryVars pins the outer-only rule
// added to liveOutputVars for SemiApply / AntiSemiApply / RollUpApply.
//
// Those operators return {Outer, Inner} from Children() while documenting that
// only the OUTER side's variables are visible downstream (see SemiApply.Vars in
// cypher/ir/plan.go). Any scope walker that descends Children() blindly — as
// collectAllVars does — harvests a prior subquery's private bindings and offers
// them to the next subquery as correlation variables. openCypher forbids that:
// CIP2015-05-13-EXISTS states that variables introduced in an existential
// subquery "are not available outside the subquery context".
//
// The assertion is two-sided. Both halves matter: without the first, a walker
// that returned nothing at all would pass; without the second, the leak returns.
func TestExistsCorrelationSetExcludesPriorSubqueryVars(t *testing.T) {
	// `r` and `b` are bound only inside the first EXISTS. `a` is the genuine
	// outer variable and must survive.
	plan := planFor(t,
		`MATCH (a:P) WHERE EXISTS { MATCH (a)-[r:Z]->(b:P) } `+
			`WITH a WHERE EXISTS { MATCH (b)-[:Z]->(:P) } RETURN a.sid AS sid`)

	// The OUTER (second) SemiApply is the one whose correlation set is at issue.
	// findSemiApply is pre-order, so it returns the outermost.
	outer := findSemiApply(plan)
	if outer == nil {
		t.Fatalf("expected a SemiApply\n%s", ir.Explain(plan))
	}
	// Guard the premise: the first subquery really does bind r and b somewhere
	// in the outer side, so their absence below is a scope decision and not
	// simply a query that never mentioned them.
	if outerSideVars := argumentVarsIn(outer.Outer); len(outerSideVars) == 0 {
		t.Fatalf("premise check: the outer side has no Arguments at all\n%s", ir.Explain(plan))
	}
	if all := subtreeOperators(outer.Outer); !slices.Contains(all, "SemiApply") {
		t.Fatalf("premise check: expected a nested SemiApply on the outer side, got %v\n%s", all, ir.Explain(plan))
	}

	corr := argumentVarsIn(outer.Inner)
	// Half 1: the real outer variable is present. A walker that under-reports
	// (the outer.Vars() shortcut) fails here.
	if !slices.Contains(corr, "a") {
		t.Errorf("the genuine outer variable `a` is missing from the correlation set: %v\n%s", corr, ir.Explain(plan))
	}
	// Half 2: the prior subquery's private bindings are absent. A walker that
	// over-reports (the collectAllVars shortcut) fails here.
	for _, leaked := range []string{"b", "r"} {
		if slices.Contains(corr, leaked) {
			t.Errorf("prior subquery's private binding %q leaked into the correlation set %v — "+
				"SemiApply.Vars() documents that only outer variables are visible downstream, and "+
				"CIP2015-05-13-EXISTS makes subquery variables unavailable outside it\n%s",
				leaked, corr, ir.Explain(plan))
		}
	}
	// And the consequence: `(b)` must be planned as a fresh scan, because the
	// subquery is legally uncorrelated on it.
	if ops := subtreeOperators(outer.Inner); !slices.Contains(ops, "AllNodesScan") {
		t.Errorf("`b` is a fresh variable, so the subquery is uncorrelated on it and must scan; got %v\n%s",
			ops, ir.Explain(plan))
	}
}
