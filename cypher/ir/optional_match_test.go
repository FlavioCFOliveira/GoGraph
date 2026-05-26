package ir_test

// optional_match_test.go — golden IR tests for OPTIONAL MATCH translation (task-221).
//
// OPTIONAL MATCH behaves identically to MATCH except that relationship hops
// emit OptionalExpand instead of Expand, preserving null-extended rows when no
// match is found.

import (
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/ir"
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

	oe, ok := plan.(*ir.OptionalExpand)
	if !ok {
		t.Fatalf("expected *ir.OptionalExpand, got %T", plan)
	}
	if oe.FromVar != "a" {
		t.Errorf("FromVar = %q, want a", oe.FromVar)
	}
	if oe.ToVar != "b" {
		t.Errorf("ToVar = %q, want b", oe.ToVar)
	}
	if oe.Direction != ir.DirectionOutgoing {
		t.Errorf("Direction = %v, want Outgoing", oe.Direction)
	}
	if len(oe.RelTypes) != 1 || oe.RelTypes[0] != "R" {
		t.Errorf("RelTypes = %v, want [R]", oe.RelTypes)
	}
	if _, ok := oe.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("Child expected *ir.AllNodesScan, got %T", oe.Child)
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

	// Root: Selection (WHERE).
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection (WHERE), got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("WHERE Selection.Predicate must be non-empty")
	}
	// Child: OptionalExpand.
	if _, ok := sel.Child.(*ir.OptionalExpand); !ok {
		t.Fatalf("sel.Child expected *ir.OptionalExpand, got %T", sel.Child)
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

	// Root: second hop OptionalExpand (R2→c).
	oe2, ok := plan.(*ir.OptionalExpand)
	if !ok {
		t.Fatalf("root expected *ir.OptionalExpand (hop 2), got %T", plan)
	}
	if oe2.ToVar != "c" {
		t.Errorf("hop2.ToVar = %q, want c", oe2.ToVar)
	}
	// Child: first hop OptionalExpand (R1→b).
	oe1, ok := oe2.Child.(*ir.OptionalExpand)
	if !ok {
		t.Fatalf("hop2.Child expected *ir.OptionalExpand (hop 1), got %T", oe2.Child)
	}
	if oe1.ToVar != "b" {
		t.Errorf("hop1.ToVar = %q, want b", oe1.ToVar)
	}
	// Leaf: AllNodesScan for anchor.
	if _, ok := oe1.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("hop1.Child expected *ir.AllNodesScan, got %T", oe1.Child)
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

	oe, ok := plan.(*ir.OptionalExpand)
	if !ok {
		t.Fatalf("expected *ir.OptionalExpand, got %T", plan)
	}
	vars := oe.Vars()
	if !containsAll(vars, "r", "b") {
		t.Errorf("Vars() = %v, want to contain r and b", vars)
	}
}
