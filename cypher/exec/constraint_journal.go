package exec

// constraint_journal.go — a rolled-back statement releases exactly ITS OWN
// constraint reservations (rmp #2321, audit finding E10's remaining half).
//
// # What this replaces, and why the thing it replaces was wrong
//
// A UNIQUE constraint is enforced against a value-set on the [ConstraintRegistry]
// (see [ConstraintRegistry.ReserveSetProperty]). A statement that reserves a value
// and then rolls back must give it back, or the value is reserved for ever and every
// later write of it is rejected by a phantom (#1342).
//
// That used to be done by REBUILDING every value-set from the graph after the undo
// replay (`reseedConstraintsInsideBarrier`). Under one writer that is merely
// wasteful — O(N) over the whole mapper on an error path, which is audit finding
// E10. Under concurrent writers it is WRONG, and wrong in the direction that
// destroys the constraint:
//
//	writer A   reserve "alice" · create · commit ─── publishes at ts=7
//	writer B   reserve "alice" → VIOLATION · rollback
//	                             └─ rebuilds the value-set from a snapshot whose
//	                                frontier is still 6, so A's node is NOT THERE.
//	                                "alice" is now free again.
//	writer C   reserve "alice" → accepted. A second Alice exists.
//
// A clear-and-rebuild cannot preserve what it cannot see, and a writer's commit is
// deliberately invisible until it is durable. Measured: with the concurrent write
// path enabled, 8 concurrent MERGE under a UNIQUE constraint left 2 or 4 nodes in 20
// of 20 runs with the rebuild in place, and exactly 1 node in 12 of 12 runs with it
// disabled.
//
// So the reseed is gone and each statement journals its OWN registry changes as
// inverses on the transaction undo log — the same log that already restores the
// graph itself. Rollback then touches only this statement's reservations, in
// O(changes), and no other writer's state is observed at all.
//
// # Both directions are journaled, and the second one is easy to forget
//
// A SET releases the OLD value before reserving the new one. If only the reservation
// were journaled, a rolled-back SET would restore the property but leave the old
// value missing from the value-set — under-reporting, so a later genuine duplicate
// would be ACCEPTED. That gap existed before and was hidden by the rebuild, which
// restored both directions indiscriminately. [releaseConstraintValue] therefore
// journals the inverse of a release as well.

