package ir

import "gograph/cypher/ast"

// LogicalPlan is the root interface implemented by every logical-plan operator.
// Children returns the operator's child plans in evaluation order (left to right
// for binary operators). Vars returns the set of variable names produced or
// consumed by this operator.
//
// Concurrency: plan trees are immutable after construction; concurrent reads
// are safe without external locking.
type LogicalPlan interface {
	// Children returns the direct children of this operator. Leaf operators
	// return nil. Unary operators return a single-element slice. Binary
	// operators return a two-element slice [left, right].
	Children() []LogicalPlan

	// Vars returns the variable names introduced or required by this operator.
	// The order is unspecified but deterministic for a given operator instance.
	Vars() []string
}

// ─────────────────────────────────────────────────────────────────────────────
// Direction enumerates relationship traversal directions.
// ─────────────────────────────────────────────────────────────────────────────

// Direction is the traversal direction for relationship expansion operators.
type Direction uint8

const (
	// DirectionOutgoing follows relationships in the outgoing direction (→).
	DirectionOutgoing Direction = iota
	// DirectionIncoming follows relationships in the incoming direction (←).
	DirectionIncoming
	// DirectionBoth follows relationships in either direction (—).
	DirectionBoth
)

// ─────────────────────────────────────────────────────────────────────────────
// Supporting value types
// ─────────────────────────────────────────────────────────────────────────────

// ProjectionItem is a named expression in a Projection operator.
type ProjectionItem struct {
	// Name is the output variable name (the AS alias, or the expression's
	// canonical string representation when no alias is given).
	Name string
	// Expression is an opaque string representation of the expression.
	Expression string
	// Expr is the parsed AST for the expression, when available. Nil for
	// legacy or string-only callers. When non-nil, the executor evaluates it
	// via expr.Eval rather than falling back to a schema-key lookup.
	Expr ast.Expression
}

// AggregateExpr is a named aggregate function in an EagerAggregation operator.
type AggregateExpr struct {
	// OutputName is the variable name assigned to the aggregate result.
	OutputName string
	// Function is the aggregate function name (e.g. "count", "sum", "avg").
	Function string
	// Argument is the expression argument to the aggregate function. An empty
	// string corresponds to count(*).
	Argument string
	// Distinct indicates whether the DISTINCT qualifier is applied inside the
	// aggregate (e.g. count(DISTINCT n)).
	Distinct bool
}

// Bound is an optional inclusive bound used in range-scan operators. A nil
// pointer means the bound is absent (open-ended range).
type Bound struct {
	// Value is the opaque string representation of the bound expression.
	Value string
	// Inclusive reports whether the bound is inclusive (≤/≥) or exclusive (</>).
	Inclusive bool
}

// SortItem is a single ORDER BY term in a Sort or Top operator.
type SortItem struct {
	// Expression is an opaque string representation of the sort key expression.
	Expression string
	// Descending indicates DESC ordering; false means ASC.
	Descending bool
}

// ─────────────────────────────────────────────────────────────────────────────
// Scan operators (leaf nodes)
// ─────────────────────────────────────────────────────────────────────────────

// Argument injects a set of variable bindings from an outer subplan into an
// inner subplan. It is the leaf node on the inner side of Apply-family operators.
type Argument struct {
	// Variables holds the variable names that are injected from the outer scope.
	Variables []string
}

// NewArgument creates an Argument operator with the given injected variables.
func NewArgument(vars []string) *Argument {
	cp := make([]string, len(vars))
	copy(cp, vars)
	return &Argument{Variables: cp}
}

// Children implements LogicalPlan. Argument is a leaf; it returns nil.
func (a *Argument) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan.
func (a *Argument) Vars() []string { return a.Variables }

// ─────────────────────────────────────────────────────────────────────────────

// AllNodesScan scans every node in the graph and binds each to NodeVar.
type AllNodesScan struct {
	// NodeVar is the variable name bound to each scanned node.
	NodeVar string
}

// NewAllNodesScan creates an AllNodesScan operator.
func NewAllNodesScan(nodeVar string) *AllNodesScan {
	return &AllNodesScan{NodeVar: nodeVar}
}

