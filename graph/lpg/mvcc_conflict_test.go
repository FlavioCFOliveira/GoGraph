package lpg

// mvcc_conflict_test.go — write-write conflicts are DETECTED, not silently lost
// (rmp #2300). Design: docs/design-write-conflict-detection.md.
//
// Each case is built so that WITHOUT detection it is a lost update: two
// transactions write the same object, both succeed, and the second silently
// discards the first. The assertion is on the detection, and each test names
// what the un-detected outcome would be, so removing the check turns the test
// red rather than merely changing a message.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// openWriter opens a write bracket by hand — Graph.beginWrite plus the writer
// snapshot it publishes — so a test can hold TWO of them open at once. The
// exclusive barrier makes that impossible through ApplyAtomically, which is
// exactly the thing this sprint is removing; until it is gone, this is how an
// overlapping writer is expressed.
//
// It returns the writer's snapshot and a function that closes the bracket.
func openWriter(t *testing.T, g *Graph[string, int64], startTS, txID uint64) (*Snapshot, func()) {
	t.Helper()
	prev := g.writerSnap.Swap(&Snapshot{startTS: startTS, txID: txID})
	snap := g.writerSnap.Load()
	return snap, func() { g.writerSnap.Store(prev) }
}

// TestConflict_LabelWriteAgainstAnInFlightWriter is first-updater-wins: a
// writer whose target was already modified by a transaction that has not
// finished must be told, not allowed to overwrite.
//
// WITHOUT DETECTION this is a lost update: writer B's label lands on top of
// writer A's uncommitted one, A commits, and A's change is gone with no error
// anywhere.
func TestConflict_LabelWriteAgainstAnInFlightWriter(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// Writer A takes a version of the node's labels and does NOT finish.
	txA := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}

	// Writer B begins at the same instant, with its own identity, and tries to
	// write the same node.
	_, closeB := openWriter(t, g, txA.startTS, mvcc.TxIDBase+9999)
	defer closeB()

	err := g.SetNodeLabel("a", "FromB")
	if err == nil {
		t.Fatal("B overwrote a node that writer A is still writing, with no error: " +
			"A's label is lost the moment A commits, and nothing anywhere reports it")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("B got %v, want a serialization conflict", err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("the error does not carry a *mvcc.Conflict: %v", err)
	}
	if c.Store != "node labels" {
		t.Fatalf("Conflict.Store = %q, want %q", c.Store, "node labels")
	}
	if !c.ConcurrentWriter() {
		t.Fatal("the blocking version belongs to an in-flight writer, so this is " +
			"first-updater-wins and must report as such")
	}

	// A's write survives, which is the whole point of refusing B.
	tsA := txA.commit()
	if !g.ReadAt(&Snapshot{startTS: tsA}).HasNodeLabel("a", "FromA") {
		t.Fatal("A's label did not survive: it was lost despite B being refused")
	}
	if g.ReadAt(&Snapshot{startTS: tsA}).HasNodeLabel("a", "FromB") {
		t.Fatal("B's refused write was applied anyway")
	}
}

// TestConflict_LabelWriteAgainstACommitNewerThanMySnapshot is
// first-committer-wins: the blocking version is COMMITTED, but committed after
// this transaction began, so adopting it would break snapshot isolation.
func TestConflict_LabelWriteAgainstACommitNewerThanMySnapshot(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// A transaction begins here, at the current instant.
	startTS := g.readTS()

	// Someone else commits a change to the same node AFTER that instant.
	other := g.beginLabelTx()
	if err := other.setNodeLabel("a", "Committed"); err != nil {
		t.Fatalf("other setNodeLabel: %v", err)
	}
	tsOther := other.commit()
	if tsOther <= startTS {
		t.Fatalf("the other transaction committed at %d, not after %d", tsOther, startTS)
	}

	// Our transaction, still on its original snapshot, tries to write it.
	_, done := openWriter(t, g, startTS, mvcc.TxIDBase+4242)
	defer done()

	err := g.SetNodeLabel("a", "Mine")
	if err == nil {
		t.Fatal("a transaction overwrote a version committed AFTER its snapshot began, with no " +
			"error: it never saw that version, so its write is based on state that no longer exists")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("got %v, want a serialization conflict", err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("the error does not carry a *mvcc.Conflict: %v", err)
	}
	if c.ConcurrentWriter() {
		t.Fatal("the blocking version is COMMITTED, so this is first-committer-wins, " +
			"not first-updater-wins")
	}
}

// TestConflict_NoneWhenWritersTouchDisjointObjects is the property that
// distinguishes conflict detection from a global lock wearing a new name.
func TestConflict_NoneWhenWritersTouchDisjointObjects(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}

	_, done := openWriter(t, g, txA.startTS, mvcc.TxIDBase+9999)
	defer done()

	// B writes a DIFFERENT node. Nothing about A blocks it.
	if err := g.SetNodeLabel("b", "FromB"); err != nil {
		t.Fatalf("B was refused a write to a disjoint object: %v — conflict detection is "+
			"behaving as a global lock", err)
	}
	if g.ConflictPending() {
		t.Fatal("a conflict was recorded for disjoint writers")
	}
}

