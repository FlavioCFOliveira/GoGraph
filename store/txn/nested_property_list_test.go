package txn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// This file is the regression gate for rmp #2783: a property value that is a
// list containing a list.
//
// The task was raised on the premise that such an element "comes back empty
// rather than as the list that was committed". Executed, the outcome is worse
// than that and in a different place. The encoder wrote the element as
// (kind 7 | length 0) — the inner list was destroyed at WRITE time, not at read
// time — and both WAL decoders then refuse kind 7 as an unknown element kind.
// That refusal lands INSIDE an already-committed v3 transaction, so replay stops
// there and drops the acknowledged transaction AND every transaction committed
// after it, while Open returns a nil error and reports the tail clean. Nothing
// ever comes back empty; a whole WAL suffix goes missing in silence.
//
// The gate is therefore in four parts:
//
//  1. TestNestedPropertyList_HistoricalBytesAreUnrecoverable_2783 — the exact
//     bytes the old encoder produced, and the proof that the value is gone
//     rather than merely unread. This is the evidence behind the existing-store
//     answer: no migration can recover what was never written.
//  2. TestCheckFlatPropertyList_2783 — the predicate itself, over the shapes
//     that must pass and the shapes that must not.
//  3. TestCommitRefusesNestedPropertyList_2783 and its edge and by-handle
//     siblings — the guard is WIRED into the commit path at every call site
//     that carries a property value.
//  4. TestCommitRefusesNestedPropertyList_NothingDurable_2783 — the refusal
//     buffers nothing durable, so the store is not left with a frame a later
//     replay would choke on.
//
// The durability half (a WAL-only recovery round-trips with no suffix loss) is
// in store/recovery/nested_property_list_test.go, which is where the replayer
// can be imported; the checkpoint half is in
// store/checkpoint/nested_property_list_test.go.

// nestedListValue is the fixture: a two-element list whose FIRST element is
// itself a list. The trailing scalar matters — it is what made the old failure
// mode legible, because the frame it produced was well-formed right up to the
// nested element and then undecodable.
func nestedListValue() lpg.PropertyValue {
	return lpg.ListValue([]lpg.PropertyValue{
		lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)}),
		lpg.StringValue("tail"),
	})
}

// TestNestedPropertyList_HistoricalBytesAreUnrecoverable_2783 pins what a WAL
// written by any released version of this package already holds for a nested
// list, and what can be done about it: nothing.
//
// The byte string is not invented. It is what encodePropertyValue emitted for
// lpg.ListValue([lpg.ListValue([1, 2])]) before this fix, captured by running
// the old encoder:
//
//	07          outer kind = PropList
//	01 00 00 00 element count = 1
//	07          element 0 kind = PropList
//	00 00 00 00 element 0 payload length = ZERO
//
// The inner list's two integers are absent. They were never encoded, so they
// are not merely unreadable — they do not exist on disk. That is the whole
// existing-store answer: a migration tool could at best learn that SOME list
// was lost, never which one, and it could not even learn how deeply it was
// nested, because a three-level nest produces these identical ten bytes.
//
// The assertion is that the decoder ERRORS. It must never return an empty list,
// which would hand a caller a value the graph never held and present the loss as
// a successful read. This assertion is deliberately independent of how recovery
// classifies the resulting tail error, so it stays valid whatever is decided
// about that classification.
func TestNestedPropertyList_HistoricalBytesAreUnrecoverable_2783(t *testing.T) {
	t.Parallel()

	historical := []byte{
		byte(lpg.PropList),
		0x01, 0x00, 0x00, 0x00,
		byte(lpg.PropList),
		0x00, 0x00, 0x00, 0x00,
	}

	// The bytes really are what this package's encoder produces for the outer
	// shape: encode a list with one EMPTY-STRING element, which is the only
	// legitimate value whose encoding is the same length, and confirm the two
	// differ in exactly one byte — the element kind. This keeps the fixture
	// honest against a future format change instead of freezing a literal.
	flatSameLen, err := encodePropertyValue(nil, lpg.ListValue([]lpg.PropertyValue{lpg.StringValue("")}))
	if err != nil {
		t.Fatalf("encoding a one-empty-string list must succeed: %v", err)
	}
	if len(flatSameLen) != len(historical) {
		t.Fatalf("fixture is stale: a one-empty-string list encodes to %d bytes, the historical "+
			"nested-list bytes are %d; the wire format moved and this fixture no longer models it",
			len(flatSameLen), len(historical))
	}
	diffs := 0
	for i := range historical {
		if historical[i] != flatSameLen[i] {
			diffs++
			if i != 5 {
				t.Fatalf("fixture is stale: the two encodings differ at offset %d, expected only "+
					"the element-kind byte at offset 5", i)
			}
		}
	}
	if diffs != 1 {
		t.Fatalf("fixture is stale: expected exactly one differing byte (the element kind), got %d", diffs)
	}

	got, _, derr := decodePropertyValue(historical)
	if derr == nil {
		t.Fatalf("decodePropertyValue ACCEPTED the historical nested-list bytes and returned a %v. "+
			"A zero-length PropList element carries no inner list, so any value returned here is "+
			"fabricated: the decoder must refuse rather than present the loss as a successful read "+
			"(rmp #2783)", got.Kind())
	}
	if !strings.Contains(derr.Error(), "unknown kind") {
		t.Fatalf("decode refusal %q does not name the unknown element kind; the diagnostic is the "+
			"only breadcrumb an operator of an affected store gets", derr)
	}
}

