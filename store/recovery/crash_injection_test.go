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

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
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
// summary of a recovered graph's observable LIVE state: every live node
// key (ascending), every label and typed property on it (sorted), and
// every outgoing edge slot to a live destination, ordered by the CSR's
// own total key — destination first, then edge handle. The fingerprint
// is returned as a single newline-separated string so two calls on
// equivalent graphs yield byte-identical output; this is the contract
// the idempotent-replay assertion relies on.
//
// Three properties make the fingerprint capable of failing (task #2270):
//
//   - Liveness gate. A node tombstoned by [lpg.Graph.RemoveNode]
//     contributes NOTHING — not its key line, not its labels or
//     properties, and not its outgoing edges — and no live node emits an
//     edge INTO a tombstoned destination. This mirrors the live topology
//     that csr.BuildFromAdjListLive publishes. Without the gate a
//     recovery that resurrects a deleted node fingerprints identically
//     to a correct one.
//
//   - Total, stable slot order. Slots are ordered by (destination,
//     handle) — the CSR's own total key — with [sort.SliceStable], so
//     the output never depends on the physical order the adjacency entry
//     happens to hold. A destination-only comparator is not total across
//     parallel edges and leaves the order of equal keys unspecified.
//
//   - Slot identity. Each slot emits its handle and weight, and its
//     per-handle labels and properties, so two parallel edges whose slot
//     assignment differs produce different fingerprints. The
//     pair-coalesced labels and properties are still emitted, once per
//     destination, because the per-pair store is a distinct surface from
//     the per-handle one and a recovery may populate either.
//
// The format is intentionally human-readable for debugging: a diff of
// two fingerprints points directly at the divergent record.
//
//	N <key>                 live node, ascending by key
//	 L <label>              node label, ascending
//	 P <key>=<kind:value>   node property, ascending by key
//	 E -> <dst>             live destination, ascending
//	  EL <label>            pair-coalesced edge label, ascending
//	  EP <key>=<value>      pair-coalesced edge property, ascending
//	  S h=<handle> w=<w>    one edge slot, ascending by handle
//	   SL <label>           per-handle edge label, ascending
//	   SP <key>=<value>     per-handle edge property, ascending
func graphFingerprint(t *testing.T, g *lpg.Graph[string, int64]) string {
	t.Helper()
	var sb strings.Builder
	for _, n := range liveNodes(g) {
		fmt.Fprintf(&sb, "N %s\n", n.key)
		labels := append([]string(nil), g.NodeLabels(n.key)...)
		sort.Strings(labels)
		for _, l := range labels {
			fmt.Fprintf(&sb, " L %s\n", l)
		}
		props := g.NodeProperties(n.key)
		for _, k := range sortedPropertyKeys(props) {
			fmt.Fprintf(&sb, " P %s=%s\n", k, formatPropertyValue(props[k]))
		}
		writeEdgeSlots(&sb, g, n.key, n.id)
	}
	return sb.String()
}

// fingerprintNode pairs a live node's interned key with its NodeID so
// the fingerprint can sort by key while still reading the adjacency
// entry by id.
type fingerprintNode struct {
	key string
	id  graph.NodeID
}

// liveNodes returns every NON-tombstoned interned node, ascending by
// key. The tombstone filter is the liveness gate: [lpg.Graph.RemoveNode]
// leaves the Mapper slot in place (NodeID stability is a hard contract),
// so walking the Mapper without consulting IsTombstoned reports deleted
// nodes as present.
func liveNodes(g *lpg.Graph[string, int64]) []fingerprintNode {
	var nodes []fingerprintNode
	g.AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool {
		if g.IsTombstoned(id) {
			return true
		}
		nodes = append(nodes, fingerprintNode{key: k, id: id})
		return true
	})
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].key < nodes[j].key })
	return nodes
}

// edgeSlot is one physical adjacency slot of a node, carrying the
// destination key, the stable edge handle, and the weight.
type edgeSlot struct {
	dst    string
	handle uint64
	w      int64
}

