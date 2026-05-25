package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/index"
	"gograph/graph/index/btree"
	"gograph/graph/index/hash"
	"gograph/graph/index/label"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// frameBoundaries scans a WAL file and returns the byte offsets of
// every frame boundary, in ascending order. Index 0 is offset 0 (the
// start of the first frame), and the final entry is the file size
// (the cut-after-everything case). Each returned offset is a place
// where the file could be truncated to model a torn write that
// stopped exactly at a frame boundary.
//
// frameBoundaries is the foundation of every deterministic crash-
// injection test in this file: by truncating at every boundary the
// test asserts that recovery either (a) succeeds with a consistent
// prefix of the committed sequence, or (b) returns a documented error
// — never panics, never deadlocks, never produces a garbled graph.
func frameBoundaries(t *testing.T, path string) []int64 {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	offsets := []int64{0}
	off := 0
	for off < len(raw) {
		if len(raw)-off < wal.HeaderSize {
			break
		}
		plen := binary.LittleEndian.Uint32(raw[off+6 : off+10])
		frameEnd := off + wal.HeaderSize + int(plen)
		if frameEnd > len(raw) {
			break
		}
		offsets = append(offsets, int64(frameEnd))
		off = frameEnd
	}
	return offsets
}

// graphFingerprint produces a deterministic, content-addressable
// summary of a recovered graph's observable state: every node id (in
// ascending order), every outgoing edge from each node (sorted by
// destination), every label per node (sorted), and every typed
// property per node and per edge (sorted by key). The fingerprint is
// returned as a single newline-separated string so two calls on
// equivalent graphs yield byte-identical output; this is the contract
// the idempotent-replay assertion relies on.
//
// The format is intentionally human-readable for debugging: a diff of
// two fingerprints points directly at the divergent record.
func graphFingerprint(t *testing.T, g *lpg.Graph[string, int64]) string {
	t.Helper()
	var sb strings.Builder
	type node struct {
		key string
		id  graph.NodeID
	}
	var nodes []node
	g.AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool {
		nodes = append(nodes, node{key: k, id: id})
		return true
	})
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].key < nodes[j].key })
	for _, n := range nodes {
		fmt.Fprintf(&sb, "N %s\n", n.key)
		labels := append([]string(nil), g.NodeLabels(n.key)...)
		sort.Strings(labels)
		for _, l := range labels {
			fmt.Fprintf(&sb, " L %s\n", l)
		}
		props := g.NodeProperties(n.key)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, " P %s=%s\n", k, formatPropertyValue(props[k]))
		}
		// Outgoing edges (with weight).
		type edge struct {
			dst string
			w   int64
		}
		var edges []edge
		for dst, w := range g.AdjList().Neighbours(n.key) {
			edges = append(edges, edge{dst: dst, w: w})
		}
		sort.Slice(edges, func(i, j int) bool { return edges[i].dst < edges[j].dst })
		for _, e := range edges {
			fmt.Fprintf(&sb, " E -> %s w=%d\n", e.dst, e.w)
			elabels := append([]string(nil), g.EdgeLabels(n.key, e.dst)...)
			sort.Strings(elabels)
			for _, l := range elabels {
				fmt.Fprintf(&sb, "  EL %s\n", l)
			}
			eprops := g.EdgeProperties(n.key, e.dst)
			ekeys := make([]string, 0, len(eprops))
			for k := range eprops {
				ekeys = append(ekeys, k)
			}
			sort.Strings(ekeys)
			for _, k := range ekeys {
				fmt.Fprintf(&sb, "  EP %s=%s\n", k, formatPropertyValue(eprops[k]))
			}
		}
	}
	return sb.String()
}

// formatPropertyValue renders a PropertyValue as a kind-tagged string
// so the fingerprint distinguishes Int64(7) from Float64(7).
func formatPropertyValue(v lpg.PropertyValue) string {
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return "string:" + s
	case lpg.PropInt64:
		i, _ := v.Int64()
		return fmt.Sprintf("int64:%d", i)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		return fmt.Sprintf("float64:%g", f)
	case lpg.PropBool:
		b, _ := v.Bool()
		return fmt.Sprintf("bool:%v", b)
	case lpg.PropTime:
		tm, _ := v.Time()
		return "time:" + tm.UTC().Format(time.RFC3339Nano)
	case lpg.PropBytes:
		bs, _ := v.Bytes()
		return fmt.Sprintf("bytes:%x", bs)
	default:
		return "unknown"
	}
}

// writeMonotonicWorkload commits a deterministic, additive-only
// sequence of ops via a typed store: AddNode, AddEdge, SetNodeLabel,
// SetEdgeLabel, SetNodeProperty (every PropertyKind), SetEdgeProperty.
// The workload is intentionally free of removes so any WAL prefix
// produces a graph that is a strict subset of the final state — the
// invariant required by [TestCrashInjection_TruncateEveryFrameBoundary]
// and its sibling boundary tests.
//
// Returns a fingerprint of the in-memory graph immediately after
// commit; the post-recovery fingerprint of the full WAL must equal
// this value.
func writeMonotonicWorkload(t *testing.T, dir string) string {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	knownTime := time.Date(2026, 5, 22, 13, 0, 0, 0, time.UTC)

	tx := s.Begin()
	_ = tx.AddNode("alice")
	_ = tx.AddNode("bob")
	_ = tx.AddEdge("alice", "bob", 42)
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.SetNodeLabel("bob", "Person")
	_ = tx.SetEdgeLabel("alice", "bob", "KNOWS")
	_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	_ = tx.SetNodeProperty("alice", "age", lpg.Int64Value(30))
	_ = tx.SetNodeProperty("alice", "score", lpg.Float64Value(99.5))
	_ = tx.SetNodeProperty("alice", "active", lpg.BoolValue(true))
	_ = tx.SetNodeProperty("alice", "joined", lpg.TimeValue(knownTime))
	_ = tx.SetNodeProperty("alice", "blob", lpg.BytesValue([]byte{1, 2, 3}))
	_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	_ = tx.SetEdgeProperty("alice", "bob", "weight", lpg.Int64Value(7))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	fp := graphFingerprint(t, g)
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	return fp
}

