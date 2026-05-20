package ir

// ddl_drop_constraint.go — IR node for DROP CONSTRAINT DDL (task-297).

// DropConstraint is a DDL plan node representing a Cypher DROP CONSTRAINT
// statement.
//
// DropConstraint is a leaf node; it has no child plan.
type DropConstraint struct {
	// Name is the constraint name to drop.
	Name string
	// Label is the node label associated with the constraint.
	// May be empty for DROP CONSTRAINT by name only.
	Label string
	// Property is the constrained property key.
	// May be empty for DROP CONSTRAINT by name only.
	Property string
	// Kind is the constraint type (unique or not-null).
	Kind ConstraintKind
	// IfExists suppresses an error when the constraint is absent.
	IfExists bool
}

// NewDropConstraint creates a DropConstraint IR node.
func NewDropConstraint(name, label, property string, kind ConstraintKind, ifExists bool) *DropConstraint {
	return &DropConstraint{
		Name:     name,
		Label:    label,
		Property: property,
		Kind:     kind,
		IfExists: ifExists,
	}
}

// Children implements LogicalPlan. DropConstraint is a leaf.
func (d *DropConstraint) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. DropConstraint introduces no variables.
func (d *DropConstraint) Vars() []string { return nil }