// liveEdgeSlots returns every outgoing slot of srcID whose destination
// is live, ordered by the CSR's total key (destination, handle). The
// sort is stable so slots that share a key (possible only when no
// handle was ever assigned, i.e. handle 0) keep a deterministic order
// instead of an unspecified one.
func liveEdgeSlots(g *lpg.Graph[string, int64], srcID graph.NodeID) []edgeSlot {
	neighbours, weights, handles := g.AdjList().LoadEntryH(srcID)
	slots := make([]edgeSlot, 0, len(neighbours))
	for i, dstID := range neighbours {
		if g.IsTombstoned(dstID) {
			continue
		}
		dstKey, ok := g.AdjList().Mapper().Resolve(dstID)
		if !ok {
			continue
		}
		s := edgeSlot{dst: dstKey}
		if i < len(weights) {
			s.w = weights[i]
		}
		if i < len(handles) {
			s.handle = handles[i]
		}
		slots = append(slots, s)
	}
	sort.SliceStable(slots, func(i, j int) bool {
		if slots[i].dst != slots[j].dst {
			return slots[i].dst < slots[j].dst
		}
		return slots[i].handle < slots[j].handle
	})
	return slots
}

// writeEdgeSlots renders the edge section of one node's fingerprint
// block: one " E -> dst" header per distinct live destination carrying
// the pair-coalesced labels and properties, followed by one "  S" line
// per slot carrying that slot's handle, weight, and per-handle labels
// and properties.
func writeEdgeSlots(sb *strings.Builder, g *lpg.Graph[string, int64], srcKey string, srcID graph.NodeID) {
	slots := liveEdgeSlots(g, srcID)
	for i := 0; i < len(slots); {
		dst := slots[i].dst
		fmt.Fprintf(sb, " E -> %s\n", dst)
		elabels := append([]string(nil), g.EdgeLabels(srcKey, dst)...)
		sort.Strings(elabels)
		for _, l := range elabels {
			fmt.Fprintf(sb, "  EL %s\n", l)
		}
		eprops := g.EdgeProperties(srcKey, dst)
		for _, k := range sortedPropertyKeys(eprops) {
			fmt.Fprintf(sb, "  EP %s=%s\n", k, formatPropertyValue(eprops[k]))
		}
		for ; i < len(slots) && slots[i].dst == dst; i++ {
			fmt.Fprintf(sb, "  S h=%d w=%d\n", slots[i].handle, slots[i].w)
			slabels := append([]string(nil), g.EdgeLabelsByHandle(srcKey, dst, slots[i].handle)...)
			sort.Strings(slabels)
			for _, l := range slabels {
				fmt.Fprintf(sb, "   SL %s\n", l)
			}
			sprops := g.EdgePropertiesByHandle(srcKey, dst, slots[i].handle)
			for _, k := range sortedPropertyKeys(sprops) {
				fmt.Fprintf(sb, "   SP %s=%s\n", k, formatPropertyValue(sprops[k]))
			}
		}
	}
}

// sortedPropertyKeys returns the keys of a property map in ascending
// order so the fingerprint is independent of Go's randomised map
// iteration.
func sortedPropertyKeys(props map[string]lpg.PropertyValue) []string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// walCheckpoint records the durable state of the monotonic workload
// immediately after one of its transactions committed: the WAL file
// size at that instant (so a truncation offset can be mapped back to
// the transaction it lands after) and the fingerprint of the in-memory
// graph the commit produced.
type walCheckpoint struct {
	// Fingerprint is the graph fingerprint immediately after the commit.
	Fingerprint string
	// Offset is the WAL file size immediately after the commit, i.e. the
	// smallest truncation offset at which this transaction survives.
	Offset int64
}

