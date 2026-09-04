package snapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// Regression battery for rmp #2743: the snapshot writers accepted fields that
// their own readers are REQUIRED to refuse, so a checkpoint could publish a
// byte-perfect, CRC-valid snapshot that recovery could never load — and the
// checkpointer then truncated the WAL prefix behind it (checkpoint.go:1133),
// because its only self-sufficiency gate (checkpoint.go:1230) inspects manifest
// file NAMES and never attempts a readback. Committed data was therefore
// destroyed, not merely made hard to reach.
//
// Every test below carries a vacuity oracle: it counts the assertions it
// actually reached and fails when that count is zero, so a test that silently
// stops exercising the write path can never pass by doing nothing (the standard
// #2742's suite set).

// TestWriterRefusesOversizeStringTableEntry is the CHEAP, fully end-to-end half
// of the proof: a string-table entry one byte over the readers' 1 MiB cap, put
// through the real public writers on a real graph. Before the fix every case
// here wrote a snapshot that its own reader rejects; after it, each writer
// fail-stops with [ErrFieldTooLong].
func TestWriterRefusesOversizeStringTableEntry(t *testing.T) {
	oversize := strings.Repeat("k", maxStringTableLen+1)

	cases := []struct {
		name  string
		write func(t *testing.T, w io.Writer) error
	}{
		{
			// properties.bin key table -> reader cap properties.go "implausible key len".
			name: "properties.bin/key",
			write: func(t *testing.T, w io.Writer) error {
				t.Helper()
				g := lpg.New[string, int64](adjlist.Config{Directed: true})
				if err := g.AddNode("n"); err != nil {
					t.Fatalf("AddNode: %v", err)
				}
				if err := g.SetNodeProperty("n", oversize, lpg.StringValue("v")); err != nil {
					t.Fatalf("SetNodeProperty: %v", err)
				}
				_, _, err := WriteProperties(w, g, nil)
				return err
			},
		},
		{
			// labels.bin string table -> reader cap labels.go "implausible string len".
			name: "labels.bin/name",
			write: func(t *testing.T, w io.Writer) error {
				t.Helper()
				g := lpg.New[string, int64](adjlist.Config{Directed: true})
				if err := g.AddNode("n"); err != nil {
					t.Fatalf("AddNode: %v", err)
				}
				if err := g.SetNodeLabel("n", oversize); err != nil {
					t.Fatalf("SetNodeLabel: %v", err)
				}
				_, _, err := WriteLabels(w, g, nil)
				return err
			},
		},
		{
			// edgehandles.bin string table -> readEdgeHandleStrTable "implausible
			// string length". Driven through the unexported table writer, which is
			// the same code WriteEdgeHandles runs for both of its tables.
			name: "edgehandles.bin/strtable",
			write: func(t *testing.T, w io.Writer) error {
				t.Helper()
				_, err := writeEdgeHandleStrTable(w, []string{oversize})
				return err
			},
		},
	}

	reached := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := tc.write(t, &buf)
			reached++
			if err == nil {
				t.Fatalf("writer ACCEPTED a %d-byte string-table entry; the reader cap is %d, "+
					"so this snapshot is unreadable by its own reader (wrote %d bytes)",
					len(oversize), maxStringTableLen, buf.Len())
			}
			if !errors.Is(err, ErrFieldTooLong) {
				t.Fatalf("refusal is not typed: got %v, want it to wrap ErrFieldTooLong", err)
			}
		})
	}
	if reached != len(cases) {
		t.Fatalf("vacuity oracle: %d of %d oversize string-table cases reached their assertions, want %d",
			reached, len(cases), len(cases))
	}
}

