package cypher

// constraint_notnull_committed_test.go — rmp #2798 gate: CREATE CONSTRAINT ...
// IS NOT NULL is validated against COMMITTED state as well as live state, and
// registers only when NEITHER violates.
//
// # The defect these tests pin
//
// [Engine.scanLabelProperty] read the LIVE property bag, which carries an
// explicit transaction's EAGER, UNCOMMITTED mutations. An open transaction that
// had eagerly FILLED the property therefore made a violating node look
// compliant: the DDL succeeded, and the rollback then left a committed node with
// no value under an ACTIVE NOT NULL constraint. Measured on the pre-fix build,
// ONE goroutine and no concurrency at all:
//
//	open tx: eagerly SET n.email = 'x@x'   (never committed)
//	CREATE CONSTRAINT ... IS NOT NULL  ->  err = <nil>, registered = true
//	rollback
//	n.email present = false                <- ACTIVE constraint, violating node
//
// That is an ACID Consistency breach, and it is rmp #2778's and rmp #2792's
// defect from the other side. It is not a wider version of rmp #2792's fix: NOT
// NULL has no backing index and no value-set, so there is no second structure to
// seed from the same read. It validates by scan and registers.
//
// # THE MIRROR IS REAL, AND BOTH SINGLE-VIEW DESIGNS ARE UNSOUND
//
// rmp #2792 measured that its UNIQUE validation must keep reading LIVE, because a
// snapshot cannot see an open transaction's eager DUPLICATE. NOT NULL looked like
// the mirror — the hazard here is an eager FILL, which a snapshot WOULD catch —
// so the tempting fix is to move the scan to a snapshot. It was measured, and it
// trades one Consistency breach for another. With validation moved to the snapshot
// ALONE, on this build:
//
//	committed: every :Person carries email
//	open tx: eagerly REMOVE n.email      (a compliant node made to look violating)
//	CREATE CONSTRAINT ... IS NOT NULL  ->  err = <nil>, registered = true
//	COMMIT                             ->  err = <nil>              <- ACCEPTED
//	n.email present = false                <- ACTIVE constraint, violating node
//
// Nothing refuses that commit, for the same SHAPE of reason as UNIQUE's
// write-time reservation: [Engine.BeginTx] allocates the transaction's
// touched-node set only when a NOT NULL constraint is ALREADY active, so a
// transaction that began before the constraint existed has none and its
// commit-time [touchedNodes.checkNotNullConstraints] is a no-op.
//
// So the answer is not "snapshot instead of live", it is BOTH: the DDL is
// refused when the COMMITTED state violates (which the live scan cannot see) OR
// when the LIVE state violates (which a snapshot cannot see). The refusal is
// conservative in the direction where a conservative answer is merely unhelpful
// — a DDL refused because an open transaction happens to be holding an eager
// null — and never in the direction where it would be a breach.
//
// # What the oracle is
//
// The committed-state assertion is read back through a predicate-free projection
// (`MATCH (n:Person) RETURN n.tag, n.email`), so the structure under test is
// never the thing reporting whether it kept its promise. NOT NULL has no backing
// index, so no index can serve that projection either.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// notNullDDL is the statement under test on every arm below.
const notNullDDL = `CREATE CONSTRAINT c_email FOR (n:Person) REQUIRE n.email IS NOT NULL`

// notNullViolationMsg is the fragment validatePreExisting produces when the
// pre-existing data fails the NOT NULL check. Asserting it stops an arm passing
// on an unrelated refusal (a name clash, a cancelled context).
const notNullViolationMsg = "pre-existing node has a null value"

