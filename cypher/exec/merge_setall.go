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
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
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
// v's runtime kind. Constraint enforcement mirrors the per-property MERGE
// action path: a violation is returned as an error (the transaction aborts),
// not silently skipped.
func applyWholeEntityValueToNode(
	mut GraphMutator,
	reg *ConstraintRegistry,
	mgr *index.Manager,
	targetVar, nodeKey string,
	isReplace bool,
	v expr.Value,
) error {
	if v == nil || expr.IsNull(v) {
		// `SET n = null` clears all properties; `SET n += null` is a no-op.
		if isReplace {
			clearNodePropsConstrained(mut, reg, nodeKey)
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
			clearNodePropsConstrained(mut, reg, nodeKey)
		}
		for _, k := range nullKeys {
			delNodePropConstrained(mut, reg, nodeKey, k)
		}
		for _, p := range props {
			if err := setNodePropConstrained(mut, reg, mgr, nodeKey, p.key, p.value); err != nil {
				return err
			}
		}
		return nil
	case expr.NodeValue:
		srcKey, ok := mut.ResolveNodeLabel(graph.NodeID(src.ID))
		if !ok {
			return nil // unresolvable source: no-op (null-source semantics)
		}
		return copyPropsToNode(mut, reg, mgr, mut.NodeProperties(srcKey), nodeKey, isReplace)
	case expr.RelationshipValue:
		s, ok1 := mut.ResolveNodeLabel(graph.NodeID(src.StartID))
		d, ok2 := mut.ResolveNodeLabel(graph.NodeID(src.EndID))
		if !ok1 || !ok2 {
			return nil
		}
		return copyPropsToNode(mut, reg, mgr, mut.EdgeProperties(s, d), nodeKey, isReplace)
	default:
		return fmt.Errorf("TypeError: SET %s: expected a Map, Node or Relationship but was %s", targetVar, v.Kind())
	}
}

// copyPropsToNode writes every entry of srcProps onto nodeKey with =/+=
// semantics. srcProps is snapshotted first so copying a node onto itself is
// safe.
func copyPropsToNode(
	mut GraphMutator,
	reg *ConstraintRegistry,
	mgr *index.Manager,
	srcProps map[string]lpg.PropertyValue,
	nodeKey string,
	isReplace bool,
) error {
	snap := make(map[string]lpg.PropertyValue, len(srcProps))
	for k, v := range srcProps {
		snap[k] = v
	}
	if isReplace {
		clearNodePropsConstrained(mut, reg, nodeKey)
	}
	for k, v := range snap {
		if err := setNodePropConstrained(mut, reg, mgr, nodeKey, k, v); err != nil {
			return err
		}
	}
	return nil
}

// clearNodePropsConstrained removes every property from nodeKey, releasing any
// constrained value first so it does not leak as a phantom reservation.
func clearNodePropsConstrained(mut GraphMutator, reg *ConstraintRegistry, nodeKey string) {
	props := mut.NodeProperties(nodeKey)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	for _, k := range keys {
		delNodePropConstrained(mut, reg, nodeKey, k)
	}
}

// delNodePropConstrained removes one property from nodeKey, releasing its
// constrained value first.
func delNodePropConstrained(mut GraphMutator, reg *ConstraintRegistry, nodeKey, key string) {
	if reg != nil {
		if oldVal, had := mut.NodeProperties(nodeKey)[key]; had {
			releaseConstraintValue(reg, mut, mut.NodeLabels(nodeKey), key, oldVal)
		}
	}
	mut.DelNodeProperty(nodeKey, key)
}

// setNodePropConstrained writes one (key,value) pair to nodeKey with
// pre-write constraint enforcement, mirroring the per-property MERGE action
// path: the node's own prior constrained value is released before the check so
// an idempotent self-set is not rejected as its own duplicate, and a violation
// is returned as an error.
func setNodePropConstrained(
	mut GraphMutator,
	reg *ConstraintRegistry,
	mgr *index.Manager,
	nodeKey, key string,
	val lpg.PropertyValue,
) error {
	if reg != nil {
		labels := mut.NodeLabels(nodeKey)
		if oldVal, had := mut.NodeProperties(nodeKey)[key]; had {
			releaseConstraintValue(reg, mut, labels, key, oldVal)
		}
		if cerr := reserveConstraintValue(reg, mut, labels, key, val, mgr); cerr != nil {
			return cerr
		}
	}
	if serr := mut.SetNodeProperty(nodeKey, key, val); serr != nil {
		return fmt.Errorf("exec: MERGE ON action SetNodeProperty %q: %w", key, serr)
	}
	if reg != nil {
		reg.RecordPropertySet(mut.NodeLabels(nodeKey), key, val)
	}
	return nil
}
