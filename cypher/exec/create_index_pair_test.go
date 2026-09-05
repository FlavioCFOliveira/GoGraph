package exec_test

// create_index_pair_test.go — gates the ONE-BARRIER contract of
// [exec.NewCreateIndexPairOp] (rmp #2703).
//
// The property under test is not "both indexes end up registered" — two
// separate barriers achieve that too. It is that the pair is registered inside
// ONE barrier invocation, so a concurrent write transaction's index-change
// fan-out (IndexBuffer.Commit → index.Manager.ApplyBatch), which runs under a
// SHARED hold of the very gate the barrier takes exclusively, can never land
// BETWEEN the two registrations. If it could, that batch would reach the
// primary and be missed PERMANENTLY by the companion — whose backfill snapshot
// predates it and which self-maintains only from changes delivered after it is
// registered — and a later query served by the companion would return an
// incomplete result for a committed write.
//
// fanOutBarrier below models exactly that window: it fans a change out AFTER
// each barrier invocation returns, i.e. in the only place a real fan-out can
// run. With one invocation the two subscribers observe the same change stream;
// split across two, the primary observes a change the companion never sees.

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// recordingSub is an index.Subscriber that records every change fanned out to
// it, so the two members of a pair can be compared for divergence.
type recordingSub struct {
	seen []graph.NodeID
	kind string
	mu   sync.Mutex
}

func (s *recordingSub) Apply(c index.Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, c.Node)
}

func (s *recordingSub) Kind() string { return s.kind }

func (s *recordingSub) observed() []graph.NodeID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]graph.NodeID(nil), s.seen...)
}

// fanOutBarrier stands in for [lpg.Graph.ApplyAtomically]. It counts its
// invocations, tracks whether it is currently held, and — the point of the
// harness — delivers one change to every registered subscriber AFTER each
// invocation returns, which is where a concurrent writer's batch can run.
type fanOutBarrier struct {
	mgr     *index.Manager
	entries int
	fanned  int
	held    bool
}

func (b *fanOutBarrier) run(fn func() error) error {
	b.entries++
	b.held = true
	err := fn()
	b.held = false
	// The gate is released here, so a concurrent write transaction's
	// index-change batch is free to run. Every subscriber registered AT THIS
	// MOMENT receives it; one registered later never will.
	b.fanned++
	b.mgr.ApplyBatch([]index.Change{{
		Node: graph.NodeID(b.fanned),
		Op:   index.OpSetNodeProperty,
	}})
	return err
}

