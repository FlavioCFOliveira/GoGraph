package cypher

import "github.com/FlavioCFOliveira/GoGraph/cypher/ast"

// Notification is an out-of-band, advisory message attached to a query result.
// Notifications are NOT result rows and never affect the rows a query returns:
// they surface performance or correctness advisories (for example, a query that
// builds a Cartesian product between disconnected patterns) so a caller — an
// embedder of [Engine] or a Bolt driver via the SUCCESS metadata "notifications"
// field — can warn the user without changing query semantics.
//
// The shape mirrors Neo4j's client notifications so a Bolt driver receives the
// same code/title/description it expects (#1483).
type Notification struct {
	// Code is the stable machine-readable notification code, e.g.
	// "Neo.ClientNotification.Statement.CartesianProductWarning".
	Code string
	// Title is the short human-readable summary.
	Title string
	// Description is the full human-readable explanation, including any
	// query-specific detail (such as the offending variable names).
	Description string
	// Severity is the advisory severity, e.g. "INFORMATION".
	Severity string
	// Category classifies the notification, e.g. "PERFORMANCE".
	Category string
}

// Cartesian-product notification constants. The code, title, and description
// mirror Neo4j's Neo.ClientNotification.Statement.CartesianProductWarning so a
// Bolt driver receives a familiar advisory (confirmed with the cypher-expert).
// Emitting a notification is openCypher-conformant: notifications are out of
// band and cannot change the result rows, side effects, or error of any
// openCypher TCK scenario.
const (
	cartesianProductCode     = "Neo.ClientNotification.Statement.CartesianProductWarning"
	cartesianProductTitle    = "This query builds a cartesian product between disconnected patterns."
	cartesianProductSeverity = "INFORMATION"
	cartesianProductCategory = "PERFORMANCE"
)

// cartesianProductDescription renders the standard description, naming one
// variable from a disconnected component so the message points at the pattern
// that triggered it (matching Neo4j's templated "(identifier is: (<varname>))"
// tail). varName may be empty when no named variable is available.
func cartesianProductDescription(varName string) string {
	ident := varName
	if ident == "" {
		ident = "an unnamed pattern element"
	}
	return "If a part of a query contains multiple disconnected patterns, this will build a " +
		"cartesian product between all those parts. This may produce a large amount of data and " +
		"slow down query processing. While occasionally intended, it may often be possible to " +
		"reformulate the query that avoids the use of this cross product, perhaps by adding a " +
		"relationship between the different parts or by using OPTIONAL MATCH (identifier is: (" +
		ident + "))"
}

// analyseCartesianProduct inspects a parsed query AST and returns a Cartesian
// product notification when the reading portion builds a cross product between
// two or more disconnected pattern components — i.e. components that share no
// bound variable AND are not connected by a WHERE join predicate referencing
// variables from more than one component. OPTIONAL MATCH components are excluded
// (an OPTIONAL MATCH of a disconnected pattern is an outer join, not the warned
// cross product). It returns nil when the query has no such disconnected
// structure.
//
// The detection mirrors Neo4j's CartesianProduct notification trigger
// (confirmed with the cypher-expert): it operates on the variable-sharing graph
// of the pattern components across comma-separated paths AND sequential MATCH
// clauses; a WHERE predicate that references variables from two components
// connects them and suppresses the warning.
// analyseCartesianProductQuery dispatches over the top-level query node,
// analysing each branch of a UNION independently and returning the first
// Cartesian-product notification found (one notification suffices to advise the
// caller).
func analyseCartesianProductQuery(q ast.Query) *Notification {
	switch n := q.(type) {
	case *ast.SingleQuery:
		return analyseCartesianProduct(n)
	case *ast.MultiQuery:
		for _, part := range n.Parts {
			if note := analyseCartesianProduct(part); note != nil {
				return note
			}
		}
	}
	return nil
}

