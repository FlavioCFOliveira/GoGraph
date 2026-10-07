package snapshot

// mapper_gap_test.go — the concurrent capture filters the mapper at its instant
// (rmp #2310), and since WAL v2 step 1 (docs/design-wal-v2.md §3) the ids it
// leaves out are HOLES rather than a refusal.
//
// Layer: short.
//
// A NodeID is packed as (intra << shardBits) | shard, and intra is assigned when the
// key is INTERNED. The capture carries only the ids ever born as of its instant, so
// an id interned by a transaction still open at the instant, or by one that aborts,
// can sit below a carried id of the same shard. Before step 1 that gap made
// [graph.Mapper.LoadFrom] reject the image ("shard 114 intra-index gap: got 1 at
// slot 0"), so the capture refused with ErrCaptureNotQuiesced. LoadFrom now accepts
// gaps below the per-shard high-water marks the image records in nodeids.bin, and
// the capture always succeeds.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// sameShardKeys returns two distinct keys that intern into the SAME mapper shard,
// found by probing a throwaway mapper — the shard is a pure function of the key, so
// the placement carries over to any other mapper.
func sameShardKeys(t *testing.T) (string, string) {
	t.Helper()
	m := graph.NewMapper[string]()
	seen := make(map[uint64]string)
	for i := 0; i < 200000; i++ {
		k := fmt.Sprintf("gap-key-%06d", i)
		s := graph.MapperShardOf(m.Intern(k))
		if prev, ok := seen[s]; ok {
			return prev, k
		}
		seen[s] = k
	}
	t.Fatal("no two probe keys landed in the same mapper shard")
	return "", ""
}

// holeImage captures g at at and returns the mapper pairs, the tombstoned ids and
// the nodeids.bin high-water marks of the image.
func holeImage(t *testing.T, g *lpg.Graph[string, float64], at *lpg.Snapshot) (map[graph.NodeID]string, []graph.NodeID, *[graph.MapperShards]uint64) {
	t.Helper()
	cs := csr.BuildFromAdjListAsOf(g.AdjList(),
		func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	capt, err := CaptureGraph[string, float64](g, cs, nil, at)
	if err != nil {
		t.Fatalf("CaptureGraph: %v", err)
	}
	rb, err := ReadMapperString(bytes.NewReader(capt.mapper.bytes))
	if err != nil {
		t.Fatalf("ReadMapperString: %v", err)
	}
	pairs := make(map[graph.NodeID]string, len(rb.Pairs))
	for _, p := range rb.Pairs {
		pairs[p.ID] = p.Key
	}
	var dead []graph.NodeID
	if capt.tombstones.present {
		tb, terr := ReadTombstones(bytes.NewReader(capt.tombstones.bytes))
		if terr != nil {
			t.Fatalf("ReadTombstones: %v", terr)
		}
		dead = tb.IDs
	}
	if !capt.nodeIDs.present {
		t.Fatal("the capture emitted mapper.bin without nodeids.bin")
	}
	next, err := ReadNodeIDs(bytes.NewReader(capt.nodeIDs.bytes))
	if err != nil {
		t.Fatalf("ReadNodeIDs: %v", err)
	}
	return pairs, dead, next
}

// assertHole checks that idA is a hole of the image — absent from mapper.bin, not
// a tombstone, below the shard's high-water mark — that idB is carried, and that
// the image loads with A's slot left empty.
func assertHole(t *testing.T, pairs map[graph.NodeID]string, dead []graph.NodeID,
	next *[graph.MapperShards]uint64, idA, idB graph.NodeID, keyA, keyB string) {
	t.Helper()
	if k, ok := pairs[idA]; ok {
		t.Errorf("mapper.bin carries the never-born node %d (%q); want a hole", uint64(idA), k)
	}
	if pairs[idB] != keyB {
		t.Errorf("mapper.bin lacks the committed node %d (%q)", uint64(idB), keyB)
	}
	for _, d := range dead {
		if d == idA {
			t.Errorf("tombstones.bin names the never-born node %d; a hole is not a removal", uint64(idA))
		}
	}
	shard := graph.MapperShardOf(idB)
	if next[shard] <= uint64(idB)>>8 {
		t.Errorf("nodeids.bin next[%d] = %d, not above node %d", shard, next[shard], uint64(idB))
	}
	entries := make([]graph.MapperEntry[string], 0, len(pairs))
	for id, k := range pairs {
		entries = append(entries, graph.MapperEntry[string]{ID: id, Key: k})
	}
	m := graph.NewMapper[string]()
	if err := m.LoadFrom(entries, next); err != nil {
		t.Fatalf("the image does not load: %v", err)
	}
	if _, ok := m.Resolve(idA); ok {
		t.Errorf("node %d resolves after the load; want a hole", uint64(idA))
	}
	if id := m.Intern(keyA); id == idA || uint64(id)>>8 < next[graph.MapperShardOf(id)] {
		t.Errorf("re-interning %q gave %d, want an id at or above the shard's high-water mark", keyA, uint64(id))
	}
}

// TestCapture_OpenTransactionAtInstantLeavesAHole drives the interleaving the
// retired ErrCaptureNotQuiesced refused: transaction A interns its key and is
// still open at the instant, and B interns the next slot of the same shard and
// commits before it (WAL v2 step 1, docs/design-wal-v2.md §3.2). The capture
// must succeed, leave A's id as a hole — absent from mapper.bin and not a
// tombstone — and produce an image that loads.
func TestCapture_OpenTransactionAtInstantLeavesAHole(t *testing.T) {
	keyA, keyB := sameShardKeys(t)

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	txA := g.BeginVersionedTx()
	defer g.EndVersionedTx(txA)
	if err := g.Writer(txA).AddNode(keyA); err != nil {
		t.Fatalf("A AddNode(%q): %v", keyA, err)
	}
	if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
		return g.Writer(tx).AddNode(keyB)
	}); err != nil {
		t.Fatalf("B AddNode(%q): %v", keyB, err)
	}
	idA, _ := g.AdjList().Mapper().Lookup(keyA)
	idB, _ := g.AdjList().Mapper().Lookup(keyB)
	if graph.MapperShardOf(idA) != graph.MapperShardOf(idB) || idA >= idB {
		t.Fatalf("premise: A=%d must sit below B=%d in one shard", uint64(idA), uint64(idB))
	}

	at := g.BeginRead()
	defer g.EndRead(at)
	pairs, dead, next := holeImage(t, g, at)
	assertHole(t, pairs, dead, next, idA, idB, keyA, keyB)
}

