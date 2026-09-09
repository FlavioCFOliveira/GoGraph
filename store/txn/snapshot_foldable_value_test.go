package txn

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// This file is the regression gate for rmp #2750: a property value that commits
// durably to the WAL and can then never be folded into a snapshot, pinning the
// WAL prefix for as long as it lives in the graph.
//
// #2750 was raised on the premise that the gap was a byte-length window,
// [1 GiB, 4 GiB), between store/txn's uint32 prefix cap and store/snapshot's
// maxValueLen. That premise does not survive contact with the code: wal.Encode
// refuses an assembled op frame over its own 1 GiB maxFrameSize, and an op frame
// carries the value plus at least 17 bytes of framing, so no SCALAR value can
// reach 1 GiB at all — it is refused at commit with wal.ErrFrameTooLarge.
//
// The defect the task describes is real all the same, at a boundary the task did
// not name: the two formats encode the SAME value to DIFFERENT lengths. Both
// write (uint8 kind | uint32 len | payload) per list element, but store/snapshot
// gives a PropInt64 element a fixed 8 bytes and a PropTime element a fixed 16,
// where this package writes a varint. A list of small integers costs 13 bytes per
// element in the snapshot against 6 in the WAL, so a list can assemble a WAL frame
// well inside 1 GiB and a snapshot value over it.
//
// The gate is therefore in three parts, and all three are needed:
//
//  1. TestSnapshotEncodedValueLen_Exact_2750 — the length this package computes
//     is EXACTLY the length store/snapshot really produces. Without this the
//     guard would bound the wrong number.
//  2. TestCheckSnapshotFoldableLen_Boundary_2750 and
//     TestSnapshotValueCapAgreement_2750 — the bound fires at exactly the right
//     number, and that number is the one store/snapshot enforces.
//  3. TestCommitRefusesUnfoldableValue_2750 — the guard is WIRED into the commit
//     path, with a value in the window the task named.
//
// The end-to-end proof of the list asymmetry itself needs 82_595_525 elements —
// it cannot be scaled down, because the asymmetry is per-element — and lives in
// store/checkpoint/foldable_value_soak_test.go behind the soak tag.

// TestSnapshotEncodedValueLen_Exact_2750 pins [snapshotEncodedValueLen] against
// the REAL store/snapshot writer, for every property kind and for a list mixing
// all of them.
//
// It matters more than the bound itself. The guard refuses a value by computing
// what store/snapshot would emit rather than emitting it, so if the computation
// drifts from the encoder by one byte per element the guard silently bounds the
// wrong quantity and #2750 reopens. Here the expected value is not a constant
// this test asserts: it is len(ValueBytes) read back out of a properties.bin the
// snapshot package actually wrote.
func TestSnapshotEncodedValueLen_Exact_2750(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 9, 7, 1, 2, 3, 4, time.UTC)
	cases := []struct {
		name  string
		value lpg.PropertyValue
	}{
		{"string-empty", lpg.StringValue("")},
		{"string", lpg.StringValue("hello, snapshot")},
		{"bytes", lpg.BytesValue([]byte{1, 2, 3, 4, 5, 6, 7})},
		{"bytes-empty", lpg.BytesValue(nil)},
		{"int64-small", lpg.Int64Value(7)},
		{"int64-large", lpg.Int64Value(math.MaxInt64)},
		{"float64", lpg.Float64Value(1.5)},
		{"bool-true", lpg.BoolValue(true)},
		{"bool-false", lpg.BoolValue(false)},
		{"time", lpg.TimeValue(ts)},
		{"list-empty", lpg.ListValue(nil)},
		{"list-ints", lpg.ListValue([]lpg.PropertyValue{
			lpg.Int64Value(0), lpg.Int64Value(63), lpg.Int64Value(math.MinInt64),
		})},
		{"list-times", lpg.ListValue([]lpg.PropertyValue{
			lpg.TimeValue(ts), lpg.TimeValue(ts.Add(time.Hour)),
		})},
		{"list-mixed", lpg.ListValue([]lpg.PropertyValue{
			lpg.StringValue("abc"), lpg.Int64Value(-1), lpg.Float64Value(0),
			lpg.BoolValue(true), lpg.TimeValue(ts), lpg.BytesValue([]byte{9, 9}),
			lpg.StringValue(""),
		})},
	}

	checked := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := snapshotWrittenValueLen(t, tc.value)
			got := snapshotEncodedValueLen(tc.value)
			checked++
			if got != int64(want) {
				t.Fatalf("snapshotEncodedValueLen = %d, but store/snapshot really wrote %d bytes "+
					"for this value; the commit-time guard would bound the wrong quantity", got, want)
			}
		})
	}
	if checked != len(cases) {
		t.Fatalf("vacuity oracle: %d of %d kinds reached the comparison, want %d",
			checked, len(cases), len(cases))
	}
}

