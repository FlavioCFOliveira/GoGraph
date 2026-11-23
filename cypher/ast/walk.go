package ast

import (
	"iter"
	"reflect"
)

// Walk yields root and its descendant nodes in depth-first preorder. It visits
// UNION branches and list elements in order, including subquery bodies and
// expressions inside patterns. A shared subtree is visited at each occurrence.
// A nil root yields nothing. Breaking out of the
// loop stops the entire traversal.
func Walk(root Node) iter.Seq[Node] {
	return func(yield func(Node) bool) {
		w := walker(yield)
		w.node(root)
	}
}

type walker func(Node) bool

func (w walker) node(node Node) bool {
	// Optional concrete children (for example Match.Where) become typed nil
	// pointers when passed as Node. All implementations of Node are pointers.
	if node == nil || reflect.ValueOf(node).IsNil() {
		return true
	}
	if !w(node) {
		return false
	}
	switch n := node.(type) {
	case *MultiQuery:
		return walkNodes(w, n.Parts)
	case *SingleQuery:
		at := len(n.ReadingClauses)
		if n.LeadingCountSet {
			at = min(max(n.LeadingClauseCount, 0), at)
		}
		return walkNodes(w, n.ReadingClauses[:at]) && walkNodes(w, n.With) &&
			walkNodes(w, n.ReadingClauses[at:]) && walkNodes(w, n.UpdatingClauses) && w.node(n.Return)
	case *Union:
		return w.node(n.Query)
	case *Match:
		return w.node(n.Pattern) && w.node(n.Where)
	case *OptionalMatch:
		return w.node(n.Pattern) && w.node(n.Where)
	case *Where:
		return w.node(n.Predicate)
	case *Unwind:
		return w.node(n.Expr)
	case *Return:
		return w.projection(n.Projection)
	case *With:
		return w.projection(n.Projection) && w.node(n.Where)
	case *Call:
		return walkNodes(w, n.Args) && w.node(n.Where)
	case *Create:
		return w.node(n.Pattern)
	case *Merge:
		return w.node(n.Pattern) && w.setItems(n.OnCreate) && w.setItems(n.OnMatch)
	case *Set:
		return w.setItems(n.Items)
	case *Remove:
		for _, item := range n.Items {
			if item != nil && !w.node(item.Target) {
				return false
			}
		}
	case *Delete:
		return walkNodes(w, n.Expressions)
	case *DetachDelete:
		return walkNodes(w, n.Expressions)
	case *Foreach:
		return w.node(n.Expr) && walkNodes(w, n.Body)
	case *Pattern:
		return walkNodes(w, n.Paths)
	case *PathPattern:
		for step := n.Head; step != nil; step = step.Next {
			if !w.node(step.Relationship) || !w.node(step.Node) {
				return false
			}
		}
	case *NodePattern:
		return w.node(n.Properties)
	case *RelationshipPattern:
		return w.node(n.Properties)
	case Expression:
		return w.expression(n)
	}
	return true
}

func walkNodes[T Node](w walker, nodes []T) bool {
	for _, n := range nodes {
		if !w.node(n) {
			return false
		}
	}
	return true
}

func (w walker) projection(p *Projection) bool {
	if p == nil {
		return true
	}
	for _, item := range p.Items {
		if item != nil && !w.node(item.Expr) {
			return false
		}
	}
	for _, item := range p.OrderBy {
		if item != nil && !w.node(item.Expr) {
			return false
		}
	}
	return w.node(p.Skip) && w.node(p.Limit)
}

func (w walker) setItems(items []*SetItem) bool {
	for _, item := range items {
		if item != nil && (!w.node(item.Target) || !w.node(item.Value)) {
			return false
		}
	}
	return true
}

func (w walker) expression(expr Expression) bool {
	switch n := expr.(type) {
	case *Property:
		return w.node(n.Receiver)
	case *FunctionInvocation:
		return walkNodes(w, n.Args)
	case *BinaryOp:
		return w.node(n.Left) && w.node(n.Right)
	case *UnaryOp:
		return w.node(n.Operand)
	case *LabelPredicate:
		return w.node(n.Receiver)
	case *CaseExpression:
		if !w.node(n.Subject) {
			return false
		}
		for _, alt := range n.Alternatives {
			if alt != nil && (!w.node(alt.Condition) || !w.node(alt.Consequent)) {
				return false
			}
		}
		return w.node(n.ElseExpr)
	case *ListLiteral:
		return walkNodes(w, n.Elements)
	case *MapLiteral:
		return walkNodes(w, n.Values)
	case *ListComprehension:
		return w.node(n.Source) && w.node(n.Predicate) && w.node(n.Projection)
	case *PatternComprehension:
		return w.node(n.Pattern) && w.node(n.Predicate) && w.node(n.Projection)
	case *MapProjection:
		if !w.node(n.Subject) {
			return false
		}
		for _, item := range n.Items {
			if item != nil && !w.node(item.Value) {
				return false
			}
		}
	case *ExistsSubquery:
		return w.node(n.Pattern) && w.node(n.Where) && w.node(n.Query)
	case *CountSubquery:
		return w.node(n.Pattern) && w.node(n.Where) && w.node(n.Query)
	case *SubscriptExpr:
		return w.node(n.Expr) && w.node(n.Index)
	case *SliceExpr:
		return w.node(n.Expr) && w.node(n.From) && w.node(n.To)
	case *ReduceExpr:
		return w.node(n.Init) && w.node(n.Source) && w.node(n.Projection)
	}
	return true
}
