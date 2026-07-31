package recovery

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// per_slot_reltype_durability_test.go — the Durability guard for per-slot
// relationship types across a checkpoint (rmp #2262).
//
// A relationship type belongs to the relationship INSTANCE, not to the endpoint
// pair, and since rmp #2258 the in-memory model stores it per adjacency SLOT.
// labels.bin v1 keyed an edge-label record by (src, dst) alone, so a multigraph
// pair's parallel slots folded into one record on disk: a checkpoint could LOSE a
// committed type (two distinct types on one pair round-tripped as one) and could
// INVENT one (a typed slot beside an untyped slot round-tripped as two typed
// slots). Both are Durability/Consistency violations — the graph answered a
// typed-degree query differently before and after a checkpoint.
//
// Every expectation below is a HAND-COMPUTED ABSOLUTE number, not a comparison
// of the recovered graph against itself: a differential oracle proves nothing
// when both arms share the same broken code. The "before" column is asserted
// too, so a regression that corrupts the in-memory model cannot make the
// round-trip look green by degrading both sides equally.

// perSlotShape is one construction sequence plus the absolute typed-degree
// answers it must produce, both in the freshly-built graph and in the graph
// recovered from a checkpoint of it.
type perSlotShape struct {
	name string
	// build runs the construction sequence against a fresh multigraph.
	build func(t *testing.T, g *lpg.Graph[string, int64])
	// wantOutDegree is the total out-degree of "a": the number of adjacency
	// slots "a" holds, summed over every destination.
	wantOutDegree int
	// wantByType maps a relationship type to the number of (a, ·) slots that
	// must carry it. A type absent from the graph must map to 0 explicitly, so
	// the "type invented on load" direction is asserted as strictly as the
	// "type vanished on load" one.
	wantByType map[string]int
	// wantTypesInUse is the sorted set [lpg.Graph.RelationshipTypesInUse] must
	// report. It is a different question from wantByType and it is asserted
	// separately on purpose: "in use" is derived from the adjacency label COLUMN
	// and the pair overflow, never from the by-handle store, so it is the one
	// answer that goes wrong if the durable format stops recording the column of
	// a slot a handle record also covers. Every Cypher-created relationship is
	// such a slot, so that mistake would empty db.relationshipTypes() after a
	// restart while every typed-degree answer stayed correct.
	wantTypesInUse []string
}