// snapshotWrittenValueLen returns the number of bytes store/snapshot actually
// emits for value, by writing a one-node, one-property graph through
// [snapshot.WriteProperties] and reading the record back with
// [snapshot.ReadProperties]. ValueBytes is exactly the payload the snapshot's
// uint32 prefix describes and exactly what checkSnapshotValueLen measures there.
func snapshotWrittenValueLen(t *testing.T, value lpg.PropertyValue) int {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeProperty("n", "p", value); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	var buf bytes.Buffer
	if _, _, err := snapshot.WriteProperties(&buf, g, nil); err != nil {
		t.Fatalf("snapshot.WriteProperties: %v", err)
	}
	rb, err := snapshot.ReadProperties(&buf)
	if err != nil {
		t.Fatalf("snapshot.ReadProperties: %v", err)
	}
	if len(rb.NodeProperties) != 1 {
		t.Fatalf("expected exactly one node property record, got %d", len(rb.NodeProperties))
	}
	return len(rb.NodeProperties[0].ValueBytes)
}

// TestSnapshotValueCapAgreement_2750 is the drift tripwire for the number
// itself. store/txn cannot import store/snapshot, so the cap is declared twice;
// this pins this side to the literal and names the other, and
// store/snapshot's TestPropertyValueCapAgreement_2750 does the reciprocal.
func TestSnapshotValueCapAgreement_2750(t *testing.T) {
	t.Parallel()

	if maxSnapshotValueLen != 1<<30 {
		t.Fatalf("maxSnapshotValueLen = %d, want 1<<30 (%d). This constant MUST equal "+
			"store/snapshot's maxValueLen (store/snapshot/properties.go): it is the cap "+
			"ReadProperties enforces, so a value above it commits and can then never be "+
			"folded into a snapshot. Change both or neither (rmp #2750)",
			maxSnapshotValueLen, 1<<30)
	}
	// The two bounds this package holds are distinct and must stay distinct:
	// the prefix bound is the looser structural one, the fold bound the tighter
	// policy one. If they ever met, one of them would be doing nothing.
	if maxSnapshotValueLen >= maxWALValueLen {
		t.Fatalf("maxSnapshotValueLen (%d) must stay strictly below maxWALValueLen (%d): "+
			"the fold bound is the tighter of the two and is checked first",
			maxSnapshotValueLen, uint64(maxWALValueLen))
	}
}

// TestCheckSnapshotFoldableLen_Boundary_2750 pins the predicate at its exact
// boundary, so a later edit cannot tighten or loosen it by one byte unnoticed.
//
// It drives the predicate with a length rather than a value for the same reason
// store/snapshot's own lenguard test does: materialising a gigabyte to move a
// comparison by one is a cost with no evidential return.
func TestCheckSnapshotFoldableLen_Boundary_2750(t *testing.T) {
	t.Parallel()

	if err := checkSnapshotFoldableLen("probe", 0); err != nil {
		t.Fatalf("checkSnapshotFoldableLen(0) = %v, want nil", err)
	}
	if err := checkSnapshotFoldableLen("probe", maxSnapshotValueLen); err != nil {
		t.Fatalf("over-restricted: a value of exactly maxSnapshotValueLen (%d) was refused: %v",
			maxSnapshotValueLen, err)
	}
	err := checkSnapshotFoldableLen("probe", maxSnapshotValueLen+1)
	if err == nil {
		t.Fatalf("checkSnapshotFoldableLen(%d) = nil; store/snapshot's ReadProperties "+
			"refuses that length, so the value could never be folded", maxSnapshotValueLen+1)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("refusal is not typed: got %v, want it to wrap ErrFieldTooLong", err)
	}
	if got, want := err.Error(), "probe is 1073741825 bytes, maximum 1073741824"; !strings.Contains(got, want) {
		t.Fatalf("refusal message %q does not name the field and both lengths (want it to contain %q)", got, want)
	}
}

