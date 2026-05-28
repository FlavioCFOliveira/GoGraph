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
	propsRaw        string
	props           []propLiteral
	onCreateActions []mergeAction
	onMatchActions  []mergeAction
	searchFn        MergeSearchFn
	schema          map[string]int
	child           Operator
	mutator         GraphMutator
	reg             *ConstraintRegistry // nil means no enforcement
	mgr             *index.Manager      // nil when reg is nil
	propsEvalFn     PropsEvalFn         // nil when all props are literals
	ctx             context.Context     //nolint:containedctx // stored for per-Next ctx check

	// iteration state, reset on each Init call
	matched    []Row
	matchedIdx int
	created    bool
	createdRow Row
	done       bool
	firedOnce  bool // tracks whether at least one merge cycle has run
}

// mergeAction is a pre-parsed ON CREATE / ON MATCH SET item. Two shapes
// are supported:
//
//   - Property assignment (`n.name = "Alice"`): nodeVar, key, value populated,
//     setLabels nil.
//   - Label set (`SET a:Foo:Bar`): nodeVar populated, setLabels carries the
//     list of label names to add to the node. key and value are empty in
//     this case.
type mergeAction struct {
	nodeVar   string
	key       string
	value     string // opaque literal string; empty for label-set actions
	setLabels []string
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
		propsRaw:        properties,
		props:           props,
		onCreateActions: onCreate,
		onMatchActions:  onMatch,
		searchFn:        searchFn,
		schema:          schema,
		child:           child,
		mutator:         mutator,
	}, nil
}

// WithParams re-parses the property map with the supplied query parameters for
// $name substitution. Returns op for chaining.
func (op *Merge) WithParams(params map[string]expr.Value) (*Merge, error) {
	if len(params) == 0 {
		return op, nil
	}
	props, err := parsePropLiteralWithParams(op.propsRaw, params)
	if err != nil {
		return nil, fmt.Errorf("exec: Merge: parse properties %q: %w", op.propsRaw, err)
	}
	op.props = props
	return op, nil
}

// WithConstraints attaches a ConstraintRegistry and index.Manager for
// pre-write enforcement in ON CREATE and ON MATCH actions. Both must be
// non-nil. Returns op for chaining.
func (op *Merge) WithConstraints(reg *ConstraintRegistry, mgr *index.Manager) *Merge {
	op.reg = reg
	op.mgr = mgr
	return op
}

// WithPropsEvalFn attaches a per-row property evaluator. When fn is non-nil
// the operator re-evaluates the MERGE node-pattern property map against each
// driving row and uses the merged (literal ∪ dynamic) property set both as
// the search predicate and as the ON CREATE node-property writes. Required
// for MERGE patterns whose inline property map contains variable references
// such as `MERGE (p:Person {login: prop.login})` after an UNWIND.
//
// Returns op for chaining.
func (op *Merge) WithPropsEvalFn(fn PropsEvalFn) *Merge {
	op.propsEvalFn = fn
	return op
}

// Init initialises the operator: executes the search plan, then dispatches
// to the ON MATCH or ON CREATE branch depending on whether the search
// returned any rows.
func (op *Merge) Init(ctx context.Context) error {
	op.resetRunState(ctx)
	return op.child.Init(ctx)
}

