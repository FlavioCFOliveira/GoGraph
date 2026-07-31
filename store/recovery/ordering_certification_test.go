package recovery

// ordering_certification_test.go — certification for the CSR within-source
// ordering (rmp #2143).
//
// The #2139 SPIKE established that the crash battery this task originally named
// as its acceptance evidence is largely FALSE COMFORT for an ordering change:
//
//   - internal/crashinject/ contains no graph-shape assertions at all; it tests
//     the harness (subprocess spawn, SIGKILL detection, timeout disambiguation).
//   - every store/recovery crash test funnels through graphFingerprint, which
//     SORTS edges by destination and reports edge labels per (src,dst) pair, so
//     it is blind both to within-source order and to which parallel slot carries
//     which label. (Task #2270 has since given graphFingerprint per-slot
//     resolution — it orders slots by the total key (destination, handle) and
//     emits each slot's handle plus its per-handle labels and properties — so it
//     is no longer blind to slot ASSIGNMENT. It remains deliberately blind to
//     within-source PHYSICAL order, which is what orderedFingerprint below
//     pins.)
//   - every byte-equality test (snapshot, csrfile, csr cross-process) is a SELF
//     comparison — build twice, or parent vs child — which a deterministic
//     reorder passes unchanged.
//   - TestRecovery_V3Snapshot_RoundTripByteStable is the one guard that is not
//     blind, but its fixture is 4 edges with max out-degree 2.
//
// This file supplies what was missing: a high-degree, parallel-edge fixture for
// the byte-stability guard; an explicit assertion that the recovery REPLAY
// TRAJECTORY (bulk, in csr.bin order) yields the same durable bytes as the
// original interleaved insertion trajectory; an ORDER-PRESERVING fingerprint that
// can see slot identity; and a legacy-shaped UNORDERED snapshot reopened by the
// current reader.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// orderedFingerprint is the order-PRESERVING companion to [graphFingerprint].
// It records each source's out-neighbours in their ACTUAL adjacency slot order,
// together with the per-slot handle, so it can detect a permutation and a
// mis-assigned parallel slot — the two things graphFingerprint cannot see.
//
// Sources are still visited in a canonical order (by key) because mapper walk
// order is a separate concern; what is pinned here is the WITHIN-SOURCE order.
func orderedFingerprint(t *testing.T, g *lpg.Graph[string, int64]) string {
	t.Helper()
	var sb strings.Builder
	type nodeRec struct {
		key string
		id  graph.NodeID
	}
	var nodes []nodeRec
	g.AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool {
		nodes = append(nodes, nodeRec{key: k, id: id})
		return true
	})
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].key < nodes[j].key })
	for _, n := range nodes {
		nb, _, handles := g.AdjList().LoadEntryH(n.id)
		if len(nb) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "N %s\n", n.key)
		for i, dstID := range nb {
			dstKey, _ := g.AdjList().Mapper().Resolve(dstID)
			var h uint64
			if handles != nil {
				h = handles[i]
			}
			// The slot INDEX is printed, so a permutation changes the string.
			fmt.Fprintf(&sb, " S%d -> %s h=%d\n", i, dstKey, h)
		}
	}
	return sb.String()
}

// stripEdgeHandles rewrites every per-slot handle in a [graphFingerprint] to a
// placeholder, so two fingerprints can be compared on node / label / property /
// edge / weight identity alone. It exists for exactly one caller — the legacy
// pre-#2141 snapshot fixture, whose csr.bin carries no handle block — and must
// not be used to paper over a handle divergence in a format that does carry one.
func stripEdgeHandles(fingerprint string) string {
	return edgeHandleField.ReplaceAllString(fingerprint, "h=* ")
}

// edgeHandleField matches the handle field of a fingerprint slot line
// ("  S h=<handle> w=<weight>").
var edgeHandleField = regexp.MustCompile(`h=\d+ `)