// notNullEngine seeds :Person nodes from tag -> email, where an empty email
// means the node carries NO email property at all, plus one :Other node "o1"
// with no email for the add-label shape. The engine has no constraint yet.
func notNullEngine(tb testing.TB, emails map[string]string) *Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for key, email := range emails {
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("seed label %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "tag", lpg.StringValue(key)); err != nil {
			tb.Fatalf("seed tag %s: %v", key, err)
		}
		if email == "" {
			continue
		}
		if err := g.SetNodeProperty(key, "email", lpg.StringValue(email)); err != nil {
			tb.Fatalf("seed email %s: %v", key, err)
		}
	}
	if err := g.SetNodeLabel("o1", "Other"); err != nil {
		tb.Fatalf("seed label o1: %v", err)
	}
	if err := g.SetNodeProperty("o1", "tag", lpg.StringValue("o1")); err != nil {
		tb.Fatalf("seed tag o1: %v", err)
	}
	e := NewEngine(g)
	e.parallelBackfillEnabled = false
	return e
}

// notNullFixtures are the two committed graphs every arm starts from. Nothing
// distinguishes them but whether k2 carries an email, which is the whole
// variable: the direction-A arms hide k2's absent email from the live view, and
// the direction-B arms manufacture an absent email the committed state does not
// have.
func notNullViolatingFixture() map[string]string {
	return map[string]string{"k1": "a@x", "k2": "", "k3": "c@x"}
}

func notNullCompliantFixture() map[string]string {
	return map[string]string{"k1": "a@x", "k2": "b@x", "k3": "c@x"}
}

// notNullDDLUnderOpenTx runs eager inside an explicit transaction, then runs the
// NOT NULL DDL while that transaction is still open, and returns the DDL's
// verdict together with whether the constraint ended up registered. The
// transaction is rolled back before returning, so the caller reads COMMITTED
// state afterwards. An empty eager runs no statement, which is the no-transaction
// control.
func notNullDDLUnderOpenTx(tb testing.TB, e *Engine, eager string) (registered bool, err error) {
	tb.Helper()
	tx, berr := e.BeginTx(context.Background())
	if berr != nil {
		tb.Fatalf("BeginTx: %v", berr)
	}
	if eager != "" {
		sres, serr := tx.Exec(eager, nil)
		drainConstraintStmt(tb, sres, serr)
	}
	err = runConstraintWrite(tb, e, notNullDDL)
	registered = e.constraintReg.HasNotNull("Person", "email")
	if rerr := tx.Rollback(); rerr != nil {
		tb.Fatalf("Rollback: %v", rerr)
	}
	return registered, err
}

// committedEmailPresent reports whether the COMMITTED graph holds an email on
// the :Person node tagged tag. It reads a predicate-free projection, so nothing
// but the graph itself can answer, and it returns false when the node is absent
// entirely — which is correct for both callers: an absent node violates nothing
// and carries no email.
func committedEmailPresent(tb testing.TB, e *Engine, tag string) bool {
	tb.Helper()
	const query = `MATCH (n:Person) RETURN n.tag, n.email`
	if plan := planOf(tb, e, query, nil); strings.Contains(plan, "NodeByIndexSeek") {
		tb.Fatalf("the committed-state read is not independent of any index — %q plans a seek:\n%s", query, plan)
	}
	res, err := e.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", query, err)
	}
	present := false
	for res.Next() {
		gotTag, ok := res.ValueAt(0).(expr.StringValue)
		if !ok || string(gotTag) != tag {
			continue
		}
		if _, isStr := res.ValueAt(1).(expr.StringValue); isStr {
			present = true
		}
	}
	if derr := res.Err(); derr != nil {
		tb.Fatalf("drain %q: %v", query, derr)
	}
	_ = res.Close()
	return present
}

