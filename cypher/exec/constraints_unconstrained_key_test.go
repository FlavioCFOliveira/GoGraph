package exec

// constraints_unconstrained_key_test.go — a write of a key no constraint names
// does not take the registry's exclusive lock (rmp #3082).
//
// Before the fix, once ANY UNIQUE or NOT NULL constraint existed, every property
// write took ConstraintRegistry.mu exclusively in reserveSetPropertyGen, whether
// or not its (label, key) was constrained, and every concurrent writer serialised
// on that one mutex. The tests below hold that mutex in the test goroutine and
// require the unconstrained calls to complete anyway: against the old code they
// block until the deadline and fail. No timing is measured; the deadline only
// bounds a hang.
//
// Layer: short. Race-clean.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// unconstrainedKeyDeadline bounds the wait for a call that must not block. It
// is a hang guard, not a measurement: a passing call returns in microseconds.
const unconstrainedKeyDeadline = 30 * time.Second

// TestReserveSetProperty_UnconstrainedKeyTakesNoExclusiveLock holds the
// registry's lock exclusively and requires a reservation, an immediate release
// and a commit of an UNCONSTRAINED key to complete while it is held.
func TestReserveSetProperty_UnconstrainedKeyTakesNoExclusiveLock(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.RegisterUnique("Person", "email", "__uniq__Person.email")
	r.RegisterNotNull("Person", "id")
	v := lpg.StringValue("x")

	var ct ConstraintTxn
	r.ReleasePropertyValue(&ct, []string{"Person", "Admin"}, "name", v)
	// A second release the reservation below does not spend, so CommitTxn has a
	// mark to apply and is exercised past its empty-transaction early return.
	r.ReleasePropertyValue(&ct, []string{"Person"}, "name", lpg.StringValue("y"))
	if ct.empty() {
		t.Fatal("no release mark recorded: CommitTxn below would not be exercised")
	}

	r.mu.Lock()
	done := make(chan error, 1)
	go func() {
		// Labels carry a constrained label, the property is not constrained
		// under any of them: (Person, name) and (Admin, name) are both free.
		err := r.ReserveSetProperty(&ct, []string{"Person", "Admin"}, "name", v, nil)
		r.ReleasePropertyValue(nil, []string{"Person"}, "name", v)
		r.CommitTxn(&ct)
		done <- err
	}()
	select {
	case err := <-done:
		r.mu.Unlock()
		if err != nil {
			t.Fatalf("unconstrained reservation refused: %v", err)
		}
	case <-time.After(unconstrainedKeyDeadline):
		r.mu.Unlock()
		<-done
		t.Fatal("a write of an unconstrained key waited on the registry's exclusive lock")
	}
}

// TestReserveSetProperty_ConstrainedKeyStillEnforcedAfterGate pins that the
// gate does not leak enforcement: a constrained key under one of several labels
// is still checked and reserved, and NOT NULL is still enforced.
func TestReserveSetProperty_ConstrainedKeyStillEnforcedAfterGate(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.RegisterUnique("Person", "email", "__uniq__Person.email")
	r.RegisterNotNull("Admin", "name")
	v := lpg.StringValue("a@b")

	if err := r.ReserveSetProperty(nil, []string{"Admin", "Person"}, "email", v, nil); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if err := r.ReserveSetProperty(nil, []string{"Person"}, "email", v, nil); err == nil {
		t.Fatal("a duplicate under UNIQUE (Person, email) was admitted")
	}
	var null lpg.PropertyValue
	if err := r.ReserveSetProperty(nil, []string{"Person", "Admin"}, "name", null, nil); err == nil {
		t.Fatal("a null under NOT NULL (Admin, name) was admitted")
	}
}

// TestReserveSetProperty_UnconstrainedReTakeSpendsOwnRelease pins the one side
// effect the lock-free path keeps: a transaction that releases a value of an
// unconstrained key and takes it again must not, once a UNIQUE constraint is
// registered on that key before it commits, delete the value it still holds
// from the new value-set at commit.
func TestReserveSetProperty_UnconstrainedReTakeSpendsOwnRelease(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.RegisterUnique("Other", "k", "__uniq__Other.k") // some key is constrained
	v := lpg.StringValue("x")
	labels := []string{"Person"}

	var ct ConstraintTxn
	r.ReleasePropertyValue(&ct, labels, "name", v)
	if err := r.ReserveSetProperty(&ct, labels, "name", v, nil); err != nil {
		t.Fatalf("re-take: %v", err)
	}
	if ct.releasedHere(constraintKey("Person", "name"), "x") {
		t.Fatal("re-taking the value left the transaction's own release mark unspent")
	}

	r.RegisterUnique("Person", "name", "__uniq__Person.name")
	if err := r.SeedUniqueValues("Person", "name", []lpg.PropertyValue{v}); err != nil {
		t.Fatal(err)
	}
	r.CommitTxn(&ct)
	if err := r.ReserveSetProperty(nil, labels, "name", v, nil); err == nil {
		t.Fatal("the commit deleted a value its transaction still holds: a duplicate was admitted")
	}
}