// writeMonotonicWorkload commits a deterministic, additive-only
// sequence of ops via a typed store, ONE TRANSACTION PER NODE:
// AddNode, SetNodeLabel, SetNodeProperty (every PropertyKind on the
// first node), AddEdge, SetEdgeLabel, SetEdgeProperty.
//
// The shape is load-bearing for the prefix property that
// [TestCrashInjection_TruncateEveryFrameBoundary] asserts. Each
// transaction creates exactly one new node whose key sorts AFTER every
// previously created key, populates that node's labels and properties,
// and adds edges only FROM the new node to already-existing ones.
// Because [graphFingerprint] emits nodes in ascending key order and a
// node's own out-edges inside its own block, every committed state's
// fingerprint is therefore a strict LINE-POSITIONAL PREFIX of the next
// one — the invariant [isPrefixOf] checks. A single fat transaction (the
// previous shape) could not exercise that invariant at all: recovery is
// atomic, so every truncation before the lone commit marker yielded an
// EMPTY graph, which the old membership-based isPrefixOf waved through
// against any fingerprint.
//
// The returned checkpoints are ordered by increasing WAL offset; the
// last one is the full committed state. The function fails the test if
// the prefix invariant it promises does not actually hold, so a future
// edit to the workload cannot silently make the boundary assertion
// vacuous again.
func writeMonotonicWorkload(t *testing.T, dir string) []walCheckpoint {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
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

	// Every step creates node n<k> (keys sort ascending in creation
	// order) and only ever appends to the END of the fingerprint.
	steps := []func(tx *txn.Tx[string, int64]){
		func(tx *txn.Tx[string, int64]) {
			_ = tx.AddNode("n0")
			_ = tx.SetNodeLabel("n0", "Person")
			_ = tx.SetNodeProperty("n0", "name", lpg.StringValue("Alice"))
			_ = tx.SetNodeProperty("n0", "age", lpg.Int64Value(30))
			_ = tx.SetNodeProperty("n0", "score", lpg.Float64Value(99.5))
			_ = tx.SetNodeProperty("n0", "active", lpg.BoolValue(true))
			_ = tx.SetNodeProperty("n0", "joined", lpg.TimeValue(knownTime))
			_ = tx.SetNodeProperty("n0", "blob", lpg.BytesValue([]byte{1, 2, 3}))
		},
		func(tx *txn.Tx[string, int64]) {
			_ = tx.AddNode("n1")
			_ = tx.SetNodeLabel("n1", "Person")
			_ = tx.SetNodeProperty("n1", "name", lpg.StringValue("Bob"))
			_ = tx.AddEdge("n1", "n0", 42)
			_ = tx.SetEdgeLabel("n1", "n0", "KNOWS")
			_ = tx.SetEdgeProperty("n1", "n0", "since", lpg.StringValue("2026"))
			_ = tx.SetEdgeProperty("n1", "n0", "weight", lpg.Int64Value(7))
		},
		func(tx *txn.Tx[string, int64]) {
			_ = tx.AddNode("n2")
			_ = tx.SetNodeLabel("n2", "Robot")
			_ = tx.SetNodeProperty("n2", "serial", lpg.BytesValue([]byte{0xAB, 0xCD}))
			_ = tx.AddEdge("n2", "n0", 7)
			_ = tx.SetEdgeLabel("n2", "n0", "OWNS")
			_ = tx.AddEdge("n2", "n1", 8)
			_ = tx.SetEdgeProperty("n2", "n1", "rate", lpg.Float64Value(0.25))
		},
		func(tx *txn.Tx[string, int64]) {
			_ = tx.AddNode("n3")
			_ = tx.SetNodeLabel("n3", "Person")
			_ = tx.SetNodeLabel("n3", "Admin")
			_ = tx.AddEdge("n3", "n2", -1)
			_ = tx.SetEdgeLabel("n3", "n2", "MANAGES")
			_ = tx.SetEdgeProperty("n3", "n2", "since", lpg.TimeValue(knownTime))
		},
		func(tx *txn.Tx[string, int64]) {
			_ = tx.AddNode("n4")
			_ = tx.SetNodeProperty("n4", "flag", lpg.BoolValue(false))
			_ = tx.AddEdge("n4", "n0", 100)
			_ = tx.AddEdge("n4", "n3", 200)
			_ = tx.SetEdgeLabel("n4", "n3", "REPORTS_TO")
		},
	}

	checkpoints := make([]walCheckpoint, 0, len(steps))
	for i, step := range steps {
		tx := s.Begin()
		step(tx)
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(step %d): %v", i, err)
		}
		size, err := walFileSize(walPath)
		if err != nil {
			t.Fatalf("stat WAL after step %d: %v", i, err)
		}
		checkpoints = append(checkpoints, walCheckpoint{
			Fingerprint: graphFingerprint(t, g),
			Offset:      size,
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Self-check: the workload promises that each committed state is a
	// positional line-prefix of the next, and that each transaction grew
	// both the WAL and the fingerprint. Verify it here so a future edit
	// that breaks the shape fails loudly at the source rather than
	// silently weakening the boundary assertion downstream.
	for i := 1; i < len(checkpoints); i++ {
		if checkpoints[i].Offset <= checkpoints[i-1].Offset {
			t.Fatalf("workload step %d did not grow the WAL: offset %d <= %d",
				i, checkpoints[i].Offset, checkpoints[i-1].Offset)
		}
		if checkpoints[i].Fingerprint == checkpoints[i-1].Fingerprint {
			t.Fatalf("workload step %d did not change the graph", i)
		}
		if !isPrefixOf(checkpoints[i-1].Fingerprint, checkpoints[i].Fingerprint) {
			t.Fatalf("workload step %d broke the prefix invariant\nprev:\n%s\nnext:\n%s",
				i, checkpoints[i-1].Fingerprint, checkpoints[i].Fingerprint)
		}
	}
	return checkpoints
}

// committedAt returns the fingerprint of the last transaction whose
// commit was durable at or before the truncation offset off, and the
// empty string when off falls before the first commit. It is the
// hand-computable expectation the boundary harness compares recovery
// against: recovery must reproduce EXACTLY the last committed state,
// never a partial transaction and never a later one.
func committedAt(checkpoints []walCheckpoint, off int64) string {
	want := ""
	for _, cp := range checkpoints {
		if off >= cp.Offset {
			want = cp.Fingerprint
		}
	}
	return want
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
	// A node that is created, wired up, and then DELETED inside the same
	// transaction. It exists in the Mapper after replay but must be
	// tombstoned, so neither the node nor the alice->carol arc appears in
	// the fingerprint. This is what makes the idempotence assertions able
	// to observe a recovery that resurrects a deleted node (task #2270).
	_ = tx.AddNode("carol")
	_ = tx.SetNodeLabel("carol", "Ghost")
	_ = tx.SetNodeProperty("carol", "doomed", lpg.BoolValue(true))
	_ = tx.AddEdge("alice", "carol", 3)
	_ = tx.RemoveNode("carol")
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
// workload as a SEQUENCE of transactions, snapshots the original WAL
// bytes, then for every record boundary truncates the file at that
// offset and runs recovery. Three assertions, in order:
//
//  1. Open never errors at a clean boundary, because each boundary
//     represents a torn-after-fsync state.
//  2. The recovered fingerprint is a positional line-prefix of the full
//     committed fingerprint ([isPrefixOf]) — recovery never invents
//     state and never reorders it.
//  3. The recovered fingerprint EQUALS, byte for byte, the state of the
//     last transaction whose commit was durable at or before the cut
//     ([committedAt]). This is the durability contract itself: no
//     committed transaction lost, no uncommitted transaction surfaced.
//
// Assertion 3 is what makes assertion 2 non-vacuous. Before task #2270
// this harness compared a single fat transaction against a
// membership-only "prefix" test: every cut before the lone commit
// marker recovered an EMPTY graph, which the old check accepted against
// any fingerprint, so the battery could not observe a dropped node, a
// duplicated edge, or a wholesale empty recovery.
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
	// directory, capturing the committed state after every transaction.
	refDir := t.TempDir()
	checkpoints := writeMonotonicWorkload(t, refDir)
	if len(checkpoints) < 2 {
		t.Fatalf("expected at least 2 committed transactions, got %d", len(checkpoints))
	}
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
	fullFingerprint := checkpoints[len(checkpoints)-1].Fingerprint
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
			wantFP := committedAt(checkpoints, off)
			if wantFP == "" {
				// The cut lands before the first commit marker: recovery
				// must surface NOTHING. Asserted positively — an empty
				// graph is only correct here, never a free pass.
				if gotFP != "" {
					t.Fatalf("recovery at boundary %d (off=%d) surfaced uncommitted state:\n%s", i, off, gotFP)
				}
				return
			}
			// Prefix consistency: gotFP must be a positional line-prefix
			// of the full committed fingerprint.
			if !isPrefixOf(gotFP, fullFingerprint) {
				t.Fatalf("recovery at boundary %d (off=%d) is not a prefix of committed state\nfull:\n%s\nrecovered:\n%s",
					i, off, fullFingerprint, gotFP)
			}
			// Durability: gotFP must be EXACTLY the last committed state.
			if gotFP != wantFP {
				t.Fatalf("recovery at boundary %d (off=%d) diverged from the last committed transaction\nwant:\n%s\ngot:\n%s",
					i, off, wantFP, gotFP)
			}
		})
	}
}

