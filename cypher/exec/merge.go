package exec

// merge.go — Merge write operator (task-275).
//
// Merge implements the Cypher MERGE semantics:
//
//  1. Execute the search sub-plan (produced by the IR) to check whether the
//     pattern already exists.
//  2. If at least one row is found → "ON MATCH" branch: apply on-match
//     property mutations to every matched row.
//  3. If no row is found → "ON CREATE" branch: create the node (using the
//     graphMutator), then apply on-create property mutations.
//
// In the current IR, on-create and on-match actions are opaque SET-item
// strings of the form `n.key = "value"`. They are parsed as single-property
// SET operations.
//
// # Single-writer safety
//
// Merge is safe under the single-writer guarantee: no concurrent MERGE
// operations can race on the same graph instance.
//
// # Concurrency
//
// Merge is NOT safe for concurrent use.

import (
	"context"
	"fmt"
	"strings"

	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/index"
)

// ─────────────────────────────────────────────────────────────────────────────
// Merge
// ─────────────────────────────────────────────────────────────────────────────

// MergeSearchFn is a function that executes the search sub-plan for the MERGE
// pattern and returns the matching rows. An empty slice means no match.
type MergeSearchFn func(ctx context.Context) ([]Row, error)

// Merge implements MERGE semantics: match-or-create a pattern.
//
// Merge is NOT safe for concurrent use.
type Merge struct {
	nodeVar         string
	labels          []string
	props           []propLiteral
	onCreateActions []mergeAction
	onMatchActions  []mergeAction
	searchFn        MergeSearchFn
	schema          map[string]int
	child           Operator
	mutator         GraphMutator
	reg             *ConstraintRegistry // nil means no enforcement
	mgr             *index.Manager      // nil when reg is nil
	ctx             context.Context     //nolint:containedctx // stored for per-Next ctx check

	// iteration state, reset on each Init call
	matched    []Row
	matchedIdx int
	created    bool
	createdRow Row
	done       bool
}

// mergeAction is a pre-parsed single-property assignment from an ON
// CREATE/MATCH SET item like `n.name = "Alice"`.
type mergeAction struct {
	nodeVar string
	key     string
	value   string // opaque literal string
}

// NewMerge creates a Merge operator.
//
// nodeVar is the variable bound to the merged node. labels and properties are
// the node-pattern components used when creating a new node. onCreateStrs and
// onMatchStrs are opaque SET-item strings from the IR translator. searchFn
// executes the read side of the match. schema maps variable names to column
// indices. mutator is the graph write surface.
func NewMerge(
	nodeVar string,
	labels []string,
	properties string,
	onCreateStrs, onMatchStrs []string,
	searchFn MergeSearchFn,
	schema map[string]int,
	child Operator,
	mutator GraphMutator,
) (*Merge, error) {
	lb := make([]string, len(labels))
	copy(lb, labels)

	props, err := parsePropLiteral(properties)
	if err != nil {
		return nil, fmt.Errorf("exec: Merge: parse properties %q: %w", properties, err)
	}

	onCreate, err := parseMergeActions(onCreateStrs)
	if err != nil {
		return nil, fmt.Errorf("exec: Merge: parse ON CREATE actions: %w", err)
	}
	onMatch, err := parseMergeActions(onMatchStrs)
	if err != nil {
		return nil, fmt.Errorf("exec: Merge: parse ON MATCH actions: %w", err)
	}

	return &Merge{
		nodeVar:         nodeVar,
		labels:          lb,
		props:           props,
		onCreateActions: onCreate,
		onMatchActions:  onMatch,
		searchFn:        searchFn,
		schema:          schema,
		child:           child,
		mutator:         mutator,
	}, nil
}

// WithConstraints attaches a ConstraintRegistry and index.Manager for
// pre-write enforcement in ON CREATE and ON MATCH actions. Both must be
// non-nil. Returns op for chaining.
func (op *Merge) WithConstraints(reg *ConstraintRegistry, mgr *index.Manager) *Merge {
	op.reg = reg
	op.mgr = mgr
	return op
}