// TestCheckFlatPropertyList_2783 drives the predicate directly, over the shapes
// that must be accepted as well as the ones that must be refused.
//
// The accepted cases are not filler. A list of strings, an EMPTY list and a
// bare scalar are the shapes an ordinary caller writes, and a guard that refused
// any of them would be a far worse defect than the one it fixes.
func TestCheckFlatPropertyList_2783(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		value      lpg.PropertyValue
		wantRefuse bool
	}{
		{"scalar-string", lpg.StringValue("x"), false},
		{"scalar-int", lpg.Int64Value(7), false},
		{"scalar-bytes", lpg.BytesValue([]byte{1, 2}), false},
		{"list-empty", lpg.ListValue(nil), false},
		{"list-flat-scalars", lpg.ListValue([]lpg.PropertyValue{
			lpg.StringValue("a"), lpg.Int64Value(1), lpg.BoolValue(true),
		}), false},
		{"list-nested-first", nestedListValue(), true},
		{"list-nested-last", lpg.ListValue([]lpg.PropertyValue{
			lpg.StringValue("a"), lpg.ListValue(nil),
		}), true},
		{"list-nested-empty-inner", lpg.ListValue([]lpg.PropertyValue{lpg.ListValue(nil)}), true},
		{"list-nested-only", lpg.ListValue([]lpg.PropertyValue{
			lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1)}),
		}), true},
	}

	reachedRefusal, reachedAccept := 0, 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkFlatPropertyList(tc.value)
			switch {
			case tc.wantRefuse && err == nil:
				t.Fatal("checkFlatPropertyList accepted a nested list; the WAL encoder writes such " +
					"an element with a zero-length payload and destroys it (rmp #2783)")
			case !tc.wantRefuse && err != nil:
				t.Fatalf("over-restricted: checkFlatPropertyList refused a legitimate value: %v", err)
			}
			if err != nil && !errors.Is(err, ErrNestedPropertyList) {
				t.Fatalf("refusal is not typed: got %v, want it to wrap ErrNestedPropertyList", err)
			}
		})
		if tc.wantRefuse {
			reachedRefusal++
		} else {
			reachedAccept++
		}
	}
	// An oracle that cannot fail proves nothing: assert the table really carried
	// both directions, so a future edit cannot leave only the easy half.
	if reachedRefusal == 0 || reachedAccept == 0 {
		t.Fatalf("vacuity oracle: the table must exercise both directions, got %d refuse / %d accept",
			reachedRefusal, reachedAccept)
	}

	// The message must name the offending element, so a caller with a long list
	// can find it.
	err := checkFlatPropertyList(lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(1), lpg.Int64Value(2), lpg.ListValue(nil),
	}))
	if err == nil {
		t.Fatal("expected a refusal for a list whose third element is a list")
	}
	if !strings.Contains(err.Error(), "element 2") {
		t.Fatalf("refusal %q does not name the offending element index", err)
	}
}

