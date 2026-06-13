package server

// client_fault_classification_test.go — regression gate for task #1435.
//
// Two client-fault conditions were previously mis-surfaced:
//
//	(a) An unsupported parameter type made BindParams return a plain error with
//	    no TCK category, so FailureCode fell through to the SERVER-fault code
//	    Neo.DatabaseError.General.UnknownError and sanitiseErr masked the
//	    message to "An internal error occurred" — telling a legitimate driver
//	    the server had an internal bug.
//	(b) A malformed PackStream frame got the correct client code
//	    (Neo.ClientError.Request.Invalid) but its message was masked to the
//	    same internal-error text (self-contradictory).
//
// After the fix:
//	(a) BindParams wraps cypher.ErrUnsupportedParamType; FailureCode maps it to
//	    Neo.ClientError.Statement.TypeError and sanitiseErr forwards the
//	    (non-sensitive) message naming the offending Go type.
//	(b) The decode path emits a fixed, honest "malformed Bolt message" string.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// internalErrPrefix is the leading text of the generic internal-error message
// produced by sanitiseErr for genuine server-fault errors.
const internalErrPrefix = "An internal error occurred"

// TestFailureCode_UnsupportedParamType maps a wrapped
// cypher.ErrUnsupportedParamType to the client-side type-error code.
func TestFailureCode_UnsupportedParamType(t *testing.T) {
	t.Parallel()
	// BindParams returns a value wrapping ErrUnsupportedParamType for a Go type
	// it cannot convert (here a struct value).
	_, err := cypher.BindParams(map[string]any{"p": struct{ X int }{X: 1}})
	if err == nil {
		t.Fatal("BindParams: expected error for unsupported param type, got nil")
	}
	if got := FailureCode(err); got != "Neo.ClientError.Statement.TypeError" {
		t.Fatalf("FailureCode = %q, want Neo.ClientError.Statement.TypeError", got)
	}
	// The message must be forwarded (client fault), not masked.
	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
	msg := sess.sanitiseErr(err)
	if strings.HasPrefix(msg, internalErrPrefix) {
		t.Fatalf("sanitiseErr masked a client-fault message: %q", msg)
	}
	if !strings.Contains(msg, "unsupported parameter type") {
		t.Fatalf("sanitiseErr message = %q, want it to name the unsupported type", msg)
	}
}

// TestHandleRun_UnsupportedParamType_ClientError drives a RUN whose parameter
// map carries an unsupported Go type through a session and asserts the FAILURE
// is a Neo.ClientError.* code with a descriptive (non-internal-error) message.
func TestHandleRun_UnsupportedParamType_ClientError(t *testing.T) {
	t.Parallel()
	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")

	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("pre-condition: state after HELLO = %v, want READY", sess.state)
	}

	// A channel value cannot be bound to an expr.Value; it stands in for a Bolt
	// Point/temporal Struct param the engine does not accept.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "RETURN $p AS p",
		Parameters: map[string]interface{}{"p": make(chan int)},
		Extra:      map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("RUN: unexpected transport error: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("RUN: no response messages")
	}
	failure, ok := msgs[0].(*proto.Failure)
	if !ok {
		t.Fatalf("RUN: expected *proto.Failure, got %T", msgs[0])
	}
	if !strings.HasPrefix(failure.Code, "Neo.ClientError.") {
		t.Errorf("FAILURE code = %q, want a Neo.ClientError.* code", failure.Code)
	}
	if failure.Code != "Neo.ClientError.Statement.TypeError" {
		t.Errorf("FAILURE code = %q, want Neo.ClientError.Statement.TypeError", failure.Code)
	}
	if strings.HasPrefix(failure.Message, internalErrPrefix) {
		t.Errorf("FAILURE message masked to internal-error text: %q", failure.Message)
	}
	if failure.Message == "" {
		t.Error("FAILURE message is empty; want a descriptive client diagnostic")
	}
}