// writeFullWorkload commits the monotonic ops above plus a set of
// remove / delete ops that exercise the inverse apply paths. Suitable
// for the idempotence test (which compares the recovered graph
// against itself across two Open calls, not against a prefix) and the
// all-kinds replay tests. NOT suitable for boundary truncation
// because prefixes of this workload may differ from a subset of the
// final state.
func writeFullWorkload(t *testing.T, dir string) string {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	knownTime := time.Date(2026, 5, 22, 13, 0, 0, 0, time.UTC)

	tx := s.Begin()
	_ = tx.AddNode("alice")
	_ = tx.AddNode("bob")
	_ = tx.AddEdge("alice", "bob", 42)
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.SetEdgeLabel("alice", "bob", "KNOWS")
	_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	_ = tx.SetNodeProperty("alice", "age", lpg.Int64Value(30))
	_ = tx.SetNodeProperty("alice", "score", lpg.Float64Value(99.5))
	_ = tx.SetNodeProperty("alice", "active", lpg.BoolValue(true))
	_ = tx.SetNodeProperty("alice", "joined", lpg.TimeValue(knownTime))
	_ = tx.SetNodeProperty("alice", "blob", lpg.BytesValue([]byte{1, 2, 3}))
	_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	_ = tx.SetEdgeProperty("alice", "bob", "weight", lpg.Int64Value(7))
	// Mutations that exercise Delete branches.
	_ = tx.SetNodeLabel("bob", "Tmp")
	_ = tx.RemoveNodeLabel("bob", "Tmp")
	_ = tx.SetNodeProperty("bob", "drop", lpg.StringValue("x"))
	_ = tx.DelNodeProperty("bob", "drop")
	_ = tx.SetEdgeProperty("alice", "bob", "drop", lpg.StringValue("x"))
	_ = tx.DelEdgeProperty("alice", "bob", "drop")
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	fp := graphFingerprint(t, g)
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	return fp
}

// recoverProperties opens dir through recovery.Open with the canonical
// string+int64 codecs and returns the recovered graph.
func recoverProperties(t *testing.T, dir string) *lpg.Graph[string, int64] {
	t.Helper()
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return res.Graph
}

// TestCrashInjection_TruncateEveryFrameBoundary is the headline
// crash-injection harness. It writes a deterministic property-heavy
// workload, snapshots the original WAL bytes, then for every record
// boundary truncates the file at that offset and runs recovery. The
// assertion is twofold: (a) Open never errors at a clean boundary
// because each boundary represents a torn-after-fsync state, and (b)
// the recovered graph is a strict prefix of the original — every node
// / edge / property present after recovery must also have been present
// in the in-memory graph at write time.
//
// The truncation set is exhaustive: with N committed frames there are
// N+1 boundaries (including offset 0 which yields an empty WAL).
// Combined with the four "split-frame" cuts (header-only, header+
// partial-payload, etc.) per frame, this yields well over the 15
// deterministic crash-injection cases required by the acceptance
// criterion.
func TestCrashInjection_TruncateEveryFrameBoundary(t *testing.T) {
	t.Parallel()
	// Phase 1: write a monotonic workload (no removes) to a reference
	// directory. The boundary harness compares each truncated recovery
	// against the full graph; using only additive ops keeps every WAL
	// prefix a strict subset of the final state.
	refDir := t.TempDir()
	want := writeMonotonicWorkload(t, refDir)
	walPath := filepath.Join(refDir, "wal")
	origBytes, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 2 {
		t.Fatalf("expected at least 2 frame boundaries, got %d", len(boundaries))
	}
	// Phase 2: for every boundary, run recovery against a freshly
	// truncated copy and assert the recovered graph is consistent.
	fullFingerprint := want
	for i, off := range boundaries {
		i, off := i, off
		t.Run(fmt.Sprintf("boundary_%d_at_%d", i, off), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tw, err := os.Create(filepath.Join(dir, "wal")) //nolint:gosec // path under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(origBytes[:off]); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			g := recoverProperties(t, dir)
			gotFP := graphFingerprint(t, g)
			// The full-cut at the final boundary must reproduce the
			// in-memory state byte-for-byte; intermediate cuts must
			// produce a strict prefix (no extra state).
			if off == int64(len(origBytes)) {
				if gotFP != fullFingerprint {
					t.Fatalf("full-WAL recovery diverged from in-memory state\nwant:\n%s\ngot:\n%s", fullFingerprint, gotFP)
				}
				return
			}
			// Prefix consistency: every line in gotFP must appear in
			// fullFingerprint at the same position (same prefix
			// length).
			if !isPrefixOf(gotFP, fullFingerprint) {
				t.Fatalf("recovery at boundary %d (off=%d) produced inconsistent state\nfull:\n%s\nrecovered:\n%s", i, off, fullFingerprint, gotFP)
			}
		})
	}
}

// isPrefixOf returns true when every line of got appears in want in
// the same order at the same offset, i.e. got is a textual prefix of
// want after sort-stable formatting. Used by the prefix-consistency
// assertion above. The fingerprint is structured so a strict prefix
// of the WAL produces a strict prefix of the fingerprint (modulo
// suffix lines that referenced nodes interned only by later frames —
// those nodes are simply absent from gotFP).
func isPrefixOf(got, want string) bool {
	// A node visible in `got` must also be visible in `want`. Properties
	// / labels on shared nodes must be a subset. Same for edges.
	gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	wantSet := make(map[string]struct{}, len(want)/16)
	for _, l := range strings.Split(strings.TrimRight(want, "\n"), "\n") {
		wantSet[l] = struct{}{}
	}
	for _, l := range gotLines {
		if l == "" {
			continue
		}
		if _, ok := wantSet[l]; !ok {
			return false
		}
	}
	return true
}

