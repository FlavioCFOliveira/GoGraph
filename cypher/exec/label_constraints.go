package exec

// label_constraints.go — UNIQUE enforcement on the LABEL-set path (rmp #2352).
//
// # The hole this closes
//
// A UNIQUE constraint binds a (label, property) PAIR, so a node comes under it by
// acquiring EITHER half. Every reservation call site was a property-set path —
// [CreateNode], [SetProperty], [SetAllProperties], the MERGE property paths — and
// none of them is reached by `SET n:Label`. The value-set therefore never learned
// that the node had joined the constrained label, and this committed clean:
//
//	CREATE CONSTRAINT person_email_uq ON (n:Person) ASSERT n.email IS UNIQUE
//	CREATE (a:Person {email:'dup@example.com'})
//	CREATE (b:Plain  {email:'dup@example.com'})   -- unconstrained: not a :Person
//	MATCH (b:Plain) SET b:Person                  -- accepted, committed
//
// leaving two :Person nodes sharing one supposedly unique email. No concurrency is
// involved; it is a plain Consistency violation on a single-threaded path.
//
// # Both directions are enforced, and the second one is the easy one to forget
//
// `REMOVE n:Label` takes the node back OUT of the constraint, so it must give the
// value back. Omitting that leaves a phantom reservation that refuses a later
// legitimate write of the same value for ever — the failure mode #1342 records and
// the one cypher/exec/constraint_journal.go warns about in as many words.
//
// # Two guards that are not optional
//
// Both are about a write that changes nothing:
//
//   - SET of a label the node ALREADY carries reserves nothing. Its value is
//     already in the value-set — reserved when the property was written — so
//     reserving again would report the node as a duplicate of ITSELF.
//   - REMOVE of a label the node does NOT carry releases nothing. The value-set is
//     keyed by (label, value) and not by node, so releasing on behalf of a node
//     that never held the label would hand away ANOTHER node's live reservation and
//     admit a genuine duplicate afterwards.
//
// # Null is not constrained
//
// A node with no value for the constrained property is outside the constraint
// entirely, so acquiring the label reserves nothing and any number of such nodes
// may carry the label together. This is the same rule [propertyValueToString]
// already applies on the property path (it reports no key for the zero
// PropertyValue) and that [ConstraintRegistry.SeedUniqueValues] applies at
// constraint-creation time. Requiring the property is the NOT NULL constraint's
// job, enforced separately at commit time (cypher/constraint_check.go).
//
// # The zero-constraint path must stay free
//
// [ConstraintRegistry.HasAnyUnique] is a lock-free atomic load and is the caller's
// gate. With it false NOTHING here runs: no registry lock, no graph read, no
// allocation. That is not a nicety — [ConstraintRegistry.uniqueActive] records
// that this registry's lock was 57 % of ALL lock delay at sixteen writers on a
// schema with no constraints at all, once rmp #2306 let writers overlap.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// txVisibleNodeReader is implemented by a mutator that can read a node's label
// membership and property values through its OWN write transaction's view.
//
// It exists because the mutator's ordinary [GraphMutator.NodeLabels] /
// [GraphMutator.NodeProperties] read the RAW graph at present time. GoGraph
// updates a stored value IN PLACE and keeps the inverse in the version chain, so
// an accessor that resolves no version returns the NEWEST value — another
// transaction's uncommitted work included. Deciding a constraint on that is the
// defect rmp #2350 fixed for the commit-time NOT NULL check, and the label path
// would reintroduce it: it reads the node's property to decide what to reserve,
// so a peer's unpublished property write would be reserved (or a peer's
// unpublished label write would suppress the reservation) on this transaction's
// behalf.
//
// It is an OPTIONAL interface asserted at the call site, following the shape
// [constraintJournal] established. The fallback is not a degradation: a mutator
// that does not implement it carries no transaction at all (the read-only and
// test stubs), and for such a caller the present IS its view.
type txVisibleNodeReader interface {
	// HasNodeLabelInTx reports whether n carries label in this transaction's view.
	HasNodeLabelInTx(n, label string) bool
	// NodePropertyInTx returns the value n carries under key in this
	// transaction's view, and whether it carries one at all.
	NodePropertyInTx(n, key string) (lpg.PropertyValue, bool)
	// NodeLabelsInTx returns n's FULL label set in this transaction's view.
	//
	// The label path needed only the single-label probe above, because it already
	// knows which label it is attaching. The PROPERTY path does not: it must ask
	// which constraints the node is currently subject to, which means enumerating
	// its labels — and enumerating them from the raw graph is the defect rmp #2355
	// fixed. See [labelsInTx].
	NodeLabelsInTx(n string) []string
}

