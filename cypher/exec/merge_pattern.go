package exec

// merge_pattern.go — compound MERGE pattern: a chain of one or more
// relationship hops where at least one node in the chain is NOT already
// bound by an earlier clause, so the whole pattern (every fresh node plus
// every relationship) must be searched for as one atomic unit and, when no
// joint match exists, created as one atomic unit.
//
// This complements [Merge] (a lone node, no relationship) and
// [MergeRelationship] (a single relationship hop whose endpoints are BOTH
// already bound — the narrower, more efficient fast path). MergePattern is
// the general case: 2026-07-02 production-readiness audit finding (round 2)
// "MERGE compound-pattern silently drops data" — `MERGE (a:L1{p:1})-[:R]->
// (b:L2{p:2})` used to create only the leading node and silently discard
// the relationship and the second node, with no error.
//
// # Algorithm
//
// Per driving row: walk the chain left to right. Position 0's candidate set
// is either {its bound value} (if bound) or every node satisfying its
// label/property predicate (a full scan, via [searchMergeNodes] — if fresh).
// Each subsequent position's candidate set is computed by expanding every
// current partial binding's frontier node along the connecting hop
// (OutNeighbours / InNeighbours, filtered by the hop's relationship type and
// property predicate) and then filtering by the next position's own
// predicate — an exact-value filter when bound, a label/property filter when
// fresh. This is a left-deep nested-loop join anchored at position 0,
// producing zero or more complete joint bindings.
//
//   - Zero bindings: the whole pattern must be created. Every fresh node is
//     created (in chain order, so a later hop can reference an earlier
//     fresh node's freshly-minted key) and every hop's relationship is
//     created connecting consecutive positions. Never reuse an existing
//     node for part of a pattern that failed to match as a whole — this is
//     openCypher's documented, intentional MERGE semantics, not a defect to
//     avoid (Neo4j Cypher Manual, MERGE section).
//   - One or more bindings: emit one output row per binding (mirroring how
//     a MATCH of the same pattern would fan out), running ON MATCH SET
//     against each.
//
// ON CREATE / ON MATCH SET actions may target ANY variable in the pattern —
// a fresh node, a bound node, or a hop's relationship variable — dispatched
// by the pre-parsed [mergeAction.nodeVar] against the chain's variable names.
//
// # Scope
//
// Handles a chain of any length (one or more hops); each hop's relationship
// pattern must declare at most one type (multi-type / typeless / variable-
// length relationships cannot be created and are rejected upstream, matching
// [MergeRelationship]'s and CREATE's existing constraints). The IR translator
// (mergeClause in cypher/ir/writes.go) is responsible for rejecting any
// pattern shape this operator does not support with a clear compile-time
// error rather than routing it here — MergePattern assumes its input is
// already a well-formed chain.
//
// # Parallel-relationship multiplicity
//
// [binding] carries one NodeID per chain position — it has no relationship
// identity — so when two or more parallel relationships satisfy a hop's
// type/property predicate between the same resolved node pair,
// [MergePattern.expandCandidates] fans out one binding per parallel
// relationship rather than a single candidate, so MERGE's match multiplicity
// equals the equivalent MATCH's (rmp #1875). The per-hop multiplicity comes
// from [MergePattern.hopMultiplicity], which counts the graph's Cypher
// CREATE-multiplicity for the pair filtered by the hop's type/property
// predicate — the same parallel-edge population a MATCH enumerates. The bound
// bindings for a hop are identical NodeID pairs (the join carries no per-edge
// identity), which is sufficient for the multiplicity-sensitive aggregations
// (`count(r)`); it does not create a duplicate node or relationship and does
// not affect the match-vs-create decision. [MergeRelationship] fans out its
// own single both-bound hop via the same EdgeCreateCount counter.
//
// # Concurrency
//
// MergePattern is NOT safe for concurrent use: one operator tree is driven by one
// goroutine. Its search-then-create sequence is NOT race-free against other
// writers — nothing serialises them since rmp #2306 — exactly as for
// [MergeRelationship]. See [Merge] for the measured behaviour and the
// uniqueness-constraint remedy.
//
// # Atomicity
//
// MergePattern performs no transaction/undo-log bookkeeping of its own — it
// only calls plain [GraphMutator] methods, each of which records its own
// inverse on the caller's undo log. Atomicity across the whole created
// pattern (all nodes and relationships appearing together, or not at all)
// is provided entirely by the standard commit/rollback barrier
// (Engine.RunInTx / ApplyAtomically), exactly as for [MergeRelationship] and
// [CreateNode]/[CreateRelationship].

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergePatternNode describes one node position in the chain.
type mergePatternNode struct {
	propsEvalFn PropsEvalFn // nil when every property is a literal
	varName     string
	propsRaw    string
	labels      []string
	props       []propLiteral // static (literal) props, parsed once from propsRaw
	col         int           // valid when bound: input-row column holding the NodeID
	outCol      int           // output-row column for this position's binding; -1 if none
	bound       bool
	parsed      bool
}

// mergePatternHop describes one relationship hop connecting chain positions
// i and i+1. Nodes always stay in pattern-declaration order, so an incoming
// hop `(a)<-[:T]-(b)` — position i is `a`, position i+1 is `b`, but the edge
// is stored/searched from b to a — sets reversed=true rather than swapping
// the chain positions themselves; swapping would break a later hop's
// reference to "the node just mentioned".
type mergePatternHop struct {
	// relPropsEvalFn evaluates this hop's inline relationship property map
	// against the driving row when it carries a non-literal value (e.g.
	// `(a)-[:R {kind: row.pk}]->(b)`). nil when every value is a literal
	// ($param references are resolved at build time into relProps via
	// [MergePattern.WithParams]). The merged (literal ∪ dynamic) set drives
	// BOTH the existing-edge search predicate and the created edge's
	// properties — without it a row-driven inline property is silently dropped
	// (stored as null on the created edge).
	relPropsEvalFn PropsEvalFn
	relVar         string
	relType        string
	relPropsRaw    string
	relProps       []propLiteral
	relCol         int // -1 if anonymous
	parsed         bool
	// relPropsRefsPatternNode is true when this hop's inline property map
	// references an earlier same-pattern node variable (e.g.
	// `(a)-[:R {k: a.id}]->(b)`). Such a hop cannot use the once-per-driving-row
	// property precomputation (which is evaluated before any position is bound);
	// it is re-evaluated per binding against a row widened with the bound nodes,
	// on both the search and create paths (#2024). Left false for the common
	// case, which keeps the precompute fast path intact.
	relPropsRefsPatternNode bool
	undirected              bool
	reversed                bool // true for `<-`: the edge runs position i+1 → position i
}

