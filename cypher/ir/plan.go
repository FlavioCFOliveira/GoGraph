package ir

import (
	"sync/atomic"

	"gograph/cypher/ast"
)

// argTagSeq generates monotonic [Argument] tags. Tags are stable for the
// lifetime of an Argument node and let physical builders thread the matching
// [exec.Argument] instance from an enclosing Apply-family operator down to
// the inner-side leaf.
var argTagSeq atomic.Uint32

// nextArgTag returns a fresh tag distinct from all previously issued tags
// across the lifetime of the process.
func nextArgTag() uint32 {
	return argTagSeq.Add(1)
}

// NextArgTag is the exported wrapper around [nextArgTag]. It is used by the
// outer cypher package's subquery evaluator (cypher/subquery_eval.go) when it
// needs to mint a fresh Argument tag while compiling an EXISTS / COUNT
// subquery's inner plan outside the regular IR-translator path.
func NextArgTag() uint32 { return nextArgTag() }

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
	// ArgumentExpr is the parsed AST for the aggregate argument, when available.
	// Non-nil when the argument is a non-trivial expression (property access,
	// function call, …); when nil, callers fall back to schema lookup keyed on
	// Argument or supply the constant Null. count(*) keeps ArgumentExpr == nil.
	ArgumentExpr ast.Expression
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
	// Expr is the original AST expression for the sort key. It is carried
	// alongside Expression so the physical builder can compile an expression
	// evaluator for keys that are not directly present in the output schema
	// (e.g. ORDER BY n.age after RETURN n — n.age is not a projected column
	// but it can be derived by evaluating the expression against the row).
	// May be nil for sort items constructed without access to the AST.
	Expr ast.Expression
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
	// Tag uniquely identifies this Argument node so the physical builder can
	// route the matching [exec.Argument] instance from an enclosing
	// [CorrelatedApply] or [OptionalApply]. Tags are assigned at construction
	// time and are stable for the lifetime of the node.
	Tag uint32
}

// NewArgument creates an Argument operator with the given injected variables
// and a freshly allocated [Argument.Tag].
func NewArgument(vars []string) *Argument {
	cp := make([]string, len(vars))
	copy(cp, vars)
	return &Argument{Variables: cp, Tag: nextArgTag()}
}