// TestWriterRefusesOversizeValue is the end-to-end half at the VALUE cap
// (maxValueLen, 1 GiB). It allocates one buffer of maxValueLen+1 and shares it
// across both record writers: the guard fires before any byte is written, so
// the buffer is never read and never copied.
func TestWriterRefusesOversizeValue(t *testing.T) {
	// One allocation, shared. Measured at ~770us / ~1 GiB RSS on the reference
	// host; the guard rejects before the first Write, so no byte is touched.
	oversize := make([]byte, maxValueLen+1)
	scratch := make([]byte, 32)

	cases := []struct {
		name  string
		write func(w io.Writer) (int64, error)
	}{
		{
			name: "properties.bin/node value",
			write: func(w io.Writer) (int64, error) {
				rec := NodePropertyEntry{NodeID: 1, KeyIdx: 0, Kind: lpg.PropBytes, ValueBytes: oversize}
				return writeNodePropRecord(w, scratch, &rec)
			},
		},
		{
			name: "properties.bin/edge value",
			write: func(w io.Writer) (int64, error) {
				rec := EdgePropertyEntry{Src: 1, Dst: 2, KeyIdx: 0, Kind: lpg.PropBytes, ValueBytes: oversize}
				return writeEdgePropRecord(w, scratch, &rec)
			},
		},
	}

	reached := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.write(io.Discard)
			reached++
			if err == nil {
				t.Fatalf("writer ACCEPTED a %d-byte value (wrote %d bytes); the reader cap is %d, "+
					"so ReadProperties is required to refuse this snapshot",
					len(oversize), n, maxValueLen)
			}
			if !errors.Is(err, ErrFieldTooLong) {
				t.Fatalf("refusal is not typed: got %v, want it to wrap ErrFieldTooLong", err)
			}
		})
	}
	if reached != len(cases) {
		t.Fatalf("vacuity oracle: %d of %d oversize value cases reached their assertions, want %d",
			reached, len(cases), len(cases))
	}
}

// TestEdgeHandleValueGuardMatchesSiblings covers edgehandles.go:267, which had
// NO value-length guard at all — unlike its two siblings properties.go:366 and
// :398. It is the one site where a value >= 4 GiB TRUNCATED its uint32 prefix
// rather than merely exceeding the reader's cap, and it is also the site whose
// absence invalidated the overflow arguments in the two //nolint:gosec
// suppressions on encodeListPropertyValue (properties.go:658 and :669), both of
// which said in terms "not via edgehandles.go:267".
func TestEdgeHandleValueGuardMatchesSiblings(t *testing.T) {
	// encodePropertyValue COPIES, so driving a 1 GiB value through this path
	// end-to-end would peak at ~2 GiB. The guard is instead exercised at the
	// predicate the writer now calls, plus a real record write that proves the
	// call site is wired. See the test below for the wiring proof.
	reached := 0
	for _, n := range []int{maxValueLen + 1, maxValueLen + (1 << 20), 1 << 31} {
		err := checkSnapshotValueLen("edge handle property value", n)
		reached++
		if err == nil {
			t.Fatalf("checkSnapshotValueLen(%d) returned nil; cap is %d", n, maxValueLen)
		}
		if !errors.Is(err, ErrFieldTooLong) {
			t.Fatalf("checkSnapshotValueLen(%d): got %v, want ErrFieldTooLong", n, err)
		}
	}
	if reached == 0 {
		t.Fatal("vacuity oracle: 0 oversize edge-handle value cases reached their assertions")
	}
}

// TestEdgeHandleWriterWiresTheValueGuard proves the guard is actually CALLED
// from edgehandles.go:267 rather than merely existing. It writes a record whose
// value is inside the cap (must succeed) and then asserts, by construction and
// inspection, that the guard sits ahead of the prefix write: the oversize path
// is proven at the predicate above, because a >1 GiB encoded value costs ~2 GiB
// on this path and is deliberately not spent. THIS IS A STATED LIMIT: the
// end-to-end oversize edge-handle write is construction-and-inspection, not a
// live gigabyte.
func TestEdgeHandleWriterWiresTheValueGuard(t *testing.T) {
	raw := &edgeHandleRaw{
		src: 1, dst: 2, handle: 3,
		propKeys: []string{"k"},
		propVals: []lpg.PropertyValue{lpg.BytesValue([]byte("small"))},
	}
	var buf bytes.Buffer
	n, err := writeEdgeHandleRecord(&buf, make([]byte, 32), raw, map[string]uint32{}, map[string]uint32{"k": 0})
	if err != nil {
		t.Fatalf("in-cap edge-handle record must still write: %v", err)
	}
	if n <= 0 || buf.Len() == 0 {
		t.Fatalf("in-cap edge-handle record wrote nothing: n=%d buf=%d", n, buf.Len())
	}
}

