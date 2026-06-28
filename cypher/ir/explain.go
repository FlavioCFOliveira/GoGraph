package ir

import (
	"strings"
)

// Explain returns a human-readable tree representation of plan, mirroring the
// style of Neo4j's textual EXPLAIN output. Each line shows the operator name
// and the variables it introduces or requires. A "-" placeholder occupies the
// cardinality column, which will be populated by the PROFILE stage in Sprint 28.
//
// Tree edges use box-drawing characters (├─, └─, │) for clarity. The output is
// deterministic for a given plan tree.
//
// Example:
//
//	ProduceResults [n]
//	└─ Projection [n]
//	   └─ NodeByLabelScan [n:Person]
func Explain(plan LogicalPlan) string {
	var b strings.Builder
	explainNode(&b, plan, "", true, true)
	return b.String()
}

// explainNode writes one line for plan then recurses into its children.
//
// prefix is the continuation string inherited from the parent (e.g. "   " or
// "│  "). isRoot marks the top-level call so no branch connector is emitted on
// the node's own line. isLast marks whether this node is the last child of its
// parent, controlling which connector (└─ vs ├─) to use.
func explainNode(b *strings.Builder, plan LogicalPlan, prefix string, isRoot, isLast bool) {
	// Connector prefixed onto this node's own line.
	var connector string
	// Continuation string passed to direct children of this node.
	var childContinuation string

	if isRoot {
		connector = ""
		childContinuation = ""
	} else if isLast {
		connector = "└─ "
		childContinuation = "   "
	} else {
		connector = "├─ "
		childContinuation = "│  "
	}

	b.WriteString(prefix)
	b.WriteString(connector)
	b.WriteString(operatorName(plan))
	b.WriteString(" [")
	b.WriteString(operatorVars(plan))
	b.WriteString("]")
	b.WriteByte('\n')

	children := plan.Children()
	nextPrefix := prefix + childContinuation
	for i, child := range children {
		explainNode(b, child, nextPrefix, false, i == len(children)-1)
	}
}

// OperatorName returns the canonical display name for the logical plan operator.
// It is the exported counterpart of the internal operatorName helper used by
// Explain, exposed so callers outside this package can render individual nodes
// (e.g. the Engine's physical-plan explain renderer).
func OperatorName(plan LogicalPlan) string { return operatorName(plan) }

// operatorName returns the canonical display name for each logical plan operator.
func operatorName(plan LogicalPlan) string {
	switch plan.(type) {
	// Scan / leaf operators
	case *Argument:
		return "Argument"
	case *AllNodesScan:
		return "AllNodesScan"
	case *NodeByLabelScan:
		return "NodeByLabelScan"
	case *NodeByIndexSeek:
		return "NodeByIndexSeek"
	case *NodeByIndexRangeScan:
		return "NodeByIndexRangeScan"

	// Traversal operators
	case *Expand:
		return "Expand"
	case *OptionalExpand:
		return "OptionalExpand"
	case *VarLengthExpand:
		return "VarLengthExpand"
	case *ProjectEndpoints:
		return "ProjectEndpoints"
	case *NamedPath:
		return "NamedPath"

	// Filter and projection
	case *Selection:
		return "Selection"
	case *Projection:
		return "Projection"
	case *EagerAggregation:
		return "EagerAggregation"

	// Ordering and pagination
	case *Sort:
		return "Sort"
	case *Top:
		return "Top"
	case *Limit:
		return "Limit"
	case *Skip:
		return "Skip"
	case *Distinct:
		return "Distinct"

	// Set operators
	case *Union:
		return "Union"
	case *UnionAll:
		return "UnionAll"

	// Apply-family operators. A plain (uncorrelated) Apply implements
	// Cartesian-product semantics — a disconnected MATCH such as
	// MATCH (a), (b). Label it CartesianProduct in EXPLAIN to match Neo4j's
	// operator name and make the cardinality-blowup join greppable (#1807).
	// The correlated variant is CorrelatedApply below.
	case *Apply:
		return "CartesianProduct"
	case *CorrelatedApply:
		return "CorrelatedApply"
	case *OptionalApply:
		return "OptionalApply"
	case *SemiApply:
		return "SemiApply"
	case *AntiSemiApply:
		return "AntiSemiApply"
	case *RollUpApply:
		return "RollUpApply"
	case *SubqueryExists:
		return "SubqueryExists"
	case *SubqueryCount:
		return "SubqueryCount"

	// Pipeline operators
	case *Eager:
		return "Eager"
	case *Unwind:
		return "Unwind"
	case *ProduceResults:
		return "ProduceResults"

	// Write operators
	case *CreateNode:
		return "CreateNode"
	case *CreateRelationship:
		return "CreateRelationship"
	case *SetProperty:
		return "SetProperty"
	case *SetAllProperties:
		return "SetAllProperties"
	case *SetLabels:
		return "SetLabels"
	case *RemoveProperty:
		return "RemoveProperty"
	case *RemoveLabels:
		return "RemoveLabels"
	case *DeleteNode:
		return "DeleteNode"
	case *DeleteRelationship:
		return "DeleteRelationship"
	case *DetachDelete:
		return "DetachDelete"
	case *Merge:
		return "Merge"
	case *ProcedureCall:
		return "ProcedureCall"

	default:
		return "Unknown"
	}
}

// operatorVars returns the vars string shown in brackets next to the operator
// name. For most operators this is just the comma-joined Vars() slice; for a
// few operators contextual detail (label, predicate, …) is included.
func operatorVars(plan LogicalPlan) string {
	switch p := plan.(type) {
	case *NodeByLabelScan:
		return p.NodeVar + ":" + p.Label
	case *NodeByIndexSeek:
		return p.NodeVar + ":" + p.Label + "." + p.Property + " = " + p.Value
	case *NodeByIndexRangeScan:
		var b strings.Builder
		b.WriteString(p.NodeVar)
		b.WriteByte(':')
		b.WriteString(p.Label)
		b.WriteByte('.')
		b.WriteString(p.Property)
		if p.Min != nil {
			if p.Min.Inclusive {
				b.WriteString(" >= ")
			} else {
				b.WriteString(" > ")
			}
			b.WriteString(p.Min.Value)
		}
		if p.Max != nil {
			if p.Max.Inclusive {
				b.WriteString(" <= ")
			} else {
				b.WriteString(" < ")
			}
			b.WriteString(p.Max.Value)
		}
		return b.String()
	case *Selection:
		return p.Predicate
	case *Unwind:
		return p.ListExpression + " AS " + p.ElementVar
	case *Merge:
		return p.Pattern
	default:
		return strings.Join(plan.Vars(), ", ")
	}
}
