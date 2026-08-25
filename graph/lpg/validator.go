package lpg

import "sync/atomic"

// SchemaValidator is the interface that schema enforcement hooks implement.
// It is satisfied by *schema.Schema after properties have been registered.
//
// Validate receives the property name and the value about to be written.
// A nil return allows the write; a non-nil return rejects it with the
// returned error, leaving the graph state unchanged.
//
// Validate enforces only per-property typing — a single value examined in
// isolation — because it runs at the mutation point, where the node is not
// yet complete (a node acquires its labels and properties one mutation at a
// time; see [Graph.SetNodeProperty]). Whole-node invariants such as
// required-property existence cannot be decided from one value and are
// enforced separately by [NodeValidator]/[Graph.ValidateNode] at the
// node-finalisation boundary.
//
// # Where the validator runs, and why it matters for durability
//
// The graph's own setters call Validate at the mutation point. A WRITE-AHEAD
// path cannot rely on that alone, because it appends and fsyncs BEFORE it
// applies: by the time the mutation point is reached the value is already
// durable, and a replay installs no validator, so a refused value would be
// materialised by recovery.
//
// Both write paths therefore validate BEFORE they buffer:
//
//   - the Cypher engine, in walMutatorAdapter.SetNodeProperty, which has always
//     done so;
//   - store/txn, in Tx.SetNodeProperty and its siblings, since rmp #2602 — see
//     [Graph.ValidateProperty], which exists for exactly that call.
//
// So an installed validator IS a durability gate on both paths: a value it
// refuses never reaches the write-ahead log. This is what the ACID Consistency
// contract requires, since it names label/property typing among the invariants a
// committed transaction must leave satisfied.
//
// Recovery still installs no validator and deliberately does not consult one; it
// does not need to, because the log cannot contain a refused value. A log
// written by an older build, before rmp #2602, is outside that guarantee.
//
// Implementations must be safe for concurrent use.
type SchemaValidator interface {
	Validate(propertyName string, value PropertyValue) error
}

// NodeValidator is the optional whole-node enforcement hook. A
// [SchemaValidator] installed via [Graph.SetValidator] that also implements
// NodeValidator gains required-property/existence enforcement: callers invoke
// [Graph.ValidateNode] at the point a node is finalised (after all of its
// labels and properties are set) to reject a node that violates a whole-node
// invariant the per-value [SchemaValidator.Validate] cannot see.
//
// ValidateNode receives the node's complete label set and property bag and
// returns a non-nil error to reject it. It is satisfied by *schema.Schema,
// whose [github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema.Schema.ValidateNode]
// has the matching signature, so an installed schema enforces required
// properties through [Graph.ValidateNode] without any extra wiring.
//
// Implementations must be safe for concurrent use.
type NodeValidator interface {
	ValidateNode(labels []string, props map[string]PropertyValue) error
}

// validatorHolder wraps a SchemaValidator pointer for atomic swap.
// Using an interface value inside atomic.Pointer avoids the need for
// a separate indirection struct.
type validatorHolder struct {
	v SchemaValidator
}

// atomicValidator provides lock-free get/set for an optional SchemaValidator.
type atomicValidator struct {
	p atomic.Pointer[validatorHolder]
}

// load returns the current validator, or nil when none is set.
func (a *atomicValidator) load() SchemaValidator {
	h := a.p.Load()
	if h == nil {
		return nil
	}
	return h.v
}

// store installs v as the current validator. Passing nil clears it.
func (a *atomicValidator) store(v SchemaValidator) {
	if v == nil {
		a.p.Store(nil)
		return
	}
	a.p.Store(&validatorHolder{v: v})
}