// runMergeForChild executes one search-or-create cycle of the merge pattern
// against the current graph state and buffers the resulting rows so that
// Next can emit them. Called once per upstream child row so a query like
//
//	MATCH (person:Person) MERGE (city:City) RETURN person, city
//
// observes the merged binding once per driving row (the second person row
// re-finds the city created on the first row, rather than skipping the
// merge entirely).
//
// When propsEvalFn is set the property map is re-evaluated against childRow
// and the merged (literal ∪ dynamic) property set drives both the search
// predicate and the ON CREATE writes — the path that powers row-driven
// MERGE shapes such as `MERGE (p:Person {login: prop.login})`.
func (op *Merge) runMergeForChild(childRow Row) error {
	op.matched = op.matched[:0]
	op.matchedIdx = 0
	op.created = false
	op.createdRow = nil

	propsForRow := op.props
	if op.propsEvalFn != nil {
		propsForRow = mergeProps(op.props, op.propsEvalFn, childRow)
	}

	var rows []Row
	var err error
	if op.propsEvalFn != nil {
		rows, err = searchMergeNodes(op.ctx, op.mutator, op.labels, propsForRow)
	} else {
		rows, err = op.searchFn(op.ctx)
	}
	if err != nil {
		return fmt.Errorf("exec: Merge: search: %w", err)
	}

	if len(rows) > 0 {
		// Combine each matching node with the driving child row so the
		// downstream projection sees both bindings.
		combined := make([]Row, 0, len(rows))
		for _, mr := range rows {
			combined = append(combined, op.combineRows(childRow, mr))
		}
		return op.runOnMatchPath(combined)
	}
	if err := op.runOnCreatePathWithProps(propsForRow); err != nil {
		return err
	}
	op.createdRow = op.combineRows(childRow, op.createdRow)
	return nil
}

// combineRows appends mergeRow's columns to childRow, growing the schema-
// mapped node slot of the merge variable when the child row does not
// already include it.
func (op *Merge) combineRows(childRow, mergeRow Row) Row {
	if len(mergeRow) == 0 {
		return childRow
	}
	mergeCol, ok := op.schema[op.nodeVar]
	if !ok || mergeCol < len(childRow) {
		// No dedicated merge column or it overlaps an existing slot —
		// fall back to a verbatim child-row passthrough; the downstream
		// projection will resolve the merge variable via the schema
		// lookup against whatever column carries the merge node.
		out := make(Row, len(childRow), len(childRow)+len(mergeRow))
		copy(out, childRow)
		return append(out, mergeRow...)
	}
	out := make(Row, mergeCol+1)
	copy(out, childRow)
	out[mergeCol] = mergeRow[0]
	return out
}

// resetRunState clears the per-Init state so the operator can be re-Init'd
// without leaking buffered matches from a previous invocation.
func (op *Merge) resetRunState(ctx context.Context) {
	op.ctx = ctx
	op.matched = op.matched[:0]
	op.matchedIdx = 0
	op.created = false
	op.createdRow = nil
	op.done = false
	op.firedOnce = false
}

// runOnMatchPath applies each ON MATCH action to every row returned by the
// search sub-plan and buffers the rows for emission from Next.
func (op *Merge) runOnMatchPath(rows []Row) error {
	for i := range rows {
		if applyErr := op.applyActions(op.onMatchActions, rows[i]); applyErr != nil {
			return fmt.Errorf("exec: Merge: ON MATCH: %w", applyErr)
		}
	}
	op.matched = rows
	return nil
}

// runOnCreatePath enforces declared constraints, creates the merge node,
// attaches its labels and properties, runs ON CREATE actions, and primes
// the operator to emit the freshly created row.
func (op *Merge) runOnCreatePath() error {
	return op.runOnCreatePathWithProps(op.props)
}

