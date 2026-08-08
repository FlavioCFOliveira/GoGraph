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
// # Concurrency
//
// Merge is NOT safe for concurrent use: one operator tree is driven by one
// goroutine.
//
// It used to claim more — that a "single-writer guarantee" stopped two MERGE
// statements racing on the same graph. That guarantee was the engine's writer
// mutex and the store's capacity-one semaphore, and rmp #2306 retired both.
//
// The search-then-create sequence is therefore NOT atomic against another writer:
// two concurrent MERGE statements on the same pattern can both find no match and
// both create, because two CREATEs of two distinct new nodes are not a
// write-write conflict and MVCC has nothing to arbitrate. Measured at eight
// duplicates from eight writers
// (cypher.TestConcurrentMerge_WithoutAConstraintMayCreateDuplicates).
//
// That is the documented behaviour rather than a defect, and it matches Neo4j,
// which requires a uniqueness constraint for the same structural reason. With a
// UNIQUE constraint on the merged property the reservation is atomic and the
// duplicates collapse to one
// (cypher.TestConcurrentMerge_AUniqueConstraintCollapsesTheDuplicates).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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
	child       Operator
	mutator     GraphMutator
	ctx         context.Context //nolint:containedctx // stored for per-Next ctx check
	searchFn    MergeSearchFn
	schema      map[string]int
	reg         *ConstraintRegistry // nil means no enforcement
	mgr         *index.Manager      // nil when reg is nil
	propsEvalFn PropsEvalFn         // nil when all props are literals
	// labelSrc narrows the row-aware merge search to a label posting list
	// instead of every interned node (#2217). nil falls back to the full walk.
	// It is consulted only by the propsEvalFn path; the literal-props path
	// carries its own source inside searchFn.
	labelSrc MergeLabelSource
	// onCreateEvals / onMatchEvals map an action's target (via
	// [MergeActionEvalKey]) to a per-row RHS evaluator for a non-literal
	// ON CREATE / ON MATCH SET expression (e.g. `SET n.num = n.num + 1`).
	// nil when every action's RHS is a literal. See [Merge.applyActions].
	onCreateEvals map[string]ValueEvalFn
	onMatchEvals  map[string]ValueEvalFn

	nodeVar  string
	propsRaw string

	labels          []string
	props           []propLiteral
	onCreateActions []mergeAction
	onMatchActions  []mergeAction
	// onCreateSetAll / onMatchSetAll carry whole-entity ON CREATE / ON MATCH
	// SET actions (`SET n = <expr>` / `SET n += <expr>`) evaluated per row.
	// The per-property [parseMergeActions] path drops such keyless actions, so
	// they are applied separately via [applyWholeEntityValueToNode] (#2031).
	onCreateSetAll []MergeSetAllAction
	onMatchSetAll  []MergeSetAllAction
	// iteration state, reset on each Init call
	matched    []Row
	createdRow Row

	matchedIdx int

	created   bool
	done      bool
	firedOnce bool // tracks whether at least one merge cycle has run
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
	props, err := parsePropLiteralWithParamsMerge(op.propsRaw, params)
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

// WithActionEvals attaches per-row RHS evaluators for ON CREATE / ON MATCH
// property-set items whose right-hand side is a non-literal expression
// (keyed by [MergeActionEvalKey]). Without these, a self-referential
// assignment such as `ON MATCH SET n.num = n.num + 1` fails to parse as a
// literal and is silently dropped (#1965). Returns op for chaining.
func (op *Merge) WithActionEvals(onCreate, onMatch map[string]ValueEvalFn) *Merge {
	op.onCreateEvals = onCreate
	op.onMatchEvals = onMatch
	return op
}

// WithSetAllActions attaches whole-entity ON CREATE / ON MATCH SET actions
// (`SET n = <expr>` / `SET n += <expr>`), which the per-property action path
// cannot represent. Each is evaluated per row and applied via
// [applyWholeEntityValueToNode] (#2031). Returns op for chaining.
func (op *Merge) WithSetAllActions(onCreate, onMatch []MergeSetAllAction) *Merge {
	op.onCreateSetAll = onCreate
	op.onMatchSetAll = onMatch
	return op
}

// applySetAllActions applies each whole-entity SET action to the node it names,
// resolved from row exactly as [Merge.applyActions] resolves a property action.
func (op *Merge) applySetAllActions(actions []MergeSetAllAction, row Row) error {
	for _, a := range actions {
		nodeKey, ok := op.resolveActionNodeKey(a.TargetVar, row)
		if !ok {
			continue
		}
		v, err := a.Eval(row)
		if err != nil {
			return err
		}
		if err := applyWholeEntityValueToNode(op.mutator, op.reg, op.mgr, a.TargetVar, nodeKey, a.IsReplace, v); err != nil {
			return err
		}
	}
	return nil
}

