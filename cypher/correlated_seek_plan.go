package cypher

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// Resolving a seek key from the bound expression instead of from source text
// (task #2182).
//
// # The defect
//
// A key that is written as a bound variable rather than as a literal never
// reached an index. `WITH 'name-1' AS nm MATCH (a:P {name: nm})` planned as
//
//	Selection(a.name = nm)
//	└─ CartesianProduct
//	   ├─ Projection [nm := 'name-1']
//	   └─ NodeByLabelScan [a:P]
//
// because the seek rewrite requires the Selection's child to be a bare scan
// leaf, and here it is an Apply. The equality was left as a residual filter over
// a full label scan. The spike for #2181 measured the consequence: at a label
// population of 20 000 the bound-key form cost 13.37 ms against 4.40 µs for the
// same query with the key written inline — a factor of 3 038.
//
// # Why the fix is a rewrite and not a wider parser
//
// The seek value was carried as a *string* and re-parsed as source text
// (resolveSeekValue), so a value that is only known from a binding could not be
// expressed at all. Teaching that parser to understand variables would widen the
// mechanism that caused the defect. Instead this pass resolves the binding over
// the AST and gives the existing seek machinery the shape it already recognises:
// it pushes a copy of the key equality into the Apply's inner arm with the bound
// variable replaced by the expression the binding holds.
//
//	Selection(a.name = nm)                    <- retained, unchanged
//	└─ CartesianProduct
//	   ├─ Projection [nm := 'name-1']
//	   └─ Selection(a.name = 'name-1')        <- added; seekable
//	      └─ NodeByLabelScan [a:P]
//
// The inner Selection sits directly over a scan leaf, so
// tryBuildIndexSeekFromSelection claims it with no change to the physical build,
// and EXPLAIN renders NodeByIndexSeek for the same reason. No new operator, no
// new cost policy, and no second copy of the index-applicability rules.
//
// # Why it is result-identical
//
// Three properties, each of which has to hold independently:
//
//  1. The pushed predicate is *implied* by the retained one. The outer Selection
//     is never removed, so the rewrite can only narrow what that filter examines.
//     A row survives the outer filter only if `a.name = nm`; the inner filter
//     tests `a.name = <what nm is bound to>`. Whenever the binding's value equals
//     nm's value on every row, the inner test admits every row the outer would.
//
//  2. The binding is constant across rows. Property 1 needs the substituted
//     expression to evaluate identically on every row the Apply's outer arm
//     produces, which is exactly why only a literal or a parameter reference is
//     substituted (foldableKeyExpr). Both are row-invariant by construction. An
//     item bound to anything that reads a variable, calls a function, or
//     aggregates is declined — its value can differ per row, and pushing it would
//     drop rows.
//
//  3. NULL and type-incompatible keys need no special case, and get none. A NULL
//     key makes the pushed equality evaluate to NULL, which fails the filter and
//     yields no rows; the outer filter reaches the same verdict for the same
//     reason, so the answers agree. A key whose type the index cannot serve makes
//     tryNewHashSeek decline, leaving the pushed Selection as an ordinary filter
//     over the scan — the pre-rewrite plan plus one redundant, correct test.
//
//     A parameter that was never supplied is a third case and is NOT a NULL key:
//     the engine raises ParameterMissing before execution reaches the seek, as
//     openCypher requires. Only EXPLAIN resolves it to NULL, because it is a
//     diagnostic that must render a plan rather than fail. So this rewrite cannot
//     turn a missing parameter into a silent empty result — see
//     TestBoundKeySeek_MissingParameterKeyStillErrors.
//
// # Parameters are substituted as AST, never as values
//
// The plan is cached by query text (parseAndAnalyse), so a rewrite that folded a
// parameter's *value* into the plan would serve the first invocation's value to
// every later one. This pass moves the *ast.Parameter node, leaving resolution to
// the physical build where params are in scope. That is what makes it safe to run
// once and cache.
//
// # Scope
//
// A single bound key against a single Apply. A key set — `UNWIND [...] AS nm` —
// needs a probe per distinct key OR-ed into one posting bitmap, which is a new
// access path and a cost gate of its own; that is task #2183, and this pass
// deliberately declines the Unwind shape rather than half-serving it.

// foldBoundSeekKeys pushes a constant-bound key equality into the inner arm of
// every Apply that carries one, so the existing index-seek rewrite can claim it.
// It mutates plan in place and returns the number of sites rewritten.
//
// Running it more than once on the same plan is harmless: a rewritten site's
// inner arm is a Selection rather than a scan leaf, so it no longer matches.
func foldBoundSeekKeys(plan ir.LogicalPlan) int {
	n := 0
	for _, sel := range collectBoundKeySelections(plan) {
		apply, ok := sel.Child.(*ir.Apply)
		if !ok {
			continue
		}
		nodeVar, _, ok := scanLeafNodeVar(apply.Inner)
		if !ok {
			continue
		}
		pushed, ok := pushableKeyEquality(sel.PredicateExpr, nodeVar, apply.Outer)
		if !ok {
			continue
		}
		apply.Inner = ir.NewSelectionExpr(pushed.String(), pushed, apply.Inner)
		n++
	}
	return n
}

