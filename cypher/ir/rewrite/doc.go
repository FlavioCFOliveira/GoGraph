// Package rewrite provides a rule-based IR plan-rewrite framework for the
// GoGraph Cypher engine. It is an experimental, standalone API intended for
// embedding use cases that want to apply custom optimisation passes to a
// [github.com/FlavioCFOliveira/GoGraph/cypher/ir.LogicalPlan] before execution.
//
// # Status: not wired into the production engine
//
// The GoGraph engine ([github.com/FlavioCFOliveira/GoGraph/cypher.Engine])
// executes plans produced by [github.com/FlavioCFOliveira/GoGraph/cypher/ir.FromAST]
// directly, without invoking any rewrite [Driver]. The rules in this package are
// therefore INACTIVE in the standard engine path — they are unit-tested but do not
// run during query execution.
//
// Embedders wishing to apply these rules should construct a [Registry], register
// the desired rules, build a [Driver], and call [Driver.Run] on the plan before
// passing it to the engine's low-level execution API. All rules are documented
// with their semantic assumptions; verify compliance with your workload before
// enabling.
//
// See also: [cypher/ir] for plan node types, [cypher.Engine] for the execution entry
// point.
package rewrite
