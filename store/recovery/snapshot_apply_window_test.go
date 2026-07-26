package recovery

// snapshot_apply_window_test.go — rmp #2170.
//
// The snapshot-apply path now brackets its CSR apply in an adjacency commit
// window, as WAL replay has done since task #1526. Inside a window a shard's
// slot array is cloned once on first touch and then mutated in place, instead of
// being cloned afresh for every edge.
//
// That is a change to COST, not to CONTENT — and this file is what holds it to
// that. The recovered graph is fingerprinted with the same content-addressable
// summary the crash-injection suite uses (every node, label, property, edge,
// weight, edge label and edge property, all in sorted order) and compared byte
// for byte against the pre-checkpoint graph it was built from.
//
// What these tests do NOT cover, stated so nobody relies on them for it: a
// LEAKED window. Injecting one (deleting the EndCommit call) leaves both tests
// green, because a leak only does harm once a later write mutates an
// already-published slot array under a concurrent reader, and the commit depth
// is not observable through any exported API. The leak is instead made
// unreachable by construction — BeginCommit and EndCommit sit on adjacent lines
// with no branch and no error return between them — which is why the production
// code uses a plain call rather than a defer.
//
// Layer: short.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestSnapshotApplyWindow_RecoveredStateIdentical proves the commit window did
// not change what recovery reconstructs.
//
// The fixture is deliberately wide rather than deep: enough distinct nodes to
// spread across several adjacency shards (the window's clone-once behaviour is
// per shard, so a single-shard fixture would not exercise it) and enough edges
// per node that a shard is touched repeatedly within the window, which is
// exactly the case where an in-place mutation could go wrong.
func TestSnapshotApplyWindow_RecoveredStateIdentical(t *testing.T) {
	t.Parallel()
	const (
		nodes         = 512
		edgesPerNode  = 6
		propsPerNode  = 3
		labelsPerNode = 2
	)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	key := func(i int) string { return "n" + itoaPad(i) }

	tx := store.Begin()
	for i := 0; i < nodes; i++ {
		k := key(i)
		if err := tx.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		for l := 0; l < labelsPerNode; l++ {
			if err := tx.SetNodeLabel(k, "L"+itoaPad((i+l)%7)); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		// Mixed property kinds, so the fingerprint distinguishes Int64(7) from
		// Float64(7) and a temporal-looking string from a plain one.
		if err := tx.SetNodeProperty(k, "name", lpg.StringValue("name-"+k)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		if err := tx.SetNodeProperty(k, "seq", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		if err := tx.SetNodeProperty(k, "ratio", lpg.Float64Value(float64(i)+0.5)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit nodes: %v", err)
	}

	tx = store.Begin()
	for i := 0; i < nodes; i++ {
		src := key(i)
		for e := 1; e <= edgesPerNode; e++ {
			dst := key((i*e + e*e) % nodes)
			if err := tx.AddEdge(src, dst, int64(i*100+e)); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if err := tx.SetEdgeLabel(src, dst, "E"+itoaPad(e%3)); err != nil {
				t.Fatalf("SetEdgeLabel: %v", err)
			}
			if err := tx.SetEdgeProperty(src, dst, "w", lpg.Int64Value(int64(e))); err != nil {
				t.Fatalf("SetEdgeProperty: %v", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit edges: %v", err)
	}

	// The committed in-memory state is the reference: this is what recovery must
	// reproduce, independently of how any previous implementation behaved.
	want := graphFingerprint(t, g)
	if want == "" {
		t.Fatal("fingerprint of the source graph is empty; the fixture built nothing")
	}

	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false; this test must exercise the snapshot-apply path")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0; the CSR must have come from the snapshot, not the WAL", res.WALOps)
	}

	got := graphFingerprint(t, res.Graph)
	if got != want {
		t.Fatalf("recovered graph differs from the committed graph.\nfirst divergence:\n%s",
			firstFingerprintDiff(want, got))
	}
}

// TestSnapshotApplyWindow_RecoveryIsRepeatable recovers the same directory twice
// and requires identical fingerprints. A window left open by the first recovery
// would let the second one mutate an already-published slot array, so a
// divergence here localises the fault to window lifecycle rather than to the
// apply itself.
func TestSnapshotApplyWindow_RecoveryIsRepeatable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx := store.Begin()
	for i := 0; i < 256; i++ {
		if err := tx.AddNode("n" + itoaPad(i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < 256; i++ {
		for e := 1; e <= 4; e++ {
			if err := tx.AddEdge("n"+itoaPad(i), "n"+itoaPad((i*e+1)%256), int64(i+e)); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	open := func() string {
		res, oerr := Open[string, int64](dir, Options[string, int64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		})
		if oerr != nil {
			t.Fatalf("recovery.Open: %v", oerr)
		}
		if !res.SnapshotHit {
			t.Fatal("SnapshotHit = false")
		}
		return graphFingerprint(t, res.Graph)
	}

	first := open()
	second := open()
	if first != second {
		t.Fatalf("two recoveries of the same directory diverged.\nfirst divergence:\n%s",
			firstFingerprintDiff(first, second))
	}
}

// itoaPad renders i zero-padded to 5 digits so lexical order matches numeric
// order, keeping the sorted fingerprint stable and its diffs readable.
func itoaPad(i int) string {
	const digits = 5
	buf := [digits]byte{'0', '0', '0', '0', '0'}
	for p := digits - 1; p >= 0 && i > 0; p-- {
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[:])
}

// firstFingerprintDiff returns a short excerpt around the first differing line
// of two fingerprints, so a failure names the divergent record instead of
// dumping two multi-megabyte strings.
func firstFingerprintDiff(want, got string) string {
	wl := splitLines(want)
	gl := splitLines(got)
	n := len(wl)
	if len(gl) < n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			return "line " + itoaPad(i+1) + "\n  want: " + wl[i] + "\n  got:  " + gl[i]
		}
	}
	if len(wl) != len(gl) {
		return "line counts differ: want " + itoaPad(len(wl)) + ", got " + itoaPad(len(gl))
	}
	return "(no line-level difference found)"
}

// splitLines splits s on '\n' without allocating a trailing empty element.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