// isPrefixOf reports whether got is a POSITIONAL LINE-PREFIX of want:
// got has no more lines than want, and got's line i is byte-identical
// to want's line i for every i. It is the assertion that carries the
// crash-injection battery's core property — that recovery yields a
// prefix of committed state, never an arbitrary subset of it.
//
// Two degenerate cases are rejected explicitly, because accepting them
// is what made the battery vacuous before task #2270:
//
//   - An EMPTY got is not a prefix of a non-empty want. The previous
//     implementation split "" into a single empty line, skipped it, and
//     returned true — so a recovery that produced NOTHING passed
//     against ANY fingerprint. Callers that legitimately expect an
//     empty recovery must assert that positively.
//
//   - A got whose lines are a mere SUBSET of want's is not a prefix.
//     The previous implementation built a set of want's lines and
//     tested membership, checking neither order nor position nor
//     completeness — so a recovery that dropped half its nodes passed,
//     because every surviving line was still a member.
//
// The workload in [writeMonotonicWorkload] is shaped so that every
// committed state genuinely is a positional prefix of the next; that
// invariant is self-checked there.
func isPrefixOf(got, want string) bool {
	if got == "" {
		// Only an empty want has an empty prefix; an empty recovery is
		// never evidence that a committed prefix survived.
		return want == ""
	}
	gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	wantLines := strings.Split(strings.TrimRight(want, "\n"), "\n")
	if len(gotLines) > len(wantLines) {
		return false
	}
	for i, line := range gotLines {
		if wantLines[i] != line {
			return false
		}
	}
	return true
}

