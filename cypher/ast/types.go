// Package ast defines the Abstract Syntax Tree (AST) for openCypher 9.
//
// Every production reachable in the read + write + DDL + procedure scope is
// represented here (FOREACH, CALL{}, and multi-graph syntax are excluded).
// The AST is the IR-input contract consumed by later compiler stages (semantic
// analysis, planning, execution).
//
// All types embed a [Position] struct for source-location tracking; the fields
// are populated by the parser in a later task.
//
// Concurrency: AST nodes are value types produced once by the parser and then
// treated as immutable. Concurrent reads are safe without external locking.
package ast

import "fmt"

// Position carries the source location of an AST node.
// Fields are populated by the parser (task 208); zero values are acceptable
// during construction.
type Position struct {
	Line   uint32
	Column uint32
	Offset uint32
}

// String returns "line:col" for diagnostic output.
func (p Position) String() string {
	return fmt.Sprintf("%d:%d", p.Line, p.Column)
}

// ----------------------------------------------------------------------------
// Core interfaces
// ----------------------------------------------------------------------------

// Node is the root interface implemented by every AST node.
// String returns a canonical Cypher representation.
type Node interface {
	// astNode is an unexported marker that prevents accidental external
	// implementation of this interface.
	astNode()
	String() string
}

// Expression is implemented by every AST node that can appear in an
// expression context (right-hand sides, WHERE predicates, RETURN items, etc.).
type Expression interface {
	Node
	exprNode()
}

// Clause is implemented by every top-level clause node.
type Clause interface {
	Node
	clauseNode()
}

// ReadingClause is implemented by clauses that read from the graph without
// modifying it: MATCH, OPTIONAL MATCH, UNWIND, CALL (read-only).
type ReadingClause interface {
	Clause
	readingClause()
}

// UpdatingClause is implemented by clauses that mutate the graph:
// CREATE, MERGE, SET, REMOVE, DELETE, DETACH DELETE, CALL (write).
type UpdatingClause interface {
	Clause
	updatingClause()
}

// ProjectionItem represents a single item in a RETURN or WITH projection,
// optionally aliased.
type ProjectionItem struct {
	Pos    Position
	EndPos Position
	Expr   Expression
	Alias  *string // nil when no AS alias is present
}

// String returns the Cypher representation of a projection item.
func (p *ProjectionItem) String() string {
	if p.Alias != nil {
		return p.Expr.String() + " AS " + *p.Alias
	}
	return p.Expr.String()
}

// SortItem represents a single ORDER BY term.
type SortItem struct {
	Pos        Position
	EndPos     Position
	Expr       Expression
	Descending bool
}

// String returns the Cypher representation of a sort item.
func (s *SortItem) String() string {
	if s.Descending {
		return s.Expr.String() + " DESC"
	}
	return s.Expr.String() + " ASC"
}

// Projection carries the column list shared by RETURN and WITH.
type Projection struct {
	Pos      Position
	EndPos   Position
	Distinct bool
	All      bool // SELECT *
	Items    []*ProjectionItem
	OrderBy  []*SortItem
	Skip     Expression // nil if absent
	Limit    Expression // nil if absent
}

// String returns the Cypher representation of the projection body (without the
// leading RETURN / WITH keyword).
func (p *Projection) String() string {
	if p.All {
		return "*"
	}
	out := ""
	if p.Distinct {
		out += "DISTINCT "
	}
	for i, item := range p.Items {
		if i > 0 {
			out += ", "
		}
		out += item.String()
	}
	if len(p.OrderBy) > 0 {
		out += " ORDER BY "
		for i, s := range p.OrderBy {
			if i > 0 {
				out += ", "
			}
			out += s.String()
		}
	}
	if p.Skip != nil {
		out += " SKIP " + p.Skip.String()
	}
	if p.Limit != nil {
		out += " LIMIT " + p.Limit.String()
	}
	return out
}

// SetItem represents one assignment in a SET clause: variable.property = expr
// or label assignment patterns.
type SetItem struct {
	Pos      Position
	EndPos   Position
	Target   Expression // Property or Variable
	Value    Expression // right-hand side; nil for label-set forms
	Operator string     // "=", "+=", or "" for label operations
	Labels   []string   // populated for SET n:Label1:Label2 form
}

// String returns the Cypher representation of a SET assignment.
func (s *SetItem) String() string {
	if len(s.Labels) > 0 {
		out := s.Target.String()
		for _, l := range s.Labels {
			out += ":" + l
		}
		return out
	}
	return s.Target.String() + " " + s.Operator + " " + s.Value.String()
}

// RemoveItem represents one item in a REMOVE clause.
type RemoveItem struct {
	Pos    Position
	EndPos Position
	Target Expression // Property or Variable
	Labels []string   // populated for REMOVE n:Label form
}

// String returns the Cypher representation of a REMOVE item.
func (r *RemoveItem) String() string {
	if len(r.Labels) > 0 {
		out := r.Target.String()
		for _, l := range r.Labels {
			out += ":" + l
		}
		return out
	}
	return r.Target.String()
}