func analyseCartesianProduct(q *ast.SingleQuery) *Notification {
	if q == nil {
		return nil
	}

	uf := newUnionFind()
	// firstVarOf records, per component representative, a representative variable
	// name for the human-readable description.
	allVarsInOrder := make([]string, 0, 8)
	seenVar := make(map[string]struct{}, 8)

	// addComponent registers one pattern component (one path pattern): every
	// variable in it joins the same union-find set. Returns the component's
	// anchor variable (the first variable) or "" when the path is fully
	// anonymous; an anonymous path still forms its own component, anchored on a
	// synthetic key so it cannot accidentally merge with another anonymous path.
	addComponent := func(vars []string, anonKey string) {
		anchor := anonKey
		if len(vars) > 0 {
			anchor = vars[0]
		}
		uf.add(anchor)
		for _, v := range vars {
			uf.add(v)
			uf.union(anchor, v)
			if _, ok := seenVar[v]; !ok {
				seenVar[v] = struct{}{}
				allVarsInOrder = append(allVarsInOrder, v)
			}
		}
	}

	anonSeq := 0
	// wherePredicates collects the WHERE predicates of the (non-optional) MATCH
	// clauses in the current segment; each is applied at the segment boundary so
	// a predicate can connect components declared in different MATCH clauses
	// within the same segment.
	var wherePredicates []ast.Expression

	// segmentHasCartesian reports whether the components accumulated so far
	// (within one WITH-bounded segment) form ≥2 disconnected groups after the
	// WHERE join predicates are applied.
	segmentHasCartesian := func() bool {
		for _, pred := range wherePredicates {
			refs := referencedVars(pred, seenVar)
			for i := 1; i < len(refs); i++ {
				uf.union(refs[0], refs[i])
			}
		}
		roots := make(map[string]struct{}, 4)
		for key := range uf.parent {
			roots[uf.find(key)] = struct{}{}
		}
		return len(roots) >= 2
	}

	// resetSegment clears the accumulated component graph at a WITH/UNWIND
	// barrier: Neo4j evaluates the cross-product warning per query part, and a
	// WITH horizon re-projects scope so patterns after it are not in a cross
	// product with patterns before it.
	resetSegment := func() {
		uf = newUnionFind()
		wherePredicates = wherePredicates[:0]
	}

	for _, rc := range q.ReadingClauses {
		switch m := rc.(type) {
		case *ast.Match:
			if m.Pattern == nil {
				continue
			}
			for _, pp := range m.Pattern.Paths {
				vars := astPathPatternVars(pp)
				anonSeq++
				addComponent(vars, anonComponentKey(anonSeq))
			}
			if m.Where != nil && m.Where.Predicate != nil {
				wherePredicates = append(wherePredicates, m.Where.Predicate)
			}
		case *ast.With, *ast.Unwind:
			// Horizon barrier: close the current segment.
			if segmentHasCartesian() {
				return cartesianNotification(allVarsInOrder)
			}
			resetSegment()
		default:
			// OPTIONAL MATCH, CALL, RETURN, UNION embedded clause: not part of
			// the warned cross product; they do not open or close a segment.
		}
	}

	if segmentHasCartesian() {
		return cartesianNotification(allVarsInOrder)
	}
	return nil
}

// cartesianNotification builds the Cartesian-product notification, naming the
// first known variable for the description.
func cartesianNotification(varsInOrder []string) *Notification {
	varName := ""
	if len(varsInOrder) > 0 {
		varName = varsInOrder[0]
	}
	return &Notification{
		Code:        cartesianProductCode,
		Title:       cartesianProductTitle,
		Description: cartesianProductDescription(varName),
		Severity:    cartesianProductSeverity,
		Category:    cartesianProductCategory,
	}
}

// unionFind is a tiny disjoint-set over string keys, used to group pattern
// variables into connected components. It is local to one analysis call and is
// not concurrency-safe.
type unionFind struct {
	parent map[string]string
}

func newUnionFind() *unionFind { return &unionFind{parent: make(map[string]string, 8)} }

// add registers key as its own singleton set if not already present.
func (u *unionFind) add(key string) {
	if _, ok := u.parent[key]; !ok {
		u.parent[key] = key
	}
}