// NewArgumentWithTag creates an Argument operator with the given variables and
// an explicit tag. It is used by IR-building helpers that need the inner-side
// Argument to share a tag with an enclosing Apply-family operator.
func NewArgumentWithTag(vars []string, tag uint32) *Argument {
	cp := make([]string, len(vars))
	copy(cp, vars)
	return &Argument{Variables: cp, Tag: tag}
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
	// PathVar, when non-empty, is the named path variable (`p` in
	// `MATCH p=(a)-[*1..3]->(b)`). The physical builder allocates a schema slot
	// for it and emits a PathValue in that column.
	PathVar string
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
	// GroupByExprs holds the parsed AST expression for each grouping key, in
	// the same order as GroupBy. Entries may be nil for legacy callers that
	// only supplied string keys; non-nil entries enable property-access and
	// complex expression evaluation at execution time.
	GroupByExprs []ast.Expression
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

// NewEagerAggregationWithExprs creates an EagerAggregation operator with parsed
// AST expressions for the grouping keys. groupBy and groupByExprs must have the
// same length; entries in groupByExprs may be nil (the executor falls back to a
// schema lookup keyed on the corresponding groupBy string in that case).
func NewEagerAggregationWithExprs(
	groupBy []string,
	groupByExprs []ast.Expression,
	aggregates []AggregateExpr,
	child LogicalPlan,
) *EagerAggregation {
	gb := make([]string, len(groupBy))
	copy(gb, groupBy)
	exprs := make([]ast.Expression, len(groupByExprs))
	copy(exprs, groupByExprs)
	agg := make([]AggregateExpr, len(aggregates))
	copy(agg, aggregates)
	return &EagerAggregation{GroupBy: gb, GroupByExprs: exprs, Aggregates: agg, Child: child}
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

// CorrelatedApply is a dependent join in which the inner subplan starts with
// an [Argument] leaf re-emitting the outer row. Unlike [Apply], the physical
// executor for CorrelatedApply forwards the inner row verbatim and does not
// concatenate it with the outer row — the outer columns are already present
// in the inner row's leading positions.
//
// CorrelatedApply is used for multi-pattern MATCH where subsequent patterns
// share at least one variable with a previously bound pattern: the shared
// variable acts as a join key implicit in the dataflow.
type CorrelatedApply struct {
	// Outer is the driving (left) subplan.
	Outer LogicalPlan
	// Inner is the correlated (right) subplan; its leftmost leaf must be an
	// [Argument] whose Tag equals [CorrelatedApply.ArgTag].
	Inner LogicalPlan
	// ArgTag is the tag shared with the inner-side Argument leaf so that the
	// physical builder can route the matching exec.Argument instance.
	ArgTag uint32
}

// NewCorrelatedApply creates a CorrelatedApply operator with a freshly issued
// [Argument] tag. The caller is responsible for placing an [Argument] node
// carrying the same tag at the leftmost leaf of inner.
func NewCorrelatedApply(outer, inner LogicalPlan) *CorrelatedApply {
	return &CorrelatedApply{Outer: outer, Inner: inner, ArgTag: nextArgTag()}
}

// NewCorrelatedApplyWithTag creates a CorrelatedApply with an explicit tag.
// Use when constructing the IR top-down and threading the tag into the inner
// subplan's Argument leaf at the same time.
func NewCorrelatedApplyWithTag(outer, inner LogicalPlan, tag uint32) *CorrelatedApply {
	return &CorrelatedApply{Outer: outer, Inner: inner, ArgTag: tag}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (c *CorrelatedApply) Children() []LogicalPlan { return []LogicalPlan{c.Outer, c.Inner} }

// Vars implements LogicalPlan. The outer variables come first; inner variables
// not already present are appended.
func (c *CorrelatedApply) Vars() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, v := range c.Outer.Vars() {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range c.Inner.Vars() {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────

// OptionalApply is the left-outer variant of [CorrelatedApply]. When the inner
// subplan produces zero rows for an outer row, the physical executor emits a
// single NULL-extended row consisting of the outer columns followed by NULL
// placeholders for every column the inner pipeline would have introduced.
//
// OptionalApply is used for OPTIONAL MATCH when a non-empty driving subplan
// already provides the outer bindings (e.g. a preceding MATCH clause).
type OptionalApply struct {
	// Outer is the driving (left) subplan.
	Outer LogicalPlan
	// Inner is the optional correlated subplan.
	Inner LogicalPlan
	// ArgTag is the tag shared with the inner-side Argument leaf.
	ArgTag uint32
}

// NewOptionalApply creates an OptionalApply operator with a freshly issued
// [Argument] tag.
func NewOptionalApply(outer, inner LogicalPlan) *OptionalApply {
	return &OptionalApply{Outer: outer, Inner: inner, ArgTag: nextArgTag()}
}

// NewOptionalApplyWithTag creates an OptionalApply with an explicit tag.
func NewOptionalApplyWithTag(outer, inner LogicalPlan, tag uint32) *OptionalApply {
	return &OptionalApply{Outer: outer, Inner: inner, ArgTag: tag}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (o *OptionalApply) Children() []LogicalPlan { return []LogicalPlan{o.Outer, o.Inner} }

// Vars implements LogicalPlan. The outer variables come first; inner variables
// not already present are appended.
func (o *OptionalApply) Vars() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, v := range o.Outer.Vars() {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range o.Inner.Vars() {
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
	// Inner is the correlated existence-check subplan; its leftmost leaf must
	// be an [Argument] whose Tag equals [SemiApply.ArgTag].
	Inner LogicalPlan
	// ArgTag is the tag shared with the inner-side Argument leaf so that the
	// physical builder can route the matching exec.Argument instance.
	ArgTag uint32
}

// NewSemiApply creates a SemiApply operator with a freshly issued [Argument]
// tag. The caller is responsible for placing an [Argument] node carrying the
// same tag at the leftmost leaf of inner.
func NewSemiApply(outer, inner LogicalPlan) *SemiApply {
	return &SemiApply{Outer: outer, Inner: inner, ArgTag: nextArgTag()}
}

// NewSemiApplyWithTag creates a SemiApply with an explicit tag. Use when
// constructing the IR top-down and threading the tag into the inner subplan's
// Argument leaf at the same time.
func NewSemiApplyWithTag(outer, inner LogicalPlan, tag uint32) *SemiApply {
	return &SemiApply{Outer: outer, Inner: inner, ArgTag: tag}
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
	// Inner is the correlated non-existence-check subplan; its leftmost leaf
	// must be an [Argument] whose Tag equals [AntiSemiApply.ArgTag].
	Inner LogicalPlan
	// ArgTag is the tag shared with the inner-side Argument leaf so that the
	// physical builder can route the matching exec.Argument instance.
	ArgTag uint32
}

// NewAntiSemiApply creates an AntiSemiApply operator with a freshly issued
// [Argument] tag.
func NewAntiSemiApply(outer, inner LogicalPlan) *AntiSemiApply {
	return &AntiSemiApply{Outer: outer, Inner: inner, ArgTag: nextArgTag()}
}

// NewAntiSemiApplyWithTag creates an AntiSemiApply with an explicit tag.
func NewAntiSemiApplyWithTag(outer, inner LogicalPlan, tag uint32) *AntiSemiApply {
	return &AntiSemiApply{Outer: outer, Inner: inner, ArgTag: tag}
}

// Children implements LogicalPlan. Returns [Outer, Inner].
func (a *AntiSemiApply) Children() []LogicalPlan { return []LogicalPlan{a.Outer, a.Inner} }

// Vars implements LogicalPlan. Only outer variables are visible downstream.
func (a *AntiSemiApply) Vars() []string { return a.Outer.Vars() }

// ─────────────────────────────────────────────────────────────────────────────

// SubqueryExists is a self-contained IR container for an EXISTS { … } subquery
// that appears inside an arbitrary expression (e.g. nested in a BinaryOp, a
// CASE branch, or a RETURN projection item). The node holds the inner logical
// plan plus the variables the subquery correlates from its lexical outer
// scope; the expression evaluator drives the inner plan per outer row at
// evaluation time and yields a BoolValue per openCypher semantics:
//
//   - true when the inner plan produces at least one row for the seeded
//     correlation bindings;
//   - false when the inner plan produces zero rows.
//
// Unlike [SemiApply], which is a top-level plan operator that filters its
// outer pipeline by row-count, SubqueryExists is an expression-embedded form
// used wherever an EXISTS { … } occurs as a sub-expression. The two paths are
// complementary: the IR translator emits [SemiApply] whenever an EXISTS is the
// entire WHERE predicate (so the existence check can short-circuit at the
// plan level), and emits SubqueryExists for every other occurrence.
//
// SubqueryExists is a logical-plan node only by virtue of holding an Inner
// plan tree; it is not itself wired into the operator pipeline. The physical
// builder reaches it through the expression evaluator, not through
// [buildOperator].
type SubqueryExists struct {
	// Inner is the subplan whose row-count drives the existence check. Its
	// leftmost leaf must be an [Argument] whose Tag equals
	// [SubqueryExists.ArgTag].
	Inner LogicalPlan
	// CorrelationVars is the snapshot of outer-scope variable names that the
	// inner plan may reference at evaluation time. The expression evaluator
	// projects the outer row onto these names before seeding the Argument.
	CorrelationVars []string
	// ArgTag is the tag shared with the inner-side Argument leaf so the
	// physical builder routes the matching exec.Argument instance per outer
	// row.
	ArgTag uint32
}

// NewSubqueryExists creates a SubqueryExists node with a freshly issued
// [Argument] tag. The caller is responsible for placing an [Argument] node
// carrying the same tag at the leftmost leaf of inner.
func NewSubqueryExists(inner LogicalPlan, correlationVars []string) *SubqueryExists {
	cp := make([]string, len(correlationVars))
	copy(cp, correlationVars)
	return &SubqueryExists{Inner: inner, CorrelationVars: cp, ArgTag: nextArgTag()}
}

// NewSubqueryExistsWithTag creates a SubqueryExists with an explicit tag.
func NewSubqueryExistsWithTag(inner LogicalPlan, correlationVars []string, tag uint32) *SubqueryExists {
	cp := make([]string, len(correlationVars))
	copy(cp, correlationVars)
	return &SubqueryExists{Inner: inner, CorrelationVars: cp, ArgTag: tag}
}

// Children implements LogicalPlan. Returns [Inner].
func (s *SubqueryExists) Children() []LogicalPlan { return []LogicalPlan{s.Inner} }

// Vars implements LogicalPlan. SubqueryExists itself introduces no new
// variables in the outer scope: it yields a boolean value at evaluation time.
func (s *SubqueryExists) Vars() []string { return nil }

// ─────────────────────────────────────────────────────────────────────────────

// SubqueryCount is the COUNT { … } counterpart of [SubqueryExists]. The
// expression evaluator drives the inner plan per outer row and yields an
// IntegerValue equal to the exact number of rows the inner plan produced for
// the seeded correlation bindings (0 when the inner plan is empty).
//
// SubqueryCount is a logical-plan node only by virtue of holding an Inner
// plan tree; it is not itself wired into the operator pipeline.
type SubqueryCount struct {
	// Inner is the subplan whose row-count is reported by the count.
	Inner LogicalPlan
	// CorrelationVars is the snapshot of outer-scope variable names visible
	// inside the subquery.
	CorrelationVars []string
	// ArgTag is the tag shared with the inner-side Argument leaf.
	ArgTag uint32
}

// NewSubqueryCount creates a SubqueryCount node with a freshly issued
// [Argument] tag.
func NewSubqueryCount(inner LogicalPlan, correlationVars []string) *SubqueryCount {
	cp := make([]string, len(correlationVars))
	copy(cp, correlationVars)
	return &SubqueryCount{Inner: inner, CorrelationVars: cp, ArgTag: nextArgTag()}
}

// NewSubqueryCountWithTag creates a SubqueryCount with an explicit tag.
func NewSubqueryCountWithTag(inner LogicalPlan, correlationVars []string, tag uint32) *SubqueryCount {
	cp := make([]string, len(correlationVars))
	copy(cp, correlationVars)
	return &SubqueryCount{Inner: inner, CorrelationVars: cp, ArgTag: tag}
}

// Children implements LogicalPlan. Returns [Inner].
func (s *SubqueryCount) Children() []LogicalPlan { return []LogicalPlan{s.Inner} }

// Vars implements LogicalPlan. SubqueryCount itself introduces no new
// variables in the outer scope: it yields an integer value at evaluation time.
func (s *SubqueryCount) Vars() []string { return nil }

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
	// ArgTag identifies the [Argument] leaf that anchors the Inner
	// subplan. The physical builder pre-allocates the matching
	// exec.Argument under this tag so RollUpApply's loop can seed it
	// with each outer row before re-initialising Inner.
	ArgTag uint32
}

// NewRollUpApply creates a RollUpApply operator. The Inner subplan's
// Argument leaf must carry the same ArgTag so the build pipeline can
// route the exec.Argument instance.
func NewRollUpApply(outer, inner LogicalPlan, collectVar string) *RollUpApply {
	return &RollUpApply{Outer: outer, Inner: inner, CollectVar: collectVar, ArgTag: nextArgTag()}
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
	// PropertiesExpr is the parsed AST for the property map, when available.
	// Non-nil when the property map contains non-literal expressions (variable
	// references, property accesses, arithmetic) that must be evaluated at
	// runtime against the current row. The physical builder uses PropertiesExpr
	// when non-nil to construct a per-row evaluation closure; nil means all
	// values were literals and Properties (the string form) is sufficient.
	PropertiesExpr ast.Expression
	// Child is the driving subplan.
	Child LogicalPlan
}

// NewCreateNode creates a CreateNode operator.
func NewCreateNode(nodeVar string, labels []string, properties string, child LogicalPlan) *CreateNode {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &CreateNode{NodeVar: nodeVar, Labels: lb, Properties: properties, Child: child}
}

// NewCreateNodeExpr creates a CreateNode operator with a parsed AST for the
// property map. Use when the property map contains non-literal expressions that
// require runtime evaluation.
func NewCreateNodeExpr(nodeVar string, labels []string, properties string, propertiesExpr ast.Expression, child LogicalPlan) *CreateNode {
	lb := make([]string, len(labels))
	copy(lb, labels)
	return &CreateNode{NodeVar: nodeVar, Labels: lb, Properties: properties, PropertiesExpr: propertiesExpr, Child: child}
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
	// PropertiesExpr is the parsed AST for the property map, when available.
	// Non-nil when the property map contains non-literal expressions that must
	// be evaluated at runtime against the current row. See CreateNode.PropertiesExpr.
	PropertiesExpr ast.Expression
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

// NewCreateRelationshipExpr creates a CreateRelationship operator with a parsed
// AST for the property map.
func NewCreateRelationshipExpr(startVar, endVar, relVar, relType, properties string, propertiesExpr ast.Expression, child LogicalPlan) *CreateRelationship {
	return &CreateRelationship{
		StartVar:       startVar,
		EndVar:         endVar,
		RelVar:         relVar,
		RelType:        relType,
		Properties:     properties,
		PropertiesExpr: propertiesExpr,
		Child:          child,
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
