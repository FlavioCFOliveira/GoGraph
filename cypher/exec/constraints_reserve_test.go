package exec

// constraints_reserve_test.go — UNIQUE enforcement is atomic (rmp #2321).
//
// The defect: [ConstraintRegistry.CheckSetProperty] takes the READ lock and only
// asks, while [ConstraintRegistry.RecordPropertySet] takes the WRITE lock and
// answers, later. Two concurrent writers both pass the check, both record, and the
// UNIQUE constraint is violated with both statements reporting success — an ACID
// Consistency violation, since a committed transaction leaves the graph failing a
// declared invariant.
//
// It was unreachable while the engine serialised whole write statements on the
// exclusive visibility barrier. It is not rare once they overlap: with the
// concurrent write path enabled, 14 of 15 -race runs of 8 concurrent MERGE under a
// UNIQUE constraint produced 2 or 4 nodes where 1 is required.
//
// [ConstraintRegistry.ReserveSetProperty] closes it by doing both halves in one
// critical section, as PostgreSQL's _bt_doinsert holds the leaf buffer lock across
// _bt_check_unique and the insert.
//
// # These tests are validated against the defect
//
// TestReserveSetProperty_ExactlyOneWinnerUnderRace fails against the check-then-act
// pair — replace the ReserveSetProperty call with CheckSetProperty followed by
// RecordPropertySet and it reports many winners. That is the whole point: a test
// for a race that passes against the racy code proves nothing.
//
// Layer: short. Race-clean.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// reserveRig builds a registry with one UNIQUE constraint on (:Person).email and
// an EMPTY value-set, which is the state a reservation has to arbitrate.
//
// Seeding matters: the value-set must be non-nil, because a nil set means "not yet
// seeded" and sends the check to the secondary hash-index source instead. RegisterUnique
// creates it, so registering is enough.
func reserveRig(t *testing.T) *ConstraintRegistry {
	t.Helper()
	r := NewConstraintRegistry()
	r.RegisterUnique("Person", "email", "__uniq__Person.email")
	if !r.HasUnique("Person", "email") {
		t.Fatal("the UNIQUE constraint did not register, so nothing below is arbitrating anything")
	}
	return r
}

// TestReserveSetProperty_ExactlyOneWinnerUnderRace is the discriminating test:
// concurrent reservations of ONE value must yield exactly one winner.
func TestReserveSetProperty_ExactlyOneWinnerUnderRace(t *testing.T) {
	t.Parallel()

	const goroutines = 32
	r := reserveRig(t)
	val := lpg.StringValue("alice@example.com")

	var (
		wg      sync.WaitGroup
		winners atomic.Int64
		losers  atomic.Int64
		other   atomic.Int64
		start   = make(chan struct{})
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // release them together, so the race is real rather than staggered
			switch err := r.ReserveSetProperty([]string{"Person"}, "email", val, nil); {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, ErrConstraintViolation):
				losers.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected error kind: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent reservations of the same value succeeded, want exactly 1 — "+
			"the UNIQUE constraint is not being enforced, so a committed transaction can leave "+
			"the graph violating a declared invariant", got, goroutines)
	}
	if got := losers.Load(); got != goroutines-1 {
		t.Errorf("%d losers, want %d — every non-winner must be told why", got, goroutines-1)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("%d reservations failed with something other than a constraint violation", got)
	}
}

// TestReserveSetProperty_NothingReservedOnViolation covers the all-or-nothing
// promise across LABELS.
//
// A node carries all its labels at once, so a value constrained under two labels
// must be reserved under both or neither. Reserving the first and then failing the
// second would leave a phantom that only a whole-graph reseed could clear — the
// exact class of stale reservation #1342 had to fix.
func TestReserveSetProperty_NothingReservedOnViolation(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.RegisterUnique("A", "k", "__uniq__A.k")
	r.RegisterUnique("B", "k", "__uniq__B.k")
	taken := lpg.StringValue("taken")

	// Make the value already in use under B only, so a two-label reservation must
	// fail — and must not leave A holding it.
	if err := r.SeedUniqueValues("B", "k", []lpg.PropertyValue{taken}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	if err := r.ReserveSetProperty([]string{"A", "B"}, "k", taken, nil); err == nil {
		t.Fatal("a value already in use under B was reserved for (A, B)")
	}

	// A must still be free. Asking through the single-label path is what detects a
	// partial reservation: if phase 1 had reserved A before phase 2 rejected B, this
	// would now report a violation.
	if err := r.ReserveSetProperty([]string{"A"}, "k", taken, nil); err != nil {
		t.Fatalf("label A holds a reservation from a call that FAILED: %v", err)
	}
}

// TestReserveSetProperty_IsIdempotentAfterRelease pins the SET-to-the-same-value
// path, which is the ordering every caller uses: release the old value, reserve the
// new one.
//
// Without the release a self-assignment (`SET n.email = <its current value>`) would
// be rejected by its own existing reservation. The write operators release first for
// exactly this reason (cypher/exec/set_all.go), and reserving at check time must not
// disturb it.
func TestReserveSetProperty_IsIdempotentAfterRelease(t *testing.T) {
	t.Parallel()

	r := reserveRig(t)
	val := lpg.StringValue("bob@example.com")

	if err := r.ReserveSetProperty([]string{"Person"}, "email", val, nil); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	// Re-reserving without releasing must be refused: that is the constraint working.
	if err := r.ReserveSetProperty([]string{"Person"}, "email", val, nil); err == nil {
		t.Fatal("the same value was reserved twice with no release in between")
	}
	// After releasing it, the same value is free again — the SET-same-value path.
	r.ReleasePropertyValue([]string{"Person"}, "email", val)
	if err := r.ReserveSetProperty([]string{"Person"}, "email", val, nil); err != nil {
		t.Fatalf("reservation after release: %v", err)
	}
}

// TestReserveSetProperty_NullIsUnconstrainedByUnique keeps UNIQUE's treatment of
// null intact: UNIQUE does not constrain nulls, so two nulls must both be accepted
// and neither may be reserved.
//
// Reserving a null would make the FIRST null write block every later one, turning a
// permitted state into a violation — so this is a guard on phase 2's
// propertyValueToString gate, not a restatement of the check.
func TestReserveSetProperty_NullIsUnconstrainedByUnique(t *testing.T) {
	t.Parallel()

	r := reserveRig(t)
	var null lpg.PropertyValue // zero value is null in the lpg type system

	for i := 0; i < 3; i++ {
		if err := r.ReserveSetProperty([]string{"Person"}, "email", null, nil); err != nil {
			t.Fatalf("null reservation %d was refused by a UNIQUE constraint: %v", i, err)
		}
	}
}

// TestReserveSetProperty_NotNullStillEnforced confirms the reserve path did not
// lose the other constraint kind while gaining atomicity: it must reject a null
// under NOT NULL, exactly as the check-only path does.
func TestReserveSetProperty_NotNullStillEnforced(t *testing.T) {
	t.Parallel()

	r := NewConstraintRegistry()
	r.RegisterNotNull("Person", "name")
	var null lpg.PropertyValue

	err := r.ReserveSetProperty([]string{"Person"}, "name", null, nil)
	if err == nil {
		t.Fatal("a null value was accepted under a NOT NULL constraint")
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("NOT NULL rejection is not a constraint violation: %v", err)
	}
}
