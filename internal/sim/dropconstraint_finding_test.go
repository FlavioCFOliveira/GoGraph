package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestRegression_DropConstraintByNameDropsConstraint is the regression guard for
// the DST SchemaChanger finding (#1552, fixed in #1556): `DROP CONSTRAINT
// <name>` (by name only) used to report SUCCESS while performing NO schema
// change — the constraint stayed enforced and its "__uniq__<Label>.<prop>"
// backing index lingered, permanently blocking re-creation of any UNIQUE
// constraint on the same (label, property). That was a fail-silent Consistency
// violation.
//
// The fix resolves the constraint NAME to its (kind, label, property) identity
// via the registry, then drops both the constraint and its backing index
// atomically. This test pins the CORRECT behaviour: after a by-name drop the
// constraint is genuinely gone (a duplicate is accepted) and the same UNIQUE
// constraint can be re-created cleanly.
func TestRegression_DropConstraintByNameDropsConstraint(t *testing.T) {
	t.Parallel()
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	runOK := func(q string) {
		t.Helper()
		if _, err := c.Run(q, nil); err != nil {
			t.Fatalf("RUN %q: %v", q, err)
		}
		if _, term, err := c.PullAll(); err != nil {
			t.Fatalf("PULL %q: %v", q, err)
		} else if f, ok := term.(*proto.Failure); ok {
			t.Fatalf("%q unexpectedly FAILED: %s %s", q, f.Code, f.Message)
		}
	}
	isRejected := func(q string) bool {
		t.Helper()
		resp, err := c.Run(q, nil)
		if err != nil {
			t.Fatalf("RUN %q: %v", q, err)
		}
		if _, ok := resp.(*proto.Failure); ok {
			_, _ = c.Reset()
			return true
		}
		_, term, err := c.PullAll()
		if err != nil {
			t.Fatalf("PULL %q: %v", q, err)
		}
		if _, ok := term.(*proto.Failure); ok {
			_, _ = c.Reset()
			return true
		}
		return false
	}

	runOK("CREATE CONSTRAINT find_c ON (a:FindAcct) ASSERT a.email IS UNIQUE")
	runOK("CREATE (a:FindAcct {email:'a@x'})")
	// While the constraint is live the duplicate must be rejected.
	if !isRejected("CREATE (a:FindAcct {email:'a@x'})") {
		t.Fatal("duplicate was accepted while the UNIQUE constraint is still live")
	}

	// DROP by name reports success AND actually removes enforcement.
	runOK("DROP CONSTRAINT find_c IF EXISTS")

	// The constraint is gone: the duplicate is now ACCEPTED.
	if isRejected("CREATE (a:FindAcct {email:'a@x'})") {
		t.Fatal("DROP CONSTRAINT by-name did not remove enforcement — the #1556 defect has regressed")
	}

	// The "__uniq__" backing index is gone too, so the same UNIQUE constraint
	// can be re-created cleanly (this used to fail with "an index by that name
	// already exists").
	runOK("DROP CONSTRAINT find_c IF EXISTS")          // tolerate the now-duplicated data before re-create
	runOK("MATCH (a:FindAcct {email:'a@x'}) DELETE a") // remove duplicates so re-create's pre-existing check passes
	runOK("CREATE CONSTRAINT find_c ON (a:FindAcct) ASSERT a.email IS UNIQUE")

	// And it enforces again after re-creation.
	runOK("CREATE (a:FindAcct {email:'a@x'})")
	if !isRejected("CREATE (a:FindAcct {email:'a@x'})") {
		t.Fatal("re-created UNIQUE constraint does not enforce")
	}
}