// collectBoundKeySelections returns every Selection in plan whose child is an
// Apply, in a read-only pre-order traversal. Collecting first and mutating after
// keeps the traversal independent of the mutation: the sites are *ir.Apply
// pointers reachable from the collected Selections, so rewriting one never
// invalidates another's position in the tree.
func collectBoundKeySelections(plan ir.LogicalPlan) []*ir.Selection {
	var out []*ir.Selection
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		if sel, ok := p.(*ir.Selection); ok && sel.PredicateExpr != nil {
			if _, isApply := sel.Child.(*ir.Apply); isApply {
				out = append(out, sel)
			}
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

// pushableKeyEquality finds, among pred's top-level AND conjuncts, an equality
// between a property of nodeVar and a variable that outer binds to a row-invariant
// expression, and returns that equality with the variable replaced by the bound
// expression.
//
// Only the first such conjunct is used. A second one would need the pushed
// predicates conjoined, which the index seek cannot serve anyway — it takes one
// (property, value) pair — so finding more would not buy a better access path.
func pushableKeyEquality(pred ast.Expression, nodeVar string, outer ir.LogicalPlan) (ast.Expression, bool) {
	for _, conj := range andConjuncts(pred) {
		binOp, ok := conj.(*ast.BinaryOp)
		if !ok || binOp.Operator != "=" {
			continue
		}
		// Both operand orders: n.prop = v and v = n.prop.
		if prop, keyVar, ok := propertyAndVariable(binOp.Left, binOp.Right, nodeVar); ok {
			if bound, ok := foldableBinding(outer, keyVar); ok {
				return &ast.BinaryOp{Operator: "=", Left: prop, Right: bound}, true
			}
		}
		if prop, keyVar, ok := propertyAndVariable(binOp.Right, binOp.Left, nodeVar); ok {
			if bound, ok := foldableBinding(outer, keyVar); ok {
				return &ast.BinaryOp{Operator: "=", Left: prop, Right: bound}, true
			}
		}
	}
	return nil, false
}

// propertyAndVariable matches propSide as a property access on nodeVar and
// keySide as a bare variable that is not nodeVar itself, returning both. The
// nodeVar exclusion rejects a self-comparison such as `a.x = a.y`, whose right
// side is not a row-invariant key but a value read from the scanned node.
func propertyAndVariable(propSide, keySide ast.Expression, nodeVar string) (*ast.Property, string, bool) {
	prop, ok := propSide.(*ast.Property)
	if !ok {
		return nil, "", false
	}
	recv, ok := prop.Receiver.(*ast.Variable)
	if !ok || recv.Name != nodeVar {
		return nil, "", false
	}
	keyVar, ok := keySide.(*ast.Variable)
	if !ok || keyVar.Name == nodeVar {
		return nil, "", false
	}
	return prop, keyVar.Name, true
}

// andConjuncts flattens a left-deep chain of AND into its operands. A predicate
// that is not an AND yields itself, so callers need no special case.
func andConjuncts(e ast.Expression) []ast.Expression {
	binOp, ok := e.(*ast.BinaryOp)
	if !ok || (binOp.Operator != "AND" && binOp.Operator != "and") {
		return []ast.Expression{e}
	}
	return append(andConjuncts(binOp.Left), andConjuncts(binOp.Right)...)
}

// foldableBinding returns the expression outer binds to name, when outer is a
// Projection carrying that name and the expression is row-invariant.
//
// Only the outer arm's own top-level Projection is consulted. Chasing the binding
// through further operators would have to prove that each one preserves the
// column unchanged — a Selection does, an aggregation does not — and the shape
// this task targets does not need it.
func foldableBinding(outer ir.LogicalPlan, name string) (ast.Expression, bool) {
	proj, ok := outer.(*ir.Projection)
	if !ok {
		return nil, false
	}
	for i := range proj.Items {
		if proj.Items[i].Name != name {
			continue
		}
		if e := proj.Items[i].Expr; e != nil && foldableKeyExpr(e) {
			return e, true
		}
		return nil, false
	}
	return nil, false
}

// foldableKeyExpr reports whether e evaluates to the same value on every row.
//
// The list is deliberately exhaustive rather than structural: these are precisely
// the forms astLiteralToValue can convert, so an accepted expression is one the
// seek can actually use, and anything else is declined rather than pushed and then
// found unusable. A null literal is absent from the list — not because pushing it
// would be wrong (see property 3 in this file's header) but because it can never
// produce a seek, so pushing it would add a filter that buys nothing.
func foldableKeyExpr(e ast.Expression) bool {
	switch e.(type) {
	case *ast.StringLiteral, *ast.IntLiteral, *ast.FloatLiteral, *ast.BoolLiteral, *ast.Parameter:
		return true
	}
	return false
}
