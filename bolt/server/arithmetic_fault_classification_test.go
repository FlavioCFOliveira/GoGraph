package server

// arithmetic_fault_classification_test.go — regression gate for the 2026-06-25
// round-2 audit (#1765, #1766, #1768): a runtime arithmetic / argument fault is
// a deterministic CLIENT condition, not a server fault. Its *expr.EvalError must
// map to a Neo.ClientError.Statement.* code so isClientFaultErr is true and the
// real message is forwarded — not replaced with generic internal-error text.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestFailureCode_ArithmeticAndArgumentFaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"overflow", "ArithmeticOverflow: integer overflow in 9223372036854775807 + 1", "Neo.ClientError.Statement.ArithmeticError"},
		{"div_by_zero", "ArithmeticError: / by zero", "Neo.ClientError.Statement.ArithmeticError"},
		{"mod_by_zero", "ArithmeticError: % by zero", "Neo.ClientError.Statement.ArithmeticError"},
		{"argument", "ArgumentError: substring: negative start index -1", "Neo.ClientError.Statement.ArgumentError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := &expr.EvalError{Msg: tc.msg}
			if got := FailureCode(err); got != tc.want {
				t.Fatalf("FailureCode = %q, want %q", got, tc.want)
			}
			if !isClientFaultErr(err) {
				t.Fatalf("isClientFaultErr = false, want true (a client-fault message must be forwarded)")
			}
			// The real message must be forwarded, not masked as an internal error.
			sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
			if msg := sess.sanitiseErr(err); strings.HasPrefix(msg, internalErrPrefix) {
				t.Fatalf("sanitiseErr masked a client-fault message: %q", msg)
			}
		})
	}
}