// TestIsPrefixOf_RejectsEmptyAndNonPrefix is the direct proof that the
// prefix assertion CAN FAIL. It drives [isPrefixOf] with the exact
// defects the crash-injection battery exists to catch — an empty
// recovery, a dropped node, a reordered fingerprint, an over-long
// fingerprint — and requires a false for each, while requiring a true
// for the genuine prefixes.
//
// Before task #2270 every "want false" row below returned TRUE: the
// implementation tested set membership only, so it checked neither
// order, nor position, nor completeness.
func TestIsPrefixOf_RejectsEmptyAndNonPrefix(t *testing.T) {
	t.Parallel()
	const full = "N a\n L A\n P k=int64:1\nN b\n L B\nN c\n"
	cases := []struct {
		name string
		got  string
		want string
		ok   bool
	}{
		{name: "identical", got: full, want: full, ok: true},
		{name: "genuine prefix, one node", got: "N a\n L A\n", want: full, ok: true},
		{name: "genuine prefix, two nodes", got: "N a\n L A\n P k=int64:1\nN b\n", want: full, ok: true},
		{name: "both empty", got: "", want: "", ok: true},

		// The degenerate case: an EMPTY recovered graph. The old
		// implementation split "" into one empty line, skipped it, and
		// returned true against ANY fingerprint.
		{name: "empty recovery against non-empty state", got: "", want: full, ok: false},

		// A dropped node. Every surviving line is still a MEMBER of want,
		// so the old membership test accepted it.
		{name: "dropped first node", got: "N b\n L B\nN c\n", want: full, ok: false},
		{name: "dropped middle node", got: "N a\n L A\n P k=int64:1\nN c\n", want: full, ok: false},
		{name: "dropped label inside a kept node", got: "N a\n P k=int64:1\nN b\n", want: full, ok: false},

		// Reordering: same multiset of lines, wrong order.
		{name: "reordered lines", got: " L A\nN a\n", want: full, ok: false},

		// Extra or altered state.
		{name: "longer than want", got: full + "N d\n", want: full, ok: false},
		{name: "altered property value", got: "N a\n L A\n P k=int64:2\n", want: full, ok: false},
		{name: "line absent from want", got: "N a\n L Z\n", want: full, ok: false},
		{name: "non-empty against empty want", got: "N a\n", want: "", ok: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isPrefixOf(tc.got, tc.want); got != tc.ok {
				t.Fatalf("isPrefixOf(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.ok)
			}
		})
	}
}

