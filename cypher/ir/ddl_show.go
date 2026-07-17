package ir

// ddl_show.go — IR nodes for the SHOW CONSTRAINTS / SHOW INDEXES schema
// introspection statements (#1922, YIELD extended by #2044).
//
// Unlike the CREATE/DROP DDL nodes, SHOW statements are pure reads: they mutate
// no state and emit a result set (named columns and rows). They are handled on
// the same hand-written DDL path as the other schema statements (ir.IsDDL /
// ir.ParseDDL) so the ANTLR grammar is untouched, but the engine executes them
// as reads — see cypher.runShowConstraints / cypher.runShowIndexes.
//
// # YIELD / WHERE / RETURN (#2044)
//
// Modern Neo4j clients (Browser :schema, the official drivers' tooling) emit the
// SHOW commands with a trailing YIELD / WHERE / RETURN sub-clause. A non-nil
// Projection on a SHOW plan carries the parsed sub-clauses; a nil Projection is
// the plain form (every column, every row, default order). The Projection is
// parsed by the hand-written DDL parser (parseShow), which reuses the existing
// ANTLR expression grammar for the WHERE predicate and RETURN items WITHOUT
// modifying the grammar (it re-parses the tail as a synthetic CALL … clause).

import "github.com/FlavioCFOliveira/GoGraph/cypher/ast"

// ShowConstraintColumns is the ordered SHOW CONSTRAINTS output schema. It is the
// single source of truth shared by the executor (cypher.runShowConstraints, which
// builds rows in this order) and the parser (parseShow, which validates YIELD
// item names against it).
var ShowConstraintColumns = []string{"name", "type", "entityType", "labelsOrTypes", "properties"}

// ShowIndexColumns is the ordered SHOW INDEXES output schema, shared by the
// executor and the parser for the same reason as [ShowConstraintColumns].
var ShowIndexColumns = []string{"name", "state", "type", "entityType", "labelsOrTypes", "properties"}

// ShowYield is one resolved YIELD projection item: it reads the SHOW column named
// Source and exposes it downstream (to WHERE, RETURN, and the result set) under
// the name Output. Output equals Source unless the YIELD item carried an AS
// alias. For YIELD * and the WHERE-without-YIELD form the parser materialises one
// ShowYield per default column with Source == Output.
type ShowYield struct {
	Source string
	Output string
}

// ShowProjection carries the optional YIELD / WHERE / RETURN sub-clauses parsed
// onto a SHOW CONSTRAINTS / SHOW INDEXES statement (#2044). Semantics follow the
// Neo4j Cypher manual's SHOW-command grammar:
//
//   - Project is the ordered YIELD projection (a WITH-style scope barrier): only
//     the Output names are visible to Where and Return. It always has at least
//     one item for a non-nil ShowProjection — YIELD * and the WHERE-only form are
//     expanded to every default column by the parser.
//   - Where filters the projected rows (evaluated per row; NULL and false both
//     drop the row, three-valued logic).
//   - Return, when non-nil, is a final scalar projection over the yielded scope
//     (it references only Output names). ORDER BY / SKIP / LIMIT / DISTINCT and
//     aggregations are rejected by the parser, so Return carries only Items (or
//     the RETURN * flag) that the executor evaluates per row.
type ShowProjection struct {
	Project []ShowYield
	Where   ast.Expression
	Return  *ast.Projection
	// ReturnColumns are the output column names for an explicit RETURN item list
	// (Return non-nil and not RETURN *), precomputed by the parser using the same
	// column-naming convention as an ordinary RETURN. Empty otherwise.
	ReturnColumns []string
}

// ShowConstraints is a DDL plan node representing a SHOW CONSTRAINTS (or the
// singular SHOW CONSTRAINT) statement. It is a leaf node; it has no child plan
// and introduces no variables. Projection is nil for the plain form and non-nil
// when a YIELD / WHERE / RETURN sub-clause was supplied (#2044).
type ShowConstraints struct {
	Projection *ShowProjection
}

// NewShowConstraints creates a plain ShowConstraints IR node (no YIELD).
func NewShowConstraints() *ShowConstraints { return &ShowConstraints{} }

// Children implements LogicalPlan. ShowConstraints is a leaf.
func (*ShowConstraints) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. ShowConstraints introduces no variables into the
// enclosing plan: it is executed directly by the engine's DDL path, never
// composed with other operators.
func (*ShowConstraints) Vars() []string { return nil }

// ShowIndexes is a DDL plan node representing a SHOW INDEXES (or the singular
// SHOW INDEX) statement. It is a leaf node; it has no child plan and introduces
// no variables. Projection is nil for the plain form and non-nil when a YIELD /
// WHERE / RETURN sub-clause was supplied (#2044).
type ShowIndexes struct {
	Projection *ShowProjection
}

// NewShowIndexes creates a plain ShowIndexes IR node (no YIELD).
func NewShowIndexes() *ShowIndexes { return &ShowIndexes{} }

// Children implements LogicalPlan. ShowIndexes is a leaf.
func (*ShowIndexes) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan. ShowIndexes introduces no variables (see
// [ShowConstraints.Vars]).
func (*ShowIndexes) Vars() []string { return nil }
