package bulkimport_test

// publish_test.go — rmp #2179.
//
// The importer is only useful if what it writes is a store. These tests take the
// round trip: build, publish, reopen through recovery, and require every label
// and property back, with WALOps=0 — proving the snapshot is self-sufficient and
// that no write-ahead log is needed to reconstruct it.
//
// The other half is the precondition. This path writes NO WAL, so importing into
// a directory that already holds one would have recovery replay that WAL on top
// of the freshly published snapshot: not a merge, corruption. That is enforced in
// code, and TestPublish_RefusesNonEmptyDirectory is what holds it there.
//
// Layer: short.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

const (
	pubNodes = 512
	pubEdges = 2048
)

func pubKey(i int) string { return "n" + strconv.Itoa(i) }

// pubFixture is a graph with labels and properties on both nodes and edges,
// including parallel edges with distinct types, so the round trip has something
// to lose.
func pubFixture() ([]bulkimport.Node, []bulkimport.Edge[int64]) {
	nodes := make([]bulkimport.Node, pubNodes)
	for i := 0; i < pubNodes; i++ {
		nodes[i] = bulkimport.Node{
			Key:    pubKey(i),
			Labels: []string{"Person", "L" + strconv.Itoa(i%3)},
			Properties: map[string]lpg.PropertyValue{
				"id":   lpg.Int64Value(int64(i)),
				"name": lpg.StringValue("name-" + pubKey(i)),
			},
		}
	}
	edges := make([]bulkimport.Edge[int64], 0, pubEdges+pubNodes)
	for i := 0; i < pubEdges; i++ {
		edges = append(edges, bulkimport.Edge[int64]{
			Src: pubKey(i % pubNodes), Dst: pubKey((i*7 + 3) % pubNodes),
			Weight: int64(i), Type: "KNOWS",
			Properties: map[string]lpg.PropertyValue{"since": lpg.Int64Value(int64(i % 100))},
		})
	}
	// Parallel pair with distinct types, to prove the handle carriage survives
	// serialisation and recovery.
	for i := 0; i < pubNodes; i++ {
		dst := pubKey((i + 1) % pubNodes)
		edges = append(edges,
			bulkimport.Edge[int64]{Src: pubKey(i), Dst: dst, Weight: 1, Type: "PAR_A"},
			bulkimport.Edge[int64]{Src: pubKey(i), Dst: dst, Weight: 1, Type: "PAR_B"},
		)
	}
	return nodes, edges
}

func openStore(t *testing.T, dir string) recovery.Result[string, int64] {
	t.Helper()
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open(%q): %v", dir, err)
	}
	return res
}