func perSlotShapes() []perSlotShape {
	return []perSlotShape{
		{
			// Two distinct types on one pair, each on its own handle-less slot.
			// Measured pre-fix at HEAD: K=2 and M=2 — the pair-keyed record set
			// {K, M} was replayed onto BOTH slots, so each slot claimed both types.
			name: "two_typed_handleless_slots",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdgeLabeled(t, g, "a", "b", "K")
				mustAddEdgeLabeled(t, g, "a", "b", "M")
			},
			wantOutDegree:  2,
			wantByType:     map[string]int{"K": 1, "M": 1},
			wantTypesInUse: []string{"K", "M"},
		},
		{
			// One typed slot beside one untyped slot. Pre-fix this round-tripped
			// as K=2: a type was INVENTED on a slot that never carried one.
			name: "typed_slot_plus_untyped_slot",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdgeLabeled(t, g, "a", "b", "K")
				mustAddEdge(t, g, "a", "b")
			},
			wantOutDegree:  2,
			wantByType:     map[string]int{"K": 1},
			wantTypesInUse: []string{"K"},
		},
		{
			// Two untyped slots named together by SetEdgeLabel, which names the
			// PAIR and therefore every one of its column-typed slots.
			name: "two_untyped_slots_named_together",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdge(t, g, "a", "b")
				mustAddEdge(t, g, "a", "b")
				g.SetEdgeLabel("a", "b", "K")
			},
			wantOutDegree:  2,
			wantByType:     map[string]int{"K": 2},
			wantTypesInUse: []string{"K"},
		},
		{
			// Three slots typed against their stable handles, the shape a Cypher
			// CREATE builds. Durable through edgehandles.bin already; asserted
			// here so a format change cannot silently regress it.
			name: "three_handle_typed_slots",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				for i := 0; i < 3; i++ {
					h := mustAddEdgeH(t, g, "a", "b")
					g.SetEdgeLabel("a", "b", "K")
					g.SetEdgeLabelByHandle("a", "b", h, "K")
				}
			},
			wantOutDegree:  3,
			wantByType:     map[string]int{"K": 3},
			wantTypesInUse: []string{"K"},
		},
		{
			// A slot that carries a handle but NO handle-keyed type record: its
			// type lives in the adjacency label column exactly as a handle-less
			// slot's does, so it is column-typed and needs the same durability.
			name: "handle_slot_typed_by_column",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdgeH(t, g, "a", "b")
				g.SetEdgeLabel("a", "b", "K")
			},
			wantOutDegree:  1,
			wantByType:     map[string]int{"K": 1},
			wantTypesInUse: []string{"K"},
		},
		{
			// A handle-typed slot and a column-typed slot on the SAME pair,
			// carrying DIFFERENT types. The two authorities must both survive
			// and must not bleed into each other.
			name: "handle_typed_beside_column_typed",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				h := mustAddEdgeH(t, g, "a", "b")
				g.SetEdgeLabel("a", "b", "K")
				g.SetEdgeLabelByHandle("a", "b", h, "K")
				mustAddEdgeLabeled(t, g, "a", "b", "M")
			},
			wantOutDegree:  2,
			wantByType:     map[string]int{"K": 1, "M": 1},
			wantTypesInUse: []string{"K", "M"},
		},
		{
			// A pair whose two column-typed slots each carry BOTH types, which is
			// what naming the pair twice means: the second SetEdgeLabel finds no
			// free column-typed slot and spills to the pair's overflow list,
			// which every column-typed slot of the pair carries.
			name: "two_slots_named_twice_overflow",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdge(t, g, "a", "b")
				mustAddEdge(t, g, "a", "b")
				g.SetEdgeLabel("a", "b", "K")
				g.SetEdgeLabel("a", "b", "M")
			},
			wantOutDegree:  2,
			wantByType:     map[string]int{"K": 2, "M": 2},
			wantTypesInUse: []string{"K", "M"},
		},
		{
			// The interleaved counterpart of the shape above: one K relationship
			// and one M relationship, distinguishable only because the type is
			// stored per slot.
			name: "two_slots_named_interleaved",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdge(t, g, "a", "b")
				g.SetEdgeLabel("a", "b", "K")
				mustAddEdge(t, g, "a", "b")
				g.SetEdgeLabel("a", "b", "M")
			},
			wantOutDegree:  2,
			wantByType:     map[string]int{"K": 1, "M": 1},
			wantTypesInUse: []string{"K", "M"},
		},
		{
			// Three parallel slots sharing one type, all handle-less. Guards the
			// ordinal against an off-by-one that would drop the last slot.
			name: "three_typed_handleless_slots_same_type",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdgeLabeled(t, g, "a", "b", "K")
				mustAddEdgeLabeled(t, g, "a", "b", "K")
				mustAddEdgeLabeled(t, g, "a", "b", "K")
			},
			wantOutDegree:  3,
			wantByType:     map[string]int{"K": 3},
			wantTypesInUse: []string{"K"},
		},
		{
			// Two destinations from one source, each a multigraph pair with its
			// own types. Guards the ordinal against being scoped to the SOURCE
			// rather than to the (src, dst) pair.
			name: "two_destinations_each_multi",
			build: func(t *testing.T, g *lpg.Graph[string, int64]) {
				t.Helper()
				mustAddEdgeLabeled(t, g, "a", "b", "K")
				mustAddEdgeLabeled(t, g, "a", "b", "M")
				mustAddEdgeLabeled(t, g, "a", "c", "K")
				mustAddEdge(t, g, "a", "c")
			},
			wantOutDegree:  4,
			wantByType:     map[string]int{"K": 2, "M": 1},
			wantTypesInUse: []string{"K", "M"},
		},
	}
}

// TestRecovery_PerSlotRelType_SurvivesCheckpoint is the acceptance guard for
// rmp #2262: for every construction sequence above, the typed degree answered by
// the freshly-built graph and the typed degree answered by the graph recovered
// from a checkpoint of it must BOTH equal the same hand-computed absolute
// number.
func TestRecovery_PerSlotRelType_SurvivesCheckpoint(t *testing.T) {
	t.Parallel()
	for _, sh := range perSlotShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			g := newMultigraph()
			sh.build(t, g)

			assertShape(t, "before checkpoint", g, sh)
			// Snapshot, then truncate the WAL to zero: the SELF-SUFFICIENT
			// recovery path, where mapper, CSR, labels, properties and edge
			// handles are applied in recovery.Open's own order with no WAL
			// frames to paper over a lossy component.
			writeSelfSufficientSnapshot(t, dir, g)

			res, err := Open[string, int64](dir, Options[string, int64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			assertShape(t, "after recovery", res.Graph, sh)
		})
	}
}

