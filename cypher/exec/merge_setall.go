package exec

// merge_setall.go — whole-entity ON CREATE / ON MATCH SET support for the node
// Merge and MergePattern operators.
//
// A whole-entity SET action inside a MERGE clause — `SET n = <expr>` or
// `SET n += <expr>` where the target is a node variable (not a property) — is
// handled here rather than by the per-property [parseMergeActions] path, which
// only recognises `var.key = value` and `var:Label` forms and silently drops a
// keyless `var = <rhs>` action (#2031). The right-hand side is evaluated per
// row and dispatched on the value's runtime kind, exactly as the standalone
// [SetAllProperties] operator does:
//
//   - map: write its entries (replace clears the node first; null-valued keys
//     are removed per openCypher SET-map semantics);
//   - node/relationship: copy every property from that entity;
//   - null: clear the node for `=` (replace); a no-op for `+=` (append);
//   - any other kind (scalar, list): a runtime TypeError.
//
// The relationship whole-entity form on the MergeRelationship fast path is
// unaffected (it has its own KVAction machinery).

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// MergeSetAllAction is a whole-entity ON CREATE / ON MATCH SET action
// (`SET n = <expr>` / `SET n += <expr>`) evaluated per row. TargetVar names the
// node variable the action writes to; IsReplace selects `=` (true, replace all)
// vs `+=` (false, merge) semantics; Eval evaluates the right-hand side against
// the merged/created row.
type MergeSetAllAction struct {
	Eval      ExprValueEvalFn
	TargetVar string
	IsReplace bool
}

// applyWholeEntityValueToNode applies a whole-entity SET value v to the node
// identified by nodeKey with `=` (isReplace) / `+=` semantics, dispatching on
// v's runtime kind. Constraint enforcement happens inside the mutator's property
// writes (rmp #2358); a violation is returned as an error, so the transaction
// aborts rather than the write being silently skipped.
func applyWholeEntityValueToNode(
	mut GraphMutator,
	targetVar, nodeKey string,
	isReplace bool,
	v expr.Value,
) error {
	if v == nil || expr.IsNull(v) {
		// `SET n = null` clears all properties; `SET n += null` is a no-op.
		if isReplace {
			clearNodeProps(mut, nodeKey)
		}
		return nil
	}
	switch src := v.(type) {
	case expr.MapValue:
		props, nullKeys, err := exprMapValueToEntries(targetVar, src)
		if err != nil {
			return err
		}
		if isReplace {
			clearNodeProps(mut, nodeKey)
		}
		for _, k := range nullKeys {
			delNodeProp(mut, nodeKey, k)
		}
		for _, p := range props {
			if err := setNodeProp(mut, nodeKey, p.key, p.value); err != nil {
				return err
			}
		}
		return nil
	case expr.NodeValue:
		srcKey, ok := mut.ResolveNodeLabel(graph.NodeID(src.ID))
		if !ok {
			return nil // unresolvable source: no-op (null-source semantics)
		}
		return copyPropsToNode(mut, mut.NodeProperties(srcKey), nodeKey, isReplace)
	case expr.RelationshipValue:
		s, ok1 := mut.ResolveNodeLabel(graph.NodeID(src.StartID))
		d, ok2 := mut.ResolveNodeLabel(graph.NodeID(src.EndID))
		if !ok1 || !ok2 {
			return nil
		}
		return copyPropsToNode(mut, mut.EdgeProperties(s, d), nodeKey, isReplace)
	default:
		return fmt.Errorf("TypeError: SET %s: expected a Map, Node or Relationship but was %s", targetVar, v.Kind())
	}
}

// copyPropsToNode writes every entry of srcProps onto nodeKey with =/+=
// semantics. srcProps is snapshotted first so copying a node onto itself is
// safe.
func copyPropsToNode(
	mut GraphMutator,
	srcProps map[string]lpg.PropertyValue,
	nodeKey string,
	isReplace bool,
) error {
	snap := make(map[string]lpg.PropertyValue, len(srcProps))
	for k, v := range srcProps {
		snap[k] = v
	}
	if isReplace {
		clearNodeProps(mut, nodeKey)
	}
	for k, v := range snap {
		if err := setNodeProp(mut, nodeKey, k, v); err != nil {
			return err
		}
	}
	return nil
}

// clearNodeProps removes every property from nodeKey. Each removal releases its
// constrained value at the mutator choke point, so none leaks as a phantom
// reservation (rmp #2358).
func clearNodeProps(mut GraphMutator, nodeKey string) {
	props := mut.NodeProperties(nodeKey)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	for _, k := range keys {
		delNodeProp(mut, nodeKey, k)
	}
}

// delNodeProp removes one property from nodeKey. DelNodeProperty releases its
// constrained value at the mutator choke point (rmp #2358), so this no longer
// carries a registry — which is why it lost the "Constrained" suffix its old name
// promised and no longer delivered.
func delNodeProp(mut GraphMutator, nodeKey, key string) {
	mut.DelNodeProperty(nodeKey, key)
}

// setNodeProp writes one (key,value) pair to nodeKey. SetNodeProperty enforces
// UNIQUE — releasing the node's own prior value before the check, so an idempotent
// self-set is not rejected as its own duplicate — and this returns the violation as
// an error so the enclosing statement aborts rather than silently skipping.
func setNodeProp(
	mut GraphMutator,
	nodeKey, key string,
	val lpg.PropertyValue,
) error {
	if serr := mut.SetNodeProperty(nodeKey, key, val); serr != nil {
		// A violation must reach the caller UNWRAPPED so errors.As recovers the
		// typed *ConstraintViolationError; only a genuine write failure is wrapped.
		if isConstraintViolation(serr) {
			return serr
		}
		return fmt.Errorf("exec: MERGE ON action SetNodeProperty %q: %w", key, serr)
	}
	return nil
}