// storageOrder returns the (src, dst) chain-position indices in EDGE-STORAGE
// order for the hop connecting chain positions i and i+1 — (i, i+1) normally,
// swapped when the hop is reversed (an incoming `<-` pattern).
func (h *mergePatternHop) storageOrder(i int) (srcIdx, dstIdx int) {
	if h.reversed {
		return i + 1, i
	}
	return i, i + 1
}

// directions reports which storage direction(s) satisfy this hop when
// walking from a chain position's key: forward only for a plain hop,
// reverse only when reversed, or both when undirected. Shared by
// [MergePattern.expandCandidates] (neighbour walk) and
// [MergePattern.hopMultiplicity] (parallel-edge count) so the two stay in
// lockstep.
func (h *mergePatternHop) directions() (checkForward, checkReverse bool) {
	return h.undirected || !h.reversed, h.undirected || h.reversed
}

// MergePattern matches-or-creates a chain of one or more relationship hops
// in which at least one node is not already bound. See the package doc
// above for the full algorithm.
type MergePattern struct {
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check

	// onCreateEvals / onMatchEvals map an action's target (via
	// [MergeActionEvalKey]) to a per-row RHS evaluator for a non-literal
	// ON CREATE / ON MATCH SET expression targeting any chain node or hop
	// relationship variable (e.g. `ON MATCH SET n.num = n.num + 1`). nil
	// when every action's RHS is a literal. See [MergePattern.applyActions].
	onCreateEvals map[string]ValueEvalFn
	onMatchEvals  map[string]ValueEvalFn

	reg *ConstraintRegistry // nil means no enforcement
	mgr *index.Manager      // nil when reg is nil

	// labelSrc narrows the anchor-node search to a label posting list instead
	// of every interned node (#2217). nil falls back to the full walk.
	labelSrc MergeLabelSource

	nodes           []mergePatternNode
	hops            []mergePatternHop // len(hops) == len(nodes)-1
	onCreateActions []mergeAction
	onMatchActions  []mergeAction
	// onCreateSetAll / onMatchSetAll carry whole-entity ON CREATE / ON MATCH
	// SET actions (`SET n = <expr>` / `SET n += <expr>`) on a chain node,
	// which the per-property [parseMergeActions] path drops (#2031).
	onCreateSetAll []MergeSetAllAction
	onMatchSetAll  []MergeSetAllAction
	// iteration state, reset per child row
	matched    []Row
	createdRow Row
	// hopPropsForRow holds each hop's effective inline relationship property set
	// (literals merged with any non-literal per-row values) computed ONCE per
	// driving row in [MergePattern.runForRow]. Both the search predicate
	// ([MergePattern.expandCandidates]) and the create write
	// ([MergePattern.createChain]) read the same entry, so a non-deterministic
	// value (e.g. `{t: timestamp()}`) is evaluated once — the created edge
	// stores exactly the value the search matched on — and a non-literal map is
	// not re-evaluated once per frontier binding. Indexed by hop position; len
	// == len(hops) after runForRow computes it.
	hopPropsForRow [][]propLiteral

	matchedIdx int

	created   bool
	done      bool
	firedOnce bool
}

// NewMergePattern creates an empty MergePattern; call AddBoundNode/
// AddFreshNode once per chain position (in order) and AddHop once per
// relationship (len(hops) == positions added - 1), then WithActions.
func NewMergePattern(child Operator, mutator GraphMutator) *MergePattern {
	return &MergePattern{child: child, mutator: mutator}
}

// AddBoundNode appends a chain position whose value is already bound by an
// earlier clause, read from input-row column col.
func (op *MergePattern) AddBoundNode(varName string, col int) *MergePattern {
	op.nodes = append(op.nodes, mergePatternNode{varName: varName, bound: true, col: col, outCol: -1})
	return op
}

// AddFreshNode appends a chain position this MERGE clause introduces fresh.
// outCol is the output-row column that receives the matched-or-created
// NodeID as an expr.IntegerValue; pass -1 if the variable is never
// referenced downstream (still searched/created, just not projected).
func (op *MergePattern) AddFreshNode(varName string, labels []string, propsRaw string, outCol int) *MergePattern {
	op.nodes = append(op.nodes, mergePatternNode{varName: varName, bound: false, labels: labels, propsRaw: propsRaw, outCol: outCol})
	return op
}

// WithLabelSource attaches the label posting-list source that narrows the
// anchor-node search to the nodes carrying a pattern label, instead of every
// interned node (#2217). src may be nil, which keeps the full walk. Returns op
// for chaining.
func (op *MergePattern) WithLabelSource(src MergeLabelSource) *MergePattern {
	op.labelSrc = src
	return op
}

// WithNodePropsEvalFn attaches a per-row property evaluator to the
// most-recently-added fresh node (mirroring [CreateNode.WithPropsEvalFn|
// CreateNode.WithPropsEvalFn]). Used when that node's property map contains
// non-literal expressions (parameters, variable references) that
// [parsePropLiteral] cannot resolve at plan-construction time; the dynamic
// entries are merged with the map's literal entries at both search and
// create time (see [mergeProps]), taking precedence on key collision. Without
// this, a node position's parameterised properties would be silently
// dropped from both the search predicate and the created node — the same
// defect class this operator exists to eliminate.
func (op *MergePattern) WithNodePropsEvalFn(fn PropsEvalFn) *MergePattern {
	if len(op.nodes) == 0 || fn == nil {
		return op
	}
	op.nodes[len(op.nodes)-1].propsEvalFn = fn
	return op
}

// effectiveNodeProps returns n's property predicate for childRow: its
// static literal entries merged with any per-row dynamic entries. Returns
// n.props unchanged (no allocation) when n has no evaluator, matching
// [mergeProps]'s zero-cost fast path for the common all-literal case.
func (op *MergePattern) effectiveNodeProps(n *mergePatternNode, childRow Row) ([]propLiteral, error) {
	return mergeProps(n.props, n.propsEvalFn, childRow)
}