// Init initialises the operator: executes the search plan, buffers matched
// rows (ON MATCH path) or creates a new node (ON CREATE path).
func (op *Merge) Init(ctx context.Context) error {
	op.ctx = ctx
	op.matched = op.matched[:0]
	op.matchedIdx = 0
	op.created = false
	op.createdRow = nil
	op.done = false

	if err := op.child.Init(ctx); err != nil {
		return err
	}

	// Execute the search sub-plan.
	rows, err := op.searchFn(ctx)
	if err != nil {
		return fmt.Errorf("exec: Merge: search: %w", err)
	}

	if len(rows) > 0 {
		// ON MATCH path: apply on-match actions to each matched row.
		for i := range rows {
			if applyErr := op.applyActions(op.onMatchActions, rows[i]); applyErr != nil {
				return fmt.Errorf("exec: Merge: ON MATCH: %w", applyErr)
			}
		}
		op.matched = rows
		return nil
	}

	// ON CREATE path: constraint enforcement before any mutation.
	if op.reg != nil {
		for _, p := range op.props {
			if cerr := op.reg.CheckSetProperty(op.labels, p.key, p.value, op.mgr); cerr != nil {
				return fmt.Errorf("exec: Merge: ON CREATE: %w", cerr)
			}
		}
	}

	// ON CREATE path: create the node.
	nodeKey := op.freshNodeKey()
	nodeID := op.mutator.AddNode(nodeKey)
	for _, lbl := range op.labels {
		op.mutator.SetNodeLabel(nodeKey, lbl)
	}
	for _, p := range op.props {
		op.mutator.SetNodeProperty(nodeKey, p.key, p.value)
		if op.reg != nil {
			op.reg.RecordPropertySet(op.labels, p.key, p.value)
		}
	}

	createdRow := Row{expr.IntegerValue(int64(nodeID))}
	if applyErr := op.applyActions(op.onCreateActions, createdRow); applyErr != nil {
		return fmt.Errorf("exec: Merge: ON CREATE: %w", applyErr)
	}
	op.created = true
	op.createdRow = createdRow
	return nil
}

// Next emits one row: either a matched row (ON MATCH) or the created row
// (ON CREATE), each emitted exactly once.
func (op *Merge) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.done {
		return false, nil
	}

	if op.created {
		op.done = true
		*out = op.createdRow
		return true, nil
	}

	if op.matchedIdx < len(op.matched) {
		*out = op.matched[op.matchedIdx]
		op.matchedIdx++
		return true, nil
	}

	op.done = true
	return false, nil
}

// Close closes the child operator.
func (op *Merge) Close() error {
	return op.child.Close()
}

// freshNodeKey returns a unique node key using the global counter.
func (op *Merge) freshNodeKey() string {
	n := globalNodeCounter.Add(1)
	return "__cx_merge_" + fmt.Sprintf("%x", n)
}

// applyActions applies a slice of mergeAction to a row. The row is expected to
// carry an IntegerValue NodeID at column 0 when op.nodeVar is involved.
func (op *Merge) applyActions(actions []mergeAction, row Row) error {
	for _, a := range actions {
		pv, err := parsePropValue(a.value)
		if err != nil {
			// Non-literal expression: skip.
			continue
		}

		var nodeKey string
		var resolved bool

		// Try to resolve via schema first.
		nodeID, schemaErr := resolveNodeIDFromRow(a.nodeVar, op.schema, row)
		if schemaErr == nil {
			nodeKey, resolved = op.mutator.ResolveNodeLabel(nodeID)
		}

		// Fall back: if the action targets op.nodeVar and the created row has
		// a NodeID at column 0.
		if !resolved && a.nodeVar == op.nodeVar && len(row) > 0 {
			if iv, ok := row[0].(expr.IntegerValue); ok {
				nodeKey, resolved = op.mutator.ResolveNodeLabel(graph.NodeID(iv))
			}
		}

		if resolved {
			// Constraint enforcement for ON MATCH / ON CREATE action.
			if op.reg != nil {
				labels := op.mutator.NodeLabels(nodeKey)
				if cerr := op.reg.CheckSetProperty(labels, a.key, pv, op.mgr); cerr != nil {
					return cerr
				}
			}
			op.mutator.SetNodeProperty(nodeKey, a.key, pv)
			if op.reg != nil {
				labels := op.mutator.NodeLabels(nodeKey)
				op.reg.RecordPropertySet(labels, a.key, pv)
			}
		}
	}
	return nil
}

// parseMergeActions parses a slice of opaque SET-item strings like
// `n.name = "Alice"` into structured mergeAction values.
// Items that do not match the expected pattern are silently skipped.
func parseMergeActions(strs []string) ([]mergeAction, error) {
	out := make([]mergeAction, 0, len(strs))
	for _, s := range strs {
		s = strings.TrimSpace(s)
		eqIdx := strings.Index(s, "=")
		if eqIdx < 0 {
			continue
		}
		lhs := strings.TrimSpace(s[:eqIdx])
		rhs := strings.TrimSpace(s[eqIdx+1:])
		dotIdx := strings.LastIndex(lhs, ".")
		if dotIdx < 0 {
			continue
		}
		varName := strings.TrimSpace(lhs[:dotIdx])
		key := strings.TrimSpace(lhs[dotIdx+1:])
		out = append(out, mergeAction{nodeVar: varName, key: key, value: rhs})
	}
	return out, nil
}