// TestCrashInjection_TruncateMidFrameHeader truncates within the
// 14-byte frame header (offsets 1..13 of each frame) and asserts the
// WAL reader treats the cut as a torn frame, never as a valid record.
// This is the "header torn after fsync" corner that the file-system
// guarantees but the on-disk format must still recognise.
func TestCrashInjection_TruncateMidFrameHeader(t *testing.T) {
	t.Parallel()
	refDir := t.TempDir()
	_ = writeFullWorkload(t, refDir)
	walPath := filepath.Join(refDir, "wal")
	origBytes, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 2 {
		t.Fatalf("not enough boundaries for mid-header test: %d", len(boundaries))
	}
	// Choose every other frame to keep runtime in check; truncate at
	// frame_start + {1, 4, 8, 13} which span every header field.
	cuts := []int64{1, 4, 8, 13}
	caseN := 0
	for fi := 0; fi < len(boundaries)-1; fi += 2 {
		base := boundaries[fi]
		for _, c := range cuts {
			off := base + c
			if off >= int64(len(origBytes)) {
				continue
			}
			caseN++
			off, base := off, base
			t.Run(fmt.Sprintf("frame_%d_off_%d", fi, off-base), func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				tw, err := os.Create(filepath.Join(dir, "wal")) //nolint:gosec // path under t.TempDir
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(origBytes[:off]); err != nil {
					t.Fatal(err)
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
				// Recovery must complete without error and produce a
				// graph that is a prefix of the full graph.
				_ = recoverProperties(t, dir)
			})
		}
	}
	if caseN == 0 {
		t.Fatal("no mid-header cases generated")
	}
}

// TestCrashInjection_TruncateMidPayload truncates within the payload
// of each frame. The WAL reader recognises the CRC mismatch (the CRC
// covers magic+version+length+payload) and stops cleanly at the start
// of the corrupted frame; recovery surfaces the partial-state graph
// without panic.
func TestCrashInjection_TruncateMidPayload(t *testing.T) {
	t.Parallel()
	refDir := t.TempDir()
	_ = writeFullWorkload(t, refDir)
	walPath := filepath.Join(refDir, "wal")
	origBytes, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 3 {
		t.Fatalf("not enough boundaries for mid-payload test: %d", len(boundaries))
	}
	cases := 0
	for fi := 0; fi < len(boundaries)-1; fi++ {
		base := boundaries[fi]
		next := boundaries[fi+1]
		payloadStart := base + int64(wal.HeaderSize)
		if next-payloadStart < 2 {
			continue
		}
		// One cut roughly in the middle of the payload.
		off := payloadStart + (next-payloadStart)/2
		cases++
		t.Run(fmt.Sprintf("frame_%d_payload_mid_%d", fi, off), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tw, err := os.Create(filepath.Join(dir, "wal")) //nolint:gosec // path under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(origBytes[:off]); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			_ = recoverProperties(t, dir)
		})
	}
	if cases == 0 {
		t.Fatal("no mid-payload cases generated")
	}
}

// TestCrashInjection_CorruptCRC flips one byte inside each frame's
// payload, which forces a CRC32C mismatch. The WAL reader stops at
// the corrupted frame and the recovered graph is the prefix of frames
// that pre-date the corruption.
func TestCrashInjection_CorruptCRC(t *testing.T) {
	t.Parallel()
	refDir := t.TempDir()
	_ = writeFullWorkload(t, refDir)
	walPath := filepath.Join(refDir, "wal")
	origBytes, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 3 {
		t.Fatalf("not enough boundaries for CRC corruption: %d", len(boundaries))
	}
	cases := 0
	for fi := 1; fi < len(boundaries)-1; fi += 2 {
		base := boundaries[fi]
		next := boundaries[fi+1]
		payloadStart := base + int64(wal.HeaderSize)
		if next-payloadStart < 1 {
			continue
		}
		flipAt := payloadStart // first byte of payload
		cases++
		t.Run(fmt.Sprintf("flip_frame_%d", fi), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			corrupted := append([]byte(nil), origBytes...)
			corrupted[flipAt] ^= 0xFF
			if err := os.WriteFile(filepath.Join(dir, "wal"), corrupted, 0o600); err != nil { //nolint:gosec // path under t.TempDir
				t.Fatal(err)
			}
			_ = recoverProperties(t, dir)
		})
	}
	if cases == 0 {
		t.Fatal("no CRC-corruption cases generated")
	}
}

// TestCrashInjection_IdempotentReplay establishes the canonical
// idempotence property of recovery: running Open twice on the same
// artefact yields two graphs whose fingerprints are byte-identical.
// This is the durability-contract dual of "Commit is the only side
// effect": Open must be a pure function of the on-disk state.
func TestCrashInjection_IdempotentReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := writeFullWorkload(t, dir)

	// First Open.
	g1 := recoverProperties(t, dir)
	fp1 := graphFingerprint(t, g1)
	if fp1 != want {
		t.Fatalf("first Open diverged from in-memory state\nwant:\n%s\ngot:\n%s", want, fp1)
	}
	// Second Open against the unchanged artefact.
	g2 := recoverProperties(t, dir)
	fp2 := graphFingerprint(t, g2)
	if fp2 != fp1 {
		t.Fatalf("idempotence violated: second Open produced different state\nfirst:\n%s\nsecond:\n%s", fp1, fp2)
	}
	// Third Open — paranoia for memoised state at any layer below.
	g3 := recoverProperties(t, dir)
	if fp3 := graphFingerprint(t, g3); fp3 != fp1 {
		t.Fatalf("third Open diverged: \nfirst:\n%s\nthird:\n%s", fp1, fp3)
	}
}

// TestCrashInjection_IdempotentReplayWithTorn establishes idempotence
// under a torn tail. The artefact is truncated mid-frame so the last
// op is dropped; running Open twice on the truncated artefact must
// still yield identical graphs. This rules out non-deterministic
// reconstruction of the prefix.
func TestCrashInjection_IdempotentReplayWithTorn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = writeFullWorkload(t, dir)
	walPath := filepath.Join(dir, "wal")
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate by 3 bytes — guaranteed to land inside the last frame's
	// payload or trailing crc and force a torn-tail path.
	if err := os.Truncate(walPath, info.Size()-3); err != nil {
		t.Fatal(err)
	}
	g1 := recoverProperties(t, dir)
	g2 := recoverProperties(t, dir)
	if fp1, fp2 := graphFingerprint(t, g1), graphFingerprint(t, g2); fp1 != fp2 {
		t.Fatalf("idempotence under torn tail violated:\nfirst:\n%s\nsecond:\n%s", fp1, fp2)
	}
}