// WithHopPropsEvalFn attaches a per-row property evaluator to the
// most-recently-added hop (mirroring [MergePattern.WithNodePropsEvalFn]). Used
// when that hop's relationship property map contains a non-literal expression
// (a variable reference, property access, or arithmetic — e.g.
// `(a)-[:R {kind: row.pk}]->(b)`) that [parsePropLiteral] cannot resolve at
// plan-construction time. The dynamic entries are merged with the map's literal
// entries at both search and create time (see [MergePattern.effectiveHopProps]),
// taking precedence on key collision. Without this, a hop's row-driven
// relationship property would be silently dropped from both the search
// predicate and the created edge — the same defect class this operator exists
// to eliminate, previously rejected at build time on the compound-pattern path.
func (op *MergePattern) WithHopPropsEvalFn(fn PropsEvalFn) *MergePattern {
	if len(op.hops) == 0 || fn == nil {
		return op
	}
	op.hops[len(op.hops)-1].relPropsEvalFn = fn
	return op
}

// MarkHopRefsPatternNode flags the most-recently-added hop as having an inline
// relationship property map that references an earlier same-pattern node, so
// its properties are evaluated per binding rather than via the once-per-row
// precomputation (#2024). Returns op for chaining.
func (op *MergePattern) MarkHopRefsPatternNode() *MergePattern {
	if len(op.hops) > 0 {
		op.hops[len(op.hops)-1].relPropsRefsPatternNode = true
	}
	return op
}

// effectiveHopProps returns h's relationship property predicate for childRow:
// its static literal entries merged with any per-row dynamic entries. Returns
// h.relProps unchanged (no allocation) when h has no evaluator, matching
// [mergeProps]'s zero-cost fast path for the common all-literal case.
func (op *MergePattern) effectiveHopProps(h *mergePatternHop, childRow Row) ([]propLiteral, error) {
	return mergeProps(h.relProps, h.relPropsEvalFn, childRow)
}

// AddHop appends the relationship connecting the two most-recently-added
// chain positions. relCol is the output-row column for a named relationship
// variable, or -1 for an anonymous one. reversed is true for an incoming
// `<-` pattern, where the edge is stored from the next chain position back
// to the current one.
func (op *MergePattern) AddHop(relVar string, relCol int, relType, relPropsRaw string, undirected, reversed bool) *MergePattern {
	op.hops = append(op.hops, mergePatternHop{relVar: relVar, relCol: relCol, relType: relType, relPropsRaw: relPropsRaw, undirected: undirected, reversed: reversed})
	return op
}

// WithParams attaches query parameters for $name substitution in every
// node's and every hop's inline property map, re-parsing each non-empty raw
// map with parameter references resolved to concrete literal values.
// Mirrors [CreateNode.WithParams]: resolving parameters once here, at build
// time, is cheaper than a per-row [PropsEvalFn] and is correct because
// parameter values are constant for the whole query execution. Must be
// called after every AddBoundNode/AddFreshNode/AddHop call it should cover.
func (op *MergePattern) WithParams(params map[string]expr.Value) (*MergePattern, error) {
	if len(params) == 0 {
		return op, nil
	}
	for i := range op.nodes {
		n := &op.nodes[i]
		if n.propsRaw == "" {
			continue
		}
		props, err := parsePropLiteralWithParamsMerge(n.propsRaw, params)
		if err != nil {
			return nil, fmt.Errorf("exec: MergePattern: parse node %q properties %q: %w", n.varName, n.propsRaw, err)
		}
		n.props = props
		n.parsed = true
	}
	for i := range op.hops {
		h := &op.hops[i]
		if h.relPropsRaw == "" {
			continue
		}
		props, err := parsePropLiteralWithParamsMerge(h.relPropsRaw, params)
		if err != nil {
			return nil, fmt.Errorf("exec: MergePattern: parse relationship properties %q: %w", h.relPropsRaw, err)
		}
		h.relProps = props
		h.parsed = true
	}
	return op, nil
}

// WithActions parses and attaches ON CREATE / ON MATCH SET items (the same
// opaque-string representation [Merge] uses). Each item may target any
// variable in the chain (a fresh node, a bound node, or a hop's relationship
// variable) — dispatched at apply time by the parsed action's target name.
func (op *MergePattern) WithActions(onCreateStrs, onMatchStrs []string) (*MergePattern, error) {
	oc, err := parseMergeActions(onCreateStrs)
	if err != nil {
		return nil, fmt.Errorf("exec: MergePattern: parse ON CREATE actions: %w", err)
	}
	om, err := parseMergeActions(onMatchStrs)
	if err != nil {
		return nil, fmt.Errorf("exec: MergePattern: parse ON MATCH actions: %w", err)
	}
	op.onCreateActions = oc
	op.onMatchActions = om
	return op, nil
}

// WithActionEvals attaches per-row RHS evaluators for ON CREATE / ON MATCH
// property-set items whose right-hand side is a non-literal expression
// (keyed by [MergeActionEvalKey] on the target variable and property key).
// Without these, an expression such as `ON MATCH SET n.num = n.num + 1`
// fails to parse as a literal and is dropped (#1965). Returns op for chaining.
func (op *MergePattern) WithActionEvals(onCreate, onMatch map[string]ValueEvalFn) *MergePattern {
	op.onCreateEvals = onCreate
	op.onMatchEvals = onMatch
	return op
}

// WithSetAllActions attaches whole-entity ON CREATE / ON MATCH SET actions
// (`SET n = <expr>` / `SET n += <expr>`) on a chain node variable, which the
// per-property action path cannot represent. Each is evaluated per row and
// applied via [applyWholeEntityValueToNode] (#2031). Returns op for chaining.
func (op *MergePattern) WithSetAllActions(onCreate, onMatch []MergeSetAllAction) *MergePattern {
	op.onCreateSetAll = onCreate
	op.onMatchSetAll = onMatch
	return op
}

// applySetAllActions applies each whole-entity SET action to the chain node it
// names (resolved via b). An action targeting a relationship variable is left
// to the relationship machinery and skipped here (#2031 covers node targets).
func (op *MergePattern) applySetAllActions(b binding, evalRow Row, actions []MergeSetAllAction) error {
	for _, a := range actions {
		idx, ok := op.nodeIndexByVar(a.TargetVar)
		if !ok {
			continue
		}
		nodeKey, ok := op.mutator.ResolveNodeLabel(b[idx])
		if !ok {
			continue
		}
		v, err := a.Eval(evalRow)
		if err != nil {
			return err
		}
		if err := applyWholeEntityValueToNode(op.mutator, a.TargetVar, nodeKey, a.IsReplace, v); err != nil {
			return err
		}
	}
	return nil
}