// TestCommitRefusesNestedPropertyList_2783 is the wiring proof for a NODE
// property: the guard is reached from Commit, and the value is refused with the
// typed sentinel instead of being made durable.
//
// Staging must NOT refuse it. Property values are bounded at the encoder in this
// package, not at the API boundary — Tx.validateProperty delegates to an
// optional lpg.SchemaValidator that is nil by default, so nothing at the
// boundary looks at the value's shape. The assertion that staging succeeds is
// therefore load-bearing: it records where the refusal lives and would catch a
// well-meaning future edit that moved it to the boundary, where the
// *PreValidated entry points the Cypher engine uses would bypass it.
func TestCommitRefusesNestedPropertyList_2783(t *testing.T) {
	t.Parallel()

	st, cleanup := newNestedListTestStore(t)
	defer cleanup()

	tx := st.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit node: %v", err)
	}

	tx2 := st.Begin()
	if err := tx2.SetNodeProperty("n", "p", nestedListValue()); err != nil {
		t.Fatalf("SetNodeProperty refused while staging (it must not: a property value's shape is "+
			"bounded at the encoder, which is the one point the PreValidated entry points also "+
			"pass): %v", err)
	}
	err := tx2.Commit()
	if err == nil {
		t.Fatal("Commit ACCEPTED a list containing a list. The encoder writes the nested element " +
			"as (kind 7 | length 0), so the inner list never reaches the WAL; both decoders then " +
			"refuse the unknown element kind, and replay stops inside this already-committed " +
			"transaction and drops it together with every transaction after it (rmp #2783)")
	}
	if !errors.Is(err, ErrNestedPropertyList) {
		t.Fatalf("Commit refused with %v, which does not wrap ErrNestedPropertyList", err)
	}
	if errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("the refusal came from the LENGTH bound (%v). A nested list is refused for its "+
			"shape at any size, and its measured length is meaningless — "+
			"snapshotEncodedValueLen sizes a nested element as zero, so it reads SMALL", err)
	}

	// Control: the same store still takes a flat list, so the guard bounds
	// rather than blocks.
	tx3 := st.Begin()
	if err := tx3.SetNodeProperty("n", "p", lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(1), lpg.StringValue("two"), lpg.BoolValue(true),
	})); err != nil {
		t.Fatalf("control SetNodeProperty: %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("over-restricted: an ordinary flat list was refused after the guard was added: %v", err)
	}
}

// TestCommitRefusesNestedPropertyList_Edge_2783 is the same wiring proof for
// [Tx.SetEdgeProperty], a separate call site of encodePropertyValue.
func TestCommitRefusesNestedPropertyList_Edge_2783(t *testing.T) {
	t.Parallel()

	st, cleanup := newNestedListTestStore(t)
	defer cleanup()

	tx := st.Begin()
	if err := tx.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit edge: %v", err)
	}

	tx2 := st.Begin()
	if err := tx2.SetEdgeProperty("a", "b", "p", nestedListValue()); err != nil {
		t.Fatalf("SetEdgeProperty refused while staging: %v", err)
	}
	err := tx2.Commit()
	if err == nil {
		t.Fatal("Commit ACCEPTED a nested list as an EDGE property (rmp #2783)")
	}
	if !errors.Is(err, ErrNestedPropertyList) {
		t.Fatalf("edge-property refusal %v does not wrap ErrNestedPropertyList", err)
	}
}

