package cypher

// constraint_check.go — commit-time enforcement of NOT NULL (property-existence)
// constraints (#1754, ACID Consistency).
//
// # The invariant
//
// A NOT NULL constraint — CREATE CONSTRAINT <n> FOR (m:Label) REQUIRE m.prop IS
// NOT NULL — is a COMMIT-TIME, how-agnostic invariant (confirmed against the
// Neo4j Cypher Manual / openCypher semantics): at transaction commit, every node
// that carries the constrained label in its FINAL committed state must have the
// property present and non-null. The means by which the node reached that state
// (CREATE, MERGE, SET n:Label, REMOVE n.prop, SET n.prop = null) is irrelevant —
// only the final state matters.
//
// Before #1754 the constraint was enforced only on the property-SET path
// ([exec.ConstraintRegistry.CheckSetProperty] when the value is null). That left
// a gap: CREATE (:Acct {id:1}) under a NOT NULL(Acct.email) constraint never sets
// `email`, so the check never fired and the violating node was wrongly accepted.
// Likewise MATCH (n) SET n:Acct adding the constrained label to a node lacking
// `email` was not caught. Both violate the CLAUDE.md Consistency mandate.
//
// # The enforcement point
//
// Enforcement runs at transaction commit, INSIDE the visibility barrier (visMu),
// BEFORE the writes become durable/visible — the same in-barrier window that the
// WAL fsync and the #1282 undo replay use. It examines ONLY the nodes the
// transaction TOUCHED (created, gained a label, or had a property removed/set to
// null), never the whole graph: a node the transaction did not touch already
// satisfied the constraint when its own transaction committed. For each touched
// node still live in its final state, for every NOT-NULL-constrained label it
// carries, the property is asserted present and non-null. On the first violation
// the WHOLE transaction is rejected atomically: the existing in-barrier undo path
// rolls every eager mutation back and a typed [exec.ConstraintViolationError]
// (Kind "NOT NULL", wrapping [exec.ErrConstraintViolation]) is surfaced, exactly
// like the SET-to-null path and the WAL-fsync-failure rollback.
//
// # Performance
//
// The check is gated: it is a no-op unless the registry holds at least one NOT
// NULL constraint, so a workload with no existence constraints pays only one
// atomic-load-cheap [exec.ConstraintRegistry.HasAnyNotNull] call per commit and
// the touched-node recording is skipped entirely. When active, the cost is
// O(touched nodes × their labels) with no graph scan and no global lock beyond
// the barrier already held. The touched-node set is allocated lazily on the first
// recorded touch, so a transaction that touches no node (or a read misrouted
// here) allocates nothing.
//
// # Concurrency
//
// touchedNodes is NOT safe for concurrent use: like [undoLog], it is owned by a
// single transaction and all recording happens on the executing goroutine under
// the write barrier. The commit-time scan also runs under the barrier, so it
// observes a quiescent graph (no concurrent writer, no in-flight View).

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// touchedNodes is the per-transaction set of node keys whose final-state label
// or property set may bring a NOT NULL constraint into play. It accumulates
// across every statement of a transaction (one instance shared by all mutator
// adapters of a RunInTx call or an ExplicitTx), mirroring [undoLog]. The zero
// value is an empty, ready-to-use set; the backing map is allocated on the first
// recorded key.
//
// Only keys that could INTRODUCE a violation are recorded: a freshly created
// node, a node that gained a label, or a node that had a property removed (which
// includes SET n.prop = null, a removal in the Cypher data model). Setting a
// property to a non-null value or removing a label can never introduce an
// existence violation, so those paths record nothing — keeping the set minimal.
type touchedNodes struct {
	// keys holds each touched node key once. A map dedups the common case of a
	// node touched by several clauses in one transaction (CREATE then SET).
	keys map[string]struct{}
}

// touch records node key n as touched. It is a no-op when the receiver is nil,
// which lets callers thread an optional *touchedNodes without nil checks at
// every call site (a nil set means "this adapter does not track touches", e.g. a
// read-only adapter or an engine with no NOT NULL constraint active).
func (t *touchedNodes) touch(n string) {
	if t == nil {
		return
	}
	if t.keys == nil {
		t.keys = make(map[string]struct{}, 8)
	}
	t.keys[n] = struct{}{}
}

// empty reports whether no node key has been recorded.
func (t *touchedNodes) empty() bool { return t == nil || len(t.keys) == 0 }

// checkNotNullConstraints asserts that every touched node still live in its
// final state satisfies every NOT NULL constraint attached to a label it carries.
// It returns the first [exec.ConstraintViolationError] found (wrapping
// [exec.ErrConstraintViolation]), or nil when all touched nodes satisfy their
// constraints.
//
// It MUST be called inside the visibility barrier (the enclosing
// ApplyAtomically / ApplyInsideLocked window), BEFORE the WAL fsync and the index
// commit, so a violation rolls the whole transaction back before any reader can
// observe it. Because the barrier excludes concurrent writers and Views, it reads
// graph state directly via the barrier-safe [lpg.Graph.HasNodeLabel] /
// [lpg.Graph.GetNodeProperty] / [lpg.Graph.IsTombstoned] APIs (the same pattern
// reseedConstraintsInsideBarrier uses), never through [lpg.Graph.View] (which
// would deadlock the non-re-entrant barrier).
//
// It is gated by the caller on [exec.ConstraintRegistry.HasAnyNotNull]; a nil
// receiver, a nil registry, or an empty touched set all short-circuit to nil.
func (t *touchedNodes) checkNotNullConstraints(reg *exec.ConstraintRegistry, g *lpg.Graph[string, float64]) error {
	if t.empty() || reg == nil || g == nil {
		return nil
	}
	mapper := g.AdjList().Mapper()
	for key := range t.keys {
		id, ok := mapper.Lookup(key)
		if !ok {
			// The key was never interned (cannot happen for a recorded touch) or
			// has no NodeID — nothing to check.
			continue
		}
		// A node not in the final committed state (deleted / tombstoned) is exempt:
		// the constraint quantifies over nodes that carry the label, and a removed
		// node carries nothing. DETACH DELETE / DELETE therefore need no check.
		if g.IsTombstoned(id) {
			continue
		}
		// Enumerate the node's final-state labels once, then test only those that
		// carry a NOT NULL constraint. NodeLabelsByID avoids the key→ID Mapper
		// lookup we already did above.
		labels := g.NodeLabelsByID(id)
		for _, label := range labels {
			props := reg.NotNullProperties(label)
			for _, prop := range props {
				v, present := g.GetNodeProperty(key, prop)
				// Absent property and an explicit null are identical in the Cypher
				// data model: both fail IS NOT NULL. GetNodeProperty reports absent
				// via present=false; a stored value is never the zero PropertyValue,
				// but guard Kind()==0 too so the check matches CheckSetProperty.
				if !present || v.Kind() == 0 {
					return &exec.ConstraintViolationError{
						Label:    label,
						Property: prop,
						Kind:     "NOT NULL",
						Detail:   "property is null on a node carrying the constrained label in its committed state",
					}
				}
			}
		}
	}
	return nil
}