// TestCapture_AbortedCreationAfterInstantLeavesAHole is the abort half: A's
// creation is rolled back after the instant, which withdraws its birth record.
// Before WAL v2 step 1 the withdrawn id read as interned long ago and was kept
// above a dropped one; now it is a hole, through a capture read and through a
// plain one.
func TestCapture_AbortedCreationAfterInstantLeavesAHole(t *testing.T) {
	for _, captureRead := range []bool{true, false} {
		t.Run(fmt.Sprintf("capture_read=%v", captureRead), func(t *testing.T) {
			keyA, keyB := sameShardKeys(t)
			g := lpg.New[string, float64](adjlist.Config{Directed: true})
			defer func() { _ = g.Close() }()
			if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
				return g.Writer(tx).AddNode("seed")
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			at := g.BeginRead()
			if captureRead {
				g.EndRead(at)
				at = g.BeginCaptureRead()
			}
			defer g.EndRead(at)
			// After the instant: B commits, then A is created and rolled back.
			if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
				return g.Writer(tx).AddNode(keyB)
			}); err != nil {
				t.Fatalf("B: %v", err)
			}
			txA := g.BeginVersionedTx()
			if err := g.Writer(txA).AddNode(keyA); err != nil {
				t.Fatalf("A: %v", err)
			}
			txA.Abandon()
			g.EndVersionedTx(txA)
			idA, _ := g.AdjList().Mapper().Lookup(keyA)
			idB, _ := g.AdjList().Mapper().Lookup(keyB)
			pairs, dead, _ := holeImage(t, g, at)
			for _, id := range []graph.NodeID{idA, idB} {
				if _, ok := pairs[id]; ok {
					t.Errorf("node %d, interned after the instant, is in mapper.bin", uint64(id))
				}
				for _, d := range dead {
					if d == id {
						t.Errorf("node %d, interned after the instant, is a tombstone", uint64(id))
					}
				}
			}
			if _, ok := pairs[mustLookup(t, g, "seed")]; !ok {
				t.Error("the seed node is missing from mapper.bin")
			}
		})
	}
}

// mustLookup returns the id of key k.
func mustLookup(t *testing.T, g *lpg.Graph[string, float64], k string) graph.NodeID {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(k)
	if !ok {
		t.Fatalf("key %q not interned", k)
	}
	return id
}

