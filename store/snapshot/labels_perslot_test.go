package snapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labels_perslot_test.go — the labels.bin version-2 edge record, at the format
// level (rmp #2262).
//
// store/recovery's per_slot_reltype_durability_test.go asserts the OBSERVABLE
// consequence — a typed degree that survives a checkpoint. This file asserts the
// BYTES that make it possible: exactly which records the writer emits, with
// which slot ordinals, in which order. The two are complementary; a format that
// happened to produce right answers by a compensating pair of mistakes would
// pass the first and fail this one.

// perSlotRecordCase is a construction sequence plus the exact record list it
// must serialise to, written out by hand.
type perSlotRecordCase struct {
	name  string
	build func(t *testing.T, g *lpg.Graph[string, int64])
	// wantStrings is the expected string table, in interning order.
	wantStrings []string
	// wantSlots is one entry per expected edge record, in emission order:
	// {slot ordinal, string-table index}. Src/Dst are checked separately
	// because the NodeIDs the mapper assigns are not literals.
	wantSlots [][2]uint32
}

func perSlotRecordCases() []perSlotRecordCase {
	return []perSlotRecordCase{
		{
			// Two parallel slots, one type each. The whole point of the format:
			// version 1 emitted ONE record per (src, dst, type) and could not say
			// that K is on the first slot and M on the second.
			name: "distinct_type_per_slot",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				if err := g.AddEdgeLabeled("a", "b", 0, "K"); err != nil {
					t.Fatal(err)
				}
				if err := g.AddEdgeLabeled("a", "b", 0, "M"); err != nil {
					t.Fatal(err)
				}
			},
			wantStrings: []string{"K", "M"},
			wantSlots:   [][2]uint32{{0, 0}, {1, 1}},
		},
		{
			// One typed slot, one untyped slot. The untyped slot contributes NO
			// record at all — that absence is what stops the type being invented
			// on it at load.
			name: "typed_then_untyped_slot",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				if err := g.AddEdgeLabeled("a", "b", 0, "K"); err != nil {
					t.Fatal(err)
				}
				if err := g.AddEdge("a", "b", 0); err != nil {
					t.Fatal(err)
				}
			},
			wantStrings: []string{"K"},
			wantSlots:   [][2]uint32{{0, 0}},
		},
		{
			// Naming the pair twice: K fills both slots' columns, so M has nowhere
			// per-slot to live and spills to the pair's overflow list. The
			// overflow half is emitted under the reserved sentinel ordinal, after
			// the slot records.
			name: "second_type_spills_to_overflow",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				for i := 0; i < 2; i++ {
					if err := g.AddEdge("a", "b", 0); err != nil {
						t.Fatal(err)
					}
				}
				g.SetEdgeLabel("a", "b", "K")
				g.SetEdgeLabel("a", "b", "M")
			},
			wantStrings: []string{"K", "M"},
			wantSlots:   [][2]uint32{{0, 0}, {1, 0}, {EdgeLabelSlotOverflow, 1}},
		},
		{
			// A Cypher CREATE's shape: the type is written to the slot's column
			// AND recorded against the slot's stable handle. The column record is
			// still emitted — edgehandles.bin owns what the slot IS, but the
			// column is what the pair-level readers consult.
			name: "handle_typed_slot_still_emits_its_column",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				h, err := g.AddEdgeH("a", "b", 0)
				if err != nil {
					t.Fatal(err)
				}
				g.SetEdgeLabel("a", "b", "K")
				g.SetEdgeLabelByHandle("a", "b", h, "K")
			},
			wantStrings: []string{"K"},
			wantSlots:   [][2]uint32{{0, 0}},
		},
		{
			// A handle-carrying slot inserted BEFORE a handle-less one: the
			// adjacency order is (handle, no-handle) but the canonical order is
			// (no-handle, handle), because csr.bin's runs are stably ordered by
			// (destination, handle). The ordinals must follow the CANONICAL order,
			// which is what the recovered adjacency will be in.
			name: "canonical_order_puts_handleless_slot_first",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				if _, err := g.AddEdgeH("a", "b", 0); err != nil {
					t.Fatal(err)
				}
				g.SetEdgeLabel("a", "b", "K")
				if err := g.AddEdgeLabeled("a", "b", 0, "M"); err != nil {
					t.Fatal(err)
				}
			},
			wantStrings: []string{"K", "M"},
			// Adjacency order is [handle-carrying(K), handle-less(M)]; canonical
			// order is [handle-less(M), handle-carrying(K)], so M is ordinal 0.
			wantSlots: [][2]uint32{{0, 1}, {1, 0}},
		},
	}
}