// resolveActionNodeKey resolves the node key an ON CREATE / ON MATCH action
// targets: via the schema column first, then falling back to column 0 when the
// action names the merge variable and that column carries the node id.
func (op *Merge) resolveActionNodeKey(targetVar string, row Row) (string, bool) {
	if id, err := resolveNodeIDFromRow(targetVar, op.schema, row); err == nil {
		if nodeKey, ok := op.mutator.ResolveNodeLabel(id); ok {
			return nodeKey, true
		}
	}
	if targetVar == op.nodeVar && len(row) > 0 {
		if iv, ok := row[0].(expr.IntegerValue); ok {
			if nodeKey, ok := op.mutator.ResolveNodeLabel(graph.NodeID(iv)); ok {
				return nodeKey, true
			}
		}
	}
	return "", false
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

// WithLabelSource attaches the label posting-list source that narrows the
// row-aware merge search to the nodes carrying a pattern label, instead of
// every interned node (#2217). It is the access path that makes the
// UNWIND-MERGE bulk-ingest idiom scale with the label's population rather than
// with the size of the whole graph.
//
// src may be nil, in which case the search keeps the full walk. Returns op for
// chaining.
func (op *Merge) WithLabelSource(src MergeLabelSource) *Merge {
	op.labelSrc = src
	return op
}

// Init initialises the operator: executes the search plan, then dispatches
// to the ON MATCH or ON CREATE branch depending on whether the search
// returned any rows.
//
// The first Merge.Init (or [CreateNode.Init]) in the process also seeds
// [globalNodeCounter] past the largest synthetic key already interned in
// op.mutator, so that the keys minted by [Merge.freshNodeKey] in this
// process cannot collide with __cx_merge_<hex> keys persisted by an earlier
// process and replayed during WAL / snapshot recovery. Without this a
// one-process-per-command consumer mints __cx_merge_1 on every command and
// the second MERGE silently overwrites the first node. The seed is gated by
// [globalNodeCounterSeededOnce] so the O(N) scan runs at most once per
// process regardless of how many CreateNode / Merge operators are built.
func (op *Merge) Init(ctx context.Context) error {
	op.resetRunState(ctx)
	globalNodeCounterSeededOnce.Do(func() {
		seedGlobalNodeCounter(op.mutator)
	})
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
		var mErr error
		propsForRow, mErr = mergeProps(op.props, op.propsEvalFn, childRow)
		if mErr != nil {
			return mErr
		}
	}

	var rows []Row
	var err error
	if op.propsEvalFn != nil {
		rows, err = searchMergeNodes(op.ctx, op.mutator, op.labelSrc, op.labels, propsForRow)
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
	return op.runOnCreatePathWithProps(childRow, propsForRow)
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
		if applyErr := op.applyActions(op.onMatchActions, op.onMatchEvals, rows[i]); applyErr != nil {
			return fmt.Errorf("exec: Merge: ON MATCH: %w", applyErr)
		}
		if applyErr := op.applySetAllActions(op.onMatchSetAll, rows[i]); applyErr != nil {
			return fmt.Errorf("exec: Merge: ON MATCH: %w", applyErr)
		}
	}
	op.matched = rows
	return nil
}