// WithConstraints attaches a ConstraintRegistry and index.Manager for
// pre-write enforcement of every fresh node's created properties and of
// every ON CREATE / ON MATCH node property action — mirroring
// [Merge.WithConstraints] and [CreateNode.WithConstraints]. Both must be
// non-nil. Returns op for chaining.
func (op *MergePattern) WithConstraints(reg *ConstraintRegistry, mgr *index.Manager) *MergePattern {
	op.reg = reg
	op.mgr = mgr
	return op
}

// Init initialises the operator and its child. The first MergePattern.Init
// (or [CreateNode.Init] / [Merge.Init]) in the process also seeds
// [globalNodeCounter], exactly as those operators do, so fresh-node keys
// minted here cannot collide with __cx_merge_<hex> keys replayed from an
// earlier process during WAL / snapshot recovery.
func (op *MergePattern) Init(ctx context.Context) error {
	op.ctx = ctx
	op.matched = nil
	op.matchedIdx = 0
	op.created = false
	op.done = false
	op.firedOnce = false
	globalNodeCounterSeededOnce.Do(func() {
		seedGlobalNodeCounter(op.mutator)
	})
	return op.child.Init(ctx)
}

// Close closes the child operator.
func (op *MergePattern) Close() error { return op.child.Close() }

// Next drives one search-or-create cycle per input row, buffering multiple
// matches (Merge5-style multiplicity, generalised: a joint pattern can have
// more than one satisfying binding) so each is emitted as its own row.
// Mirrors [Merge.Next]'s buffering shape, including firing exactly once
// against an empty driving row when MergePattern is the leading clause
// (no preceding MATCH/WITH at all).
func (op *MergePattern) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	for {
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
		var childRow Row
		ok, err := op.child.Next(&childRow)
		if err != nil {
			return false, err
		}
		if !ok {
			if !op.firedOnce {
				op.firedOnce = true
				op.done = true
				if err := op.runForRow(Row{}); err != nil {
					return false, err
				}
				continue
			}
			op.done = true
			return false, nil
		}
		op.firedOnce = true
		if err := op.runForRow(childRow); err != nil {
			return false, err
		}
	}
}

// runForRow executes one search-or-create cycle against childRow, buffering
// the resulting row(s) into op.matched or op.createdRow for Next to drain.
func (op *MergePattern) runForRow(childRow Row) error {
	op.matched = op.matched[:0]
	op.matchedIdx = 0
	op.created = false
	op.createdRow = nil

	if err := op.parseLazy(); err != nil {
		return err
	}

	// Evaluate every hop's effective inline relationship property set ONCE for
	// this driving row, so the search predicate and the create write share a
	// single evaluation (deterministic, and no per-frontier re-evaluation).
	if cap(op.hopPropsForRow) < len(op.hops) {
		op.hopPropsForRow = make([][]propLiteral, len(op.hops))
	} else {
		op.hopPropsForRow = op.hopPropsForRow[:len(op.hops)]
	}
	for i := range op.hops {
		if op.hops[i].relPropsRefsPatternNode {
			// This hop's inline properties reference a fresh same-pattern node,
			// which is not bound until search/create binds it; evaluating here
			// (before any binding) would read null. It is evaluated per binding
			// on the search and create paths instead (#2024).
			op.hopPropsForRow[i] = nil
			continue
		}
		props, hpErr := op.effectiveHopProps(&op.hops[i], childRow)
		if hpErr != nil {
			return hpErr
		}
		op.hopPropsForRow[i] = props
	}

	bindings, err := op.search(childRow)
	if err != nil {
		return fmt.Errorf("exec: MergePattern: search: %w", err)
	}
	if len(bindings) > 0 {
		for _, b := range bindings {
			row, err := op.emitRow(childRow, b)
			if err != nil {
				return err
			}
			if err := op.applyActions(b, row, op.onMatchActions, op.onMatchEvals); err != nil {
				return err
			}
			if err := op.applySetAllActions(b, row, op.onMatchSetAll); err != nil {
				return err
			}
			op.matched = append(op.matched, row)
		}
		return nil
	}
	b, err := op.createChain(childRow)
	if err != nil {
		return fmt.Errorf("exec: MergePattern: create: %w", err)
	}
	row, err := op.emitRow(childRow, b)
	if err != nil {
		return err
	}
	if err := op.applyActions(b, row, op.onCreateActions, op.onCreateEvals); err != nil {
		return err
	}
	if err := op.applySetAllActions(b, row, op.onCreateSetAll); err != nil {
		return err
	}
	op.created = true
	op.createdRow = row
	return nil
}

// parseLazy parses every node's inline property map and every hop's inline
// relationship property map exactly once (cached on first call).
func (op *MergePattern) parseLazy() error {
	for i := range op.nodes {
		n := &op.nodes[i]
		if n.parsed {
			continue
		}
		if n.propsRaw != "" {
			props, err := parsePropLiteral(n.propsRaw)
			if err != nil {
				return fmt.Errorf("parse node %q properties %q: %w", n.varName, n.propsRaw, err)
			}
			n.props = props
		}
		n.parsed = true
	}
	for i := range op.hops {
		h := &op.hops[i]
		if h.parsed {
			continue
		}
		if h.relPropsRaw != "" {
			props, err := parsePropLiteral(h.relPropsRaw)
			if err != nil {
				return fmt.Errorf("parse relationship properties %q: %w", h.relPropsRaw, err)
			}
			h.relProps = props
		}
		h.parsed = true
	}
	return nil
}

// binding is one complete joint solution: one NodeID per chain position.
type binding []graph.NodeID