import (
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// constraintJournal is implemented by a mutator that can record an inverse for a
// constraint-registry change, so a rolled-back statement undoes it.
//
// It is an OPTIONAL interface, asserted at the call site, following the same shape
// as the count mutator: a read-only or stub mutator implements neither, and for such
// a caller there is no transaction to roll back, so having nowhere to journal is the
// correct answer rather than a missing capability.
type constraintJournal interface {
	// RecordConstraintInverse appends inv to the statement's undo log. inv must,
	// when invoked, return the constraint registry to the state it was in
	// immediately before the change being journaled.
	RecordConstraintInverse(inv func())
}

// constraintTxnHolder is implemented by a mutator that owns the transaction's
// UNCOMMITTED constraint contribution — the values it has released and not yet
// committed. See [ConstraintTxn] for why a release is deferred and a reservation is
// not.
//
// Optional for the same reason as [constraintJournal]: a read-only or stub mutator
// has no transaction, and for such a caller applying a release immediately is the
// correct answer rather than a missing capability.
type constraintTxnHolder interface {
	// ConstraintTxn returns the transaction's contribution, never nil.
	ConstraintTxn() *ConstraintTxn
}

// constraintTxnOf returns the transaction's constraint contribution, or nil when the
// mutator owns none.
func constraintTxnOf(mutator GraphMutator) *ConstraintTxn {
	h, ok := mutator.(constraintTxnHolder)
	if !ok {
		return nil
	}
	return h.ConstraintTxn()
}

// reserveConstraintValue validates and reserves value under every label, and
// journals the release so a rollback gives it back.
//
// It is the entry point every write operator uses. The reservation and the journal
// entry are ordered so that a failure leaves nothing behind: reg refuses without
// reserving, and nothing is journaled unless the reservation succeeded.
//
// reg may be nil (no constraints registered), in which case there is nothing to
// enforce and nothing to journal.
func reserveConstraintValue(
	reg *ConstraintRegistry, mutator GraphMutator,
	labels []string, prop string, value lpg.PropertyValue, mgr *index.Manager,
) error {
	if reg == nil {
		return nil
	}
	ct := constraintTxnOf(mutator)
	if err := reg.ReserveSetProperty(ct, labels, prop, value, mgr); err != nil {
		return err
	}
	ls := copyLabels(labels)
	journalConstraintInverse(mutator, func() {
		// A rolled-back RESERVATION is given back to the shared value-set directly,
		// and that stays correct: the value was reserved for the whole life of the
		// statement, so no peer can have committed it in the meantime and deleting it
		// cannot disturb anyone. Only the RELEASE direction had to be deferred; see
		// [ConstraintTxn]. Passing nil applies the delete immediately, which is what
		// an inverse must do.
		reg.ReleasePropertyValue(nil, ls, prop, value)
	})
	return nil
}

// releaseConstraintValue records that value is released under every label, and
// journals the withdrawal of that record so a rolled-back statement takes it back.
//
// Callers release the OLD value of a property they are about to overwrite or remove.
//
// # The release is DEFERRED, and the inverse is now PRIVATE (rmp #2366)
//
// Both halves used to touch the SHARED value-set: the release deleted the value, and
// the journaled inverse re-inserted it. The inverse is the defect. It judges the
// re-insertion against the rolling-back transaction's own view, while a PEER's
// COMMITTED release may already have freed the same value — so the value came back
// reserved with no live node holding it, permanently unusable. The file comment above
// explains why rebuilding from the graph is not the answer either.
//
// So the release is recorded on the transaction ([ConstraintTxn]) and applied to the
// shared set only by [ConstraintRegistry.CommitTxn]. The inverse then withdraws a
// PRIVATE mark, which no peer can observe and which therefore needs no ordering
// against a peer's commit. A statement that rolls back inside a still-live
// transaction unwinds exactly its own contribution, and a whole-transaction rollback
// unwinds all of it, by the same mechanism.
//
// With no transaction (ct nil) the release applies immediately and there is nothing
// to journal, because there is nothing that can roll back.
func releaseConstraintValue(
	reg *ConstraintRegistry, mutator GraphMutator,
	labels []string, prop string, value lpg.PropertyValue,
) {
	if reg == nil {
		return
	}
	ct := constraintTxnOf(mutator)
	reg.ReleasePropertyValue(ct, labels, prop, value)
	if ct == nil {
		return
	}
	ls := copyLabels(labels)
	journalConstraintInverse(mutator, func() {
		for _, label := range ls {
			ct.unmarkReleased(constraintKey(label, prop), valueSetKeyOf(value))
		}
	})
}

// valueSetKeyOf is the value-set's string form of value, or the empty string when the
// value is not one UNIQUE constrains. A release of an unconstrained value records no
// mark, so withdrawing one is a no-op on a key that was never written.
func valueSetKeyOf(value lpg.PropertyValue) string {
	s, _ := propertyValueToString(value)
	return s
}

// journalConstraintInverse records inv on the statement's undo log when the mutator
// keeps one.
//
// The caller copies the labels slice before building inv ([copyLabels]), because a
// caller's slice is frequently a reference into per-row state that the next row
// overwrites. A captured alias would make the inverse release a value under whatever
// labels the last row happened to carry, which is a silent wrong-key release — the
// kind of defect that only shows up as a phantom reservation much later.
func journalConstraintInverse(mutator GraphMutator, inv func()) {
	j, ok := mutator.(constraintJournal)
	if !ok {
		return
	}
	j.RecordConstraintInverse(inv)
}

// isConstraintViolation reports whether err is (or wraps) a constraint violation.
//
// It exists because enforcement moved INSIDE the mutator (rmp #2358): a write
// method now returns either a genuine write failure, which its caller wraps with
// operator context, or a violation, which must reach the caller UNWRAPPED so
// errors.As can recover the typed *ConstraintViolationError the Bolt layer reports.
// Wrapping a violation in an operator's own message is not cosmetic — it changes the
// error TYPE a driver sees, and the TCK asserts on that type.
func isConstraintViolation(err error) bool {
	return errors.Is(err, ErrConstraintViolation)
}

// copyLabels returns a defensive copy of labels for capture in a closure that
// outlives the caller's row. See [journalConstraintInverse] for why.
func copyLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, len(labels))
	copy(out, labels)
	return out
}
