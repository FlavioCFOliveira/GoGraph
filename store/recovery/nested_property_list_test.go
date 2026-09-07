package recovery

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestNestedPropertyList_WALOnlyRecoveryLosesNothing_2783 is the durability half
// of the rmp #2783 gate, and the reason the defect was severe rather than
// merely untidy.
//
// # What it looked like before the fix, measured
//
// The workload below — commit a label, then a nested-list property, then a
// second property — was run against the unfixed encoder. Every commit returned
// nil. WAL-only recovery then reported:
//
//	Open err   = <nil>
//	WALOps     = 1                  (the label; nothing else)
//	TailErr    = "recovery: corrupt op inside a committed v3 transaction"
//	IsClean()  = true
//	nested     = ABSENT
//	after      = ABSENT
//
// Two acknowledged transactions gone, and the only trace was a TailErr that
// [tailErrIsCorruption] classified as benign — so Open returned success, no
// corruption metric was incremented, and nothing was logged.
//
// That misclassification was a SECOND, independent defect, and it is no longer
// current behaviour: rmp #2794 gave the stop reason a sentinel
// ([ErrCommittedTxnCorruptOp]), so the same state now reports IsClean() ==
// false, increments a counter and logs a structured warning (the open still
// succeeds — see the recorded departure note on [tailErrIsCorruption]). The
// listing above is the historical measurement, not the contract; the current
// contract is gated by
// TestRecovery_CorruptOpInsideCommittedTxn_NotCleanAndDiagnosable_2794.
//
// The mechanism of THIS defect: the encoder
// wrote the nested element as (kind 7 | length 0), the decoder refuses
// kind 7, and that refusal inside an already-committed v3 transaction stops
// replay for the rest of the file.
//
// # What this test asserts
//
// The value can no longer be committed, so recovery can never be asked to
// replay it — and, crucially, the transaction that FOLLOWS the refused one
// survives the round trip. The second assertion is the one that would have
// caught the real severity: a test that only checked the refused property was
// missing would have passed on the unfixed code too, because it was missing
// there as well.
func TestNestedPropertyList_WALOnlyRecoveryLosesNothing_2783(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	codec := txn.NewStringCodec()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithCodec[string, int64](g, w, codec)

	tx := store.Begin()
	if err := tx.SetNodeLabel("n", "Thing"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit label: %v", err)
	}

	nested := lpg.ListValue([]lpg.PropertyValue{
		lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)}),
		lpg.StringValue("tail"),
	})
	tx2 := store.Begin()
	if err := tx2.SetNodeProperty("n", "nested", nested); err != nil {
		t.Fatalf("SetNodeProperty (staging): %v", err)
	}
	cerr := tx2.Commit()
	if cerr == nil {
		t.Fatal("Commit ACCEPTED a nested list. It is written as (kind 7 | length 0), so the " +
			"inner list never reaches the WAL; the decoder then refuses kind 7 INSIDE this " +
			"already-committed transaction and replay stops for the remainder of the file, " +
			"discarding this transaction and every one after it while Open returns nil (rmp #2783)")
	}
	if !errors.Is(cerr, txn.ErrNestedPropertyList) {
		t.Fatalf("Commit refused with %v, which does not wrap txn.ErrNestedPropertyList", cerr)
	}

	// A transaction AFTER the refused one. On the unfixed encoder this was lost
	// too, silently — it is the assertion that measures the real blast radius.
	tx3 := store.Begin()
	if err := tx3.SetNodeProperty("n", "after", lpg.StringValue("survivor")); err != nil {
		t.Fatalf("SetNodeProperty after the refusal: %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("commit after the refusal: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       codec,
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.TailErr != nil {
		t.Fatalf("replay reported a tail error (%v). Every frame in this WAL was written by a "+
			"commit that returned nil, so every frame must replay: a tail error here means a "+
			"value was encoded that cannot be decoded (rmp #2783)", res.TailErr)
	}

	rg := res.Graph
	if !rg.HasNodeLabel("n", "Thing") {
		t.Fatal("the label committed BEFORE the refused transaction did not survive recovery")
	}
	after, ok := rg.GetNodeProperty("n", "after")
	if !ok {
		t.Fatal("the property committed AFTER the refused transaction did not survive recovery. " +
			"This is the silent WAL-suffix loss rmp #2783 caused: replay stopped at the " +
			"undecodable nested-list frame and never reached this one")
	}
	if s, _ := after.String(); s != "survivor" {
		t.Fatalf("recovered after = %q, want %q", s, "survivor")
	}
	// The refused value is not durable, so it must not reappear — least of all
	// as an empty list, which would be a value the graph never held.
	if v, ok := rg.GetNodeProperty("n", "nested"); ok {
		t.Fatalf("a REFUSED property came back from recovery as %v; the refusal must buffer "+
			"nothing durable", v.Kind())
	}

	// A flat list is the control, over the same store and the same round trip:
	// the guard must bound this shape and not the neighbouring legitimate one.
	w2, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("reopen wal: %v", err)
	}
	store2 := res.NewStore(w2, txn.Options[string, int64]{
		Codec:       codec,
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	flat := lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(1), lpg.StringValue("two"), lpg.BoolValue(true),
	})
	tx4 := store2.Begin()
	if err := tx4.SetNodeProperty("n", "flat", flat); err != nil {
		t.Fatalf("control SetNodeProperty: %v", err)
	}
	if err := tx4.Commit(); err != nil {
		t.Fatalf("over-restricted: a flat list was refused at commit: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("wal.Close 2: %v", err)
	}

	res2, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       codec,
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	got, ok := res2.Graph.GetNodeProperty("n", "flat")
	if !ok {
		t.Fatal("control: a flat list did not survive WAL-only recovery")
	}
	elems, ok := got.List()
	if !ok {
		t.Fatalf("control: recovered flat property is %v, want PropList", got.Kind())
	}
	if len(elems) != 3 {
		t.Fatalf("control: recovered flat list has %d elements, want 3", len(elems))
	}
	if s, _ := elems[1].String(); s != "two" {
		t.Fatalf("control: recovered flat list element 1 = %q, want %q", s, "two")
	}
}