// buildHighDegreeGraph writes a graph with a source whose out-degree is well past
// the ordering path's insertion-sort cutoff, plus parallel edges, inserting each
// source's neighbours in DESCENDING destination-key order so the CSR ordering has
// real work to do and the recovery replay trajectory genuinely differs from the
// insertion trajectory.
func buildHighDegreeGraph(t *testing.T, dir string) (*lpg.Graph[string, int64], *wal.Writer) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions(g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	for s := 0; s < 3; s++ {
		src := fmt.Sprintf("s%02d", s)
		for d := 79; d >= 0; d-- { // descending: 80 neighbours, past the cutoff
			dst := fmt.Sprintf("d%02d", d)
			tx := store.Begin()
			if err := tx.SetNodeLabel(src, "S"); err != nil {
				t.Fatal(err)
			}
			if err := tx.AddEdge(src, dst, int64(d)); err != nil {
				t.Fatal(err)
			}
			if err := tx.SetEdgeLabel(src, dst, "E"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Multi-property edges: this is what exposed the map-iteration determinism
	// defect fixed alongside #2141, so keep it in the certification fixture.
	for _, k := range []string{"alpha", "beta", "gamma"} {
		if err := g.SetEdgeProperty("s00", "d40", k, lpg.Int64Value(7)); err != nil {
			t.Fatal(err)
		}
	}
	return g, w
}

// TestRecovery_HighDegreeSource_RoundTripByteStable is
// TestRecovery_V3Snapshot_RoundTripByteStable re-parameterised past the ordering
// path's cutoff. With a 4-edge, max-out-degree-2 fixture the guard passes for any
// ordering rule; with 80 neighbours per source inserted in descending order it
// actually exercises one.
func TestRecovery_HighDegreeSource_RoundTripByteStable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g, w := buildHighDegreeGraph(t, dir)

	cs := csr.BuildFromAdjList(g.AdjList())
	if !cs.RunsOrdered() {
		t.Fatal("fixture CSR is not ordered; the ordering invariant does not hold")
	}
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull (first): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	originals := captureComponentBytes(t, snapDir)
	before := orderedFingerprint(t, g)

	// Recover from the snapshot alone. ApplyCSRToGraph replays edges in BULK, in
	// csr.bin order — a different trajectory from the interleaved, descending
	// insertion above — so this is the case that can expose a history-dependent
	// durable artefact.
	if err := os.Truncate(filepath.Join(dir, "wal"), 0); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	cs2 := csr.BuildFromAdjList(res.Graph.AdjList())
	snapDir2 := filepath.Join(dir, "snapshot2")
	if err := snapshot.WriteSnapshotFull(snapDir2, cs2, res.Graph); err != nil {
		t.Fatalf("WriteSnapshotFull (second): %v", err)
	}
	rewritten := captureComponentBytes(t, snapDir2)

	for _, name := range []string{snapshot.CSRFile, snapshot.LabelsFile,
		snapshot.PropertiesFile, snapshot.MapperFile} {
		if !bytes.Equal(originals[name], rewritten[name]) {
			t.Errorf("component %q drifted across a high-degree round-trip: %d -> %d bytes",
				name, len(originals[name]), len(rewritten[name]))
		}
	}

	// The recovered adjacency's within-source order is the csr.bin order, which is
	// ordered; the ORIGINAL was inserted descending. So the ordered fingerprints
	// legitimately differ, and that difference is exactly what proves the replay
	// trajectory is not the insertion trajectory — the precondition that makes the
	// byte-stability assertion above meaningful rather than vacuous.
	after := orderedFingerprint(t, res.Graph)
	if before == after {
		t.Error("the recovered adjacency has the same within-source order as the " +
			"original, so this fixture does not exercise a differing replay " +
			"trajectory and the byte-stability assertion above proves nothing")
	}

	// Re-snapshotting the recovered graph and recovering AGAIN must now be a fixed
	// point: the second recovery replays an already-ordered csr.bin, so its
	// adjacency order must match the first recovery's exactly.
	res2Dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(res2Dir, "snapshot"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteSnapshotFull(filepath.Join(res2Dir, "snapshot"), cs2, res.Graph); err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Open(filepath.Join(res2Dir, "wal")); err != nil {
		t.Fatal(err)
	}
	res2, err := Open[string, int64](res2Dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open (second recovery): %v", err)
	}
	if got := orderedFingerprint(t, res2.Graph); got != after {
		t.Error("recovery is not a fixed point: replaying an already-ordered " +
			"csr.bin produced a different within-source order")
	}
}

// TestRecovery_UnorderedLegacySnapshot_ReopensIdentically simulates a snapshot
// written BEFORE #2141: its csr.bin holds input-order runs and carries no handle
// block, exactly as the pre-change writer produced. The current reader must
// recover it to an observationally identical graph and no format version bump
// must be required, because within-source order is DERIVED at build time and
// never trusted from disk.
func TestRecovery_UnorderedLegacySnapshot_ReopensIdentically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g, w := buildHighDegreeGraph(t, dir)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Build the CSR, then DELIBERATELY un-order each source's run by reversing it,
	// and hand the arrays to FromArrays — which by contract does not order. That is
	// the legacy shape.
	cs := csr.BuildFromAdjList(g.AdjList())
	verts := append([]uint64(nil), cs.VerticesSlice()...)
	edges := append([]graph.NodeID(nil), cs.EdgesSlice()...)
	weights := append([]int64(nil), cs.WeightsSlice()...)
	for i := 0; i+1 < len(verts); i++ {
		lo, hi := verts[i], verts[i+1]
		if hi <= lo {
			continue // empty run: hi-1 would underflow (these are uint64)
		}
		for a, b := lo, hi-1; a < b; a, b = a+1, b-1 {
			edges[a], edges[b] = edges[b], edges[a]
			if weights != nil {
				weights[a], weights[b] = weights[b], weights[a]
			}
		}
	}
	legacy := csr.FromArrays(verts, edges, weights, cs.Order(), cs.Size())
	if legacy.RunsOrdered() {
		t.Fatal("the legacy fixture is still ordered, so it does not model a pre-#2141 snapshot")
	}

	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, legacy, g); err != nil {
		t.Fatalf("WriteSnapshotFull (legacy): %v", err)
	}
	if err := os.Truncate(filepath.Join(dir, "wal"), 0); err != nil {
		t.Fatal(err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open on a legacy unordered snapshot: %v", err)
	}

	// Observationally identical: the order-INSENSITIVE fingerprint must match, so
	// every node, label, property, edge and weight survived.
	//
	// Edge HANDLES are excluded from this comparison, and only here: the legacy
	// fixture is built by csr.FromArrays, which by construction carries no handle
	// block, so there is nothing on disk for the reader to restore and every
	// recovered slot is handle 0. That is the contract of a pre-#2141 snapshot,
	// not a loss — it is asserted positively immediately below. A MODERN v3
	// snapshot does round-trip handles, and there graphFingerprint is compared
	// unnormalised.
	if want, got := stripEdgeHandles(graphFingerprint(t, g)), stripEdgeHandles(graphFingerprint(t, res.Graph)); want != got {
		t.Errorf("legacy unordered snapshot did not recover to an identical graph\nwant:\n%s\ngot:\n%s", want, got)
	}
	// Pin the handle contract of the legacy format explicitly rather than
	// letting the normalisation above hide it.
	res.Graph.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		_, _, handles := res.Graph.AdjList().LoadEntryH(id)
		for i, h := range handles {
			if h != 0 {
				t.Errorf("node %s slot %d recovered handle %d from a legacy snapshot that carries no handle block; want 0", key, i, h)
				return false
			}
		}
		return true
	})
	// And the rebuilt CSR is ordered again, re-derived from the recovered
	// adjacency rather than inherited from the file.
	if !csr.BuildFromAdjList(res.Graph.AdjList()).RunsOrdered() {
		t.Error("the CSR rebuilt from a graph recovered from a legacy snapshot is not ordered")
	}
}