// TestCreateConstraint_NotNull_EagerFillCannotHideAViolation is the gate on the
// reported defect. Each arm's COMMITTED graph violates NOT NULL — k2 carries
// :Person and no email — and each eager, uncommitted statement makes the LIVE
// view look compliant by a different route. The DDL must be refused with nothing
// registered on every one of them.
//
// The three shapes cover each thing the scan resolves: the property VALUE, the
// LABEL, and node EXISTENCE. They are three of the same four shapes rmp #2778
// and rmp #2792 use, so the gates are comparable arm for arm; the fourth,
// createNode, cannot HIDE a violation and appears in the direction-B test
// instead.
//
// MEASURED PRE-FIX, all three arms: refused = false, registered = TRUE. That is
// what this test fails on.
//
// Each arm also asserts, after the rollback, that k2 still lacks its email —
// otherwise an arm whose eager statement had silently not applied would pass for
// the wrong reason.
func TestCreateConstraint_NotNull_EagerFillCannotHideAViolation(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct{ name, eager string }{
		{"setProperty", `MATCH (n:Person) WHERE n.tag = 'k2' SET n.email = 'hidden@x'`},
		{"removeLabel", `MATCH (n:Person) WHERE n.tag = 'k2' REMOVE n:Person`},
		{"deleteNode", `MATCH (n:Person) WHERE n.tag = 'k2' DETACH DELETE n`},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			e := notNullEngine(t, notNullViolatingFixture())
			registered, err := notNullDDLUnderOpenTx(t, e, arm.eager)
			if err == nil {
				t.Errorf("CREATE CONSTRAINT ... IS NOT NULL SUCCEEDED while the COMMITTED graph "+
					"held a :Person with no email and an open transaction had hidden it from the "+
					"live view with %q. The constraint is now active over violating committed "+
					"data, which is an ACID Consistency breach. Validation must also read "+
					"committed state.", arm.eager)
			} else if !strings.Contains(err.Error(), notNullViolationMsg) {
				t.Errorf("CREATE CONSTRAINT was refused, but not for the null: %v", err)
			}
			if registered {
				t.Error("the refused CREATE CONSTRAINT left a NOT NULL constraint registered on " +
					"(Person).email")
			}
			if committedEmailPresent(t, e, "k2") {
				t.Fatal("this arm is not testing what it claims: after the rollback the committed " +
					"k2 DOES carry an email, so there was no violation to hide")
			}
		})
	}
}

// TestCreateConstraint_NotNull_SucceedsOverCompliantCommittedData is the
// sensitivity control for the test above, and the criterion that the fix BOUNDS
// rather than BLOCKS. A refusal that fires on everything proves nothing, so the
// same DDL, over the same shapes of eager write, must still SUCCEED when the
// committed data genuinely carries the property.
//
// setProperty is the same shape as the direction-A arm that must be refused, with
// the single variable flipped: k2's email is committed. createCompliant adds a
// node that satisfies the constraint. Neither view violates, so the DDL is
// registered.
func TestCreateConstraint_NotNull_SucceedsOverCompliantCommittedData(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct{ name, eager string }{
		{"noOpenTransaction", ``},
		{"setProperty", `MATCH (n:Person) WHERE n.tag = 'k2' SET n.email = 'changed@x'`},
		{"createCompliant", `CREATE (m:Person {tag: 'k4', email: 'd@x'})`},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			e := notNullEngine(t, notNullCompliantFixture())
			registered, err := notNullDDLUnderOpenTx(t, e, arm.eager)
			if err != nil {
				t.Errorf("CREATE CONSTRAINT ... IS NOT NULL was REFUSED over committed data in "+
					"which every :Person carries an email: %v", err)
			}
			if !registered {
				t.Error("CREATE CONSTRAINT returned no error but registered no NOT NULL " +
					"constraint on (Person).email")
			}
		})
	}
}

