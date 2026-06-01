package ir_test

// optional_match_test.go — golden IR tests for OPTIONAL MATCH translation (task-221).
//
// OPTIONAL MATCH behaves identically to MATCH except that relationship hops
// emit OptionalExpand instead of Expand, preserving null-extended rows when no
// match is found.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. OPTIONAL MATCH (n) — node-only (no relationship)
//    → AllNodesScan("n")
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_NodeOnly_AllNodesScan(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)
	// `OPTIONAL MATCH (n)` at the start of the query now wraps the
	// scan in an OptionalApply over a SingleRow seed so the empty-graph
	// case emits one NULL-extended row (openCypher 9 §3.2.4).
	apply, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	scan, ok := apply.Inner.(*ir.AllNodesScan)
	if !ok {
		t.Fatalf("expected inner *ir.AllNodesScan, got %T", apply.Inner)
	}
	if scan.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", scan.NodeVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. OPTIONAL MATCH (n:Person) — label scan
//    → NodeByLabelScan("n","Person")
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_LabelScan(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)
	// Node-only OPTIONAL MATCH is wrapped in OptionalApply (see Test_
	// OptionalMatch_NodeOnly_AllNodesScan for rationale).
	apply, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	// Selection wraps the scan for the label predicate.
	inner := apply.Inner
	for {
		sel, isSel := inner.(*ir.Selection)
		if !isSel {
			break
		}
		inner = sel.Child
	}
	scan, ok := inner.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("expected inner *ir.NodeByLabelScan, got %T", inner)
	}
	if scan.Label != "Person" {
		t.Errorf("Label = %q, want Person", scan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. OPTIONAL MATCH (a)-[:R]->(b) — single hop produces OptionalExpand
//    → OptionalExpand(from="a", relTypes=["R"], dir=Outgoing, to="b",
//                     child=AllNodesScan("a"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_SingleHop_OptionalExpand(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Types:     []string{"R"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("b")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	// OPTIONAL MATCH at the start of a query is now wrapped in an
	// OptionalApply over a singleton Argument seed so an empty match
	// emits one NULL-extended row (openCypher 9 §3.2.4). The inner
	// pattern uses regular Expand because the OptionalApply provides
	// the full-pattern NULL emission semantics.
	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	e, ok := opt.Inner.(*ir.Expand)
	if !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Expand, got %T", opt.Inner)
	}
	if e.FromVar != "a" {
		t.Errorf("FromVar = %q, want a", e.FromVar)
	}
	if e.ToVar != "b" {
		t.Errorf("ToVar = %q, want b", e.ToVar)
	}
	if e.Direction != ir.DirectionOutgoing {
		t.Errorf("Direction = %v, want Outgoing", e.Direction)
	}
	if len(e.RelTypes) != 1 || e.RelTypes[0] != "R" {
		t.Errorf("RelTypes = %v, want [R]", e.RelTypes)
	}
	if _, ok := e.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("Expand.Child expected *ir.AllNodesScan, got %T", e.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. OPTIONAL MATCH (a)-[:R]->(b) WHERE b.active = true
//    WHERE predicate becomes Selection above the OptionalExpand.
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_WithWhere(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "b"}, Key: "active"},
		Operator: "=",
		Right:    &ast.BoolLiteral{Value: true},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Types:     []string{"R"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("b")},
						},
					}},
				}},
				Where: &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	// OPTIONAL MATCH at the start of a query is wrapped in an
	// OptionalApply. The WHERE Selection sits inside, between the
	// OptionalApply and the Expand body.
	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	sel, ok := opt.Inner.(*ir.Selection)
	if !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Selection (WHERE), got %T", opt.Inner)
	}
	if sel.Predicate == "" {
		t.Error("WHERE Selection.Predicate must be non-empty")
	}
	if _, ok := sel.Child.(*ir.Expand); !ok {
		t.Fatalf("sel.Child expected *ir.Expand, got %T", sel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. MATCH (n) OPTIONAL MATCH (n)-[:KNOWS]->(m)
//    Second clause chains off the existing plan.
//    Root → OptionalExpand with child = Apply(AllNodesScan(n), AllNodesScan(n))
//    Actually: the child plan from MATCH(n) is passed in; OptionalExpand
//    uses it as its driving child.
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_ChainsAfterMatch(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Types:     []string{"KNOWS"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("m")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	// Since task-392, OPTIONAL MATCH chained after a non-empty driving plan
	// produces an OptionalApply node: the outer side is the driving plan
	// (here AllNodesScan(n)); the inner side starts with an Argument leaf
	// that re-emits the outer row, followed by a regular Expand for the
	// (n)-[:KNOWS]->(m) hop. The OptionalApply itself handles the
	// full-pattern NULL emission semantics, so OptionalExpand is NOT used
	// inside the inner subtree.
	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	// Outer: AllNodesScan("n") from the first MATCH.
	if _, ok := opt.Outer.(*ir.AllNodesScan); !ok {
		t.Fatalf("OptionalApply.Outer expected *ir.AllNodesScan, got %T", opt.Outer)
	}
	// Inner: Expand wrapping an Argument leaf.
	exp, ok := opt.Inner.(*ir.Expand)
	if !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Expand, got %T", opt.Inner)
	}
	if _, ok := exp.Child.(*ir.Argument); !ok {
		t.Fatalf("Expand.Child expected *ir.Argument, got %T", exp.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. OPTIONAL MATCH (a)-[:R1]->(b)-[:R2]->(c) — two hops, both OptionalExpand
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_TwoHops(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Types: []string{"R1"}, Direction: ast.RelDirectionOutgoing},
							Node:         &ast.NodePattern{Variable: strPtr("b")},
							Next: &ast.PathElement{
								Relationship: &ast.RelationshipPattern{Types: []string{"R2"}, Direction: ast.RelDirectionOutgoing},
								Node:         &ast.NodePattern{Variable: strPtr("c")},
							},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	// Root: OptionalApply wrapper.
	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("root expected *ir.OptionalApply, got %T", plan)
	}
	// Inner: second hop Expand (R2→c) on top of first hop Expand (R1→b).
	e2, ok := opt.Inner.(*ir.Expand)
	if !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Expand (hop 2), got %T", opt.Inner)
	}
	if e2.ToVar != "c" {
		t.Errorf("hop2.ToVar = %q, want c", e2.ToVar)
	}
	e1, ok := e2.Child.(*ir.Expand)
	if !ok {
		t.Fatalf("hop2.Child expected *ir.Expand (hop 1), got %T", e2.Child)
	}
	if e1.ToVar != "b" {
		t.Errorf("hop1.ToVar = %q, want b", e1.ToVar)
	}
	if _, ok := e1.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("hop1.Child expected *ir.AllNodesScan, got %T", e1.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. OPTIONAL MATCH Vars() — RelVar and ToVar are both exposed
// ─────────────────────────────────────────────────────────────────────────────

func Test_OptionalMatch_Vars(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Variable:  strPtr("r"),
								Types:     []string{"R"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("b")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	e, ok := opt.Inner.(*ir.Expand)
	if !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Expand, got %T", opt.Inner)
	}
	vars := e.Vars()
	if !containsAll(vars, "r", "b") {
		t.Errorf("Vars() = %v, want to contain r and b", vars)
	}
}

// Test_Match_RelInlineProperty verifies that inline relationship property
// predicates (e.g. -[r:KNOWS {name:'monkey'}]->) become a Selection wrapping
// the Expand. Pre-fix the IR translator silently dropped
// RelationshipPattern.Properties, so the plan accepted every KNOWS edge
// regardless of the inline property — breaking Match2 [5] and similar
// scenarios where the inline property is the only discriminator.
func Test_Match_RelInlineProperty(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			// MATCH (a)-[r:KNOWS {k: 1}]->(b)
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Variable:  strPtr("r"),
								Types:     []string{"KNOWS"},
								Direction: ast.RelDirectionOutgoing,
								Properties: &ast.MapLiteral{
									Keys:   []string{"k"},
									Values: []ast.Expression{&ast.IntLiteral{Value: 1}},
								},
							},
							Node: &ast.NodePattern{Variable: strPtr("b")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	// Topmost operator must be a Selection enforcing r.k = 1 over the Expand.
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection on top, got %T", plan)
	}
	if _, ok := sel.Child.(*ir.Expand); !ok {
		t.Fatalf("Selection.Child expected *ir.Expand, got %T", sel.Child)
	}
}

// Test_OptionalMatch_ExpandIntoBothBound covers the openCypher pattern
//
//	MATCH (a)-[:T]->(b)-->(c)
//	OPTIONAL MATCH (a)-[r:T]->(c)
//
// where both endpoints of the optional path are already bound by a chain of
// Expands in the outer plan. The fix for the long-standing T986 bug was to
// detect that a is reachable via the cumulative variables introduced by the
// outer subtree — Expand.Vars() reports only its own (RelVar, ToVar) pair, so
// a non-recursive check missed the leading NodeByLabelScan's binding and
// routed the optional pattern through a plain Apply with a fresh AllNodesScan
// for a (and a separate Argument leaf), producing wrong row data.
//
// After the fix the inner subtree must use the Argument leaf as the Expand's
// source (a is the leadVar, treated as shared with the outer) and append a
// destination-rebinding equality Selection on top.
func Test_OptionalMatch_ExpandIntoBothBound(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			// MATCH (a:A)-[:KNOWS]->(b)-->(c)
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a"), Labels: []string{"A"}},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Types: []string{"KNOWS"}, Direction: ast.RelDirectionOutgoing},
							Node:         &ast.NodePattern{Variable: strPtr("b")},
							Next: &ast.PathElement{
								Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing},
								Node:         &ast.NodePattern{Variable: strPtr("c")},
							},
						},
					}},
				}},
			},
			// OPTIONAL MATCH (a)-[r:KNOWS]->(c)
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Variable:  strPtr("r"),
								Types:     []string{"KNOWS"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("c")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply at root, got %T", plan)
	}
	// Inner must be Selection(c == synthetic) → Expand(a→r→synthetic) → Argument.
	sel, ok := opt.Inner.(*ir.Selection)
	if !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Selection (destRebinding equality), got %T", opt.Inner)
	}
	exp, ok := sel.Child.(*ir.Expand)
	if !ok {
		t.Fatalf("Selection.Child expected *ir.Expand, got %T", sel.Child)
	}
	if exp.FromVar != "a" {
		t.Errorf("Expand.FromVar = %q, want a", exp.FromVar)
	}
	if exp.RelVar != "r" {
		t.Errorf("Expand.RelVar = %q, want r", exp.RelVar)
	}
	if _, ok := exp.Child.(*ir.Argument); !ok {
		t.Fatalf("Expand.Child expected *ir.Argument (outer-row seam), got %T — pre-fix this was an AllNodesScan wrapped in plain Apply", exp.Child)
	}
}