// assertShape checks the absolute out-degree and per-type degree of "a" in g.
func assertShape(t *testing.T, phase string, g *lpg.Graph[string, int64], sh perSlotShape) {
	t.Helper()
	if got, ok := g.OutDegree("a"); !ok || got != sh.wantOutDegree {
		t.Errorf("%s: OutDegree(a) = %d (ok=%v), want %d", phase, got, ok, sh.wantOutDegree)
	}
	for relType, want := range sh.wantByType {
		if got := typedOutDegree(g, "a", relType); got != want {
			t.Errorf("%s: OutDegreeByType(a, %q) = %d, want %d", phase, relType, got, want)
		}
	}
	// Every type NOT named by the shape must answer 0, so a type invented by the
	// round trip is caught as loudly as a type lost by it.
	for _, relType := range []string{"K", "M", "Z"} {
		if _, named := sh.wantByType[relType]; named {
			continue
		}
		if got := typedOutDegree(g, "a", relType); got != 0 {
			t.Errorf("%s: OutDegreeByType(a, %q) = %d, want 0 (type never attached)",
				phase, relType, got)
		}
	}
	got := g.RelationshipTypesInUse()
	sort.Strings(got)
	if !slices.Equal(got, sh.wantTypesInUse) {
		t.Errorf("%s: RelationshipTypesInUse() = %v, want %v", phase, got, sh.wantTypesInUse)
	}
}

// typedOutDegree returns the number of out-slots of src carrying relType, or 0
// when the type was never interned in g (which is itself an answer: the type is
// absent).
func typedOutDegree(g *lpg.Graph[string, int64], src, relType string) int {
	lid, ok := g.Registry().Lookup(relType)
	if !ok {
		return 0
	}
	n, ok := g.OutDegreeByType(src, lid)
	if !ok {
		return 0
	}
	return n
}

// newMultigraph returns the directed multigraph every shape is built on.
func newMultigraph() *lpg.Graph[string, int64] {
	return lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
}

func mustAddEdge(t *testing.T, g *lpg.Graph[string, int64], src, dst string) {
	t.Helper()
	if err := g.AddEdge(src, dst, 0); err != nil {
		t.Fatalf("AddEdge(%q,%q): %v", src, dst, err)
	}
}

func mustAddEdgeLabeled(t *testing.T, g *lpg.Graph[string, int64], src, dst, relType string) {
	t.Helper()
	if err := g.AddEdgeLabeled(src, dst, 0, relType); err != nil {
		t.Fatalf("AddEdgeLabeled(%q,%q,%q): %v", src, dst, relType, err)
	}
}

func mustAddEdgeH(t *testing.T, g *lpg.Graph[string, int64], src, dst string) uint64 {
	t.Helper()
	h, err := g.AddEdgeH(src, dst, 0)
	if err != nil {
		t.Fatalf("AddEdgeH(%q,%q): %v", src, dst, err)
	}
	return h
}