// search returns every complete joint binding of the chain against the live
// graph, honouring childRow's bound-column values. Returns an empty slice
// (not an error) when no joint solution exists.
func (op *MergePattern) search(childRow Row) ([]binding, error) {
	first := &op.nodes[0]
	var frontier []binding
	if first.bound {
		id, _, ok, err := op.resolveBound(childRow, first)
		if err != nil {
			return nil, err
		}
		if !ok {
			// A null-bound leading position (e.g. from an OPTIONAL MATCH
			// that found nothing) cannot search or create — openCypher
			// treats a MERGE pattern referencing a null-bound entity as a
			// runtime error rather than a silent no-op, since it can
			// neither be matched against nor safely created.
			return nil, fmt.Errorf("exec: MergePattern: bound variable %q is null", first.varName)
		}
		frontier = []binding{{id}}
	} else {
		firstProps, epErr := op.effectiveNodeProps(first, childRow)
		if epErr != nil {
			return nil, epErr
		}
		rows, err := searchMergeNodes(op.ctx, op.mutator, op.labelSrc, first.labels, firstProps)
		if err != nil {
			return nil, err
		}
		frontier = make([]binding, 0, len(rows))
		for _, r := range rows {
			id, ok := nodeIDFromValue(r[0])
			if !ok {
				continue
			}
			frontier = append(frontier, binding{id})
		}
	}

	for hopIdx := range op.hops {
		if len(frontier) == 0 {
			return nil, nil
		}
		hop := &op.hops[hopIdx]
		target := &op.nodes[hopIdx+1]
		next := make([]binding, 0, len(frontier))
		for _, b := range frontier {
			fromID := b[len(b)-1]
			fromKey, ok := op.mutator.ResolveNodeLabel(fromID)
			if !ok {
				continue
			}
			// Widen the row with the nodes bound so far so the target's inline
			// properties can reference an earlier same-pattern node (#2024).
			evalRow := op.bindingEvalRow(childRow, b, len(b))
			hopProps := op.hopPropsForRow[hopIdx]
			if op.hops[hopIdx].relPropsRefsPatternNode {
				// The hop's inline properties reference an earlier same-pattern
				// node; evaluate them against this binding rather than the
				// once-per-row precomputation (which cannot see the binding).
				hp, hpErr := op.effectiveHopProps(&op.hops[hopIdx], evalRow)
				if hpErr != nil {
					return nil, hpErr
				}
				hopProps = hp
			}
			cands, err := op.expandCandidates(fromKey, hop, target, evalRow, hopProps)
			if err != nil {
				return nil, err
			}
			for _, c := range cands {
				extended := make(binding, len(b), len(b)+1)
				copy(extended, b)
				next = append(next, append(extended, c))
			}
		}
		frontier = next
	}
	return frontier, nil
}

// expandCandidates returns the NodeIDs reachable from fromKey via hop that
// also satisfy target's predicate (an exact-value filter when target is
// bound, a label/property filter when fresh).
func (op *MergePattern) expandCandidates(fromKey string, hop *mergePatternHop, target *mergePatternNode, childRow Row, hopProps []propLiteral) ([]graph.NodeID, error) {
	// hopProps is the hop's effective inline relationship property predicate for
	// THIS driving row (literals merged with any non-literal per-row values,
	// e.g. `{kind: row.pk}`), pre-computed once per row by runForRow, so the
	// search matches on the evaluated value exactly as the created edge will
	// carry it.
	if target.bound {
		// The target node is fixed to its bound value, but the hop may be
		// satisfied by more than one PARALLEL relationship between the pair.
		// Fan out one candidate per pre-existing parallel relationship that
		// satisfies the hop's type/property predicate so MERGE's match
		// multiplicity equals the equivalent MATCH's (#1875) — instead of the
		// single boolean edge-exists candidate that under-counted `count(r)`.
		id, key, ok, err := op.resolveBound(childRow, target)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("exec: MergePattern: bound variable %q is null", target.varName)
		}
		mult := op.hopMultiplicity(fromKey, key, hop, hopProps)
		if mult == 0 {
			return nil, nil
		}
		out := make([]graph.NodeID, mult)
		for i := range out {
			out[i] = id
		}
		return out, nil
	}

	targetProps, tpErr := op.effectiveNodeProps(target, childRow)
	if tpErr != nil {
		return nil, tpErr
	}
	var out []graph.NodeID
	seen := map[string]struct{}{}
	tryNeighbour := func(candKey string, edgeSrc, edgeDst string) {
		if _, dup := seen[candKey]; dup {
			return
		}
		if hop.relType != "" {
			if !edgeHasLabel(op.mutator, edgeSrc, edgeDst, hop.relType) {
				return
			}
		}
		if len(hopProps) > 0 && !nodeMatchesAllProperties(hopProps, op.mutator.EdgeProperties(edgeSrc, edgeDst)) {
			return
		}
		if !nodeMatchesAllLabels(target.labels, op.mutator.NodeLabels(candKey)) {
			return
		}
		if !nodeMatchesAllProperties(targetProps, op.mutator.NodeProperties(candKey)) {
			return
		}
		id, ok := op.mutator.ResolveNodeID(candKey)
		if !ok {
			return
		}
		seen[candKey] = struct{}{}
		// Fan out one candidate per pre-existing parallel relationship to this
		// neighbour that satisfies the hop, matching MATCH multiplicity (#1875).
		// hopMultiplicity returns 1 for a single edge, so a non-parallel graph
		// is unaffected.
		mult := op.hopMultiplicity(fromKey, candKey, hop, hopProps)
		for i := 0; i < mult; i++ {
			out = append(out, id)
		}
	}

	checkForward, checkReverse := hop.directions()
	if checkForward {
		for _, n := range op.mutator.OutNeighbours(fromKey) {
			tryNeighbour(n, fromKey, n)
		}
	}
	if checkReverse {
		for _, n := range op.mutator.InNeighbours(fromKey) {
			tryNeighbour(n, n, fromKey)
		}
	}
	return out, nil
}

// edgeSatisfiesHop reports whether the directed edge (src, dst) satisfies
// hop's type and the per-row inline property predicate hopProps.
func edgeSatisfiesHop(mutator GraphMutator, src, dst string, hop *mergePatternHop, hopProps []propLiteral) bool {
	if !mutator.HasEdge(src, dst) {
		return false
	}
	if hop.relType != "" && !edgeHasLabel(mutator, src, dst, hop.relType) {
		return false
	}
	if len(hopProps) > 0 && !nodeMatchesAllProperties(hopProps, mutator.EdgeProperties(src, dst)) {
		return false
	}
	return true
}

// edgeHasLabel reports whether the directed edge (src, dst) carries label
// among its (possibly multigraph-unioned) relationship types.
func edgeHasLabel(mutator GraphMutator, src, dst, label string) bool {
	for _, l := range mutator.EdgeLabels(src, dst) {
		if l == label {
			return true
		}
	}
	return false
}

// hopMultiplicity reports how many pre-existing parallel relationships between
// the resolved (fromKey, toKey) node pair satisfy hop's type and property
// predicate, in the hop's permitted direction(s). It is the per-hop analogue of
// the parallel-edge fan-out a MATCH of the same pattern produces (#1875): a
// bound MERGE hop over two parallel :T edges must contribute two joint bindings
// so `RETURN count(r)` equals the MATCH count. Returns 0 when the pair is not
// connected by any matching edge (the caller then treats the hop as unmatched).
func (op *MergePattern) hopMultiplicity(fromKey, toKey string, hop *mergePatternHop, hopProps []propLiteral) int {
	checkForward, checkReverse := hop.directions()
	var n int
	if checkForward {
		n += op.countMatchingInstances(fromKey, toKey, hop, hopProps)
	}
	if checkReverse {
		n += op.countMatchingInstances(toKey, fromKey, hop, hopProps)
	}
	return n
}

