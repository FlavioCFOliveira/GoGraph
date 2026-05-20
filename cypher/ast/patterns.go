package ast

import "strings"

// ----------------------------------------------------------------------------
// Relationship direction
// ----------------------------------------------------------------------------

// RelDirection indicates the directionality of a relationship pattern.
type RelDirection int8

const (
	// RelDirectionNone means the relationship has no specified direction: -[r]-
	RelDirectionNone RelDirection = iota
	// RelDirectionOutgoing means left-to-right: -[r]->
	RelDirectionOutgoing
	// RelDirectionIncoming means right-to-left: <-[r]-
	RelDirectionIncoming
)

// String returns the Cypher token pair for the direction (left side, right side).
func (d RelDirection) String() string {
	switch d {
	case RelDirectionOutgoing:
		return "outgoing"
	case RelDirectionIncoming:
		return "incoming"
	default:
		return "none"
	}
}

// ----------------------------------------------------------------------------
// Pattern nodes
// ----------------------------------------------------------------------------

// NodePattern represents a node within a path pattern: (n:Label {prop: val}).
type NodePattern struct {
	Pos        Position
	EndPos     Position
	Variable   *string    // nil when anonymous
	Labels     []string   // zero or more labels
	Properties Expression // nil or a MapLiteral / Parameter
}

func (*NodePattern) astNode() {}

// String returns the Cypher node pattern.
func (n *NodePattern) String() string {
	out := "("
	if n.Variable != nil {
		out += *n.Variable
	}
	for _, l := range n.Labels {
		out += ":" + l
	}
	if n.Properties != nil {
		out += " " + n.Properties.String()
	}
	out += ")"
	return out
}

// RangeQuantifier represents a variable-length range on a relationship: *1..3.
type RangeQuantifier struct {
	Pos    Position
	EndPos Position
	Min    *int64 // nil means no lower bound specified
	Max    *int64 // nil means no upper bound specified
}

// String returns the Cypher range quantifier.
func (r *RangeQuantifier) String() string {
	if r.Min == nil && r.Max == nil {
		return "*"
	}
	out := "*"
	if r.Min != nil {
		out += intStr(*r.Min)
	}
	out += ".."
	if r.Max != nil {
		out += intStr(*r.Max)
	}
	return out
}

// intStr converts an int64 to its decimal string.
func intStr(v int64) string {
	return (&IntLiteral{Value: v}).String()
}

// RelationshipPattern represents a relationship within a path pattern.
//
//	-[r:REL_TYPE {prop: val}]->
type RelationshipPattern struct {
	Pos        Position
	EndPos     Position
	Variable   *string    // nil when anonymous
	Types      []string   // zero or more relationship types (OR semantics)
	Properties Expression // nil or MapLiteral / Parameter
	Direction  RelDirection
	Range      *RangeQuantifier // nil for fixed-length
}

func (*RelationshipPattern) astNode() {}

// String returns the Cypher relationship pattern (including direction arrows).
func (r *RelationshipPattern) String() string {
	inner := "["
	if r.Variable != nil {
		inner += *r.Variable
	}
	for i, t := range r.Types {
		if i == 0 {
			inner += ":"
		} else {
			inner += "|"
		}
		inner += t
	}
	if r.Range != nil {
		inner += r.Range.String()
	}
	if r.Properties != nil {
		inner += " " + r.Properties.String()
	}
	inner += "]"

	switch r.Direction {
	case RelDirectionOutgoing:
		return "-" + inner + "->"
	case RelDirectionIncoming:
		return "<-" + inner + "-"
	default:
		return "-" + inner + "-"
	}
}

// PathElement is one alternating step in a path: node (rel node)*.
// It holds exactly one NodePattern followed by zero or more (rel, node) pairs.
type PathElement struct {
	Node         *NodePattern
	Relationship *RelationshipPattern // nil for the first node
	Next         *PathElement         // nil for the last node
}

// PathPattern represents a single path within a pattern:
// (a)-[r]->(b)-[s]->(c).
type PathPattern struct {
	Pos      Position
	EndPos   Position
	Variable *string      // path variable, nil when absent
	Head     *PathElement // linked list of alternating node/rel steps
}

func (*PathPattern) astNode()  {}
func (*PathPattern) exprNode() {}

// String returns the Cypher path pattern.
func (p *PathPattern) String() string {
	out := ""
	if p.Variable != nil {
		out += *p.Variable + " = "
	}
	el := p.Head
	for el != nil {
		if el.Relationship != nil {
			out += el.Relationship.String()
		}
		if el.Node != nil {
			out += el.Node.String()
		}
		el = el.Next
	}
	return out
}

// Pattern represents the comma-separated list of path patterns in a MATCH
// or CREATE clause.
type Pattern struct {
	Pos    Position
	EndPos Position
	Paths  []*PathPattern
}

func (*Pattern) astNode() {}

// String returns the comma-separated path patterns.
func (p *Pattern) String() string {
	parts := make([]string, len(p.Paths))
	for i, path := range p.Paths {
		parts[i] = path.String()
	}
	return strings.Join(parts, ", ")
}