// TestPublish_RoundTripsThroughRecovery is the acceptance test: the published
// snapshot opens as a store with every label and property intact, and with
// WALOps=0 — every byte came from the snapshot.
func TestPublish_RoundTripsThroughRecovery(t *testing.T) {
	t.Parallel()
	nodes, edges := pubFixture()
	dir := filepath.Join(t.TempDir(), "store")

	out, err := bulkimport.ImportInto[int64](context.Background(), dir,
		bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges)
	if err != nil {
		t.Fatalf("ImportInto: %v", err)
	}
	if out.Stats.Nodes != pubNodes {
		t.Fatalf("Stats.Nodes = %d, want %d", out.Stats.Nodes, pubNodes)
	}
	if want := len(edges); out.Stats.Edges != want {
		t.Fatalf("Stats.Edges = %d, want %d", out.Stats.Edges, want)
	}
	if got, want := out.SnapshotDir, filepath.Join(dir, "snapshot"); got != want {
		t.Fatalf("SnapshotDir = %q, want %q — recovery reads that name and no other", got, want)
	}

	res := openStore(t, dir)
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false: recovery did not find the published snapshot")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 — the snapshot must be self-sufficient", res.WALOps)
	}
	if got := res.Graph.LiveOrder(); got != uint64(pubNodes) {
		t.Fatalf("LiveOrder = %d, want %d", got, pubNodes)
	}

	// Node labels and properties.
	g := res.Graph
	for _, i := range []int{0, 7, pubNodes - 1} {
		k := pubKey(i)
		labels := g.NodeLabels(k)
		if len(labels) != 2 {
			t.Fatalf("node %q has labels %v, want 2", k, labels)
		}
		pv, ok := g.GetNodeProperty(k, "id")
		if !ok {
			t.Fatalf("node %q lost property id", k)
		}
		if iv, _ := pv.Int64(); iv != int64(i) {
			t.Fatalf("node %q property id = %d, want %d", k, iv, i)
		}
		if _, ok := g.GetNodeProperty(k, "name"); !ok {
			t.Fatalf("node %q lost property name", k)
		}
	}

	// Edge type and property. The importer attaches both to the edge HANDLE, so
	// they must be read back by handle: the pair-addressed EdgeLabels reads the
	// per-slot label column, which is a different store and would report nothing
	// here. That distinction is the whole reason the handle API is used (see
	// bulkimport.Builder.AddEdge).
	s0, d0 := pubKey(0), pubKey(3)
	hs := handlesFor(t, g, s0, d0)
	if len(hs) == 0 {
		t.Fatalf("edge %s->%s has no handle after recovery", s0, d0)
	}
	foundType, foundProp := false, false
	for _, h := range hs {
		if len(g.EdgeLabelsByHandle(s0, d0, h)) > 0 {
			foundType = true
		}
		if _, ok := g.EdgePropertiesByHandle(s0, d0, h)["since"]; ok {
			foundProp = true
		}
	}
	if !foundType {
		t.Fatalf("edge %s->%s lost its type through the snapshot round trip", s0, d0)
	}
	if !foundProp {
		t.Fatalf("edge %s->%s lost property since through the snapshot round trip", s0, d0)
	}

	// Parallel edges: both types must have survived, on DISTINCT handles of the
	// same pair. A pair-addressed carriage would have collapsed them to one.
	pd := pubKey(1)
	types := map[string]bool{}
	for _, h := range handlesFor(t, g, pubKey(0), pd) {
		for _, l := range g.EdgeLabelsByHandle(pubKey(0), pd, h) {
			types[l] = true
		}
	}
	if !types["PAR_A"] || !types["PAR_B"] {
		t.Fatalf("parallel edge types on %s->%s survived as %v, want both PAR_A and PAR_B",
			pubKey(0), pd, types)
	}
}

// handlesFor returns the stable handles of every edge slot from src to dst, read
// from the aligned adjacency arrays. Handle-carried metadata can only be read
// back this way.
func handlesFor(t *testing.T, g *lpg.Graph[string, int64], src, dst string) []uint64 {
	t.Helper()
	srcID, ok := g.AdjList().Mapper().Lookup(src)
	if !ok {
		t.Fatalf("source %q is not interned", src)
	}
	dstID, ok := g.AdjList().Mapper().Lookup(dst)
	if !ok {
		t.Fatalf("target %q is not interned", dst)
	}
	nbrs, _, handles := g.AdjList().LoadEntryH(srcID)
	var out []uint64
	for i, n := range nbrs {
		if n == dstID && i < len(handles) && handles[i] != 0 {
			out = append(out, handles[i])
		}
	}
	return out
}

// TestPublish_RefusesNonEmptyDirectory pins the precondition that prevents silent
// corruption. Each case is a directory shape a real store could be in.
func TestPublish_RefusesNonEmptyDirectory(t *testing.T) {
	t.Parallel()
	nodes, edges := pubFixture()

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"holds a WAL", func(t *testing.T, dir string) {
			// The dangerous case: recovery would replay this on top of the snapshot.
			if err := os.WriteFile(filepath.Join(dir, "wal"), []byte("frames"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"holds a snapshot", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "snapshot"), 0o750); err != nil {
				t.Fatal(err)
			}
		}},
		{"holds a leftover assembly", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "snapshot.tmp"), 0o750); err != nil {
				t.Fatal(err)
			}
		}},
		{"holds an unrelated file", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			_, err := bulkimport.ImportInto[int64](context.Background(), dir,
				bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges)
			if !errors.Is(err, bulkimport.ErrStoreNotEmpty) {
				t.Fatalf("ImportInto into a directory that %s = %v, want ErrStoreNotEmpty",
					tc.name, err)
			}
			// And nothing was written: the refusal must not have half-created a store.
			if _, serr := os.Stat(filepath.Join(dir, "snapshot", "manifest.json")); serr == nil {
				t.Fatal("a snapshot was published despite the refusal")
			}
		})
	}
}

