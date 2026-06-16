package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestFinding_DropConstraintByNameIsFailSilent documents a robustness defect the
// DST SchemaChanger (#1552) surfaced in the engine's DROP CONSTRAINT path, with
// a minimal reproducer. It is NOT a fix — the defect needs an engine change that
// materially widens this task's scope, so it is reported for a decision rather
// than fixed blindly. The test pins the CURRENT (incorrect) behaviour so the
// eventual fix is forced to update it.
//
// Defect: `DROP CONSTRAINT <name>` (by name only) reports SUCCESS but performs
// NO schema change. The IR for a by-name drop carries an empty label/property
// (a documented IR limitation), so the executor cannot resolve the UNIQUE
// backing index ("__uniq__Label.prop"); with IF EXISTS it then silently
// "succeeds" without dropping the index or unregistering the constraint. Two
// consequences, both observable below:
//
//  1. CONSISTENCY / fail-silent: the constraint stays fully enforced after a
//     DROP that acknowledged success — the client believes it is gone but a
//     duplicate is still rejected. This violates the module's fail-stop,
//     never-fail-silent mandate.
//  2. The orphaned backing index permanently blocks re-creating any UNIQUE
//     constraint on the same (label, property): a later CREATE fails with
//     "an index by that name already exists".
//
// Reproducer (also runnable over the wire): CREATE → DROP (reports OK) →
// duplicate still rejected; re-CREATE fails.
func TestFinding_DropConstraintByNameIsFailSilent(t *testing.T) {
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
	// DROP by name reports success.
	runOK("DROP CONSTRAINT find_c IF EXISTS")

	// CURRENT (defective) behaviour: the constraint is STILL enforced after the
	// "successful" drop. When the engine is fixed so DROP CONSTRAINT <name>
	// actually drops the backing index + registry entry, the duplicate will be
	// ACCEPTED and this expectation must flip to false.
	if !isRejected("CREATE (a:FindAcct {email:'a@x'})") {
		t.Log("DROP CONSTRAINT by-name now removes enforcement — the documented defect is FIXED; update this test and the SchemaChanger notes")
		t.Fail()
	}
}
