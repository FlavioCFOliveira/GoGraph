package exec

// cyphermorphism.go — cyphermorphism enforcement for Expand (task-241).
//
// Cypher morphism rules require that, in a path pattern, no two relationship
// bindings may refer to the same relationship (edge). When Expand introduces a
// new relationship bound to edge position e, it must reject any output row
// where e already appears in one of the relationship columns carried by the
// input row.
//
// # Implementation
//
// WithCyphermorphism attaches a relCols slice to an Expand operator. The slice
// identifies which columns of the *input* row already hold edge IDs (as
// expr.IntegerValue). During buildRow, before writing the output, the operator
// checks whether the new edgeID matches any value in those columns. If it does,
// the row is silently skipped and Expand continues to the next edge.
//
// # Zero-alloc contract
//
// The check is an O(len(relCols)) linear scan over a small, stack-pinned slice
// of column indices — no allocation.

// expandOptions carries optional configuration that cannot be expressed via
// ExpandConfig (which is a value type passed at construction).  Options are
// applied after construction with the functional-option pattern.
type expandOptions struct {
	relCols []int // input-row columns that carry existing edge IDs
}

// expandOption is a functional option for an Expand operator.
type expandOption func(*expandOptions)

// WithCyphermorphism returns an option that enables cyphermorphism enforcement.
// relCols lists the column indices in each input row that hold existing edge
// IDs (as [expr.IntegerValue]). When Expand is about to emit a row with a new
// edgeID, it rejects the row if edgeID equals any value already present in
// those columns.
func WithCyphermorphism(relCols []int) expandOption {
	return func(o *expandOptions) {
		o.relCols = relCols
	}
}

// applyOptions applies zero or more functional options to opts.
func applyOptions(opts *expandOptions, options []expandOption) {
	for _, o := range options {
		o(opts)
	}
}

// NewExpandWithOptions creates an Expand operator with optional extra
// configuration applied via functional options (e.g. [WithCyphermorphism]).
//
// All ExpandConfig fields behave identically to [NewExpand]. Options are
// applied after base construction and augment the operator's behaviour without
// altering its public type.
func NewExpandWithOptions(input Operator, fwd, rev csrAdjacency, cfg ExpandConfig, options ...expandOption) *Expand {
	op := NewExpand(input, fwd, rev, cfg)
	var opts expandOptions
	applyOptions(&opts, options)
	op.relCols = opts.relCols
	return op
}