// runOnCreatePathWithProps enforces declared constraints, creates the merge
// node, attaches its labels and properties, runs ON CREATE actions, and primes
// the operator to emit the freshly created row. It accepts the resolved
// property set so that row-aware MERGE (`MERGE (p:Person {login: prop.login})`)
// writes the per-row values rather than the static literal-only set.
//
// The created node is combined with childRow BEFORE the ON CREATE actions run
// so their per-row RHS evaluators see a schema-consistent row — the merge
// variable at its schema column and every driving-clause binding at its own —
// exactly as the ON MATCH path already does. Without this an expression
// action such as `ON CREATE SET n.x = other.y` could not resolve `other`.
func (op *Merge) runOnCreatePathWithProps(childRow Row, props []propLiteral) error {
	if op.reg != nil {
		for _, p := range props {
			if cerr := reserveConstraintValue(op.reg, op.mutator, op.labels, p.key, p.value, op.mgr); cerr != nil {
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
	}

	createdRow := op.combineRows(childRow, Row{expr.IntegerValue(int64(nodeID))})
	if applyErr := op.applyActions(op.onCreateActions, op.onCreateEvals, createdRow); applyErr != nil {
		return fmt.Errorf("exec: Merge: ON CREATE: %w", applyErr)
	}
	if applyErr := op.applySetAllActions(op.onCreateSetAll, createdRow); applyErr != nil {
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

// freshNodeKey returns a unique node key drawn from the process-global
// counter. The key is never visible to Cypher callers; only the NodeID is
// emitted into the row. The "__cx_merge_<hex>" form (synthKeyPrefix +
// mergeKeyInfix + hex) is parsed by [parseSynthKeySuffix] so [Merge.Init] /
// [CreateNode.Init] seed the shared counter past it on recovery.
func (op *Merge) freshNodeKey() string {
	n := globalNodeCounter.Add(1)
	return synthKeyPrefix + mergeKeyInfix + fmt.Sprintf("%x", n)
}

// applyActions applies a slice of mergeAction to a row. The row is expected to
// carry an IntegerValue NodeID at column 0 when op.nodeVar is involved.
//
// evals maps an action's target (via [MergeActionEvalKey]) to a per-row
// evaluator for a non-literal RHS expression; it is the ON CREATE or ON MATCH
// evaluator set depending on which branch is running. When a property-set
// action's RHS is not a literal, the evaluator computes its value against row
// so `ON MATCH SET n.num = n.num + 1` reads the node's current value instead
// of being silently dropped (#1965).
func (op *Merge) applyActions(actions []mergeAction, evals map[string]ValueEvalFn, row Row) error {
	for _, a := range actions {
		var nodeKey string
		var nodeID graph.NodeID
		var resolved bool

		// Try to resolve via schema first.
		id, schemaErr := resolveNodeIDFromRow(a.nodeVar, op.schema, row)
		if schemaErr == nil {
			if nodeKey, resolved = op.mutator.ResolveNodeLabel(id); resolved {
				nodeID = id
			}
		}

		// Fall back: if the action targets op.nodeVar and the created row has
		// a NodeID at column 0.
		if !resolved && a.nodeVar == op.nodeVar && len(row) > 0 {
			if iv, ok := row[0].(expr.IntegerValue); ok {
				if nodeKey, resolved = op.mutator.ResolveNodeLabel(graph.NodeID(iv)); resolved {
					nodeID = graph.NodeID(iv)
				}
			}
		}

		if !resolved {
			continue
		}

		// Label-set action (`SET a:Foo:Bar`): add every label to the node.
		if len(a.setLabels) > 0 {
			// Attaching a label puts the node under every UNIQUE constraint
			// declared on that label, so reserve before writing — see
			// cypher/exec/label_constraints.go. This is a SECOND label-write site
			// besides the SetLabels operator, and enforcing at only one of them
			// leaves the identical duplicate committing through MERGE (rmp #2352).
			enforceLabels := op.reg != nil && op.reg.HasAnyUnique()
			var rd nodeStateReader
			if enforceLabels {
				rd = nodeStateReaderFor(op.mutator)
			}
			for _, lbl := range a.setLabels {
				if enforceLabels {
					if cerr := reserveLabelUnique(op.reg, op.mutator, op.mgr, rd, nodeKey, lbl); cerr != nil {
						return cerr
					}
				}
				if serr := op.mutator.SetNodeLabel(nodeKey, lbl); serr != nil {
					return fmt.Errorf("exec: Merge: action SetNodeLabel %q: %w", lbl, serr)
				}
			}
			continue
		}

		// Property-set action. Resolve the value: literal fast path first,
		// then the per-row expression evaluator for a non-literal RHS.
		pv, ok, remove, err := op.resolveActionValue(a, evals, row, nodeID)
		if err != nil {
			return err
		}
		if remove {
			// The RHS evaluated to null → openCypher removes the property.
			if op.reg != nil {
				if oldVal, had := op.mutator.NodeProperties(nodeKey)[a.key]; had {
					releaseConstraintValue(op.reg, op.mutator, labelsInTx(op.mutator, nodeKey), a.key, oldVal)
				}
			}
			op.mutator.DelNodeProperty(nodeKey, a.key)
			continue
		}
		if !ok {
			// Literal null (preserve prior skip behaviour), a non-literal RHS
			// with no evaluator, or an evaluator no-op (eval error / unstorable
			// type — matching regular SET). No write.
			continue
		}
		// Constraint enforcement for ON MATCH / ON CREATE action.
		if op.reg != nil {
			labels := labelsInTx(op.mutator, nodeKey)
			// Release this node's own old constrained value BEFORE the check and
			// the overwrite. Without this the replaced value leaks as a permanent
			// phantom reservation (no live node holds it yet it is blocked
			// forever, #1904), and an idempotent MERGE self-set is rejected as its
			// own duplicate. A UNIQUE constraint guarantees at most one holder, so
			// releasing first cannot mask a real cross-node duplicate; a failed
			// check aborts the transaction and rebuilds every value-set.
			if oldVal, had := op.mutator.NodeProperties(nodeKey)[a.key]; had {
				releaseConstraintValue(op.reg, op.mutator, labels, a.key, oldVal)
			}
			if cerr := reserveConstraintValue(op.reg, op.mutator, labels, a.key, pv, op.mgr); cerr != nil {
				return cerr
			}
		}
		if serr := op.mutator.SetNodeProperty(nodeKey, a.key, pv); serr != nil {
			return fmt.Errorf("exec: Merge: action SetNodeProperty: %w", serr)
		}
	}
	return nil
}

// resolveActionValue resolves the value for a MERGE property-set action.
//
// It returns (value, ok, remove, err):
//   - a literal RHS returns (pv, true, false, nil) — the fast path;
//   - a literal null RHS returns (_, false, false, nil) — preserving the prior
//     skip behaviour of the node-only Merge operator;
//   - a non-literal RHS with a registered evaluator returns the evaluated
//     value (pv, true, false, nil), a removal request (_, false, true, nil)
//     when it evaluates to null, or a no-op (_, false, false, nil) when the
//     evaluator reports no value (eval error / unstorable type — matching how
//     regular SET degrades), or a hard error for an unstorable-map RHS;
//   - a non-literal RHS with no evaluator returns (_, false, false, nil) — the
//     prior skip behaviour (defensive: every non-literal action is wired with
//     an evaluator by the physical builder).
func (op *Merge) resolveActionValue(a mergeAction, evals map[string]ValueEvalFn, row Row, nodeID graph.NodeID) (pv lpg.PropertyValue, ok, remove bool, err error) {
	lit, perr := parsePropValue(a.value)
	switch {
	case perr == nil:
		return lit, true, false, nil
	case isNullPropertyValueErr(perr):
		return lpg.PropertyValue{}, false, false, nil
	case errors.Is(perr, ErrNestedPropertyValue):
		// A nested collection is a hard InvalidPropertyType error, not a
		// deferrable non-literal RHS: fail-stop rather than drop the action (F3).
		return lpg.PropertyValue{}, false, false, perr
	default:
		fn, has := evals[MergeActionEvalKey(a.nodeVar, a.key)]
		if !has {
			return lpg.PropertyValue{}, false, false, nil
		}
		evalRow := op.actionEvalRow(row, a.nodeVar, nodeID)
		val, isNull, hasValue, evalErr := fn(evalRow)
		if evalErr != nil {
			return lpg.PropertyValue{}, false, false, evalErr
		}
		if isNull {
			return lpg.PropertyValue{}, false, true, nil
		}
		if !hasValue {
			return lpg.PropertyValue{}, false, false, nil
		}
		return val, true, false, nil
	}
}

// actionEvalRow returns a row for a per-row RHS evaluator: a copy of row with
// targetVar's NodeID pinned at its schema column so `targetVar.<key>` resolves
// to the matched/created node's current stored value regardless of how the
// caller laid out the row. Other columns are preserved so cross-variable
// references (`SET n.x = other.y`) still resolve. When targetVar has no schema
// column the row is returned unchanged (best effort).
func (op *Merge) actionEvalRow(row Row, targetVar string, nodeID graph.NodeID) Row {
	col, ok := op.schema[targetVar]
	if !ok {
		return row
	}
	width := len(row)
	if col+1 > width {
		width = col + 1
	}
	out := make(Row, width)
	copy(out, row)
	out[col] = expr.IntegerValue(int64(nodeID))
	return out
}

// MergeActionEvalKey composes the map key under which a MERGE ON CREATE /
// ON MATCH property-set action's per-row RHS evaluator is registered and
// looked up. targetVar is the entity variable the action writes (a node, a
// bound endpoint, or a relationship variable) and key is the property key.
// The NUL separator cannot appear in a Cypher identifier, so the composed key
// is unambiguous across distinct (variable, property) pairs. The physical
// builder ([cypher] package) and the operators here must agree on this
// encoding, so it is defined once and exported.
func MergeActionEvalKey(targetVar, key string) string {
	return targetVar + "\x00" + key
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