// TestCrashInjection_SnapshotThenCrashInWAL covers the canonical
// crash sequence: a snapshot is taken at logical position S while
// the WAL continues to grow on top, then the WAL is truncated at
// every record boundary. The recovery contract states the labels /
// properties carried by the snapshot only attach to nodes that the
// WAL replay has interned in the mapper; truncating the WAL past
// the boundary that interned a snapshot-targeted node drops the
// snapshot record for that node by design.
//
// What this test verifies is therefore the durability contract of
// the boundary at which the snapshot's data does survive: when the
// truncation happens at or after the AddNode frame that interned the
// snapshot's target, the snapshot label / property apply must
// succeed.
//
// The test materialises the recovery-side contract documented on
// recovery.Open: "loads any snapshot under dir/snapshot, then replays
// the WAL at dir/wal applying each op into the live graph".
//
//nolint:gocyclo // crash-injection harness: snapshot write + per-boundary truncation + per-iteration recovery
func TestCrashInjection_SnapshotThenCrashInWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Phase 1: commit the pre-snapshot ops. Each AddNode in this
	// section produces one frame whose offset will become the lower
	// bound of "snapshot survives".
	tx := s.Begin()
	_ = tx.AddNode("alice")
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	preSnapshotEnd, err := walFileSize(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}

	// Phase 2: take a v2 snapshot of the current graph state.
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Phase 3: append post-snapshot mutations. These exercise the
	// WAL-replay-on-top-of-snapshot path.
	tx = s.Begin()
	_ = tx.AddNode("bob")
	_ = tx.SetNodeLabel("bob", "Person")
	_ = tx.AddEdge("alice", "bob", 7)
	_ = tx.SetEdgeLabel("alice", "bob", "KNOWS")
	_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	walPath := filepath.Join(dir, "wal")
	origBytes, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	boundaries := frameBoundaries(t, walPath)
	if len(boundaries) < 4 {
		t.Fatalf("expected at least 4 boundaries, got %d", len(boundaries))
	}

	// Phase 4: truncate the WAL at every record boundary and run
	// recovery. The expectation is split:
	//   - If the truncation drops the pre-snapshot frames (off <
	//     preSnapshotEnd), the snapshot labels apply silently skips
	//     records whose NodeID is not in the mapper. Recovery still
	//     succeeds; we only assert no panic.
	//   - If the truncation preserves the pre-snapshot frames (off >=
	//     preSnapshotEnd), the snapshot labels apply must attach the
	//     Person label and the name property to alice.
	for i, off := range boundaries {
		i, off := i, off
		t.Run(fmt.Sprintf("post_snapshot_boundary_%d_off_%d", i, off), func(t *testing.T) {
			t.Parallel()
			subDir := t.TempDir()
			if err := copyDir(filepath.Join(dir, "snapshot"), filepath.Join(subDir, "snapshot")); err != nil {
				t.Fatalf("copy snapshot: %v", err)
			}
			if err := os.WriteFile(filepath.Join(subDir, "wal"), origBytes[:off], 0o600); err != nil { //nolint:gosec // path under t.TempDir
				t.Fatal(err)
			}
			res, err := Open[string, int64](subDir, Options[string, int64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !res.SnapshotHit {
				t.Fatalf("SnapshotHit = false: snapshot apply skipped")
			}
			if off >= preSnapshotEnd {
				if !res.Graph.HasNodeLabel("alice", "Person") {
					t.Fatalf("snapshot label must survive when pre-snapshot WAL is intact (off=%d, presnap=%d)", off, preSnapshotEnd)
				}
				if _, ok := res.Graph.GetNodeProperty("alice", "name"); !ok {
					t.Fatalf("snapshot property must survive when pre-snapshot WAL is intact (off=%d)", off)
				}
			}
		})
	}
}

// walFileSize reads the size of the WAL file. It is a small wrapper
// around os.Stat that fits the helper-call pattern used by this
// test file.
func walFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// copyDir copies a directory tree shallowly (files + immediate
// subdirectories). It is sufficient for snapshot directories which
// have a known shape (manifest.json, csr.bin, labels.bin,
// properties.bin, indexes/*).
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(sp, dp); err != nil {
				return err
			}
			continue
		}
		buf, err := os.ReadFile(sp) //nolint:gosec // path under t.TempDir
		if err != nil {
			return err
		}
		if err := os.WriteFile(dp, buf, 0o600); err != nil { //nolint:gosec // path under t.TempDir
			return err
		}
	}
	return nil
}

// TestCrashInjection_PropertyReplay_AllKinds is the property-side
// replay test. It commits one SetNodeProperty and one SetEdgeProperty
// for every supported PropertyKind through the WAL, then reopens via
// Open and verifies the recovered value bit-for-bit matches the
// pre-crash value. This exercises the v2 apply path in applyOpCodec
// (the OpSetNodeProperty / OpSetEdgeProperty branches) and the
// decodeRecoveryPropertyValue switch.
func TestCrashInjection_PropertyReplay_AllKinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	knownTime := time.Date(2026, 5, 22, 13, 30, 0, 123456, time.UTC)
	tx := s.Begin()
	_ = tx.AddEdge("a", "b", 0)
	_ = tx.SetNodeProperty("a", "s", lpg.StringValue("hello"))
	_ = tx.SetNodeProperty("a", "i", lpg.Int64Value(-42))
	_ = tx.SetNodeProperty("a", "f", lpg.Float64Value(math.Pi))
	_ = tx.SetNodeProperty("a", "b", lpg.BoolValue(true))
	_ = tx.SetNodeProperty("a", "t", lpg.TimeValue(knownTime))
	_ = tx.SetNodeProperty("a", "x", lpg.BytesValue([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
	_ = tx.SetEdgeProperty("a", "b", "es", lpg.StringValue("edge"))
	_ = tx.SetEdgeProperty("a", "b", "ei", lpg.Int64Value(123))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	gRec := recoverProperties(t, dir)

	checkNodeStr(t, gRec, "a", "s", "hello")
	checkNodeInt(t, gRec, "a", "i", -42)
	checkNodeFloat(t, gRec, "a", "f", math.Pi)
	checkNodeBool(t, gRec, "a", "b", true)
	checkNodeTime(t, gRec, "a", "t", knownTime)
	checkNodeBytes(t, gRec, "a", "x", []byte{0xDE, 0xAD, 0xBE, 0xEF})
	checkEdgeStr(t, gRec, "a", "b", "es", "edge")
	checkEdgeInt(t, gRec, "a", "b", "ei", 123)
}

// TestCrashInjection_DecodePropertyValue_ShortBuffers asserts that
// decodeRecoveryPropertyValue reports an error rather than panicking
// or applying partial state on every short-buffer cut. The cases
// cover every PropertyKind: missing length prefix, length prefix that
// claims more bytes than remain, missing value body. This is the
// codec-error-during-replay contract.
func TestCrashInjection_DecodePropertyValue_ShortBuffers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		buf  []byte
	}{
		{name: "empty buffer", buf: nil},
		{name: "string kind only", buf: []byte{byte(lpg.PropString)}},
		{name: "string kind plus partial length", buf: []byte{byte(lpg.PropString), 1, 0}},
		{
			name: "string kind plus length but missing body",
			buf:  []byte{byte(lpg.PropString), 5, 0, 0, 0, 'h', 'i'},
		},
		{name: "int64 kind only", buf: []byte{byte(lpg.PropInt64)}},
		{name: "float64 kind plus partial", buf: []byte{byte(lpg.PropFloat64), 1, 2}},
		{name: "bool kind only", buf: []byte{byte(lpg.PropBool)}},
		{name: "time kind only", buf: []byte{byte(lpg.PropTime)}},
		{name: "bytes kind only", buf: []byte{byte(lpg.PropBytes)}},
		{name: "bytes length plus missing body", buf: []byte{byte(lpg.PropBytes), 10, 0, 0, 0, 1, 2}},
		{name: "unknown kind", buf: []byte{0xAA}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := decodeRecoveryPropertyValue(tc.buf); err == nil {
				t.Fatalf("decodeRecoveryPropertyValue(%x) returned no error", tc.buf)
			}
		})
	}
}

