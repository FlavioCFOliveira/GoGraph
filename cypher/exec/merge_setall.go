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

// applyWholeEntityValueToEdge is [applyWholeEntityValueToNode] for a
// relationship target: the same kind dispatch (map / node / relationship / null
// / TypeError), writing to the edge identified by (srcKey, dstKey) and, when
// resolved, the stable per-instance handle.
//
// MergePattern needs this because it — not [MergeRelationship] — is the operator
// that runs whenever a MERGE pattern has an unbound endpoint, and it used to skip
// a whole-entity action on a chain relationship variable as "left to the
// relationship machinery". That machinery is MergeRelationship's KVAction path,
// which only ever sees the narrow both-endpoints-bound, all-literal-map shape, so
// every other shape had no writer at all and was lost silently (rmp #2510).
//
// Each write goes to the per-pair aggregate first and is then mirrored to the
// instance's own by-handle bag, which is what reads route through; only the
// aggregate write is counted, so the statement reports one +properties per
// assignment. This is the idiom [MergePattern.applyRelAction] and
// [SetAllProperties.writeOne] already use.
func applyWholeEntityValueToEdge(
	mut GraphMutator,
	targetVar, srcKey, dstKey string,
	handle uint64,
	isReplace bool,
	v expr.Value,
) error {
	if v == nil || expr.IsNull(v) {
		// `SET r = null` clears all properties; `SET r += null` is a no-op.
		if isReplace {
			clearEdgeProps(mut, srcKey, dstKey, handle)
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
			clearEdgeProps(mut, srcKey, dstKey, handle)
		}
		for _, k := range nullKeys {
			delEdgeProp(mut, srcKey, dstKey, handle, k)
		}
		for _, p := range props {
			if err := setEdgeProp(mut, srcKey, dstKey, handle, p.key, p.value); err != nil {
				return err
			}
		}
		return nil
	case expr.NodeValue:
		nodeKey, ok := mut.ResolveNodeLabel(graph.NodeID(src.ID))
		if !ok {
			return nil // unresolvable source: no-op (null-source semantics)
		}
		return copyPropsToEdge(mut, mut.NodeProperties(nodeKey), srcKey, dstKey, handle, isReplace)
	case expr.RelationshipValue:
		s, ok1 := mut.ResolveNodeLabel(graph.NodeID(src.StartID))
		d, ok2 := mut.ResolveNodeLabel(graph.NodeID(src.EndID))
		if !ok1 || !ok2 {
			return nil
		}
		return copyPropsToEdge(mut, relSourceProps(mut, s, d, src.ID), srcKey, dstKey, handle, isReplace)
	default:
		return fmt.Errorf("TypeError: SET %s: expected a Map, Node or Relationship but was %s", targetVar, v.Kind())
	}
}

// relSourceProps reads the property map of the SOURCE relationship of a
// whole-entity copy. Since rmp #2317 a [expr.RelationshipValue]'s ID is the
// instance's stable handle, and reads are bag-authoritative, so a resolved handle
// reads that instance's own bag; only a handle-less value falls back to the
// per-pair aggregate, which in a multigraph is the union over parallel twins.
//
// [applyWholeEntityValueToNode]'s own relationship-source branch still reads the
// aggregate unconditionally. Aligning it would change what `MERGE (n) ON CREATE
// SET n = r` copies when r has parallel twins — a behaviour change on a path that
// is correct for every non-parallel graph and outside rmp #2510's scope — so it is
// deliberately left alone rather than altered in passing.
func relSourceProps(mut GraphMutator, srcKey, dstKey string, handle uint64) map[string]lpg.PropertyValue {
	if handle != 0 {
		if bag := mut.EdgePropertiesByHandle(srcKey, dstKey, handle); len(bag) > 0 {
			return bag
		}
	}
	return mut.EdgeProperties(srcKey, dstKey)
}

// copyPropsToEdge writes every entry of srcProps onto the edge with =/+=
// semantics. srcProps is snapshotted first so copying an edge onto itself is
// safe.
func copyPropsToEdge(
	mut GraphMutator,
	srcProps map[string]lpg.PropertyValue,
	srcKey, dstKey string,
	handle uint64,
	isReplace bool,
) error {
	snap := make(map[string]lpg.PropertyValue, len(srcProps))
	for k, v := range srcProps {
		snap[k] = v
	}
	if isReplace {
		clearEdgeProps(mut, srcKey, dstKey, handle)
	}
	for k, v := range snap {
		if err := setEdgeProp(mut, srcKey, dstKey, handle, k, v); err != nil {
			return err
		}
	}
	return nil
}

// clearEdgeProps removes every property a whole-entity replace must tear down:
// the per-pair aggregate keys plus, when the handle is resolved, the targeted
// instance's own bag — [relClearKeys]'s union, because the aggregate alone can
// miss a key only the instance carries (#2502).
func clearEdgeProps(mut GraphMutator, srcKey, dstKey string, handle uint64) {
	for k := range relClearKeys(mut, srcKey, dstKey, handle) {
		delEdgeProp(mut, srcKey, dstKey, handle, k)
	}
}

// delEdgeProp removes one property from the edge. With a resolved handle and a
// mutator implementing [relInstancePropRemover], the removal is gated on that
// instance's OWN bag so -properties is not counted for a key only a parallel
// sibling carried (#2501); the handle-less fallback is the pairwise removal.
func delEdgeProp(mut GraphMutator, srcKey, dstKey string, handle uint64, key string) {
	if handle != 0 {
		if m, ok := mut.(relInstancePropRemover); ok {
			m.DelEdgePropertyOnInstance(srcKey, dstKey, handle, key)
			return
		}
		mut.DelEdgeProperty(srcKey, dstKey, key)
		mut.DelEdgePropertyByHandle(srcKey, dstKey, handle, key)
		return
	}
	mut.DelEdgeProperty(srcKey, dstKey, key)
}

// setEdgeProp writes one (key, value) pair to the edge: the per-pair aggregate
// (which counts the +properties side effect) and then the instance's own
// by-handle bag (which reads route through, and which does not double-count).
func setEdgeProp(
	mut GraphMutator,
	srcKey, dstKey string,
	handle uint64,
	key string,
	val lpg.PropertyValue,
) error {
	if err := mut.SetEdgeProperty(srcKey, dstKey, key, val); err != nil {
		return fmt.Errorf("exec: MERGE ON action SetEdgeProperty %q: %w", key, err)
	}
	if handle != 0 {
		if err := mut.SetEdgePropertyByHandle(srcKey, dstKey, handle, key, val); err != nil {
			return fmt.Errorf("exec: MERGE ON action SetEdgePropertyByHandle %q: %w", key, err)
		}
	}
	return nil
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
