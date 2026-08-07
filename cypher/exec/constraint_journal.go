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
	if err := reg.ReserveSetProperty(labels, prop, value, mgr); err != nil {
		return err
	}
	ls := copyLabels(labels)
	journalConstraintInverse(mutator, func() {
		reg.ReleasePropertyValue(ls, prop, value)
	})
	return nil
}

// releaseConstraintValue drops value from the value-set under every label, and
// journals the re-record so a rollback puts it back.
//
// Callers release the OLD value of a property they are about to overwrite or remove.
// The journal entry is what stops a rolled-back statement from leaving the value-set
// under-reporting — see the file comment.
func releaseConstraintValue(
	reg *ConstraintRegistry, mutator GraphMutator,
	labels []string, prop string, value lpg.PropertyValue,
) {
	if reg == nil {
		return
	}
	reg.ReleasePropertyValue(labels, prop, value)
	ls := copyLabels(labels)
	journalConstraintInverse(mutator, func() {
		reg.RecordPropertySet(ls, prop, value)
	})
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