// TestPublish_AcceptsAbsentAndEmptyDirectories pins the two shapes that ARE
// allowed, so the precondition is not accidentally stricter than documented.
func TestPublish_AcceptsAbsentAndEmptyDirectories(t *testing.T) {
	t.Parallel()
	nodes, edges := pubFixture()

	t.Run("absent", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "does", "not", "exist")
		if _, err := bulkimport.ImportInto[int64](context.Background(), dir,
			bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges); err != nil {
			t.Fatalf("ImportInto into an absent directory: %v", err)
		}
		if res := openStore(t, dir); !res.SnapshotHit {
			t.Fatal("the store did not open")
		}
	})
	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := bulkimport.ImportInto[int64](context.Background(), dir,
			bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges); err != nil {
			t.Fatalf("ImportInto into an empty directory: %v", err)
		}
		if res := openStore(t, dir); !res.SnapshotHit {
			t.Fatal("the store did not open")
		}
	})
}

// TestPublish_CrashedImportLeavesTheStoreUnchanged pins the crash story: an
// assembly directory left behind by a crash before the rename is neither opened
// nor kept, so the store looks exactly as it did before the import began.
func TestPublish_CrashedImportLeavesTheStoreUnchanged(t *testing.T) {
	t.Parallel()
	nodes, edges := pubFixture()

	// Publish into a scratch directory, then MOVE the published snapshot to the
	// assembly name in a fresh store. That is byte-for-byte the state a crash
	// between assembly and rename leaves behind.
	scratch := filepath.Join(t.TempDir(), "scratch")
	if _, err := bulkimport.ImportInto[int64](context.Background(), scratch,
		bulkimport.Options{Directed: true, Multigraph: true}, nodes, edges); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	assembly := filepath.Join(store, "snapshot.tmp")
	if err := os.Rename(filepath.Join(scratch, "snapshot"), assembly); err != nil {
		t.Fatal(err)
	}

	res := openStore(t, store)
	if res.SnapshotHit {
		t.Fatal("recovery opened the assembly directory; a partial import must be invisible")
	}
	if got := res.Graph.LiveOrder(); got != 0 {
		t.Fatalf("LiveOrder = %d after a crashed import, want 0", got)
	}
	if _, err := os.Stat(assembly); !os.IsNotExist(err) {
		t.Fatalf("the assembly directory survived recovery.Open: %v", err)
	}
}

// TestPublish_RefusesUnfinishedBuilder pins that a graph cannot be published
// while its adjacency commit window is still open, which would publish shards
// that are still mutable in place.
func TestPublish_RefusesUnfinishedBuilder(t *testing.T) {
	t.Parallel()
	b := bulkimport.New[int64](bulkimport.Options{Directed: true})
	if err := b.AddNode(bulkimport.Node{Key: "a"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "store")
	if _, err := bulkimport.Publish[int64](context.Background(), dir, b); !errors.Is(err, bulkimport.ErrNotFinished) {
		t.Fatalf("Publish of an unfinished builder = %v, want ErrNotFinished", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the refusal created the store directory")
	}
}

// TestPublish_HonoursContextCancellation pins the context contract on a call that
// can take a long time for a large graph.
func TestPublish_HonoursContextCancellation(t *testing.T) {
	t.Parallel()
	b := bulkimport.New[int64](bulkimport.Options{Directed: true})
	if err := b.AddNode(bulkimport.Node{Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "store")
	if _, err := bulkimport.Publish[int64](ctx, dir, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish with a cancelled context = %v, want context.Canceled", err)
	}
}