// TestGraphFingerprint_LivenessGate is the direct proof that the
// fingerprint's liveness gate CAN FAIL to match a resurrected node. It
// builds one graph, fingerprints it, tombstones a node, fingerprints
// again, then revives the node and fingerprints a third time.
//
// The three fingerprints must be: live != tombstoned, and revived ==
// live. Before task #2270 all three were IDENTICAL, because the
// fingerprint walked the Mapper with no liveness gate — so a recovery
// that resurrected a deleted node was indistinguishable from a correct
// one.
func TestGraphFingerprint_LivenessGate(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("bob", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("bob", "name", lpg.StringValue("Bob")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	live := graphFingerprint(t, g)
	if !strings.Contains(live, "N bob\n") {
		t.Fatalf("live fingerprint must carry bob:\n%s", live)
	}

	g.RemoveNode("bob")
	dead := graphFingerprint(t, g)
	if dead == live {
		t.Fatalf("fingerprint did not observe the tombstone: a resurrected node "+
			"is indistinguishable from a correct recovery\n%s", dead)
	}
	if strings.Contains(dead, "N bob") {
		t.Fatalf("tombstoned node still contributes its key line:\n%s", dead)
	}
	// The dangling arc alice->bob must go too: RemoveNode tombstones
	// without stripping incident edges, and the live topology
	// (csr.BuildFromAdjListLive) omits arcs incident to a dead node.
	if strings.Contains(dead, "E -> bob") {
		t.Fatalf("edge into a tombstoned node still contributes:\n%s", dead)
	}

	// AddNode clears the tombstone; the fingerprint must return to the
	// live value exactly, so the gate is a filter, not a lossy rewrite.
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode (revive): %v", err)
	}
	if revived := graphFingerprint(t, g); revived != live {
		t.Fatalf("revived fingerprint diverged from the live one\nlive:\n%s\nrevived:\n%s", live, revived)
	}
}

// TestGraphFingerprint_ParallelSlotAssignment is the direct proof that
// the fingerprint CAN FAIL when two parallel edges swap their slot
// assignment. Two multigraph edges a->b carry distinct handles; the
// control graph attaches label X and property "one" to the first handle
// and Y / "two" to the second, and the swapped graph attaches them the
// other way round.
//
// The fingerprints must differ. Before task #2270 they were IDENTICAL:
// edge labels and properties were read through the endpoint-pair
// surface only, and the edge comparator sorted by destination alone, so
// per-slot identity never reached the output.
func TestGraphFingerprint_ParallelSlotAssignment(t *testing.T) {
	t.Parallel()
	build := func(firstLabel, secondLabel, firstProp, secondProp string) string {
		g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
		h1, err := g.AddEdgeH("a", "b", 10)
		if err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
		h2, err := g.AddEdgeH("a", "b", 20)
		if err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
		if h1 == h2 {
			t.Fatalf("parallel edges must get distinct handles, both = %d", h1)
		}
		g.SetEdgeLabelByHandle("a", "b", h1, firstLabel)
		g.SetEdgeLabelByHandle("a", "b", h2, secondLabel)
		if err := g.SetEdgePropertyByHandle("a", "b", h1, "role", lpg.StringValue(firstProp)); err != nil {
			t.Fatalf("SetEdgePropertyByHandle: %v", err)
		}
		if err := g.SetEdgePropertyByHandle("a", "b", h2, "role", lpg.StringValue(secondProp)); err != nil {
			t.Fatalf("SetEdgePropertyByHandle: %v", err)
		}
		return graphFingerprint(t, g)
	}
	control := build("X", "Y", "one", "two")
	swapped := build("Y", "X", "two", "one")
	if control == swapped {
		t.Fatalf("swapping the slot assignment of two parallel edges left the "+
			"fingerprint unchanged; per-slot identity is invisible\n%s", control)
	}
	// The control must be reproducible: the comparator has to be total
	// and stable, not merely different from the swapped case.
	if again := build("X", "Y", "one", "two"); again != control {
		t.Fatalf("fingerprint is not deterministic across identical builds\nfirst:\n%s\nsecond:\n%s", control, again)
	}
	// Both slots must be individually visible.
	for _, want := range []string{"SL X", "SL Y", "SP role=string:one", "SP role=string:two"} {
		if !strings.Contains(control, want) {
			t.Fatalf("fingerprint omits %q:\n%s", want, control)
		}
	}
}

// TestCrashInjection_TombstonedNodeNotResurrected is the end-to-end
// durability proof for node deletion: a transaction creates a node,
// wires an edge to it, and deletes it; recovery must replay the
// deletion, not just the creation.
//
// The assertion bites only because [graphFingerprint] has a liveness
// gate: if recovery failed to reconstruct the tombstone the recovered
// fingerprint would carry "N carol" (and the dangling arc) while the
// pre-crash one does not.
func TestCrashInjection_TombstonedNodeNotResurrected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := writeFullWorkload(t, dir)
	if strings.Contains(want, "N carol") {
		t.Fatalf("pre-crash fingerprint must not carry the deleted node:\n%s", want)
	}
	g := recoverProperties(t, dir)
	// The Mapper must still hold the slot (NodeID stability is a hard
	// contract), so the only thing keeping carol out of the fingerprint
	// is the reconstructed tombstone.
	id, ok := g.AdjList().Mapper().Lookup("carol")
	if !ok {
		t.Fatal("recovery did not intern the deleted node; the test would pass for the wrong reason")
	}
	if !g.IsTombstoned(id) {
		t.Fatal("recovery resurrected a deleted node: the tombstone was not replayed")
	}
	if got := graphFingerprint(t, g); got != want {
		t.Fatalf("recovered state diverged from the pre-crash state\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestCrashInjection_ParallelEdgeSlotIdentitySurvivesRecovery is the
// end-to-end durability proof for per-slot identity: a transaction
// commits two PARALLEL edges a->b with distinct stable handles, each
// carrying its own label and property, and recovery must restore the
// same label and property on the same handle.
//
// The assertion bites only because [graphFingerprint] reads the
// per-handle surfaces and orders slots by (destination, handle): a
// recovery that swapped the two slots' labels would produce an
// identical fingerprint under the endpoint-pair-keyed formatting this
// test file used before task #2270.
func TestCrashInjection_ParallelEdgeSlotIdentitySurvivesRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	const (
		handleX = 11
		handleY = 22
	)
	tx := s.Begin()
	if err := tx.AddEdgeWithHandle("a", "b", 10, handleX); err != nil {
		t.Fatalf("AddEdgeWithHandle(X): %v", err)
	}
	if err := tx.AddEdgeWithHandle("a", "b", 20, handleY); err != nil {
		t.Fatalf("AddEdgeWithHandle(Y): %v", err)
	}
	if err := tx.SetEdgeLabelByHandle("a", "b", handleX, "X"); err != nil {
		t.Fatalf("SetEdgeLabelByHandle(X): %v", err)
	}
	if err := tx.SetEdgeLabelByHandle("a", "b", handleY, "Y"); err != nil {
		t.Fatalf("SetEdgeLabelByHandle(Y): %v", err)
	}
	if err := tx.SetEdgePropertyByHandle("a", "b", handleX, "role", lpg.StringValue("one")); err != nil {
		t.Fatalf("SetEdgePropertyByHandle(X): %v", err)
	}
	if err := tx.SetEdgePropertyByHandle("a", "b", handleY, "role", lpg.StringValue("two")); err != nil {
		t.Fatalf("SetEdgePropertyByHandle(Y): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	want := graphFingerprint(t, g)
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Guard: the pre-crash fingerprint must actually carry both slots
	// distinctly, otherwise the comparison below could pass blind.
	for _, marker := range []string{"S h=11 w=10", "S h=22 w=20", "SL X", "SL Y", "SP role=string:one", "SP role=string:two"} {
		if !strings.Contains(want, marker) {
			t.Fatalf("pre-crash fingerprint omits %q:\n%s", marker, want)
		}
	}

	rec := recoverProperties(t, dir)
	if got := graphFingerprint(t, rec); got != want {
		t.Fatalf("parallel-edge slot identity did not survive recovery\nwant:\n%s\ngot:\n%s", want, got)
	}
	// Assert the per-handle mapping directly too, so the failure message
	// names the swapped slot rather than a whole fingerprint diff.
	for _, tc := range []struct {
		label, role string
		handle      uint64
	}{
		{handle: handleX, label: "X", role: "one"},
		{handle: handleY, label: "Y", role: "two"},
	} {
		if labels := rec.EdgeLabelsByHandle("a", "b", tc.handle); len(labels) != 1 || labels[0] != tc.label {
			t.Errorf("handle %d recovered labels %v, want [%s]", tc.handle, labels, tc.label)
		}
		props := rec.EdgePropertiesByHandle("a", "b", tc.handle)
		v, ok := props["role"]
		if !ok {
			t.Errorf("handle %d lost its role property", tc.handle)
			continue
		}
		if got, _ := v.String(); got != tc.role {
			t.Errorf("handle %d role = %q, want %q", tc.handle, got, tc.role)
		}
	}
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

// TestCrashInjection_CorruptCRC flips one byte inside each NON-tail
// frame's payload, which forces a CRC32C mismatch. A CRC mismatch in an
// already-durable (non-tail) frame is genuine corruption — not a
// crash-truncated tail — so recovery is fail-stop: Open returns a non-nil
// error that errors.Is(err, wal.ErrCRCMismatch) (task #1289). The WAL
// reader still stops at the corrupted frame, and the committed prefix that
// pre-dates the corruption is placed in Result.Graph for diagnostics.
//
// The final frame is excluded from the flip set: a single fat-transaction
// workload ends in an OpCommit marker, and corrupting the marker (the last
// frame) tears the batch rather than corrupting a committed frame, which
// the torn-tail tests cover instead.
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
	// Stop before the final frame (len(boundaries)-1 is the last frame's
	// end): corrupting the trailing frame is a torn tail, not corruption.
	for fi := 1; fi < len(boundaries)-2; fi += 2 {
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
			res, err := Open[string, int64](dir, Options[string, int64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			})
			if err == nil {
				t.Fatalf("Open returned nil for a non-tail CRC mismatch at frame %d; want a hard error", fi)
			}
			if !errors.Is(err, wal.ErrCRCMismatch) {
				t.Fatalf("Open error = %v, want errors.Is(err, wal.ErrCRCMismatch)", err)
			}
			if res.IsClean() {
				t.Fatalf("Result.IsClean() = true for a CRC mismatch at frame %d, want false", fi)
			}
			if res.Graph == nil {
				t.Fatal("Result.Graph must be non-nil on corruption (diagnostics)")
			}
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
		p := make([]byte, 0, 2+len(body)+32) // 32 = upper bound on two codec-encoded strings
		p = append(p, txn.OpRecordV2, byte(kind))
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
			body := []byte{0x03, 0x00, 'k', 'e', 'y', byte(lpg.PropString), 0x10, 0, 0, 0} // claim 16 bytes, none follow
			return build(txn.OpSetNodeProperty, body)
		}()},
		{"SetEdgeProperty key ok but value unknown kind", func() []byte {
			body := []byte{0x03, 0x00, 'k', 'e', 'y', 0xAA} // 0xAA = unknown PropertyKind
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
			ok := applyOpCodec(g, &op, codec, wcodec, nil)
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
	if !applyOpCodec(g, &op, codec, wcodec, nil) {
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
	if !applyOpCodec(g, &op2, codec, wcodec, nil) {
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
	if !applyOpCodec(g, &op, codec, wcodec, nil) {
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
	if !applyOpCodec(g, &op, codec, wcodec, nil) {
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
		if !applyOpCodec(g, &op, codec, wcodec, nil) {
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
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open on empty WAL: %v", err)
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