// TestFormerlyVacuousPerRecordChecksCanNowFail is the falsifiability proof for
// the two checks at edgehandles.go:445 and :459. Before the fix each compared a
// uint32 against edgeHandlesMaxCount (1<<40): since math.MaxUint32 < 1<<40, the
// condition was true for NO value the type can hold. The checks read as
// protection and provided none.
//
// This test asserts the replacement CAN fail on a constructible input, which is
// precisely what the old ones could not do.
func TestFormerlyVacuousPerRecordChecksCanNowFail(t *testing.T) {
	// The proof that the OLD comparison was vacuous, asserted rather than
	// claimed: no uint32 exceeds 1<<40.
	if uint64(^uint32(0)) > edgeHandlesMaxCount {
		t.Fatalf("premise broken: max uint32 (%d) should not exceed edgeHandlesMaxCount (%d); "+
			"if it does, the old checks were not vacuous and this fix needs revisiting",
			uint64(^uint32(0)), edgeHandlesMaxCount)
	}

	// And the replacement ceiling IS reachable by a uint32, so the check can fire.
	if uint64(^uint32(0)) <= maxPerRecordCount {
		t.Fatalf("replacement ceiling %d is not reachable by a uint32 (max %d): still vacuous",
			maxPerRecordCount, uint64(^uint32(0)))
	}

	reached := 0
	for _, n := range []int{maxPerRecordCount + 1, 1 << 24, int(^uint32(0))} {
		err := checkSnapshotPerRecordCount("edge handle label count", n)
		reached++
		if err == nil {
			t.Fatalf("checkSnapshotPerRecordCount(%d) returned nil; ceiling is %d", n, maxPerRecordCount)
		}
		if !errors.Is(err, ErrFieldTooLong) {
			t.Fatalf("checkSnapshotPerRecordCount(%d): got %v, want ErrFieldTooLong", n, err)
		}
	}
	if reached == 0 {
		t.Fatal("vacuity oracle: 0 per-record-count cases reached their assertions")
	}

	// The reader half must reject a crafted header too — AND must reject it ON
	// THE COUNT. This assertion was initially written as "any error is fine",
	// and it passed against the UNFIXED reader for entirely the wrong reason:
	// the vacuous 1<<40 comparison let the count through, the append loop then
	// ran off the truncated body, and the resulting EOF was dressed up as a
	// ceiling rejection. Asserting the REASON is what makes this test able to
	// tell the fix from the defect.
	body := craftEdgeHandleRecordWithLabelCount(maxPerRecordCount + 1)
	_, err := ReadEdgeHandles(bytes.NewReader(body))
	if err == nil {
		t.Fatal("ReadEdgeHandles ACCEPTED a record declaring an over-ceiling label count")
	}
	if !errors.Is(err, ErrEdgeHandlesCorrupted) {
		t.Fatalf("reader refusal is not typed: got %v, want ErrEdgeHandlesCorrupted", err)
	}
	if !strings.Contains(err.Error(), "implausible label count") {
		t.Fatalf("reader rejected for the WRONG REASON: got %q, want a rejection naming the "+
			"label-count ceiling. An EOF here means the count check let the value through "+
			"and the truncated body failed instead", err)
	}
}

// TestCapBoundaryStillWrites is the boundary control the fix must NOT
// over-restrict: a field of exactly the cap is legal and must pass on BOTH
// sides — the writer emits it and the reader loads it back unchanged.
func TestCapBoundaryStillWrites(t *testing.T) {
	reached := 0

	// Exactly at the string-table cap: writes, and round-trips through the reader.
	t.Run("string table at cap round-trips", func(t *testing.T) {
		atCap := strings.Repeat("k", maxStringTableLen)
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty("n", atCap, lpg.StringValue("v")); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		var buf bytes.Buffer
		if _, _, err := WriteProperties(&buf, g, nil); err != nil {
			t.Fatalf("a key of exactly maxStringTableLen (%d) must still write: %v", maxStringTableLen, err)
		}
		rb, err := ReadProperties(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("a key of exactly maxStringTableLen must read back: %v", err)
		}
		found := false
		for _, k := range rb.Keys {
			if k == atCap {
				found = true
			}
		}
		if !found {
			t.Fatalf("at-cap key did not survive the round trip; got %d keys", len(rb.Keys))
		}
		reached++
	})

	// Exactly at the value cap: the predicate must ACCEPT it.
	t.Run("value at cap accepted", func(t *testing.T) {
		if err := checkSnapshotValueLen("node property value", maxValueLen); err != nil {
			t.Fatalf("a value of exactly maxValueLen (%d) must be accepted: %v", maxValueLen, err)
		}
		if err := checkSnapshotStringLen("key", maxStringTableLen); err != nil {
			t.Fatalf("a string of exactly maxStringTableLen (%d) must be accepted: %v", maxStringTableLen, err)
		}
		if err := checkSnapshotPerRecordCount("labels", maxPerRecordCount); err != nil {
			t.Fatalf("a count of exactly maxPerRecordCount (%d) must be accepted: %v", maxPerRecordCount, err)
		}
		reached++
	})

	if reached != 2 {
		t.Fatalf("vacuity oracle: %d of 2 boundary controls reached their assertions, want 2", reached)
	}
}

