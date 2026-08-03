package server

// serialization_conflict_test.go — the MVCC write-write conflict's path to a
// Bolt client (rmp #2300).
//
// The claim under test is not "the code looks retriable". It is "the OFFICIAL
// DRIVER retries it", and the only witness that settles that is the driver's own
// classifier, so these tests call it rather than reasoning about the string.
//
// They also assert the NEGATIVE, and that half is the point. The driver's
// db.Neo4jError.reclassify() rewrites two codes out of the TransientError family
// BEFORE the classification is parsed:
//
//	Neo.TransientError.Transaction.LockClientStopped -> Neo.ClientError...
//	Neo.TransientError.Transaction.Terminated        -> Neo.ClientError...
//
// So a perfectly plausible-looking choice is silently demoted and never retried.
// A test that only checked the code we picked would go green against that
// mistake too.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/db"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// conflictCode is the code the server maps a serialization conflict to. It is
// spelled out here rather than read from FailureCode so a change to the mapping
// has to change this line too, in a file that explains what the choice depends
// on.
const conflictCode = "Neo.TransientError.Transaction.Outdated"

// TestFailureCode_SerializationConflict pins the mapping itself, through every
// wrapping the error travels under.
func TestFailureCode_SerializationConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"the sentinel", mvcc.ErrSerializationConflict},
		{"a typed conflict", mvcc.NewConflict("node labels", 1<<63, 7, 1<<63+1)},
		{"wrapped once", fmt.Errorf("cypher: exec: %w", mvcc.ErrSerializationConflict)},
		{
			"wrapped twice around the typed form",
			fmt.Errorf("cypher: commit: %w",
				fmt.Errorf("lpg: %w", mvcc.NewConflict("adjacency", 1<<63, 7, 1<<63+1))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FailureCode(tc.err); got != conflictCode {
				t.Fatalf("FailureCode = %q, want %q", got, conflictCode)
			}
		})
	}
}

// TestFailureCode_SerializationConflictIsRetriedByTheRealDriver asks the official
// driver whether it would retry what the server sends.
//
// [neo4j.IsRetryable] is the exported entry point to the same classifier
// session.ExecuteWrite/ExecuteRead consult on every attempt
// (neo4j/error.go -> internal/retry.IsRetryable -> db.Neo4jError.IsRetriable),
// and db.Neo4jError is the type the driver builds from a Bolt FAILURE message —
// so constructing one with the server's code and asking the driver is the same
// question the managed transaction asks, without needing a real collision to
// reach the wire.
func TestFailureCode_SerializationConflictIsRetriedByTheRealDriver(t *testing.T) {
	t.Parallel()

	code := FailureCode(mvcc.ErrSerializationConflict)
	asDriverSaw := &db.Neo4jError{Code: code, Msg: mvcc.ErrSerializationConflict.Error()}
	if !neo4j.IsRetryable(asDriverSaw) {
		t.Fatalf("the driver would NOT retry %q, so a managed transaction reports the "+
			"conflict to the application instead of retrying it — which is the whole "+
			"point of surfacing it as transient", code)
	}

	// The driver's own reason, restated so a failure above says WHY.
	if got := asDriverSaw.Classification(); got != "TransientError" {
		t.Fatalf("the driver classifies %q as %q, want TransientError", code, got)
	}
}

// TestFailureCode_DemotedTransientCodesAreNotRetriable is the negative control,
// and it is what makes the positive test above mean something.
//
// These two codes LOOK like the right family and are not. If a future edit moves
// the conflict onto one of them — Terminated in particular reads as a natural fit
// — this test is what refuses it.
func TestFailureCode_DemotedTransientCodesAreNotRetriable(t *testing.T) {
	t.Parallel()
	demoted := []string{
		"Neo.TransientError.Transaction.Terminated",
		"Neo.TransientError.Transaction.LockClientStopped",
	}
	for _, code := range demoted {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			if neo4j.IsRetryable(&db.Neo4jError{Code: code}) {
				t.Fatalf("the driver retries %q; this test encodes the opposite, so either "+
					"the driver's reclassify() changed or this expectation is stale — "+
					"re-read neo4j/db/errors.go before touching the conflict mapping", code)
			}
			if code == conflictCode {
				t.Fatalf("the conflict is mapped to %q, which the driver demotes out of the "+
					"TransientError family and never retries", code)
			}
		})
	}
}

// TestSanitiseErr_ForwardsTheConflictMessage covers the second half of the
// plumbing: a conflict is neither a client fault nor an internal error, so
// neither of sanitiseErr's existing branches gives the right answer.
//
// Without the explicit case the client would receive "An internal error
// occurred", which is wrong twice over — it is not internal, and it hides the one
// fact an operator reading driver logs wants.
func TestSanitiseErr_ForwardsTheConflictMessage(t *testing.T) {
	t.Parallel()
	s := &Session{id: "test-session"}

	conflict := mvcc.NewConflict("node properties", 1<<63, 7, 1<<63+1)
	got := s.sanitiseErr(conflict)
	if strings.Contains(got, "An internal error occurred") {
		t.Fatalf("a serialization conflict was sanitised to the internal-error text (%q): a "+
			"retriable collision is not an internal fault", got)
	}
	if got != conflict.Error() {
		t.Fatalf("sanitiseErr = %q, want the conflict's own message %q", got, conflict.Error())
	}
	// And it discloses nothing internal: no timestamps, no transaction ids.
	for _, leak := range []string{"9223372036854775808", "7", "startTS", "txID"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the forwarded message contains %q: %q", leak, got)
		}
	}
}

// TestFailureCode_ConflictIsNotClassifiedAsAClientFault guards the interaction
// between the two halves: isClientFaultErr derives from FailureCode, and a
// TransientError code must not be treated as a client fault, or the sanitiser's
// disclosure rule would be decided by the wrong branch.
func TestFailureCode_ConflictIsNotClassifiedAsAClientFault(t *testing.T) {
	t.Parallel()
	if isClientFaultErr(mvcc.ErrSerializationConflict) {
		t.Fatal("a serialization conflict is reported as a client fault; it is a transient " +
			"collision, and the two must not share a classification")
	}
	if !errors.Is(mvcc.NewConflict("adjacency", 1<<63, 7, 1<<63+1), mvcc.ErrSerializationConflict) {
		t.Fatal("the typed conflict no longer unwraps to the sentinel, so every errors.Is " +
			"call site in the Bolt layer silently stops matching")
	}
}
