package ast

import "strings"

// ----------------------------------------------------------------------------
// Clause nodes
// ----------------------------------------------------------------------------

// Where represents a WHERE predicate attached to MATCH, WITH, or similar.
// It is modelled as a standalone node rather than a field because it can carry
// its own position and is shared between reading and filtering clauses.
type Where struct {
	Pos       Position
	Predicate Expression
}

func (*Where) astNode()       {}
func (*Where) clauseNode()    {}
func (*Where) readingClause() {}

// String returns the WHERE clause.
func (w *Where) String() string { return "WHERE " + w.Predicate.String() }

// Match is a MATCH clause.
type Match struct {
	Pos     Position
	Pattern *Pattern
	Where   *Where // nil when no WHERE predicate
}

func (*Match) astNode()       {}
func (*Match) clauseNode()    {}
func (*Match) readingClause() {}

// String returns the MATCH clause.
func (m *Match) String() string {
	out := "MATCH " + m.Pattern.String()
	if m.Where != nil {
		out += " " + m.Where.String()
	}
	return out
}

// OptionalMatch is an OPTIONAL MATCH clause.
type OptionalMatch struct {
	Pos     Position
	Pattern *Pattern
	Where   *Where // nil when no WHERE predicate
}

func (*OptionalMatch) astNode()       {}
func (*OptionalMatch) clauseNode()    {}
func (*OptionalMatch) readingClause() {}

// String returns the OPTIONAL MATCH clause.
func (o *OptionalMatch) String() string {
	out := "OPTIONAL MATCH " + o.Pattern.String()
	if o.Where != nil {
		out += " " + o.Where.String()
	}
	return out
}

// Create is a CREATE clause.
type Create struct {
	Pos     Position
	Pattern *Pattern
}

func (*Create) astNode()        {}
func (*Create) clauseNode()     {}
func (*Create) updatingClause() {}

// String returns the CREATE clause.
func (c *Create) String() string { return "CREATE " + c.Pattern.String() }

// Merge is a MERGE clause, with optional ON CREATE and ON MATCH actions.
type Merge struct {
	Pos      Position
	Pattern  *PathPattern
	OnCreate []*SetItem // actions on ON CREATE SET
	OnMatch  []*SetItem // actions on ON MATCH SET
}

func (*Merge) astNode()        {}
func (*Merge) clauseNode()     {}
func (*Merge) updatingClause() {}

// String returns the MERGE clause.
func (m *Merge) String() string {
	out := "MERGE " + m.Pattern.String()
	if len(m.OnCreate) > 0 {
		parts := make([]string, len(m.OnCreate))
		for i, s := range m.OnCreate {
			parts[i] = s.String()
		}
		out += " ON CREATE SET " + strings.Join(parts, ", ")
	}
	if len(m.OnMatch) > 0 {
		parts := make([]string, len(m.OnMatch))
		for i, s := range m.OnMatch {
			parts[i] = s.String()
		}
		out += " ON MATCH SET " + strings.Join(parts, ", ")
	}
	return out
}

// Set is a SET clause.
type Set struct {
	Pos   Position
	Items []*SetItem
}

func (*Set) astNode()        {}
func (*Set) clauseNode()     {}
func (*Set) updatingClause() {}

// String returns the SET clause.
func (s *Set) String() string {
	parts := make([]string, len(s.Items))
	for i, item := range s.Items {
		parts[i] = item.String()
	}
	return "SET " + strings.Join(parts, ", ")
}

// Remove is a REMOVE clause.
type Remove struct {
	Pos   Position
	Items []*RemoveItem
}

func (*Remove) astNode()        {}
func (*Remove) clauseNode()     {}
func (*Remove) updatingClause() {}

// String returns the REMOVE clause.
func (r *Remove) String() string {
	parts := make([]string, len(r.Items))
	for i, item := range r.Items {
		parts[i] = item.String()
	}
	return "REMOVE " + strings.Join(parts, ", ")
}

// Delete is a DELETE clause.
type Delete struct {
	Pos         Position
	Expressions []Expression
}

func (*Delete) astNode()        {}
func (*Delete) clauseNode()     {}
func (*Delete) updatingClause() {}

// String returns the DELETE clause.
func (d *Delete) String() string {
	parts := make([]string, len(d.Expressions))
	for i, e := range d.Expressions {
		parts[i] = e.String()
	}
	return "DELETE " + strings.Join(parts, ", ")
}

// DetachDelete is a DETACH DELETE clause.
type DetachDelete struct {
	Pos         Position
	Expressions []Expression
}

func (*DetachDelete) astNode()        {}
func (*DetachDelete) clauseNode()     {}
func (*DetachDelete) updatingClause() {}

// String returns the DETACH DELETE clause.
func (d *DetachDelete) String() string {
	parts := make([]string, len(d.Expressions))
	for i, e := range d.Expressions {
		parts[i] = e.String()
	}
	return "DETACH DELETE " + strings.Join(parts, ", ")
}

// YieldItem represents a single item in a YIELD clause.
type YieldItem struct {
	Pos   Position
	Name  string
	Alias *string // nil when no AS alias
}

// String returns the YIELD item.
func (y *YieldItem) String() string {
	if y.Alias != nil {
		return y.Name + " AS " + *y.Alias
	}
	return y.Name
}

// Call is a CALL procedure clause.
//
//	CALL namespace.procedure(args) YIELD items WHERE predicate
type Call struct {
	Pos       Position
	Namespace []string
	Procedure string
	Args      []Expression // nil or empty means no argument list
	Yield     []*YieldItem // nil means no YIELD clause; empty slice means YIELD *
	Where     *Where       // nil when no WHERE predicate on YIELD
}

func (*Call) astNode()        {}
func (*Call) clauseNode()     {}
func (*Call) readingClause()  {}
func (*Call) updatingClause() {}

// String returns the CALL clause.
func (c *Call) String() string {
	parts := make([]string, 0, len(c.Namespace)+1)
	parts = append(parts, c.Namespace...)
	parts = append(parts, c.Procedure)
	out := "CALL " + strings.Join(parts, ".")

	if c.Args != nil {
		argParts := make([]string, len(c.Args))
		for i, a := range c.Args {
			argParts[i] = a.String()
		}
		out += "(" + strings.Join(argParts, ", ") + ")"
	}

	if c.Yield != nil {
		if len(c.Yield) == 0 {
			out += " YIELD *"
		} else {
			yieldParts := make([]string, len(c.Yield))
			for i, y := range c.Yield {
				yieldParts[i] = y.String()
			}
			out += " YIELD " + strings.Join(yieldParts, ", ")
		}
	}

	if c.Where != nil {
		out += " " + c.Where.String()
	}
	return out
}