func sameStream(a, b []graph.NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCreateIndexPairOp_PairRegistersInOneBarrier is the atomicity gate. It
// FAILS if the operator registers the primary and the companion in two separate
// barrier invocations.
func TestCreateIndexPairOp_PairRegistersInOneBarrier(t *testing.T) {
	defer goleak.VerifyNone(t)

	mgr := index.NewManager()
	primary := &recordingSub{kind: "btree"}
	companion := &recordingSub{kind: "btree"}
	bar := &fanOutBarrier{mgr: mgr}

	var schemaChanges, schemaChangesInsideBarrier int
	op := exec.NewCreateIndexPairOp(
		exec.IndexRegistration{Name: "user_idx", Sub: primary},
		exec.IndexRegistration{Name: "user_idx_num", Sub: companion},
		false,
		mgr,
		bar.run,
		func() {
			schemaChanges++
			if bar.held {
				schemaChangesInsideBarrier++
			}
		},
	)

	if err := op.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if ok {
		t.Fatal("DDL operator should emit no rows")
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}

	// (1) STRUCTURAL: the whole pair went in under ONE barrier invocation.
	if bar.entries != 1 {
		t.Errorf("index pair registered in %d barrier invocations, want exactly 1: "+
			"a concurrent index fan-out can land between two barriers and be missed "+
			"permanently by the companion", bar.entries)
	}

	// (2) Both indexes are registered, and the operator reports that IT
	// registered them, which is what an unwinding caller acts on.
	if _, gerr := mgr.GetIndex("user_idx"); gerr != nil {
		t.Fatalf("primary not registered: %v", gerr)
	}
	if _, gerr := mgr.GetIndex("user_idx_num"); gerr != nil {
		t.Fatalf("companion not registered: %v", gerr)
	}
	if !op.Registered() {
		t.Error("Registered() = false after a real primary registration")
	}
	if !op.CompanionRegistered() {
		t.Error("CompanionRegistered() = false after a real companion registration")
	}

	// (3) NON-VACUITY: the oracle in (4) is only meaningful if a change was
	// actually delivered. Without this the test would pass on a harness that
	// silently fanned nothing out.
	if bar.fanned == 0 {
		t.Fatal("vacuous oracle: no change was ever fanned out, so (4) proves nothing")
	}
	if len(primary.observed()) == 0 {
		t.Fatal("vacuous oracle: the primary observed no change at all")
	}

	// (4) CONSEQUENCE: with one barrier, no fan-out can reach one member of the
	// pair without the other, so both observed exactly the same change stream.
	gotPrimary, gotCompanion := primary.observed(), companion.observed()
	if !sameStream(gotPrimary, gotCompanion) {
		t.Errorf("index pair diverged: primary observed %v, companion observed %v; "+
			"a change reached one member of the pair and not the other, so the "+
			"companion is permanently missing a committed write", gotPrimary, gotCompanion)
	}

	// (5) onSchemaChange fires exactly once, and INSIDE the barrier, so no
	// cache can be refilled from the pre-change catalog before the change is
	// visible.
	if schemaChanges != 1 {
		t.Errorf("onSchemaChange called %d times, want 1", schemaChanges)
	}
	if schemaChangesInsideBarrier != 1 {
		t.Errorf("onSchemaChange called inside the barrier %d times, want 1",
			schemaChangesInsideBarrier)
	}
}

// TestCreateIndexPairOp_IfNotExistsAbsorbed_RegistersNothing pins the no-op
// branch: an absorbed duplicate applies no schema change, registers no
// companion, and reports nothing for the caller to unwind.
func TestCreateIndexPairOp_IfNotExistsAbsorbed_RegistersNothing(t *testing.T) {
	defer goleak.VerifyNone(t)

	mgr := index.NewManager()
	if err := mgr.CreateIndex("user_idx", &recordingSub{kind: "btree"}); err != nil {
		t.Fatal(err)
	}
	bar := &fanOutBarrier{mgr: mgr}

	schemaChanges := 0
	op := exec.NewCreateIndexPairOp(
		exec.IndexRegistration{Name: "user_idx", Sub: &recordingSub{kind: "btree"}},
		exec.IndexRegistration{Name: "user_idx_num", Sub: &recordingSub{kind: "btree"}},
		true, // IF NOT EXISTS
		mgr,
		bar.run,
		func() { schemaChanges++ },
	)

	_ = op.Init(context.Background())
	var row exec.Row
	if _, err := op.Next(&row); err != nil {
		t.Fatalf("IF NOT EXISTS should not error: %v", err)
	}

	if op.Registered() {
		t.Error("Registered() = true after IF NOT EXISTS absorbed a duplicate")
	}
	if op.CompanionRegistered() {
		t.Error("CompanionRegistered() = true after the primary was absorbed")
	}
	if _, gerr := mgr.GetIndex("user_idx_num"); gerr == nil {
		t.Error("companion registered even though the primary was absorbed")
	}
	if schemaChanges != 0 {
		t.Errorf("onSchemaChange called %d times on an absorbed duplicate, want 0", schemaChanges)
	}
	if bar.entries != 1 {
		t.Errorf("barrier entered %d times, want exactly 1", bar.entries)
	}
}

// TestCreateIndexPairOp_SharedCompanion_NotReportedAsOurs pins the shared-
// companion rule: a second user index on the same (label, property) absorbs the
// existing companion and must NOT report it as its own, or an unwind would drop
// a companion the first index still relies on.
func TestCreateIndexPairOp_SharedCompanion_NotReportedAsOurs(t *testing.T) {
	defer goleak.VerifyNone(t)

	mgr := index.NewManager()
	firstCompanion := &recordingSub{kind: "btree"}
	if err := mgr.CreateIndex("shared_num", firstCompanion); err != nil {
		t.Fatal(err)
	}
	bar := &fanOutBarrier{mgr: mgr}

	op := exec.NewCreateIndexPairOp(
		exec.IndexRegistration{Name: "second_idx", Sub: &recordingSub{kind: "btree"}},
		exec.IndexRegistration{Name: "shared_num", Sub: &recordingSub{kind: "btree"}},
		false,
		mgr,
		bar.run,
		nil,
	)

	_ = op.Init(context.Background())
	var row exec.Row
	if _, err := op.Next(&row); err != nil {
		t.Fatalf("a pre-existing companion must be absorbed, got: %v", err)
	}
	if !op.Registered() {
		t.Error("Registered() = false after the primary was really registered")
	}
	if op.CompanionRegistered() {
		t.Error("CompanionRegistered() = true for a companion this operator did not create")
	}
	// The pre-existing companion is still the registered one.
	sub, gerr := mgr.GetIndex("shared_num")
	if gerr != nil {
		t.Fatalf("shared companion vanished: %v", gerr)
	}
	if sub != index.Subscriber(firstCompanion) {
		t.Error("the absorbed companion was replaced; the first index's companion was displaced")
	}
	if bar.entries != 1 {
		t.Errorf("barrier entered %d times, want exactly 1", bar.entries)
	}
}

// TestCreateIndexPairOp_NoCompanion_StillOneBarrier covers the empty-graph case
// where no companion could be built: the zero IndexRegistration must be treated
// as "no companion" rather than registered under an empty name.
func TestCreateIndexPairOp_NoCompanion_StillOneBarrier(t *testing.T) {
	defer goleak.VerifyNone(t)

	mgr := index.NewManager()
	bar := &fanOutBarrier{mgr: mgr}
	op := exec.NewCreateIndexPairOp(
		exec.IndexRegistration{Name: "solo_idx", Sub: &recordingSub{kind: "btree"}},
		exec.IndexRegistration{}, // no companion was built
		false,
		mgr,
		bar.run,
		nil,
	)

	_ = op.Init(context.Background())
	var row exec.Row
	if _, err := op.Next(&row); err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if !op.Registered() {
		t.Error("Registered() = false after a real registration")
	}
	if op.CompanionRegistered() {
		t.Error("CompanionRegistered() = true when no companion was supplied")
	}
	if names := mgr.ListIndexes(); len(names) != 1 {
		t.Errorf("registered %v, want exactly the primary", names)
	}
	if bar.entries != 1 {
		t.Errorf("barrier entered %d times, want exactly 1", bar.entries)
	}
}

// TestCreateIndexPairOp_NilBarrier_RunsUnwrapped pins the documented nil-barrier
// contract, which the standalone [exec.NewCreateIndexOp] form relies on.
func TestCreateIndexPairOp_NilBarrier_RunsUnwrapped(t *testing.T) {
	defer goleak.VerifyNone(t)

	mgr := index.NewManager()
	op := exec.NewCreateIndexPairOp(
		exec.IndexRegistration{Name: "a", Sub: &recordingSub{kind: "btree"}},
		exec.IndexRegistration{Name: "a_num", Sub: &recordingSub{kind: "btree"}},
		false,
		mgr,
		nil, // no barrier
		nil,
	)
	_ = op.Init(context.Background())
	var row exec.Row
	if _, err := op.Next(&row); err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if !op.Registered() || !op.CompanionRegistered() {
		t.Fatalf("nil barrier must still register the pair; got registered=%v companion=%v",
			op.Registered(), op.CompanionRegistered())
	}
}