// TestCommitRefusesUnfoldableValue_2750 is the wiring proof: the guard is
// reached from Commit, and a value in the window #2750 named — above
// store/snapshot's 1 GiB cap, below the WAL prefix's 4 GiB — is refused there
// with the typed sentinel instead of being made durable.
//
// # Why this fixture costs almost nothing
//
// The value is a []byte of maxSnapshotValueLen+1 that is never written to.
// snapshotEncodedScalarLen reads only its len, and the refusal happens before
// encodePropertyValue appends a single byte of it, so the pages are never
// faulted in: the allocation is 1 GiB of address space and a few pages of RSS.
// Removing the guard is what makes it expensive — the encoder then copies the
// whole gigabyte into the WAL scratch buffer before wal.Encode rejects the
// assembled frame.
//
// # What it looks like without the fix
//
// Before the guard this commit did not fail with ErrFieldTooLong. It failed with
// wal.ErrFrameTooLarge — the WAL's own frame ceiling, reached only because this
// particular value is a scalar and its op frame is 17-odd bytes larger than the
// value. That is why this assertion is on the SENTINEL and not merely on
// "an error": the refusal has to come from the bound that knows about the
// checkpoint format, not from an unrelated ceiling that happens to sit nearby
// and does not cover the list case at all.
func TestCommitRefusesUnfoldableValue_2750(t *testing.T) {
	t.Parallel()

	st, cleanup := newFoldableTestStore(t)
	defer cleanup()

	tx := st.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit node: %v", err)
	}

	// One byte over the cap store/snapshot's ReadProperties enforces, and well
	// under the 4 GiB the WAL's uint32 prefix could express: squarely inside the
	// window rmp #2750 named.
	unfoldable := make([]byte, maxSnapshotValueLen+1)

	tx2 := st.Begin()
	if err := tx2.SetNodeProperty("n", "p", lpg.BytesValue(unfoldable)); err != nil {
		t.Fatalf("SetNodeProperty refused while staging (it must not: property values are "+
			"bounded at the encoder, not the API): %v", err)
	}
	err := tx2.Commit()
	if err == nil {
		t.Fatalf("Commit ACCEPTED a %d-byte property value. store/snapshot's ReadProperties "+
			"caps a value at %d, so CaptureGraph would refuse it on every checkpoint, the WAL "+
			"prefix would never be truncated, and the WAL would grow without bound for as long "+
			"as the value lived in the graph (rmp #2750)", len(unfoldable), maxSnapshotValueLen)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("Commit refused with %v, which does not wrap ErrFieldTooLong. The refusal must "+
			"come from the fold bound (checkSnapshotFoldableLen), not from wal.ErrFrameTooLarge: "+
			"the frame ceiling covers scalars only by accident of framing overhead and does not "+
			"cover the list case at all (rmp #2750)", err)
	}
	if errors.Is(err, wal.ErrFrameTooLarge) {
		t.Fatalf("Commit reached wal.Encode before the fold bound refused the value: %v", err)
	}

	// Control: the SAME store still takes an ordinary value, so the guard bounds
	// rather than blocks. A scalar at exactly maxSnapshotValueLen is deliberately
	// NOT the control here — it passes this bound and is then refused by
	// wal.Encode's frame ceiling, because the op frame carries the value plus its
	// framing. The at-cap control that CAN commit is a list, and it is in the
	// soak test.
	tx3 := st.Begin()
	if err := tx3.SetNodeProperty("n", "p", lpg.BytesValue([]byte("ordinary"))); err != nil {
		t.Fatalf("control SetNodeProperty: %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("over-restricted: an ordinary value was refused after the guard was added: %v", err)
	}
}

// TestCommitRefusesUnfoldableEdgeValue_2750 is
// [TestCommitRefusesUnfoldableValue_2750] for the edge-property encoder, which
// is a separate call site of encodePropertyValue and would otherwise be
// unguarded evidence.
func TestCommitRefusesUnfoldableEdgeValue_2750(t *testing.T) {
	t.Parallel()

	st, cleanup := newFoldableTestStore(t)
	defer cleanup()

	tx := st.Begin()
	if err := tx.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit edge: %v", err)
	}

	unfoldable := make([]byte, maxSnapshotValueLen+1)
	tx2 := st.Begin()
	if err := tx2.SetEdgeProperty("a", "b", "p", lpg.BytesValue(unfoldable)); err != nil {
		t.Fatalf("SetEdgeProperty refused while staging: %v", err)
	}
	err := tx2.Commit()
	if err == nil {
		t.Fatalf("Commit ACCEPTED a %d-byte EDGE property value; the snapshot caps it at %d (rmp #2750)",
			len(unfoldable), maxSnapshotValueLen)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("edge-property refusal %v does not wrap ErrFieldTooLong (rmp #2750)", err)
	}
}

// newFoldableTestStore returns a WAL-backed store over a temporary directory,
// with the WAL closed by the returned cleanup.
func newFoldableTestStore(t *testing.T) (*Store[string, float64], func()) {
	t.Helper()
	w, err := wal.Open(t.TempDir() + "/wal")
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return NewStoreWithCodec[string, float64](g, w, NewStringCodec()), func() { _ = w.Close() }
}
