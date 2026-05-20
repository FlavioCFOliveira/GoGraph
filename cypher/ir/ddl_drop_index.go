package ir

// ddl_drop_index.go — IR node for DROP INDEX DDL (task-295).

// DropIndex is a DDL plan node representing a Cypher DROP INDEX statement.
//
// DropIndex is a leaf node; it has no child plan.
type DropIndex struct {
	// Name is the index name to drop.
	Name string
	// IfExists suppresses ErrIndexNotFound when the index is absent.
	IfExists bool
}

// NewDropIndex creates a DropIndex IR node.
func NewDropIndex(name string, ifExists bool) *DropIndex {
	return &DropIndex{Name: name, IfExists: ifExists}
}

// Children implements LogicalPlan. DropIndex is a leaf.
func (d *DropIndex) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. DropIndex introduces no variables.
func (d *DropIndex) Vars() []string { return nil }