// TestConflict_OwnSecondWriteIsNotAConflict covers the case Memgraph tests
// first: a transaction must be free to write the same object twice.
func TestConflict_OwnSecondWriteIsNotAConflict(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if err := g.ApplyAtomically(func() error {
		if err := g.SetNodeLabel("a", "One"); err != nil {
			return err
		}
		// Same transaction, same node, second write. The head is now this
		// transaction's own version, which must not block it.
		return g.SetNodeLabel("a", "Two")
	}); err != nil {
		t.Fatalf("a transaction was refused its own second write to the same node: %v", err)
	}
	if g.ConflictPending() {
		t.Fatal("a conflict was recorded against a transaction writing its own version")
	}
	now := g.ReadAt(&Snapshot{startTS: g.readTS()})
	if !now.HasNodeLabel("a", "One") || !now.HasNodeLabel("a", "Two") {
		t.Fatal("both labels of the transaction's own two writes should be present")
	}
}

// TestConflict_TakeClearsIt covers the reason TakeConflict takes rather than
// reads: a conflict outliving its transaction would abort an innocent one,
// because the record is still per-graph (rmp #2301 moves it).
func TestConflict_TakeClearsIt(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	txA := g.beginLabelTx()
	if err := txA.setNodeLabel("a", "FromA"); err != nil {
		t.Fatalf("A setNodeLabel: %v", err)
	}
	_, done := openWriter(t, g, txA.startTS, mvcc.TxIDBase+9999)
	defer done()

	if err := g.SetNodeLabel("a", "FromB"); err == nil {
		t.Fatal("expected a conflict")
	}
	// SetNodeLabel already took it on the way out.
	if g.ConflictPending() {
		t.Fatal("the conflict was not taken by the write that reported it, so the next " +
			"transaction on this graph would inherit it")
	}
	if err := g.TakeConflict(); err != nil {
		t.Fatalf("TakeConflict on a clean graph returned %v, want nil", err)
	}
}

// TestConflict_PropertyWriteAgainstAnInFlightWriter is the property store's
// half of first-updater-wins. Without detection it is a lost update exactly as
// the label case is: B's value lands on top of A's uncommitted one and A's
// change vanishes when A commits.
func TestConflict_PropertyWriteAgainstAnInFlightWriter(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("a", "v", StringValue("fromA")); err != nil {
		t.Fatalf("A setNodeProperty: %v", err)
	}

	_, done := openWriter(t, g, txA.startTS, mvcc.TxIDBase+9999)
	defer done()

	err := g.SetNodeProperty("a", "v", StringValue("fromB"))
	if err == nil {
		t.Fatal("B overwrote a property that writer A is still writing, with no error: " +
			"A's value is lost the moment A commits, and nothing reports it")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("B got %v, want a serialization conflict", err)
	}
	var c *mvcc.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("the error does not carry a *mvcc.Conflict: %v", err)
	}
	if c.Store != "node properties" {
		t.Fatalf("Conflict.Store = %q, want %q", c.Store, "node properties")
	}
}

// TestConflict_PropertyWriteThatChangesNothing covers the guard the property
// path needs and the label path gets for free: re-setting a property to the
// value it already holds records no version, so it must not conflict either.
// MERGE's MATCH branch re-asserts properties on every match, so a conflict here
// would abort transactions that changed nothing at all.
func TestConflict_PropertyWriteThatChangesNothing(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	txA := g.beginLabelTx()
	if err := txA.setNodeProperty("a", "v", StringValue("same")); err != nil {
		t.Fatalf("A setNodeProperty: %v", err)
	}

	_, done := openWriter(t, g, txA.startTS, mvcc.TxIDBase+9999)
	defer done()

	// B writes the value that is already there. No version is recorded, so
	// there is nothing to lose and nothing to conflict over.
	if err := g.SetNodeProperty("a", "v", StringValue("same")); err != nil {
		t.Fatalf("a write that changes nothing was refused: %v — MERGE's MATCH branch "+
			"re-asserts properties on every match, so this would abort transactions "+
			"that wrote nothing at all", err)
	}
	if g.ConflictPending() {
		t.Fatal("a conflict was recorded for a write that changed nothing")
	}
}