// countMatchingInstances counts the parallel relationship instances stored in
// the directed (src → dst) slot that satisfy hop's type and property predicate.
// The count is the graph's Cypher CREATE-multiplicity for the pair
// (EdgeCreateCount) filtered by the per-instance type/property metadata the
// write path records (SetEdgeLabelAt / SetEdgePropertyAt), so it equals the
// number of parallel edges a MATCH would enumerate. When the pair carries no
// CREATE-multiplicity metadata (an edge added directly through the Go API,
// bypassing IncEdgeCreateCount) it falls back to the boolean edgeSatisfiesHop
// result — one instance — preserving the pre-fix behaviour for that path.
func (op *MergePattern) countMatchingInstances(src, dst string, hop *mergePatternHop, hopProps []propLiteral) int {
	if !edgeSatisfiesHop(op.mutator, src, dst, hop, hopProps) {
		return 0
	}
	mult := op.mutator.EdgeCreateCount(src, dst)
	if mult <= 0 {
		return 1
	}
	var n int
	for idx := int64(1); idx <= mult; idx++ {
		if op.instanceMatchesHop(src, dst, idx, hop, hopProps) {
			n++
		}
	}
	if n == 0 {
		// edgeSatisfiesHop already proved a matching edge exists between the
		// pair, but no per-instance record matched (a storage path that records
		// the type only on the per-pair union): contribute one so the matching
		// pair is never dropped.
		return 1
	}
	return n
}

// instanceMatchesHop reports whether CREATE instance idx of the directed
// (src → dst) pair carries hop's type and satisfies its property predicate,
// using the per-instance metadata (EdgeLabelsAt / EdgePropertiesAt). An
// instance that recorded no per-instance labels is treated as type-agnostic
// (deferred to the per-pair check the caller already performed) so a storage
// path recording the type only on the per-pair union is not wrongly excluded;
// an instance that DID record labels but not hop.relType is excluded, which is
// what filters a mixed-type parallel pair down to the matching subset.
func (op *MergePattern) instanceMatchesHop(src, dst string, idx int64, hop *mergePatternHop, hopProps []propLiteral) bool {
	if hop.relType != "" {
		labels := op.mutator.EdgeLabelsAt(src, dst, idx)
		if len(labels) > 0 && !containsLabel(labels, hop.relType) {
			return false
		}
	}
	if len(hopProps) > 0 {
		props := op.mutator.EdgePropertiesAt(src, dst, idx)
		if props == nil {
			props = op.mutator.EdgeProperties(src, dst)
		}
		if !nodeMatchesAllProperties(hopProps, props) {
			return false
		}
	}
	return true
}

// containsLabel reports whether labels contains want.
func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// resolveBound reads a bound chain position's NodeID from childRow, and
// resolves its string key. ok is false for a null value (e.g. an unmatched
// OPTIONAL MATCH endpoint).
func (op *MergePattern) resolveBound(childRow Row, n *mergePatternNode) (graph.NodeID, string, bool, error) {
	if n.col < 0 || n.col >= len(childRow) {
		return 0, "", false, nil
	}
	id, ok := nodeIDFromValue(childRow[n.col])
	if !ok {
		return 0, "", false, nil
	}
	key, ok := op.mutator.ResolveNodeLabel(id)
	if !ok {
		return 0, "", false, fmt.Errorf("unresolved bound NodeID %d for %q", id, n.varName)
	}
	return id, key, true, nil
}

// createChain creates every fresh node and every hop's relationship, in
// chain order, and returns the complete binding. Never reuses an existing
// node for any position — by construction this is only called after search
// has already established that no joint match exists for the whole pattern.
func (op *MergePattern) createChain(childRow Row) (binding, error) {
	b := make(binding, len(op.nodes))
	keys := make([]string, len(op.nodes))
	for i := range op.nodes {
		n := &op.nodes[i]
		if n.bound {
			id, key, ok, err := op.resolveBound(childRow, n)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("bound variable %q is null", n.varName)
			}
			b[i], keys[i] = id, key
			continue
		}
		key := op.freshNodeKey()
		if _, err := op.mutator.AddNode(key); err != nil {
			return nil, fmt.Errorf("AddNode %q: %w", n.varName, err)
		}
		for _, lbl := range n.labels {
			if err := op.mutator.SetNodeLabel(key, lbl); err != nil {
				return nil, fmt.Errorf("SetNodeLabel %q: %w", n.varName, err)
			}
		}
		// Widen the row with the nodes created to this position's left so this
		// fresh node's inline properties can reference an earlier same-pattern
		// node (#2024), matching CREATE's left-to-right resolution.
		props, epErr := op.effectiveNodeProps(n, op.bindingEvalRow(childRow, b, i))
		if epErr != nil {
			return nil, epErr
		}
		for _, p := range props {
			// Reserved inside SetNodeProperty (rmp #2358); the labels are attached
			// before this loop, so the reservation sees the full label set.
			if err := op.mutator.SetNodeProperty(key, p.key, p.value); err != nil {
				if isConstraintViolation(err) {
					return nil, err
				}
				return nil, fmt.Errorf("SetNodeProperty %q.%s: %w", n.varName, p.key, err)
			}
		}
		id, ok := op.mutator.ResolveNodeID(key)
		if !ok {
			return nil, fmt.Errorf("freshly created node %q did not resolve", n.varName)
		}
		b[i], keys[i] = id, key
	}
	for i := range op.hops {
		hop := &op.hops[i]
		srcIdx, dstIdx := hop.storageOrder(i)
		srcKey, dstKey := keys[srcIdx], keys[dstIdx]
		_, _, handle, err := op.mutator.AddEdgeH(srcKey, dstKey, 0)
		if err != nil {
			return nil, fmt.Errorf("AddEdge: %w", err)
		}
		if hop.relType != "" {
			op.mutator.SetEdgeLabel(srcKey, dstKey, hop.relType)
			op.mutator.SetEdgeLabelByHandle(srcKey, dstKey, handle, hop.relType)
		}
		// Write the hop's effective inline properties for THIS driving row
		// (literals merged with any non-literal per-row values, e.g.
		// `{kind: row.pk}`) — the SAME set the search predicate matched on,
		// computed once per driving row by runForRow (op.hopPropsForRow), so a
		// non-deterministic value is stored identically to how it was searched.
		hopProps := op.hopPropsForRow[i]
		if hop.relPropsRefsPatternNode {
			// The hop's inline properties reference an earlier same-pattern node
			// (e.g. `(a)-[:R {k: a.id}]->(b)`); evaluate them against the created
			// binding so the stored value matches the search predicate (#2024).
			hp, hpErr := op.effectiveHopProps(hop, op.bindingEvalRow(childRow, b, len(op.nodes)))
			if hpErr != nil {
				return nil, hpErr
			}
			hopProps = hp
		}
		for _, p := range hopProps {
			if err := op.mutator.SetEdgeProperty(srcKey, dstKey, p.key, p.value); err != nil {
				return nil, fmt.Errorf("SetEdgeProperty %s: %w", p.key, err)
			}
			if err := op.mutator.SetEdgePropertyByHandle(srcKey, dstKey, handle, p.key, p.value); err != nil {
				return nil, fmt.Errorf("SetEdgePropertyByHandle %s: %w", p.key, err)
			}
		}
	}
	return b, nil
}

