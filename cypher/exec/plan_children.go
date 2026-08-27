package exec

// This file gives every operator that draws rows from another operator its
// [PlanChildren] method, so the physical plan tree can be recovered for
// rendering and profiling (rmp #2222).
//
// The methods live together rather than beside each operator because they form
// one contract that must be COMPLETE: a single missing method truncates the
// rendered plan at that node and silently hides everything beneath it.
// TestPlanChildren_EveryOperatorWithInputsImplementsIt derives the obligation
// from the source and fails the build when a new operator forgets it.
//
// Inputs are returned in EXECUTION order, which for an asymmetric operator is
// the order that explains its cost: a join yields its build side before its
// probe side, an apply its outer before its inner.
//
// The columnar operators are deliberately absent. ColumnarFilter, ColumnarLimit
// and ColumnarProject embed their row-mode counterparts (Filter, Limit, *Project)
// and hold the SAME child object again as a ChunkProducer, so the promoted method
// already reports the right input — and a node still renders as ColumnarFilter,
// because the name comes from the concrete type. ColumnarHashJoin embeds nothing
// and so declares its own below.

// --- a single input --------------------------------------------------------

// PlanChildren reports the operator whose rows it searches paths from.
func (op *AllShortestPaths) PlanChildren() []Operator { return []Operator{op.input} }

// PlanChildren reports the input whose rows it creates a node for.
func (op *CreateNode) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows it creates a relationship for.
func (op *CreateRelationship) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the nodes it deletes.
func (op *DeleteNode) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the relationships it deletes.
func (op *DeleteRelationship) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the nodes it detaches and deletes.
func (op *DetachDelete) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it deduplicates.
func (op *Distinct) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it fully materialises before emitting.
func (op *Eager) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it groups and aggregates.
func (op *EagerAggregation) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the operator whose rows it expands from.
func (op *Expand) PlanChildren() []Operator { return []Operator{op.input} }

// PlanChildren reports the input it filters.
func (op *Filter) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the aggregation it adapts to a single global group.
func (op *GlobalAggregateAdapter) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it truncates.
func (op *Limit) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows drive the merge.
func (op *Merge) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows drive the pattern merge.
func (op *MergePattern) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows drive the relationship merge.
func (op *MergeRelationship) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows drive the procedure call.
func (op *ProcedureCallOp) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it projects.
func (op *CountRows) PlanChildren() []Operator { return []Operator{op.child} }

func (op *Project) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the nodes it relabels.
func (op *RemoveLabels) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the entities it strips.
func (op *RemoveProperty) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the entities it overwrites.
func (op *SetAllProperties) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the nodes it labels.
func (op *SetLabels) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose rows name the entities it updates.
func (op *SetProperty) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the operator whose rows it searches paths from.
func (op *ShortestPath) PlanChildren() []Operator { return []Operator{op.input} }

// PlanChildren reports the input whose leading rows it discards.
func (op *Skip) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it orders.
func (op *Sort) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input it orders and truncates.
func (op *Top) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the input whose list column it expands.
func (op *Unwind) PlanChildren() []Operator { return []Operator{op.child} }

// PlanChildren reports the operator whose rows it expands from.
func (op *VarLengthExpand) PlanChildren() []Operator { return []Operator{op.input} }

// PlanChildren reports the plan this result set drains. ResultSet is the
// pipeline root the driver pulls from.
func (op *ResultSet) PlanChildren() []Operator { return []Operator{op.plan} }

// --- two inputs: the outer drives, the inner is re-driven per outer row ----

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *AntiSemiApply) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *Apply) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *CorrelatedApply) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *Foreach) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *OptionalApply) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *RollUpApply) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the outer input that drives this operator, then the
// inner sub-plan it re-drives for each outer row.
func (op *SemiApply) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// --- joins: the build side first, because it is materialised before probing --

// PlanChildren reports the build side first, then the probe side: the build input
// is materialised into the hash table before a single probe row is read, which is
// the order that explains the join's cost.
func (op *HashJoin) PlanChildren() []Operator { return []Operator{op.build, op.probe} }

// PlanChildren reports the outer input that drives the seek, then the inner arm.
//
// The order is the OPPOSITE of the hash joins' above, and for the same reason
// theirs is build-first: it is the order that explains the cost. This join
// materialises nothing — the outer arm drives it and each outer row is answered by
// an index seek, so the outer side is what the row count multiplies.
//
// The inner arm is reported even though most queries never execute it: it serves
// only the fallback path (an integer key too large for float64 to represent
// exactly), and rendering it is what makes that path visible in a plan rather than
// a surprise in a profile.
func (op *IndexNestedLoopJoin) PlanChildren() []Operator { return []Operator{op.outer, op.inner} }

// PlanChildren reports the build side first, then the probe side, as
// [HashJoin.PlanChildren] does.
//
// ColumnarHashJoin declares its own rather than inheriting: it embeds nothing,
// holding both sides as [ChunkProducer]. Rendering it under its own name is how
// the columnar tier becomes visible in a plan (rmp #2222 AC 1) — the row-mode
// HashJoin and this one are different operators with different costs, and a
// reader must be able to tell which ran.
func (op *ColumnarHashJoin) PlanChildren() []Operator { return []Operator{op.build, op.probe} }

// --- set operations --------------------------------------------------------

// PlanChildren reports the two inputs whose rows it concatenates.
func (op *UnionAll) PlanChildren() []Operator { return []Operator{op.left, op.right} }

// PlanChildren reports the Distinct it wraps.
//
// Union is a Distinct over a UnionAll; reporting the Distinct keeps the rendered
// tree faithful to what actually executes rather than flattening two operators
// into one.
func (op *Union) PlanChildren() []Operator { return []Operator{op.inner} }

// PlanChildren reports the upstream input first, then the Expand it wraps.
//
// OptionalExpand holds both its upstream input and the Expand it re-drives one
// row at a time (through singleArg). Both are real operators in the executed
// pipeline, so both are reported; input comes first because it drives.
func (op *OptionalExpand) PlanChildren() []Operator { return []Operator{op.input, op.child} }
