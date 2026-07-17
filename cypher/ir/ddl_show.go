package ir

// ddl_show.go — IR nodes for the SHOW CONSTRAINTS / SHOW INDEXES schema
// introspection statements (#1922).
//
// Unlike the CREATE/DROP DDL nodes, SHOW statements are pure reads: they mutate
// no state and emit a result set (named columns and rows). They are handled on
// the same hand-written DDL path as the other schema statements (ir.IsDDL /
// ir.ParseDDL) so the ANTLR grammar is untouched, but the engine executes them
// as reads — see cypher.runShowConstraints / cypher.runShowIndexes.

// ShowConstraints is a DDL plan node representing a SHOW CONSTRAINTS (or the
// singular SHOW CONSTRAINT) statement. It is a leaf node; it has no child plan
// and introduces no variables. It carries no fields: the plain form lists every
// registered constraint.
type ShowConstraints struct{}

// NewShowConstraints creates a ShowConstraints IR node.
func NewShowConstraints() *ShowConstraints { return &ShowConstraints{} }

// Children implements LogicalPlan. ShowConstraints is a leaf.
func (*ShowConstraints) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. ShowConstraints introduces no variables.
func (*ShowConstraints) Vars() []string { return nil }

// ShowIndexes is a DDL plan node representing a SHOW INDEXES (or the singular
// SHOW INDEX) statement. It is a leaf node; it has no child plan and introduces
// no variables. It carries no fields: the plain form lists every registered
// index.
type ShowIndexes struct{}

// NewShowIndexes creates a ShowIndexes IR node.
func NewShowIndexes() *ShowIndexes { return &ShowIndexes{} }

// Children implements LogicalPlan. ShowIndexes is a leaf.
func (*ShowIndexes) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. ShowIndexes introduces no variables.
func (*ShowIndexes) Vars() []string { return nil }