// TestCapture_QuiescedInstantEmitsEveryVisibleNode is the positive arm: with no write
// transaction open when the instant is taken — the state the commit serialiser's
// drain guarantees the checkpointer — the capture succeeds and carries every
// committed node.
//
// Without it, TestCapture_FilteredMapperKeepsIntraIndexesContiguous would be
// satisfied by a guard that rejected every concurrent capture.
func TestCapture_QuiescedInstantEmitsEveryVisibleNode(t *testing.T) {
	keyA, keyB := sameShardKeys(t)

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	// Both transactions COMMIT before the instant, in interning order.
	for _, k := range []string{keyA, keyB} {
		key := k
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			return g.Writer(tx).AddNode(key)
		}); err != nil {
			t.Fatalf("AddNode(%q): %v", key, err)
		}
	}

	at := g.BeginRead()
	cs := csr.BuildFromAdjListAsOf(g.AdjList(),
		func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	capt, err := CaptureGraph[string, float64](g, cs, nil, at)
	g.EndRead(at)
	if err != nil {
		t.Fatalf("CaptureGraph on a quiesced instant: %v", err)
	}

	rb, err := ReadMapperString(bytes.NewReader(capt.mapper.bytes))
	if err != nil {
		t.Fatalf("ReadMapperString: %v", err)
	}
	entries := make([]graph.MapperEntry[string], 0, len(rb.Pairs))
	for _, p := range rb.Pairs {
		entries = append(entries, graph.MapperEntry[string]{ID: p.ID, Key: p.Key})
	}
	fresh := graph.NewMapper[string]()
	if lerr := fresh.LoadFrom(entries, nil); lerr != nil {
		t.Fatalf("the captured mapper does not load back: %v", lerr)
	}
	for _, k := range []string{keyA, keyB} {
		if _, ok := fresh.Lookup(k); !ok {
			t.Errorf("committed key %q is missing from the image", k)
		}
	}
	if got, want := capt.Order(), uint64(2); got != want {
		t.Errorf("image carries %d nodes, want %d", got, want)
	}
}

// TestCapture_TombstoneIDsAreAscending pins tombstones.bin's documented input
// contract for the INSTANT path.
//
// # Why it needs a test of its own
//
// The present-time path gets ascending ids for free: it reads the roaring bitmap,
// whose ToArray is ordered. The instant path cannot use the bitmap — the bitmap
// answers "removed NOW" and keeps no history — so it derives the set from a mapper
// walk instead, and that walk is NOT ascending. A NodeID packs as
// (intra << shardBits) | shard and Walk is shard-major, so a graph with more than one
// node per shard walks 0, 256, 512, 768, 1, 257, …
//
// The graph here is deliberately larger than the 256 shards. Below that every shard
// holds a single node, ids equal shard indexes, and the walk IS ascending — which is
// exactly why the wrong claim survived being written down, and why a small fixture
// would pass against the unsorted code.
func TestCapture_TombstoneIDsAreAscending(t *testing.T) {
	const n = 2000

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	for i := 0; i < n; i++ {
		if err := g.AddNode(fmt.Sprintf("tomb-%05d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	// Remove every third node, so the dead set spans many shards at several intra
	// indexes and the walk order is thoroughly non-monotonic.
	var removed int
	for i := 0; i < n; i += 3 {
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			_, err := g.Writer(tx).RemoveNode(fmt.Sprintf("tomb-%05d", i))
			return err
		}); err != nil {
			t.Fatalf("RemoveNode: %v", err)
		}
		removed++
	}

	at := g.BeginRead()
	got := g.TombstonedIDsAsOf(at)
	g.EndRead(at)

	if len(got) != removed {
		t.Fatalf("TombstonedIDsAsOf returned %d ids, want %d", len(got), removed)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("TombstonedIDsAsOf is not ascending at index %d: %d follows %d. "+
				"tombstones.bin documents ascending ids as its input contract, and the "+
				"present-time form satisfies it via the roaring bitmap's ToArray — the "+
				"instant form derives the set from a shard-major mapper walk and must sort",
				i, uint64(got[i]), uint64(got[i-1]))
		}
	}
	// The fixture must actually exercise the non-monotonic case, or a sort-free
	// implementation would pass. Confirm the underlying walk really is out of order.
	var walked []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		walked = append(walked, id)
		return true
	})
	monotonic := true
	for i := 1; i < len(walked); i++ {
		if walked[i] < walked[i-1] {
			monotonic = false
			break
		}
	}
	if monotonic {
		t.Fatal("the mapper walk happened to be ascending on this fixture, so the sort was " +
			"never exercised and this test proves nothing — enlarge the graph past the " +
			"shard count")
	}
}