// runOnCreatePathWithProps is the workhorse of [runOnCreatePath]; it accepts
// the resolved property set so that row-aware MERGE (`MERGE (p:Person
// {login: prop.login})`) writes the per-row values rather than the static
// literal-only set.
func (op *Merge) runOnCreatePathWithProps(props []propLiteral) error {
	if op.reg != nil {
		for _, p := range props {
			if cerr := op.reg.CheckSetProperty(op.labels, p.key, p.value, op.mgr); cerr != nil {
				return fmt.Errorf("exec: Merge: ON CREATE: %w", cerr)
			}
		}
	}

	nodeKey := op.freshNodeKey()
	nodeID, err := op.mutator.AddNode(nodeKey)
	if err != nil {
		return fmt.Errorf("exec: Merge: ON CREATE AddNode: %w", err)
	}
	for _, lbl := range op.labels {
		if serr := op.mutator.SetNodeLabel(nodeKey, lbl); serr != nil {
			return fmt.Errorf("exec: Merge: ON CREATE SetNodeLabel: %w", serr)
		}
	}
	for _, p := range props {
		if serr := op.mutator.SetNodeProperty(nodeKey, p.key, p.value); serr != nil {
			return fmt.Errorf("exec: Merge: ON CREATE SetNodeProperty: %w", serr)
		}
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
	for {
		// Emit any rows buffered from the previous child row.
		if op.matchedIdx < len(op.matched) {
			*out = op.matched[op.matchedIdx]
			op.matchedIdx++
			return true, nil
		}
		if op.created {
			op.created = false
			*out = op.createdRow
			return true, nil
		}
		if op.done {
			return false, nil
		}
		// Drained — pull the next child row, run the merge cycle.
		var childRow Row
		ok, err := op.child.Next(&childRow)
		if err != nil {
			return false, err
		}
		if !ok {
			// Child has no more rows. When MERGE is the leading clause
			// (no driving rows at all) the operator still fires once
			// against an empty driving row so a standalone
			// `MERGE (a:Foo)` creates the node — this matches the
			// openCypher single-empty-row semantics that powers the
			// Argument leaf.
			if !op.firedOnce {
				op.firedOnce = true
				op.done = true
				if err := op.runMergeForChild(Row{}); err != nil {
					return false, err
				}
				continue
			}
			op.done = true
			return false, nil
		}
		op.firedOnce = true
		if err := op.runMergeForChild(childRow); err != nil {
			return false, err
		}
	}
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

		if !resolved {
			continue
		}

		// Label-set action (`SET a:Foo:Bar`): add every label to the node.
		if len(a.setLabels) > 0 {
			for _, lbl := range a.setLabels {
				if serr := op.mutator.SetNodeLabel(nodeKey, lbl); serr != nil {
					return fmt.Errorf("exec: Merge: action SetNodeLabel %q: %w", lbl, serr)
				}
			}
			continue
		}

		// Property-set action.
		pv, err := parsePropValue(a.value)
		if err != nil {
			// Non-literal expression: skip.
			continue
		}
		// Constraint enforcement for ON MATCH / ON CREATE action.
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			if cerr := op.reg.CheckSetProperty(labels, a.key, pv, op.mgr); cerr != nil {
				return cerr
			}
		}
		if serr := op.mutator.SetNodeProperty(nodeKey, a.key, pv); serr != nil {
			return fmt.Errorf("exec: Merge: action SetNodeProperty: %w", serr)
		}
		if op.reg != nil {
			labels := op.mutator.NodeLabels(nodeKey)
			op.reg.RecordPropertySet(labels, a.key, pv)
		}
	}
	return nil
}

// parseMergeActions parses a slice of opaque SET-item strings into structured
// mergeAction values. Two surface shapes are recognised:
//
//   - `var.key = value`            → property assignment
//   - `var:Label1:Label2…`         → label-set on the node
//
// Items that do not match either pattern are silently skipped.
func parseMergeActions(strs []string) ([]mergeAction, error) {
	out := make([]mergeAction, 0, len(strs))
	for _, s := range strs {
		s = strings.TrimSpace(s)
		if eqIdx := strings.Index(s, "="); eqIdx >= 0 {
			lhs := strings.TrimSpace(s[:eqIdx])
			rhs := strings.TrimSpace(s[eqIdx+1:])
			dotIdx := strings.LastIndex(lhs, ".")
			if dotIdx < 0 {
				continue
			}
			varName := strings.TrimSpace(lhs[:dotIdx])
			key := strings.TrimSpace(lhs[dotIdx+1:])
			out = append(out, mergeAction{nodeVar: varName, key: key, value: rhs})
			continue
		}
		// Label-set form: identifier followed by one or more `:Label` parts.
		if colonIdx := strings.Index(s, ":"); colonIdx > 0 {
			varName := strings.TrimSpace(s[:colonIdx])
			rest := s[colonIdx+1:]
			parts := strings.Split(rest, ":")
			labels := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					labels = append(labels, p)
				}
			}
			if varName != "" && len(labels) > 0 {
				out = append(out, mergeAction{nodeVar: varName, setLabels: labels})
			}
		}
	}
	return out, nil
}
