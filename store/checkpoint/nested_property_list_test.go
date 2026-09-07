package checkpoint_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpoint_NestedPropertyListIsRefusedAtCommit_2783 pins the interaction
// between rmp #2783 and the permanent-checkpoint-block mechanism rmp #2750
// named, so that interaction is measured rather than assumed.
//
// # What it looked like before the fix, measured
//
// The commit below returned nil, and then:
//
//	RunCheckpoint()      = "snapshot: capture properties.bin: snapshot: nested PropList not supported"
//	RunCheckpoint() again = the same error
//
// Permanently: store/snapshot refuses the value outright, so no checkpoint could
// ever complete for as long as the value lived in the graph, and the WAL prefix
// was therefore never truncated. That is the second half of #2783's severity —
// the WAL half loses an acknowledged transaction on replay, and this half makes
// the WAL grow without bound in the meantime.
//
// Note that this is the OPPOSITE direction from #2750's own case, which was a
// value the WAL accepted and the snapshot refused for its LENGTH. Here the two
// formats disagree on the value's SHAPE, and the WAL is the one that silently
// destroys it while the snapshot at least refuses loudly.
//
// # What this test asserts
//
// The value is refused at commit, and a checkpoint over the same store then
// succeeds — the store is not trapped. Unlike a test that merely asserted
// "RunCheckpoint fails for a nested list", this fails on the unfixed code twice
// over: the commit is accepted there, and the checkpoint that follows is not.
func TestCheckpoint_NestedPropertyListIsRefusedAtCommit_2783(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := txn.NewStoreWithCodec[string, float64](g, w, txn.NewStringCodec())

	tx := st.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit node: %v", err)
	}

	nested := lpg.ListValue([]lpg.PropertyValue{
		lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)}),
		lpg.StringValue("tail"),
	})
	tx2 := st.Begin()
	if err := tx2.SetNodeProperty("n", "nested", nested); err != nil {
		t.Fatalf("SetNodeProperty (staging): %v", err)
	}
	cerr := tx2.Commit()
	if cerr == nil {
		t.Fatal("Commit ACCEPTED a nested list. store/snapshot refuses to fold it, so every " +
			"checkpoint from now on fails and the WAL prefix is never truncated (rmp #2783, " +
			"the mechanism rmp #2750 named)")
	}
	if !errors.Is(cerr, txn.ErrNestedPropertyList) {
		t.Fatalf("Commit refused with %v, which does not wrap txn.ErrNestedPropertyList", cerr)
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.Codec()),
	)
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("checkpoint failed after the value was refused, so the store is still trapped: %v", err)
	}

	// The bounds-rather-than-blocks control, over the same store and the same
	// checkpointer: a FLAT list commits and folds.
	tx3 := st.Begin()
	if err := tx3.SetNodeProperty("n", "flat", lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(1), lpg.StringValue("two"), lpg.BoolValue(true),
	})); err != nil {
		t.Fatalf("control SetNodeProperty: %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("over-restricted: a flat list was refused at commit: %v", err)
	}
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("over-restricted: a flat list committed but could not be checkpointed: %v", err)
	}
}