// labelsInTx returns n's labels as THIS transaction sees them: its own eager writes
// included, no other transaction's unpublished work.
//
// # Why every constraint decision on the property path must come through here
//
// A UNIQUE constraint attaches to a LABEL, so deciding what to reserve or release
// for a property write starts by asking which labels the node carries. Asking the
// raw graph gets the newest stored value, which includes other in-flight
// transactions' eager writes — and conflicts are per SUBSTORE, so a peer writing the
// LABEL while this transaction writes the PROPERTY never collides. rmp #2355
// measured both halves of the consequence:
//
//	T1: REMOVE b:Person      (uncommitted; the raw bag now shows no :Person)
//	T2: SET b.email = 'new'  (raw read => "unconstrained" => NO reservation)
//	T1: ROLLBACK             (b is a :Person again)
//	=> a second :Person then took 'new' too, and 'old' was never released,
//	   leaving it reserved with no live node holding it.
//
// ONE helper rather than a corrected read at each of the ~26 decision sites,
// deliberately: the drift between sites is what left the property path behind when
// rmp #2352 hardened the label path, and a single choke point is what stops the next
// site from drifting again.
//
// The fallback is not a degradation. A mutator that does not implement
// [txVisibleNodeReader] carries no transaction at all — the read-only and test stubs
// — and for such a caller the present IS its view.
func labelsInTx(mut GraphMutator, n string) []string {
	if tv, ok := mut.(txVisibleNodeReader); ok {
		return tv.NodeLabelsInTx(n)
	}
	return mut.NodeLabels(n)
}

// nodeStateReader reads the label membership and property values the label
// constraint path needs, through the mutator's transaction view when it has one
// and through the mutator's plain accessors otherwise.
//
// The zero value is unusable; build one with [nodeStateReaderFor]. It is a value
// type carrying two words so a caller can hoist it out of a per-label loop
// without allocating.
type nodeStateReader struct {
	tx  txVisibleNodeReader // nil when the mutator carries no transaction
	mut GraphMutator
}

// nodeStateReaderFor binds a reader to mut, preferring its transaction view.
func nodeStateReaderFor(mut GraphMutator) nodeStateReader {
	tv, _ := mut.(txVisibleNodeReader)
	return nodeStateReader{tx: tv, mut: mut}
}

// hasLabel reports whether n carries label.
func (r nodeStateReader) hasLabel(n, label string) bool {
	if r.tx != nil {
		return r.tx.HasNodeLabelInTx(n, label)
	}
	for _, l := range r.mut.NodeLabels(n) {
		if l == label {
			return true
		}
	}
	return false
}

// property returns n's value for key, and whether it has one.
func (r nodeStateReader) property(n, key string) (lpg.PropertyValue, bool) {
	if r.tx != nil {
		return r.tx.NodePropertyInTx(n, key)
	}
	v, ok := r.mut.NodeProperties(n)[key]
	return v, ok
}

// reserveLabelUnique enforces every UNIQUE constraint that attaching label to
// nodeKey brings into play, reserving the node's current value for each
// constrained property so no concurrent or subsequent writer can take it.
//
// It returns a *ConstraintViolationError wrapping [ErrConstraintViolation] when
// the node's value for a constrained property is already held under that label.
// Each reservation is journaled through [reserveConstraintValue], so a rolled-back
// statement gives back exactly what it took.
//
// It MUST be called BEFORE [GraphMutator.SetNodeLabel]: the already-carries-it
// guard reads the node's labels, and after the write that guard would suppress
// the very check it is meant to admit.
//
// Callers gate on [ConstraintRegistry.HasAnyUnique]; reg must be non-nil.
func reserveLabelUnique(
	reg *ConstraintRegistry, mutator GraphMutator, mgr *index.Manager,
	rd nodeStateReader, nodeKey, label string,
) error {
	props := reg.UniqueProperties(label)
	if len(props) == 0 {
		return nil // this label is not part of any uniqueness constraint
	}
	if rd.hasLabel(nodeKey, label) {
		// Already a member: its values are already reserved under this label, so
		// re-reserving would reject the node as its own duplicate. This also makes
		// a repeated label in one statement (SET n:A:A) idempotent, because the
		// first iteration's write is visible to the second's read.
		return nil
	}
	for _, prop := range props {
		value, ok := rd.property(nodeKey, prop)
		if !ok || value.Kind() == 0 {
			continue // null: not constrained by UNIQUE
		}
		if err := reserveConstraintValue(reg, mutator, []string{label}, prop, value, mgr); err != nil {
			return err
		}
	}
	return nil
}

// releaseLabelUnique gives back every UNIQUE reservation that detaching label
// from nodeKey frees, so a later legitimate write of the same value is not
// refused by a phantom.
//
// Each release is journaled through [releaseConstraintValue], so a rolled-back
// REMOVE puts the reservation back.
//
// It MUST be called BEFORE [GraphMutator.RemoveNodeLabel]: it reads both the
// node's membership and its property values, and after the write neither is
// available to read.
//
// Callers gate on [ConstraintRegistry.HasAnyUnique]; reg must be non-nil.
func releaseLabelUnique(
	reg *ConstraintRegistry, mutator GraphMutator,
	rd nodeStateReader, nodeKey, label string,
) {
	props := reg.UniqueProperties(label)
	if len(props) == 0 {
		return // this label is not part of any uniqueness constraint
	}
	if !rd.hasLabel(nodeKey, label) {
		// Not a member: this REMOVE frees nothing. Releasing anyway would hand
		// away whichever OTHER node genuinely holds this value under this label.
		return
	}
	for _, prop := range props {
		value, ok := rd.property(nodeKey, prop)
		if !ok || value.Kind() == 0 {
			continue // null: nothing was ever reserved
		}
		releaseConstraintValue(reg, mutator, []string{label}, prop, value)
	}
}