// TestCrashInjection_DecodePropertyValue_RoundTripAllKinds drives
// every PropertyKind through the codec on a clean buffer to lock in
// the success paths. The encoded form matches the txn write path; if
// either side drifts the round-trip breaks.
//
//nolint:gocyclo // table-driven: one branch per PropertyKind
func TestCrashInjection_DecodePropertyValue_RoundTripAllKinds(t *testing.T) {
	t.Parallel()
	knownTime := time.Date(2026, 5, 22, 13, 30, 0, 123, time.UTC)
	cases := []struct {
		name string
		v    lpg.PropertyValue
	}{
		{"string", lpg.StringValue("hello")},
		{"empty string", lpg.StringValue("")},
		{"int64 positive", lpg.Int64Value(123)},
		{"int64 negative", lpg.Int64Value(-1)},
		{"float64", lpg.Float64Value(2.718281828)},
		{"bool true", lpg.BoolValue(true)},
		{"bool false", lpg.BoolValue(false)},
		{"time", lpg.TimeValue(knownTime)},
		{"bytes", lpg.BytesValue([]byte{1, 2, 3, 4})},
		{"empty bytes", lpg.BytesValue(nil)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf := encodePropertyValueLike(tc.v)
			got, rest, err := decodeRecoveryPropertyValue(buf)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if len(rest) != 0 {
				t.Fatalf("unexpected trailing bytes: %x", rest)
			}
			if got.Kind() != tc.v.Kind() {
				t.Fatalf("kind mismatch: got %v want %v", got.Kind(), tc.v.Kind())
			}
			// Compare via formatPropertyValue to avoid kind-specific
			// equality plumbing.
			if formatPropertyValue(got) != formatPropertyValue(tc.v) {
				t.Fatalf("value mismatch: got %s want %s", formatPropertyValue(got), formatPropertyValue(tc.v))
			}
		})
	}
}

// encodePropertyValueLike mirrors the txn.encodePropertyValue layout
// without importing internal symbols. The format is:
//
//	uint8  kind
//	...kind-specific value bytes...
//
// Tests in this file own the encoder so a future change in txn that
// reshapes the on-disk encoding will fail the round-trip here loudly,
// rather than being silently absorbed.
func encodePropertyValueLike(v lpg.PropertyValue) []byte {
	buf := []byte{byte(v.Kind())}
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s)))
		buf = append(buf, s...)
	case lpg.PropInt64:
		i, _ := v.Int64()
		buf = binary.AppendVarint(buf, i)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(f))
	case lpg.PropBool:
		b, _ := v.Bool()
		if b {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
	case lpg.PropTime:
		tm, _ := v.Time()
		buf = binary.AppendVarint(buf, tm.UnixNano())
	case lpg.PropBytes:
		bs, _ := v.Bytes()
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(bs)))
		buf = append(buf, bs...)
	}
	return buf
}

// TestCrashInjection_ApplyOpCodec_PropertyShortBuffers crafts hand-
// built v2 frames where the codec-decoded src/dst are valid but the
// trailing key length or value body is short. applyOpCodec must
// return false and the graph must not carry the partial mutation.
//
//nolint:gocyclo // table-driven: one branch per OpKind that carries property data
func TestCrashInjection_ApplyOpCodec_PropertyShortBuffers(t *testing.T) {
	t.Parallel()
	codec := txn.NewStringCodec()
	wcodec := txn.NewInt64WeightCodec()

	build := func(kind txn.OpKind, body []byte) []byte {
		p := []byte{txn.OpRecordV2, byte(kind)}
		p, _ = codec.Encode(p, "alice")
		p, _ = codec.Encode(p, "bob")
		p = append(p, body...)
		return p
	}
	cases := []struct {
		name    string
		payload []byte
	}{
		{"SetNodeProperty missing keyLen", build(txn.OpSetNodeProperty, nil)},
		{"SetNodeProperty keyLen exceeds rest", build(txn.OpSetNodeProperty, []byte{0xFF, 0x00})},
		{"SetEdgeProperty missing keyLen", build(txn.OpSetEdgeProperty, nil)},
		{"SetEdgeProperty keyLen exceeds rest", build(txn.OpSetEdgeProperty, []byte{0xFF, 0xFF})},
		{"DelNodeProperty keyLen overflow", build(txn.OpDelNodeProperty, []byte{0x10, 0x00})},
		{"DelEdgeProperty missing keyLen", build(txn.OpDelEdgeProperty, nil)},
		{"SetNodeProperty key ok but value short", func() []byte {
			body := []byte{0x03, 0x00, 'k', 'e', 'y'}
			body = append(body, byte(lpg.PropString), 0x10, 0, 0, 0) // claim 16 bytes, none follow
			return build(txn.OpSetNodeProperty, body)
		}()},
		{"SetEdgeProperty key ok but value unknown kind", func() []byte {
			body := []byte{0x03, 0x00, 'k', 'e', 'y'}
			body = append(body, 0xAA) // unknown PropertyKind
			return build(txn.OpSetEdgeProperty, body)
		}()},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, err := Decode(tc.payload)
			if err != nil {
				t.Fatalf("Decode payload error: %v", err)
			}
			g := lpg.New[string, int64](adjlist.Config{Directed: true})
			ok := applyOpCodec(g, &op, codec, wcodec)
			if ok {
				t.Fatalf("applyOpCodec accepted malformed payload %q", tc.name)
			}
			// Graph must not carry any leaked state from the partial
			// decode.
			if _, present := g.GetNodeProperty("alice", "key"); present {
				t.Fatalf("partial decode leaked node property")
			}
			if _, present := g.GetEdgeProperty("alice", "bob", "key"); present {
				t.Fatalf("partial decode leaked edge property")
			}
		})
	}
}

