// Package ir defines the logical plan intermediate representation (IR) for the
// Cypher query compiler. It covers the operator set described in Marton (2017),
// "An optimising Cypher to SQL compiler", adjusted for the openCypher 9 subset
// supported by this module.
//
// # Logical Plan
//
// A logical plan is a tree of [LogicalPlan] nodes. Each node is a concrete
// operator that knows its children and the set of variable names it introduces
// or requires. Leaf operators (AllNodesScan, NodeByLabelScan, etc.) have no
// children; unary operators (Selection, Projection, etc.) have exactly one
// child; binary operators (Apply, Union, etc.) have exactly two children.
//
// The IR is produced by the planner after semantic analysis and consumed by the
// optimiser and code-generation stages.
//
// # Operator taxonomy
//
// Scan operators read rows from the graph store:
//
//   - [Argument]              — injects bindings passed from an outer subplan
//   - [AllNodesScan]          — full node scan
//   - [NodeByLabelScan]       — scan nodes with a specific label
//   - [NodeByIndexSeek]       — exact-match index lookup
//   - [NodeByIndexRangeScan]  — range-scan index lookup
//
// Traversal operators follow relationships:
//
//   - [Expand]                — single-hop relationship expansion
//   - [OptionalExpand]        — left-outer-join expansion (OPTIONAL MATCH)
//   - [VarLengthExpand]       — variable-length path expansion
//   - [ProjectEndpoints]      — project start/end nodes of a relationship variable
//
// Filter and projection operators reshape the row stream:
//
//   - [Selection]             — WHERE predicate
//   - [Projection]            — RETURN / WITH column computation
//   - [EagerAggregation]      — GROUP BY + aggregate functions
//   - [Sort]                  — ORDER BY
//   - [Top]                   — ORDER BY … LIMIT (fused operator)
//   - [Limit]                 — LIMIT
//   - [Skip]                  — SKIP
//   - [Distinct]              — DISTINCT deduplication
//
// Set operators combine row streams:
//
//   - [Union]                 — UNION (with duplicate elimination)
//   - [UnionAll]              — UNION ALL (without duplicate elimination)
//
// Apply-family operators implement correlated subplan evaluation:
//
//   - [Apply]                 — correlated join (inner must produce ≥1 row)
//   - [SemiApply]             — EXISTS-style filter (keep outer if inner non-empty)
//   - [AntiSemiApply]         — NOT EXISTS-style filter (keep outer if inner empty)
//   - [RollUpApply]           — collect inner rows into a list column
//
// Pipeline operators control evaluation order and data flow:
//
//   - [Eager]                 — full materialisation barrier
//   - [Unwind]                — list expansion (UNWIND)
//   - [ProduceResults]        — root operator; defines the result columns
//
// Write operators mutate the graph:
//
//   - [CreateNode]            — CREATE (node)
//   - [CreateRelationship]    — CREATE (relationship)
//   - [SetProperty]           — SET n.prop = expr
//   - [SetLabels]             — SET n:Label
//   - [RemoveProperty]        — REMOVE n.prop
//   - [RemoveLabels]          — REMOVE n:Label
//   - [DeleteNode]            — DELETE node
//   - [DeleteRelationship]    — DELETE relationship
//   - [DetachDelete]          — DETACH DELETE node
//   - [Merge]                 — MERGE pattern
//
// Procedure operators call stored procedures:
//
//   - [ProcedureCall]         — CALL procedure(…) YIELD …
//
// # Concurrency
//
// Logical plan nodes are immutable value trees constructed by the planner.
// Concurrent reads are safe without external locking. Nodes must not be
// mutated after construction.
package ir