// TestCreateConstraint_NotNull_EagerNullStillRefusesTheConstraint is the
// counterweight, and it is why the LIVE scan is KEPT alongside the committed one
// rather than replaced by it.
//
// Every arm's COMMITTED graph is compliant; each eager, uncommitted statement
// makes the LIVE view look violating. A snapshot-reading validation cannot see
// any of them, so it would ACCEPT the constraint — and nothing then refuses the
// transaction's commit, because [Engine.BeginTx] allocated no touched-node set
// for a transaction that began before the constraint existed. Measured with
// validation moved to the snapshot alone: CREATE CONSTRAINT succeeded, the
// COMMIT returned nil, and a committed node carried no email under an active
// NOT NULL constraint.
//
// So this test fails on the tempting one-line version of the fix — moving the
// scan to the snapshot — and that is exactly its purpose. It is the direct
// counterpart of
// TestCreateConstraint_UncommittedDuplicateStillRefusesTheConstraint on the
// UNIQUE side.
//
// It also pins that these four arms are NOT what the fix changed: all four were
// already refused pre-fix, by the live scan, and must stay refused.
func TestCreateConstraint_NotNull_EagerNullStillRefusesTheConstraint(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct{ name, eager string }{
		{"removeProperty", `MATCH (n:Person) WHERE n.tag = 'k2' REMOVE n.email`},
		{"setNull", `MATCH (n:Person) WHERE n.tag = 'k2' SET n.email = null`},
		{"createNode", `CREATE (m:Person {tag: 'ghost'})`},
		{"addLabel", `MATCH (o:Other) WHERE o.tag = 'o1' SET o:Person`},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			e := notNullEngine(t, notNullCompliantFixture())
			registered, err := notNullDDLUnderOpenTx(t, e, arm.eager)
			if err == nil {
				t.Errorf("CREATE CONSTRAINT ... IS NOT NULL SUCCEEDED while an open transaction "+
					"held an eager, uncommitted null from %q. Nothing checks NOT NULL at commit "+
					"for a transaction that began before the constraint existed, so that "+
					"transaction can now commit the null and leave a committed node violating an "+
					"active constraint. The validation scan must keep reading the live graph "+
					"as well as committed state.", arm.eager)
			} else if !strings.Contains(err.Error(), notNullViolationMsg) {
				t.Errorf("CREATE CONSTRAINT was refused, but not for the null: %v", err)
			}
			if registered {
				t.Error("the refused CREATE CONSTRAINT left a NOT NULL constraint registered on " +
					"(Person).email")
			}
		})
	}
}

// TestCreateConstraint_NotNull_EnforcementAfterRegistrationUnchanged asserts, in
// BOTH directions, that adding the committed-state validation moved the DDL's
// verdict and nothing else: once a NOT NULL constraint is registered, a committed
// write that FILLS the property is still accepted and one that NULLS it is still
// refused.
//
// The accepted arms are the sensitivity control for the refused ones: a check
// that refuses everything would satisfy the refusals alone.
func TestCreateConstraint_NotNull_EnforcementAfterRegistrationUnchanged(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name        string
		write       string
		wantRefused bool
	}{
		{"fillNewProperty", `MATCH (n:Person) WHERE n.tag = 'k2' SET n.email = 'filled@x'`, false},
		{"createWithProperty", `CREATE (m:Person {tag: 'k4', email: 'd@x'})`, false},
		{"setNull", `MATCH (n:Person) WHERE n.tag = 'k2' SET n.email = null`, true},
		{"removeProperty", `MATCH (n:Person) WHERE n.tag = 'k2' REMOVE n.email`, true},
		{"createWithoutProperty", `CREATE (m:Person {tag: 'ghost'})`, true},
		{"addLabelToNullNode", `MATCH (o:Other) WHERE o.tag = 'o1' SET o:Person`, true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			e := notNullEngine(t, notNullCompliantFixture())
			if err := runConstraintWrite(t, e, notNullDDL); err != nil {
				t.Fatalf("CREATE CONSTRAINT over compliant committed data: %v", err)
			}
			if !e.constraintReg.HasNotNull("Person", "email") {
				t.Fatal("the constraint was not registered, so this arm would assert nothing")
			}
			err := runConstraintWrite(t, e, arm.write)
			switch {
			case arm.wantRefused && err == nil:
				t.Errorf("a committed write that nulls the constrained property was ACCEPTED "+
					"under an active NOT NULL constraint: %q", arm.write)
			case !arm.wantRefused && err != nil:
				t.Errorf("a committed write that fills the constrained property was REFUSED "+
					"under an active NOT NULL constraint: %q: %v", arm.write, err)
			}
			// The committed graph must agree with the verdict, so an arm cannot
			// pass on an error that was returned without the write being undone.
			present := committedEmailPresent(t, e, "k2")
			if arm.name == "setNull" || arm.name == "removeProperty" {
				if !present {
					t.Error("the refused write was not rolled back: k2 no longer carries an email")
				}
			}
		})
	}
}