// freshNodeKey mints a synthetic node key from the same process-wide counter
// [CreateNode]/[Merge] use, with the same "merge_" infix so the
// [parseSynthKeySuffix] recovery re-seed scan recognises it without any
// changes there.
func (op *MergePattern) freshNodeKey() string {
	n := globalNodeCounter.Add(1)
	return synthKeyPrefix + mergeKeyInfix + fmt.Sprintf("%x", n)
}

// emitRow extends childRow with every chain position's binding (fresh
// positions only — a bound position's column already carries its value)
// and every named hop's RelationshipValue.
func (op *MergePattern) emitRow(childRow Row, b binding) (Row, error) {
	width := len(childRow)
	for i := range op.nodes {
		if op.nodes[i].outCol >= width {
			width = op.nodes[i].outCol + 1
		}
	}
	for i := range op.hops {
		if op.hops[i].relCol >= width {
			width = op.hops[i].relCol + 1
		}
	}
	row := make(Row, width)
	copy(row, childRow)
	for i := range op.nodes {
		n := &op.nodes[i]
		if n.bound || n.outCol < 0 {
			continue
		}
		row[n.outCol] = expr.IntegerValue(int64(b[i]))
	}
	for i := range op.hops {
		hop := &op.hops[i]
		if hop.relCol < 0 {
			continue
		}
		srcIdx, dstIdx := hop.storageOrder(i)
		srcKey, ok1 := op.mutator.ResolveNodeLabel(b[srcIdx])
		dstKey, ok2 := op.mutator.ResolveNodeLabel(b[dstIdx])
		if !ok1 || !ok2 {
			continue
		}
		var relProps expr.MapValue
		if raw := op.mutator.EdgeProperties(srcKey, dstKey); len(raw) > 0 {
			relProps = make(expr.MapValue, len(raw))
			for k, pv := range raw {
				if v, ok := lpgPropToExprBinding(pv); ok {
					relProps[k] = v
				}
			}
		}
		row[hop.relCol] = expr.RelationshipValue{
			ID:         uint64(b[srcIdx])<<32 | uint64(b[dstIdx]),
			StartID:    uint64(b[srcIdx]),
			EndID:      uint64(b[dstIdx]),
			Type:       hop.relType,
			Properties: relProps,
		}
	}
	return row, nil
}

// bindingEvalRow builds the row a fresh position's inline property map is
// evaluated against so it can reference an EARLIER same-pattern node bound to
// its left (e.g. `(a {id:1})-[:R]->(b {k: a.id})` — b.k reads a). It widens
// childRow to the full pattern width and sets the first nBound fresh node
// positions to their NodeID at their output column; positions not yet bound
// stay null, matching CREATE's left-to-right resolution. A bound position's
// value already rides along in childRow (copied through). #2024.
func (op *MergePattern) bindingEvalRow(childRow Row, b binding, nBound int) Row {
	width := len(childRow)
	for i := range op.nodes {
		if op.nodes[i].outCol >= width {
			width = op.nodes[i].outCol + 1
		}
	}
	row := make(Row, width)
	copy(row, childRow)
	for i := 0; i < nBound && i < len(op.nodes) && i < len(b); i++ {
		n := &op.nodes[i]
		if n.bound || n.outCol < 0 || n.outCol >= width {
			continue
		}
		row[n.outCol] = expr.IntegerValue(int64(b[i]))
	}
	return row
}

// applyActions applies each pre-parsed ON CREATE / ON MATCH action to
// whichever chain entity its nodeVar names — a fresh or bound node position,
// or a hop's relationship variable — resolved against b.
// evalRow is the schema-consistent row emitted for this binding (nodes as
// IntegerValue at their output columns, relationships as RelationshipValue at
// theirs); it feeds the per-row RHS evaluators in evals so an expression
// action such as `ON MATCH SET n.num = n.num + 1` reads the entity's current
// value instead of being dropped as a literal-parse failure (#1965).
func (op *MergePattern) applyActions(b binding, evalRow Row, actions []mergeAction, evals map[string]ValueEvalFn) error {
	for _, act := range actions {
		if idx, ok := op.nodeIndexByVar(act.nodeVar); ok {
			key, ok := op.mutator.ResolveNodeLabel(b[idx])
			if !ok {
				continue
			}
			if err := op.applyNodeAction(key, act, evalRow, evals); err != nil {
				return err
			}
			continue
		}
		if hopIdx, ok := op.hopIndexByRelVar(act.nodeVar); ok {
			hop := &op.hops[hopIdx]
			srcIdx, dstIdx := hop.storageOrder(hopIdx)
			srcKey, ok1 := op.mutator.ResolveNodeLabel(b[srcIdx])
			dstKey, ok2 := op.mutator.ResolveNodeLabel(b[dstIdx])
			if !ok1 || !ok2 {
				continue
			}
			if err := op.applyRelAction(srcKey, dstKey, act, evalRow, evals); err != nil {
				return err
			}
			continue
		}
		// act.nodeVar names neither a chain node nor a chain relationship —
		// parseMergeActions only recognises the `var.key = value` and
		// `var:Label` shapes, so an out-of-scope or unrecognised target is
		// simply not one this operator can apply; skip rather than error,
		// matching Merge's own tolerant behaviour.
	}
	return nil
}

// nodeIndexByVar returns the chain position index whose variable name
// matches v, if any.
func (op *MergePattern) nodeIndexByVar(v string) (int, bool) {
	for i := range op.nodes {
		if op.nodes[i].varName == v {
			return i, true
		}
	}
	return -1, false
}

