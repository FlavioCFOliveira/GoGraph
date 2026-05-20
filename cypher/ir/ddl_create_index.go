package ir

// ddl_create_index.go — IR node for CREATE INDEX DDL (task-294).

// IndexType selects the backing store for a Cypher index.
type IndexType uint8

const (
	// IndexTypeHash is a hash-based exact-match index (default).
	IndexTypeHash IndexType = iota
	// IndexTypeBTree is a B-tree range-capable index.
	IndexTypeBTree
)

// CreateIndex is a DDL plan node representing a Cypher CREATE INDEX statement.
//
// CreateIndex is a leaf node; it has no child plan.
type CreateIndex struct {
	// Name is the user-defined index name, or empty for an auto-named index.
	Name string
	// Label is the node label on which the index is built.
	Label string
	// Property is the node property key indexed.
	Property string
	// Type is the backing index kind (hash or btree).
	Type IndexType
	// IfNotExists suppresses ErrIndexExists when the index already exists.
	IfNotExists bool
}

// NewCreateIndex creates a CreateIndex IR node.
func NewCreateIndex(name, label, property string, idxType IndexType, ifNotExists bool) *CreateIndex {
	return &CreateIndex{
		Name:        name,
		Label:       label,
		Property:    property,
		Type:        idxType,
		IfNotExists: ifNotExists,
	}
}

// Children implements LogicalPlan. CreateIndex is a leaf.
func (c *CreateIndex) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. CreateIndex introduces no variables.
func (c *CreateIndex) Vars() []string { return nil }