// TestWriteLabels_PerSlotEdgeRecords pins the exact version-2 edge records the
// writer emits for each construction sequence, including the slot ordinals and
// the overflow sentinel.
func TestWriteLabels_PerSlotEdgeRecords(t *testing.T) {
	t.Parallel()
	for _, tc := range perSlotRecordCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
			tc.build(t, g)
			srcID, ok := g.AdjList().Mapper().Lookup("a")
			if !ok {
				t.Fatal(`node "a" not interned`)
			}
			dstID, ok := g.AdjList().Mapper().Lookup("b")
			if !ok {
				t.Fatal(`node "b" not interned`)
			}

			var buf bytes.Buffer
			size, _, err := WriteLabels(&buf, g, nil)
			if err != nil {
				t.Fatalf("WriteLabels: %v", err)
			}
			if int64(buf.Len()) != size {
				t.Errorf("reported size %d != bytes written %d", size, buf.Len())
			}
			if v := binary.LittleEndian.Uint32(buf.Bytes()[4:8]); v != labelsFormatVersion {
				t.Errorf("format version = %d, want %d", v, labelsFormatVersion)
			}

			rb, err := ReadLabels(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("ReadLabels: %v", err)
			}
			if rb.Version != labelsFormatVersion {
				t.Errorf("readback version = %d, want %d", rb.Version, labelsFormatVersion)
			}
			if got := rb.Strings; !slices.Equal(got, tc.wantStrings) {
				t.Errorf("string table = %v, want %v", got, tc.wantStrings)
			}
			if len(rb.EdgeLabels) != len(tc.wantSlots) {
				t.Fatalf("edge records = %d (%v), want %d (%v)",
					len(rb.EdgeLabels), rb.EdgeLabels, len(tc.wantSlots), tc.wantSlots)
			}
			for i, want := range tc.wantSlots {
				got := rb.EdgeLabels[i]
				if got.Src != uint64(srcID) || got.Dst != uint64(dstID) {
					t.Errorf("record %d endpoints = (%d,%d), want (%d,%d)",
						i, got.Src, got.Dst, srcID, dstID)
				}
				if got.Slot != want[0] || got.StringIdx != want[1] {
					t.Errorf("record %d = {slot %d, stringIdx %d}, want {slot %d, stringIdx %d}",
						i, got.Slot, got.StringIdx, want[0], want[1])
				}
			}
		})
	}
}

// TestReadLabels_VersionGate covers the three version outcomes the reader must
// distinguish: the current version parsed per-slot, the previous version parsed
// with the narrower record and flagged per-pair, and anything else rejected.
//
// Accepting version 1 is the deliberate upgrade path (see
// labelsFormatVersionPerSlot): a labels.bin already on disk cannot be rewritten
// retroactively, so rejecting it would make committed data unreachable.
func TestReadLabels_VersionGate(t *testing.T) {
	t.Parallel()

	t.Run("v1-accepted-as-per-pair", func(t *testing.T) {
		t.Parallel()
		buf := &bytes.Buffer{}
		_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
		_ = binary.Write(buf, binary.LittleEndian, uint32(1)) // the OLD version
		_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // one string
		_ = binary.Write(buf, binary.LittleEndian, uint32(1))
		_, _ = buf.WriteString("K")
		_ = binary.Write(buf, binary.LittleEndian, uint64(0)) // no node records
		_ = binary.Write(buf, binary.LittleEndian, uint64(1)) // one edge record
		// A v1 edge record is Src | Dst | StringIdx — twenty bytes, no ordinal.
		_ = binary.Write(buf, binary.LittleEndian, uint64(3)) // Src
		_ = binary.Write(buf, binary.LittleEndian, uint64(4)) // Dst
		_ = binary.Write(buf, binary.LittleEndian, uint32(0)) // StringIdx
		rb, err := ReadLabels(buf)
		if err != nil {
			t.Fatalf("v1 labels.bin rejected: %v", err)
		}
		if rb.Version != 1 {
			t.Errorf("readback version = %d, want 1", rb.Version)
		}
		if len(rb.EdgeLabels) != 1 {
			t.Fatalf("edge records = %d, want 1", len(rb.EdgeLabels))
		}
		rec := rb.EdgeLabels[0]
		if rec.Src != 3 || rec.Dst != 4 || rec.StringIdx != 0 {
			t.Errorf("record = %+v, want {Src:3 Dst:4 StringIdx:0}", rec)
		}
		if rec.Slot != 0 {
			t.Errorf("v1 record Slot = %d, want the zero value (the field does not exist in v1)", rec.Slot)
		}
	})

	for _, bad := range []uint32{0, labelsFormatVersion + 1, 99} {
		t.Run("rejects-version", func(t *testing.T) {
			t.Parallel()
			buf := &bytes.Buffer{}
			_ = binary.Write(buf, binary.LittleEndian, labelsMagic)
			_ = binary.Write(buf, binary.LittleEndian, bad)
			_ = binary.Write(buf, binary.LittleEndian, uint64(0))
			if _, err := ReadLabels(buf); !errors.Is(err, ErrLabelsCorrupted) {
				t.Fatalf("version %d = %v, want ErrLabelsCorrupted", bad, err)
			}
		})
	}
}