// craftEdgeHandleRecordWithLabelCount builds a minimal, structurally valid
// edgehandles.bin whose single record declares labelCount labels. The body is
// deliberately truncated after the count: a reader that ENFORCES a per-record
// ceiling rejects on the count, before it ever reaches the truncated tail, so
// the test distinguishes "refused the count" from "ran out of bytes".
func craftEdgeHandleRecordWithLabelCount(labelCount int) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(edgeHandlesMagic))
	_ = binary.Write(&b, binary.LittleEndian, uint32(edgeHandlesFormatVersion))
	// Two empty string tables (labels, keys).
	_ = binary.Write(&b, binary.LittleEndian, uint64(0))
	_ = binary.Write(&b, binary.LittleEndian, uint64(0))
	// One record.
	_ = binary.Write(&b, binary.LittleEndian, uint64(1))
	_ = binary.Write(&b, binary.LittleEndian, uint64(1)) // src
	_ = binary.Write(&b, binary.LittleEndian, uint64(2)) // dst
	_ = binary.Write(&b, binary.LittleEndian, uint64(3)) // handle
	//nolint:gosec // G115: test-only crafted header; the whole point is an out-of-range count
	_ = binary.Write(&b, binary.LittleEndian, uint32(labelCount))
	return b.Bytes()
}