// find returns the canonical representative of key's set, path-compressing.
func (u *unionFind) find(key string) string {
	root := key
	for u.parent[root] != root {
		root = u.parent[root]
	}
	for u.parent[key] != root {
		u.parent[key], key = root, u.parent[key]
	}
	return root
}

// union merges the sets containing a and b. Both must already be add()ed.
func (u *unionFind) union(a, b string) {
	if _, ok := u.parent[a]; !ok {
		return
	}
	if _, ok := u.parent[b]; !ok {
		return
	}
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}

// anonComponentKey builds a synthetic union-find key for a fully anonymous path
// pattern so two anonymous paths form two distinct components rather than
// merging on an empty name.
func anonComponentKey(seq int) string {
	return "\x00anon-component\x00" + string(rune('0'+seq%10)) + itoaSmall(seq)
}

// itoaSmall renders a non-negative int without importing strconv into the hot
// notification path; the value is a small clause counter.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// astPathPatternVars returns every variable named in a path pattern: the path
// variable (if any), every named node, and every named relationship. Anonymous
// elements contribute nothing.
func astPathPatternVars(pp *ast.PathPattern) []string {
	if pp == nil {
		return nil
	}
	var out []string
	if pp.Variable != nil {
		out = append(out, *pp.Variable)
	}
	for el := pp.Head; el != nil; el = el.Next {
		if el.Node != nil && el.Node.Variable != nil {
			out = append(out, *el.Node.Variable)
		}
		if el.Relationship != nil && el.Relationship.Variable != nil {
			out = append(out, *el.Relationship.Variable)
		}
	}
	return out
}

// referencedVars returns the distinct variables from known (the set of pattern
// variables) that appear anywhere in expr. Only pattern variables matter for
// the connectedness check, so the walk filters by membership in known. The walk
// covers every AST expression node so a predicate buried inside a function call,
// CASE, comprehension, or subscript still connects its components.
func referencedVars(e ast.Expression, known map[string]struct{}) []string {
	seen := make(map[string]struct{}, 4)
	var out []string
	var walk func(ast.Expression)
	add := func(name string) {
		if _, ok := known[name]; !ok {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	walk = func(e ast.Expression) {
		switch n := e.(type) {
		case nil:
			return
		case *ast.Variable:
			add(n.Name)
		case *ast.Property:
			walk(n.Receiver)
		case *ast.FunctionInvocation:
			for _, a := range n.Args {
				walk(a)
			}
		case *ast.BinaryOp:
			walk(n.Left)
			walk(n.Right)
		case *ast.UnaryOp:
			walk(n.Operand)
		case *ast.LabelPredicate:
			walk(n.Receiver)
		case *ast.CaseExpression:
			walk(n.Subject)
			for _, alt := range n.Alternatives {
				if alt != nil {
					walk(alt.Condition)
					walk(alt.Consequent)
				}
			}
			walk(n.ElseExpr)
		case *ast.ListComprehension:
			walk(n.Source)
			walk(n.Predicate)
			walk(n.Projection)
		case *ast.PatternComprehension:
			walk(n.Predicate)
			walk(n.Projection)
		case *ast.MapProjection:
			walk(n.Subject)
			for _, it := range n.Items {
				if it != nil {
					walk(it.Value)
				}
			}
		case *ast.SubscriptExpr:
			walk(n.Expr)
			walk(n.Index)
		case *ast.SliceExpr:
			walk(n.Expr)
			walk(n.From)
			walk(n.To)
		case *ast.ReduceExpr:
			walk(n.Init)
			walk(n.Source)
			walk(n.Projection)
		case *ast.ExistsSubquery, *ast.CountSubquery:
			// A subquery introduces its own pattern scope; it does not connect
			// outer pattern components for the purpose of the cross-product
			// warning. Leaving it unwalked can only yield an extra advisory, not
			// a wrong result.
		default:
			// Unknown/extending node types contribute no connection; a missed
			// connection only yields an extra advisory notification, never a
			// wrong result.
		}
	}
	walk(e)
	return out
}