// TestApplyLabelsToGraph_SlotOrdinalOutOfRange covers the defensive skip: a
// record naming a slot the live adjacency does not have must be dropped, never
// retargeted at a slot that does exist. Retargeting would reintroduce exactly
// the "type invented on a slot that never carried one" failure the format
// removes.
func TestApplyLabelsToGraph_SlotOrdinalOutOfRange(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 0); err != nil { // ONE slot: ordinal 0 only
		t.Fatal(err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	dstID, _ := g.AdjList().Mapper().Lookup("b")
	rb := LabelsReadback{
		Version: labelsFormatVersion,
		Strings: []string{"K"},
		EdgeLabels: []EdgeLabelEntry{
			{Src: uint64(srcID), Dst: uint64(dstID), Slot: 7, StringIdx: 0},
		},
	}
	if err := ApplyLabelsToGraph(g, rb); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}
	if g.HasEdgeLabel("a", "b", "K") {
		t.Error("a type was attached from a record naming a slot that does not exist")
	}
	if types := g.RelationshipTypesInUse(); len(types) != 0 {
		t.Errorf("RelationshipTypesInUse() = %v, want empty", types)
	}
}

// TestApplyLabelsToGraph_OverflowSentinelRestoresPairHalf covers the other v2
// record kind: the sentinel ordinal must land in the pair's overflow list, which
// every column-typed slot of the pair carries.
func TestApplyLabelsToGraph_OverflowSentinelRestoresPairHalf(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 2; i++ {
		if err := g.AddEdge("a", "b", 0); err != nil {
			t.Fatal(err)
		}
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	dstID, _ := g.AdjList().Mapper().Lookup("b")
	rb := LabelsReadback{
		Version: labelsFormatVersion,
		Strings: []string{"K", "M"},
		EdgeLabels: []EdgeLabelEntry{
			{Src: uint64(srcID), Dst: uint64(dstID), Slot: 0, StringIdx: 0},
			{Src: uint64(srcID), Dst: uint64(dstID), Slot: 1, StringIdx: 0},
			{Src: uint64(srcID), Dst: uint64(dstID), Slot: EdgeLabelSlotOverflow, StringIdx: 1},
		},
	}
	if err := ApplyLabelsToGraph(g, rb); err != nil {
		t.Fatalf("ApplyLabelsToGraph: %v", err)
	}
	kID, ok := g.Registry().Lookup("K")
	if !ok {
		t.Fatal("K not interned")
	}
	mID, ok := g.Registry().Lookup("M")
	if !ok {
		t.Fatal("M not interned")
	}
	// Both slots carry K inline and M through the pair overflow, so both types
	// answer 2 — the state the records describe.
	if n, _ := g.OutDegreeByType("a", kID); n != 2 {
		t.Errorf("OutDegreeByType(a, K) = %d, want 2", n)
	}
	if n, _ := g.OutDegreeByType("a", mID); n != 2 {
		t.Errorf("OutDegreeByType(a, M) = %d, want 2", n)
	}
}

// TestForEachPairSlotRelType_SkipsUntypedSlot pins the ordinal contract at the
// lpg boundary: an untyped slot is skipped but still CONSUMES its ordinal, so a
// pair whose only typed slot is the second reports ordinal 1, not 0.
func TestForEachPairSlotRelType_SkipsUntypedSlot(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 0); err != nil { // untyped, ordinal 0
		t.Fatal(err)
	}
	if err := g.AddEdgeLabeled("a", "b", 0, "K"); err != nil { // typed, ordinal 1
		t.Fatal(err)
	}
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	dstID, _ := g.AdjList().Mapper().Lookup("b")
	type visitRec struct {
		ordinal int
		name    string
	}
	var got []visitRec
	g.ForEachPairSlotRelTypeByID(srcID, dstID, func(ordinal int, name string) {
		got = append(got, visitRec{ordinal, name})
	})
	if len(got) != 1 || got[0].ordinal != 1 || got[0].name != "K" {
		t.Fatalf("visits = %v, want exactly [{1 K}]", got)
	}
}