// TestRecovery_LabelsV1Snapshot_StillOpens is the upgrade-path guard for the
// labels.bin version bump (rmp #2262). testdata/labels_v1_multigraph holds a
// REAL snapshot directory written by the version-1 writer — generated before the
// format change, not synthesised afterwards — carrying the four multigraph
// shapes this task is about.
//
// The contract it pins is deliberate and documented next to the code
// (snapshot.labelsFormatVersionPerSlot): a version-1 file is ACCEPTED and read
// with the PER-PAIR semantics it was written with, not rejected and not
// reinterpreted. Rejecting it would make committed data unreachable after an
// upgrade for no gain, because the per-slot information was never written to the
// file and cannot be recovered from it.
//
// The expectations below are therefore the numbers the v1 format can produce,
// hand-derived from those semantics — INCLUDING the ones that are wrong about
// the graph that was snapshotted. That is the honest statement of what a v1 file
// can carry: its loss is frozen, not repaired. Node "a" has four out-slots (two
// to "b", two to "c"); replaying the pair-keyed records types every free
// column-typed slot of each pair, so all four report K and the two a→b slots
// report M as well, where the truth was K=2 and M=1.
func TestRecovery_LabelsV1Snapshot_StillOpens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "labels_v1_multigraph"))); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	// The fixture must genuinely be version 1, or this test proves nothing.
	raw, err := os.ReadFile(filepath.Join(dir, "snapshot", snapshot.LabelsFile)) //nolint:gosec // fixture under t.TempDir
	if err != nil {
		t.Fatalf("read fixture labels.bin: %v", err)
	}
	if len(raw) < 8 {
		t.Fatalf("fixture labels.bin too short: %d bytes", len(raw))
	}
	if v := binary.LittleEndian.Uint32(raw[4:8]); v != 1 {
		t.Fatalf("fixture labels.bin format version = %d, want 1 (fixture is not a v1 file)", v)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open on a v1 labels.bin: %v", err)
	}
	g := res.Graph

	for _, want := range []struct {
		src           string
		outDegree     int
		k, m          int
		absentIsZeroZ bool
	}{
		// Pair-keyed replay types every column-typed slot of the pair: both a→b
		// slots take K and then M (the second type spills to the pair overflow,
		// which every column-typed slot of the pair carries), and both a→c slots
		// take K. Truth was K=2, M=1 — the v1 file cannot express it.
		{src: "a", outDegree: 4, k: 4, m: 2, absentIsZeroZ: true},
		// Two untyped slots named together: v1 already round-tripped this one
		// correctly, because naming the pair IS what happened.
		{src: "d", outDegree: 2, k: 2, m: 0, absentIsZeroZ: true},
		// Three handle-typed slots: durable through edgehandles.bin, unaffected
		// by the labels.bin version.
		{src: "f", outDegree: 3, k: 3, m: 0, absentIsZeroZ: true},
	} {
		if got, ok := g.OutDegree(want.src); !ok || got != want.outDegree {
			t.Errorf("v1 fixture: OutDegree(%q) = %d (ok=%v), want %d",
				want.src, got, ok, want.outDegree)
		}
		if got := typedOutDegree(g, want.src, "K"); got != want.k {
			t.Errorf("v1 fixture: OutDegreeByType(%q, K) = %d, want %d", want.src, got, want.k)
		}
		if got := typedOutDegree(g, want.src, "M"); got != want.m {
			t.Errorf("v1 fixture: OutDegreeByType(%q, M) = %d, want %d", want.src, got, want.m)
		}
		if want.absentIsZeroZ {
			if got := typedOutDegree(g, want.src, "Z"); got != 0 {
				t.Errorf("v1 fixture: OutDegreeByType(%q, Z) = %d, want 0", want.src, got)
			}
		}
	}
	// The node label the fixture carries must survive too: the node-record layout
	// is unchanged by the bump, and this proves the v1 branch parses the whole
	// file rather than bailing out after the header.
	if labs := g.NodeLabels("a"); len(labs) != 1 || labs[0] != "N" {
		t.Errorf("v1 fixture: NodeLabels(a) = %v, want [N]", labs)
	}

	// Read old, write NEW: re-checkpointing the recovered graph must emit the
	// current version, so a store upgrades itself on its next checkpoint.
	out := t.TempDir()
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFullWithMapperCodec(
		filepath.Join(out, "snapshot"), cs, g, txn.NewStringCodec()); err != nil {
		t.Fatalf("re-checkpoint: %v", err)
	}
	rewritten, err := os.ReadFile(filepath.Join(out, "snapshot", snapshot.LabelsFile)) //nolint:gosec // t.TempDir
	if err != nil {
		t.Fatalf("read rewritten labels.bin: %v", err)
	}
	if v := binary.LittleEndian.Uint32(rewritten[4:8]); v != 2 {
		t.Errorf("re-checkpointed labels.bin format version = %d, want 2", v)
	}
}

// TestRecovery_PerSlotRelType_LabelsByteStable pins the ordinal's idempotence on
// a MULTIGRAPH: write → recover → write must produce a byte-identical
// labels.bin.
//
// TestRecovery_V3Snapshot_RoundTripByteStable already asserts this property, but
// only over a SIMPLE graph, where every pair has exactly one slot and every
// ordinal is 0 — so it could not have caught an ordinal that shifts across a
// round trip. This covers the case the ordinal exists for: if the write-side
// canonical order and the order the CSR replay produces ever diverged, the
// second write would emit different ordinals and the bytes would drift.
func TestRecovery_PerSlotRelType_LabelsByteStable(t *testing.T) {
	t.Parallel()
	for _, sh := range perSlotShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			g := newMultigraph()
			sh.build(t, g)
			snapDir := writeSelfSufficientSnapshot(t, dir, g)
			first, err := os.ReadFile(filepath.Join(snapDir, snapshot.LabelsFile)) //nolint:gosec // t.TempDir
			if err != nil {
				t.Fatalf("read first labels.bin: %v", err)
			}
			if v := binary.LittleEndian.Uint32(first[4:8]); v != 2 {
				t.Fatalf("labels.bin format version = %d, want 2", v)
			}

			res, err := Open[string, int64](dir, Options[string, int64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			out := t.TempDir()
			cs := csr.BuildFromAdjList(res.Graph.AdjList())
			if err := snapshot.WriteSnapshotFull(filepath.Join(out, "snapshot"), cs, res.Graph); err != nil {
				t.Fatalf("re-checkpoint: %v", err)
			}
			second, err := os.ReadFile(filepath.Join(out, "snapshot", snapshot.LabelsFile)) //nolint:gosec // t.TempDir
			if err != nil {
				t.Fatalf("read second labels.bin: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("labels.bin drifted across a round trip: %d -> %d bytes",
					len(first), len(second))
			}
		})
	}
}