// Children implements LogicalPlan. AllNodesScan is a leaf; it returns nil.
func (a *AllNodesScan) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan.
func (a *AllNodesScan) Vars() []string { return []string{a.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// NodeByLabelScan scans nodes that carry a specific label and binds each to
// NodeVar.
type NodeByLabelScan struct {
	// NodeVar is the variable name bound to each scanned node.
	NodeVar string
	// Label is the node label to filter on.
	Label string
}

// NewNodeByLabelScan creates a NodeByLabelScan operator.
func NewNodeByLabelScan(nodeVar, label string) *NodeByLabelScan {
	return &NodeByLabelScan{NodeVar: nodeVar, Label: label}
}

// Children implements LogicalPlan. NodeByLabelScan is a leaf; it returns nil.
func (n *NodeByLabelScan) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan.
func (n *NodeByLabelScan) Vars() []string { return []string{n.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// NodeByIndexSeek performs an exact-match index lookup and binds each matching
// node to NodeVar.
type NodeByIndexSeek struct {
	// NodeVar is the variable name bound to each matching node.
	NodeVar string
	// Label is the indexed node label.
	Label string
	// Property is the indexed property key.
	Property string
	// Value is the opaque string representation of the exact-match value.
	Value string
}

// NewNodeByIndexSeek creates a NodeByIndexSeek operator.
func NewNodeByIndexSeek(nodeVar, label, property, value string) *NodeByIndexSeek {
	return &NodeByIndexSeek{
		NodeVar:  nodeVar,
		Label:    label,
		Property: property,
		Value:    value,
	}
}

// Children implements LogicalPlan. NodeByIndexSeek is a leaf; it returns nil.
func (n *NodeByIndexSeek) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan.
func (n *NodeByIndexSeek) Vars() []string { return []string{n.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// NodeByIndexRangeScan performs a range-scan index lookup on a property and
// binds each matching node to NodeVar. Either Min or Max (or both) must be
// non-nil; a nil bound means the range is open on that end.
type NodeByIndexRangeScan struct {
	// NodeVar is the variable name bound to each matching node.
	NodeVar string
	// Label is the indexed node label.
	Label string
	// Property is the indexed property key.
	Property string
	// Min is the lower bound (inclusive/exclusive) or nil for an open lower end.
	Min *Bound
	// Max is the upper bound (inclusive/exclusive) or nil for an open upper end.
	Max *Bound
}

// NewNodeByIndexRangeScan creates a NodeByIndexRangeScan operator.
func NewNodeByIndexRangeScan(nodeVar, label, property string, lower, upper *Bound) *NodeByIndexRangeScan {
	return &NodeByIndexRangeScan{
		NodeVar:  nodeVar,
		Label:    label,
		Property: property,
		Min:      lower,
		Max:      upper,
	}
}

// Children implements LogicalPlan. NodeByIndexRangeScan is a leaf; it returns nil.
func (n *NodeByIndexRangeScan) Children() []LogicalPlan { return nil }

// Vars implements LogicalPlan.
func (n *NodeByIndexRangeScan) Vars() []string { return []string{n.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────
// Traversal operators
// ─────────────────────────────────────────────────────────────────────────────

// Expand performs a single-hop relationship expansion from an already-bound
// FromVar and introduces a RelVar (relationship) and ToVar (destination node).
type Expand struct {
	// FromVar is the already-bound source node variable.
	FromVar string
	// RelVar is the variable name bound to the traversed relationship.
	RelVar string
	// RelTypes is the list of allowed relationship types. An empty slice means
	// any type is accepted.
	RelTypes []string
	// Direction is the traversal direction relative to FromVar.
	Direction Direction
	// ToVar is the variable name bound to the destination node.
	ToVar string
	// Child is the subplan that produces FromVar.
	Child LogicalPlan
}

// NewExpand creates an Expand operator.
func NewExpand(fromVar, relVar string, relTypes []string, dir Direction, toVar string, child LogicalPlan) *Expand {
	rt := make([]string, len(relTypes))
	copy(rt, relTypes)
	return &Expand{
		FromVar:   fromVar,
		RelVar:    relVar,
		RelTypes:  rt,
		Direction: dir,
		ToVar:     toVar,
		Child:     child,
	}
}

// Children implements LogicalPlan.
func (e *Expand) Children() []LogicalPlan { return []LogicalPlan{e.Child} }

// Vars implements LogicalPlan.
func (e *Expand) Vars() []string { return []string{e.RelVar, e.ToVar} }

// ─────────────────────────────────────────────────────────────────────────────

// OptionalExpand performs a left-outer-join relationship expansion. When no
// relationship is found the row is kept with RelVar and ToVar bound to null.
type OptionalExpand struct {
	// FromVar is the already-bound source node variable.
	FromVar string
	// RelVar is the variable name bound to the traversed relationship (or null).
	RelVar string
	// RelTypes is the list of allowed relationship types. An empty slice means
	// any type is accepted.
	RelTypes []string
	// Direction is the traversal direction relative to FromVar.
	Direction Direction
	// ToVar is the variable name bound to the destination node (or null).
	ToVar string
	// Child is the subplan that produces FromVar.
	Child LogicalPlan
}

// NewOptionalExpand creates an OptionalExpand operator.
func NewOptionalExpand(fromVar, relVar string, relTypes []string, dir Direction, toVar string, child LogicalPlan) *OptionalExpand {
	rt := make([]string, len(relTypes))
	copy(rt, relTypes)
	return &OptionalExpand{
		FromVar:   fromVar,
		RelVar:    relVar,
		RelTypes:  rt,
		Direction: dir,
		ToVar:     toVar,
		Child:     child,
	}
}

// Children implements LogicalPlan.
func (o *OptionalExpand) Children() []LogicalPlan { return []LogicalPlan{o.Child} }

// Vars implements LogicalPlan.
func (o *OptionalExpand) Vars() []string { return []string{o.RelVar, o.ToVar} }

// ─────────────────────────────────────────────────────────────────────────────

// VarLengthExpand performs a variable-length path expansion between MinDepth and
// MaxDepth hops. A MaxDepth of zero means unbounded.
type VarLengthExpand struct {
	// FromVar is the already-bound source node variable.
	FromVar string
	// RelVar is the variable name bound to the collected path relationships.
	RelVar string
	// RelTypes is the list of allowed relationship types. An empty slice means
	// any type is accepted.
	RelTypes []string
	// Direction is the traversal direction relative to FromVar.
	Direction Direction
	// ToVar is the variable name bound to the destination node.
	ToVar string
	// MinDepth is the minimum number of hops (inclusive, ≥1).
	MinDepth int
	// MaxDepth is the maximum number of hops (inclusive). Zero means unbounded.
	MaxDepth int
	// Child is the subplan that produces FromVar.
	Child LogicalPlan
}

// NewVarLengthExpand creates a VarLengthExpand operator.
func NewVarLengthExpand(fromVar, relVar string, relTypes []string, dir Direction, toVar string, minDepth, maxDepth int, child LogicalPlan) *VarLengthExpand {
	rt := make([]string, len(relTypes))
	copy(rt, relTypes)
	return &VarLengthExpand{
		FromVar:   fromVar,
		RelVar:    relVar,
		RelTypes:  rt,
		Direction: dir,
		ToVar:     toVar,
		MinDepth:  minDepth,
		MaxDepth:  maxDepth,
		Child:     child,
	}
}

// Children implements LogicalPlan.
func (v *VarLengthExpand) Children() []LogicalPlan { return []LogicalPlan{v.Child} }

// Vars implements LogicalPlan.
func (v *VarLengthExpand) Vars() []string { return []string{v.RelVar, v.ToVar} }

// ─────────────────────────────────────────────────────────────────────────────

// ProjectEndpoints projects the start and/or end nodes of a relationship
// variable that is already in scope.
type ProjectEndpoints struct {
	// RelVar is the already-bound relationship variable.
	RelVar string
	// StartVar is the variable name bound to the start node (may be empty if
	// not needed).
	StartVar string
	// EndVar is the variable name bound to the end node (may be empty if not
	// needed).
	EndVar string
	// Child is the subplan that produces RelVar.
	Child LogicalPlan
}

// NewProjectEndpoints creates a ProjectEndpoints operator.
func NewProjectEndpoints(relVar, startVar, endVar string, child LogicalPlan) *ProjectEndpoints {
	return &ProjectEndpoints{
		RelVar:   relVar,
		StartVar: startVar,
		EndVar:   endVar,
		Child:    child,
	}
}

// Children implements LogicalPlan.
func (p *ProjectEndpoints) Children() []LogicalPlan { return []LogicalPlan{p.Child} }

// Vars implements LogicalPlan.
func (p *ProjectEndpoints) Vars() []string {
	out := make([]string, 0, 2)
	if p.StartVar != "" {
		out = append(out, p.StartVar)
	}
	if p.EndVar != "" {
		out = append(out, p.EndVar)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Filter and projection operators
// ─────────────────────────────────────────────────────────────────────────────

// Selection filters rows from its child using Predicate. It corresponds to a
// WHERE clause in the logical plan.
type Selection struct {
	// Predicate is the opaque string representation of the filter expression.
	Predicate string
	// PredicateExpr is the parsed AST for the predicate, when available. Nil
	// for legacy or string-only callers. When non-nil, the executor evaluates
	// it via expr.Eval rather than the pass-through stub.
	PredicateExpr ast.Expression
	// Child is the subplan whose rows are filtered.
	Child LogicalPlan
}

// NewSelection creates a Selection operator with a string-only predicate.
func NewSelection(predicate string, child LogicalPlan) *Selection {
	return &Selection{Predicate: predicate, Child: child}
}

// NewSelectionExpr creates a Selection with both the string predicate and its
// parsed AST. The executor uses PredicateExpr when non-nil, falling back to
// the pass-through stub otherwise.
func NewSelectionExpr(predicate string, predExpr ast.Expression, child LogicalPlan) *Selection {
	return &Selection{Predicate: predicate, PredicateExpr: predExpr, Child: child}
}

// Children implements LogicalPlan.
func (s *Selection) Children() []LogicalPlan { return []LogicalPlan{s.Child} }

// Vars implements LogicalPlan. Selection does not introduce new variables; it
// passes through the child's variables.
func (s *Selection) Vars() []string { return s.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// Projection computes a set of named expressions (RETURN / WITH items) from its
// child's rows. Only the columns declared in Items are propagated downstream.
type Projection struct {
	// Items is the ordered list of output columns.
	Items []ProjectionItem
	// Child is the subplan whose rows are projected.
	Child LogicalPlan
}

// NewProjection creates a Projection operator.
func NewProjection(items []ProjectionItem, child LogicalPlan) *Projection {
	cp := make([]ProjectionItem, len(items))
	copy(cp, items)
	return &Projection{Items: cp, Child: child}
}

// Children implements LogicalPlan.
func (p *Projection) Children() []LogicalPlan { return []LogicalPlan{p.Child} }

// Vars implements LogicalPlan.
func (p *Projection) Vars() []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Name
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────

// EagerAggregation groups rows from its child by GroupBy keys and computes
// aggregate functions over each group.
type EagerAggregation struct {
	// GroupBy is the ordered list of grouping key variable names.
	GroupBy []string
	// Aggregates is the list of aggregate expressions computed per group.
	Aggregates []AggregateExpr
	// Child is the subplan whose rows are aggregated.
	Child LogicalPlan
}

// NewEagerAggregation creates an EagerAggregation operator.
func NewEagerAggregation(groupBy []string, aggregates []AggregateExpr, child LogicalPlan) *EagerAggregation {
	gb := make([]string, len(groupBy))
	copy(gb, groupBy)
	agg := make([]AggregateExpr, len(aggregates))
	copy(agg, aggregates)
	return &EagerAggregation{GroupBy: gb, Aggregates: agg, Child: child}
}

// Children implements LogicalPlan.
func (e *EagerAggregation) Children() []LogicalPlan { return []LogicalPlan{e.Child} }

// Vars implements LogicalPlan. The output variables are the grouping keys
// followed by the aggregate output names.
func (e *EagerAggregation) Vars() []string {
	out := make([]string, 0, len(e.GroupBy)+len(e.Aggregates))
	out = append(out, e.GroupBy...)
	for _, a := range e.Aggregates {
		out = append(out, a.OutputName)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────

// Sort orders rows from its child according to SortItems.
type Sort struct {
	// SortItems is the ordered list of sort keys.
	SortItems []SortItem
	// Child is the subplan whose rows are sorted.
	Child LogicalPlan
}

// NewSort creates a Sort operator.
func NewSort(items []SortItem, child LogicalPlan) *Sort {
	cp := make([]SortItem, len(items))
	copy(cp, items)
	return &Sort{SortItems: cp, Child: child}
}

// Children implements LogicalPlan.
func (s *Sort) Children() []LogicalPlan { return []LogicalPlan{s.Child} }

// Vars implements LogicalPlan. Sort does not introduce new variables.
func (s *Sort) Vars() []string { return s.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// Top is a fused ORDER BY … LIMIT operator that sorts and truncates in a single
// pass, avoiding the need to materialise the full sorted result.
type Top struct {
	// SortItems is the ordered list of sort keys.
	SortItems []SortItem
	// Limit is the maximum number of rows to produce.
	Limit int64
	// Child is the subplan whose rows are sorted and truncated.
	Child LogicalPlan
}

// NewTop creates a Top operator.
func NewTop(items []SortItem, limit int64, child LogicalPlan) *Top {
	cp := make([]SortItem, len(items))
	copy(cp, items)
	return &Top{SortItems: cp, Limit: limit, Child: child}
}

// Children implements LogicalPlan.
func (t *Top) Children() []LogicalPlan { return []LogicalPlan{t.Child} }

// Vars implements LogicalPlan. Top does not introduce new variables.
func (t *Top) Vars() []string { return t.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// Limit truncates the row stream from its child to at most Count rows.
type Limit struct {
	// Count is the maximum number of rows to pass through.
	Count int64
	// Child is the subplan whose output is truncated.
	Child LogicalPlan
}

// NewLimit creates a Limit operator.
func NewLimit(count int64, child LogicalPlan) *Limit {
	return &Limit{Count: count, Child: child}
}

// Children implements LogicalPlan.
func (l *Limit) Children() []LogicalPlan { return []LogicalPlan{l.Child} }

// Vars implements LogicalPlan. Limit does not introduce new variables.
func (l *Limit) Vars() []string { return l.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// Skip discards the first Count rows from its child's output.
type Skip struct {
	// Count is the number of leading rows to skip.
	Count int64
	// Child is the subplan whose leading rows are discarded.
	Child LogicalPlan
}

// NewSkip creates a Skip operator.
func NewSkip(count int64, child LogicalPlan) *Skip {
	return &Skip{Count: count, Child: child}
}

// Children implements LogicalPlan.
func (s *Skip) Children() []LogicalPlan { return []LogicalPlan{s.Child} }

// Vars implements LogicalPlan. Skip does not introduce new variables.
func (s *Skip) Vars() []string { return s.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// Distinct removes duplicate rows from its child's output. Rows are equal when
// all column values are equal.
type Distinct struct {
	// Child is the subplan whose duplicate rows are removed.
	Child LogicalPlan
}

// NewDistinct creates a Distinct operator.
func NewDistinct(child LogicalPlan) *Distinct {
	return &Distinct{Child: child}
}

// Children implements LogicalPlan.
func (d *Distinct) Children() []LogicalPlan { return []LogicalPlan{d.Child} }

// Vars implements LogicalPlan. Distinct does not introduce new variables.
func (d *Distinct) Vars() []string { return d.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────
// Set operators
// ─────────────────────────────────────────────────────────────────────────────

// Union computes the set union of two row streams, eliminating duplicates.
// The left and right children must produce the same column schema.
type Union struct {
	// Left is the first input subplan.
	Left LogicalPlan
	// Right is the second input subplan.
	Right LogicalPlan
}

// NewUnion creates a Union operator.
func NewUnion(left, right LogicalPlan) *Union {
	return &Union{Left: left, Right: right}
}

// Children implements LogicalPlan. Returns [Left, Right].
func (u *Union) Children() []LogicalPlan { return []LogicalPlan{u.Left, u.Right} }

// Vars implements LogicalPlan. Returns the left child's variables (both sides
// must produce the same schema).
func (u *Union) Vars() []string { return u.Left.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// UnionAll computes the multiset union (bag union) of two row streams without
// duplicate elimination.
type UnionAll struct {
	// Left is the first input subplan.
	Left LogicalPlan
	// Right is the second input subplan.
	Right LogicalPlan
}

// NewUnionAll creates a UnionAll operator.
func NewUnionAll(left, right LogicalPlan) *UnionAll {
	return &UnionAll{Left: left, Right: right}
}

// Children implements LogicalPlan. Returns [Left, Right].
func (u *UnionAll) Children() []LogicalPlan { return []LogicalPlan{u.Left, u.Right} }

// Vars implements LogicalPlan.
func (u *UnionAll) Vars() []string { return u.Left.Vars() }

// ─────────────────────────────────────────────────────────────────────────────
// Apply-family operators
// ─────────────────────────────────────────────────────────────────────────────

// Apply is a correlated join: for each row produced by Outer, Inner is
// evaluated with the outer bindings injected (via an Argument leaf). The
// combined row is emitted only when Inner produces at least one result row.
type Apply struct {
	// Outer is the driving (left) subplan.
	Outer LogicalPlan
	// Inner is the correlated (right) subplan.
	Inner LogicalPlan
}

// NewApply creates an Apply operator.
func NewApply(outer, inner LogicalPlan) *Apply {
	return &Apply{Outer: outer, Inner: inner}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (a *Apply) Children() []LogicalPlan { return []LogicalPlan{a.Outer, a.Inner} }

// Vars implements LogicalPlan.
func (a *Apply) Vars() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, v := range a.Outer.Vars() {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range a.Inner.Vars() {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────

// SemiApply implements an EXISTS-style filter: outer rows are kept only when
// Inner produces at least one result row for the given outer bindings. The
// inner variables are not propagated to the output.
type SemiApply struct {
	// Outer is the driving subplan.
	Outer LogicalPlan
	// Inner is the correlated existence-check subplan.
	Inner LogicalPlan
}

// NewSemiApply creates a SemiApply operator.
func NewSemiApply(outer, inner LogicalPlan) *SemiApply {
	return &SemiApply{Outer: outer, Inner: inner}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (s *SemiApply) Children() []LogicalPlan { return []LogicalPlan{s.Outer, s.Inner} }

// Vars implements LogicalPlan. Only outer variables are visible downstream.
func (s *SemiApply) Vars() []string { return s.Outer.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// AntiSemiApply implements a NOT EXISTS-style filter: outer rows are kept only
// when Inner produces zero result rows for the given outer bindings.
type AntiSemiApply struct {
	// Outer is the driving subplan.
	Outer LogicalPlan
	// Inner is the correlated non-existence-check subplan.
	Inner LogicalPlan
}

// NewAntiSemiApply creates an AntiSemiApply operator.
func NewAntiSemiApply(outer, inner LogicalPlan) *AntiSemiApply {
	return &AntiSemiApply{Outer: outer, Inner: inner}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (a *AntiSemiApply) Children() []LogicalPlan { return []LogicalPlan{a.Outer, a.Inner} }

// Vars implements LogicalPlan. Only outer variables are visible downstream.
func (a *AntiSemiApply) Vars() []string { return a.Outer.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// RollUpApply evaluates Inner for each outer row and collects all Inner result
// rows into a list, which is bound to CollectVar in the output row.
type RollUpApply struct {
	// Outer is the driving subplan.
	Outer LogicalPlan
	// Inner is the correlated subplan whose results are collected.
	Inner LogicalPlan
	// CollectVar is the variable name bound to the collected list.
	CollectVar string
}

// NewRollUpApply creates a RollUpApply operator.
func NewRollUpApply(outer, inner LogicalPlan, collectVar string) *RollUpApply {
	return &RollUpApply{Outer: outer, Inner: inner, CollectVar: collectVar}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (r *RollUpApply) Children() []LogicalPlan { return []LogicalPlan{r.Outer, r.Inner} }

// Vars implements LogicalPlan.
func (r *RollUpApply) Vars() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, v := range r.Outer.Vars() {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	out = append(out, r.CollectVar)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Pipeline operators
// ─────────────────────────────────────────────────────────────────────────────

// Eager forces full materialisation of its child's output before producing any
// rows. It acts as a pipeline-breaking barrier to ensure write-read isolation
// in queries that both read and write the graph.
type Eager struct {
	// Child is the subplan to materialise.
	Child LogicalPlan
}

// NewEager creates an Eager operator.
func NewEager(child LogicalPlan) *Eager {
	return &Eager{Child: child}
}

// Children implements LogicalPlan.
func (e *Eager) Children() []LogicalPlan { return []LogicalPlan{e.Child} }

// Vars implements LogicalPlan. Eager does not introduce new variables.
func (e *Eager) Vars() []string { return e.Child.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// Unwind expands a list-valued expression into one row per element, binding
// each element to ElementVar. It corresponds to the UNWIND clause.
type Unwind struct {
	// ListExpression is the opaque string representation of the list expression.
	ListExpression string
	// ListExpr is the parsed AST for the list expression, when available. Nil
	// means the expression could not be parsed; callers must fall back to a
	// static empty list. The executor uses ListExpr when non-nil.
	ListExpr ast.Expression
	// ElementVar is the variable name bound to each list element.
	ElementVar string
	// Child is the subplan that provides the context rows. May be nil when
	// UNWIND appears at the start of a query.
	Child LogicalPlan
}

// NewUnwind creates an Unwind operator with a string-only list expression.
func NewUnwind(listExpression, elementVar string, child LogicalPlan) *Unwind {
	return &Unwind{ListExpression: listExpression, ElementVar: elementVar, Child: child}
}

// NewUnwindExpr creates an Unwind operator with both the string and its parsed
// AST. The executor uses ListExpr when non-nil, falling back to an empty list.
func NewUnwindExpr(listExpression string, listExpr ast.Expression, elementVar string, child LogicalPlan) *Unwind {
	return &Unwind{ListExpression: listExpression, ListExpr: listExpr, ElementVar: elementVar, Child: child}
}

// Children implements LogicalPlan. Returns [Child] when Child is non-nil,
// otherwise nil.
func (u *Unwind) Children() []LogicalPlan {
	if u.Child == nil {
		return nil
	}
	return []LogicalPlan{u.Child}
}

// Vars implements LogicalPlan.
func (u *Unwind) Vars() []string { return []string{u.ElementVar} }

// ─────────────────────────────────────────────────────────────────────────────

// ProduceResults is the root operator of every logical plan tree. It names the
// final output columns that are returned to the query caller.
type ProduceResults struct {
	// Columns is the ordered list of output column names.
	Columns []string
	// Child is the subplan that produces the result rows.
	Child LogicalPlan
}

// NewProduceResults creates a ProduceResults operator.
func NewProduceResults(columns []string, child LogicalPlan) *ProduceResults {
	cp := make([]string, len(columns))
	copy(cp, columns)
	return &ProduceResults{Columns: cp, Child: child}
}

// Children implements LogicalPlan.
func (p *ProduceResults) Children() []LogicalPlan { return []LogicalPlan{p.Child} }

// Vars implements LogicalPlan.
func (p *ProduceResults) Vars() []string { return p.Columns }

// ─────────────────────────────────────────────────────────────────────────────
// Write operators
// ─────────────────────────────────────────────────────────────────────────────

// CreateNode creates a new graph node with the given labels and properties,
// binding it to NodeVar.
type CreateNode struct {
	// NodeVar is the variable name bound to the newly created node.
	NodeVar string
	// Labels is the list of labels assigned to the new node.
	Labels []string
	// Properties is the opaque string representation of the property map
	// expression (e.g. "{name: 'Alice'}").
	Properties string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewCreateNode creates a CreateNode operator.
func NewCreateNode(nodeVar string, labels []string, properties string, child LogicalPlan) *CreateNode {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &CreateNode{NodeVar: nodeVar, Labels: lb, Properties: properties, Child: child}
}

// Children implements LogicalPlan.
func (c *CreateNode) Children() []LogicalPlan { return []LogicalPlan{c.Child} }

// Vars implements LogicalPlan.
func (c *CreateNode) Vars() []string { return []string{c.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// CreateRelationship creates a new graph relationship between two already-bound
// nodes, binding the new relationship to RelVar.
type CreateRelationship struct {
	// StartVar is the already-bound source node variable.
	StartVar string
	// EndVar is the already-bound destination node variable.
	EndVar string
	// RelVar is the variable name bound to the newly created relationship.
	RelVar string
	// RelType is the relationship type.
	RelType string
	// Properties is the opaque string representation of the property map
	// expression.
	Properties string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewCreateRelationship creates a CreateRelationship operator.
func NewCreateRelationship(startVar, endVar, relVar, relType, properties string, child LogicalPlan) *CreateRelationship {
	return &CreateRelationship{
		StartVar:   startVar,
		EndVar:     endVar,
		RelVar:     relVar,
		RelType:    relType,
		Properties: properties,
		Child:      child,
	}
}

// Children implements LogicalPlan.
func (c *CreateRelationship) Children() []LogicalPlan { return []LogicalPlan{c.Child} }

// Vars implements LogicalPlan.
func (c *CreateRelationship) Vars() []string { return []string{c.RelVar} }

// ─────────────────────────────────────────────────────────────────────────────

// SetProperty sets or updates a single property on a node or relationship.
type SetProperty struct {
	// EntityVar is the already-bound node or relationship variable.
	EntityVar string
	// PropertyKey is the property key to set.
	PropertyKey string
	// Value is the opaque string representation of the new value expression.
	Value string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewSetProperty creates a SetProperty operator.
func NewSetProperty(entityVar, propertyKey, value string, child LogicalPlan) *SetProperty {
	return &SetProperty{
		EntityVar:   entityVar,
		PropertyKey: propertyKey,
		Value:       value,
		Child:       child,
	}
}

// Children implements LogicalPlan.
func (s *SetProperty) Children() []LogicalPlan { return []LogicalPlan{s.Child} }

// Vars implements LogicalPlan. SetProperty does not introduce new variables.
func (s *SetProperty) Vars() []string { return []string{s.EntityVar} }

// ─────────────────────────────────────────────────────────────────────────────

// SetLabels adds one or more labels to an already-bound node.
type SetLabels struct {
	// NodeVar is the already-bound node variable.
	NodeVar string
	// Labels is the list of labels to add.
	Labels []string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewSetLabels creates a SetLabels operator.
func NewSetLabels(nodeVar string, labels []string, child LogicalPlan) *SetLabels {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &SetLabels{NodeVar: nodeVar, Labels: lb, Child: child}
}

// Children implements LogicalPlan.
func (s *SetLabels) Children() []LogicalPlan { return []LogicalPlan{s.Child} }

// Vars implements LogicalPlan.
func (s *SetLabels) Vars() []string { return []string{s.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// RemoveProperty removes a single property from a node or relationship.
type RemoveProperty struct {
	// EntityVar is the already-bound node or relationship variable.
	EntityVar string
	// PropertyKey is the property key to remove.
	PropertyKey string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewRemoveProperty creates a RemoveProperty operator.
func NewRemoveProperty(entityVar, propertyKey string, child LogicalPlan) *RemoveProperty {
	return &RemoveProperty{EntityVar: entityVar, PropertyKey: propertyKey, Child: child}
}

// Children implements LogicalPlan.
func (r *RemoveProperty) Children() []LogicalPlan { return []LogicalPlan{r.Child} }

// Vars implements LogicalPlan.
func (r *RemoveProperty) Vars() []string { return []string{r.EntityVar} }

// ─────────────────────────────────────────────────────────────────────────────

// RemoveLabels removes one or more labels from an already-bound node.
type RemoveLabels struct {
	// NodeVar is the already-bound node variable.
	NodeVar string
	// Labels is the list of labels to remove.
	Labels []string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewRemoveLabels creates a RemoveLabels operator.
func NewRemoveLabels(nodeVar string, labels []string, child LogicalPlan) *RemoveLabels {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &RemoveLabels{NodeVar: nodeVar, Labels: lb, Child: child}
}

// Children implements LogicalPlan.
func (r *RemoveLabels) Children() []LogicalPlan { return []LogicalPlan{r.Child} }

// Vars implements LogicalPlan.
func (r *RemoveLabels) Vars() []string { return []string{r.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// DeleteNode deletes an already-bound node from the graph. The node must have
// no relationships; use DetachDelete to delete a node together with its
// relationships.
type DeleteNode struct {
	// NodeVar is the already-bound node variable to delete.
	NodeVar string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewDeleteNode creates a DeleteNode operator.
func NewDeleteNode(nodeVar string, child LogicalPlan) *DeleteNode {
	return &DeleteNode{NodeVar: nodeVar, Child: child}
}

// Children implements LogicalPlan.
func (d *DeleteNode) Children() []LogicalPlan { return []LogicalPlan{d.Child} }

// Vars implements LogicalPlan.
func (d *DeleteNode) Vars() []string { return []string{d.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// DeleteRelationship deletes an already-bound relationship from the graph.
type DeleteRelationship struct {
	// RelVar is the already-bound relationship variable to delete.
	RelVar string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewDeleteRelationship creates a DeleteRelationship operator.
func NewDeleteRelationship(relVar string, child LogicalPlan) *DeleteRelationship {
	return &DeleteRelationship{RelVar: relVar, Child: child}
}

// Children implements LogicalPlan.
func (d *DeleteRelationship) Children() []LogicalPlan { return []LogicalPlan{d.Child} }

// Vars implements LogicalPlan.
func (d *DeleteRelationship) Vars() []string { return []string{d.RelVar} }

// ─────────────────────────────────────────────────────────────────────────────

// DetachDelete deletes an already-bound node and all its incident relationships.
type DetachDelete struct {
	// NodeVar is the already-bound node variable to delete.
	NodeVar string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewDetachDelete creates a DetachDelete operator.
func NewDetachDelete(nodeVar string, child LogicalPlan) *DetachDelete {
	return &DetachDelete{NodeVar: nodeVar, Child: child}
}

// Children implements LogicalPlan.
func (d *DetachDelete) Children() []LogicalPlan { return []LogicalPlan{d.Child} }

// Vars implements LogicalPlan.
func (d *DetachDelete) Vars() []string { return []string{d.NodeVar} }

// ─────────────────────────────────────────────────────────────────────────────

// Merge implements the MERGE clause: it matches the given pattern or creates it
// if absent. OnCreate and OnMatch hold the update expressions applied under each
// branch (opaque strings; a dedicated expression IR is introduced later).
type Merge struct {
	// Pattern is the opaque string representation of the MERGE pattern.
	Pattern string
	// OnCreate is the list of update expression strings applied when the pattern
	// is created (the ON CREATE SET sub-clause).
	OnCreate []string
	// OnMatch is the list of update expression strings applied when the pattern
	// is matched (the ON MATCH SET sub-clause).
	OnMatch []string
	// BoundVars lists the variable names that are bound by the MERGE pattern and
	// made available downstream.
	BoundVars []string
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewMerge creates a Merge operator.
func NewMerge(pattern string, onCreate, onMatch, boundVars []string, child LogicalPlan) *Merge {
	oc := make([]string, len(onCreate))
	copy(oc, onCreate)
	om := make([]string, len(onMatch))
	copy(om, onMatch)
	bv := make([]string, len(boundVars))
	copy(bv, boundVars)
	return &Merge{
		Pattern:   pattern,
		OnCreate:  oc,
		OnMatch:   om,
		BoundVars: bv,
		Child:     child,
	}
}

// Children implements LogicalPlan.
func (m *Merge) Children() []LogicalPlan { return []LogicalPlan{m.Child} }

// Vars implements LogicalPlan.
func (m *Merge) Vars() []string { return m.BoundVars }

// ─────────────────────────────────────────────────────────────────────────────
// Procedure call operator
// ─────────────────────────────────────────────────────────────────────────────

// ProcedureCall invokes a stored procedure and binds its yield columns. It
// corresponds to CALL procedure(…) YIELD col1, col2 in Cypher.
type ProcedureCall struct {
	// Namespace is the optional namespace path of the procedure
	// (e.g. ["apoc", "algo"] for apoc.algo.dijkstra).
	Namespace []string
	// Name is the bare procedure name.
	Name string
	// Arguments is the list of opaque string representations of the call
	// argument expressions.
	Arguments []string
	// YieldVars is the ordered list of variable names produced by the YIELD
	// clause. An empty slice means YIELD * (all output columns).
	YieldVars []string
	// Child is the driving subplan. May be nil when CALL appears at the start
	// of a query.
	Child LogicalPlan
}

// NewProcedureCall creates a ProcedureCall operator.
func NewProcedureCall(namespace []string, name string, arguments, yieldVars []string, child LogicalPlan) *ProcedureCall {
	ns := make([]string, len(namespace))
	copy(ns, namespace)
	args := make([]string, len(arguments))
	copy(args, arguments)
	yv := make([]string, len(yieldVars))
	copy(yv, yieldVars)
	return &ProcedureCall{
		Namespace: ns,
		Name:      name,
		Arguments: args,
		YieldVars: yv,
		Child:     child,
	}
}

// Children implements LogicalPlan. Returns [Child] when Child is non-nil,
// otherwise nil.
func (p *ProcedureCall) Children() []LogicalPlan {
	if p.Child == nil {
		return nil
	}
	return []LogicalPlan{p.Child}
}

// Vars implements LogicalPlan.
func (p *ProcedureCall) Vars() []string { return p.YieldVars }