// TestCommitRefusesNestedPropertyList_EdgeByHandle_2783 covers the third and
// last op kind that carries a property value, OpSetEdgePropertyByHandle. It is
// encoded by the same function as OpSetEdgeProperty plus a trailing handle, but
// it reaches it through a different [Tx] method and a different arm of the op
// dispatch, so leaving it ungated would leave the claim "every value-bearing op
// is covered" resting on a code reading rather than on a run.
func TestCommitRefusesNestedPropertyList_EdgeByHandle_2783(t *testing.T) {
	t.Parallel()

	// A weight codec is required here and not in the sibling tests: the
	// by-handle path emits OpAddEdgeH, which persists a weight unconditionally
	// and refuses a store that cannot encode one.
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := NewStoreWithOptions[string, float64](g, w, Options[string, float64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewFloat64WeightCodec(),
	})

	const handle = uint64(1)
	tx := st.Begin()
	if err := tx.AddEdgeWithHandle("a", "b", 0, handle); err != nil {
		t.Fatalf("AddEdgeWithHandle: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit edge: %v", err)
	}

	tx2 := st.Begin()
	if err := tx2.SetEdgePropertyByHandle("a", "b", handle, "p", nestedListValue()); err != nil {
		t.Fatalf("SetEdgePropertyByHandle refused while staging: %v", err)
	}
	err = tx2.Commit()
	if err == nil {
		t.Fatal("Commit ACCEPTED a nested list as an edge property set BY HANDLE (rmp #2783)")
	}
	if !errors.Is(err, ErrNestedPropertyList) {
		t.Fatalf("by-handle refusal %v does not wrap ErrNestedPropertyList", err)
	}
}

// TestCommitRefusesNestedPropertyList_NothingDurable_2783 asserts the refusal
// costs nothing durable: the WAL is byte-for-byte the size it was before the
// refused transaction.
//
// This is the assertion that distinguishes a refusal from a failure. A guard
// that rejected the value only AFTER the frame had been appended would leave the
// very frame whose replay stops recovery — the store would be refused a value
// and poisoned by it in the same call.
func TestCommitRefusesNestedPropertyList_NothingDurable_2783(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := NewStoreWithCodec[string, float64](g, w, NewStringCodec())

	tx := st.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit node: %v", err)
	}
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	before := fi.Size()

	tx2 := st.Begin()
	if err := tx2.SetNodeProperty("n", "p", nestedListValue()); err != nil {
		t.Fatalf("SetNodeProperty (staging): %v", err)
	}
	if err := tx2.Commit(); err == nil {
		t.Fatal("Commit ACCEPTED a nested list (rmp #2783)")
	}

	fi2, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal 2: %v", err)
	}
	if fi2.Size() != before {
		t.Fatalf("the refused transaction wrote %d bytes to the WAL; a refusal must buffer nothing "+
			"durable, or the store is left holding the exact frame whose replay stops recovery "+
			"(was %d, now %d)", fi2.Size()-before, before, fi2.Size())
	}

	// And the store is still usable afterwards: the refusal is not a poison pill.
	tx3 := st.Begin()
	if err := tx3.SetNodeProperty("n", "p", lpg.StringValue("ok")); err != nil {
		t.Fatalf("SetNodeProperty after a refusal: %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("the store is unusable after a refused transaction: %v", err)
	}
}

// newNestedListTestStore returns a WAL-backed store over a temporary directory,
// with the WAL closed by the returned cleanup.
func newNestedListTestStore(t *testing.T) (*Store[string, float64], func()) {
	t.Helper()
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return NewStoreWithCodec[string, float64](g, w, NewStringCodec()), func() { _ = w.Close() }
}
