package exec

// merge_unique_race.go — telling a MERGE creation RACE apart from a genuine
// UNIQUE violation (rmp #2987).
//
// MERGE fires its create branch only when its search found no node matching the
// whole pattern in the transaction's snapshot. When the create then fails because
// a UNIQUE value-set already holds one of the pattern's own key values, there are
// exactly two possibilities:
//
//   - a node the transaction CAN see holds the value — the pattern differs from
//     it in some other label or property. The violation is deterministic: the
//     statement fails the same way however often it runs.
//   - no visible node holds it. The holder is a concurrent transaction that
//     either committed after this snapshot was taken or is still in flight; it
//     reserved the value eagerly, which is what refused this one. That is the
//     creation race of concurrent MERGE, and the statement re-run on a snapshot
//     that sees the holder's outcome MATCHES the winner (or creates, if the holder
//     aborted).
//
// This file marks the second case. It decides nothing about retrying: whether a
// statement may be re-run is a property of its transaction boundary, which only
// the cypher package knows (an autocommit statement can be; one statement of an
// explicit transaction cannot, because its snapshot is the transaction's).
//
// The marked error keeps its whole chain: its message is the violation's, and
// errors.Is(err, ErrConstraintViolation) and errors.As(err, **ConstraintViolationError)
// both still hold. A caller that does not ask whether it is a race sees exactly
// the violation it always saw.

import (
	"context"
	"errors"
	"slices"
)

// mergeUniqueRaceError marks a UNIQUE violation raised by a MERGE's own create
// branch whose conflicting value no node visible to the statement's transaction
// holds. See the file comment for why that makes it a race and not a violation.
//
// It is detected outside this package through its MergeUniqueRace method, by an
// interface assertion, so the marker adds no identifier to the package API.
type mergeUniqueRaceError struct{ cause error }

func (e *mergeUniqueRaceError) Error() string { return e.cause.Error() }

func (e *mergeUniqueRaceError) Unwrap() error { return e.cause }

// MergeUniqueRace reports that the violation is a MERGE creation race against a
// holder the statement's snapshot cannot see. It is the method the cypher
// package's autocommit retry asserts for.
func (e *mergeUniqueRaceError) MergeUniqueRace() bool { return true }

// classifyMergeUniqueViolation returns err marked as a creation race when it is a
// UNIQUE violation on (one of nodeLabels, p.key) and no node visible to mut's
// transaction carries that label with a value equal to p.value; otherwise it
// returns err unchanged.
//
// It runs only on the failure path of a MERGE create, so a MERGE that succeeds
// pays nothing for it. The visibility probe is the MERGE search itself, narrowed
// to the one (label, key, value) the violation names, so it reads exactly what
// the statement's own match phase reads: the transaction's view, through the
// label posting list or the property index when one can prove its answer
// complete. A probe error leaves err unmarked — the conservative answer, which
// surfaces the violation rather than retrying on a premise not established.
func classifyMergeUniqueViolation(ctx context.Context, mut GraphMutator, src MergeLabelSource, probe *mergeProbeSlot, nodeLabels []string, p propLiteral, err error) error {
	var cv *ConstraintViolationError
	if !errors.As(err, &cv) || cv.Kind != "UNIQUE" || cv.Property != p.key || !slices.Contains(nodeLabels, cv.Label) {
		return err
	}
	// A fresh slot: the operator's own one has resolved its (label, key) choice
	// for the whole pattern, and this search names a single key.
	var slot *mergeProbeSlot
	if probe != nil {
		slot = newMergeProbeSlot(probe.prober)
	}
	rows, serr := searchMergeNodes(ctx, mut, src, slot, []string{cv.Label}, []propLiteral{p})
	if serr != nil || len(rows) > 0 {
		return err
	}
	return &mergeUniqueRaceError{cause: err}
}