// hopIndexByRelVar returns the hop index whose relationship variable name
// matches v, if any.
func (op *MergePattern) hopIndexByRelVar(v string) (int, bool) {
	for i := range op.hops {
		if op.hops[i].relVar != "" && op.hops[i].relVar == v {
			return i, true
		}
	}
	return -1, false
}

// applyNodeAction applies one mergeAction (a property write or a label-set)
// to the node identified by key. evalRow / evals feed the per-row RHS
// evaluator for a non-literal expression (#1965); see [MergePattern.applyActions].
func (op *MergePattern) applyNodeAction(key string, act mergeAction, evalRow Row, evals map[string]ValueEvalFn) error {
	if len(act.setLabels) > 0 {
		// Attaching a label puts the node under every UNIQUE constraint declared
		// on that label, so reserve before writing — see
		// cypher/exec/label_constraints.go. The pattern-MERGE action path is a
		// distinct label-write site from both the SetLabels operator and the
		// single-node MERGE action, and all three need the same enforcement or the
		// duplicate simply commits through whichever one was left out (rmp #2352).
		for _, lbl := range act.setLabels {
			// Reserved inside SetNodeLabel, at the mutator choke point (rmp #2358).
			if err := op.mutator.SetNodeLabel(key, lbl); err != nil {
				if isConstraintViolation(err) {
					return err
				}
				return fmt.Errorf("exec: MergePattern: SetNodeLabel: %w", err)
			}
		}
		return nil
	}
	v, parseErr := parsePropValue(act.value)
	if parseErr != nil {
		remove, resolved, val, evalErr := op.resolveNonLiteral(act, parseErr, evalRow, evals)
		if evalErr != nil {
			return evalErr
		}
		if remove {
			// SET x.k = null (literal or expression→null) removes the property;
			// DelNodeProperty releases its old constrained value, so a UNIQUE slot
			// is not leaked as a phantom reservation (#1904, rmp #2358).
			op.mutator.DelNodeProperty(key, act.key)
			return nil
		}
		if !resolved {
			// Non-literal RHS with no evaluator, or an evaluator no-op (eval
			// error / unstorable type — matching regular SET): no write.
			return nil
		}
		v = val
	}
	// Released-then-reserved inside SetNodeProperty (rmp #2358), which is what stops
	// the replaced value leaking as a permanent phantom reservation (#1904) and an
	// idempotent self-set being rejected as its own duplicate.
	if err := op.mutator.SetNodeProperty(key, act.key, v); err != nil {
		if isConstraintViolation(err) {
			return err
		}
		return fmt.Errorf("exec: MergePattern: SetNodeProperty: %w", err)
	}
	return nil
}

// applyRelAction applies one mergeAction to the relationship (srcKey,
// dstKey). Only the single-property write and label-set shapes are
// supported here (matching the pre-existing scope: whole-entity REPLACE /
// entity-copy actions on a compound-pattern relationship are not yet
// supported and are silently skipped, mirroring how such actions on an
// out-of-scope target are already tolerated elsewhere in this operator).
func (op *MergePattern) applyRelAction(srcKey, dstKey string, act mergeAction, evalRow Row, evals map[string]ValueEvalFn) error {
	if len(act.setLabels) > 0 {
		// A relationship has no labels beyond its single type; SET r:Foo
		// is rejected at compile time (sema), so this shape cannot occur
		// for a relationship target. Defensive no-op.
		return nil
	}
	v, parseErr := parsePropValue(act.value)
	if parseErr != nil {
		remove, resolved, val, evalErr := op.resolveNonLiteral(act, parseErr, evalRow, evals)
		if evalErr != nil {
			return evalErr
		}
		if remove {
			op.mutator.DelEdgeProperty(srcKey, dstKey, act.key)
			if handle, ok := op.mutator.FirstEdgeHandle(srcKey, dstKey); ok && handle != 0 {
				op.mutator.DelEdgePropertyByHandle(srcKey, dstKey, handle, act.key)
			}
			return nil
		}
		if !resolved {
			return nil
		}
		v = val
	}
	if err := op.mutator.SetEdgeProperty(srcKey, dstKey, act.key, v); err != nil {
		return fmt.Errorf("exec: MergePattern: SetEdgeProperty: %w", err)
	}
	if handle, ok := op.mutator.FirstEdgeHandle(srcKey, dstKey); ok && handle != 0 {
		if err := op.mutator.SetEdgePropertyByHandle(srcKey, dstKey, handle, act.key, v); err != nil {
			return fmt.Errorf("exec: MergePattern: SetEdgePropertyByHandle: %w", err)
		}
	}
	return nil
}

// resolveNonLiteral handles a MergePattern property-set action whose RHS did
// not parse as a literal. Its four results answer, in order:
//   - remove: the RHS is a literal null or evaluates to null → the caller
//     removes the property (openCypher SET-to-null semantics);
//   - resolved (with val): a registered per-row evaluator produced a value;
//   - neither, nil err: an evaluator no-op (eval error / unstorable type —
//     matching how the regular SET operator degrades);
//   - neither, non-nil err: a non-literal RHS with NO registered evaluator,
//     which returns the original parse error to preserve this operator's prior
//     fail-stop behaviour (it never silently dropped such an action). Every
//     genuine non-literal action is wired with an evaluator by the physical
//     builder, so the error branch is defensive only.
func (op *MergePattern) resolveNonLiteral(act mergeAction, parseErr error, evalRow Row, evals map[string]ValueEvalFn) (remove, resolved bool, val lpg.PropertyValue, err error) {
	if isNullPropertyValueErr(parseErr) {
		return true, false, lpg.PropertyValue{}, nil
	}
	fn, has := evals[MergeActionEvalKey(act.nodeVar, act.key)]
	if !has {
		return false, false, lpg.PropertyValue{}, fmt.Errorf("exec: MergePattern: parse value %q: %w", act.value, parseErr)
	}
	v, isNull, hasValue, evalErr := fn(evalRow)
	if evalErr != nil {
		return false, false, lpg.PropertyValue{}, evalErr
	}
	if isNull {
		return true, false, lpg.PropertyValue{}, nil
	}
	if !hasValue {
		return false, false, lpg.PropertyValue{}, nil
	}
	return false, true, v, nil
}

// isNullPropertyValueErr reports whether err is the sentinel ErrPropertyValueIsNull.
func isNullPropertyValueErr(err error) bool {
	return errors.Is(err, ErrPropertyValueIsNull)
}