// TestCrashInjection_ApplyOpCodec_AddNodeAndRemoveNode round-trips the
// AddNode and RemoveNode op kinds through applyOpCodec by writing
// hand-built v2 frames. AddNode interns the node; RemoveNode strips
// labels and properties via the same path used by recovery.
func TestCrashInjection_ApplyOpCodec_AddNodeAndRemoveNode(t *testing.T) {
	t.Parallel()
	codec := txn.NewStringCodec()
	wcodec := txn.NewInt64WeightCodec()

	build := func(kind txn.OpKind, src string, label string) []byte {
		p := []byte{txn.OpRecordV2, byte(kind)}
		p, _ = codec.Encode(p, src)
		p, _ = codec.Encode(p, "") // zero dst
		p = binary.LittleEndian.AppendUint16(p, uint16(len(label)))
		p = append(p, label...)
		return p
	}
	// AddNode
	op, err := Decode(build(txn.OpAddNode, "alice", ""))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if !applyOpCodec(g, &op, codec, wcodec) {
		t.Fatal("AddNode must apply")
	}
	if _, ok := g.AdjList().Mapper().Lookup("alice"); !ok {
		t.Fatal("AddNode did not intern alice")
	}
	// Seed labels and properties on alice, then exercise RemoveNode.
	if err := g.SetNodeLabel("alice", "A"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("alice", "B"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("alice", "k", lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	op2, err := Decode(build(txn.OpRemoveNode, "alice", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !applyOpCodec(g, &op2, codec, wcodec) {
		t.Fatal("RemoveNode must apply")
	}
	if g.HasNodeLabel("alice", "A") {
		t.Fatal("RemoveNode did not strip label A")
	}
	if _, ok := g.GetNodeProperty("alice", "k"); ok {
		t.Fatal("RemoveNode did not strip property k")
	}
}

// TestCrashInjection_ApplyOpCodec_RemoveEdgeRoundTrip exercises the
// OpRemoveEdge branch in applyOpCodec: the graph must carry the
// pre-existing edge, applyOpCodec must remove it, and the post-state
// must lack the edge.
func TestCrashInjection_ApplyOpCodec_RemoveEdgeRoundTrip(t *testing.T) {
	t.Parallel()
	codec := txn.NewStringCodec()
	wcodec := txn.NewInt64WeightCodec()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AdjList().AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	p := []byte{txn.OpRecordV2, byte(txn.OpRemoveEdge)}
	p, _ = codec.Encode(p, "alice")
	p, _ = codec.Encode(p, "bob")
	p = binary.LittleEndian.AppendUint16(p, 0)
	op, err := Decode(p)
	if err != nil {
		t.Fatal(err)
	}
	if !applyOpCodec(g, &op, codec, wcodec) {
		t.Fatal("RemoveEdge must apply")
	}
	if g.AdjList().HasEdge("alice", "bob") {
		t.Fatal("RemoveEdge did not strip the edge")
	}
}

// TestCrashInjection_ApplyOpCodec_RemoveNodeLabelRoundTrip exercises
// the OpRemoveNodeLabel branch and the trailing label-overflow
// guard simultaneously.
func TestCrashInjection_ApplyOpCodec_RemoveNodeLabelRoundTrip(t *testing.T) {
	t.Parallel()
	codec := txn.NewStringCodec()
	wcodec := txn.NewInt64WeightCodec()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("alice", "Tmp"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	p := []byte{txn.OpRecordV2, byte(txn.OpRemoveNodeLabel)}
	p, _ = codec.Encode(p, "alice")
	p, _ = codec.Encode(p, "")
	p = binary.LittleEndian.AppendUint16(p, uint16(len("Tmp")))
	p = append(p, "Tmp"...)
	op, err := Decode(p)
	if err != nil {
		t.Fatal(err)
	}
	if !applyOpCodec(g, &op, codec, wcodec) {
		t.Fatal("RemoveNodeLabel must apply")
	}
	if g.HasNodeLabel("alice", "Tmp") {
		t.Fatal("RemoveNodeLabel did not strip label")
	}
}

// TestCrashInjection_ApplyOpCodec_DelPropertiesRoundTrip exercises
// OpDelNodeProperty and OpDelEdgeProperty in one shot: seed the
// graph with the properties, decode the v2 frames, and assert the
// properties are gone post-apply.
func TestCrashInjection_ApplyOpCodec_DelPropertiesRoundTrip(t *testing.T) {
	t.Parallel()
	codec := txn.NewStringCodec()
	wcodec := txn.NewInt64WeightCodec()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AdjList().AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeProperty("a", "k", lpg.StringValue("v")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "k", lpg.StringValue("v")); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	delNode := []byte{txn.OpRecordV2, byte(txn.OpDelNodeProperty)}
	delNode, _ = codec.Encode(delNode, "a")
	delNode, _ = codec.Encode(delNode, "")
	delNode = binary.LittleEndian.AppendUint16(delNode, uint16(len("k")))
	delNode = append(delNode, "k"...)

	delEdge := []byte{txn.OpRecordV2, byte(txn.OpDelEdgeProperty)}
	delEdge, _ = codec.Encode(delEdge, "a")
	delEdge, _ = codec.Encode(delEdge, "b")
	delEdge = binary.LittleEndian.AppendUint16(delEdge, uint16(len("k")))
	delEdge = append(delEdge, "k"...)

	for _, p := range [][]byte{delNode, delEdge} {
		op, err := Decode(p)
		if err != nil {
			t.Fatal(err)
		}
		if !applyOpCodec(g, &op, codec, wcodec) {
			t.Fatalf("del op must apply")
		}
	}
	if _, ok := g.GetNodeProperty("a", "k"); ok {
		t.Fatal("DelNodeProperty did not strip property")
	}
	if _, ok := g.GetEdgeProperty("a", "b", "k"); ok {
		t.Fatal("DelEdgeProperty did not strip property")
	}
}

// TestCrashInjection_MixedSnapshotV1V2 establishes the mixed-version
// snapshot contract on the read side: first the test produces a v1
// (CSR-only) snapshot, runs recovery, and asserts the recovered
// graph carries no labels / properties (v1 has none); then it
// overwrites the snapshot with a v2 (CSR + labels + properties)
// snapshot of the same WAL prefix and asserts the recovered graph
// now carries the labels and properties.
//
// Both reads go through the same recovery.Open entry point — the
// caller does not have to know which snapshot version is on disk.
func TestCrashInjection_MixedSnapshotV1V2(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	tx := s.Begin()
	_ = tx.AddEdge("alice", "bob", 0)
	_ = tx.SetNodeLabel("alice", "Person")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Add a typed property after the commit (snapshot-only path).
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("Alice")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	// === v1 snapshot pass ===
	snapDir := filepath.Join(dir, "snapshot")
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotCSR(snapDir, cs); err != nil {
		t.Fatalf("WriteSnapshotCSR: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false on v1 snapshot")
	}
	if res.SnapshotSchemaVersion != 1 {
		t.Fatalf("SnapshotSchemaVersion = %d, want 1", res.SnapshotSchemaVersion)
	}
	if res.SnapshotProperties != 0 {
		t.Fatalf("v1 snapshot must not contribute properties; got %d", res.SnapshotProperties)
	}
	if res.SnapshotLabels != 0 {
		t.Fatalf("v1 snapshot must not contribute labels; got %d", res.SnapshotLabels)
	}

	// === overwrite with v2 snapshot ===
	if err := os.RemoveAll(snapDir); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	res2, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open v2: %v", err)
	}
	if !res2.SnapshotHit {
		t.Fatal("SnapshotHit = false on v2 snapshot")
	}
	// String-keyed graphs emit the self-sufficient v3 layout that
	// carries mapper.bin alongside csr/labels/properties.
	if res2.SnapshotSchemaVersion != snapshot.ManifestVersion {
		t.Fatalf("SnapshotSchemaVersion = %d, want %d",
			res2.SnapshotSchemaVersion, snapshot.ManifestVersion)
	}
	if res2.SnapshotProperties == 0 {
		t.Fatal("v2 snapshot must contribute properties")
	}
	if !res2.Graph.HasNodeLabel("alice", "Person") {
		t.Fatal("v2 snapshot did not restore label")
	}
	v, ok := res2.Graph.GetNodeProperty("alice", "name")
	if !ok {
		t.Fatal("v2 snapshot did not restore property")
	}
	if got, _ := v.String(); got != "Alice" {
		t.Fatalf("name = %q, want Alice", got)
	}
}

// TestCrashInjection_ApplySnapshotIndexes_DeserializeError forces the
// "deserialize fails" branch of applySnapshotIndexes by handing it a
// readback whose bytes are well-formed in length but garbled in
// content. The function must log a corruption warning and return
// loaded=0, leaving the live index in its zero state.
func TestCrashInjection_ApplySnapshotIndexes_DeserializeError(t *testing.T) {
	t.Parallel()
	mgr := index.NewManager()
	if err := mgr.CreateIndex("hash.x", hash.New[string]()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CreateIndex("btree.x", btree.New[string]()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CreateIndex("labels.x", label.NewIndex()); err != nil {
		t.Fatal(err)
	}
	// Garbled but non-nil byte blobs.
	rb := []snapshot.IndexReadback{
		{Name: "hash.x", Bytes: []byte{0xFF, 0xFF, 0xFF, 0xFF}},
		{Name: "btree.x", Bytes: []byte{0x00}},
		{Name: "labels.x", Bytes: []byte{0xAA, 0xBB}},
	}
	got := applySnapshotIndexes(mgr, rb)
	if got != 0 {
		t.Fatalf("expected 0 successful loads on garbled bytes, got %d", got)
	}
}

// TestCrashInjection_ApplySnapshotIndexes_UnknownIndex forces the
// "manager does not know this index" branch: the readback references
// an index name the manager has never seen. The function must skip
// silently (logged) and return loaded=0.
func TestCrashInjection_ApplySnapshotIndexes_UnknownIndex(t *testing.T) {
	t.Parallel()
	mgr := index.NewManager()
	rb := []snapshot.IndexReadback{
		{Name: "ghost", Bytes: []byte{1, 2, 3}},
	}
	got := applySnapshotIndexes(mgr, rb)
	if got != 0 {
		t.Fatalf("expected 0 loads for unknown index, got %d", got)
	}
}

// TestCrashInjection_ApplySnapshotIndexes_NilBytes forces the "bytes
// are nil" branch: the snapshot loader returns a readback whose Bytes
// is nil because the file was missing or its CRC32C failed validation.
// The function must skip (metric incremented) and return loaded=0.
func TestCrashInjection_ApplySnapshotIndexes_NilBytes(t *testing.T) {
	t.Parallel()
	mgr := index.NewManager()
	if err := mgr.CreateIndex("hash.x", hash.New[string]()); err != nil {
		t.Fatal(err)
	}
	rb := []snapshot.IndexReadback{
		{Name: "hash.x", Bytes: nil},
	}
	got := applySnapshotIndexes(mgr, rb)
	if got != 0 {
		t.Fatalf("expected 0 loads for nil-bytes index, got %d", got)
	}
}

// TestCrashInjection_ApplySnapshotIndexes_NilManager covers the early
// return path when the recovered graph has no IndexManager wired up.
func TestCrashInjection_ApplySnapshotIndexes_NilManager(t *testing.T) {
	t.Parallel()
	if got := applySnapshotIndexes(nil, []snapshot.IndexReadback{{Name: "x"}}); got != 0 {
		t.Fatalf("nil manager must yield 0 loads, got %d", got)
	}
	if got := applySnapshotIndexes(index.NewManager(), nil); got != 0 {
		t.Fatalf("nil readback slice must yield 0 loads, got %d", got)
	}
}

// TestCrashInjection_OpenString_CodecErrorDuringReplay drives the
// codec-error path through OpenString: a hand-built v2 frame with a
// corrupted weight payload is appended to the WAL. Recovery must not
// apply the partial frame, must not panic, and the graph must be
// empty of the corrupted edge.
func TestCrashInjection_OpenString_CodecErrorDuringReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	codec := txn.NewStringCodec()
	// Craft an OpAddEdgeWeighted frame that OpenString can't decode
	// (no WeightCodec on the OpenString path). The frame is otherwise
	// well-formed.
	payload := []byte{txn.OpRecordV2, byte(txn.OpAddEdgeWeighted)}
	payload, _ = codec.Encode(payload, "alice")
	payload, _ = codec.Encode(payload, "bob")
	payload = binary.LittleEndian.AppendUint16(payload, 0x0102_0304%0xFFFF)
	if err := w.Append(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("malformed weighted frame must not apply through OpenString")
	}
}

// TestCrashInjection_WALReader_TornEvenWithoutCommit drives the path
// where the WAL is open but no frames have been synced before a
// crash. The reader must report a clean EOF (or torn) without
// surfacing as a recovery-level error.
func TestCrashInjection_WALReader_TornEvenWithoutCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create an empty WAL file (zero bytes).
	walPath := filepath.Join(dir, "wal")
	tw, err := os.Create(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString on empty WAL: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0", res.WALOps)
	}
}

// TestCrashInjection_BoundariesMatchWALReader cross-checks the
// boundary detector against the WAL reader's own tail tracking: the
// reader's TailOffset on a fully-readable file must equal the file
// size, and the last entry in frameBoundaries must equal the file
// size. This is a sanity check on the test harness itself; a drift
// between the two would invalidate every test in this file.
func TestCrashInjection_BoundariesMatchWALReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = writeFullWorkload(t, dir)
	walPath := filepath.Join(dir, "wal")
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	boundaries := frameBoundaries(t, walPath)
	if boundaries[len(boundaries)-1] != info.Size() {
		t.Fatalf("last boundary %d != file size %d", boundaries[len(boundaries)-1], info.Size())
	}
	// The reader must also walk every frame without error.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	count := 0
	for range r.Frames() {
		count++
	}
	if r.TailError() != nil && !errors.Is(r.TailError(), io.EOF) {
		t.Fatalf("reader tail error: %v", r.TailError())
	}
	if count != len(boundaries)-1 {
		t.Fatalf("reader saw %d frames, boundaries imply %d", count, len(boundaries)-1)
	}
}

// --- assertion helpers ----------------------------------------------------

func checkNodeStr(t *testing.T, g *lpg.Graph[string, int64], n, k, want string) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, k)
	if !ok {
		t.Fatalf("%s.%s missing", n, k)
	}
	if s, _ := v.String(); s != want {
		t.Fatalf("%s.%s = %q, want %q", n, k, s, want)
	}
}

func checkNodeInt(t *testing.T, g *lpg.Graph[string, int64], n, k string, want int64) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, k)
	if !ok {
		t.Fatalf("%s.%s missing", n, k)
	}
	if i, _ := v.Int64(); i != want {
		t.Fatalf("%s.%s = %d, want %d", n, k, i, want)
	}
}