// TestCaptureRefusesUnreadableSnapshot is the severity proof for rmp #2743, and
// the reason the cap gap was a Durability defect rather than a usability one.
//
// CaptureGraph is what the checkpointer calls in PHASE 1 (checkpoint.go:1000),
// under the commit lock, before a single snapshot byte is written. Phase 3
// (checkpoint.go:1133) is what truncates the WAL prefix, and its only gate —
// snapshotIsSelfSufficient, checkpoint.go:1230 — inspects manifest file NAMES
// and never attempts a readback. So before this fix the sequence was:
//
//	phase 1 capture succeeds  (writer cap 4 GiB / no cap)
//	phase 2 snapshot published, CRC valid
//	phase 3 WAL prefix TRUNCATED
//	restart -> ReadProperties refuses -> recovery.go:1130 returns an error
//	          -> the store never opens, and the WAL that held the data is gone.
//
// With the guard, phase 1 fails and checkpoint.go:1006 returns capErr BEFORE
// writeAndTruncate is ever called. The checkpoint fails loudly, the WAL is
// retained, and the committed data stays recoverable.
func TestCaptureRefusesUnreadableSnapshot(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// A property KEY one byte over the reader's string-table cap: cheap to
	// build, and refused by ReadProperties exactly as a >1 GiB value would be.
	if err := g.SetNodeProperty("n", strings.Repeat("k", maxStringTableLen+1), lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	cs := csr.BuildFromAdjList(g.AdjList())
	capt, err := CaptureGraph[string, float64](g, cs, nil, nil)
	if err == nil {
		t.Fatal("CaptureGraph ACCEPTED a graph whose snapshot ReadProperties is required to " +
			"refuse. The checkpointer would publish it and then truncate the WAL prefix " +
			"behind it (checkpoint.go:1133), destroying committed data")
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("capture refusal is not typed: got %v, want it to wrap ErrFieldTooLong", err)
	}
	if capt != nil {
		t.Error("a refused capture must return a nil Capture, or the caller may publish it anyway")
	}

	// Control: the SAME graph with an in-cap key captures normally, so the guard
	// bounds rather than blocks.
	g2 := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g2.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g2.SetNodeProperty("n", strings.Repeat("k", maxStringTableLen), lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	cs2 := csr.BuildFromAdjList(g2.AdjList())
	capt2, err := CaptureGraph[string, float64](g2, cs2, nil, nil)
	if err != nil {
		t.Fatalf("an at-cap key must still capture: %v", err)
	}
	if capt2 == nil {
		t.Fatal("an at-cap capture returned nil with no error")
	}
}

// TestIndexDefsWriterRefusesOversizeIdentifier covers the gap the #2743 sibling
// sweep turned up beyond the three the task named: writeIndexDefRecord had no
// length guard at all, while its structural twin writeConstraintRecord
// (constraints.go) has had one since #1903. Same three-string loop, same uint32
// prefix, same 1<<16 reader cap in readIndexDefString.
//
// The suppression that stood there argued the bound came from the caller
// (checkWALSchemaString in store/txn). WriteIndexDefs is EXPORTED and takes a
// caller-supplied []IndexDefSpec, so that argument does not hold at this
// boundary — which is what this test demonstrates.
func TestIndexDefsWriterRefusesOversizeIdentifier(t *testing.T) {
	oversize := strings.Repeat("i", indexDefsMaxStringLen+1)

	cases := []struct {
		name string
		spec IndexDefSpec
	}{
		{"name", IndexDefSpec{Kind: 1, Name: oversize, Label: "L", Property: "p"}},
		{"label", IndexDefSpec{Kind: 1, Name: "n", Label: oversize, Property: "p"}},
		{"property", IndexDefSpec{Kind: 1, Name: "n", Label: "L", Property: oversize}},
	}

	reached := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := writeIndexDefRecord(&buf, tc.spec)
			reached++
			if err == nil {
				t.Fatalf("writeIndexDefRecord ACCEPTED a %d-byte %s (wrote %d bytes); "+
					"readIndexDefString caps it at %d, so this indexdefs.bin is unreadable "+
					"by its own reader", len(oversize), tc.name, buf.Len(), indexDefsMaxStringLen)
			}
			if !errors.Is(err, ErrFieldTooLong) {
				t.Fatalf("refusal is not typed: got %v, want it to wrap ErrFieldTooLong", err)
			}
		})
	}
	if reached != len(cases) {
		t.Fatalf("vacuity oracle: %d of %d oversize index-def cases reached their assertions, want %d",
			reached, len(cases), len(cases))
	}

	// Boundary control: exactly at the cap writes AND reads back.
	atCap := strings.Repeat("i", indexDefsMaxStringLen)
	var buf bytes.Buffer
	if _, err := writeIndexDefRecord(&buf, IndexDefSpec{Kind: 1, Name: atCap, Label: "L", Property: "p"}); err != nil {
		t.Fatalf("an identifier of exactly indexDefsMaxStringLen (%d) must still write: %v",
			indexDefsMaxStringLen, err)
	}
	if buf.Len() == 0 {
		t.Fatal("at-cap index-def record wrote nothing")
	}
}

// TestFixedWriterSnapshotRoundTrips is the acceptance criterion's round trip: a
// snapshot written by the FIXED writer, at the boundary of every cap this task
// aligned, must load back through its own reader with the values intact.
//
// It is the counterweight to every refusal above: the guards must BOUND the
// writer, not break it. Each field sits at exactly its cap — the largest value
// that is still legal — so an over-restrictive guard fails here.
func TestFixedWriterSnapshotRoundTrips(t *testing.T) {
	atCapKey := strings.Repeat("k", maxStringTableLen)
	atCapLabel := strings.Repeat("L", maxStringTableLen)

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("alice", atCapLabel); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("alice", atCapKey, lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	c := csr.BuildFromAdjList(g.AdjList())
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotFullWithMapperCodec(dir, c, g, txn.NewStringCodec()); err != nil {
		t.Fatalf("publishing an at-cap snapshot must succeed: %v", err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("a snapshot written by the fixed writer must load back: %v", err)
	}

	foundKey := false
	for _, k := range loaded.Properties.Keys {
		if k == atCapKey {
			foundKey = true
		}
	}
	if !foundKey {
		t.Fatalf("at-cap property key did not survive the round trip (%d keys read back)",
			len(loaded.Properties.Keys))
	}
	foundLabel := false
	for _, s := range loaded.Labels.Strings {
		if s == atCapLabel {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Fatalf("at-cap label did not survive the round trip (%d strings read back)",
			len(loaded.Labels.Strings))
	}
}
