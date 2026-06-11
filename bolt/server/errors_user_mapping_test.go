package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// runToFailure drives query through the real session message path (HELLO
// already done by the caller): RUN, and when RUN succeeds, PULL —
// returning the first FAILURE encountered. Errors that surface during
// materialisation/commit inside the engine are reported by RUN; errors that
// surface while draining the cursor are reported by PULL. Fails the test when
// neither message yields a FAILURE.
func runToFailure(t *testing.T, sess *Session, query string) *proto.Failure {
	t.Helper()
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{Query: query})
	if err != nil {
		t.Fatalf("HandleMessage(RUN %q): %v", query, err)
	}
	if f, ok := msgs[0].(*proto.Failure); ok {
		return f
	}
	msgs, err = sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("HandleMessage(PULL after %q): %v", query, err)
	}
	for _, m := range msgs {
		if f, ok := m.(*proto.Failure); ok {
			return f
		}
	}
	t.Fatalf("query %q: no FAILURE from RUN or PULL", query)
	return nil
}

// newReadySessionFor returns a session over eng that has completed HELLO and
// is in StateReady.
func newReadySessionFor(t *testing.T, eng *cypher.Engine) *Session {
	t.Helper()
	sess := newSession(eng, NoAuthHandler{}, "")
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("pre-condition: want READY after HELLO, got %v", sess.state)
	}
	return sess
}

// TestFailure_UserErrorMapping is the regression gate for task #1353: every
// user-condition error class the cypher engine actually produces must surface
// over Bolt with the correct Neo4j status-code family (ClientError, so drivers
// do not treat a user mistake as a server fault) AND with the engine's own
// diagnostic message — never the genericised "An internal error occurred"
// text. One representative scenario per engine error class, driven through
// the real Session.HandleMessage RUN/PULL path (the audit's repro shape).
//
// Status codes follow the official Neo4j taxonomy
// (https://neo4j.com/docs/status-codes/current/):
//
//   - parse error            → Neo.ClientError.Statement.SyntaxError
//   - semantic (scope) error → Neo.ClientError.Statement.SyntaxError —
//     the engine's sema pass tags UndefinedVariable with the TCK-pinned
//     category "SyntaxError" (cypher/sema kindMappings), matching Neo4j,
//     which also raises SyntaxError for undefined variables.
//   - runtime type error     → Neo.ClientError.Statement.TypeError
//   - constraint violation   → Neo.ClientError.Schema.ConstraintValidationFailed
//   - result-row cap         → Neo.ClientError.General.LimitExceeded (#1293)
//   - transaction too large  → Neo.ClientError.General.TransactionOutOfMemoryError
func TestFailure_UserErrorMapping(t *testing.T) {
	t.Parallel()

	// Shared in-memory engine for the stateless classes. The constraint class
	// seeds its state directly through the engine (not the session) so the
	// session stays in READY for the single failing RUN.
	newEngine := func(t *testing.T) *cypher.Engine {
		t.Helper()
		return cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	}

	t.Run("parse_error", func(t *testing.T) {
		t.Parallel()
		f := runToFailure(t, newReadySessionFor(t, newEngine(t)), "RETURN (")
		assertFailure(t, f, "Neo.ClientError.Statement.SyntaxError", "parse error at")
	})

	t.Run("semantic_error", func(t *testing.T) {
		t.Parallel()
		f := runToFailure(t, newReadySessionFor(t, newEngine(t)), "RETURN undefinedVar")
		assertFailure(t, f, "Neo.ClientError.Statement.SyntaxError", `undefined variable "undefinedVar"`)
	})

	t.Run("runtime_type_error", func(t *testing.T) {
		t.Parallel()
		f := runToFailure(t, newReadySessionFor(t, newEngine(t)), "WITH [1,2] AS l RETURN l[1.5]")
		assertFailure(t, f, "Neo.ClientError.Statement.TypeError", "InvalidArgumentType: list index must be Integer")
	})

	t.Run("constraint_violation", func(t *testing.T) {
		t.Parallel()
		eng := newEngine(t)
		for _, q := range []string{
			"CREATE CONSTRAINT ON (p:Person) ASSERT p.name IS UNIQUE",
			"CREATE (:Person {name: 'a'})",
		} {
			res, err := eng.RunInTxAny(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("seed %q: %v", q, err)
			}
			_ = res.Close()
		}
		f := runToFailure(t, newReadySessionFor(t, eng), "CREATE (:Person {name: 'a'})")
		assertFailure(t, f, "Neo.ClientError.Schema.ConstraintValidationFailed", "UNIQUE constraint on (Person).name")
	})

	t.Run("result_row_cap", func(t *testing.T) {
		t.Parallel()
		eng := cypher.NewEngineWithOptions(
			lpg.New[string, float64](adjlist.Config{}),
			cypher.EngineOptions{MaxResultRows: 1},
		)
		f := runToFailure(t, newReadySessionFor(t, eng), "UNWIND [1,2,3] AS x RETURN x")
		assertFailure(t, f, "Neo.ClientError.General.LimitExceeded", "result row limit exceeded")
	})

	t.Run("transaction_too_large", func(t *testing.T) {
		t.Parallel()
		w, err := wal.Open(filepath.Join(t.TempDir(), "gate.wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		t.Cleanup(func() { _ = w.Close() })
		g := lpg.New[string, float64](adjlist.Config{})
		st := txn.NewStoreWithCodecCapped[string, float64](g, w, txn.NewStringCodec(), 1)
		eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{Store: st})
		f := runToFailure(t, newReadySessionFor(t, eng), "CREATE (:A {x: 1}), (:B {y: 2})")
		assertFailure(t, f, "Neo.ClientError.General.TransactionOutOfMemoryError", "transaction exceeds the per-transaction op cap")
	})
}

// assertFailure checks the FAILURE's status code and that its message keeps
// the engine's faithful diagnostic (wantMsg substring) instead of the
// genericised internal-error text.
func assertFailure(t *testing.T, f *proto.Failure, wantCode, wantMsg string) {
	t.Helper()
	if f.Code != wantCode {
		t.Errorf("failure code: got %q, want %q", f.Code, wantCode)
	}
	if !strings.Contains(f.Message, wantMsg) {
		t.Errorf("failure message: got %q, want substring %q", f.Message, wantMsg)
	}
	if strings.Contains(f.Message, "An internal error occurred") {
		t.Errorf("failure message was genericised: %q", f.Message)
	}
}
