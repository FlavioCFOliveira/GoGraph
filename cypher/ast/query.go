package ast

import "strings"

// ----------------------------------------------------------------------------
// Query-level nodes
// ----------------------------------------------------------------------------

// Query is the top-level AST node.  A query is either a single query or a
// UNION of single queries.
type Query interface {
	Node
	queryNode()
}

// SingleQuery is a sequence of reading and updating clauses, terminated by an
// optional RETURN.
type SingleQuery struct {
	Pos             Position
	EndPos          Position
	ReadingClauses  []ReadingClause
	UpdatingClauses []UpdatingClause
	Return          *Return // nil when the query has no RETURN
	With            []*With // WITH clauses that appear before RETURN
}

func (*SingleQuery) astNode()   {}
func (*SingleQuery) queryNode() {}

// String returns the Cypher representation of the single query.
func (q *SingleQuery) String() string {
	var parts []string
	for _, r := range q.ReadingClauses {
		parts = append(parts, r.String())
	}
	for _, w := range q.With {
		parts = append(parts, w.String())
	}
	for _, u := range q.UpdatingClauses {
		parts = append(parts, u.String())
	}
	if q.Return != nil {
		parts = append(parts, q.Return.String())
	}
	return strings.Join(parts, " ")
}

// MultiQuery is a UNION of SingleQuery nodes.
type MultiQuery struct {
	Pos    Position
	EndPos Position
	Parts  []*SingleQuery
	All    bool // true for UNION ALL; false for UNION (deduplicating)
}

func (*MultiQuery) astNode()   {}
func (*MultiQuery) queryNode() {}

// String returns the Cypher UNION query.
func (m *MultiQuery) String() string {
	if len(m.Parts) == 0 {
		return ""
	}
	keyword := " UNION "
	if m.All {
		keyword = " UNION ALL "
	}
	parts := make([]string, len(m.Parts))
	for i, p := range m.Parts {
		parts[i] = p.String()
	}
	return strings.Join(parts, keyword)
}

// Union is a standalone UNION clause (used as an intermediate representation
// for some parsing strategies).  MultiQuery is preferred for the final AST.
type Union struct {
	Pos    Position
	EndPos Position
	All    bool
	Query  *SingleQuery
}

func (*Union) astNode()       {}
func (*Union) clauseNode()    {}
func (*Union) readingClause() {}

// String returns the UNION clause.
func (u *Union) String() string {
	if u.All {
		return "UNION ALL " + u.Query.String()
	}
	return "UNION " + u.Query.String()
}

// Return is a RETURN clause.
type Return struct {
	Pos        Position
	EndPos     Position
	Projection *Projection
}

func (*Return) astNode()       {}
func (*Return) clauseNode()    {}
func (*Return) readingClause() {}

// String returns the RETURN clause.
func (r *Return) String() string { return "RETURN " + r.Projection.String() }

// With is a WITH clause, used for intermediate projections and filtering.
type With struct {
	Pos        Position
	EndPos     Position
	Projection *Projection
	Where      *Where // nil when no WHERE predicate
}

func (*With) astNode()       {}
func (*With) clauseNode()    {}
func (*With) readingClause() {}

// String returns the WITH clause.
func (w *With) String() string {
	out := "WITH " + w.Projection.String()
	if w.Where != nil {
		out += " " + w.Where.String()
	}
	return out
}

// Unwind is an UNWIND clause: UNWIND expr AS variable.
type Unwind struct {
	Pos      Position
	EndPos   Position
	Expr     Expression
	Variable string
}

func (*Unwind) astNode()       {}
func (*Unwind) clauseNode()    {}
func (*Unwind) readingClause() {}

// String returns the UNWIND clause.
func (u *Unwind) String() string {
	return "UNWIND " + u.Expr.String() + " AS " + u.Variable
}