// TestRecovery_NoTombstoneResurrectionUnderOrdering pins that ordering commutes
// with the per-arc liveness filter: a node removed before the snapshot must not
// reappear, and no live edge may be dropped.
func TestRecovery_NoTombstoneResurrectionUnderOrdering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g, w := buildHighDegreeGraph(t, dir)

	// Remove a middle destination, which tombstones it without stripping its
	// incident edges; the live filter is what must exclude its arcs.
	g.RemoveNode("d40")

	live := g.LiveNodeFilter()
	cs := csr.BuildFromAdjListLive(g.AdjList(), live)
	if !cs.RunsOrdered() {
		t.Fatal("the live-filtered CSR is not ordered")
	}
	// No arc may mention the tombstoned node in either position.
	removedID, ok := g.AdjList().Mapper().Lookup("d40")
	if !ok {
		t.Fatal("expected the removed node to remain interned")
	}
	for _, d := range cs.EdgesSlice() {
		if d == removedID {
			t.Fatal("a tombstoned destination survived into the ordered live CSR")
		}
	}

	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, "wal"), 0); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for src := 0; src < 3; src++ {
		key := fmt.Sprintf("s%02d", src)
		for dst := range res.Graph.AdjList().Neighbours(key) {
			if dst == "d40" {
				t.Fatal("the tombstoned node resurrected through the ordered snapshot")
			}
		}
		// Every OTHER destination must still be present: ordering must not drop.
		n, _ := res.Graph.AdjList().OutDegree(key)
		if n != 79 {
			t.Errorf("%s out-degree after recovery = %d, want 79", key, n)
		}
	}
}