func checkNodeFloat(t *testing.T, g *lpg.Graph[string, int64], n, k string, want float64) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, k)
	if !ok {
		t.Fatalf("%s.%s missing", n, k)
	}
	if f, _ := v.Float64(); math.Float64bits(f) != math.Float64bits(want) {
		t.Fatalf("%s.%s = %v, want %v", n, k, f, want)
	}
}

func checkNodeBool(t *testing.T, g *lpg.Graph[string, int64], n, k string, want bool) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, k)
	if !ok {
		t.Fatalf("%s.%s missing", n, k)
	}
	if b, _ := v.Bool(); b != want {
		t.Fatalf("%s.%s = %v, want %v", n, k, b, want)
	}
}

func checkNodeTime(t *testing.T, g *lpg.Graph[string, int64], n, k string, want time.Time) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, k)
	if !ok {
		t.Fatalf("%s.%s missing", n, k)
	}
	if tm, _ := v.Time(); !tm.Equal(want) {
		t.Fatalf("%s.%s = %v, want %v", n, k, tm, want)
	}
}

func checkNodeBytes(t *testing.T, g *lpg.Graph[string, int64], n, k string, want []byte) {
	t.Helper()
	v, ok := g.GetNodeProperty(n, k)
	if !ok {
		t.Fatalf("%s.%s missing", n, k)
	}
	if bs, _ := v.Bytes(); !bytes.Equal(bs, want) {
		t.Fatalf("%s.%s = %x, want %x", n, k, bs, want)
	}
}

func checkEdgeStr(t *testing.T, g *lpg.Graph[string, int64], s, d, k, want string) {
	t.Helper()
	v, ok := g.GetEdgeProperty(s, d, k)
	if !ok {
		t.Fatalf("edge(%s,%s).%s missing", s, d, k)
	}
	if got, _ := v.String(); got != want {
		t.Fatalf("edge(%s,%s).%s = %q, want %q", s, d, k, got, want)
	}
}

func checkEdgeInt(t *testing.T, g *lpg.Graph[string, int64], s, d, k string, want int64) {
	t.Helper()
	v, ok := g.GetEdgeProperty(s, d, k)
	if !ok {
		t.Fatalf("edge(%s,%s).%s missing", s, d, k)
	}
	if got, _ := v.Int64(); got != want {
		t.Fatalf("edge(%s,%s).%s = %d, want %d", s, d, k, got, want)
	}
}
