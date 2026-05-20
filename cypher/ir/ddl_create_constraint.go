package ir

// ddl_create_constraint.go — IR node for CREATE CONSTRAINT DDL (task-296).

// ConstraintKind distinguishes UNIQUE from NOT_NULL constraints.
type ConstraintKind uint8

const (
	// ConstraintUnique requires that at most one node with a given label has a
	// particular value for the constrained property.
	ConstraintUnique ConstraintKind = iota
	// ConstraintNotNull requires that every node with a given label has a
	// non-null value for the constrained property.
	ConstraintNotNull
)

// CreateConstraint is a DDL plan node representing a Cypher CREATE CONSTRAINT
// statement.
//
// CreateConstraint is a leaf node; it has no child plan.
type CreateConstraint struct {
	// Name is the user-defined constraint name.
	Name string
	// Label is the node label to which the constraint applies.
	Label string
	// Property is the node property key constrained.
	Property string
	// Kind is the constraint type (unique or not-null).
	Kind ConstraintKind
	// IfNotExists suppresses an error when the constraint already exists.
	IfNotExists bool
}

// NewCreateConstraint creates a CreateConstraint IR node.
func NewCreateConstraint(name, label, property string, kind ConstraintKind, ifNotExists bool) *CreateConstraint {
	return &CreateConstraint{
		Name:        name,
		Label:       label,
		Property:    property,
		Kind:        kind,
		IfNotExists: ifNotExists,
	}
}

// Children implements LogicalPlan. CreateConstraint is a leaf.
func (c *CreateConstraint) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. CreateConstraint introduces no variables.
func (c *CreateConstraint) Vars() []string { return nil }
